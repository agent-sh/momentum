package loop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// An evaluation's tests run its trials and judges in the real sandbox, as the loop's tests do: bubblewrap, the test
// binary as momentum's init and the door's client, and a route to a fake broker (testJail). What a fake agent can
// reach there is its own directory, its harness's directory and the trial's checkouts; the commands it types are
// answered by the door, whose programs are scripts of the test's, outside the sandbox, which log what they were
// asked with the variables the door gave them.

// evalFixture builds only synthetic evidence. The fake agent and the fake judge keep their records in their
// directories and print recorded stream messages; no model is called. The fixture's directory holds the store the
// snapshots are made from and the door's log apart from the directory the sandbox mounts, so a read of either from
// inside fails.
func evalFixture(t *testing.T) (*Eval, GoldenTask) {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "source", "source.db")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	ev := store.Event{Source: store.Transcripts, SourceKey: "symptom", TS: at, Session: "s", Actor: store.ActorTool, Kind: "tool_result", Summary: "the worker failed"}
	if _, err = st.Put(context.Background(), []store.Event{ev}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	task := GoldenTask{ID: "worker-reads-before-lock", Family: "failures", CaseTitle: "the worker failed", Cutoff: at.Add(time.Hour), RootCause: "a verified cause", KeyFacts: []string{"a verified fact"}}
	task.Symptom.Query.EventIDs = []string{ev.ID()}
	harness := filepath.Join(dir, "harness")
	agent := script(t, harness, "fixture-run", `printf '%s\n' "$@" > args.txt
momentum evidence status --since 1d > door-answer.txt 2>&1
`+modelResult(`{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":1,"structured_output":{"mechanism":"a mechanism","root":"root:cause","root_cause":"root:cause","fix":"a fix","summary":"a summary"}}`))
	judge := script(t, harness, "fixture-judge", `printf '%s\n' "$@" > args.txt
`+modelResult(`{"type":"result","subtype":"success","total_cost_usd":0.1,"structured_output":{"verdict":"reached","facts":[],"reason":"a reason"}}`))
	runner := &Runner{Store: testStore(t), Agent: testJail(t, Agent{Runner: []string{agent}, Model: "requested-model"}),
		Momentum: evaluationDoorProgram(t, filepath.Join(dir, "door"), "momentum"), Budgets: Budgets{Run: 4, RunTime: time.Minute, MaxTurns: 10},
		Constraints: []string{"never wake him"}}
	return &Eval{Runner: runner, Source: source, Dir: filepath.Join(dir, "evaluation"), ID: "synthetic-evaluation", Trials: 1, Budget: 50, Parallel: 1,
		Judge: testJail(t, Agent{Runner: []string{judge}, Model: "requested-judge", Fallback: "unused"}), GoldenHash: digest("synthetic golden file")}, task
}

// evaluationDoorProgram is a program the door runs, outside the sandbox: it logs one line to the log beside it, in dir, with
// what it was asked and the variables the door gave it (argv, the clock, the store's directory, the repositories it
// may read, the case, its working directory), and answers with its arguments.
func evaluationDoorProgram(t *testing.T, dir, name string) string {
	t.Helper()
	return script(t, dir, name, `printf '%s|%s|%s|%s|%s|%s\n' "$*" "$MOMENTUM_NOW" "$MOMENTUM_HOME" "$MOMENTUM_LOOP_READ" "$MOMENTUM_LOOP_CASE" "$PWD" >> "`+filepath.Join(dir, "log")+`"
echo "the door answered: `+name+` $*"`)
}

// doorLines are what an evaluation's fake door program was asked, one line each, as evaluationDoorProgram logs them.
func doorLines(t *testing.T, e *Eval) []string {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(filepath.Dir(e.Runner.Momentum), "log"))
	if strings.TrimSpace(string(raw)) == "" {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// historyGit runs git in a synthetic repository, at a date when one is given.
func historyGit(t *testing.T, dir string, at time.Time, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	env := append(os.Environ(), "GIT_AUTHOR_NAME=r", "GIT_AUTHOR_EMAIL=r@example.com", "GIT_COMMITTER_NAME=r", "GIT_COMMITTER_EMAIL=r@example.com")
	if !at.IsZero() {
		env = append(env, "GIT_AUTHOR_DATE="+at.Format(time.RFC3339), "GIT_COMMITTER_DATE="+at.Format(time.RFC3339))
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitAt gives synthetic commits explicit dates, independent of the test's wall clock.
func commitAt(t *testing.T, dir string, at time.Time, file, contents, message string) string {
	t.Helper()
	t.Setenv("GIT_AUTHOR_DATE", at.Format(time.RFC3339))
	t.Setenv("GIT_COMMITTER_DATE", at.Format(time.RFC3339))
	write(t, filepath.Join(dir, file), contents)
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-qm", message)
	return strings.TrimSpace(run(t, dir, "git", "rev-parse", "HEAD"))
}

var (
	builtMomentum string
	builtOnce     sync.Once
	builtErr      error
)

// realMomentum is momentum's own binary, built once for the tests that run the commands a door runs: the evaluation's
// store, clock and checkouts are then read by the commands themselves, in the sandbox's door, as an evaluation runs
// them.
func realMomentum(t *testing.T) string {
	t.Helper()
	builtOnce.Do(func() {
		dir, err := os.MkdirTemp(testCache, "momentum-bin-")
		if err != nil {
			builtErr = err
			return
		}
		builtMomentum = filepath.Join(dir, "momentum")
		cmd := exec.Command("go", "build", "-buildvcs=false", "-o", builtMomentum, "../../cmd/momentum")
		if out, err := cmd.CombinedOutput(); err != nil {
			builtErr = &buildError{err, string(out)}
		}
	})
	if builtErr != nil {
		t.Fatal(builtErr)
	}
	return builtMomentum
}

type buildError struct {
	err error
	out string
}

func (b *buildError) Error() string { return "build momentum: " + b.err.Error() + ": " + b.out }
