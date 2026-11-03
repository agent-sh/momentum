package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/sandbox"
	"github.com/agent-sh/momentum/internal/store"
)

const evalResult = `{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":999,"structured_output":{"mechanism":"marker-7f3a cause","alternatives":["another cause"],"fix":"marker-7f3a fix","summary":"marker-7f3a report"}}`
const evalGrade = `{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":999,"structured_output":{"verdict":"reached","facts":[],"reason":"marker-7f3a reason"}}`

func requireEvalSandbox(t *testing.T) {
	t.Helper()
	if err := sandbox.Available(true); err != nil {
		if os.Getenv("MOMENTUM_REQUIRE_SANDBOX") != "" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
}

func sandboxEval(t *testing.T, body, grade string) (*Eval, GoldenTask) {
	t.Helper()
	requireEvalSandbox(t)
	e, task := evalFixture(t)
	e.Dir = shortScratch(t)
	e.Runner.Agent = Agent{Runner: []string{script(t, t.TempDir(), "agent", body+"\n"+fixtureResult(evalResult))},
		Model: "global.anthropic.claude-opus-5-5", Bedrock: testBedrock(t)}
	e.Judge = Agent{Runner: []string{script(t, t.TempDir(), "judge", grade)},
		Model: "global.anthropic.claude-opus-5-5", Bedrock: testBedrock(t)}
	return e, task
}

func TestEvaluationRealSandboxSharesAsOfInputsAndIsolatesJudge(t *testing.T) {
	body := `python3 - <<'PY'
import pathlib, hashlib, sqlite3, json, os
root=pathlib.Path('/ro')
assert os.getcwd() == '/work'
assert not list(pathlib.Path('/work').iterdir())
assert not pathlib.Path('/usr/bin/momentum').exists()
paths=sorted(root.rglob('*'))
listing=[]
for path in paths:
    name=path.as_posix()
    if path.is_symlink():
        listing.append(name+' link '+os.readlink(path))
    elif path.is_dir():
        listing.append(name+' directory')
    else:
        listing.append(name+' '+hashlib.sha256(path.read_bytes()).hexdigest())
        try:
            with path.open('ab') as out: out.write(b'changed')
        except OSError: pass
        else: raise AssertionError('writable /ro input')
connection=sqlite3.connect('file:/ro/store.sqlite?mode=ro',uri=True)
assert connection.execute("SELECT count(*) FROM events WHERE source_key='later'").fetchone()[0] == 0
assert connection.execute("SELECT count(*) FROM events WHERE source_key='symptom'").fetchone()[0] == 1
assert connection.execute("SELECT max(ts) FROM events").fetchone()[0] < 1772445660000
pathlib.Path('/out/listing.txt').write_text('\n'.join(listing))
assert pathlib.Path('/ro/repos/repo-1/file').read_text() == 'marker-7f3a before\n'
assert pathlib.Path('/ro/clone/file').read_text() == 'marker-7f3a before\n'
assert len(list(pathlib.Path('/run/input').iterdir())) == 3
assert not pathlib.Path('/run/input/golden.jsonl').exists()
PY
git -C /ro/repos/repo-1 log --all --format=%s > /out/repository-log
git -C /ro/clone log --all --format=%s > /out/tacit-log`
	judge := `python3 - "$@" <<'PY'
import pathlib, sys
args=sys.argv[1:]
assert '--add-dir' not in args
assert args[args.index('--tools')+1] == ''
assert '--fallback-model' not in args
assert sorted(p.name for p in pathlib.Path('/run/input').iterdir()) == ['schema.json','system.md']
assert not pathlib.Path('/ro').exists()
assert not pathlib.Path('/work').exists()
assert not pathlib.Path('/out').exists()
PY
` + fixtureResult(evalGrade)
	e, task := sandboxEval(t, body, judge)
	end, _ := e.snapshotEnd(context.Background(), task)
	e.Runner.Inputs.ReadDirs = nil
	repo, _, _ := evalRepository(t, end, "example")
	tacit, _, _ := evalRepository(t, end, "facts")
	e.Repositories, e.Tacit, e.Trials, e.Parallel, e.Budget = []string{repo}, tacit, 2, 2, 120
	src, err := store.Open(e.Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Put(context.Background(), []store.Event{{Source: "fixture", SourceKey: "later", Kind: "tool_result", Actor: store.ActorTool, TS: end.Add(time.Hour), Summary: "later answer"}}); err != nil {
		t.Fatal(err)
	}
	src.Close()
	trials, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trials) != 4 {
		t.Fatalf("evaluation: %v, %d trials", err, len(trials))
	}
	var listing string
	for i, tr := range trials {
		if tr.Err != "" || tr.Grade.Verdict != "reached" || tr.Run.Status != store.RunOK || tr.Run.USD != fixtureCharge(1) ||
			tr.Run.Usage.Total() != fixtureTokens(1) || tr.Run.Turns != 1 || tr.JudgeUSD != fixtureCharge(1) {
			t.Fatalf("trial and relay accounting: %+v", tr)
		}
		got := readText(t, filepath.Join(tr.Run.Dir, "out", "listing.txt"))
		if i == 0 {
			listing = got
		} else {
			sameTexts(t, listing, got)
		}
		if strings.TrimSpace(readText(t, filepath.Join(tr.Run.Dir, "out", "repository-log"))) != "before" ||
			strings.TrimSpace(readText(t, filepath.Join(tr.Run.Dir, "out", "tacit-log"))) != "before" {
			t.Fatal("later history reached an investigation")
		}
		stored, err := e.Runner.Store.LoopRunByID(context.Background(), tr.Run.ID)
		if err != nil || stored.Experiment != "eval:"+tr.Key+":"+tr.Arm {
			t.Fatal("trial lacks its experiment tag")
		}
		if tr.InputHashes["/ro/records/ev-"+task.Symptom.Query.EventIDs[0]+".txt"] == "" {
			t.Fatal("raw record capture missing from trial")
		}
		if strings.Contains(tr.Run.Dir+tr.Run.Experiment+readText(t, filepath.Join(HostDir(tr.Run.Dir), "prompt.txt")), task.ID) {
			t.Fatal("answer-bearing task name reached an arm")
		}
	}
	spend, err := e.Runner.Store.LoopSpend(context.Background(), time.Time{}, 4)
	if err != nil || spend != 0 {
		t.Fatal("evaluation spend entered live budgets")
	}
	inputs, err := os.ReadDir(filepath.Join(e.Dir, "inputs"))
	if err != nil || len(inputs) != 0 {
		t.Fatal("task inputs remain after the evaluation")
	}
}

func TestEvaluationRealSandboxFailedStartChargesNothingAndExcludesPair(t *testing.T) {
	e, task := sandboxEval(t, "", fixtureResult(evalGrade))
	e.Runner.Agent.Runner = []string{filepath.Join(t.TempDir(), "absent")}
	trials, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trials) != 2 {
		t.Fatalf("evaluation: %v", err)
	}
	for _, tr := range trials {
		if tr.Err == "" || !tr.Run.Started.IsZero() || tr.Run.USD != 0 || tr.JudgeUSD != 0 || tr.Grade.Verdict != "" {
			t.Fatalf("failed start charged or graded: %+v", tr)
		}
	}
	for _, arm := range e.Results(trials).Arms {
		if arm.ComparedTasks != 0 || arm.Errors != 1 || arm.PassAtK != nil {
			t.Fatal("failed-start pair entered comparison")
		}
	}
}

