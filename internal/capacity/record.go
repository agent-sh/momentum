package capacity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// What a run did goes into the store, and into the report through notes (no notification):
//   - each deletion and prune is an event (kind capacity-delete, capacity-prune) and a decided note that says what
//     went, its size, since when it was idle, the rule, and how to have it back or keep it; a prune that removed
//     nothing is an event only;
//   - each item only reported is an event (capacity-report), once, and its rule's problem note for that reason
//     lists them; the note is put again only when an item new to it appears, so a note the owner resolved stays
//     resolved until the evidence changes, and it is resolved by itself when the rule finds none;
//   - a deletion that failed is an event (capacity-failed) and its rule's problem note for failures;
//   - a fast fill is an event (capacity-fill) and the disk's problem note, high importance, put once per fill.

// The event kinds.
const (
	KindDelete = "capacity-delete"
	KindPrune  = "capacity-prune"
	KindReport = "capacity-report"
	KindFailed = "capacity-failed"
	KindFill   = "capacity-fill"
)

// overrides say, per rule, how to have an item back or keep it, for its decided note.
var overrides = map[string]string{
	RuleModels:    "Download it again when work on it resumes; to keep an item, list it in capacity.models.keep in sources.json.",
	RuleTargets:   "Cargo builds it again on the next build; to keep a project's build output, list the project in capacity.exclude.paths.",
	RuleWorktrees: "Its commits are on its remote, or its changes are in the default branch; git worktree add brings a branch back.",
	RuleUV:        "uv downloads the wheels it dropped again on the next install.",
	RuleDocker:    "Docker builds the cache again on the next build.",
}

// ruleText says what each rule deleted, for a note's title.
var ruleText = map[string]string{
	RuleModels: "idle model data", RuleTargets: "Rust build output", RuleWorktrees: "leftover worktree",
	RuleUV: "uv cache", RuleDocker: "Docker",
}

// recordDone records one deletion or prune at once: its event and its decided note.
func (r *runner) recordDone(ctx context.Context, f *Finding) error {
	kind := KindDelete
	if f.Rule == RuleUV || f.Rule == RuleDocker {
		kind = KindPrune
	}
	e := r.event(kind, *f, fmt.Sprintf("%s/%s/%s/%d", kind, f.Rule, keyPath(*f), r.now.UnixMilli()), store.ActorAgent)
	e.TS = r.opt.Clock() // when it went: a later run tells its own change to a directory from someone's by it
	if _, err := r.st.Put(ctx, []store.Event{e}); err != nil {
		return err
	}
	if f.none { // the event keeps that it ran; a note of nothing deleted would only crowd the decided list
		return nil
	}
	_, err := r.st.PutNote(ctx, decidedNote(e, *f, e.TS))
	return err
}

