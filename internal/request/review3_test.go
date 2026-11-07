package request

// Tests from the third review of the requests change: what a call's success proves it made. Synthetic data only.

import (
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// artifactsAs is Artifacts on a call of tool and its result, failed or not.
func artifactsAs(tool, input, output string, failed bool) string {
	call := store.Event{Source: store.Transcripts, SourceKey: "c", TS: at(1), Tool: tool, Kind: "tool_call", Summary: input}
	res := store.Event{Source: store.Transcripts, SourceKey: "r", TS: at(1).Add(time.Second), Tool: tool, Kind: "tool_result", Summary: output}
	if failed {
		res.Meta = `{"is_error":true}`
	}
	return kinds(Artifacts(call, res))
}

// A commit, push or pull request a successful call's command made is observed done work, whatever its output shows:
// when the whole call's success proves that command succeeded. A && chain proves every command of it, a list or a
// pipeline only its last, || and what runs in the background, a condition or a cut command nothing.
func TestWhatACallsSuccessProvesItMade(t *testing.T) {
	commit, push, pr := "commit commit (hash not shown)", "push push (refs not shown) to origin", "pr pull request (address not shown)"
	cases := []struct {
		name, tool, input, output string
		failed                    bool
		want                      string
	}{
		{"a quiet commit, plain", "Bash", `{"command":"git commit -qm x"}`, "", false, commit},
		{"a quiet push, plain", "Bash", `{"command":"git push -q origin main"}`, "", false, push},
		{"in a && chain", "Bash", `{"command":"cd /work/x && git add -A && git commit -qm x && git push -q origin main"}`, "", false,
			commit + " in /work/x | " + push},
		{"in a ; list, the last proven", "Bash", `{"command":"git commit -qm x; git push -q origin main"}`, "", false, push},
		{"in a ; list, not last", "Bash", `{"command":"git commit -qm x; echo done"}`, "done", false, ""},
		{"after ||", "Bash", `{"command":"git push -q origin main || git commit -qm x"}`, "", false, ""},
		{"|| then &&", "Bash", `{"command":"false || true && git commit -qm x"}`, "", false, commit},
		{"a failed result", "Bash", `{"command":"git commit -qm x && git push -q origin main"}`, "", true, ""},
		{"a pipeline's first command", "Bash", `{"command":"git commit -qm x | cat"}`, "", false, ""},
		{"in the background", "Bash", `{"command":"git push -q origin main &"}`, "", false, ""},
		{"in a condition", "Bash", `{"command":"if true; then git commit -qm x; fi"}`, "", false, ""},
		{"a cut command", "Bash", `{"command":"git commit -qm 'a long message the store cut…`, "", false, ""},
		{"in a subshell, last", "Bash", `{"command":"(cd /work/z && git commit -qm x)"}`, "", false, commit + " in /work/z"},
		{"git -C", "Bash", `{"command":"git -C /work/y commit -qm x"}`, "", false, commit + " in /work/y"},
		{"Codex's workdir", "exec_command", `{"cmd":"git commit -qm x","workdir":"/work/w"}`, "Process exited with code 0", false, commit + " in /work/w"},
		{"a shell's -c script", "Bash", `{"command":"bash -lc 'git push -q origin main'"}`, "", false, push},
		{"a line that shows it supersedes", "Bash", `{"command":"git commit -m x && git push origin main"}`, "[main 1a2b3c4] x", false,
			"commit commit 1a2b3c4 on main: x | " + push},
		{"an address kept in a variable", "Bash", `{"command":"N=$(gh pr create --fill) && echo ok"}`, "ok", false, pr},
		{"an address kept in a variable, in a ; list", "Bash", `{"command":"N=$(gh pr create --fill); echo made"}`, "made", false, ""},
		{"a pull request that already exists", "Bash", `{"command":"gh pr create --fill"}`,
			`a pull request for branch "topic" into branch "main" already exists: https://github.com/example/x/pull/3`, false, ""},
		{"a dry run", "Bash", `{"command":"gh pr create --dry-run --fill && git push -q --dry-run"}`, "", false, ""},
		{"everything up to date", "Bash", `{"command":"git push origin main"}`, "Everything up-to-date", false, ""},
		{"a merge already up to date", "Bash", `{"command":"git merge -q topic"}`, "Already up to date.", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := artifactsAs(c.tool, c.input, c.output, c.failed); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// The handoff shows a commit the call made but did not show as made, details not shown, in the session's directory.
func TestAHandoffShowsWhatWasMadeButNotShown(t *testing.T) {
	f := newFixture(t)
	f.owner("s-quiet", at(0), "commit the docs")
	f.tool("s-quiet", "", "Bash", at(1), `{"command":"git add -A && git commit -qm docs"}`, "", false)
	rq := f.group()[0]
	if n := rq.Newest(); n == nil || n.Kind != KindCommit || !strings.Contains(n.Text, "hash not shown") {
		t.Fatalf("newest: %+v", n)
	}
	if h := Handoff(rq); !strings.Contains(h, "commit (hash not shown) in /work/s-quiet: made, details not shown") {
		t.Fatalf("handoff:\n%s", h)
	}
}
