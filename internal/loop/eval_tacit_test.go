package loop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tacitStoreAt is a synthetic tacit store: a git repository with a fact before the end of the evaluation's store and
// one after it, and the commit of the first.
func tacitStoreAt(t *testing.T, e *Eval, task GoldenTask) (src, before string) {
	t.Helper()
	end, err := e.snapshotEnd(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	src = filepath.Join(dir, "tacit-store")
	historyGit(t, dir, end.Add(-48*time.Hour), "init", "-q", "-b", "main", src)
	before = commitAt(t, src, end.Add(-24*time.Hour), "lock-order.md", "the export takes the lock first", "a fact before the end")
	commitAt(t, src, end.Add(24*time.Hour), "lock-order-fixed.md", "the lock order was fixed", "a lesson after the end")
	return src, before
}

// fakeTacit is the tacit a door runs in a test, outside the sandbox: it answers a version, builds the clone's index
// as `hook index` does, and logs, for any other command, what tacit's environment gave it, one line.
func fakeTacit(t *testing.T, dir string) string {
	t.Helper()
	return script(t, dir, "tacit", `case "$1" in
version) echo "tacit v0.0.1-test"; exit 0;;
hook) touch "$TACIT_INDEX/index-v15-$(git -C "$TACIT_STORE" rev-parse HEAD).db"; exit 0;;
esac
printf '%s %s %s %s %s %s %s %s %s %s\n' "$TACIT_STORE" "$TACIT_LOCAL_LAYER" "$TACIT_SETTINGS" "$(ls "$TACIT_STORE" | tr '\n' ',')" "$TACIT_INDEX" "$TACIT_STATE_DIR" "$TACIT_LOCAL" "$(ls "$TACIT_INDEX" | tr '\n' ',')" "$(cat "$TACIT_SETTINGS")" "$(ls -A "$TACIT_LOCAL" | wc -l)" >> "`+filepath.Join(dir, "log")+`"
echo "tacit answered $*"`)
}

// A trial reads tacit's store as the machine held it at the end of store, through tacit's own commands: a clone at
// the end outside the trial's directory, with settings, state and a local layer of the run's own and empty; the
// reference names tacit, the door runs its reading commands, and the clone goes with the trial.
func TestATrialReadsTacitAsOfItsEnd(t *testing.T) {
	e, task := evalFixture(t)
	src, before := tacitStoreAt(t, e, task)
	e.Tacit = src
	e.Runner.Tacit = fakeTacit(t, filepath.Join(filepath.Dir(filepath.Dir(e.Source)), "tacit-door"))
	harness := filepath.Dir(e.Runner.Agent.Runner[0])
	e.Runner.Agent.Runner = []string{script(t, harness, "tacit-reader", `tacit search lock > tacit-answer.txt 2>&1
tacit fact lock-order > /dev/null 2>&1
`+modelResult(`{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":1,"structured_output":{"mechanism":"m","fix":"f","summary":"s"}}`))}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
	}
	raw, _ := os.ReadFile(filepath.Join(filepath.Dir(e.Runner.Tacit), "log"))
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 4 { // each arm ran two tacit commands
		t.Fatalf("%q", raw)
	}
	for i, tr := range trs {
		if tr.Err != "" {
			t.Fatal(tr.Err)
		}
		f := strings.Fields(lines[2*i])
		if len(f) != 10 || f[0] == src || strings.HasPrefix(f[0], tr.Run.Dir) || f[1] != "off" || !strings.Contains(f[3], "lock-order.md") || strings.Contains(f[3], "fixed") {
			t.Fatalf("%s: tacit as the trial read it: %q", tr.Arm, lines[2*i])
		}
		// the shared index, already built; state, settings and a local layer of the trial's own, outside its directory, empty
		index, state, local := f[4], f[5], f[6]
		private := filepath.Join(e.Dir, "private", tr.Key)
		if filepath.Dir(index) != filepath.Dir(f[0]) || !strings.Contains(f[7], "index-v15-"+before+".db") || f[8] != "{}" || f[9] != "0" ||
			strings.HasPrefix(state, tr.Run.Dir) || strings.HasPrefix(local, tr.Run.Dir) ||
			!strings.HasPrefix(state, private) || !strings.HasPrefix(local, private) || !strings.HasPrefix(f[2], private) {
			t.Fatalf("%s: tacit's index, state, settings or local layer: %q", tr.Arm, lines[2*i])
		}
		if !tr.TacitOffered || tr.TacitErr != "" || tr.TacitCommit != before || tr.TacitCalls != 2 {
			t.Errorf("%s: the trial does not say it was offered tacit and ran it twice: %v %q %q %d", tr.Arm, tr.TacitOffered, tr.TacitCommit, tr.TacitErr, tr.TacitCalls)
		}
		if _, err := os.Stat(f[0]); err == nil {
			t.Errorf("%s: the clone outlived its trial", tr.Arm)
		}
		prompt, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "prompt.txt"))
		if !strings.Contains(string(prompt), "tacit search WORDS") || strings.Index(string(prompt), "tacit search WORDS") > strings.Index(string(prompt), "No other momentum or tacit command") {
			t.Errorf("%s: the reference lacks tacit, or names it after the end of the list:\n%s", tr.Arm, prompt)
		}
		answer, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "tacit-answer.txt"))
		if !strings.Contains(string(answer), "tacit answered search lock") {
			t.Errorf("%s: the harness read %q", tr.Arm, answer)
		}
	}
}

