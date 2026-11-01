package capacity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
//     what went, its size, the space it freed (the files whose last link it removed, or a prune's tool's own
//     figure, named as the tool's), since when it was idle and the rule;
//   - together they are the run's one decided note (capacity:run:MS, the run's start), put with each one's event in
//     one transaction, so a run stopped midway leaves a note of what it did until then: what the run freed on disk,
//     how many items each rule took, the largest of them, and how to keep one next time; a prune that removed nothing
//     is an event only, and a run that took nothing writes no note;
//   - each item only reported is an event (capacity-report), once; what a rule leaves by its own policy is no problem
//     and has no note (leftByDesign), and any other reason has its rule's problem note, which lists them; the note is
//     put again only when an item new to it appears, so a note the owner resolved stays resolved until the evidence
//     changes, and it is resolved by itself when the rule finds none;
//   - a deletion that failed is an event (capacity-failed) and its rule's problem note for failures;
//   - a rule that could not run, and a disk whose use could not be read, is an event (capacity-failed) and a problem
//     note that names the error (recordScans);
//   - a fast fill is an event (capacity-fill) and the disk's problem note, high importance, put once per fill; once it
//     ended, the note is resolved and the disk's decided note (capacity:filled:DISK) says what it filled.

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

// comesBack says how what a rule took comes back where the note says no other way: a removed worktree's work, and
// what the prunes took (there is nothing of theirs to keep).
var comesBack = map[string]string{
	RuleWorktrees: "A removed worktree's commits are merged or on its remote: git worktree add brings it back.",
	RuleUV:        "uv downloads the wheels it dropped again.",
	RuleDocker:    "Docker builds its cache again on the next build.",
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
	ran := map[string]bool{} // the rules whose scan ran whole: what they did not find is gone
	for _, rr := range r.res.Rules {
		if !rr.Ran {
			continue
		}
		whole := rr.Err == ""
		ran[rr.Rule] = whole
		for _, f := range rr.Findings {
			switch {
			case f.Done:
			case f.Err != "": // a deletion that failed is on record, whatever the scan did after it
				g := groupOf(groups, f.Rule, "failed")
				g.items = append(g.items, f)
				g.events = append(g.events, r.event(KindFailed, f, fmt.Sprintf("failed/%s/%s", f.Rule, keyPath(f)), store.ActorDetector))
			case f.Action == ActReport && whole:
				g := groupOf(groups, f.Rule, f.class)
				g.items = append(g.items, f)
				g.events = append(g.events, r.event(KindReport, f, fmt.Sprintf("report/%s/%s/%s", f.Rule, f.class, keyPath(f)), store.ActorDetector))
			}
		}
	}
	held, err := r.uvHold(ctx)
	if err != nil {
		return err
	}
	for _, key := range sortedKeys(groups) {
		g := groups[key]
		fresh, err := r.putNew(ctx, g.events)
		if err != nil {
			return err
		}
		if leftByDesign[g.class] {
			continue // the rule's own policy: its events and the run's output say so, and no note
		}
		n := groupNote("capacity:"+g.rule+":"+g.class, g.rule, g.class, g.items, r.now)
		if g.rule == RuleUV && g.class == "busy" {
			// a hold is a problem once it has lasted the rule's day, whatever holds the prune off: until then the run
			// and its event say why it waits, and a note that stands (an older build's, put at once) waits too
			if r.now.Sub(held) < uvQuietMax {
				if err := r.ended(ctx, n.ID); err != nil {
					return err
				}
				continue
			}
			old, had, err := r.st.Note(ctx, n.ID)
			if err != nil {
				return err
			}
			// due, unless the owner resolved it after it came due: then it stays resolved until the hold ends
			fresh = fresh || !had || old.Done.Before(held.Add(uvQuietMax))
			n.At = held // when the problem began
		}
		if err := r.putAgain(ctx, n, fresh); err != nil {
			return err
		}
	}
	// A problem note whose rule ran and found none of its items any more is resolved, by the rule: it stands again
	// when its items come back, unlike one the owner resolved. An older build's note for items left by design is
	// resolved by any run: it is no problem.
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
		switch {
		case !contains(Rules, rule): // a disk's, a scan's, a hold's or the jobs' note: their own records resolve them
		case leftByDesign[class]:
			if _, _, err := r.st.DoneNote(ctx, n.ID, r.now); err != nil {
				return err
			}
		case rule == RuleUV && class == "busy" && r.blind != "": // a look that may not see the holders: the hold goes on
		case ran[rule] && groups[rule+"\x00"+class] == nil:
			if err := r.ended(ctx, n.ID); err != nil {
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
	if err := r.recordDisks(ctx); err != nil {
		return err
	}
	return r.recordScans(ctx)
}

// putAgain puts a problem note when what it is about is new to the store (fresh), when it stands, or when it holds
// again after it ended (ended). One the owner resolved stays resolved while the same problem lasts. A note that stands
// keeps its time, when the problem began, and takes the latest title and detail.
func (r *runner) putAgain(ctx context.Context, n store.Note, fresh bool) error {
	old, had, err := r.st.Note(ctx, n.ID)
	if err != nil {
		return err
	}
	auto, err := r.st.Mark(ctx, "auto:"+n.ID)
	if err != nil {
		return err
	}
	standing := had && old.Done.IsZero()
	again := had && !old.Done.IsZero() && !auto.IsZero() && !old.Done.After(auto)
	if standing {
		n.At = old.At
	}
	if fresh || standing || again {
		_, err = r.st.PutNote(ctx, n)
	}
	return err
}

// ended says that what a problem note is about is over: a note that stands is resolved, and putAgain puts the note
// again when the problem comes back, whoever resolved it.
func (r *runner) ended(ctx context.Context, id string) error {
	old, had, err := r.st.Note(ctx, id)
	if err != nil || !had {
		return err
	}
	auto, err := r.st.Mark(ctx, "auto:"+id)
	if err != nil {
		return err
	}
	switch {
	case old.Done.IsZero():
		if _, _, err := r.st.DoneNote(ctx, id, r.now); err != nil {
			return err
		}
	case !auto.IsZero() && !old.Done.After(auto):
		return nil // the mark says already that it ended after its last resolution
	}
	return r.st.SetMark(ctx, "auto:"+id, r.now)
}

// recordScans records each rule that could not run (its scan failed, or its last scan's mark could not be read), a
// problem note each (capacity:scan:RULE), and the disks whose use could not be read, one note that names them
// (capacity:scan:disk). Each error is an event (capacity-failed), once; a note is put as putAgain puts one, and ends
// at the next run that scans without an error. A rule this run did not scan (not due, or not among --rule's) leaves
// its note as it is; a rule no longer configured ends its own.
func (r *runner) recordScans(ctx context.Context) error {
	failed := func(rule, key, summary, why string) store.Event {
		h := sha256.Sum256([]byte(why))
		e := r.event(KindFailed, Finding{Rule: rule, Path: key, Err: why}, "scan/"+key+"/"+hex.EncodeToString(h[:8]), store.ActorDetector)
		e.Summary = summary
		return e
	}
	type scan struct {
		id, title, detail string
		events            []store.Event // none: it worked
	}
	var scans []scan
	for _, rr := range r.res.Rules {
		if !rr.Ran && rr.Err == "" && r.cfg.configured(rr.Rule) {
			continue // not scanned on this run: nothing says whether it works now
		}
		s := scan{id: "capacity:scan:" + rr.Rule, title: "Capacity's " + rr.Rule + " rule could not run", detail: upper(rr.Err) + "."}
		if len(rr.Findings) > 0 { // it found items before the error: the run's output and events say what came of them
			s.title = "Capacity's " + rr.Rule + " rule stopped partway"
		}
		if rr.Err != "" {
			s.events = []store.Event{failed(rr.Rule, rr.Rule, "the "+rr.Rule+" rule could not run: "+rr.Err, rr.Err)}
		}
		scans = append(scans, s)
	}
	disks := scan{id: "capacity:scan:disk"}
	var names, unread []string
	for _, d := range r.res.Disks {
		if d.Err != "" {
			names, unread = append(names, d.Disk), append(unread, d.Disk+": "+d.Err)
			disks.events = append(disks.events, failed(RuleDisk, d.Disk, "the use of "+d.Disk+" could not be read: "+d.Err, d.Err))
		}
	}
	switch len(names) {
	case 0:
	case 1:
		disks.title = "Capacity cannot watch " + names[0] + ": its use could not be read"
		disks.detail = unread[0] + ". No fill or full note can come for it until its use can be read."
	default:
		disks.title = fmt.Sprintf("Capacity cannot watch %d disks: their use could not be read", len(names))
		disks.detail = strings.Join(unread, "; ") + ". No fill or full note can come for them until their use can be read."
	}
	scans = append(scans, disks)
	for _, s := range scans {
		if len(s.events) == 0 {
			if err := r.ended(ctx, s.id); err != nil {
				return err
			}
			continue
		}
		fresh, err := r.putNew(ctx, s.events)
		if err != nil {
			return err
		}
		if err := r.putAgain(ctx, store.Note{ID: s.id, Kind: store.NoteProblem, Source: store.Capacity, At: r.now,
			Title: s.title, Detail: s.detail}, fresh); err != nil {
			return err
		}
	}
	return nil
}

// recordHolds records a run that could not trust its look at the processes, or could not ask Docker which
// containers run, and so deleted nothing, or could not read the live sessions whole, and so left the task folders
// and worktrees: a problem note for each reason, put as putAgain puts one, and ended by the next run that has no such
// reason. Docker and the sessions are asked only when a rule needs them.
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
		if h.why == "" {
			if err := r.ended(ctx, h.id); err != nil {
				return err
			}
			continue
		}
		sum := sha256.Sum256([]byte(h.why))
		e := r.event(KindFailed, Finding{Rule: "hold", Path: h.id, Err: h.why}, "hold/"+hex.EncodeToString(sum[:8]), store.ActorDetector)
		fresh, err := r.putNew(ctx, []store.Event{e})
		if err != nil {
			return err
		}
		if err := r.putAgain(ctx, store.Note{ID: h.id, Kind: store.NoteProblem, Source: store.Capacity, At: r.now,
			Title: h.title, Detail: upper(h.why) + "."}, fresh); err != nil {
			return err
		}
	}
	return nil
}

