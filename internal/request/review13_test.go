package request

// Tests from the thirteenth review of the requests change. Synthetic data only.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// A command the store keeps as JSON, git or gh right after an escaped newline: the word is read off the decoded
// command, so the call is read, for every harness. A harness whose records this cannot read whole says its details
// were not read.
func TestAGitWordAfterAnEscapedNewlineIsRead(t *testing.T) {
	commit := "[main 1a2b3c4] Fix\n 1 file changed\n"
	commitWant := "commit commit 1a2b3c4 on main: Fix"
	pr := "https://github.com/o/r/pull/9\n"
	prWant := "pr pull request https://github.com/o/r/pull/9"
	for _, c := range []struct {
		name, harness, tool, input, output, want string
		unread                                   int
	}{
		{"claude", "claude", "Bash", `{"command":"cd /work/x\ngit commit -m Fix"}`, commit, commitWant, 0},
		{"claude heredoc and gh", "claude", "Bash", `{"command":"cat > body.md <<'EOF'\nSummary\nEOF\ngh pr create --title T --body-file body.md"}`, pr, prWant, 0},
		{"pi", "pi", "bash", `{"command":"cd /work/x\ngit commit -m Fix"}`, commit, commitWant, 0},
		{"codex exec_command", "codex", "exec_command", `{"cmd":"cd /work/x\ngit commit -m Fix"}`,
			"Chunk ID: a\nWall time: 0.1 seconds\nProcess exited with code 0\nOutput:\n" + commit, commitWant, 0},
		{"codex code mode", "codex", "exec", `const r = await tools.exec_command({cmd:"cd /work/x\ngit commit -m Fix"}); text(r)`,
			`Script completed Wall time 0.2 seconds Output: {"chunk_id":"a","exit_code":0,"output":"[main 1a2b3c4] Fix\n 1 file changed\n"}`, commitWant, 0},
		{"hermes, its output cut", "hermes", "terminal", `{"command": "cd /work/x\ngit commit -m Fix"}`,
			`{"output": "[main 1a2b3c4] Fix\n 1 file changed, 2 insert…`, commitWant, 1},
		{"opencode", "opencode", "bash", `{"command":"cd /work/x\ngit commit -m Fix"}`, commit, "", 1},
		{"kimi", "kimi", "Bash", `{"command":"cd /work/x\ngit commit -m Fix"}`, commit, "", 1},
		{"grok", "grok", "run_terminal_command", `{"command":"cd /work/x\ngit commit -m Fix"}`, commit, "", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			s := "019a0000-0000-7000-8000-00000000d001"
			if c.harness == "hermes" {
				s = "20261108_100000_nlgit"
			}
			f.owner(s, at(0), "fix the parser")
			f.pairWithID(s, "", c.harness, c.tool, "call_n1", at(1), c.input, c.output)
			rq := f.groupWith(Options{Redact: fakeRedact})[0]
			if got := kinds(rq.Results); got != c.want || rq.CutUnread+rq.CutGone != c.unread {
				t.Errorf("got %q, details not read %d", got, rq.CutUnread+rq.CutGone)
			}
		})
	}
}

// A pasted handoff followed by his own words: the handoff ends at its closing line, and his words are what came
// after it, in the text as typed and as the store keeps it, its lines joined.
func TestHisWordsAfterAPastedHandoff(t *testing.T) {
	for _, joined := range []bool{false, true} {
		f := newFixture(t)
		f.owner("s-src", at(0), "tune the cache")
		f.reply("s-src", at(1), "Tuned.")
		src := f.group()[0]
		text := Handoff(src) + "now do the env overrides"
		if joined {
			text = strings.Join(strings.Fields(text), " ")
		}
		f.owner("s-next", at(10), text)
		reqs := f.group()
		if len(reqs) != 1 {
			t.Fatalf("joined %v: %d requests", joined, len(reqs))
		}
		if a := reqs[0].Asks[len(reqs[0].Asks)-1]; a.Handoff != src.ID || a.Words != "now do the env overrides" {
			t.Errorf("joined %v: his words %q", joined, a.Words)
		}
	}
}

