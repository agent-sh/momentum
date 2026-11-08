package request

// Tests from the sixth review of the requests change. Synthetic data only.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
	stamp "github.com/agent-sh/momentum/sessions/state"
)

// A call that runs gh pr view, gh pr list or gh api beside gh pr create prints addresses that are not the one it
// made, and which is which is not known: it makes no pull request.
func TestAPullRequestBesideAListingIsNotKnown(t *testing.T) {
	cases := []struct{ name, input, output string }{
		{"gh pr view", `{"command":"gh pr create --fill; gh pr view 3 --json url --jq .url"}`, "https://github.com/o/r/pull/9\nhttps://github.com/o/r/pull/3\n"},
		{"gh pr list", `{"command":"gh pr create --fill && gh pr list --json url -q '.[].url'"}`, "https://github.com/o/r/pull/9\nhttps://github.com/o/r/pull/4\n"},
		{"gh api", `{"command":"gh pr create --fill && gh api repos/o/r/pulls -q '.[0].html_url'"}`, "https://github.com/o/r/pull/9\nhttps://github.com/o/r/pull/4\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := artifactsAs("Bash", c.input, c.output, false); got != "" {
				t.Errorf("got %q, want none", got)
			}
		})
	}
}

// The store keeps a plain output with its lines flattened to spaces, where no line ends: nothing is read off it.
// It is read whole from the transcript, which keeps the lines; with the transcript gone it shows nothing, and the
// call is counted as cut.
func TestAJoinedOutputIsReadWholeOrNotAtAll(t *testing.T) {
	command := "git commit --allow-empty -m 'Trigger CI' && git push -u origin feat 2>&1 | tail -n 3 && gh pr create --fill"
	output := "[feat 1a2b3c4] Trigger CI\nTo github.com:example/x.git\n   1111111..1a2b3c4  feat -> feat\nremote: \nhttps://github.com/example/x/pull/12\n"
	call := store.Event{Source: store.Transcripts, SourceKey: "c", TS: at(1), Harness: "claude", Tool: "Bash", Kind: "tool_call", Summary: `{"command":"x"}`}
	res := store.Event{Source: store.Transcripts, SourceKey: "r", TS: at(2), Harness: "claude", Tool: "Bash", Kind: "tool_result", Summary: storedCut(output)}
	call.Summary = `{"command":` + jsonString(command) + `}`
	if got := kinds(Artifacts(call, res)); got != "" {
		t.Errorf("read off the joined text: %q", got)
	}
	want := "commit commit 1a2b3c4 on feat: Trigger CI | push pushed feat -> feat (1111111..1a2b3c4) to github.com:example/x.git | pr pull request https://github.com/example/x/pull/12"
	for _, gone := range []bool{false, true} {
		f := newFixture(t)
		path := claudeCall(t, f, t.TempDir(), "s-joined", command, output)
		if gone {
			removeFile(t, path)
		}
		rq := f.groupWith(Options{Redact: fakeRedact})[0]
		switch got := kinds(rq.Results); {
		case !gone && got != want:
			t.Errorf("read whole: %q", got)
		case gone && (got != "" || rq.CutGone != 1):
			t.Errorf("transcript gone: %q, cut and gone %d", got, rq.CutGone)
		}
	}
}

