package detect

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func TestContentDoesNotOpenFailureCases(t *testing.T) {
	for _, harness := range store.Harnesses {
		for _, tool := range []string{"Read", "Grep", "Glob", "WebFetch"} {
			for _, text := range []string{"the script returns exit code 1 when notes.txt is missing", "Error: marker-7f3a", "Blocked: marker-7f3a"} {
				st := testStore(t)
				for i := range 3 {
					e := ev(store.Transcripts, fmt.Sprintf("s%d", i%2), "tool_result", store.ActorTool, tool, text, `{"is_error":false}`, t0.Add(time.Duration(i)*time.Minute))
					e.Harness = harness
					put(t, st, e)
				}
				if fs, err := (Failures{}).Detect(context.Background(), st, week()); err != nil || len(fs) != 0 {
					t.Fatalf("%s/%s content opened %d cases: %v", harness, tool, len(fs), err)
				}
			}
		}
	}
}

func TestEveryHarnessShellKeysOnlySetupFailures(t *testing.T) {
	for _, tc := range []struct{ harness, tool string }{
		{"claude", "Bash"}, {"codex", "exec_command"}, {"hermes", "terminal"},
		{"pi", "bash"}, {"opencode", "bash"}, {"grok", "run_terminal_command"}, {"kimi", "Shell"},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			a, ok := FailureOf(tc.harness, tc.tool, `{"type":"Bash","exit_code":1,"output":"cat: notes.txt: No such file or directory"}`, `{"command":"cat notes.txt"}`)
			b, other := FailureOf(tc.harness, tc.tool, `{"type":"Bash","exit_code":1,"output":"curl: Could not resolve host: neutral.invalid"}`, `{"command":"curl neutral.invalid"}`)
			if !ok || !other || a.Origin != "exec" || b.Origin != "exec" || a == b {
				t.Fatalf("unrelated setup errors merged: %+v / %+v", a, b)
			}
			if _, ok := FailureOf(tc.harness, tc.tool, "--- FAIL: TestMarker (0.01s) exit status 1", `{"command":"go test"}`); ok {
				t.Fatal("work failure keyed as setup")
			}
			for _, command := range []string{"grep marker-7f3a notes.txt", "rg marker-7f3a notes.txt", "diff notes.txt other.txt", "test -f notes.txt", "[ -f notes.txt ]"} {
				if _, ok := FailureOf(tc.harness, tc.tool, `{"exit_code":1,"output":""}`, `{"command":"`+command+`"}`); ok {
					t.Fatalf("ordinary predicate exit keyed: %s", command)
				}
			}
		})
	}
}
