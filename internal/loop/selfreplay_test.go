package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// replayStandIn stands in for the loop's replay program in a fixture of its repository: it keys the events
// internal/detect/testdata/keys.txt lists ("ID SIGNATURE" a line), as the tree it is built from holds them, and writes them
// as the replay writes its output; the detector's own "// key ID SIGNATURE" lines, when it has any, take their place.
// It reads the store's copy, so a bind that did not reach it fails the run.
const replayStandIn = `package main

import (
	"encoding/json"
	"flag"
	"os"
	"strings"
)

func main() {
	st := flag.String("store", "", "")
	since := flag.String("since", "", "")
	until := flag.String("until", "", "")
	out := flag.String("out", "", "")
	flag.Parse()
	if _, err := os.ReadFile(*st); err != nil {
		os.Exit(3)
	}
	keys, err := os.ReadFile("internal/detect/testdata/keys.txt")
	if err != nil {
		os.Exit(4)
	}
	if src, err := os.ReadFile("internal/detect/failures.go"); err == nil {
		var own []string
		for _, l := range strings.Split(string(src), "\n") {
			if k, ok := strings.CutPrefix(l, "// key "); ok {
				own = append(own, k)
			}
		}
		if len(own) > 0 {
			keys = []byte(strings.Join(own, "\n"))
		}
	}
	f, err := os.Create(*out)
	if err != nil {
		os.Exit(5)
	}
	enc := json.NewEncoder(f)
	enc.Encode(map[string]any{"replay": 1, "since": *since, "until": *until})
	for _, l := range strings.Split(strings.TrimSpace(string(keys)), "\n") {
		id, sig, _ := strings.Cut(l, " ")
		enc.Encode(map[string]string{"id": id, "sig": sig})
	}
	f.Close()
}
`

const (
	ownSig   = "failures/v2:tool:read:own"
	otherSig = "failures/v2:tool:bash:other"
	keysPath = "internal/detect/testdata/keys.txt"
	keysBase = "e1 " + ownSig + "\ne2 " + ownSig + "\ne3 " + otherSig + "\n"
	detectGo = "package detect\n\n// Version is the keying's version.\nfunc Version() int { return 1 }\n"
	detectT  = "package detect\n\nimport \"testing\"\n\nfunc TestVersionIsTwo(t *testing.T) {\n\tif Version() != 2 {\n\t\tt.Fatal(Version())\n\t}\n}\n"
)

// replayFixture is a ship tool over a repository the gate names as the loop's own, holding a detector, its keys and
// the replay's stand-in, with a package prepared for a case of ownSig, and a store copy to replay over.
func replayFixture(t *testing.T) (*Ship, store.Case, Package) {
	t.Helper()
	return replayFixtureBy(t, "test")
}

// replayFixtureBy is replayFixture, its case opened by the actor.
func replayFixtureBy(t *testing.T, actor string) (*Ship, store.Case, Package) {
	t.Helper()
	st := testStore(t)
	f := newRepo(t, map[string]string{"internal/measure/rekey/replay/main.go": replayStandIn, keysPath: keysBase,
		"internal/detect/failures.go": detectGo})
	s := shipFor(t, st, f)
	s.Gate.Resolve = func(context.Context, string) (string, error) { return SelfRepo, nil }
	c := testingCaseBy(t, st, ownSig, nil, actor)
	p, err := s.Prepare(context.Background(), c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	s.ReplayStore = filepath.Join(f.dir, "store-copy.sqlite")
	write(t, s.ReplayStore, "a store's copy")
	return s, c, p
}

// changeKeys makes the change to the detector: its version moves to two, its test with it, and its keys become keys.
func changeKeys(t *testing.T, p Package, keys string) {
	t.Helper()
	write(t, filepath.Join(p.Work, "internal/detect/failures.go"), strings.Replace(detectGo, "return 1", "return 2", 1))
	write(t, filepath.Join(p.Work, "internal/detect/failures_test.go"), detectT)
	write(t, filepath.Join(p.Work, keysPath), keys)
}

// A change to the loop's own detectors is replayed over the store's copy with the base's detectors and the change's:
// it may key the case's events otherwise and stop keying them (the package keeps what it did), and a change that
// stops keying events of another signature is refused.
func TestADetectorChangeIsReplayedOverTheStoredHistory(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		keys             string
		refused          bool
		rekeyed, dropped int
	}{
		"its own events keyed otherwise": {"e1 failures/v2:tool:read:own:path\ne2 " + ownSig + "\ne3 " + otherSig + "\n", false, 1, 0},
		"its own events dropped":         {"e3 " + otherSig + "\n", false, 0, 2},
		"another case's events dropped":  {"e1 " + ownSig + "\ne2 " + ownSig + "\n", true, 0, 1},
		"another case's events re-keyed": {"e1 " + ownSig + "\ne2 " + ownSig + "\ne3 failures/v2:tool:bash:moved\n", true, 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			s, cs, p := replayFixture(t)
			changeKeys(t, p, c.keys)
			p, err := s.CheckArgs(ctx, cs.ID, []string{"go", "test", "./internal/detect"}, nil, fixturePatch(t, s, cs.ID))
			if c.refused {
				if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "1 stored events of 1 signatures its case does not hold") || p.State == Checked {
					t.Fatalf("want the replay's refusal: state %s: %v", p.State, err)
				}
				return
			}
			if err != nil || p.State != Checked || len(p.Hand) != 0 || p.Check == nil || p.Check.Replay == nil {
				t.Fatalf("a replayed change: hand %v state %s check %+v: %v", p.Hand, p.State, p.Check, err)
			}
			r := p.Check.Replay
			if rekeyed, dropped := r.Diff.Touches([]string{ownSig}); rekeyed != c.rekeyed || dropped != c.dropped || !slices.Equal(r.Own, []string{ownSig}) {
				t.Fatalf("the replay: %+v", r)
			}
		})
	}
}

