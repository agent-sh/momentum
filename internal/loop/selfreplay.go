package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/detect"
	"github.com/agent-sh/momentum/internal/measure"
	"github.com/agent-sh/momentum/internal/measure/rekey"
	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/sandbox"
	"github.com/agent-sh/momentum/internal/store"
)

// SelfReplay is what a change to the loop's own detectors does to the stored history: the window replayed and how
// the change keys the events the detectors key there, against its base (rekey.Diff). The package keeps it, so the
// measure step can tell a problem that stopped from one the detector keys otherwise now.
type SelfReplay struct {
	Since time.Time  `json:"since"`
	Until time.Time  `json:"until"`
	Diff  rekey.Diff `json:"diff"`
	// Own are the case's signatures at the check: the events the change may drop or re-key are theirs alone.
	Own []string `json:"own,omitempty"`
	// OwnKept counts the case's own events the change keys as the base does: a change that leaves some of them keyed
	// so has not moved the case's whole problem, which its measurement then reads.
	OwnKept int `json:"own_kept"`
	// OwnSeen are the case's signatures the base keyed events of in the window: a signature with none there, the
	// replay says nothing of.
	OwnSeen []string `json:"own_seen,omitempty"`
	// Paths are the paths the replayed change touches: what the replay read, for the judgment of it (replayJudged).
	Paths []string `json:"paths,omitempty"`
}

// Summary is the replay in counts, for the case's log, the check's note and the review: no signature's text, which
// comes from the store.
func (r SelfReplay) Summary() string {
	d := r.Diff
	return fmt.Sprintf("detector replay over %s to %s: %d events keyed before, %d after; %d re-keyed under %d other signatures, %d dropped of %d signatures, %d added",
		r.Since.UTC().Format(time.DateOnly), r.Until.UTC().Format(time.DateOnly), d.Before, d.After, rekey.Moved(d.Rekeyed), len(d.Rekeyed),
		rekey.Events(d.Dropped), len(d.Dropped), rekey.Events(d.Added))
}

// ReplayWindow is how much stored history a detector change is replayed over: the window a detectors' run reads,
// up to the end of the store's copy.
const ReplayWindow = detect.DefaultWindow

// replayPlan is the plan of a detector fix (replayJudges): such a case's problem is the detector's own keying, which
// no forward count of a signal measures, and the replay, which refused any move of another signature's events, is its
// measure. The plan counts nothing and closes the case as a detector fix once the change is in effect
// (Runner.detectorFixed). Any other change gets nil, and its plan is counted (validPlan).
func (s *Ship) replayPlan(ctx context.Context, c store.Case, p Package, pr Proposal, at time.Time) (*Plan, error) {
	if judged, err := s.replayJudges(ctx, c, p); err != nil || !judged {
		return nil, err
	}
	if !measure.WholeWeeks(pr.Window) {
		return nil, refused("a measurement window is a whole number of weeks (7d, 14d and so on)")
	}
	if err := installedFile(pr.Installed); err != nil {
		return nil, err
	}
	return &Plan{Signal: pr.Signal, Window: pr.Window, At: at, Installed: pr.Installed, Replay: true}, nil
}

// replayJudges says whether a package's checked change is judged by its detector replay: a change of the loop's own
// repository confined to its detectors (detectorConfined), whose replay covers the case's signatures (replayCovers),
// for a case the detectors opened and filed all the evidence of (detectorOpened): evidence filed by hand, or merged
// from another case, may be events no detector keyed, and those are counted.
func (s *Ship) replayJudges(ctx context.Context, c store.Case, p Package) (bool, error) {
	if p.Kind == KindConfig || !strings.EqualFold(p.Repo, SelfRepo) || p.Check == nil || p.Check.Replay == nil ||
		!detectorConfined(p.Check.Replay.Paths) || !replayCovers(p.Check.Replay, c.Signatures) {
		return false, nil
	}
	return s.runner().detectorOpened(ctx, c)
}

// detectorConfined says whether every path is under the detectors (internal/detect/) and none is of the detectors'
// frame there (selfHand), which the replay does not read.
func detectorConfined(changed []string) bool {
	if len(changed) == 0 {
		return false
	}
	for _, path := range changed {
		if !slices.ContainsFunc(replayed, func(dir string) bool { return inPath(path, dir) }) ||
			slices.ContainsFunc(selfHand, func(h string) bool { return inPathFold(path, h) }) {
			return false
		}
	}
	return true
}

// replayCovers says whether a replay moved the whole of the case's problem out of the case's signatures: each of them
// is one the replay was keyed for (a signature the case took after its check is not in what it kept) and the base
// keyed events of in its window; it kept none of their events keyed as the base keys them, moved none of them to
// another of the case's own, and keyed no event newly under any of them.
func replayCovers(r *SelfReplay, sigs []string) bool {
	if r == nil || len(sigs) == 0 || r.OwnKept > 0 {
		return false
	}
	mine := map[string]bool{}
	for _, sig := range append(slices.Clone(sigs), r.Own...) {
		mine[sig] = true
	}
	for _, sig := range sigs {
		if !slices.Contains(r.Own, sig) || !slices.Contains(r.OwnSeen, sig) {
			return false
		}
	}
	for _, m := range r.Diff.Rekeyed {
		if mine[m.From] && mine[m.To] {
			return false
		}
	}
	for _, a := range r.Diff.Added {
		if mine[a.Signature] {
			return false
		}
	}
	rekeyed, dropped := r.Diff.Touches(sigs)
	return rekeyed+dropped > 0
}

