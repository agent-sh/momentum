package detect

import "testing"

// A script the agent sent to Claude Code's REPL that read a property of undefined or null threw in the agent's own
// code: the virtual repl-error echo Claude Code writes ("TypeError: undefined is not an object") and the REPL's
// report that opens with it are no failure of the REPL. The REPL's own errors, other tools' TypeErrors and other
// script exceptions keep their keys.
func TestAREPLScriptsUndefinedReadIsNoFailure(t *testing.T) {
	for _, c := range []struct{ tool, text, want string }{
		{"REPL", "TypeError: undefined is not an object", ""},
		{"REPL", "TypeError: null is not an object", ""},
		{"REPL", "<repl-output> <error>TypeError: undefined is not an object (evaluating 'h13.slice') The script threw partway through", ""},
		{"REPL", "<repl-output> <error>The script did not run: it failed to parse, so nothing in it was executed.",
			"tool:repl:<repl-output> <error>the script did not run: it failed to parse, so nothing in"},
		{"REPL", "ReferenceError: W is not defined", "tool:repl:referenceerror: w is not defined"},
		{"REPL", "Syntax error, so nothing ran", "tool:repl:syntax error, so nothing ran"},
		{"mcp__page-tools__evaluate", "TypeError: undefined is not an object", "mcp:page-tools:typeerror: undefined is not an object"},
	} {
		k, ok := FailureOf("claude", c.tool, c.text, "{}")
		got := ""
		if ok {
			got = k.String()
		}
		if got != c.want {
			t.Errorf("%s %q:\n got %q\nwant %q", c.tool, c.text, got, c.want)
		}
	}
}
