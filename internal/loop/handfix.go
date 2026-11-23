package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/detect"
	"github.com/agent-sh/momentum/internal/measure"
	"github.com/agent-sh/momentum/internal/measure/rekey"
	"github.com/agent-sh/momentum/internal/store"
)

// HandFixWindow is the window a fix by hand is measured over, before it and after it: a week, the loop's shortest
// whole-weeks window, so both windows hold the same days.
const HandFixWindow = 7 * 24 * time.Hour

// measureWorking is the measure step's part for the cases that have no change of the loop's in place (open,
// investigating or testing, of a family whose detector reads the store alone, detect.Replayable). Each closes on
// what the stored history shows, never on a person's word:
//   - as a detector fix, when its evidence of the last week is in the store and momentum's own detector keys none of
//     it by the case's signatures now: its problem is not seen any more because the detector keys it otherwise, or no
//     longer as a failure, not because it stopped; the close says which (detectorFix).
//   - as improved, when a fix by hand is on record (named with `case fixed`, or seen landing at the place its
//     investigation names: a file's change, a commit there) and its signatures' recurrence in the week after the fix is
//     measurably below the week before it, by the measure step's own test (handFixed).
func (r *Runner) measureWorking(ctx context.Context, o Options) ([]Measured, error) {
	// each family's cases are read on their own: a row of one family that cannot be read stops no other family's, and
	// the pass's picks report it
	var todo []store.Case
	for _, family := range detect.Replayable {
		for _, status := range []store.CaseStatus{store.CaseOpen, store.CaseInvestigating, store.CaseTesting} {
			cs, err := r.Store.Cases(ctx, store.CaseFilter{Family: family, Statuses: []store.CaseStatus{status}})
			if err != nil {
				r.logf("measure: the %s cases of %s cannot be read: %v", status, family, err)
				continue
			}
			for _, c := range cs {
				if o.takes(c) && len(c.Signatures) > 0 {
					todo = append(todo, c)
				}
			}
		}
	}
	if len(todo) == 0 {
		return nil, nil
	}
	now := r.now()
	week, err := r.keyed(ctx, now.Add(-HandFixWindow), now)
	if err != nil {
		r.logf("measure: the detectors could not key the last week: %v", err)
		return nil, nil
	}
	var out []Measured
	for _, c := range todo {
		m, ok, err := r.measureOne(ctx, c, week, now, o.Dry)
		if err != nil {
			r.logf("case %s: %v", c.ID, err)
			continue
		}
		if ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// keyedSpan is what the detectors key over a span, by event id, with the events.
type keyedSpan struct {
	since, until time.Time
	sig          map[string]string
	events       map[string]store.Event
}

// keyed keys the store's events over a span with the detectors this binary holds, the owner-burden detector with the
// program prefixes the runner's own has.
func (r *Runner) keyed(ctx context.Context, since, until time.Time) (keyedSpan, error) {
	var programs detect.Programs
	for _, d := range r.Detectors {
		if o, ok := d.(detect.OwnerBurden); ok {
			programs = o.Programs
		}
	}
	ks, err := detect.KeyedEvents(ctx, r.Store, detect.Window{Since: since, Until: until}, programs)
	if err != nil {
		return keyedSpan{}, err
	}
	out := keyedSpan{since: since, until: until, sig: map[string]string{}, events: map[string]store.Event{}}
	for _, k := range ks {
		id := k.Event.ID()
		if _, ok := out.sig[id]; !ok {
			out.sig[id], out.events[id] = k.Signature, k.Event
		}
	}
	return out, nil
}

// measureOne decides one working case; ok is false when there is nothing to say of it.
func (r *Runner) measureOne(ctx context.Context, c store.Case, week keyedSpan, now time.Time, dry bool) (Measured, bool, error) {
	m := Measured{Case: c.ID, Title: c.Title}
	if live, err := r.livePackage(c); err != nil || live {
		return m, false, nil // a change of the loop's is on its way, or a package its own steps report: they decide the case
	}
	if !dry {
		// a case another ship step holds is decided on a later pass: nothing here is urgent
		unlock, err := r.Ship.lock(ctx, c.ID)
		if err != nil {
			return m, false, nil
		}
		defer unlock()
	}
	if done, effect, err := r.detectorFix(ctx, c, week); err != nil {
		return m, false, err
	} else if done {
		m.Status, m.Why = "detector fix", effect
		if !dry {
			if _, err := r.Store.CloseDetectorFix(ctx, c.ID, effect, "loop:measure"); err != nil {
				return m, false, err
			}
		}
		return m, true, nil
	}
	if c.Status == store.CaseTesting && !dry {
		if err := r.seeHandFixes(ctx, c); err != nil {
			r.logf("case %s: a fix by hand could not be looked for: %v", c.ID, err)
		}
	}
	fixes, err := r.Store.HandFixes(ctx, c.ID)
	if err != nil {
		return m, false, err
	}
	for _, f := range fixes {
		if f.Measured {
			continue
		}
		return r.handFixed(ctx, c, f, now, dry)
	}
	return m, false, nil
}

// livePackage says whether the case has a package of the loop's that has not ended: its own steps decide the case.
func (r *Runner) livePackage(c store.Case) (bool, error) {
	if r.Ship == nil {
		return false, nil
	}
	p, ok, err := r.Ship.Load(c.ID)
	if err != nil || !ok {
		return false, err
	}
	return !ended(p), nil
}

// detectorFix says whether the case's problem is no longer seen only because the detector keys it otherwise now: its
// evidence of the span holds events the store still has, and the detector keys none of them by the case's own
// signatures. The effect says what it keys them as: re-keyed (by another signature, with the most-used one named) or
// dropped (no longer a failure).
func (r *Runner) detectorFix(ctx context.Context, c store.Case, span keyedSpan) (bool, string, error) {
	// only a case the detectors opened holds what they keyed: a case a person or an agent opened, or evidence filed by
	// hand, may hold events no detector ever keyed by its signatures
	if opened, err := r.detectorOpened(ctx, c); err != nil || !opened {
		return false, "", err
	}
	ids, err := r.Store.EvidenceEvents(ctx, c.ID, span.since)
	if err != nil {
		return false, "", err
	}
	held := 0
	rekeyed, dropped := map[string]int{}, 0
	for _, id := range ids {
		e, ok, err := r.Store.EventByID(ctx, id)
		if err != nil {
			return false, "", err
		}
		if !ok || e.TS.Before(span.since) || !e.TS.Before(span.until) || store.InDir(e.Cwd, r.Dir) {
			continue
		}
		held++
		sig, keyed := span.sig[id]
		switch {
		case keyed && slices.Contains(c.Signatures, sig):
			return false, "", nil // still keyed as the case keys it
		case keyed:
			rekeyed[sig]++
		default:
			dropped++
		}
	}
	if held < detect.FailuresRecur.Events {
		return false, "", nil // fewer than a finding holds: too little to say the detector keys the problem otherwise
	}
	counts := []rekey.Count{}
	for s, n := range rekeyed {
		counts = append(counts, rekey.Count{Signature: s, N: n})
	}
	sort.Slice(counts, func(i, j int) bool {
		return counts[i].N > counts[j].N || counts[i].N == counts[j].N && counts[i].Signature < counts[j].Signature
	})
	var parts []string
	if n := rekey.Events(counts); n > 0 {
		parts = append(parts, fmt.Sprintf("re-keyed: %d of its %d events of the last week are keyed now by %d other signatures, the most by %s", n, held, len(counts), plainClip(counts[0].Signature, 160)))
	}
	if dropped > 0 {
		parts = append(parts, fmt.Sprintf("dropped: %d of its %d events of the last week are no failures to the detector now", dropped, held))
	}
	return true, "momentum's own detector no longer keys its problem as this case does (" + strings.Join(parts, "; ") + "): not measured as an improvement", nil
}

// DetectorActor is who the loop's detectors file their findings as: the pass's detect step.
const DetectorActor = "loop:detect"

// detectorOpened says whether the loop's detectors opened the case, by its log.
func (r *Runner) detectorOpened(ctx context.Context, c store.Case) (bool, error) {
	log, err := r.Store.CaseLog(ctx, c.ID)
	if err != nil {
		return false, err
	}
	for _, e := range log {
		if e.Action == "opened" {
			return e.Actor == DetectorActor, nil
		}
	}
	return false, nil
}

// handFixed measures a fix by hand once the week after it has passed and every source it counts has read past it: the
// case's signatures' events, keyed by this binary's detectors, in the session hours (or the machine's, or the owner's)
// of the week before the fix against the week after (measure.Compare). A rate measurably lower closes the case as
// improved; anything else records what it found, and the case stays as it is.
func (r *Runner) handFixed(ctx context.Context, c store.Case, f store.HandFix, now time.Time, dry bool) (Measured, bool, error) {
	m := Measured{Case: c.ID, Title: c.Title, Due: f.At.Add(HandFixWindow)}
	if now.Before(m.Due) {
		m.Status, m.Why = "measuring", fmt.Sprintf("a fix by hand (%s) is measured once the week after it has passed", f.Ref)
		return m, true, nil
	}
	span, err := r.keyed(ctx, f.At.Add(-HandFixWindow), m.Due)
	if err != nil {
		return m, false, err
	}
	var before, after []store.Event
	sources := []string{}
	for id, sig := range span.sig {
		e := span.events[id]
		if !slices.Contains(c.Signatures, sig) || store.InDir(e.Cwd, r.Dir) {
			continue
		}
		if !slices.Contains(sources, e.Source) {
			sources = append(sources, e.Source)
		}
		if e.TS.Before(f.At) {
			before = append(before, e)
		} else {
			after = append(after, e)
		}
	}
	record := func(effect string) (Measured, bool, error) {
		m.Status, m.Why = "no measured effect", effect
		if !dry {
			if err := r.Store.HandFixMeasured(ctx, c.ID, f, effect, "loop:measure"); err != nil {
				return m, false, err
			}
		}
		return m, true, nil
	}
	if len(before) == 0 {
		if done, effect, err := r.detectorFix(ctx, c, keyedSpan{since: span.since, until: f.At, sig: span.sig, events: span.events}); err != nil {
			return m, false, err
		} else if done {
			m.Status, m.Why = "detector fix", effect
			if !dry {
				_, err = r.Store.CloseDetectorFix(ctx, c.ID, effect, "loop:measure")
			}
			return m, true, err
		}
		return record(fmt.Sprintf("nothing to measure the fix by hand (%s) by: no event of the case's signatures in the week before it", f.Ref))
	}
	sort.Strings(sources)
	units := unitsOf(before)
	plan := Plan{Signal: Signal{Source: sources[0]}, Units: units, Sources: sources}
	if unread, err := unreadWindow(ctx, r.Store, plan, f.At.Add(-HandFixWindow), m.Due, r.Dir); err != nil {
		return m, false, err
	} else if len(unread) > 0 {
		m.Status, m.Why = "waiting", "the store has not read past the week after the fix by hand: "+strings.Join(unread, ", ")
		return m, true, nil
	}
	b, err := tallySources(ctx, r.Store, plan.Signal, units, before, f.At.Add(-HandFixWindow), f.At, r.Dir, sources)
	if err != nil {
		return m, false, err
	}
	a, err := tallySources(ctx, r.Store, plan.Signal, units, after, f.At, m.Due, r.Dir, sources)
	if err != nil {
		return m, false, err
	}
	res, compared := measure.Compare(b, a)
	if compared {
		m.Result = &res
	}
	if !compared || !res.Improved {
		why := "no measured effect of the fix by hand (" + f.Ref + "): " + res.Effect
		if !compared {
			why = "nothing to compare the fix by hand (" + f.Ref + ") by: no activity in one of its weeks"
		}
		return record(why)
	}
	m.Status, m.Why = "improved", "a fix by hand: "+f.Ref
	if dry {
		return m, true, nil
	}
	if err := r.noteOnce(ctx, store.Note{ID: "shipped:" + store.IDPart(c.ID) + ":hand:" + strconv.FormatInt(f.At.UnixMilli(), 10), Kind: store.NoteShipped,
		Title:  plainClip("improved by a fix by hand: "+c.Title, store.TitleMax),
		Detail: fmt.Sprintf("A fix made by hand (%s, %s) measurably cut the problem over the week after it.", f.Ref, f.How), Effect: res.Effect}); err != nil {
		return m, false, err
	}
	if _, err := r.Store.CloseHandFixed(ctx, c.ID, f, HandFixWindow, b.Hits, a.Hits, res.Effect, "loop:measure"); err != nil {
		return m, false, err
	}
	return m, true, nil
}

// placePath finds the paths a place names: a whole path or one under the home, up to a space or a punctuation mark that
// ends it.
var placePath = regexp.MustCompile(`(?:^|[\s(])((?:~/|/)[^\s,;()'"]+)`)

// repoPath finds the paths of a repository a fix names: words with a slash and a file's ending.
var repoPath = regexp.MustCompile(`[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)+\.[A-Za-z0-9]+`)

// seeHandFixes records the fixes by hand seen landing at the place a testing case's investigation names, since the
// case went to testing in its episode: a file the place names that changed after that, or a commit of its repository's
// default branch that touches a file the case's fix or mechanism names.
func (r *Runner) seeHandFixes(ctx context.Context, c store.Case) error {
	log, err := r.Store.CaseLog(ctx, c.ID)
	if err != nil {
		return err
	}
	var since time.Time
	var place, repo string
	for _, e := range log {
		switch {
		case e.Action == "set" && strings.Contains(e.Detail, "-> testing"):
			since = e.At
		case e.Action == repoAction:
			var v struct {
				Repo, Place string
				Episode     int64
			}
			if json.Unmarshal([]byte(e.Detail), &v) == nil && v.Episode == c.Episode {
				place, repo = v.Place, v.Repo
			}
		}
	}
	if since.IsZero() {
		return nil
	}
	for _, m := range placePath.FindAllStringSubmatch(place, -1) {
		name := strings.TrimRight(m[1], ".:")
		full := name
		if rest, ok := strings.CutPrefix(name, "~/"); ok {
			full = filepath.Join(r.Inputs.Home, rest)
		}
		info, err := os.Lstat(full)
		if err != nil || !info.Mode().IsRegular() || !info.ModTime().After(since) || info.ModTime().After(time.Now()) {
			continue
		}
		if _, err := r.Store.RecordHandFix(ctx, c.ID, "file:"+name, info.ModTime(), store.HowSeen, "loop:measure"); err != nil {
			return err
		}
	}
	if repo == "" {
		return nil
	}
	dir := r.readDirSource(repo)
	identity, err := r.repoIdentity(ctx, repo)
	if dir == "" || err != nil {
		return err
	}
	named := map[string]bool{}
	for _, p := range repoPath.FindAllString(c.NextTest+" "+c.Mechanism, -1) {
		named[strings.TrimPrefix(p, "./")] = true
	}
	if len(named) == 0 {
		return nil
	}
	ref := "HEAD"
	if _, err := gitOutput(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/HEAD"); err == nil {
		ref = "refs/remotes/origin/HEAD"
	}
	out, err := gitOutput(ctx, dir, "log", "--no-merges", "--format=%x00%H %ct", "--name-only", "--since="+since.UTC().Format(time.RFC3339), ref, "--")
	if err != nil {
		return err
	}
	for _, commit := range strings.Split(out, "\x00") {
		lines := strings.Split(strings.TrimSpace(commit), "\n")
		head := strings.Fields(lines[0])
		if len(head) != 2 {
			continue
		}
		at, err := strconv.ParseInt(head[1], 10, 64)
		if err != nil || !time.Unix(at, 0).After(since) {
			continue
		}
		if !slices.ContainsFunc(lines[1:], func(f string) bool { return named[strings.TrimSpace(f)] }) {
			continue
		}
		if _, err := r.Store.RecordHandFix(ctx, c.ID, "git:"+identity+"@"+head[0], time.Unix(at, 0), store.HowSeen, "loop:measure"); err != nil {
			return err
		}
	}
	return nil
}

// detectorFixed closes a deployed case whose own change was to the detectors, and re-keyed or dropped the case's own
// events in its replay (SelfReplay): its problem is no longer seen because the detector keys it otherwise now, which
// no rate measures, so it closes as a detector fix once the change is in effect, and its package is kept.
func (r *Runner) detectorFixed(ctx context.Context, c store.Case, p *Package, m Measured, rekeyed, dropped int, dry bool) Measured {
	var parts []string
	if rekeyed > 0 {
		parts = append(parts, fmt.Sprintf("re-keyed: %d of its events in the replay are keyed now by other signatures", rekeyed))
	}
	if dropped > 0 {
		parts = append(parts, fmt.Sprintf("dropped: %d of its events in the replay are no failures to the detector now", dropped))
	}
	effect := "its change was to momentum's own detector (" + strings.Join(parts, "; ") + "): not measured as an improvement"
	m.Status, m.Why = "detector fix", effect
	if dry {
		return m
	}
	if err := r.noteOnce(ctx, store.Note{ID: noteID("decided", *p), Kind: store.NoteDecided, Title: plainClip("momentum fixed its detector: "+c.Title, store.TitleMax),
		Detail: "Its change keys the problem's events otherwise now: the case closes as a detector fix, not as an improvement.", Effect: effect, Links: prLinks(p.PR)}); err != nil {
		m.Status, m.Why = "failed", "the note: "+err.Error()
		return m
	}
	if _, err := r.Store.CloseDetectorFix(ctx, c.ID, effect, "loop:measure"); err != nil {
		m.Status, m.Why = "failed", err.Error()
		return m
	}
	kept, err := r.Ship.keepLocked(c.ID, measure.Result{Effect: effect})
	if err != nil {
		m.Status, m.Why = "failed", err.Error()
		return m
	}
	r.resolve(ctx, &kept)
	return m
}