// With no tacit store the trial has no tacit: the reference does not name it, the door refuses it, and the record says
// it was not offered.
func TestATrialWithoutTacitHasNone(t *testing.T) {
	e, task := evalFixture(t)
	harness := filepath.Dir(e.Runner.Agent.Runner[0])
	e.Runner.Agent.Runner = []string{script(t, harness, "wants-tacit", `tacit search lock > /dev/null 2>&1; echo $? > tacit-code.txt
`+modelResult(`{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":1,"structured_output":{"mechanism":"m","fix":"f","summary":"s"}}`))}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trs {
		prompt, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "prompt.txt"))
		code, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "tacit-code.txt"))
		if strings.Contains(string(prompt), "tacit search") || tr.TacitOffered || tr.TacitCalls != 0 || strings.TrimSpace(string(code)) != "126" {
			t.Fatalf("%s: tacit offered with no store (code %s)", tr.Arm, code)
		}
	}
}

// A trial whose tacit index cannot be built reads no tacit, and its record says why.
func TestATrialWhoseTacitIndexFailsSaysSo(t *testing.T) {
	e, task := evalFixture(t)
	src, _ := tacitStoreAt(t, e, task)
	e.Runner.Tacit = fakeTacit(t, filepath.Join(filepath.Dir(filepath.Dir(e.Source)), "tacit-door"))
	e.Tacit, e.TacitWarm = src, []string{script(t, t.TempDir(), "warm", "echo no index; exit 1")}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trs {
		prompt, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "prompt.txt"))
		if tr.TacitOffered || !strings.Contains(tr.TacitErr, "no index") || strings.Contains(string(prompt), "tacit search") {
			t.Fatalf("%s: %v %q", tr.Arm, tr.TacitOffered, tr.TacitErr)
		}
	}
}

// A warm-up that ends well but builds no index of the clone's commit (tacit's hook index exits 0 whatever happened)
// leaves both arms without tacit, and says why.
func TestAWarmUpThatBuildsNoIndexGivesNoTacit(t *testing.T) {
	e, task := evalFixture(t)
	src, _ := tacitStoreAt(t, e, task)
	e.Runner.Tacit = fakeTacit(t, filepath.Join(filepath.Dir(filepath.Dir(e.Source)), "tacit-door"))
	e.Tacit, e.TacitWarm = src, []string{script(t, t.TempDir(), "warm", `touch "$TACIT_INDEX/index-v15-0000.db.lock"; echo gave up waiting`)}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
	}
	for _, tr := range trs {
		prompt, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "prompt.txt"))
		if tr.TacitOffered || !strings.Contains(tr.TacitErr, "was not built") || strings.Contains(string(prompt), "tacit search") {
			t.Fatalf("%s: %v %q", tr.Arm, tr.TacitOffered, tr.TacitErr)
		}
	}
}

// The warm-up runs the tacit the arms run, the evaluation's copy, not the one the process's path finds.
func TestTheWarmUpRunsTheEvaluationsCopyOfTacit(t *testing.T) {
	e, task := evalFixture(t)
	src, _ := tacitStoreAt(t, e, task)
	dir := t.TempDir()
	e.Runner.Tacit = fakeTacit(t, filepath.Join(dir, "arms"))
	process := filepath.Join(dir, "process-bin")
	script(t, process, "tacit", `echo the process tacit; exit 1`)
	t.Setenv("PATH", process+string(os.PathListSeparator)+os.Getenv("PATH"))
	e.Tacit = src
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
	}
	for _, tr := range trs {
		if !tr.TacitOffered || tr.TacitErr != "" {
			t.Fatalf("%s: %v %q", tr.Arm, tr.TacitOffered, tr.TacitErr)
		}
	}
}

// A pair whose inputs cannot be made reads no tacit either, and its trials say why.
func TestATrialWithNoPairSaysWhyItHasNoTacit(t *testing.T) {
	e, task := evalFixture(t)
	src, _ := tacitStoreAt(t, e, task)
	e.Tacit = src
	e.Source = filepath.Join(t.TempDir(), "no-such-store.db")
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trs {
		if tr.TacitOffered || !strings.Contains(tr.TacitErr, "tacit's store as of the end") {
			t.Fatalf("%s: %v %q", tr.Arm, tr.TacitOffered, tr.TacitErr)
		}
	}
}

