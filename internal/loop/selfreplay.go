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

// replayPlan is the plan of a detector fix: a change of the loop's own repository confined to its detectors
// (internal/detect/; of the frame there the replay does not read, selfHand, only test files), whose check's
// replay re-keyed or dropped the case's own events and kept none of them keyed as the base keys them. Such a case's
// problem is the detector's own keying: no forward count of the signal measures it, and the replay, which refused any
// move of another signature's events, is its measure. The plan counts nothing and closes the case as a detector fix
// once the change is in effect (Runner.detectorFixed). Any other change gets nil, and its plan is counted (validPlan).
func replayPlan(c store.Case, p Package, changed []string, pr Proposal, at time.Time) (*Plan, error) {
	if p.Kind == KindConfig || !strings.EqualFold(p.Repo, SelfRepo) || p.Check == nil || p.Check.Replay == nil || len(changed) == 0 {
		return nil, nil
	}
	for _, path := range changed {
		if !slices.ContainsFunc(replayed, func(dir string) bool { return inPath(path, dir) }) {
			return nil, nil
		}
		if !strings.HasSuffix(path, "_test.go") && slices.ContainsFunc(selfHand, func(h string) bool { return inPathFold(path, h) }) {
			return nil, nil
		}
	}
	r := p.Check.Replay
	if rekeyed, dropped := r.Diff.Touches(c.Signatures); rekeyed+dropped == 0 || r.OwnKept > 0 {
		return nil, nil
	}
	if !measure.WholeWeeks(pr.Window) {
		return nil, refused("a measurement window is a whole number of weeks (7d, 14d and so on)")
	}
	if err := installedFile(pr.Installed); err != nil {
		return nil, err
	}
	return &Plan{Signal: pr.Signal, Window: pr.Window, At: at, Installed: pr.Installed, Replay: true}, nil
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
// detector path, or in another repository, replays nothing. A detector path a person lands for what the change does
// to it (selfScope) is replayed all the same when a store copy is here: the replay is how the change is judged
// (replayPlan), not who lands it; with no copy, such a change is a person's and is measured forward.
func (s *Ship) selfReplay(ctx context.Context, p Package, tree string, changed, hand []string) (*SelfReplay, error) {
	if !strings.EqualFold(p.Repo, SelfRepo) || !slices.ContainsFunc(changed, isReplayed) {
		return nil, nil
	}
	if s.ReplayStore == "" && !slices.ContainsFunc(changed, func(c string) bool { return isReplayed(c) && !slices.Contains(hand, c) }) {
		return nil, nil
	}
	if s.ReplayStore == "" {
		return nil, refused("a change to the loop's own detectors is replayed over a copy of the store, and none is here: a loop pass checks it")
	}
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
		OwnKept: rekey.Kept(keys[0], keys[1], c.Signatures)}
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
