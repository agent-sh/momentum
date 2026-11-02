package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/momentum/internal/store"
)

// A revision whose report names a root another case holds leaves the root alone: a merge would close the case
// while its change is revising on its pull request.
func TestARevisionsReportedRootLeavesItsCaseOnItsPullRequest(t *testing.T) {
	for _, state := range []string{Revising} { // a proposed or landing change runs no build: it waits on its review or landing
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			a := openCase(t, st, "failures", "a", 1)
			mech, fix := "the lockfile is read before the lock", "take the lock first"
			if _, err := st.UpdateCase(ctx, a.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &fix}, "fixture"); err != nil {
				t.Fatal(err)
			}
			b := openCase(t, st, "failures", "b", 1)
			if _, _, err := st.AddCaseSignature(ctx, b.ID, "root:shared", "fixture"); err != nil {
				t.Fatal(err)
			}
			agent := fakeAgent(t, strings.Replace(okResult, `"case":"x"`, `"case":"x","root":"root:shared"`, 1))
			r := runnerFor(t, st, agent)
			var logged []string
			r.Log = func(format string, a ...any) { logged = append(logged, format) }
			p := Package{Case: a.ID, Kind: "repo", State: state, PR: "https://example.invalid/o/r/pull/7", Work: filepath.Join(r.Dir, "work", a.ID),
				Reviews: []Review{{Round: 1, High: 1, Text: "round 1: one high finding", Comment: "https://example.invalid/o/r/pull/7#c1"}}}
			if err := r.Ship.save(&p); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Once(ctx, Options{Case: a.ID, Families: []string{"failures"}}); err != nil {
				t.Fatal(err)
			}
			if live, _ := st.CaseLive(ctx, a.ID); live.ID != a.ID {
				t.Fatalf("the case went into %s by its report's root while its change is %s", live.ID, state)
			}
			if !strings.Contains(strings.Join(logged, "\n"), "its change is %s on its pull request") {
				t.Fatalf("the runner did not say why it left the root: %q", logged)
			}
		})
	}
}

// A run cut short whose findings file holds only alternatives gives both arms' judges the same four empty fields.
func TestAnAlternativesOnlyFindingsFileReachesNeitherJudge(t *testing.T) {
	e, task := evalFixture(t)
	e.Runner.Agent.Runner = []string{script(t, t.TempDir(), "alternatives-only", `cat > findings.md <<'NOTES'
## Alternatives ruled out
a slow disk
NOTES
`)}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
	}
	if trs[0].JudgePromptHash != trs[1].JudgePromptHash {
		t.Fatal("the arms' judge inputs differ")
	}
	raw, _ := os.ReadFile(filepath.Join(trs[0].JudgeDir, "prompt.txt"))
	if strings.Contains(string(raw), "a slow disk") {
		t.Fatalf("alternatives alone reached a judge:\n%s", raw)
	}
}

// An investigation whose run ends in an error after it kept findings (here its fallback model cannot start once
// the safeguards refused the first) still leaves them in its case.
func TestAnInvestigationThatErredKeepsItsFindings(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	refused := `{"type":"result","subtype":"success","is_error":true,"num_turns":1,"total_cost_usd":0.25,"result":"the safeguards blocked it"}`
	agent := script(t, t.TempDir(), "refused-once", `cat > findings.md <<'NOTES'
## Mechanism
the export reads the lockfile first
## Fix
take the lock first
NOTES
rm -f "$0"
echo '`+refused+`'`)
	r := runnerFor(t, st, []string{agent})
	r.Agent.Fallback = "model-y"
	c := openCase(t, st, "failures", "a", 1)
	if _, err := r.Once(ctx, Options{Families: []string{"failures"}}); err != nil {
		t.Fatal(err)
	}
	runs, _ := st.LoopRuns(ctx, store.RunFilter{Case: c.ID})
	if len(runs) != 1 || runs[0].Status != store.RunFailed {
		t.Fatalf("the run did not end in an error: %+v", runs)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Mechanism != "the export reads the lockfile first" || got.NextTest != "take the lock first" || got.Status != store.CaseInvestigating {
		t.Fatalf("the case holds %q, %q, %s", got.Mechanism, got.NextTest, got.Status)
	}
}

// A run cut short sets only what its findings file holds: a file with only a mechanism keeps the fix and the
// alternatives the case held from an earlier run.
func TestARunCutShortKeepsWhatItsFileDoesNotHold(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	agent := []string{script(t, t.TempDir(), "unfinished", `cat > findings.md <<'NOTES'
## Mechanism
still settling the disk against the lock order
NOTES
echo '{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":30,"total_cost_usd":1.5}'
`)}
	r := runnerFor(t, st, agent)
	c := openCase(t, st, "failures", "a", 1)
	mech, fix := "the export reads the lockfile before it takes the lock; the disk is left open", "take the lock first"
	if _, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseInvestigating, Mechanism: &mech, NextTest: &fix, Alternatives: []string{"a slow disk"}}, "loop:build"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Once(ctx, Options{Families: []string{"failures"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.CaseByID(ctx, c.ID)
	if got.Mechanism != "still settling the disk against the lock order" || got.NextTest != fix || len(got.Alternatives) != 1 {
		t.Fatalf("the case holds %q, next test %q, alternatives %q", got.Mechanism, got.NextTest, got.Alternatives)
	}
}

// A change is in progress while it is on its pull request (proposed, revising, landing, or checked again after a
// review), and an unreadable package counts as in progress; the runner's root and the case commands both ask this.
func TestAChangeIsOpenWhileItsPullRequestIs(t *testing.T) {
	st := testStore(t)
	s := &Ship{Store: st, Dir: t.TempDir()}
	if _, open := s.ChangeOpen("c00000001"); open {
		t.Fatal("a case with no package is open")
	}
	// a change before its proposal (prepared, checked with no pull request) is the build's to resume or end; one on its
	// pull request is its review's
	for _, tc := range []struct {
		state, pr string
		want      bool
	}{{Prepared, "", false}, {Checked, "", false}, {Checked, "https://example.com/pr/1", true}, {Proposed, "https://example.com/pr/1", true},
		{Revising, "https://example.com/pr/1", true}, {Landing, "https://example.com/pr/1", true}, {Landed, "", false}, {RolledBack, "", false},
		{Kept, "", false}, {Dropped, "", false}} {
		p := Package{Case: "c00000001", Kind: "repo", State: tc.state, PR: tc.pr}
		if err := s.save(&p); err != nil {
			t.Fatal(err)
		}
		if got, open := s.ChangeOpen("c00000001"); open != tc.want || got != tc.state {
			t.Errorf("%s %q: open %v (%q)", tc.state, tc.pr, open, got)
		}
	}
	write(t, filepath.Join(s.pkgDir("c00000001"), "package.json"), "{not json")
	if _, open := s.ChangeOpen("c00000001"); !open {
		t.Error("an unreadable package is not open")
	}
	if _, open := (*Ship)(nil).ChangeOpen("c00000001"); open {
		t.Error("no ship tool, yet open")
	}
}
