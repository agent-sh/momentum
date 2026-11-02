package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A build that resumes a change before its proposal and ends well with the change where it began (it reports
// blocked: its plan cannot count the problem) is a stop, so two in a row trip the stop rule before the case's budget
// is gone; and once its case leaves testing (closed by hand here, as a build may close it), the next loop run drops
// the change: its work tree and branch go, and another case's change to the same repository is no longer refused.
func TestAChangeThatCannotGoOnEndsWithItsCase(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c := testingCase(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	write(t, filepath.Join(p.Work, "fake_test.go"), answerTest)
	if _, err := s.Check(ctx, c.ID, "go test ./...", nil); err != nil {
		t.Fatal(err)
	}
	none := Signal{Source: store.Transcripts, Kinds: []string{"tool_result"}, Tool: "Bash", Match: []string{"zz-matches-nothing"}}
	if _, err := s.Propose(ctx, c.ID, Proposal{Title: "Answer is 42", Body: "Off by one.", Rollback: "revert", Signal: none, Window: 7 * 24 * time.Hour}); err == nil {
		t.Fatal("a plan that counts nothing was proposed")
	}
	blocked := `{"type":"result","subtype":"success","is_error":false,"num_turns":4,"total_cost_usd":4,"session_id":"s1",` +
		`"structured_output":{"case":"x","step":"build","outcome":"blocked","summary":"the plan cannot count the problem","blocked_by":"ship propose"}}`
	agent := fakeAgent(t, blocked)
	r := runnerFor(t, st, agent)
	r.Ship = s
	var steps []string
	for i := 0; i < 4; i++ {
		done, err := r.Once(ctx, Options{Case: c.ID, Families: []string{"failures"}})
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range done {
			steps = append(steps, d.Step+" "+d.Run.Status+" "+d.Run.Failure+" "+d.Skipped)
		}
	}
	got, _ := st.CaseByID(ctx, c.ID)
	health, _ := st.Cases(ctx, store.CaseFilter{Family: "component-health", All: true})
	if got.Cost.Runs != 2 || len(health) != 1 || !strings.Contains(strings.Join(steps, "\n"), "no progress") {
		t.Fatalf("steps:\n%s\ncase %s over %d runs; loop cases %d", strings.Join(steps, "\n"), got.Status, got.Cost.Runs, len(health))
	}
	if args, _ := agentArgs(t, agent); !strings.Contains(args, "close the case or send it back") {
		t.Errorf("the build is not told it may end the case:\n%s", args)
	}
	c2 := testingCase2(t, st)
	if _, err := s.Prepare(ctx, c2.ID, f.main); err == nil {
		t.Fatal("the fixture's scope check let another change in beside the stuck one")
	}
	if _, err := st.CloseCase(ctx, c.ID, store.NotWorthIt, "", "its plan cannot count the problem", "cli"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Once(ctx, Options{Case: c.ID, Families: []string{"failures"}}); err != nil {
		t.Fatal(err)
	}
	pkg, _, _ := s.Load(c.ID)
	if _, err := os.Stat(p.Work); pkg.State != Dropped || !os.IsNotExist(err) {
		t.Fatalf("the change of a closed case: %s, its work tree %v", pkg.State, err)
	}
	if out := run(t, f.main, "git", "branch", "--list", p.Branch); strings.TrimSpace(out) != "" {
		t.Errorf("its branch stays: %q", out)
	}
	if _, err := s.Prepare(ctx, c2.ID, f.main); err != nil {
		t.Errorf("another case's change, once the stuck one is dropped: %v", err)
	}
}

// A case sent back by hand while its change was before its proposal has its change dropped by the next loop run,
// so the build after its next investigation starts from its new findings, not the old change.
func TestAChangeWhoseCaseWentBackIsDropped(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c := testingCase(t, st)
	if _, err := s.Prepare(ctx, c.ID, f.main); err != nil {
		t.Fatal(err)
	}
	mech := "not the answer: the export reads the lockfile first"
	if _, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseInvestigating, Mechanism: &mech}, "cli"); err != nil {
		t.Fatal(err)
	}
	r := runnerFor(t, st, fakeAgent(t, investigatedResult))
	r.Ship = s
	if _, err := r.Once(ctx, Options{Case: c.ID, Families: []string{"failures"}}); err != nil {
		t.Fatal(err)
	}
	if pkg, _, _ := s.Load(c.ID); pkg.State != Dropped {
		t.Fatalf("the change of a case sent back: %s", pkg.State)
	}
	if mid, _ := st.CaseByID(ctx, c.ID); mid.Status != store.CaseTesting {
		t.Fatalf("after its investigation the case is %s", mid.Status)
	}
	build := fakeAgent(t, okResult)
	r.Agent.Runner = build
	if _, err := r.Once(ctx, Options{Case: c.ID, Families: []string{"failures"}}); err != nil {
		t.Fatal(err)
	}
	if args, _ := agentArgs(t, build); strings.Contains(args, "resume it") || strings.Contains(args, "were checked") {
		t.Errorf("the build resumes the old change:\n%s", args)
	}
}

// An investigation that ended well with a report short of a mechanism or a fix did not decide: its findings go into
// the case, which closes as insufficient evidence (no stop, no case of the loop's own), and comes back once as much
// evidence again comes in, to be investigated again. A report naming neither takes its findings file's.
func TestAnUndecidedInvestigationClosesItsCase(t *testing.T) {
	for name, report := range map[string]string{
		"a mechanism and no fix": `{"mechanism":"the export reads the lockfile before it takes the lock","alternatives":[],"fix":"","summary":"no fix within reach: the evidence cannot decide"}`,
		"neither, a full file":   `{"mechanism":"","alternatives":[],"fix":"","summary":"the evidence cannot decide"}`,
	} {
		t.Run(name, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			dir := t.TempDir()
			result := `{"type":"result","subtype":"success","is_error":false,"num_turns":4,"total_cost_usd":1,"session_id":"s1","structured_output":` + report + `}`
			p := script(t, dir, "agent", `printf '## Mechanism\nthe export reads the lockfile first\n## Fix\ntake the lock first\n' > findings.md
echo '{"type":"system","subtype":"init"}'
echo '`+result+`'`)
			r := runnerFor(t, st, []string{p})
			c := openCase(t, st, "failures", "a", 2)
			done, err := r.Once(ctx, Options{Families: []string{"failures"}})
			if err != nil || len(done) != 1 || done[0].Run.Status != store.RunOK {
				t.Fatalf("%+v %v", done, err)
			}
			got, _ := st.CaseByID(ctx, c.ID)
			health, _ := st.Cases(ctx, store.CaseFilter{Family: "component-health", All: true})
			if got.Status != store.CaseClosed || got.Reason != store.Insufficient || !strings.Contains(got.Mechanism, "lockfile") || len(health) != 0 {
				t.Fatalf("the case: %s %s, mechanism %q; loop cases %d", got.Status, got.Reason, got.Mechanism, len(health))
			}
			if name == "neither, a full file" && got.NextTest != "take the lock first" {
				t.Errorf("the findings file's fix: %q", got.NextTest)
			}
			time.Sleep(5 * time.Millisecond)
			more := store.Finding{Family: "failures", Signature: "a", Title: "case a"}
			for i := 0; i < 2; i++ {
				more.Evidence = append(more.Evidence, store.Evidence{Ref: "file:/t/a-later-" + string(rune('0'+i)), At: time.Now()})
			}
			if _, _, err := st.ObserveCase(ctx, more, "test"); err != nil {
				t.Fatal(err)
			}
			if back, _ := st.CaseByID(ctx, c.ID); back.Status != store.CaseOpen {
				t.Fatalf("with as much evidence again the case is %s", back.Status)
			}
			done, err = r.Once(ctx, Options{Families: []string{"failures"}})
			if err != nil || len(done) != 1 || done[0].Step != StepInvestigate || done[0].Run.ID == "" {
				t.Fatalf("the case that came back: %+v %v", done, err)
			}
		})
	}
}

