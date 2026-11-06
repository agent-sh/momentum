package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func checkFixtureA4(t *testing.T) (*Ship, store.Case, Patch) {
	t.Helper()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c := testingCase(t, st)
	p, err := s.Prepare(context.Background(), c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), strings.Replace(fakeGo, "return 41", "return 42", 1))
	write(t, filepath.Join(p.Work, "answer_test.py"), "from pathlib import Path\nprint('marker-7f3a')\nassert 'return 42' in Path('fake.go').read_text()\n")
	return s, c, fixturePatch(t, s, p.Case)
}

func TestShipCheckRechecksCaseAuthorityAfterItsGate(t *testing.T) {
	s, c, patch := checkFixtureA4(t)
	ctx := context.Background()
	other := openCase(t, s.Store, "failures", "other", 1)
	before, _ := s.Store.CaseLog(ctx, other.ID)
	s.Sandbox.Gate = func(context.Context) error {
		s.Sandbox.Gate = nil
		_, err := s.Store.CloseCase(ctx, c.ID, store.Duplicate, other.ID, "fixture", "fixture")
		return err
	}
	p, err := s.Check(ctx, c.ID, "python3 answer_test.py", nil, patch)
	after, _ := s.Store.CaseLog(ctx, other.ID)
	for _, entry := range after[len(before):] {
		if entry.Actor == s.Actor {
			t.Fatalf("merged destination received a run note: %+v", entry)
		}
	}
	if err == nil || p.State == Checked {
		t.Fatalf("stale authority persisted check: %+v %v", p, err)
	}
	if _, err := os.Stat(filepath.Join(s.pkgDir(c.ID), "check.json")); !os.IsNotExist(err) {
		t.Fatalf("stale authority wrote check: %v", err)
	}
}

func TestShipCheckCleansGeneratedTextBeforePersistence(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean", true: "failure"}[fails], func(t *testing.T) {
			s, c, patch := checkFixtureA4(t)
			old := Scrubber
			defer func() { Scrubber = old }()
			if fails {
				Scrubber = []string{"false"}
			} else {
				Scrubber = []string{"sed", "-e", "s/access marker-7f3a/access [masked]/g", "-e", "s/^marker-7f3a$/[masked]/"}
			}
			before, _ := os.ReadFile(filepath.Join(s.pkgDir(c.ID), "package.json"))
			p, err := s.Check(context.Background(), c.ID, "python3 answer_test.py access marker-7f3a", nil, patch)
			path := filepath.Join(s.pkgDir(c.ID), "check.json")
			if fails {
				after, _ := os.ReadFile(filepath.Join(s.pkgDir(c.ID), "package.json"))
				if err == nil || string(before) != string(after) {
					t.Fatalf("failed cleaner changed package: %+v %v", p, err)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("failed cleaner persisted check: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			command, err := s.checkCommand(p)
			if err != nil || command[len(command)-1] != "marker-7f3a" {
				t.Fatalf("cleaning changed execution input: %v %v", command, err)
			}
			for _, path := range []string{path, filepath.Join(s.pkgDir(c.ID), "package.json")} {
				raw, err := os.ReadFile(path)
				if err != nil || strings.Contains(string(raw), "marker-7f3a") || !strings.Contains(string(raw), "[masked]") {
					t.Fatalf("unclean generated check: %s %v", raw, err)
				}
			}
			log, _ := s.Store.CaseLog(context.Background(), c.ID)
			raw, _ := json.Marshal(log)
			if strings.Contains(string(raw), "marker-7f3a") {
				t.Fatalf("unclean generated note: %s", raw)
			}
		})
	}
}

func TestCheckOutputIsCleanedWholeBeforeItsTail(t *testing.T) {
	old := Scrubber
	defer func() { Scrubber = old }()
	cleaner := script(t, t.TempDir(), "cleaner", `text=$(cat)
case "$text" in *"prefix-7f3a"*) printf '%s' "$text" | sed 's/marker-7f3a/[masked]/g' ;; *) printf '%s' "$text" ;; esac`)
	Scrubber = []string{cleaner}
	s := testSandbox(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "probe.py"), "print('prefix-7f3a'); print('x'*2000); print('marker-7f3a')\n")
	out, err := s.Run(context.Background(), dir, []string{"python3", "probe.py"}, nil)
	if err != nil || strings.Contains(out.Tail, "marker-7f3a") || !strings.Contains(out.Tail, "[masked]") {
		t.Fatalf("tail was selected before whole-output cleaning: %+v %v", out, err)
	}
}

func TestAnUnreviewedHeadWithNoRoundsLeftWaits(t *testing.T) {
	r, c, p := waitingFixture(t)
	p.Head = "different"
	r.Ship.save(&p)
	if err := r.noteWaiting(context.Background()); err != nil {
		t.Fatal(err)
	}
	if standing, _ := standingNotes(t, r.Store); len(standing) != 1 {
		t.Fatalf("unreviewable head has no passive note: %+v", standing)
	}
	if got, _, _, _ := r.pick(context.Background(), c.Family, "", false); got.ID != "" {
		t.Fatal("unreviewable head selected")
	}
}

func TestConflictAfterTheThirdCleanReviewWaitsThroughSelection(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	c, p := recoveryReviewed(t, s, st, f)
	reviewRounds(&p, p.Head, time.Now().Add(-time.Minute))
	p.Reviews[len(p.Reviews)-1].Medium = 0
	s.save(&p)
	pushOther(t, f, "main", map[string]string{"fake.go": fixed("43")})
	if _, err := s.Land(ctx, c.ID); err == nil {
		t.Fatal("conflict landed")
	}
	p, _, _ = s.Load(c.ID)
	if !roundsUsed(p) || !p.RefreshBase {
		t.Fatalf("conflict escaped exhausted rounds: %+v", p)
	}
	r := localRunner(t, st)
	r.Ship = s
	if got, _, _, _ := r.pick(ctx, c.Family, "", false); got.ID != "" {
		t.Fatalf("conflict selected another build: %+v", got)
	}
	if err := r.noteWaiting(ctx); err != nil {
		t.Fatal(err)
	}
	if standing, _ := standingNotes(t, st); len(standing) != 1 || standing[0].ID != waitingPrefix+c.ID {
		t.Fatalf("no passive conflict note: %+v", standing)
	}
	before, _ := json.Marshal(p)
	if _, err := s.buildBase(s.gitContext(shipContext(ctx, p.Case)), p, ""); err == nil { // RefreshBase is set
		t.Fatal("exhausted conflict refreshed automatically")
	}
	after, _, _ := s.Load(c.ID)
	raw, _ := json.Marshal(after)
	if !bytes.Equal(before, raw) {
		t.Fatal("waiting package changed")
	}
}
