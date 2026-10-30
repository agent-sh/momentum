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
//     went, its size, since when it was idle, the rule, and how to have it back or keep it;
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

func (r *runner) record(ctx context.Context) error {
	var evs []store.Event
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
				kind := KindDelete
				if f.Rule == RuleUV || f.Rule == RuleDocker {
					kind = KindPrune
				}
				e := r.event(kind, f, fmt.Sprintf("%s/%s/%s/%d", kind, f.Rule, keyPath(f), r.now.UnixMilli()), store.ActorAgent)
				evs = append(evs, e)
				if _, err := r.st.PutNote(ctx, decidedNote(e, f, r.now)); err != nil {
					return err
				}
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
	if _, err := r.st.Put(ctx, evs); err != nil {
		return err
	}
	for _, key := range sortedKeys(groups) {
		g := groups[key]
		stored, err := r.st.Stored(ctx, g.events)
		if err != nil {
			return err
		}
		fresh := false
		for _, e := range g.events {
			fresh = fresh || !stored[e.ID()]
		}
		if _, err := r.st.Put(ctx, g.events); err != nil {
			return err
		}
		id := "capacity:" + g.rule + ":" + g.class
		old, had, err := r.st.Note(ctx, id)
		if err != nil {
			return err
		}
		if fresh || (had && old.Done.IsZero()) {
			if _, err := r.st.PutNote(ctx, groupNote(id, g.rule, g.class, g.items, r.now)); err != nil {
				return err
			}
		}
	}
	// A problem note whose rule ran and found none of its items any more is resolved.
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
		}
	}
	if err := r.recordJobs(ctx); err != nil {
		return err
	}
	return r.recordDisks(ctx)
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
	stored, err := r.st.Stored(ctx, []store.Event{e})
	if err != nil {
		return err
	}
	if _, err := r.st.Put(ctx, []store.Event{e}); err != nil {
		return err
	}
	if !stored[e.ID()] || (had && old.Done.IsZero()) {
		_, err = r.st.PutNote(ctx, store.Note{ID: id, Kind: store.NoteProblem, Source: store.Capacity, At: r.now,
			Title:  "Capacity deleted nothing: the scheduled jobs could not all be read",
			Detail: "A job may name a path it would delete, so it waits until they can be read: " + r.namedErr.Error()})
	}
	return err
}

// recordDisks records each fast fill, and puts the disk's note once per fill: again only after a run without one.
func (r *runner) recordDisks(ctx context.Context) error {
	for _, d := range r.res.Disks {
		id := "capacity:fill:" + d.Disk
		if !d.Fast {
			continue
		}
		f := Finding{Rule: RuleDisk, Path: d.Disk, Detail: fillDetail(d)}
		e := r.event(KindFill, f, fmt.Sprintf("fill/%s/%d", d.Disk, r.now.UnixMilli()), store.ActorDetector)
		meta, _ := json.Marshal(d)
		e.Meta = string(meta)
		e.Summary = fillTitle(d)
		last, err := r.st.Mark(ctx, "fill:"+d.Disk)
		if err != nil {
			return err
		}
		if _, err := r.st.Put(ctx, []store.Event{e}); err != nil {
			return err
		}
		if err := r.st.SetMark(ctx, "fill:"+d.Disk, r.now); err != nil {
			return err
		}
		old, had, err := r.st.Note(ctx, id)
		if err != nil {
			return err
		}
		ongoing := !last.IsZero() && r.now.Sub(last) <= 2*time.Hour // the runs since have all seen it fill
		if had && !old.Done.IsZero() && ongoing {
			continue // resolved during this fill: it stays resolved
		}
		n := store.Note{ID: id, Kind: store.NoteProblem, Title: fillTitle(d), Detail: fillDetail(d), Source: store.Capacity,
			Importance: store.ImportanceHigh, At: r.now}
		if had && ongoing {
			n.At = old.At
		}
		if _, err := r.st.PutNote(ctx, n); err != nil {
			return err
		}
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
	if f.Rule == RuleUV || f.Rule == RuleDocker {
		title = fmt.Sprintf("Pruned %s: %s", f.Path, gib(f.Freed))
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
		Effect: "Freed " + gib(f.Freed), Source: store.Capacity, Importance: store.ImportanceNormal, At: now}
}

// groupTitles say what a rule's report-only items of one reason are.
var groupTitles = map[string]string{
	"excluded":    "in excluded projects, left for their owners",
	"unknown":     "of no known project, left",
	"tracked":     "that git does not ignore, left",
	"uncommitted": "with uncommitted work, left",
	"unpushed":    "with commits on no remote, left",
	"unreadable":  "git could not read, left",
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
	if rule == RuleDocker && class == "volumes" {
		title = fmt.Sprintf("Docker volumes no container uses hold %s, only reported", gib(total))
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