// Without a store's copy to replay it over (the owner's own check, outside a pass), a detector change is a person's
// to land; a change elsewhere in the repository is not replayed at all.
func TestADetectorChangeWithNoStoreCopyIsAPersonsToLand(t *testing.T) {
	ctx := context.Background()
	s, cs, p := replayFixture(t)
	s.ReplayStore = ""
	changeKeys(t, p, keysBase)
	p, err := s.CheckArgs(ctx, cs.ID, []string{"go", "test", "./internal/detect"}, nil, fixturePatch(t, s, cs.ID))
	if err != nil || p.State != Checked || !slices.Contains(p.Hand, "internal/detect/failures.go") || p.Check.Replay != nil {
		t.Fatalf("a detector change with no store copy: hand %v state %s: %v", p.Hand, p.State, err)
	}
	if err := os.Remove(filepath.Join(filepath.Dir(s.Dir), "store-copy.sqlite")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

// A detector fix, a change confined to internal/detect/ whose replay moved every event of its case's own signatures,
// is proposed on its replay: no signal is counted for it, as the case's problem is the detector's keying, and that
// holds when a path of it is a person's to land for what the change does there. A detector change that left some of
// the case's events keyed as they were, or a case the detectors did not open alone, still needs a plan the store
// counts (here one that waits on the store's reads).
func TestADetectorFixIsProposedOnItsReplayNotACount(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		keys   string
		hand   bool
		actor  string
		replay bool
	}{
		"its own events dropped":                     {"e3 " + otherSig + "\n", false, DetectorActor, true},
		"its own events dropped, landed by a person": {"e3 " + otherSig + "\n", true, DetectorActor, true},
		"some of its own events keyed as they were":  {"e1 failures/v2:tool:read:own:path\ne2 " + ownSig + "\ne3 " + otherSig + "\n", false, DetectorActor, false},
		"a case a person opened":                     {"e3 " + otherSig + "\n", false, "owner", false},
	} {
		t.Run(name, func(t *testing.T) {
			s, cs, p := replayFixtureBy(t, c.actor)
			if c.hand {
				// every path a person's to land: the detector adds a package variable whose value runs code (selfKept),
				// and its test is the detectors' frame's; the detector holds its keys
				var keys strings.Builder
				for _, l := range strings.Split(strings.TrimSpace(c.keys), "\n") {
					keys.WriteString("// key " + l + "\n")
				}
				write(t, filepath.Join(p.Work, "internal/detect/failures.go"), strings.Replace(detectGo, "return 1", "return 2", 1)+
					"\nvar two = len(\"xy\")\n\n"+keys.String())
				write(t, filepath.Join(p.Work, "internal/detect/detect_test.go"), detectT)
			} else {
				changeKeys(t, p, c.keys)
			}
			p, err := s.CheckArgs(ctx, cs.ID, []string{"go", "test", "./internal/detect"}, nil, fixturePatch(t, s, cs.ID))
			if err != nil || p.State != Checked || p.Check.Replay == nil || len(p.Hand) == 2 != c.hand {
				t.Fatalf("the check: hand %v state %s replay %v: %v", p.Hand, p.State, p.Check != nil && p.Check.Replay != nil, err)
			}
			p, err = proposePatch(t, s, ctx, cs.ID, Proposal{Title: "Key the read failure by its path", Body: "The detector keys it otherwise.",
				Rollback: "Revert the commit.", Signal: goodSignal, Window: 7 * 24 * time.Hour})
			if !c.replay {
				if wait := (*waitingPlan)(nil); !errors.As(err, &wait) || p.Plan == nil || p.Plan.Replay {
					t.Fatalf("a change its replay does not judge was proposed without a counted plan: %+v: %v", p.Plan, err)
				}
				return
			}
			if err != nil || p.Plan == nil || !p.Plan.Replay || p.Plan.Units != "" || p.Head == "" {
				t.Fatalf("a detector fix's proposal: plan %+v head %q: %v", p.Plan, p.Head, err)
			}
		})
	}
}

// A hand landing of the head the loop checked keeps its replay plan; one of another head, which its replay never read,
// is measured by hand.
func TestAHandLandingOfAnotherHeadIsNotJudgedByTheReplay(t *testing.T) {
	ctx := context.Background()
	for _, checked := range []bool{true, false} {
		s, c, _, p := proposedHand(t)
		p.Plan.Replay, p.Plan.Units = true, ""
		if err := s.save(&p); err != nil {
			t.Fatal(err)
		}
		if err := s.landHand(ctx, c, &p, p.Head, time.Now(), checked); err != nil {
			t.Fatal(err)
		}
		if got, _, _ := s.Load(c.ID); got.State != Landed || got.Plan.Replay != checked {
			t.Fatalf("landed the checked head %v: state %s, replay plan %v", checked, got.State, got.Plan.Replay)
		}
	}
}
