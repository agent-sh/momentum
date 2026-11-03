package loop

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func currentReviewHead(t *testing.T, s *Ship, id string) string {
	t.Helper()
	p, ok, err := s.Load(id)
	if err != nil || !ok || p.Head == "" {
		t.Fatalf("review fixture has no head: %v", err)
	}
	return p.Head
}

func heldBuildFixture(t *testing.T) (*Runner, store.Case, repoFixture) {
	t.Helper()
	fakeManager(t)
	r, c := inputRunner(t)
	f := newRepo(t, nil)
	r.Ship = shipWithSandbox(r.Store, f, Sandbox{})
	r.Ship.Dir = r.Dir
	r.Inputs.ReadDirs = []string{f.main}
	mechanism, fix := "one cause", "one fix"
	var err error
	c, err = r.Store.UpdateCase(context.Background(), c.ID, store.CaseUpdate{
		Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &fix,
	}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	setCaseRepo(t, r.Store, c, "main")
	return r, c, f
}

// The fourth review's busy-repository probe, with assertions for the hold.
func TestBusyBuildPreparationDoesNotStopTheCase(t *testing.T) {
	r, c, _ := heldBuildFixture(t)
	ctx := context.Background()
	r.Ship.Gate.Busy = func(context.Context, string) (string, error) {
		return "fixture session works in it", nil
	}
	for i := 0; i < 3; i++ {
		done, err := r.Once(ctx, Options{Families: []string{c.Family}, Case: c.ID})
		if err != nil || len(done) != 1 || !strings.Contains(done[0].Skipped, "in use") || done[0].Run.ID != "" {
			t.Fatalf("busy preparation became a run: %+v %v", done, err)
		}
	}
	runs, err := r.Store.LoopRuns(ctx, store.RunFilter{})
	if err != nil || len(runs) != 0 {
		t.Fatalf("hold stored runs: %d %v", len(runs), err)
	}
	got, err := r.Store.CaseByID(ctx, c.ID)
	if err != nil || got.Cost.Runs != 0 || got.Cost.USD != 0 {
		t.Fatalf("hold charged the case: %+v %v", got.Cost, err)
	}
	cs, err := r.Store.Cases(ctx, store.CaseFilter{All: true})
	if err != nil || len(cs) != 1 {
		t.Fatalf("hold opened a component-health case: %d %v", len(cs), err)
	}
	notes, all := standingNotes(t, r.Store)
	if len(notes) != 1 || all != 1 || notes[0].Importance != store.ImportanceLow {
		t.Fatalf("hold lacks one passive note: %+v", notes)
	}
	if _, _, err := r.Store.DoneNote(ctx, notes[0].ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Once(ctx, Options{Families: []string{c.Family}, Case: c.ID}); err != nil {
		t.Fatal(err)
	}
	if notes, all := standingNotes(t, r.Store); len(notes) != 0 || all != 1 {
		t.Fatal("unchanged hold reopened its resolved note")
	}
	// Reaching toolchain preparation proves the former hold did not stop it.
	r.Ship.Gate.Busy = func(context.Context, string) (string, error) { return "", nil }
	r.Toolchains.ReadOnly = []string{filepath.Join(t.TempDir(), "absent")}
	done, err := r.Once(ctx, Options{Families: []string{c.Family}, Case: c.ID})
	if err != nil || len(done) != 1 || done[0].Run.Failure != "build toolchains" {
		t.Fatalf("free repository remained stopped: %+v %v", done, err)
	}
}

func TestBuildPreparationGateAndLockHoldsWriteNoRun(t *testing.T) {
	for _, cause := range []string{"owner", "scope", "busy-read", "lock"} {
		t.Run(cause, func(t *testing.T) {
			r, c, _ := heldBuildFixture(t)
			ctx := context.Background()
			switch cause {
			case "owner":
				r.Ship.Gate.Owners = nil
			case "scope":
				r.Ship.Gate.RepoGuard = []string{"false"}
			case "busy-read":
				r.Ship.Gate.Busy = func(context.Context, string) (string, error) {
					return "", errors.New("marker-7f3a")
				}
			case "lock":
				unlock, err := r.Ship.lock(ctx, c.ID)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			}
			for i := 0; i < 2; i++ {
				done, err := r.Once(ctx, Options{Families: []string{c.Family}, Case: c.ID})
				if err != nil || len(done) != 1 || done[0].Skipped == "" || done[0].Run.ID != "" {
					t.Fatalf("preparation hold became a run: %+v %v", done, err)
				}
			}
			runs, err := r.Store.LoopRuns(ctx, store.RunFilter{})
			if err != nil || len(runs) != 0 {
				t.Fatalf("hold stored runs: %d %v", len(runs), err)
			}
			notes, all := standingNotes(t, r.Store)
			if len(notes) != 1 || all != 1 || notes[0].Importance != store.ImportanceLow {
				t.Fatalf("hold lacks one passive note: %+v", notes)
			}
		})
	}
}

func TestBuildStageGateAndLockRefusalsAreHolds(t *testing.T) {
	for _, stage := range []string{"build check", "build propose"} {
		for _, cause := range []string{"gate", "lock", "bad-output"} {
			t.Run(stage+"/"+cause, func(t *testing.T) {
				s, _, c, p, pr := b4Checked(t)
				ctx := context.Background()
				var causeErr error
				if cause == "lock" {
					unlock, err := s.lock(ctx, c.ID)
					if err != nil {
						t.Fatal(err)
					}
					defer unlock()
					if stage == "build check" {
						_, causeErr = s.Check(ctx, c.ID, "go test ./...", nil)
					} else {
						_, causeErr = s.Propose(ctx, c.ID, pr)
					}
				} else if cause == "gate" {
					s.Gate.Owners = nil
					_, causeErr = s.Gate.Repository(ctx, p.Dir)
				} else {
					causeErr = refused("invalid patch or measurement")
				}
				res, err := stageFailure(Result{Started: true, CostKnown: true, Status: store.RunOK, USD: .1}, stage, causeErr)
				if cause == "bad-output" {
					if err != nil || res.Status != store.RunFailed || res.Failure != stage {
						t.Fatalf("invalid agent output stopped counting: %+v %v", res, err)
					}
				} else if !errors.Is(err, ErrRefused) || res.Status == store.RunFailed {
					t.Fatalf("host hold became an agent failure: %+v %v", res, err)
				}
			})
		}
	}
}

func TestBuildStageHoldsKeepChargesWithoutRuns(t *testing.T) {
	for _, stage := range []string{"build check", "build propose"} {
		t.Run(stage, func(t *testing.T) {
			s, _, c, _, _ := b4Checked(t)
			ctx := context.Background()
			r := &Runner{Store: s.Store, Ship: s, Dir: s.Dir, Budgets: DefaultBudgets}
			unlock, err := s.lock(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			_, cause := s.lock(ctx, c.ID)
			for i := 0; i < 2; i++ {
				run, why := r.reserve(ctx, c.Family, c.ID, StepBuild)
				if why != "" {
					t.Fatal(why)
				}
				res, err := stageFailure(Result{Started: true, CostKnown: true, Status: store.RunOK,
					USD: 90, RelayAdmitted: 1, RelayCharged: 1, RelayUSD: .032, Tokens: 12, Turns: 1}, stage, cause)
				done := r.completeStep(ctx, c, run, res, err)
				if done.Run.ID != "" || !strings.Contains(done.Skipped, "another ship step") {
					t.Fatalf("hold did not skip the step: %+v", done)
				}
				// A repeated completion must neither charge nor count twice.
				if err := r.Store.HoldLoopRun(ctx, store.LoopRun{ID: run.ID}); err != nil {
					t.Fatal(err)
				}
			}
			runs, err := r.Store.LoopRuns(ctx, store.RunFilter{})
			if err != nil || len(runs) != 0 {
				t.Fatalf("hold left run rows: %d %v", len(runs), err)
			}
			got, err := r.Store.CaseByID(ctx, c.ID)
			if err != nil || got.Cost.Runs != 0 || got.Cost.USD != .064 || got.Cost.Tokens != 24 {
				t.Fatalf("hold lost or invented accounting: %+v %v", got.Cost, err)
			}
			spent, err := r.Store.LoopSpend(ctx, time.Now().Add(-time.Hour), r.Budgets.Run)
			if err != nil || spent != .064 {
				t.Fatalf("budget lost the held agent's spend: %g %v", spent, err)
			}
			if why, err := r.stopped(ctx, got, false); err != nil || why != "" {
				t.Fatalf("held stages stopped the case: %s %v", why, err)
			}
			notes, all := standingNotes(t, r.Store)
			if len(notes) != 1 || all != 1 {
				t.Fatalf("stage hold duplicated its note: %+v", notes)
			}
			log, err := r.Store.CaseLog(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			receipts := 0
			for _, entry := range log {
				if entry.Action == "loop hold" {
					receipts++
				}
			}
			if receipts != 2 {
				t.Fatalf("hold accounting was not atomic and once-only: %d", receipts)
			}
		})
	}
}

func TestBuildHoldNotesKeepEachCauseResolved(t *testing.T) {
	st := testStore(t)
	r := &Runner{Store: st}
	c := testingCase(t, st)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		for _, cause := range []string{"repository busy", "scope refused"} {
			if err := r.noteBuildSkip(ctx, c, "prepare", cause); err != nil {
				t.Fatal(err)
			}
		}
		if i == 0 {
			notes, all := standingNotes(t, st)
			if len(notes) != 2 || all != 2 {
				t.Fatalf("causes overwrote each other's notes: %+v", notes)
			}
			for _, note := range notes {
				if _, _, err := st.DoneNote(ctx, note.ID, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if notes, all := standingNotes(t, st); len(notes) != 0 || all != 2 {
		t.Fatal("unchanged causes reopened resolved notes")
	}
}

func TestReviewedRefusesAHeadChangedAfterExport(t *testing.T) {
	s, _, c, p, pr := b4Checked(t)
	ctx := context.Background()
	p, err := proposePatch(t, s, ctx, c.ID, pr)
	if err != nil {
		t.Fatal(err)
	}
	export, err := s.export(ctx, p, "review", p.Head)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.unexport(p, export); err != nil {
			t.Error(err)
		}
	}()
	readHead := p.Head
	p.Head = p.BaseCommit
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	for _, head := range []string{readHead, ""} {
		if _, err := s.Reviewed(ctx, c.ID, Review{Head: head, Text: "Read the exported tree."}); !errors.Is(err, ErrRefused) {
			t.Fatalf("review accepted an unread head: %v", err)
		}
	}
	got, _, err := s.Load(c.ID)
	if err != nil || len(got.Reviews) != 0 {
		t.Fatalf("stale verdict was stored: %d %v", len(got.Reviews), err)
	}
	p.Head = readHead
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	got, err = s.Reviewed(ctx, c.ID, Review{Head: readHead, Text: "Read the exported tree."})
	if err != nil || len(got.Reviews) != 1 || got.Reviews[0].Head != readHead {
		t.Fatalf("matching exported head was refused: %v", err)
	}
}

func TestFourthReviewMaskShapes(t *testing.T) {
	code := []string{
		"x: Vec<u8> = a::b", "dead::beef",
		"r.cfg.app", "s.opts.local", "c.server.test", "m.store.net", "t.mu.dev", "p.db.internal",
		"Foo.Bar.test", "Server.example.com", "long_receiver.docs.example.com",
		"see example.org", "host=db.internal",
	}
	for _, line := range code {
		t.Run(line, func(t *testing.T) {
			if got := maskPersonal(line); got != line {
				t.Errorf("code was masked: %q", got)
			}
			if err := patchText(context.Background(), line, "added lines"); err != nil {
				t.Error(err)
			}
		})
	}
	for _, line := range []string{
		"ip:2001:db8::1", "ip:fe80::1", "fe80::1", "AB::CD",
		"ab::cd:ef", "2001:db8:0:0:0:0:0:1", "addr=2001:db8::1",
		"https://r.cfg.app/path", "ssh r.cfg.app", "host: m.store.net",
		"docs.example.com", "build-box-7.example.com",
	} {
		t.Run(line, func(t *testing.T) {
			if got := maskPersonal(line); got == line {
				t.Fatal("personal shape survived")
			}
		})
	}
}

func TestLessonDescriptionIsPublicAndUnverified(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "reserved"}[retry], func(t *testing.T) {
			st := testStore(t)
			c := testingCase(t, st)
			db, err := sql.Open("sqlite", st.Path())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec("UPDATE cases SET status = ?, reason = ? WHERE id = ?", store.CaseClosed, store.Improved, c.ID); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			text := "Read /home/someone/notes at docs.example.com and marker-7f3a@example.com."
			if retry {
				if _, _, err := st.BeginLesson(ctx, c.ID, text); err != nil {
					t.Fatal(err)
				}
			}
			dir := t.TempDir()
			args := filepath.Join(dir, "args")
			tacit := script(t, dir, "tacit", `printf '%s\n' "$@" > '`+args+`'`)
			s := &Ship{Store: st, Dir: filepath.Join(dir, "loop")}
			if err := PublishLesson(ctx, st, s, c.ID, text, tacit, "fixture"); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(args)
			if err != nil {
				t.Fatal(err)
			}
			fields := strings.Split(string(raw), "\n")
			var desc string
			for i, field := range fields {
				if field == "--desc" {
					desc = fields[i+1]
				}
			}
			if !strings.HasPrefix(desc, "unverified lesson from a loop run: ") || maskPersonal(desc) != desc || !strings.Contains(desc, "[personal]") {
				t.Fatalf("description was personal or trusted: %q", desc)
			}
		})
	}
}

func TestSplitSignalEditClearsEvidenceButCannotBeProposed(t *testing.T) {
	for _, half := range []string{"first", "second", "both", "unrelated"} {
		t.Run(half, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			old := strings.Replace(fakeGo, `"zz-signal-text"`, "\"zz-signal-\" +\n\t\t\"text\"", 1)
			f := newRepo(t, map[string]string{"fake.go": old})
			s := shipWithSandbox(st, f, Sandbox{})
			c := testingCase(t, st)
			if plan, err := s.validPlan(ctx, c, goodSignal, 7*24*time.Hour, "", s.now()); err != nil || plan.Gate != 6 {
				t.Fatalf("probe did not clear the evidence gate: %d %v", plan.Gate, err)
			}
			p, err := s.Prepare(ctx, c.ID, f.main)
			if err != nil {
				t.Fatal(err)
			}
			newText := strings.Replace(old, "return 41", "return 42", 1)
			if half == "first" || half == "both" {
				newText = strings.Replace(newText, `"zz-signal-"`, `"quiet-"`, 1)
			}
			if half == "second" || half == "both" {
				newText = strings.Replace(newText, `"text"`, `"word"`, 1)
			}
			write(t, filepath.Join(p.Work, "fake.go"), newText)
			write(t, filepath.Join(p.Work, "fake_test.go"), answerTest)
			snapshot := fixturePatch(t, s, c.ID)
			tree, changed, err := s.applyPatch(ctx, p, []Patch{snapshot})
			if err != nil {
				t.Fatal(err)
			}
			p.State, p.Changed = Checked, changed
			p.Check = &Check{Tree: tree, Before: Outcome{Exit: 1}, After: Outcome{Exit: 0}}
			if err := s.save(&p); err != nil {
				t.Fatal(err)
			}
			pr := Proposal{Title: "Correct answer", Body: "One cause.", Rollback: "Revert.", Signal: goodSignal, Window: 7 * 24 * time.Hour}
			_, err = s.Propose(ctx, c.ID, pr, snapshot)
			if half == "unrelated" {
				if err != nil {
					t.Fatalf("unchanged split signal refused another fix: %v", err)
				}
			} else if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "measurement counts") {
				t.Fatalf("split signal edit was proposed: %v", err)
			}
		})
	}
}
