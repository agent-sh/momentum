package loop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A trial reads tacit's store as the machine held it at the end of store, through tacit's own commands: a clone
// at the end outside the trial's directory, with settings, state and a local layer of the run's own and empty; the
// reference names tacit, the permissions allow its reading commands, and the clone goes when the trial ends.
func TestATrialReadsTacitAsOfItsEnd(t *testing.T) {
	e, task := evalFixture(t)
	end, err := e.snapshotEnd(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "tacit-store")
	historyGit(t, dir, end.Add(-48*time.Hour), "init", "-q", "-b", "main", src)
	commitAt(t, src, end.Add(-24*time.Hour), "lock-order.md", "the export takes the lock first", "a fact before the end")
	commitAt(t, src, end.Add(24*time.Hour), "lock-order-fixed.md", "the lock order was fixed", "a lesson after the end")
	e.Tacit = src
	// the index is built before either arm runs: the warm-up leaves the clone's index in it, which both arms must
	// find already there
	e.TacitWarm = []string{script(t, dir, "warm", `touch "$TACIT_INDEX/index-v15-$(git -C "$TACIT_STORE" rev-parse HEAD).db"`)}
	seen := filepath.Join(dir, "seen")
	// the harness runs the user's shell startup before each command, which here points tacit at the live store; the
	// trial's env file, sourced after it, points it back
	functions := filepath.Join(dir, "functions")
	e.Runner.Agent.Runner = []string{script(t, dir, "tacit-reader", `TACIT_STORE=`+src+`; export TACIT_STORE
tacit() { echo the live store; }
if [ -n "$CLAUDE_ENV_FILE" ]; then . "$CLAUDE_ENV_FILE"; fi
command -v tacit >> `+functions+`
printf '%s %s %s %s %s %s %s %s %s %s\n' "$TACIT_STORE" "$TACIT_LOCAL_LAYER" "$TACIT_SETTINGS" "$(ls "$TACIT_STORE" | tr '\n' ',')" "$TACIT_INDEX" "$TACIT_STATE_DIR" "$TACIT_LOCAL" "$(ls "$TACIT_INDEX" | tr '\n' ',')" "$(cat "$TACIT_SETTINGS")" "$(ls -A "$TACIT_LOCAL" | wc -l)" >> `+seen+`
printf '%s\n' '{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":1,"structured_output":{"mechanism":"m","fix":"f","summary":"s"}}'`)}
	before := strings.TrimSpace(run(t, src, "git", "rev-parse", "HEAD~1"))
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
	}
	if raw, _ := os.ReadFile(functions); len(raw) != 0 {
		t.Fatalf("a tacit of the startup's own stayed: %q", raw)
	}
	raw, _ := os.ReadFile(seen)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("%q", raw)
	}
	for i, tr := range trs {
		if tr.Err != "" {
			t.Fatal(tr.Err)
		}
		f := strings.Fields(lines[i])
		if len(f) != 10 || f[0] == src || strings.HasPrefix(f[0], tr.Run.Dir) || f[1] != "off" || !strings.Contains(f[3], "lock-order.md") || strings.Contains(f[3], "fixed") {
			t.Fatalf("%s: tacit as the trial read it: %q", tr.Arm, lines[i])
		}
		// the shared index, already built; state, settings and a local layer of the run's own, outside the trial, empty
		index, state, local := f[4], f[5], f[6]
		if filepath.Dir(index) != filepath.Dir(f[0]) || !strings.Contains(f[7], "index-v15-"+before+".db") || f[8] != "{}" || f[9] != "0" || strings.HasPrefix(state, tr.Run.Dir) || strings.HasPrefix(local, tr.Run.Dir) ||
			!strings.HasPrefix(state, filepath.Join(e.Dir, "harness")) || !strings.HasPrefix(local, filepath.Join(e.Dir, "harness")) || !strings.HasPrefix(f[2], filepath.Join(e.Dir, "harness")) {
			t.Fatalf("%s: tacit's index, state, settings or local layer: %q", tr.Arm, lines[i])
		}
		if !tr.Tacit || tr.TacitErr != "" || tr.TacitCommit != before {
			t.Errorf("%s: the trial does not say it read tacit, or what: %v %q %q", tr.Arm, tr.Tacit, tr.TacitCommit, tr.TacitErr)
		}
		if _, err := os.Stat(f[0]); err == nil {
			t.Errorf("%s: the clone outlived its trial", tr.Arm)
		}
		prompt, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "prompt.txt"))
		if !strings.Contains(string(prompt), "tacit search WORDS") || strings.Index(string(prompt), "tacit search WORDS") > strings.Index(string(prompt), "No other shell commands") {
			t.Errorf("%s: the reference lacks tacit, or names it after the end of the list:\n%s", tr.Arm, prompt)
		}
		settings, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "settings.json"))
		var s struct{ Permissions Settings }
		json.Unmarshal(settings, &s)
		allowed := strings.Join(s.Permissions.Allow, " ")
		if !strings.Contains(allowed, "Bash(tacit search *)") || !strings.Contains(allowed, "Bash(momentum evidence unit *)") || strings.Contains(allowed, "evidence file") {
			t.Errorf("%s: permissions %s", tr.Arm, allowed)
		}
	}
}

