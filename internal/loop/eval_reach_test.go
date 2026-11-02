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
	seen := filepath.Join(dir, "seen")
	e.Runner.Agent.Runner = []string{script(t, dir, "tacit-reader", `printf '%s %s %s %s\n' "$TACIT_STORE" "$TACIT_LOCAL_LAYER" "$TACIT_SETTINGS" "$(ls "$TACIT_STORE" | tr '\n' ',')" >> `+seen+`
printf '%s\n' '{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":1,"structured_output":{"mechanism":"m","fix":"f","summary":"s"}}'`)}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
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
		if len(f) != 4 || f[0] == src || strings.HasPrefix(f[0], tr.Run.Dir) || f[1] != "off" || !strings.Contains(f[3], "lock-order.md") || strings.Contains(f[3], "fixed") {
			t.Fatalf("%s: tacit as the trial read it: %q", tr.Arm, lines[i])
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

// With no tacit store the trial has no tacit, as before.
func TestATrialWithoutTacitHasNone(t *testing.T) {
	e, task := evalFixture(t)
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trs {
		prompt, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "prompt.txt"))
		settings, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "settings.json"))
		if strings.Contains(string(prompt), "tacit") || strings.Contains(string(settings), "Bash(tacit") {
			t.Fatalf("%s: tacit offered with no store", tr.Arm)
		}
	}
}