func TestEvaluationRealSandboxMalformedJudgeKeepsBothCostsAndReason(t *testing.T) {
	bad := `{"type":"result","subtype":"success","num_turns":1,"structured_output":{"verdict":"reached"}}`
	e, task := sandboxEval(t, "", fixtureResult(bad))
	trials, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trials {
		if !strings.Contains(tr.Err, "missing member") || tr.JudgeUSD != fixtureCharge(2) {
			t.Fatalf("judge lost its error or retry cost: %+v", tr)
		}
		for _, name := range []string{"transcript.jsonl", "transcript.fallback.jsonl"} {
			if _, err := os.Stat(filepath.Join(HostDir(tr.JudgeDir), name)); err != nil {
				t.Fatal("judge lost an attempt's archive")
			}
		}
	}
	for _, arm := range e.Results(trials).Arms {
		if arm.ComparedTasks != 0 || arm.Ungraded != 1 {
			t.Fatal("malformed grade entered comparison")
		}
	}
}

func TestEvaluationBudgetReservesRelayMarginAndBothArms(t *testing.T) {
	log := fakeEvalManager(t)
	e, task := evalFixture(t)
	route := filepath.Join(t.TempDir(), "route.env")
	write(t, route, "CLAUDE_CODE_USE_BEDROCK=1\nANTHROPIC_BEDROCK_BASE_URL=http://127.0.0.1:9/fixture\nCLAUDE_CODE_SKIP_BEDROCK_AUTH=1\n")
	e.Runner.Agent.Bedrock, e.Judge.Bedrock = &Bedrock{File: route}, &Bedrock{File: route}
	e.Budget, e.Trials, e.Parallel = 29.99, 3, 3
	trials, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trials) != 6 {
		t.Fatalf("budget: %v %d", err, len(trials))
	}
	for _, tr := range trials {
		if tr.Skipped == "" || tr.Run.ID != "" || tr.Err != "" {
			t.Fatal("admitted a pair without the relay margin")
		}
	}
	runs, err := e.Runner.Store.LoopRuns(context.Background(), store.RunFilter{})
	if err != nil || len(runs) != 0 {
		t.Fatal("budget skipped pair left reservations")
	}
	if got := readText(t, log); got != "probe\nlist\nstop momentum-run-eval-old.service\n" {
		t.Fatal("evaluation probed or stopped the wrong unit scope")
	}
}

func TestEvaluationLauncherFailureRecordsNoStartOrCharge(t *testing.T) {
	// The manager starts and exits successfully without entering bubblewrap.
	// This is a launcher regression fixture, not a boundary test.
	fakeEvalManager(t)
	e, task := evalFixture(t)
	e.Runner.Agent = Agent{Runner: []string{script(t, t.TempDir(), "agent", fixtureResult(evalResult))},
		Model: "global.anthropic.claude-opus-5-5", Bedrock: testBedrock(t)}
	e.Judge = e.Runner.Agent
	trials, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trials {
		if tr.Err == "" || !tr.Run.Started.IsZero() || tr.Run.USD != 0 || tr.JudgeUSD != 0 {
			t.Fatal("a launched manager was counted as an investigation")
		}
	}
}

func fakeEvalManager(t *testing.T) string {
	t.Helper()
	log := fakeManager(t)
	dir := strings.Split(os.Getenv("PATH"), ":")[0]
	path := filepath.Join(dir, "systemctl")
	raw := readText(t, path)
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(raw, UnitPrefix, "momentum-run-eval-")), 0700); err != nil {
		t.Fatal(err)
	}
	return log
}

func TestEvaluationRelaySocketWorksUnderALongArchivePath(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, strings.Repeat("fixture", 20))
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "relay.sock")
	ln, err := listenRelaySocket(socket)
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skip("this sandbox denies fixture Unix sockets")
		}
		t.Fatal(err)
	}
	defer os.Remove(socket)
	defer ln.Close()
	if info, err := os.Lstat(socket); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatal("long-path socket was not created at its ordinary path")
	}
}