// With no tacit store the trial has no tacit, as before, and its record says so.
func TestATrialWithoutTacitHasNone(t *testing.T) {
	e, task := evalFixture(t)
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trs {
		prompt, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "prompt.txt"))
		settings, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "settings.json"))
		if strings.Contains(string(prompt), "tacit") || strings.Contains(string(settings), "Bash(tacit") || tr.Tacit {
			t.Fatalf("%s: tacit offered with no store", tr.Arm)
		}
	}
}

// A trial whose tacit index cannot be built reads no tacit, and its record says why.
func TestATrialWhoseTacitIndexFailsSaysSo(t *testing.T) {
	e, task := evalFixture(t)
	end, err := e.snapshotEnd(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "tacit-store")
	historyGit(t, dir, end.Add(-48*time.Hour), "init", "-q", "-b", "main", src)
	commitAt(t, src, end.Add(-24*time.Hour), "a.md", "a fact", "a fact before the end")
	e.Tacit, e.TacitWarm = src, []string{script(t, dir, "warm", "echo no index; exit 1")}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trs {
		prompt, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "prompt.txt"))
		if tr.Tacit || !strings.Contains(tr.TacitErr, "no index") || strings.Contains(string(prompt), "tacit search") {
			t.Fatalf("%s: %v %q", tr.Arm, tr.Tacit, tr.TacitErr)
		}
	}
}

// A warm-up that ends well but builds no index of the clone's commit (tacit's hook index exits 0 whatever happened)
// leaves both arms without tacit, and says why.
func TestAWarmUpThatBuildsNoIndexGivesNoTacit(t *testing.T) {
	e, task := evalFixture(t)
	end, err := e.snapshotEnd(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "tacit-store")
	historyGit(t, dir, end.Add(-48*time.Hour), "init", "-q", "-b", "main", src)
	commitAt(t, src, end.Add(-24*time.Hour), "a.md", "a fact", "a fact before the end")
	e.Tacit, e.TacitWarm = src, []string{script(t, dir, "warm", `touch "$TACIT_INDEX/index-v15-0000.db.lock"; echo gave up waiting`)}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
	}
	for _, tr := range trs {
		prompt, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "prompt.txt"))
		if tr.Tacit || !strings.Contains(tr.TacitErr, "was not built") || strings.Contains(string(prompt), "tacit search") {
			t.Fatalf("%s: %v %q", tr.Arm, tr.Tacit, tr.TacitErr)
		}
	}
}

// The warm-up runs the tacit the arms run, the first on their path, not the evaluating process's.
func TestTheWarmUpRunsTheArmsTacit(t *testing.T) {
	e, task := evalFixture(t)
	end, err := e.snapshotEnd(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "tacit-store")
	historyGit(t, dir, end.Add(-48*time.Hour), "init", "-q", "-b", "main", src)
	commitAt(t, src, end.Add(-24*time.Hour), "a.md", "a fact", "a fact before the end")
	arms, process := filepath.Join(dir, "arms-bin"), filepath.Join(dir, "process-bin")
	os.MkdirAll(arms, 0o700)
	os.MkdirAll(process, 0o700)
	script(t, arms, "tacit", `touch "$TACIT_INDEX/index-v15-$(git -C "$TACIT_STORE" rev-parse HEAD).db"`)
	script(t, process, "tacit", `echo the evaluating process tacit; exit 1`)
	t.Setenv("PATH", process+string(os.PathListSeparator)+os.Getenv("PATH"))
	e.Tacit, e.Runner.Bins = src, []string{arms}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
	}
	for _, tr := range trs {
		if !tr.Tacit || tr.TacitErr != "" {
			t.Fatalf("%s: %v %q", tr.Arm, tr.Tacit, tr.TacitErr)
		}
	}
}

// A pair whose store cannot be made reads no tacit either, and its trials say why.
func TestATrialWithNoPairSaysWhyItHasNoTacit(t *testing.T) {
	e, task := evalFixture(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "tacit-store")
	end, err := e.snapshotEnd(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	historyGit(t, dir, end.Add(-48*time.Hour), "init", "-q", "-b", "main", src)
	commitAt(t, src, end.Add(-24*time.Hour), "a.md", "a fact", "a fact before the end")
	e.Tacit = src
	e.Source = filepath.Join(dir, "no-such-store.db")
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trs {
		if tr.Tacit || !strings.Contains(tr.TacitErr, "tacit's store as of the end") {
			t.Fatalf("%s: %v %q", tr.Arm, tr.Tacit, tr.TacitErr)
		}
	}
}