// One id line of the handoff's in an uncut text: the opening line alone is the paste, so what follows it is his;
// a last line at the end of the text ends the paste there, so what comes before it is his.
func TestASingleIDLineInAnUncutText(t *testing.T) {
	for _, c := range []struct{ name, text, want string }{
		{"the opening line", "%s - a handoff of the owner's request\nand fix the flaky test", "and fix the flaky test"},
		{"the last line", "carry on with it %s", "carry on with it"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.owner("s-src", at(0), "tune the cache")
			src := f.group()[0]
			f.owner("s-next", at(10), strings.Replace(c.text, "%s", Line(src.ID), 1))
			if a := f.group()[0].Asks; len(a) != 2 || a[1].Words != c.want {
				t.Errorf("asks %q", words(a))
			}
		})
	}
}

// A turn in a joined thread from before the request began (a paste whose clock is behind the opening's) is no ask
// of the request's.
func TestATurnBeforeTheRequestBeganIsNoAsk(t *testing.T) {
	f := newFixture(t)
	f.owner("s-src", at(10), "tune the cache")
	src := f.group()[0]
	f.owner("s-next", at(5), Line(src.ID)+" - a handoff of the owner's request Asked: tune the cache "+Line(src.ID)+" and the docs")
	if reqs := f.group(); len(reqs) != 1 || len(reqs[0].Asks) != 1 {
		t.Errorf("asks %q", words(reqs[0].Asks))
	}
}

// A printed output text that holds a result's own object (a command that prints JSON with an output key) is not
// read as that command's text: which line is whose is not known.
func TestAPrintedTextHoldingAResultIsNotPlain(t *testing.T) {
	f := newFixture(t)
	s := "019a0000-0000-7000-8000-00000000d002"
	f.owner(s, at(0), "commit the fix")
	script := "const r = await tools.exec_command({cmd:\"git commit -m Fix && cat events.json\"});\ntext(r.output);\n"
	out := "Script completed\nWall time 0.4 seconds\nOutput:\n\n[main 1a2b3c4] Fix\n" + strings.Repeat(" a.go | 2 +-\n", 20) +
		"{\"id\": 7, \"output\": \"[main 9f9f9f9] an old commit\"}\n"
	f.pairWithID(s, "", "codex", "exec", "call_p1", at(1), script, out)
	if got := kinds(f.group()[0].Results); got != "" {
		t.Errorf("got %q", got)
	}
}

// A key another printed object holds (another tool's structuredContent id) is not what the script printed with a
// result: the result is paired by its print.
func TestAnotherObjectsIDIsNotCarried(t *testing.T) {
	f := newFixture(t)
	s := "019a0000-0000-7000-8000-00000000d003"
	f.owner(s, at(0), "open the pull request")
	script := `const r = await tools.exec_command({cmd:"gh pr create --fill"}); const st = await tools.mcp__ci__status({}); text(JSON.stringify(st.structuredContent)); text(r)`
	out := `Script completed Wall time 0.4 seconds Output: {"id": 7, "ok": true} {"chunk_id":"a","exit_code":0,"output":"https://github.com/o/r/pull/9\n"}`
	f.pairWithID(s, "", "codex", "exec", "call_s1", at(1), script, out)
	if got := kinds(f.group()[0].Results); got != "pr pull request https://github.com/o/r/pull/9" {
		t.Errorf("got %q", got)
	}
}

// A gh command inside an assignment's command substitution keeps its output in the variable: it prints no address,
// so the pull request the call created counts.
func TestAnAssignedSubstitutionPrintsNothing(t *testing.T) {
	for _, c := range []struct{ name, command, want string }{
		{"assigned", `T=$(gh issue view 12 --json title -q .title) && gh pr create --title "$T" --fill`, "pr pull request https://github.com/o/r/pull/9"},
		{"exported", `export T=$(gh issue view 12 --json title -q .title) && gh pr create --title "$T" --fill`, "pr pull request https://github.com/o/r/pull/9"},
		{"printed", `gh issue view 12 && gh pr create --fill`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.owner("s-sub", at(0), "open the pull request")
			f.tool("s-sub", "", "Bash", at(1), `{"command":`+jsonString(c.command)+`}`, "https://github.com/o/r/pull/9\n", false)
			if got := kinds(f.groupWith(Options{Redact: fakeRedact})[0].Results); got != c.want {
				t.Errorf("got %q", got)
			}
		})
	}
}

