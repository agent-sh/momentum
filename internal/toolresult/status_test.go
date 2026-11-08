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
		{"opencode", "bash"}, {"grok", "run_terminal_command"}, {"kimi", "Bash"},
	} {
		t.Run(tc.harness+"/"+tc.tool, func(t *testing.T) {
			if !IsShell(tc.tool) {
				t.Fatal("shell is not recognized")
			}
			inferred := Failed(tc.harness, tc.tool, "Exit code: 1 cat: notes.txt: No such file or directory", false)
			if want := tc.harness == "codex" || tc.harness == "hermes"; inferred != want {
				t.Fatalf("shell's exit inferred=%v, want %v", inferred, want)
			}
			// the same words inside a command's own output are no exit
			if Failed(tc.harness, tc.tool, "the script returns exit code 1 when notes.txt is missing", false) || Failed(tc.harness, tc.tool, "error: marker-7f3a is in the log", false) && tc.harness != "codex" && tc.harness != "hermes" {
				t.Fatalf("%s/%s read a command's own output as a failure", tc.harness, tc.tool)
			}
		})
	}
}

func TestCancelledAndPatchRefusalsAreRead(t *testing.T) {
	if Failed("codex", "exec_command", "Tool execution cancelled by user", true) {
		t.Error("a flagged cancellation was a failure")
	}
	if !Failed("codex", "apply_patch", "apply_patch verification failed: invalid hunk", false) {
		t.Error("a patch refusal in plain text was no failure")
	}
	if Failed("codex", "exec_command", "Exit code: 0\nOutput: the script returns exit code 1", false) {
		t.Error("an exit of 0 with words in its output was a failure")
	}
}

// A result wrapper that parses is read by its top-level fields alone; one the store cut is read by the same marks,
// in its top level only.
func TestAnEnvelopeIsReadByItsTopLevel(t *testing.T) {
	for _, c := range []struct {
		text   string
		failed bool
	}{
		// Hermes's write_file wrote and verified the file; its lint, nested, found an error
		{`{"bytes_written": 120, "verified": true, "lint": {"status": "error", "output": "SyntaxError"}, "resolved_path": "/work/a.py"}`, false},
		{`{"success": false, "error": "Could not find a match"}`, true},
		{`{"output": "done", "exit_code": 1, "error": null}`, true},
		{`{"output": "done", "exit_code": 0, "error": null}`, false},
		{`{"error": {"code": -32603, "message": "permission denied"}}`, true},
		// cut: a nested object's marks are passed over, the top level's read
		{`{"bytes_written": 120, "lint": {"status": "error", "output": "SyntaxErr…`, false},
		{`{"success": false, "error": "Could not find a match for old_str…`, true},
		{`{"output": "a { brace", "exit_code": 2, "out…`, true},
	} {
		if got := EnvelopeFailed(c.text); got != c.failed {
			t.Errorf("%s: got %v, want %v", c.text, got, c.failed)
		}
	}
}