// A tacit replaced on the machine between the arms of a pair does not split the pair: both arms run the copy the
// evaluation made when it started, and the result names its hash and version.
func TestATacitReplacedBetweenTheArmsDoesNotSplitAPair(t *testing.T) {
	e, task := evalFixture(t)
	src, before := tacitStoreAt(t, e, task)
	dir := filepath.Join(filepath.Dir(filepath.Dir(e.Source)), "tacit-door")
	e.Tacit = src
	e.Runner.Tacit = script(t, dir, "tacit", `case "$1" in version) echo "tacit v1"; exit 0;; hook) touch "$TACIT_INDEX/index-v15-$(git -C "$TACIT_STORE" rev-parse HEAD).db"; exit 0;; esac
echo "tacit-build-1 answers $*"`)
	harness := filepath.Dir(e.Runner.Agent.Runner[0])
	replaced := filepath.Join(dir, "replaced")
	// the first arm's first command to momentum is a reinstall of tacit, as another session's install would be: a new
	// file written beside it and moved over it
	e.Runner.Momentum = script(t, dir, "momentum", `if mkdir "`+replaced+`" 2>/dev/null; then
  printf '#!/bin/sh\ncase "$1" in version) echo "tacit v2"; exit 0;; esac\necho "tacit-build-2 answers $*"\n' > "`+filepath.Join(dir, ".tacit-new")+`"
  chmod 755 "`+filepath.Join(dir, ".tacit-new")+`"; mv "`+filepath.Join(dir, ".tacit-new")+`" "`+e.Runner.Tacit+`"
fi
echo ok`)
	e.Runner.Agent.Runner = []string{script(t, harness, "tacit-reader", `tacit search lock > tacit-first.txt 2>&1
momentum evidence status > /dev/null 2>&1
tacit search lock > tacit-second.txt 2>&1
`+modelResult(`{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":1,"structured_output":{"mechanism":"m","fix":"f","summary":"s"}}`))}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
	}
	for _, tr := range trs {
		if tr.Err != "" || !tr.TacitOffered || tr.TacitCommit != before {
			t.Fatalf("%s: %+v", tr.Arm, tr)
		}
		for _, f := range []string{"tacit-first.txt", "tacit-second.txt"} {
			got, _ := os.ReadFile(filepath.Join(tr.Run.Dir, f))
			if strings.TrimSpace(string(got)) != "tacit-build-1 answers search lock" {
				t.Errorf("%s ran %s: %q", tr.Arm, f, got)
			}
		}
		if tr.Provenance.TacitVersion != "tacit v1" || len(tr.Provenance.Programs["tacit"]) != 64 {
			t.Errorf("%s: the trial names tacit %q %v", tr.Arm, tr.Provenance.TacitVersion, tr.Provenance.Programs)
		}
	}
	if err := e.WriteResults(e.Results(trs)); err != nil {
		t.Fatal(err)
	}
	var result EvaluationResults
	raw, _ := os.ReadFile(filepath.Join(e.Dir, "results.json"))
	if err := json.Unmarshal(raw, &result); err != nil || result.Provenance.TacitVersion != "tacit v1" || result.Provenance.Programs["tacit"] != trs[0].Provenance.Programs["tacit"] {
		t.Fatalf("results.json names %q %v: %v", result.Provenance.TacitVersion, result.Provenance.Programs, err)
	}
}

// A momentum replaced while the evaluation runs changes neither what the door runs nor what the sandbox holds: a
// trial's commands are the build the evaluation started with, and results.json names its hash.
func TestAMomentumReplacedWhileTheEvaluationRunsChangesNothing(t *testing.T) {
	e, task := evalFixture(t)
	dir := filepath.Dir(e.Runner.Momentum)
	momentum := e.Runner.Momentum
	harness := filepath.Dir(e.Runner.Agent.Runner[0])
	// the first arm's command replaces the installed momentum, as a rebuild would
	e.Runner.Momentum = script(t, dir, "momentum", `if mkdir "`+filepath.Join(dir, "rebuilt")+`" 2>/dev/null; then
  printf '#!/bin/sh\necho rebuilt\n' > "`+momentum+`.new"; chmod 755 "`+momentum+`.new"; mv "`+momentum+`.new" "`+momentum+`"
fi
echo this-build`)
	original := digestOfFile(t, e.Runner.Momentum)
	e.Runner.Agent.Runner = []string{script(t, harness, "runs-momentum", `momentum evidence status > said.txt 2>&1
`+modelResult(`{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":1,"structured_output":{"mechanism":"m","fix":"f","summary":"s"}}`))}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
	}
	for _, tr := range trs {
		said, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "said.txt"))
		if strings.TrimSpace(string(said)) != "this-build" {
			t.Errorf("%s: the door ran %q", tr.Arm, said)
		}
		if tr.Provenance.Programs["momentum"] != original {
			t.Errorf("%s: the trial names the build %q, want %s", tr.Arm, tr.Provenance.Programs["momentum"], original)
		}
	}
	if got, _ := os.ReadFile(momentum); !strings.Contains(string(got), "rebuilt") {
		t.Fatal("the test did not replace momentum")
	}
}

func digestOfFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256hex(raw)
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
