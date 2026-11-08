package request

// Tests from the third review of the requests change: an artifact is only what a line of a call's output shows.
// Synthetic data only.

import (
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// harnessOf is the harness whose tool a test's call is: Codex's exec tools, Hermes's terminal and writers, else
// Claude Code's.
func harnessOf(tool string) string {
	switch tool {
	case "exec", "exec_command":
		return "codex"
	case "terminal", "write_file", "patch":
		return "hermes"
	}
	return "claude"
}

// artifactsAs is Artifacts on a call of tool and its result, failed or not, as its harness keeps them.
func artifactsAs(tool, input, output string, failed bool) string {
	h := harnessOf(tool)
	call := store.Event{Source: store.Transcripts, SourceKey: "c", TS: at(1), Harness: h, Tool: tool, Kind: "tool_call", Summary: input}
	res := store.Event{Source: store.Transcripts, SourceKey: "r", TS: at(1).Add(time.Second), Harness: h, Tool: tool, Kind: "tool_result", Summary: output}
	if failed {
		res.Meta = `{"is_error":true}`
	}
	return kinds(Artifacts(call, res))
}

// A command that shows nothing made nothing this can see, whatever its success seems to prove: git and gh print
// nothing for a quiet commit or push, and a successful call proves little of each command it ran (a merge already
// up to date, an exit before the commit, an EXIT trap, a script's inner command, a command still running).
func TestOnlyAShownLineIsAnArtifact(t *testing.T) {
	cases := []struct {
		name, tool, input, output string
		failed                    bool
		want                      string
	}{
		{"a quiet commit", "Bash", `{"command":"git commit -qm x"}`, "", false, ""},
		{"a quiet push", "Bash", `{"command":"git push -q origin main"}`, "", false, ""},
		{"a quiet && chain", "Bash", `{"command":"cd /work/x && git add -A && git commit -qm x && git push -q origin main"}`, "", false, ""},
		{"a shell's -c script", "Bash", `{"command":"bash -lc 'git push -q origin main'"}`, "", false, ""},
		{"Codex's workdir", "exec_command", `{"cmd":"git commit -qm x","workdir":"/work/w"}`, "Process exited with code 0", false, ""},
		{"an address kept in a variable", "Bash", `{"command":"N=$(gh pr create --fill) && echo ok"}`, "ok", false, ""},
		{"merge -q, already up to date", "Bash", `{"command":"git merge -q origin/main"}`, "", false, ""},
		{"rebase -q, up to date", "Bash", `{"command":"git fetch -q origin && git rebase -q origin/main"}`, "", false, ""},
		{"merge --ff-only, a fast-forward", "Bash", `{"command":"git merge --ff-only topic"}`,
			"Updating 1a2b3c4..5d6e7f8 Fast-forward b | 1 + 1 file changed, 1 insertion(+) create mode 100644 b", false, ""},
		{"merge --squash", "Bash", `{"command":"git merge --squash topic"}`,
			"Updating 1a2b3c4..5d6e7f8 Fast-forward Squash commit -- not updating HEAD b | 1 + 1 file changed", false, ""},
		{"exit before the commit", "Bash", `{"command":"[ -z \"$(git status --porcelain)\" ] && exit 0; git commit -qm x"}`, "", false, ""},
		{"exit in a branch before the push", "Bash", `{"command":"if git diff --quiet origin/main; then exit 0; fi; git push -q origin main"}`, "", false, ""},
		{"an EXIT trap", "Bash", `{"command":"trap 'exit 0' EXIT; git push -q origin main"}`,
			"! [rejected] main -> main (fetch first) error: failed to push some refs to 'github.com:example/x.git'", false, ""},
		{"Codex code mode, an inner push failed", "exec",
			`const a = await tools.exec_command({cmd: "git push -q origin main"}); text(a)`,
			`Script completed Wall time 1.1 seconds Output: {"chunk_id":"1a","exit_code":1,"output":"error: failed to push some refs to 'github.com:example/x.git'"}`,
			false, ""},
		{"Claude Code, run in the background", "Bash", `{"command":"git push -q origin main","run_in_background":true}`,
			"Command running in background with ID: b1a2c3d. Output is being written to: /work/tasks/b1a2c3d.output", false, ""},
		{"Claude Code, moved to the background", "Bash", `{"command":"git commit -qm 'Fix the parser'"}`,
			"Command was moved to the background (ID: b4e5f6a) after running for 180s. Output is being written to: /work/tasks/b4e5f6a.output", false, ""},
		{"Codex, still running at the yield", "exec_command", `{"cmd":"git push -q origin main","yield_time_ms":10000}`,
			"Chunk ID: 2a Wall time: 10.0 seconds Process running with session ID 4242 Output: running the pre-push checks", false, ""},
		{"everything up to date", "Bash", `{"command":"git push origin main"}`, "Everything up-to-date", false, ""},
		{"a shown commit line", "Bash", `{"command":"git commit -m x && git push -q origin main"}`, "[main 1a2b3c4] x", false,
			"commit commit 1a2b3c4 on main: x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := artifactsAs(c.tool, c.input, c.output, c.failed); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A request whose only call made a quiet commit holds no result: its handoff names no commit.
func TestAQuietCommitIsNoResult(t *testing.T) {
	f := newFixture(t)
	f.owner("s-quiet", at(0), "commit the docs")
	f.tool("s-quiet", "", "Bash", at(1), `{"command":"git add -A && git commit -qm docs"}`, "", false)
	rq := f.group()[0]
	if len(rq.Results) != 0 {
		t.Fatalf("results: %s", kinds(rq.Results))
	}
	if h := Handoff(rq); strings.Contains(h, "not shown") {
		t.Fatalf("handoff:\n%s", h)
	}
}