// recordPrunes records the prunes one rule made on this run, as soon as it made them.
func (r *runner) recordPrunes(ctx context.Context, rule string) error {
	for _, rr := range r.res.Rules {
		if rr.Rule != rule {
			continue
		}
		for i := range rr.Findings {
			if rr.Findings[i].Done {
				if err := r.recordDone(ctx, &rr.Findings[i]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// record records what the run could only report, and resolves what it no longer finds. Deletions and prunes were
// recorded as they happened.
func (r *runner) record(ctx context.Context) error {
	groups := map[string]*group{}
	ran := map[string]bool{}
	for _, rr := range r.res.Rules {
		if !rr.Ran || rr.Err != "" {
			continue
		}
		ran[rr.Rule] = true
		for _, f := range rr.Findings {
			switch {
			case f.Done:
			case f.Err != "":
				g := groupOf(groups, f.Rule, "failed")
				g.items = append(g.items, f)
				g.events = append(g.events, r.event(KindFailed, f, fmt.Sprintf("failed/%s/%s", f.Rule, keyPath(f)), store.ActorDetector))
			case f.Action == ActReport:
				g := groupOf(groups, f.Rule, f.class)
				g.items = append(g.items, f)
				g.events = append(g.events, r.event(KindReport, f, fmt.Sprintf("report/%s/%s/%s", f.Rule, f.class, keyPath(f)), store.ActorDetector))
			}
		}
	}
	for _, key := range sortedKeys(groups) {
		g := groups[key]
		fresh, err := r.putNew(ctx, g.events)
		if err != nil {
			return err
		}
		id := "capacity:" + g.rule + ":" + g.class
		old, had, err := r.st.Note(ctx, id)
		if err != nil {
			return err
		}
		auto, err := r.st.Mark(ctx, "auto:"+id)
		if err != nil {
			return err
		}
		standing := had && old.Done.IsZero()
		resolvedByUs := had && !old.Done.IsZero() && !auto.IsZero() && !old.Done.After(auto)
		if fresh || standing || resolvedByUs {
			if _, err := r.st.PutNote(ctx, groupNote(id, g.rule, g.class, g.items, r.now)); err != nil {
				return err
			}
		}
	}
	// A problem note whose rule ran and found none of its items any more is resolved, by the rule: it stands again
	// when its items come back, unlike one the owner resolved.
	ns, err := r.st.Notes(ctx)
	if err != nil {
		return err
	}
	for _, n := range ns {
		rest, ok := strings.CutPrefix(n.ID, "capacity:")
		if !ok || !n.Done.IsZero() || n.Kind != store.NoteProblem {
			continue
		}
		rule, class, _ := strings.Cut(rest, ":")
		if ran[rule] && groups[rule+"\x00"+class] == nil {
			if _, _, err := r.st.DoneNote(ctx, n.ID, r.now); err != nil {
				return err
			}
			if err := r.st.SetMark(ctx, "auto:"+n.ID, r.now); err != nil {
				return err
			}
		}
	}
	if err := r.recordJobs(ctx); err != nil {
		return err
	}
	if err := r.recordHolds(ctx); err != nil {
		return err
	}
	return r.recordDisks(ctx)
}

// recordHolds records a run that could not trust its look at the processes, or could not ask Docker which
// containers run, and so deleted nothing: a problem note for each reason, put when its reason is new to it, and
// resolved by the next run that has no such reason. Docker is asked only when a rule scans.
func (r *runner) recordHolds(ctx context.Context) error {
	for _, h := range []struct {
		id, why, title string
		checked        bool
	}{
		{"capacity:hold:look", r.blind, "Capacity deleted nothing: it cannot trust its look at the processes", true},
		{"capacity:hold:docker", r.dockerErr, "Capacity deleted nothing: Docker could not be asked which containers run", r.dockerAsked},
	} {
		if !h.checked {
			continue
		}
		old, had, err := r.st.Note(ctx, h.id)
		if err != nil {
			return err
		}
		if h.why == "" {
			if had && old.Done.IsZero() {
				if _, _, err := r.st.DoneNote(ctx, h.id, r.now); err != nil {
					return err
				}
			}
			continue
		}
		sum := sha256.Sum256([]byte(h.why))
		e := r.event(KindFailed, Finding{Rule: "hold", Path: h.id, Err: h.why}, "hold/"+hex.EncodeToString(sum[:8]), store.ActorDetector)
		fresh, err := r.putNew(ctx, []store.Event{e})
		if err != nil {
			return err
		}
		if fresh || (had && old.Done.IsZero()) {
			if _, err := r.st.PutNote(ctx, store.Note{ID: h.id, Kind: store.NoteProblem, Source: store.Capacity, At: r.now,
				Title: h.title, Detail: upper(h.why) + "."}); err != nil {
				return err
			}
		}
	}
	return nil
}

// putNew stores the events the store does not hold yet, so an item's event keeps its first sighting, and reports
// whether there was one.
func (r *runner) putNew(ctx context.Context, evs []store.Event) (bool, error) {
	stored, err := r.st.Stored(ctx, evs)
	if err != nil {
		return false, err
	}
	var add []store.Event
	for _, e := range evs {
		if !stored[e.ID()] {
			add = append(add, e)
		}
	}
	if len(add) == 0 {
		return false, nil
	}
	_, err = r.st.Put(ctx, add)
	return true, err
}

// recordJobs records a run that could not read the scheduled jobs, and so deleted nothing: a problem note, put when
// its error is new to it, and resolved by the next run that reads them all.
func (r *runner) recordJobs(ctx context.Context) error {
	const id = "capacity:jobs"
	if r.named == nil && r.namedErr == nil {
		return nil // no rule scanned: the jobs were not read
	}
	old, had, err := r.st.Note(ctx, id)
	if err != nil {
		return err
	}
	if r.namedErr == nil {
		if had && old.Done.IsZero() {
			_, _, err = r.st.DoneNote(ctx, id, r.now)
		}
		return err
	}
	h := sha256.Sum256([]byte(r.namedErr.Error()))
	e := r.event(KindFailed, Finding{Rule: "jobs", Path: "scheduled jobs", Err: r.namedErr.Error()},
		"jobs/"+hex.EncodeToString(h[:8]), store.ActorDetector)
	fresh, err := r.putNew(ctx, []store.Event{e})
	if err != nil {
		return err
	}
	if fresh || (had && old.Done.IsZero()) {
		_, err = r.st.PutNote(ctx, store.Note{ID: id, Kind: store.NoteProblem, Source: store.Capacity, At: r.now,
			Title:  "Capacity deleted and pruned nothing: the scheduled jobs could not all be read",
			Detail: "A job may name a path it would delete, so it waits until they can be read: " + r.namedErr.Error()})
	}
	return err
}

// recordDisks records each fast fill: an event at every run that sees it, and the disk's note, put once per fill and
// rewritten to say it ended at the first run without it. A disk above its full percent has a note of its own, put
// once and resolved when it drops below.
func (r *runner) recordDisks(ctx context.Context) error {
	for _, d := range r.res.Disks {
		if d.Err != "" {
			continue
		}
		if err := r.recordFill(ctx, d); err != nil {
			return err
		}
		if err := r.recordFull(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) recordFill(ctx context.Context, d DiskState) error {
	id := "capacity:fill:" + d.Disk
	open, err := r.st.Mark(ctx, "fill-open:"+d.Disk)
	if err != nil {
		return err
	}
	old, had, err := r.st.Note(ctx, id)
	if err != nil {
		return err
	}
	if !d.Fast {
		if open.IsZero() {
			return nil
		}
		if had && old.Done.IsZero() { // the fill ended: the note says so, and stands until the owner reads it
			old.Title = fmt.Sprintf("%s filled fast from %s to %s; %.0f%% used now", d.Disk, old.At.Format("Jan 2 15:04"),
				r.now.Format("15:04"), d.pctFull)
			if _, err := r.st.PutNote(ctx, old); err != nil {
				return err
			}
		}
		return r.st.ClearMark(ctx, "fill-open:"+d.Disk)
	}
	f := Finding{Rule: RuleDisk, Path: d.Disk, Detail: fillDetail(d)}
	e := r.event(KindFill, f, fmt.Sprintf("fill/%s/%d", d.Disk, r.now.UnixMilli()), store.ActorDetector)
	meta, _ := json.Marshal(d)
	e.Meta, e.Summary = string(meta), fillTitle(d)
	if _, err := r.st.Put(ctx, []store.Event{e}); err != nil {
		return err
	}
	if !open.IsZero() && had && !old.Done.IsZero() {
		return nil // resolved during this fill: it stays resolved
	}
	n := store.Note{ID: id, Kind: store.NoteProblem, Title: fillTitle(d), Detail: fillDetail(d), Source: store.Capacity,
		Importance: store.ImportanceHigh, At: r.now}
	if !open.IsZero() && had {
		n.At = old.At // the same fill: it keeps its start
	} else if err := r.st.SetMark(ctx, "fill-open:"+d.Disk, r.now); err != nil {
		return err
	}
	_, err = r.st.PutNote(ctx, n)
	return err
}

func (r *runner) recordFull(ctx context.Context, d DiskState) error {
	id := "capacity:full:" + d.Disk
	open, err := r.st.Mark(ctx, "full-open:"+d.Disk)
	if err != nil {
		return err
	}
	full := d.pctFull >= r.cfg.Fill.Full
	switch {
	case full && open.IsZero():
		if err := r.st.SetMark(ctx, "full-open:"+d.Disk, r.now); err != nil {
			return err
		}
		_, err := r.st.PutNote(ctx, store.Note{ID: id, Kind: store.NoteProblem, Source: store.Capacity, Importance: store.ImportanceHigh,
			At:     r.now,
			Title:  fmt.Sprintf("%s is %.0f%% used, above %.0f%%: %s free", d.Disk, d.pctFull, r.cfg.Fill.Full, gib(d.Avail)),
			Detail: "Capacity deletes only what its rules find idle; what it can only report is among the problems."})
		return err
	case !full && !open.IsZero():
		if n, had, err := r.st.Note(ctx, id); err != nil {
			return err
		} else if had && n.Done.IsZero() {
			if _, _, err := r.st.DoneNote(ctx, id, r.now); err != nil {
				return err
			}
		}
		return r.st.ClearMark(ctx, "full-open:"+d.Disk)
	}
	return nil
}

func fillTitle(d DiskState) string {
	t := fmt.Sprintf("%s is filling at %.0f GiB/h (%.0f%% used)", d.Disk, d.Rate, d.pctFull)
	if d.FullIn > 0 {
		t += fmt.Sprintf("; full at this rate in %s", d.FullIn.Round(time.Minute))
	}
	return t
}

func fillDetail(d DiskState) string {
	if len(d.Writers) == 0 {
		return "No process of this user wrote fast in the seconds measured; a root writer (Docker, a system service) cannot be seen."
	}
	var parts []string
	for _, w := range d.Writers[:min(len(d.Writers), 4)] {
		s := fmt.Sprintf("%s (pid %d) %.0f MiB/s in %s", w.Comm, w.PID, w.Rate, short(w.Cwd))
		if w.Session != "" {
			s += ", session " + w.Session
		}
		parts = append(parts, s)
	}
	return "Writing: " + strings.Join(parts, "; ") + "."
}

// group is one rule's report-only items for one reason, and their events.
type group struct {
	rule, class string
	items       []Finding
	events      []store.Event
}

func groupOf(gs map[string]*group, rule, class string) *group {
	k := rule + "\x00" + class
	if gs[k] == nil {
		gs[k] = &group{rule: rule, class: class}
	}
	return gs[k]
}

// event is a finding as an event of source capacity.
func (r *runner) event(kind string, f Finding, key string, actor store.Actor) store.Event {
	meta := map[string]any{"rule": f.Rule, "path": f.Path, "bytes": f.Bytes}
	if f.Freed > 0 {
		meta["freed"] = f.Freed
	}
	if !f.Idle.IsZero() {
		meta["idle_since"] = f.Idle.Format(time.RFC3339)
	}
	if !f.Seen.IsZero() {
		meta["seen"] = f.Seen.Format(time.RFC3339)
	}
	for k, v := range map[string]string{"why": f.Why, "detail": f.Detail, "error": f.Err, "class": f.class, "branch": f.branch} {
		if v != "" {
			meta[k] = v
		}
	}
	b, _ := json.Marshal(meta)
	return store.Event{Source: store.Capacity, SourceKey: key, TS: r.now, Actor: actor, Kind: kind, Tool: f.Rule,
		Summary: summary(kind, f), Meta: string(b), Ref: f.Path}
}

func summary(kind string, f Finding) string {
	switch kind {
	case KindDelete, KindPrune:
		return fmt.Sprintf("%s %s: %s, freed %s", ruleText[f.Rule], short(f.Path), gib(f.Bytes), gib(f.Freed))
	case KindFailed:
		return fmt.Sprintf("could not delete %s %s: %s", ruleText[f.Rule], short(f.Path), f.Err)
	}
	return fmt.Sprintf("%s %s (%s) left: %s", ruleText[f.Rule], short(f.Path), gib(f.Bytes), f.Why)
}

// decidedNote is a deletion's or a prune's note.
func decidedNote(e store.Event, f Finding, now time.Time) store.Note {
	title := fmt.Sprintf("Deleted %s %s (%s)", ruleText[f.Rule], short(f.Path), gib(f.Bytes))
	effect := "Freed " + gib(f.Freed)
	if f.Rule == RuleUV || f.Rule == RuleDocker {
		title = fmt.Sprintf("Pruned %s: %s", f.Path, gib(f.Freed))
	}
	if f.unsaid {
		title, effect = "Pruned "+f.Path, "Freed an amount the tool did not print"
	}
	var detail []string
	if f.Detail != "" {
		detail = append(detail, upper(f.Detail)+".")
	}
	if !f.Idle.IsZero() {
		detail = append(detail, "Idle since "+f.Idle.Format("2006-01-02")+".")
	}
	detail = append(detail, "Rule "+f.Rule+".", overrides[f.Rule])
	return store.Note{ID: "capacity:deleted:" + e.ID(), Kind: store.NoteDecided, Title: title, Detail: strings.Join(detail, " "),
		Effect: effect, Source: store.Capacity, Importance: store.ImportanceNormal, At: now}
}

// groupTitles say what a rule's report-only items of one reason are.
var groupTitles = map[string]string{
	"excluded":    "in excluded projects, left for their owners",
	"unknown":     "of no known project, left",
	"tracked":     "that git does not ignore, left",
	"uncommitted": "with uncommitted work, left",
	"unpushed":    "with commits on no remote, left",
	"unreadable":  "git could not read, left",
	"ignored":     "with ignored files that are not a cache, left",
	"mount":       "holding a mount point, left",
	"volumes":     "only reported",
	"failed":      "that could not be deleted",
}

// groupNote is the problem note of one rule's report-only items for one reason.
func groupNote(id, rule, class string, items []Finding, now time.Time) store.Note {
	sort.SliceStable(items, func(i, j int) bool { return items[i].Bytes > items[j].Bytes })
	var total int64
	for _, f := range items {
		total += f.Bytes
	}
	what := ruleText[rule]
	title := fmt.Sprintf("%d %s %s (%s)", len(items), plural(what, len(items)), groupTitles[class], gib(total))
	switch {
	case rule == RuleDocker && class == "volumes":
		title = fmt.Sprintf("Docker volumes no container uses hold %s, only reported", gib(total))
	case rule == RuleUV && class == "busy":
		title = fmt.Sprintf("The uv cache is %s and waits to be pruned: uv processes read it", gib(total))
	}
	var parts []string
	for _, f := range items[:min(len(items), 3)] {
		why := f.Why
		if f.Err != "" {
			why = f.Err
		}
		parts = append(parts, fmt.Sprintf("%s %s: %s", short(f.Path), gib(f.Bytes), why))
	}
	detail := strings.Join(parts, "; ") + "."
	if len(items) > 3 {
		detail += fmt.Sprintf(" And %d more; momentum capacity --dry-run lists them.", len(items)-3)
	}
	return store.Note{ID: id, Kind: store.NoteProblem, Title: title, Detail: detail, Source: store.Capacity,
		Importance: store.ImportanceNormal, At: now}
}

func plural(what string, n int) string {
	if n == 1 || strings.HasSuffix(what, "output") || strings.HasSuffix(what, "data") || strings.HasSuffix(what, "cache") {
		return what
	}
	return what + "s"
}

func upper(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// keyPath is a finding's identity in its events' keys: its canonical path, hashed when long.
func keyPath(f Finding) string {
	p := f.canon
	if p == "" {
		p = f.Path
	}
	if len(p) <= 200 {
		return p
	}
	h := sha256.Sum256([]byte(p))
	return hex.EncodeToString(h[:12])
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