// A command the store cut where it ends may have lost an option that changes what it does: read as stored, it
// makes nothing; the commands before it do. A shell's -c script the cut ends is read the same way.
func TestACutCommandMakesNothing(t *testing.T) {
	cases := []struct{ name, input, output, want string }{
		{"a push the cut ends", `{"command":"git push origin feat…`, "To github.com:o/r.git 1111111..2222222 feat -> feat", ""},
		{"a commit the cut ends", `{"command":"git commit -am 'a long message the cut…`, "[main 1234567] a long message", ""},
		{"a commit before a cut push", `{"command":"git commit -am x && git push origin feat…`, "[main 1234567] x\n 1 file changed", "commit commit 1234567 on main: x"},
		{"a shell's -c script the cut ends", `{"command":["bash","-lc","git commit -m x && git push origin feature/very-long-bra…`,
			"[main 1234567] x\n 1 file changed", "commit commit 1234567 on main: x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := artifactsAs("Bash", c.input, c.output, false); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A session a sessions run still saw working after a follow-up (the runs that see no change write no event) is
// working: its stamp says when it was last seen in the phase its newest event entered.
func TestASessionSeenWorkingAfterAFollowUpWorks(t *testing.T) {
	f := newFixture(t)
	f.owner("s1", at(0), "fix the parser")
	f.phase("s1", at(0).Add(10*time.Second), "phase", "working", nil)
	f.owner("s1", at(3), "also add a test")
	f.put()
	st := stamp.Stamps{"s1": {Phase: sessions.Phase("working"), Since: at(0).Add(10 * time.Second), Seen: at(5)}}
	if err := f.st.SaveSessionStamps(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	rq := f.groupWith(Options{Redact: fakeRedact})[0]
	if rq.State != Working {
		t.Errorf("state %s (%s)", rq.State, rq.Why)
	}
}

// A cut call whose stored part names neither git nor gh as a word makes nothing this reads; with its record gone it
// is counted as cut, since its git may lie past the cut.
func TestACutCallWhoseRecordIsGoneIsCounted(t *testing.T) {
	for _, words := range []string{"echo high", "echo hi", "echo ghost", "echo gitlab", "git push origin main"} {
		t.Run(words, func(t *testing.T) {
			f := newFixture(t)
			f.owner("s-nogit", at(0), "check the tree")
			gone := filepath.Join(t.TempDir(), "s-nogit.jsonl")
			input := `{"command":"cd ` + longDir + ` && ` + words + ` && make test"}`
			f.cutPair("s-nogit", "claude", "Bash", "toolu_n1", at(1), input, hookText+"ok", gone+"@0#0", gone+"@100#0")
			if rq := f.groupWith(Options{Redact: fakeRedact})[0]; rq.CutGone != 1 || len(rq.Results) != 0 {
				t.Errorf("cut and gone %d, results %q", rq.CutGone, kinds(rq.Results))
			}
		})
	}
}

// A row for each rule the other tests leave untaken: each fails without its rule.
func TestEachKeptRuleHasARow(t *testing.T) {
	notes := "last run:\n[main 9f8e7d6] An older commit from another repo\n 2 files changed"
	commit := "[main 1a2b3c4] Fix\n 1 file changed"
	push := "To github.com:o/r.git\n   1111111..2222222  main -> main"
	pushed := "push pushed main -> main (1111111..2222222) to github.com:o/r.git"
	cases := []struct{ name, tool, input, output, want string }{
		// redirects: only standard output sent to a file silences a command
		{"a commit appended to a file", "Bash", `{"command":"git commit -am Fix >> log.txt; cat notes.txt"}`, notes, ""},
		{"a commit's errors sent to a file", "Bash", `{"command":"git commit -am Fix 2>err.log"}`, commit, "commit commit 1a2b3c4 on main: Fix"},
		{"a commit sent to /dev/stdout", "Bash", `{"command":"git commit -am Fix > /dev/stdout"}`, commit, "commit commit 1a2b3c4 on main: Fix"},
		// a -c script inside a command the cut ends: its last command is cut too
		{"a -c script the cut ends", "Bash", `{"command":"bash -lc 'git commit -am Fix && git push origin feat…`, commit + "\n" + push, "commit commit 1a2b3c4 on main: Fix"},
		// what a cut leaves open is closed, or read to its last whole command
		{"a subshell the cut leaves open", "Bash", `{"command":"(cd /w && git commit -am Fix && echo do…`, commit, "commit commit 1a2b3c4 on main: Fix"},
		{"a group the cut leaves open", "Bash", `{"command":"{ git commit -am Fix; echo do…`, commit, "commit commit 1a2b3c4 on main: Fix"},
		{"a loop the cut leaves open", "Bash", `{"command":"for r in a; do git -C $r commit -am Fix; echo do…`, commit, "commit commit 1a2b3c4 on main: Fix"},
		{"a condition the cut leaves open, to the last whole command", "Bash", `{"command":"git commit -am Fix && git push origin main && if tru…`,
			commit + "\n" + push, "commit commit 1a2b3c4 on main: Fix | " + pushed},
		// words and wrappers
		{"an escaped program", "Bash", `{"command":"\\git push origin main"}`, push, pushed},
		{"timeout -k", "Bash", `{"command":"timeout -k 5 30 git push origin main"}`, push, pushed},
		{"exec", "Bash", `{"command":"exec git push origin main"}`, push, pushed},
		{"exec -a", "Bash", `{"command":"exec -a name git push origin main"}`, push, pushed},
		{"time", "Bash", `{"command":"/usr/bin/time git push origin main"}`, push, pushed},
		{"time -f", "Bash", `{"command":"/usr/bin/time -f %e git push origin main"}`, push, pushed},
		{"time -o", "Bash", `{"command":"/usr/bin/time -o t.txt git push origin main"}`, push, pushed},
		{"git -c", "Bash", `{"command":"git -c user.name=x commit -am Fix"}`, commit, "commit commit 1a2b3c4 on main: Fix"},
		{"git --git-dir", "Bash", `{"command":"git --git-dir /w/.git commit -am Fix"}`, commit, "commit commit 1a2b3c4 on main: Fix"},
		// what git prints
		{"a pull's ref line beside a push a tail cut", "Bash", `{"command":"git pull origin main && git push origin main 2>&1 | tail -n 1"}`,
			"   1111111..2222222  main -> origin/main", ""},
		{"cherry-pick --no-commit", "Bash", `{"command":"git cherry-pick --no-commit 9f9f9f9"}`, "[main 1a2b3c4] Pick", ""},
		{"commit --quiet", "Bash", `{"command":"git commit --quiet -am Fix && cat notes.txt"}`, notes, ""},
		{"commit -q in a cluster", "Bash", `{"command":"git commit -qam Fix && cat notes.txt"}`, notes, ""},
		{"a To-less push's remote, origin", "Bash", `{"command":"git push 2>&1 | tail -n 1"}`, "1111111..2222222 main -> main",
			"push pushed main -> main (1111111..2222222) to origin"},
		{"a forced update", "Bash", `{"command":"git push --force-with-lease origin feat"}`, "To github.com:o/r.git\n + 1111111...2222222 feat -> feat (forced update)",
			"push pushed feat -> feat (1111111..2222222) to github.com:o/r.git"},
		// Codex's code mode keeps an output's stdout and stderr as JSON
		{"code mode's stdout", "exec", `text(await tools.exec_command({cmd:"git commit -am Fix"}))`,
			`Script completed Output: {"exit_code":0,"stdout":"[main 1a2b3c4] Fix\n 1 file changed"}`, "commit commit 1a2b3c4 on main: Fix"},
		{"code mode's stderr", "exec", `text(await tools.exec_command({cmd:"git push origin main"}))`,
			`Script completed Output: {"exit_code":0,"stderr":"To github.com:o/r.git\n   1111111..2222222  main -> main"}`, pushed},
		// the string literal reader's escapes
		{"a tab", "Bash", `{"command":"git\tpush origin main"}`, push, pushed},
		{"a carriage return", "terminal", `{"command": "git commit -am Fix"}`, `{"output": "[main 1a2b3c4] Fix\r\n 1 file changed"}`, "commit commit 1a2b3c4 on main: Fix"},
		{"a backspace", "terminal", `{"command": "git commit -am Fix"}`, `{"output": "x\n[main 1a2b3c4] a\bc"}`, "commit commit 1a2b3c4 on main: a\bc"},
		{"a form feed", "terminal", `{"command": "git commit -am Fix"}`, `{"output": "x\n[main 1a2b3c4] a\fc"}`, "commit commit 1a2b3c4 on main: a\fc"},
		{"a JavaScript \\u{...}", "exec", `text(await tools.exec_command({cmd:"\u{67}it push origin main"}))`,
			`Script completed Output: {"exit_code":0,"output":"To github.com:o/r.git\n   1111111..2222222  main -> main"}`, pushed},
		{"a JavaScript \\x", "exec", `text(await tools.exec_command({cmd:"\x67it push origin main"}))`,
			`Script completed Output: {"exit_code":0,"output":"To github.com:o/r.git\n   1111111..2222222  main -> main"}`, pushed},
		{"a surrogate pair", "terminal", `{"command": "git commit -am Fix"}`, `{"output": "x\n[main 1a2b3c4] Fix \ud83d\ude00"}`, "commit commit 1a2b3c4 on main: Fix \U0001F600"},
		{"a \\u that is no escape", "terminal", `{"command": "git commit -am Fix"}`, `{"output": "x\n[main 1a2b3c4] a\uZZb"}`, `commit commit 1a2b3c4 on main: a\uZZb`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := artifactsAs(c.tool, c.input, c.output, false); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