// The handoff lists the commits, pushes and pull requests before the files: many newer writes do not push a commit
// out of the shown.
func TestTheHandoffListsCommitsBeforeFiles(t *testing.T) {
	f := newFixture(t)
	f.owner("s-h", at(0), "fix the parser")
	f.tool("s-h", "", "Bash", at(1), `{"command":"git commit -am Fix"}`, "[main 1a2b3c4] Fix\n 1 file changed", false)
	for i := range artifacts + 1 {
		f.tool("s-h", "", "Write", at(2+i), fmt.Sprintf(`{"file_path":"/work/x/f%d.go","content":"package x"}`, i), "File created successfully", false)
	}
	h := Handoff(f.group()[0])
	if !strings.Contains(h, "commit 1a2b3c4 on main: Fix") || !strings.Contains(h, "(2 more)") {
		t.Errorf("handoff:\n%s", h)
	}
}

// A Codex command still running shows its commit lines only: its ref lines count once a poll shows it exited 0,
// read with what it printed while it ran; a poll that shows it failed counts none.
func TestARunningCommandsRefLinesWaitForItsExit(t *testing.T) {
	running := "Chunk ID: a\nWall time: 1.0 seconds\nProcess running with session ID 42\nOutput:\nTo github.com:o/r.git\n   1111111..2222222  feat -> feat\n"
	pushed := "push pushed feat -> feat (1111111..2222222) to github.com:o/r.git"
	for _, c := range []struct {
		name, poll, want string
	}{
		{"no poll", "", ""},
		{"a poll that shows it still runs", "Chunk ID: b\nWall time: 3.0 seconds\nProcess running with session ID 42\nOutput:\n", ""},
		{"a poll that shows it exited 0", "Chunk ID: b\nWall time: 3.0 seconds\nProcess exited with code 0\nOutput:\n", pushed},
		{"a poll that shows it failed", "Chunk ID: b\nWall time: 3.0 seconds\nProcess exited with code 1\nOutput:\nerror: hook declined\n", ""},
		{"ref lines a poll showed while it still ran", "", pushed},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			s := "019a0000-0000-7000-8000-00000000d004"
			f.owner(s, at(0), "push the branch")
			if strings.HasPrefix(c.name, "ref lines a poll showed") {
				f.pairWithID(s, "", "codex", "exec_command", "call_r1", at(1), `{"cmd":"git push origin feat","yield_time_ms":1000}`,
					"Chunk ID: a\nWall time: 1.0 seconds\nProcess running with session ID 42\nOutput:\n")
				f.pairWithID(s, "", "codex", "write_stdin", "call_r2", at(2), `{"session_id":42,"chars":""}`, strings.Replace(running, "Chunk ID: a", "Chunk ID: b", 1))
				f.pairWithID(s, "", "codex", "write_stdin", "call_r3", at(3), `{"session_id":42,"chars":""}`,
					"Chunk ID: c\nWall time: 3.0 seconds\nProcess exited with code 0\nOutput:\n")
			} else {
				f.pairWithID(s, "", "codex", "exec_command", "call_r1", at(1), `{"cmd":"git push origin feat","yield_time_ms":1000}`, running)
			}
			if c.poll != "" {
				f.pairWithID(s, "", "codex", "write_stdin", "call_r2", at(2), `{"session_id":42,"chars":""}`, c.poll)
			}
			if got := kinds(f.group()[0].Results); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	// a code-mode script's command still running at its end
	f := newFixture(t)
	s := "019a0000-0000-7000-8000-00000000d005"
	f.owner(s, at(0), "open the pull request")
	f.pairWithID(s, "", "codex", "exec", "call_r3", at(1), `const r = await tools.exec_command({cmd:"gh pr create --fill", yield_time_ms: 1000}); text(r)`,
		`Script completed Wall time 1.0 seconds Output: {"chunk_id":"a","session_id":43,"output":"https://github.com/o/r/pull/9\n"}`)
	if got := kinds(f.group()[0].Results); got != "" {
		t.Errorf("code mode, still running: %q", got)
	}
	f.pairWithID(s, "", "codex", "write_stdin", "call_r4", at(2), `{"session_id":43,"chars":""}`,
		"Chunk ID: c\nWall time: 2.0 seconds\nProcess exited with code 0\nOutput:\n")
	if got := kinds(f.group()[0].Results); got != "pr pull request https://github.com/o/r/pull/9" {
		t.Errorf("code mode, after its exit: %q", got)
	}
}

// Codex's write_stdin tool names its session in its JSON's session_id: the same words inside a value it sends are
// not its session.
func TestAPollsSessionIsItsJSONs(t *testing.T) {
	f := newFixture(t)
	s := "019a0000-0000-7000-8000-00000000d006"
	f.owner(s, at(0), "open the pull request")
	f.pairWithID(s, "", "codex", "exec_command", "call_j1", at(1), `{"cmd":"gh pr create --fill","yield_time_ms":1000}`,
		"Chunk ID: a\nWall time: 1.0 seconds\nProcess running with session ID 42\nOutput:\n")
	f.pairWithID(s, "", "codex", "write_stdin", "call_j2", at(2), `{"chars":"session_id: 5\n","session_id":42}`,
		"Chunk ID: b\nWall time: 3.0 seconds\nProcess exited with code 0\nOutput:\nhttps://github.com/o/r/pull/7\n")
	if got := kinds(f.group()[0].Results); got != "pr pull request https://github.com/o/r/pull/7" {
		t.Errorf("got %q", got)
	}
}

// A writer the store cut whose input names its path whole is read as stored: a patch header inside its content is
// no patch of its own, so nothing past the cut is wanted from its transcript.
func TestAPatchHeaderInAWritersContentIsNoPatch(t *testing.T) {
	f := newFixture(t)
	gone := filepath.Join(t.TempDir(), "s-w.jsonl")
	content := "notes on the format: *** Update File: b.go and then a long paragraph " + strings.Repeat("of words ", 30)
	f.cutPair("s-w", "claude", "Write", "toolu_w1", at(1), `{"file_path":"/work/a.md","content":`+jsonString(content)+`}`, "File created successfully",
		gone+"@0#0", gone+"@100#0")
	f.owner("s-w", at(0), "write the notes")
	rq := f.groupWith(Options{Redact: fakeRedact})[0]
	if got := kinds(rq.Results); got != "file /work/a.md" || rq.CutGone != 0 {
		t.Errorf("got %q, cut and gone %d", got, rq.CutGone)
	}
}

// A code-mode script whose command's text names write_stdin polls nothing: its output the store cut is not wanted
// from its transcript when nothing in it makes anything (the store's read takes it for the gh inside "high").
func TestACommandNamingWriteStdinIsNoPoll(t *testing.T) {
	f := newFixture(t)
	s := "019a0000-0000-7000-8000-00000000d007"
	f.owner(s, at(0), "find the polls")
	gone := filepath.Join(t.TempDir(), "rollout-gone.jsonl")
	f.cutPair(s, "codex", "exec", "call_w1", at(1), `text(await tools.exec_command({cmd:"grep -n 'write_stdin(' high-level.md"}))`,
		`Script completed Wall time 0.2 seconds Output: {"chunk_id":"a","exit_code":0,"output":"`+strings.Repeat("notes.md:1: write_stdin( call ", 12), gone+"@0#0", gone+"@100#0")
	if rq := f.groupWith(Options{Redact: fakeRedact})[0]; rq.CutGone != 0 {
		t.Errorf("cut and gone %d", rq.CutGone)
	}
}
