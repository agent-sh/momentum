package capacity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
)

// What a run did goes into the store, and into the report through notes (no notification):
//   - each deletion, offload and prune is an event (kind capacity-delete, capacity-offload, capacity-prune) that says
//     what went, its size, the space it freed, since when it was idle and the rule;
//   - together they are the run's one decided note (capacity:run:MS, the run's start), put with each one's event in
//     one transaction, so a run stopped midway leaves a note of what it did until then: what the run freed, how many
//     items each rule took, the largest of them, and how to keep one next time; a prune that removed nothing is an
//     event only, and a run that took nothing writes no note;
//   - each item only reported is an event (capacity-report), once, and its rule's problem note for that reason
//     lists them; the note is put again only when an item new to it appears, so a note the owner resolved stays
//     resolved until the evidence changes, and it is resolved by itself when the rule finds none;
//   - a deletion that failed is an event (capacity-failed) and its rule's problem note for failures;
//   - a fast fill is an event (capacity-fill) and the disk's problem note, high importance, put once per fill.

// The event kinds.
const (
	KindDelete  = "capacity-delete"
	KindOffload = "capacity-offload"
	KindPrune   = "capacity-prune"
	KindReport  = "capacity-report"
	KindFailed  = "capacity-failed"
	KindFill    = "capacity-fill"
)

// keeps say, per rule that deletes or offloads, how to keep an item next time: the run's note joins its rules' in
// one sentence, and a note short of room names only what to set (keepsBrief). The keys are sources.json's.
var keeps = map[string]string{
	RuleModels:    "list model data in capacity.models.keep",
	RuleTargets:   "list a build output's project in capacity.exclude.paths",
	RuleWorktrees: "lock a worktree (git worktree lock)",
	RuleScratch:   "list a task folder in capacity.scratch.keep",
}

var keepsBrief = map[string]string{
	RuleModels: "capacity.models.keep", RuleTargets: "capacity.exclude.paths (the project)", RuleWorktrees: "git worktree lock",
	RuleScratch: "capacity.scratch.keep",
}

// comesBack says, per rule that prunes, how what it took comes back: there is nothing to keep.
var comesBack = map[string]string{
	RuleUV:     "uv downloads the wheels it dropped again.",
	RuleDocker: "Docker builds its cache again on the next build.",
}

// modelDataGone says, in a run that deleted a task folder's model data, that restic does not hold it.
const modelDataGone = "Model data is not offloaded: it is downloaded or made again."

// pruneTool names the tool of each rule that prunes.
var pruneTool = map[string]string{RuleUV: "uv", RuleDocker: "Docker"}

// ruleText says what each rule deleted, for a note's title.
var ruleText = map[string]string{
	RuleModels: "idle model data", RuleTargets: "Rust build output", RuleWorktrees: "leftover worktree",
	RuleScratch: "task folder", RuleUV: "uv cache", RuleDocker: "Docker",
}

// countText names what each rule deleted as a run's title counts it, and briefText shorter, for a title that would
// not fit on a line.
var (
	countText = map[string]string{RuleModels: "idle model item", RuleTargets: "Rust build output", RuleWorktrees: "leftover worktree",
		RuleScratch: "task folder"}
	briefText = map[string]string{RuleModels: "model item", RuleTargets: "build output", RuleWorktrees: "worktree", RuleScratch: "folder"}
)

// recordDone records one deletion, offload or prune at once: its event, and the run's note with it.
func (r *runner) recordDone(ctx context.Context, f *Finding) error {
	kind := KindDelete
	switch {
	case f.Rule == RuleUV || f.Rule == RuleDocker:
		kind = KindPrune
	case f.Rule == RuleScratch && f.offloaded:
		kind = KindOffload
	}
	e := r.event(kind, *f, fmt.Sprintf("%s/%s/%s/%d", kind, f.Rule, keyPath(*f), r.now.UnixMilli()), store.ActorAgent)
	e.TS = r.opt.Clock() // when it went: a later run tells its own change to a directory from someone's by it
	if f.none {
		// the event keeps that it ran; in the run's note, a prune that removed nothing would only crowd it
		_, err := r.st.Put(ctx, []store.Event{e})
		return err
	}
	return r.recordTaken(ctx, e, itemOf(*f))
}

// recordModelData records, before the offload that follows, the model data a task folder's act deleted: its own
// event, and the run's note with it, so the deletion is on record whatever the offload does.
func (r *runner) recordModelData(ctx context.Context, f *Finding) error {
	g := *f
	g.offloaded, g.snapshot, g.Freed, g.Bytes = false, "", f.modelFreed, f.removed
	e := r.event(KindDelete, g, fmt.Sprintf("%s/%s/%s/models/%d", KindDelete, f.Rule, keyPath(*f), r.now.UnixMilli()), store.ActorAgent)
	e.TS = r.opt.Clock()
	return r.recordTaken(ctx, e, itemOf(g))
}

