package toolresult

import "testing"

func TestContentAndClaudeResultsNeedNativeFlags(t *testing.T) {
	for _, harness := range []string{"claude", "codex", "hermes", "pi", "opencode", "grok", "kimi", "unknown"} {
		for _, tool := range []string{"Read", "Grep", "Glob", "WebFetch", "read_file", "ReadFile", "grep"} {
			for _, text := range []string{
				"the script returns exit code 1 when notes.txt is missing",
				"Error: marker-7f3a is a heading",
				"Blocked: marker-7f3a",
				`{"error":"marker-7f3a","ok":false,"exit_code":1}`,
			} {
				if Failed(harness, tool, text, false) {
					t.Errorf("%s/%s inferred failure from content %q", harness, tool, text)
				}
				if !Failed(harness, tool, text, true) {
					t.Errorf("%s/%s discarded native flag", harness, tool)
				}
			}
		}
	}
	for _, text := range []string{"Exit code: 1 cat: notes.txt: No such file or directory", "Blocked: marker-7f3a", `{"error":"marker-7f3a","ok":false}`} {
		if Failed("claude", "Bash", text, false) {
			t.Errorf("Claude's native flag overridden by %q", text)
		}
	}
	for _, tc := range []struct{ harness, tool string }{
		{"pi", "bash"}, {"unknown", "Bash"}, {"codex", "Shell"}, {"grok", "terminal"},
	} {
		if Failed(tc.harness, tc.tool, "Exit code: 1 cat: notes.txt: No such file or directory", false) {
			t.Errorf("%s/%s borrowed another harness's inference rule", tc.harness, tc.tool)
		}
	}
}

func TestHarnessShellTools(t *testing.T) {
	for _, tc := range []struct{ harness, tool string }{
		{"claude", "Bash"}, {"codex", "exec"}, {"codex", "exec_command"}, {"codex", "write_stdin"},
		{"hermes", "terminal"}, {"hermes", "execute_code"}, {"pi", "bash"},
		{"opencode", "bash"}, {"grok", "run_terminal_command"}, {"kimi", "Shell"},
	} {
		t.Run(tc.harness+"/"+tc.tool, func(t *testing.T) {
			if !IsShell(tc.tool) {
				t.Fatal("shell is not recognized")
			}
			if tc.harness != "claude" && tc.harness != "pi" && !Failed(tc.harness, tc.tool, "Exit code: 1 cat: notes.txt: No such file or directory", false) {
				t.Fatal("shell's exit not inferred")
			}
		})
	}
}