// uvHeldMark keeps when the stretch of the uv rule's scans that hold its prune off began.
const uvHeldMark = "busy-open:uv"

// uvHold is when the uv rule's prune was first held off in the stretch of its scans this run's carries on, whatever
// held it off (its findings' class is busy, a quiet wait's too), or zero when its scan did not run whole or was not
// held. The first scan of a stretch sets the mark. A whole scan that was not held ends the stretch and clears the
// mark, before the stretch's note is resolved, so a run that stops between the two leaves no stretch behind: the
// next resolves the note. A run that cannot trust its look at the processes, which may not see the holders, ends no
// stretch, and a rule this run did not scan leaves it as it is.
func (r *runner) uvHold(ctx context.Context) (time.Time, error) {
	for _, rr := range r.res.Rules {
		if rr.Rule != RuleUV || !rr.Ran || rr.Err != "" {
			continue
		}
		held := false
		for _, f := range rr.Findings {
			held = held || f.class == "busy"
		}
		switch at, err := r.st.Mark(ctx, uvHeldMark); {
		case err != nil:
			return time.Time{}, err
		case !held && r.blind == "" && !at.IsZero():
			return time.Time{}, r.st.ClearMark(ctx, uvHeldMark)
		case !held:
			return time.Time{}, nil
		case at.IsZero():
			return r.now, r.st.SetMark(ctx, uvHeldMark, r.now)
		default:
			return at, nil
		}
	}
	return time.Time{}, nil
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

// recordJobs records a run that could not read the scheduled jobs, and so deleted nothing: a problem note, put as
// putAgain puts one, and ended by the next run that reads them all.
func (r *runner) recordJobs(ctx context.Context) error {
	const id = "capacity:jobs"
	if r.named == nil && r.namedErr == nil {
		return nil // no rule scanned: the jobs were not read
	}
	if r.namedErr == nil {
		return r.ended(ctx, id)
	}
	h := sha256.Sum256([]byte(r.namedErr.Error()))
	e := r.event(KindFailed, Finding{Rule: "jobs", Path: "scheduled jobs", Err: r.namedErr.Error()},
		"jobs/"+hex.EncodeToString(h[:8]), store.ActorDetector)
	fresh, err := r.putNew(ctx, []store.Event{e})
	if err != nil {
		return err
	}
	return r.putAgain(ctx, store.Note{ID: id, Kind: store.NoteProblem, Source: store.Capacity, At: r.now,
		Title:  "Capacity deleted and pruned nothing: the scheduled jobs could not all be read",
		Detail: "A job may name a path it would delete, so it waits until they can be read: " + r.namedErr.Error()}, fresh)
}

// recordDisks records each fast fill: an event at every run that sees it, and the disk's problem note, put once per
// fill. At the first run without it the fill is over: its problem note is resolved, and a decided note says what it
// filled, which the report shows for 12 hours as it shows every decided note. A disk above its full percent has a
// note of its own, put once and resolved when it drops below.
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

// fillID, fullID and filledID are a disk's notes: the problem notes of its fast fill and of its full percent, and the
// decided note of its last fill that ended, which the next fill's end replaces. They name the disk by its path,
// escaped where an id may not hold a character of it (store.IDPart): a path that needs no escape keeps the ids it
// always had.
func fillID(disk string) string   { return "capacity:fill:" + store.IDPart(disk) }
func fullID(disk string) string   { return "capacity:full:" + store.IDPart(disk) }
func filledID(disk string) string { return "capacity:filled:" + store.IDPart(disk) }

func (r *runner) recordFill(ctx context.Context, d DiskState) error {
	id := fillID(d.Disk)
	open, err := r.st.Mark(ctx, "fill-open:"+d.Disk)
	if err != nil {
		return err
	}
	old, had, err := r.st.Note(ctx, id)
	if err != nil {
		return err
	}
	// A fill that ended: one open that this run does not see, or one whose problem note stands with no fill open (an
	// older build kept it, rewritten to say the fill ended, until someone resolved it; a run that died once the mark
	// went leaves it so too). The mark goes first, so the next run ends a fill this run did not finish ending.
	var start time.Time
	switch {
	case !open.IsZero() && !d.Fast:
		start = open
		if err := r.st.ClearMark(ctx, "fill-open:"+d.Disk); err != nil {
			return err
		}
	case open.IsZero() && had && old.Done.IsZero():
		start = old.At
	}
	if !start.IsZero() {
		if err := r.filled(ctx, d, start); err != nil {
			return err
		}
	}
	if !d.Fast {
		return nil
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
		runs, err := r.fastRuns(ctx, d.Disk, open)
		if err != nil {
			return err
		}
		all.Writers = writersOf(runs)
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

// filled ends the fill that began at start (its first run): its problem note is resolved, and its decided note says
// when it began, when it ended, what the disk's use grew by between the two, and the writers its runs saw. It began
// where the window its first run read the rate over began: the growth that showed the fill is the fill's. It ended at
// the first sample after its last run (this run's, for a fill that ends on it), which the store keeps for a run that
// comes after one that died. The note is dated at the end, so the report shows it for 12 hours from then.
func (r *runner) filled(ctx context.Context, d DiskState, start time.Time) error {
	runs, err := r.fastRuns(ctx, d.Disk, start)
	if err != nil {
		return err
	}
	from, last := start, start
	if len(runs) > 0 { // newest first
		last = runs[0].at
		if first := runs[len(runs)-1]; first.state.WindowMS > 0 {
			from = first.at.Add(-time.Duration(first.state.WindowMS) * time.Millisecond)
		}
	}
	end := r.now
	after, err := r.st.DiskSamples(ctx, d.Disk, last.Add(time.Millisecond))
	if err != nil {
		return err
	}
	if len(after) > 0 {
		end = after[0].At
	}
	// The title spans the fill, its first run's window to its last fast run, and says what the disk gained in it. A drop
	// after the last fast run (a cleanup, a deleted checkpoint) is not the fill's, so it does not cut the figure.
	title := fmt.Sprintf("%s filled fast from %s to %s", d.Disk, from.Format("Jan 2 15:04"), endClock(from, last))
	base, err := r.st.DiskSamples(ctx, d.Disk, from)
	if err != nil {
		return err
	}
	peak, err := r.st.DiskSamples(ctx, d.Disk, last)
	if err != nil {
		return err
	}
	if len(base) > 0 && len(peak) > 0 && base[0].At.Before(peak[0].At) {
		title += " (" + signedGiB(peak[0].Used-base[0].Used) + ")"
	}
	all := d
	all.Writers = writersOf(runs)
	if _, err := r.st.PutNote(ctx, store.Note{ID: filledID(d.Disk), Kind: store.NoteDecided, Title: title,
		Detail: fillDetail(all), Source: store.Capacity, At: end}); err != nil {
		return err
	}
	if _, _, err := r.st.DoneNote(ctx, fillID(d.Disk), r.now); err != nil && !errors.Is(err, store.ErrNoNote) {
		return err
	}
	return nil
}

// endClock is a fill's end as its note says it: the time, and the date too when it is not the start's.
func endClock(start, end time.Time) string {
	if y, m, day := start.Date(); end.Year() == y && end.Month() == m && end.Day() == day {
		return end.Format("15:04")
	}
	return end.Format("Jan 2 15:04")
}

// signedGiB is a change in size, with its sign.
func signedGiB(n int64) string {
	if n < 0 {
		return "-" + gib(-n)
	}
	return "+" + gib(n)
}

// recordFull puts a disk's full note once per stretch above its full percent, dated when the stretch began, and
// resolves it when the disk drops below. A run that set the stretch's mark and put no note (an older build's, on an id
// the store refused, or one that died between the two) leaves it to the next, and so does one that cleared the mark
// and did not resolve the note.
func (r *runner) recordFull(ctx context.Context, d DiskState) error {
	id := fullID(d.Disk)
	open, err := r.st.Mark(ctx, "full-open:"+d.Disk)
	if err != nil {
		return err
	}
	n, had, err := r.st.Note(ctx, id)
	if err != nil {
		return err
	}
	full := d.pctFull >= r.cfg.Fill.Full
	switch {
	case full && (open.IsZero() || !had || n.At.Before(open)):
		at := open
		if open.IsZero() {
			at = r.now
			if err := r.st.SetMark(ctx, "full-open:"+d.Disk, r.now); err != nil {
				return err
			}
		}
		_, err := r.st.PutNote(ctx, store.Note{ID: id, Kind: store.NoteProblem, Source: store.Capacity, Importance: store.ImportanceHigh,
			At:     at,
			Title:  fmt.Sprintf("%s is %.0f%% used, above %.0f%%: %s free", d.Disk, d.pctFull, r.cfg.Fill.Full, gib(d.Avail)),
			Detail: "Capacity deletes only what its rules find idle; momentum capacity --dry-run --all lists what they leave, and why."})
		return err
	case !full:
		// the mark goes first: a run that dies before the note goes leaves it standing with no mark, and the next
		// resolves it, where a mark on a resolved note would read as the owner's resolution
		if !open.IsZero() {
			if err := r.st.ClearMark(ctx, "full-open:"+d.Disk); err != nil {
				return err
			}
		}
		if had && n.Done.IsZero() {
			if _, _, err := r.st.DoneNote(ctx, id, r.now); err != nil {
				return err
			}
		}
	}
	return nil
}

// fastRun is one run that saw a disk fill fast, from its event: when it was, and what it saw of the disk.
type fastRun struct {
	at    time.Time
	state DiskState
}

// fastRuns are the runs that saw the disk fill fast from since on, newest first, from their events.
func (r *runner) fastRuns(ctx context.Context, disk string, since time.Time) ([]fastRun, error) {
	evs, err := r.st.Events(ctx, store.Query{Source: store.Capacity, Kinds: []string{KindFill}, Since: since, Limit: math.MaxInt32})
	if err != nil {
		return nil, err
	}
	var out []fastRun
	for _, e := range evs {
		var past DiskState
		if json.Unmarshal([]byte(e.Meta), &past) == nil && past.Disk == disk {
			out = append(out, fastRun{at: e.TS, state: past})
		}
	}
	return out, nil
}

// writersOf are the writers of a fill's runs, each process once, with what it wrote over the runs it was a writer in,
// and its fastest look.
func writersOf(runs []fastRun) []Writer {
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
	for _, run := range runs { // newest first, this run's among them: its event is recorded first
		for _, w := range run.state.Writers {
			add(w)
		}
	}
	out := make([]Writer, 0, len(by))
	for _, w := range by {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pace() > out[j].pace() })
	return out
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
	// what went says what it freed, nothing among it; a failure, what went before it failed
	if (kind == KindDelete || kind == KindOffload || kind == KindPrune) && !f.Unsaid || freed > 0 {
		meta["freed"] = freed
	}
	if f.By != "" {
		meta["freed_by"] = f.By // the tool's own figure
	}
	if f.AtLeast {
		meta["at_least"] = true // the tool's figure is a lower bound
	}
	if f.Unsaid {
		meta["unsaid"] = true // the tool printed no figure
	}
	if f.Out > 0 {
		meta["out"] = f.Out
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
			gib(offloadedBytes(f)), noteSize(offloadFreed(f)))
		if f.removed > 0 {
			s += fmt.Sprintf(", its model data (%s) deleted before", gib(f.removed))
		}
		return s
	case KindDelete:
		if f.Rule == RuleScratch {
			return fmt.Sprintf("deleted the model data of task folder %s: %s, freed %s", short(f.Path), gib(f.removed), noteSize(f.Freed))
		}
		return fmt.Sprintf("%s %s: %s, freed %s", ruleText[f.Rule], short(f.Path), gib(f.Bytes), noteSize(f.Freed))
	case KindPrune:
		s := fmt.Sprintf("%s %s: %s", ruleText[f.Rule], short(f.Path), gib(f.Bytes))
		if f.unsized {
			s = fmt.Sprintf("%s %s: size unknown", ruleText[f.Rule], short(f.Path))
		}
		if f.Out > 0 {
			s += ", " + gib(f.Out) + " out of it"
		}
		switch {
		case f.Unsaid:
			return s + ", freed an amount " + pruneTool[f.Rule] + " did not print"
		case f.AtLeast:
			return s + ", freed at least " + noteSize(f.Freed) + byCount(f.By)
		}
		return s + ", freed " + noteSize(f.Freed) + byCount(f.By)
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
	size       int64  // what went: the item, what of a task folder went to restic, its model data, what left a cache
	freed      int64  // the space it freed on its disk: the files whose last link it removed, or a tool's own figure
	by         string // the tool whose figure freed is, or "" for capacity's own count
	went       int64  // what went, each inode once, as capacity counted it: more than freed is what is still linked
	offload    bool   // a task folder offloaded to restic
	model      bool   // a task folder's model data
	unsaid     bool   // a prune whose tool did not print what it freed
	atLeast    bool   // a prune whose tool said what it freed is at least its figure
}

// itemOf is what a finding's act took, as the run's note counts it: an item's size, what of a task folder went to
// restic, a task folder's model data, or what went out of a pruned cache (else what the prune's tool says it freed).
func itemOf(f Finding) runItem {
	it := runItem{rule: f.Rule, path: f.Path, size: f.Bytes, freed: f.Freed, by: f.By, went: f.went, unsaid: f.Unsaid,
		atLeast: f.AtLeast}
	switch {
	case f.Rule == RuleUV || f.Rule == RuleDocker:
		it.size = f.Out // a prune's size is the whole cache's
		if it.size == 0 {
			it.size = f.Freed
		}
	case f.Rule == RuleScratch && f.offloaded:
		it.offload, it.size, it.freed, it.went = true, offloadedBytes(f), offloadFreed(f), f.went-f.modelWent
	case f.Rule == RuleScratch:
		it.model, it.size, it.went = true, f.removed, f.modelWent
	}
	return it
}

// runNote is a run's decided note, keyed by the run's start: what its deletions, offloads and prunes took so far.
func runNote(start, at time.Time, items []runItem) store.Note {
	return store.Note{ID: "capacity:run:" + strconv.FormatInt(start.UnixMilli(), 10), Kind: store.NoteDecided,
		Title: runTitle(items), Detail: runDetail(items), Source: store.Capacity, Importance: store.ImportanceNormal, At: at}
}

// runTitle is a run's effect in one line: what it freed on disk, how many items each rule took, the task folders it
// offloaded with what went to restic and those whose model data alone went, and what it pruned ("Freed 12.4 GiB: 5
// Rust build outputs, 2 leftover worktrees; pruned Docker (1.2 GiB by Docker's count)"). What a prune done by a tool
// freed is the tool's own figure, named as the tool's and not in what the run freed. A title too long for a line
// names the prunes by their tools, then names the items shorter, then counts the prunes, then ends at the last whole
// clause that fits, with "and more".
func runTitle(items []runItem) string {
	var freed, restic int64
	counted, prunes := false, 0
	count, pruned := map[string]int{}, map[string]bool{}
	offloaded, modelOnly := map[string]bool{}, map[string]bool{}
	type figure struct {
		n            int64
		said, unsaid bool
	}
	figures := map[string]*figure{} // per rule whose prune a tool did, the tool's own figure
	for _, it := range items {
		if it.by == "" && !it.unsaid {
			freed, counted = freed+it.freed, true
		} else {
			g := figures[it.rule]
			if g == nil {
				g = &figure{}
				figures[it.rule] = g
			}
			if it.unsaid || it.atLeast { // its figure, if any, is a lower bound
				g.unsaid = true
			}
			if !it.unsaid {
				g.n, g.said = g.n+it.freed, true
			}
		}
		switch {
		case it.rule == RuleUV || it.rule == RuleDocker:
			pruned[it.rule], prunes = true, prunes+1
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
	head := "" // a run whose only act is a tool's prune has no count of its own
	if counted {
		head = "Freed " + noteSize(freed)
	}
	compose := func(cs []string, p string) string {
		list := strings.Join(cs, ", ")
		if list != "" && p != "" {
			list += "; "
		}
		if head == "" {
			return upper(list + p)
		}
		return head + ": " + list + p
	}
	fits := func(s string) bool { return utf8.RuneCountInString(s) <= store.TitleMax }
	var long, brief []string
	for _, rule := range []string{RuleUV, RuleDocker} {
		if !pruned[rule] {
			continue
		}
		name := map[string]string{RuleUV: "the uv cache", RuleDocker: pruneTool[RuleDocker]}[rule]
		switch g := figures[rule]; {
		case g == nil: // capacity's own count, in what the run freed
		case g.said && g.unsaid:
			name += fmt.Sprintf(" (at least %s by %s's count)", noteSize(g.n), pruneTool[rule])
		case g.said:
			name += fmt.Sprintf(" (%s by %s's count)", noteSize(g.n), pruneTool[rule])
		default:
			name += fmt.Sprintf(" (%s printed no total)", pruneTool[rule])
		}
		long, brief = append(long, name), append(brief, pruneTool[rule])
	}
	type form struct {
		names  map[string]string
		prunes string
	}
	forms := []form{{countText, ""}, {briefText, ""}}
	if prunes > 0 {
		counted := fmt.Sprintf("%d %s", prunes, plural("prune", prunes))
		tools := "pruned " + strings.Join(brief, " and ")
		forms = []form{{countText, "pruned " + strings.Join(long, " and ")}, {countText, tools}, {briefText, tools}, {briefText, counted}}
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

// noteSize is a size in the run's note: as gib says it, but "under 1 MiB" for one above zero and below 1 MiB.
func noteSize(n int64) string {
	if n > 0 && n < 1<<20 {
		return "under 1 MiB"
	}
	return gib(n)
}

// detailMax is the longest detail a run's note has: the most the phone's view keeps of a detail.
const detailMax = 400

// runDetail is what a run took in plain sentences: its largest three items with their sizes, and what each freed where
// that is another size; where each offloaded folder's pointer is, and that model data is not offloaded; how to keep an
// item next time, in one sentence; and last, that a file linked elsewhere frees nothing until its last link goes
// (when a deletion freed less than went with it), what is not known of a prune, and how a removed worktree and what the
// prunes took come back. A detail longer than detailMax leaves out those last sentences, the last first, then names two
// items, then says how to keep one in fewer words with three items again, then two, then leaves out the model data's
// sentence, then names one item, then leaves out the pointer's, and last clips its item: how to keep one stays.
func runDetail(items []runItem) string {
	var sized, offloads []runItem
	took, model, unsaid, linked := map[string]bool{}, false, "", false
	for _, it := range items {
		took[it.rule], model = true, model || it.model
		switch {
		case it.unsaid && it.size == 0:
			unsaid = pruneTool[it.rule]
			continue // nothing of it is known
		case it.unsaid: // what left the cache is known, and the item says its tool printed no total
		case it.offload:
			offloads = append(offloads, it)
			fallthrough
		default: // a deletion that took more than it freed left files linked elsewhere; a tool's figure says nothing of it
			linked = linked || it.went > it.freed && noteSize(it.went) != noteSize(it.freed)
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
	var extra []string // what a detail short of room leaves out first
	if linked {
		extra = append(extra, linkedElsewhere)
	}
	if unsaid != "" {
		extra = append(extra, unsaid+" did not print what its prune freed.")
	}
	for _, rule := range Rules {
		if took[rule] && comesBack[rule] != "" {
			extra = append(extra, comesBack[rule])
		}
	}
	f := struct {
		n, extra              int
		brief, model, pointer bool
	}{n: 3, extra: len(extra), model: model, pointer: pointer != ""}
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
		return lead, append(rest, extra[:f.extra]...)
	}
	join := func(lead string, rest []string) string {
		if lead == "" {
			return strings.Join(rest, " ")
		}
		return strings.Join(append([]string{lead}, rest...), " ")
	}
	var steps []func()
	for range extra { // the last sentence first: why less was freed than went is the last to go
		steps = append(steps, func() { f.extra-- })
	}
	steps = append(steps, func() { f.n = 2 }, func() { f.brief, f.n = true, 3 }, func() { f.n = 2 },
		func() { f.model = false }, func() { f.n = 1 }, func() { f.pointer = false })
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

// itemText names what an item took, with its size, and what it freed on disk where that is another size: a file
// still linked elsewhere frees nothing. A tool's own figure is named as the tool's.
func itemText(it runItem) string {
	s := short(it.path) + " " + noteSize(it.size)
	switch {
	case it.rule == RuleUV || it.rule == RuleDocker:
		s = "pruned " + s
	case it.offload:
		s += " to restic"
	case it.model:
		s = "model data in " + s
	}
	freed := noteSize(it.freed)
	if it.atLeast {
		freed = "at least " + freed
	}
	switch {
	case it.unsaid:
		s += " (" + pruneTool[it.rule] + " printed no total)"
	case it.by != "" && freed == noteSize(it.size):
		s += " (" + it.by + "'s count)"
	case it.by != "":
		s += " (" + freed + " freed by " + it.by + "'s count)"
	case freed != noteSize(it.size):
		s += " (" + freed + " freed)"
	}
	return s
}

// linkedElsewhere says, in a run whose deletion freed less than went with it, why: the file's other links, which
// a later deletion of the run may take too.
const linkedElsewhere = "A file linked elsewhere frees nothing until its last link goes."

// leftByDesign are the reasons a rule leaves an item by its own policy: whose the item is, the work it holds, a
// mount it must not reach past, a pointer it would overwrite, a volume that may hold data. They are no problem: their
// items are events, and the run lists them, with no note. Any other reason is a problem, with its rule's note: the
// rule would take the item and something outside the rule keeps it (uv's prune that processes or a lock hold off,
// busy; an offload out of its time, too-big; model data this user cannot remove, stuck; an item it cannot read,
// unreadable), or a deletion failed. A reason new to this list is a problem until it is placed here.
var leftByDesign = map[string]bool{"excluded": true, "unknown": true, "tracked": true, "uncommitted": true,
	"unpushed": true, "ignored": true, "mount": true, "pointer": true, "volumes": true}

// groupTitles say what a rule's items of one problem reason are.
var groupTitles = map[string]string{
	"unreadable": "git could not read, left",
	"failed":     "that could not be deleted",
	"stuck":      "whose model data this user cannot remove, left",
	"too-big":    "too big to offload in one run, left",
}

// groupNote is the problem note of one rule's items for one problem reason.
func groupNote(id, rule, class string, items []Finding, now time.Time) store.Note {
	sort.SliceStable(items, func(i, j int) bool { return items[i].Bytes > items[j].Bytes })
	var total int64
	for _, f := range items {
		total += f.Bytes
	}
	what, why := ruleText[rule], groupTitles[class]
	if why == "" { // a reason with no title of its own: its name says it
		why = "left: " + class
	}
	title := fmt.Sprintf("%d %s %s (%s)", len(items), plural(what, len(items)), why, gib(total))
	switch {
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
