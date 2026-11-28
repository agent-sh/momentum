package detect

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A script the agent sent to Claude Code's REPL that read a property of undefined or null threw in the agent's own
// code: the virtual repl-error echo Claude Code writes ("TypeError: undefined is not an object") and the REPL's
// report that opens with it are no failure of the REPL. The REPL's own errors, other tools' TypeErrors and other
// script exceptions keep their keys.
func TestAREPLScriptsUndefinedReadIsNoFailure(t *testing.T) {
	for _, c := range []struct{ tool, text, want string }{
		{"REPL", "TypeError: undefined is not an object", ""},
		{"REPL", "TypeError: null is not an object", ""},
		{"REPL", "<repl-output> <error>TypeError: undefined is not an object (evaluating 'hunks[13]') The script threw partway through", ""},
		{"REPL", "<repl-output> <error>The script did not run: it failed to parse, so nothing in it was executed",
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

// The virtual repl-error echoes Claude Code writes for scripts that threw, as the transcripts keep them (a synthetic
// REPL call with input {}, its is_error result cut to the exception's first clause), four in two sessions, file no
// finding. A REPL error of the harness's own, as often, is still filed.
func TestREPLScriptEchoesFileNoFinding(t *testing.T) {
	st := testStore(t)
	for i, session := range []string{"s1", "s1", "s2", "s2"} {
		at := t0.Add(time.Duration(i) * time.Hour)
		id := fmt.Sprintf("repl-error-%04d", i)
		put(t, st, call(session, "REPL", id, "{}", at),
			result(session, "REPL", id, "TypeError: undefined is not an object", at.Add(time.Second)))
	}
	fs, err := Failures{}.Detect(context.Background(), st, week())
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 0 {
		t.Fatalf("script echoes filed %v", signatures(fs))
	}
	for i, session := range []string{"s3", "s3", "s4", "s4"} {
		at := t0.Add(time.Duration(i) * time.Hour)
		put(t, st, toolError(session, "REPL", "<repl-output> <error>The script did not run: it failed to parse, so nothing in it was executed", at))
	}
	if fs, err = (Failures{}).Detect(context.Background(), st, week()); err != nil {
		t.Fatal(err)
	}
	if len(fs) != 1 || !strings.HasPrefix(fs[0].Signature, "failures/v2:tool:repl:") || len(fs[0].Evidence) != 4 {
		t.Fatalf("the REPL's own error: %v", signatures(fs))
	}
}