// recordTaken puts an act's event and, in the same transaction, the run's note with what the act took added: a run
// stopped midway leaves a note of what it did until then.
func (r *runner) recordTaken(ctx context.Context, e store.Event, it runItem) error {
	items := append(r.taken[:len(r.taken):len(r.taken)], it)
	if _, err := r.st.PutWithNote(ctx, []store.Event{e}, runNote(r.now, e.TS, items)); err != nil {
		return err
	}
	r.taken = items
	return nil
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
// containers run, and so deleted nothing, or could not read the live sessions whole, and so left the task folders
// and worktrees: a problem note for each reason, put when its reason is new to it, and resolved by the next run that
// has no such reason. Docker and the sessions are asked only when a rule needs them.
func (r *runner) recordHolds(ctx context.Context) error {
	for _, h := range []struct {
		id, why, title string
		checked        bool
	}{
		{"capacity:hold:look", r.blind, "Capacity deleted nothing: it cannot trust its look at the processes", true},
		{"capacity:hold:docker", r.dockerErr, "Capacity deleted nothing: Docker could not be asked which containers run", r.dockerAsked},
		{"capacity:hold:sessions", r.sessionsHeld, "Capacity left task folders and worktrees: the live agent sessions could not all be read",
			r.sessionsHeld != "" || r.sessionsWhole},
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
	all := d
	if !open.IsZero() { // the note names every writer the fill's runs saw: one may have finished since
		all.Writers, err = r.fillWriters(ctx, d, open)
		if err != nil {
			return err
		}
	}
	n := store.Note{ID: id, Kind: store.NoteProblem, Title: fillTitle(d), Detail: fillDetail(all), Source: store.Capacity,
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

// fillWriters are the writers of the fill's runs, this one's among them (from their events), each process once,
// with what it wrote over the runs it was a writer in, and its fastest look.
func (r *runner) fillWriters(ctx context.Context, d DiskState, since time.Time) ([]Writer, error) {
	evs, err := r.st.Events(ctx, store.Query{Source: store.Capacity, Kinds: []string{KindFill}, Since: since, Limit: math.MaxInt32})
	if err != nil {
		return nil, err
	}
	type key struct {
		pid   int
		start int64
	}
	by := map[key]Writer{}
	add := func(w Writer) { // each run's writes are its own stretch: they add up; the newest run names it
		k := key{w.PID, w.Start}
		if o, ok := by[k]; ok {
			o.Written, o.Over, o.Rate = o.Written+w.Written, o.Over+w.Over, max(o.Rate, w.Rate)
			o.Session = firstNonEmpty(o.Session, w.Session)
			w = o
		}
		by[k] = w
	}
	for _, e := range evs { // newest first, this run's among them: its event is recorded first
		var past DiskState
		if json.Unmarshal([]byte(e.Meta), &past) == nil && past.Disk == d.Disk {
			for _, w := range past.Writers {
				add(w)
			}
		}
	}
	out := make([]Writer, 0, len(by))
	for _, w := range by {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pace() > out[j].pace() })
	return out, nil
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
		return "No process of this user wrote fast, at a look or since the run before. A root writer (Docker, a system service) cannot be seen, " +
			"nor what a process wrote once it ended under systemd --user or as an orphan, or under a parent that does not wait for it."
	}
	var parts []string
	for _, w := range d.Writers[:min(len(d.Writers), 4)] {
		s := fmt.Sprintf("%s (pid %d)", w.Comm, w.PID)
		if w.Over > 0 && w.Written > 0 {
			over := time.Duration(w.Over * float64(time.Second))
			if over >= time.Minute {
				over = over.Round(time.Minute)
			} else {
				over = over.Round(time.Second)
			}
			s += fmt.Sprintf(" wrote %s in %s", gib(w.Written), over)
		}
		if w.Rate >= 1 {
			s += fmt.Sprintf(", %.0f MiB/s at a look", w.Rate)
		}
		s += " in " + short(w.Cwd)
		if w.Session != "" {
			s += ", session " + w.Session
		}
		parts = append(parts, s)
	}
	return "Writers: " + strings.Join(parts, "; ") + "."
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
	freed := f.Freed
	switch {
	case kind == KindOffload:
		meta["bytes"], freed = offloadedBytes(f), offloadFreed(f)
	case kind == KindFailed && f.Rule == RuleScratch:
		freed = offloadFreed(f) // what its model data's deletion freed is on its own event
	}
	if freed > 0 {
		meta["freed"] = freed
	}
	if !f.Idle.IsZero() {
		meta["idle_since"] = f.Idle.Format(time.RFC3339)
	}
	if !f.Seen.IsZero() {
		meta["seen"] = f.Seen.Format(time.RFC3339)
	}
	if (kind == KindDelete || kind == KindOffload) && len(f.ownDirs) > 0 {
		meta["own"] = f.ownDirs // the directories it changed: a later run tells capacity's change from someone's
	}
	if f.Rule == RuleScratch {
		if len(f.files) > 0 {
			meta["model_files"], meta["model_bytes"] = len(f.files), f.modelBytes
		}
		if f.removed > 0 {
			meta["model_deleted"] = f.removed
		}
		if f.snapshot != "" {
			meta["snapshot"] = f.snapshot
		}
		if len(f.makers) > 0 {
			var by []map[string]string
			for _, mk := range f.makers {
				by = append(by, map[string]string{"session": mk.session, "harness": mk.harness, "at": mk.at.Format(time.RFC3339)})
			}
			meta["made_by"] = by
		}
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

// offloadedBytes is what of a task folder went to restic: all of it but the model data deleted before.
func offloadedBytes(f Finding) int64 { return f.Bytes - f.removed }

// offloadFreed is the space an offload freed: the act's, but what the model data's deletion before it freed.
func offloadFreed(f Finding) int64 { return max(0, f.Freed-f.modelFreed) }

func summary(kind string, f Finding) string {
	switch kind {
	case KindOffload:
		s := fmt.Sprintf("offloaded task folder %s to restic snapshot %s: %s, freed %s", short(f.Path), f.snapshot,
			gib(offloadedBytes(f)), gib(offloadFreed(f)))
		if f.removed > 0 {
			s += fmt.Sprintf(", its model data (%s) deleted before", gib(f.removed))
		}
		return s
	case KindDelete:
		if f.Rule == RuleScratch {
			return fmt.Sprintf("deleted the model data of task folder %s: %s, freed %s", short(f.Path), gib(f.removed), gib(f.Freed))
		}
		fallthrough
	case KindPrune:
		size, freed := gib(f.Bytes), gib(f.Freed)
		if f.unsized {
			size = "size unknown"
		}
		if f.unsaid {
			freed = "an amount the tool did not print"
		}
		return fmt.Sprintf("%s %s: %s, freed %s", ruleText[f.Rule], short(f.Path), size, freed)
	case KindFailed:
		if f.Rule == RuleScratch && f.modelOnly {
			return fmt.Sprintf("could not delete the model data of task folder %s: %s", short(f.Path), f.Err)
		}
		if f.Rule == RuleScratch {
			return fmt.Sprintf("could not offload task folder %s: %s", short(f.Path), f.Err)
		}
		return fmt.Sprintf("could not delete %s %s: %s", ruleText[f.Rule], short(f.Path), f.Err)
	}
	return fmt.Sprintf("%s %s (%s) left: %s", ruleText[f.Rule], short(f.Path), gib(f.Bytes), f.Why)
}

// runItem is what one deletion, offload or prune of a run took, for the run's note.
type runItem struct {
	rule, path string
	size       int64 // what went: the item, what of a task folder went to restic, its model data, what a prune freed
	freed      int64 // the space it freed on its disk
	offload    bool  // a task folder offloaded to restic
	model      bool  // a task folder's model data
	unsaid     bool  // a prune whose tool did not print what it freed
}

// itemOf is what a finding's act took, as the run's note counts it: an item's size, what of a task folder went to
// restic, a task folder's model data, or what a prune freed.
func itemOf(f Finding) runItem {
	it := runItem{rule: f.Rule, path: f.Path, size: f.Bytes, freed: f.Freed, unsaid: f.unsaid}
	switch {
	case f.Rule == RuleUV || f.Rule == RuleDocker:
		it.size = f.Freed // a prune's size is the whole cache's; what went is what it freed
	case f.Rule == RuleScratch && f.offloaded:
		it.offload, it.size, it.freed = true, offloadedBytes(f), offloadFreed(f)
	case f.Rule == RuleScratch:
		it.model, it.size = true, f.removed
	}
	return it
}

// runNote is a run's decided note, keyed by the run's start: what its deletions, offloads and prunes took so far.
func runNote(start, at time.Time, items []runItem) store.Note {
	return store.Note{ID: "capacity:run:" + strconv.FormatInt(start.UnixMilli(), 10), Kind: store.NoteDecided,
		Title: runTitle(items), Detail: runDetail(items), Source: store.Capacity, Importance: store.ImportanceNormal, At: at}
}

// runTitle is a run's effect in one line: what it freed, how many items each rule took, the task folders it offloaded
// with what went to restic and those whose model data alone went, and what it pruned ("Freed 12.4 GiB: 5 Rust build
// outputs, 2 leftover worktrees; pruned Docker"). A title too long for a line names the prunes by their tools, then
// counts them, then names the items shorter, then ends at the last whole clause that fits, with "and more".
func runTitle(items []runItem) string {
	var freed, restic int64
	unsaid, prunes := "", 0
	count, pruned := map[string]int{}, map[string]bool{}
	offloaded, modelOnly := map[string]bool{}, map[string]bool{}
	for _, it := range items {
		freed += it.freed
		switch {
		case it.rule == RuleUV || it.rule == RuleDocker:
			pruned[it.rule], prunes = true, prunes+1
			if it.unsaid {
				unsaid = pruneTool[it.rule]
			}
		case it.offload:
			offloaded[it.path], restic = true, restic+it.size
		case it.model:
			modelOnly[it.path] = true
		default:
			count[it.rule]++
		}
	}
	for p := range offloaded { // a folder whose model data went before its offload is one folder, offloaded
		delete(modelOnly, p)
	}
	clauses := func(names map[string]string) []string {
		var out []string
		for _, rule := range []string{RuleModels, RuleTargets, RuleWorktrees} {
			if n := count[rule]; n > 0 {
				out = append(out, fmt.Sprintf("%d %s", n, plural(names[rule], n)))
			}
		}
		if n := len(offloaded); n > 0 {
			out = append(out, fmt.Sprintf("%d %s (%s to restic)", n, plural(names[RuleScratch], n), noteSize(restic)))
		}
		if n := len(modelOnly); n > 0 {
			out = append(out, fmt.Sprintf("model data of %d %s", n, plural(names[RuleScratch], n)))
		}
		return out
	}
	head := "Freed " + noteSize(freed)
	switch {
	case unsaid != "" && freed < 1<<20:
		head = "Freed an amount " + unsaid + " did not print"
	case unsaid != "":
		head = "Freed at least " + gib(freed)
	}
	compose := func(cs []string, p string) string {
		list := strings.Join(cs, ", ")
		if list != "" && p != "" {
			list += "; "
		}
		return head + ": " + list + p
	}
	fits := func(s string) bool { return utf8.RuneCountInString(s) <= store.TitleMax }
	var long, brief []string
	if pruned[RuleUV] {
		long, brief = append(long, "the uv cache"), append(brief, pruneTool[RuleUV])
	}
	if pruned[RuleDocker] {
		long, brief = append(long, pruneTool[RuleDocker]), append(brief, pruneTool[RuleDocker])
	}
	type form struct {
		names  map[string]string
		prunes string
	}
	forms := []form{{countText, ""}, {briefText, ""}}
	if prunes > 0 {
		counted := fmt.Sprintf("%d %s", prunes, plural("prune", prunes))
		forms = []form{{countText, "pruned " + strings.Join(long, " and ")}, {countText, "pruned " + strings.Join(brief, " and ")},
			{countText, counted}, {briefText, counted}}
	}
	for _, f := range forms {
		if t := compose(clauses(f.names), f.prunes); fits(t) {
			return t
		}
	}
	last := forms[len(forms)-1]
	cs, p := clauses(last.names), last.prunes
	for p != "" || len(cs) > 1 { // whole clauses from the end, the prunes first
		if p != "" {
			p = ""
		} else {
			cs = cs[:len(cs)-1]
		}
		if t := compose(cs, p) + " and more"; fits(t) {
			return t
		}
	}
	return compose(cs, p) + " and more" // the store clips it
}

// noteSize is a size in the run's note: as gib says it, but "under 1 MiB" for one above zero that it would say as 0.
func noteSize(n int64) string {
	if n > 0 && n < 1<<20 {
		return "under 1 MiB"
	}
	return gib(n)
}

// detailMax is the longest detail a run's note has: the most the phone's view keeps of a detail.
const detailMax = 400

// runDetail is what a run took in plain sentences: its largest three items with their sizes; where each offloaded
// folder's pointer is, and that model data is not offloaded; how to keep an item next time, in one sentence; and last,
// what is not known of a prune and how what the prunes took comes back. A detail longer than detailMax leaves out the
// prunes' sentences, then names two items, then says how to keep one in fewer words, then leaves out the model data's
// sentence, then names one item, then leaves out the pointer's, and last clips its item: how to keep one stays.
func runDetail(items []runItem) string {
	var sized, offloads []runItem
	took, model, unsaid := map[string]bool{}, false, ""
	for _, it := range items {
		took[it.rule], model = true, model || it.model
		switch {
		case it.unsaid:
			unsaid = pruneTool[it.rule]
			continue // what it freed is not known
		case it.offload:
			offloads = append(offloads, it)
		}
		sized = append(sized, it)
	}
	sort.SliceStable(sized, func(i, j int) bool { return sized[i].size > sized[j].size })
	var pointer string
	switch {
	case len(offloads) == 1:
		pointer = short(offloads[0].path) + pointerSuffix + " says how to restore it."
	case len(offloads) > 1:
		pointer = "Each offloaded folder's NAME" + pointerSuffix + " beside it says how to restore it."
	}
	var long, brief []string
	for _, rule := range Rules {
		if took[rule] && keeps[rule] != "" {
			long, brief = append(long, keeps[rule]), append(brief, keepsBrief[rule])
		}
	}
	keep := ""
	switch n := len(long); {
	case n == 1:
		keep = "To keep one next time, " + long[0] + "."
	case n > 1:
		keep = "To keep one next time, " + strings.Join(long[:n-1], ", ") + " or " + long[n-1] + "."
	}
	keepBrief := ""
	if len(brief) > 0 {
		keepBrief = "To keep one next time: " + strings.Join(brief, ", ") + "."
	}
	var prunes []string
	if unsaid != "" {
		prunes = append(prunes, unsaid+" did not print what its prune freed.")
	}
	for _, rule := range Rules {
		if took[rule] && comesBack[rule] != "" {
			prunes = append(prunes, comesBack[rule])
		}
	}
	f := struct {
		n                             int
		prunes, brief, model, pointer bool
	}{n: 3, prunes: true, model: model, pointer: pointer != ""}
	parts := func() (string, []string) {
		var named []string
		for _, it := range sized[:min(f.n, len(sized))] {
			named = append(named, itemText(it))
		}
		lead := ""
		switch {
		case len(named) == 0:
		case len(named) < len(sized):
			lead = "Largest: " + strings.Join(named, ", ") + "."
		default:
			lead = upper(strings.Join(named, ", ")) + "."
		}
		var rest []string
		if f.pointer {
			rest = append(rest, pointer)
		}
		if f.model {
			rest = append(rest, modelDataGone)
		}
		k := keep
		if f.brief {
			k = keepBrief
		}
		if k != "" {
			rest = append(rest, k)
		}
		if f.prunes {
			rest = append(rest, prunes...)
		}
		return lead, rest
	}
	join := func(lead string, rest []string) string {
		if lead == "" {
			return strings.Join(rest, " ")
		}
		return strings.Join(append([]string{lead}, rest...), " ")
	}
	steps := []func(){
		func() { f.prunes = false }, func() { f.n = 2 }, func() { f.brief = true }, func() { f.model = false },
		func() { f.n = 1 }, func() { f.pointer = false },
	}
	for _, step := range append(steps, nil) {
		lead, rest := parts()
		if d := join(lead, rest); utf8.RuneCountInString(d) <= detailMax || step == nil {
			if room := detailMax - utf8.RuneCountInString(join("", rest)) - 1; utf8.RuneCountInString(d) > detailMax && room > 1 {
				d = join(plain.Clip(lead, room), rest) // an item whose name alone is too long
			}
			return d
		}
		step()
	}
	return ""
}

// itemText names what an item took, with its size.
func itemText(it runItem) string {
	switch {
	case it.rule == RuleUV || it.rule == RuleDocker:
		return "pruned " + short(it.path) + " " + noteSize(it.size)
	case it.offload:
		return short(it.path) + " " + noteSize(it.size) + " to restic"
	case it.model:
		return "model data in " + short(it.path) + " " + noteSize(it.size)
	}
	return short(it.path) + " " + noteSize(it.size)
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
	"pointer":     "beside the pointer of their offload, left",
	"stuck":       "whose model data this user cannot remove, left",
	"too-big":     "too big to offload in one run, left",
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
	case rule == RuleScratch && class == "unreadable":
		title = fmt.Sprintf("%d %s with directories capacity cannot read, left (%s)", len(items), plural(what, len(items)), gib(total))
	case rule == RuleScratch && class == "failed":
		title = fmt.Sprintf("%d %s that could not be offloaded (%s)", len(items), plural(what, len(items)), gib(total))
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
	if n == 1 || strings.HasSuffix(what, "data") || strings.HasSuffix(what, "cache") {
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