// replayDst is where a replay reads the store's copy in its sandbox.
const replayDst = "/ro/replay/store.sqlite"

// replayCmd is the replay a detector change is read by: the loop's own replay program (internal/measure/rekey/replay,
// which no change touches), built from each tree, so each tree's detectors key the same copy of the store.
func replayCmd(since, until time.Time) []string {
	return []string{"go", "run", "./internal/measure/rekey/replay", "-store", replayDst,
		"-since", since.UTC().Format(time.RFC3339Nano), "-until", until.UTC().Format(time.RFC3339Nano), "-out", "/out/keys.jsonl"}
}

// selfReplay reads a change to the loop's own detectors by the stored history before it may land: it keys the last
// ReplayWindow of the store's copy (ReplayStore) with the base's detectors and with the change's, in the sandbox,
// and compares what each tree's detectors file there (the evidence of their findings, after every threshold). A change
// may key the case's own events otherwise and stop keying them, whose own investigation says they are no failures; a
// change that re-keys or stops keying events of any other signature would make another case read its problem gone,
// or failures disappear with no evidence that they were false positives, and is refused. A change with no replayed
// detector path, or in another repository, replays nothing. A change whose detector paths are all a person's to land
// for what it does there (selfScope) is replayed too when a store copy is here, since the replay may judge it
// (replayPlan); with no copy, or a replay that refuses it, it is a person's as before and its plan is counted.
func (s *Ship) selfReplay(ctx context.Context, p Package, tree string, changed, hand []string) (*SelfReplay, error) {
	if !strings.EqualFold(p.Repo, SelfRepo) || !slices.ContainsFunc(changed, isReplayed) {
		return nil, nil
	}
	lands := slices.ContainsFunc(changed, func(c string) bool { return isReplayed(c) && !slices.Contains(hand, c) })
	if s.ReplayStore == "" {
		if !lands {
			return nil, nil
		}
		return nil, refused("a change to the loop's own detectors is replayed over a copy of the store, and none is here: a loop pass checks it")
	}
	r, err := s.replayOver(ctx, p, tree, changed)
	if err != nil && !lands && ctx.Err() == nil {
		// as before the replay judged such a change: it stays a person's, with a counted plan
		s.logf("case %s: its detector change is a person's to land, and its plan is counted: %v", p.Case, err)
		return nil, nil
	}
	return r, err
}

// replayOver keys the last ReplayWindow of the store's copy with the base's detectors and the change's (selfReplay).
func (s *Ship) replayOver(ctx context.Context, p Package, tree string, changed []string) (*SelfReplay, error) {
	src, err := filepath.EvalSymlinks(s.ReplayStore)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(src)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("the replay's store copy is no regular file")
	}
	c, err := s.Store.CaseByID(ctx, p.Case)
	if err != nil {
		return nil, err
	}
	until := info.ModTime()
	if now := s.now(); until.After(now) {
		until = now
	}
	since := until.Add(-ReplayWindow)
	binds := []sandbox.Bind{{Src: src, Dst: replayDst}}
	keys := make([]map[string]string, 2)
	for i, rev := range []string{p.BaseCommit, tree} {
		dir, err := s.export(ctx, p, fmt.Sprintf("replay-%d", i), rev)
		if err != nil {
			return nil, err
		}
		o, out, err := s.Sandbox.RunOut(ctx, dir, replayCmd(since, until), s.Env, binds, "keys.jsonl", rekey.MaxOutput)
		err = errors.Join(err, s.unexport(p, dir))
		if err != nil {
			return nil, err
		}
		if o.Exit != 0 {
			side := map[int]string{0: "the base", 1: "the change"}[i]
			return nil, refused("the detector replay did not run on %s (exit %d): a change to the detectors keeps internal/measure/rekey/replay building and running", side, o.Exit)
		}
		h, k, err := rekey.Read(out)
		if err != nil {
			return nil, refused("the detector replay's output on the change cannot be read: %v", err)
		}
		if !h.Since.Equal(since) || !h.Until.Equal(until) {
			return nil, refused("the detector replay keyed another window than it was given")
		}
		keys[i] = k
	}
	r := &SelfReplay{Since: since, Until: until, Diff: rekey.Compare(keys[0], keys[1]), Own: slices.Clone(c.Signatures),
		OwnKept: rekey.Kept(keys[0], keys[1], c.Signatures), Paths: slices.Clone(changed)}
	for _, sig := range c.Signatures {
		for _, keyed := range keys[0] {
			if keyed == sig {
				r.OwnSeen = append(r.OwnSeen, sig)
				break
			}
		}
	}
	if moves := r.Diff.ForeignMoves(c.Signatures); len(moves) > 0 {
		return r, refused("the change keys %d stored events of %d signatures its case does not hold by other signatures (the most, %d events, from %s): a detector change re-keys only the events of the case it fixes, or another case would read its problem gone; %s",
			rekey.Moved(moves), len(moves), moves[0].N, plain.Clip(moves[0].From, 160), r.Summary())
	}
	if foreign := r.Diff.Foreign(c.Signatures); len(foreign) > 0 {
		return r, refused("the change stops keying %d stored events of %d signatures its case does not hold (the most, %d events: %s): a detector change makes failures disappear only for the case it fixes, whose investigation says they are none; %s",
			rekey.Events(foreign), len(foreign), foreign[0].N, plain.Clip(foreign[0].Signature, 160), r.Summary())
	}
	return r, nil
}