// The stop rule counts only the runs since a case last came back: a case its two failed runs stopped, closed by
// hand and back on new evidence, is investigated again.
func TestTheStopRuleCountsOnlyRunsSinceTheCaseCameBack(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	turns := `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":30,"total_cost_usd":1}`
	r := runnerFor(t, st, fakeAgent(t, turns))
	c := openCase(t, st, "failures", "a", 2)
	for i := 0; i < 2; i++ {
		if _, err := r.Once(ctx, Options{Families: []string{"failures"}}); err != nil {
			t.Fatal(err)
		}
	}
	if why, err := r.stopped(ctx, c, true); err != nil || why == "" {
		t.Fatalf("two runs that failed the same way: %q %v", why, err)
	}
	if _, err := st.CloseCase(ctx, c.ID, store.Insufficient, "", "held by the stop rule; let it come back", "cli"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	back := store.Finding{Family: "failures", Signature: "a", Title: "case a"}
	for i := 0; i < 3; i++ {
		back.Evidence = append(back.Evidence, store.Evidence{Ref: "file:/t/a-back-" + string(rune('0'+i)), At: time.Now()})
	}
	if _, _, err := st.ObserveCase(ctx, back, "test"); err != nil {
		t.Fatal(err)
	}
	reopened, _ := st.CaseByID(ctx, c.ID)
	if reopened.Status != store.CaseOpen {
		t.Fatalf("the case did not come back: %s", reopened.Status)
	}
	if why, err := r.stopped(ctx, reopened, true); err != nil || why != "" {
		t.Fatalf("the case that came back is still held: %q %v", why, err)
	}
}
