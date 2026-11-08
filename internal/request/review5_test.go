package request

// Tests from the fifth review of the requests change. Synthetic data only.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/agent-sh/momentum/internal/store"
)

// writerAs is Artifacts on a writer's call of harness, as the store keeps its input.
func writerAs(harness, tool, input string) string {
	call := store.Event{Source: store.Transcripts, SourceKey: "c", TS: at(1), Harness: harness, Tool: tool, Kind: "tool_call", Summary: input, Meta: `{"tool_id":"call_w"}`}
	res := store.Event{Source: store.Transcripts, SourceKey: "r", TS: at(1).Add(time.Second), Harness: harness, Tool: tool, Kind: "tool_result", Summary: `{"success": true}`}
	return kinds(Artifacts(call, res))
}

// A value the store's cut touches is not known: it is dropped, never read shorter. For each artifact, the cut
// inside it, at its end (it may have gone on), after it, and no cut.
func TestWhatTheCutTouchesIsDropped(t *testing.T) {
	commit := "commit commit 1a2b3c4 on main: Fix the parser"
	push := "push pushed main -> main (1111111..2222222) to github.com:example/x.git"
	pr := "pr pull request https://github.com/example/x/pull/12"
	shells := []struct{ name, input, output, want string }{
		{"a commit's hash, cut inside", `{"command":"git commit -am 'Fix the parser'"}`, "[main 1a2b…", ""},
		{"a commit's hash, cut at its end", `{"command":"git commit -am 'Fix the parser'"}`, "[main 1a2b3c4…", ""},
		{"a commit's subject, cut inside", `{"command":"git commit -am 'Fix the parser'"}`, "[main 1a2b3c4] Fix the pa…", "commit commit 1a2b3c4 on main"},
		{"a commit's subject, cut at its end", `{"command":"git commit -am 'Fix the parser'"}`, "[main 1a2b3c4] Fix the parser…", "commit commit 1a2b3c4 on main"},
		{"a commit line, cut after it", `{"command":"git commit -am 'Fix the parser'"}`, "[main 1a2b3c4] Fix the parser 1 file changed, 2 insertions…", commit},
		{"a commit line, no cut", `{"command":"git commit -am 'Fix the parser'"}`, "[main 1a2b3c4] Fix the parser 1 file changed", commit},
		{"a ref line, cut inside", `{"command":"git push origin main"}`, "To github.com:example/x.git 1111111..2222222 main -> ma…", ""},
		{"a ref line, cut at its end", `{"command":"git push origin main"}`, "To github.com:example/x.git 1111111..2222222 main -> main…", ""},
		{"a ref line, cut after it", `{"command":"git push origin main"}`, "To github.com:example/x.git 1111111..2222222 main -> main …", push},
		{"a ref line, no cut", `{"command":"git push origin main"}`, "To github.com:example/x.git 1111111..2222222 main -> main", push},
		{"a branch, cut inside", `{"command":"git push origin feature/parser"}`, "To github.com:example/x.git 1111111..2222222 feature/parser -> feature/pa…", ""},
		{"an address, cut inside", `{"command":"gh pr create --fill"}`, "Creating pull request for fix into main in example/x https://github.com/example/x/pull/1…", ""},
		{"an address, cut at its end", `{"command":"gh pr create --fill"}`, "Creating pull request for fix into main in example/x https://github.com/example/x/pull/12…", ""},
		{"an address, cut after it", `{"command":"gh pr create --fill"}`, "Creating pull request for fix into main in example/x https://github.com/example/x/pull/12 …", pr},
		{"an address, no cut", `{"command":"gh pr create --fill"}`, "Creating pull request for fix into main in example/x https://github.com/example/x/pull/12", pr},
		// Hermes keeps an output as JSON: a value the cut ends loses its last token the same way
		{"a JSON output's address, cut at its end", "", `{"output": "https://github.com/example/x/pull/12…`, ""},
		{"a JSON output's address, cut after it", "", `{"output": "https://github.com/example/x/pull/12\nWarning: …`, pr},
	}
	for _, c := range shells {
		t.Run(c.name, func(t *testing.T) {
			tool, input := "Bash", c.input
			if input == "" {
				tool, input = "terminal", `{"command": "gh pr create --fill"}`
			}
			if got := artifactsAs(tool, input, c.output, false); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	writes := []struct{ name, harness, tool, input, want string }{
		{"a path, cut inside", "hermes", "patch", `{"old_string": "a", "new_string": "b", "path": "/work/a.g…`, ""},
		{"a path, cut at its end", "hermes", "patch", `{"old_string": "a", "new_string": "b", "path": "/work/a.go…`, ""},
		{"a path, cut after it", "hermes", "patch", `{"path": "/work/a.go", "old_string": "a", "new_st…`, "file /work/a.go"},
		{"a path, no cut", "hermes", "patch", `{"path": "/work/a.go", "old_string": "a", "new_string": "b"}`, "file /work/a.go"},
		{"a patch's file, cut inside", "opencode", "patch", `{"patchText":"*** Begin Patch\n*** Update File: /work/a.g…`, ""},
		{"a patch's file, cut at its end", "opencode", "patch", `{"patchText":"*** Begin Patch\n*** Update File: /work/a.go…`, ""},
		{"a patch's file, cut after it", "opencode", "patch", `{"patchText":"*** Begin Patch\n*** Update File: /work/a.go\n@@\n-a…`, "file /work/a.go"},
		{"a patch's file, no cut", "opencode", "patch", `{"patchText":"*** Begin Patch\n*** Update File: /work/a.go\n@@\n-a\n+b\n*** End Patch"}`, "file /work/a.go"},
		{"a raw patch's file, cut inside", "codex", "apply_patch", "*** Begin Patch *** Update File: src/a.g…", ""},
		{"a raw patch's file, cut after it", "codex", "apply_patch", "*** Begin Patch *** Update File: src/a.go @@ -a…", "file src/a.go"},
	}
	for _, c := range writes {
		t.Run(c.name, func(t *testing.T) {
			if got := writerAs(c.harness, c.tool, c.input); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A writer's path the store's cut ends sends the call to the whole read; with its transcript gone, the handoff
// shows no path and counts the call as cut.
func TestAPathTheCutEndsIsReadWholeOrNotAtAll(t *testing.T) {
	in := `{"new_string": "` + strings.Repeat("x", 150) + `", "old_string": "` + strings.Repeat("y", 40) + `", "path": "/work/project/src/parser.go"}`
	stored := in[:len(in)-18] + "…"
	f := newFixture(t)
	s := "20261107_100000_hpath"
	f.owner(s, at(0), "fix the parser")
	f.evs[len(f.evs)-1].Harness = "hermes"
	gone := filepath.Join(t.TempDir(), "state.db")
	f.cutPair(s, "hermes", "patch", "call_w", at(1), stored, `{"success": true}`, gone+"@1#0", gone+"@2#0")
	f.evs[len(f.evs)-2].Summary = stored
	rq := f.groupWith(Options{Redact: fakeRedact})[0]
	if got := kinds(rq.Results); got != "" || rq.CutGone != 1 {
		t.Fatalf("results %q, cut and gone %d", got, rq.CutGone)
	}
	if h := Handoff(rq); strings.Contains(h, "/work/proje") || !strings.Contains(h, "details cut, transcript gone: 1 of its calls") {
		t.Fatalf("handoff:\n%s", h)
	}
}

// A pull request is the address gh printed on a line of its own: the last one, outside the commit lines, and not
// one it says already exists, nor one another command printed as a field's value.
func TestThePullRequestIsTheAddressGhPrinted(t *testing.T) {
	body := strings.Repeat("A body long enough to put the next address far from the refusal. ", 5)
	cases := []struct{ name, input, output, want string }{
		{"a commit subject names another", `{"command":"git commit -am 'Port the fix from https://github.com/up/r/pull/123' && git push -u origin fix && gh pr create --fill"}`,
			"[fix 1a2b3c4] Port the fix from https://github.com/up/r/pull/123\n 1 file changed\nTo github.com:me/r.git\n * [new branch]      fix -> fix\nbranch 'fix' set up to track 'origin/fix'.\nhttps://github.com/me/r/pull/7",
			"pull request https://github.com/me/r/pull/7"},
		{"a commit subject names another, the lines joined", `{"command":"git commit -am 'Port the fix from https://github.com/up/r/pull/123' && git push -u origin fix && gh pr create --fill"}`,
			"[fix 1a2b3c4] Port the fix from https://github.com/up/r/pull/123 1 file changed To github.com:me/r.git * [new branch] fix -> fix branch 'fix' set up to track 'origin/fix'. https://github.com/me/r/pull/7",
			"pull request https://github.com/me/r/pull/7"},
		{"an existing one, then gh pr view", `{"command":"gh pr create --fill || gh pr view"}`,
			"a pull request for branch \"fix\" into branch \"main\" already exists:\nhttps://github.com/me/r/pull/7\ntitle:\tFix the parser\n--\n" + body + "\nurl:\thttps://github.com/me/r/pull/7\n", ""},
		{"an existing one, then gh pr view, the lines joined", `{"command":"gh pr create --fill || gh pr view"}`,
			"a pull request for branch \"fix\" into branch \"main\" already exists: https://github.com/me/r/pull/7 title: Fix the parser -- " + body + " url: https://github.com/me/r/pull/7", ""},
		{"two made in a loop, the last", `{"command":"for r in a b; do gh pr create -R me/$r --fill; done"}`,
			"https://github.com/me/a/pull/3\nhttps://github.com/me/b/pull/4\n", "pull request https://github.com/me/b/pull/4"},
		{"an address inside a longer one", `{"command":"gh pr create --fill"}`, "see https://github.com/me/r/pull/7/files", ""},
		{"only a commit subject's, the lines joined", `{"command":"git commit -am 'Port https://github.com/up/r/pull/123' && gh pr create --fill"}`,
			"[fix 1a2b3c4] Port https://github.com/up/r/pull/123 1 file changed", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var prs []string
			call := store.Event{Source: store.Transcripts, SourceKey: "c", TS: at(1), Harness: "claude", Tool: "Bash", Kind: "tool_call", Summary: c.input}
			res := store.Event{Source: store.Transcripts, SourceKey: "r", TS: at(1).Add(time.Second), Harness: "claude", Tool: "Bash", Kind: "tool_result", Summary: c.output}
			for _, x := range Artifacts(call, res) {
				if x.Kind == KindPR {
					prs = append(prs, x.Text)
				}
			}
			if got := strings.Join(prs, " | "); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// Codex's apply_patch keeps the raw patch as its input: only its file headers name files, not a path key inside
// a patched line.
func TestARawPatchNamesOnlyItsHeaders(t *testing.T) {
	in := storedCut("*** Begin Patch\n*** Update File: src/router.js\n@@\n-  { path: '/old', component: Old },\n+  { path: '/home', component: Home },\n*** End Patch")
	if got := writerAs("codex", "apply_patch", in); got != "file src/router.js" {
		t.Errorf("got %q, want the patched file", got)
	}
}

// A fork or a resume copies its parent's context with the original times: each event belongs to the segment its
// own time falls in, so what it did after a pasted handoff is the pasted request's.
func TestAForksWorkAfterAHandoffIsThePastedRequests(t *testing.T) {
	f := newFixture(t)
	s := "e1e1e1e1-0000-4000-8000-000000000001"
	other := ID(at(-100), "the other ask")
	f.owner("s-other", at(-100), "the other ask")
	f.owner(s, at(0), "fix the parser")
	f.reply(s, at(1), "Fixed.")
	f.owner(s, at(5), "momentum request "+other+" - a handoff of the owner's request\nmomentum request "+other)
	f.event(s, "afork0000000000001", "agent_text", store.ActorAgent, at(2), "context the fork copied", nil)
	f.tool(s, "afork0000000000001", "Bash", at(8), `{"command":"git commit -am 'Port it'"}`, "[main 7a7a7a7] Port it\n 1 file changed", false)
	for _, rq := range f.group() {
		has := strings.Contains(kinds(rq.Results), "7a7a7a7")
		if rq.ID == other && !has || rq.ID != other && has {
			t.Errorf("request %s (pasted %v) holds %s", rq.ID, rq.ID == other, kinds(rq.Results))
		}
	}
}

// The same for a main-thread commit after the handoff that a fork copied with its time, whichever copy the store's
// order pairs first.
func TestAForkCopyOfACommitAfterAHandoffIsThePastedRequests(t *testing.T) {
	f := newFixture(t)
	s := "e1e1e1e1-0000-4000-8000-000000000002"
	other := ID(at(-100), "the other ask")
	f.owner("s-other", at(-100), "the other ask")
	f.owner(s, at(0), "fix the parser")
	f.owner(s, at(5), "momentum request "+other+" - a handoff of the owner's request\nmomentum request "+other)
	key := func(who string, n int) string { return fmt.Sprintf("%s@%012d#000", who, n) }
	add := func(agent, kind string, ts time.Time, sum, sk string) {
		f.event(s, agent, kind, store.ActorAgent, ts, sum, map[string]any{"tool_id": "toolu_same"})
		f.evs[len(f.evs)-1].Tool, f.evs[len(f.evs)-1].SourceKey = "Bash", sk
	}
	in, out := `{"command":"git commit -am 'Port it'"}`, "[main 7b7b7b7] Port it\n 1 file changed"
	add("", "tool_call", at(6), in, key(s, 600))
	add("", "tool_result", at(6).Add(time.Second), out, key(s, 601))
	f.event(s, "afork0000000000002", "agent_text", store.ActorAgent, at(2), "context the fork copied", nil)
	f.evs[len(f.evs)-1].SourceKey = key("agent-afork0000000000002", 0)
	add("afork0000000000002", "tool_call", at(6), in, key("agent-afork0000000000002", 10))
	add("afork0000000000002", "tool_result", at(6).Add(time.Second), out, key("agent-afork0000000000002", 11))
	for _, rq := range f.group() {
		has := strings.Contains(kinds(rq.Results), "7b7b7b7")
		if rq.ID == other && !has || rq.ID != other && has {
			t.Errorf("request %s (pasted %v) holds %s", rq.ID, rq.ID == other, kinds(rq.Results))
		}
	}
}

// A commit line counts only where the git command that makes it prints it: a quiet one, or one whose output goes
// to a file, printed none, so a commit line beside it is another command's (a cat of a file).
func TestACommitLineIsGitsOwn(t *testing.T) {
	line := "last run:\n[main 9f8e7d6] An older commit from another repo\n 2 files changed"
	cases := []struct{ name, input, want string }{
		{"a quiet commit and a cat", `{"command":"git commit -q -am 'Fix' && cat notes/last-run.txt"}`, ""},
		{"a commit to /dev/null and a cat", `{"command":"git commit -am 'Fix' > /dev/null && cat notes/last-run.txt"}`, ""},
		{"a commit in a group sent to a file", `{"command":"{ git add -A; git commit -am 'Fix'; } &> log.txt; cat notes/last-run.txt"}`, ""},
		{"a quiet commit in a shell's -c script", `{"command":"bash -c 'git commit -q -am Fix' && cat notes/last-run.txt"}`, ""},
		{"a shell's -c script sent to /dev/null", `{"command":"bash -c 'git commit -am Fix' > /dev/null; cat notes/last-run.txt"}`, ""},
		{"a commit through a pipe", `{"command":"git commit -am 'Fix' 2>&1 | tail -n 3"}`, "commit commit 9f8e7d6 on main: An older commit from another repo"},
		// the limit: a commit that prints beside a command that prints text cannot be told apart from it
		{"a commit that prints, and a cat", `{"command":"git commit -am 'Fix' && cat notes/last-run.txt"}`, "commit commit 9f8e7d6 on main: An older commit from another repo"},
		// --quit and --abort end a merge under way: no commit
		{"git merge --quit", `{"command":"git merge --quit"}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := artifactsAs("Bash", c.input, line, false); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// One record longer than the reader's limit loses only itself: the cut calls after it in the same transcript are
// still read.
func TestALongRecordLosesOnlyItself(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprintf("compressed %v", compressed), func(t *testing.T) {
			f := newFixture(t)
			session := "s-long-record"
			path := filepath.Join(t.TempDir(), session+".jsonl")
			big := strings.Repeat("x", recordMax+(1<<20))
			in1 := `{"command":"cd ` + longDir + ` && git commit -am wip && cat big.log"}`
			in2 := `{"command":` + jsonString("cd "+longDir+" && git commit -m 'Fix the parser'") + `}`
			output := hookText + "[main 1a2b3c4] Fix the parser\n 1 file changed"
			offs := writeLines(t, path,
				`{"type":"user","message":{"role":"user","content":"fix the parser"}}`,
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_l1","name":"Bash","input":`+in1+`}]}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_l1","content":`+jsonString(big)+`}]}}`,
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_l2","name":"Bash","input":`+in2+`}]}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_l2","content":`+jsonString(output)+`}]}}`)
			if compressed {
				compress(t, path)
			}
			f.owner(session, at(0), "fix the parser")
			f.cutPair(session, "claude", "Bash", "toolu_l1", at(1), in1, big, fmt.Sprintf("%s@%d#0", path, offs[1]), fmt.Sprintf("%s@%d#0", path, offs[2]))
			f.cutPair(session, "claude", "Bash", "toolu_l2", at(2), in2, output, fmt.Sprintf("%s@%d#0", path, offs[3]), fmt.Sprintf("%s@%d#0", path, offs[4]))
			rq := f.groupWith(Options{Redact: fakeRedact})[0]
			if !strings.Contains(kinds(rq.Results), "1a2b3c4") || rq.CutGone != 1 {
				t.Errorf("results %s; cut and gone %d", kinds(rq.Results), rq.CutGone)
			}
		})
	}
}

// What a thread holds from before the request's start is not the request's: a cut call there is not read for it.
func TestACutCallBeforeTheRequestIsNotItsResult(t *testing.T) {
	f := newFixture(t)
	session := "s-before"
	path := filepath.Join(t.TempDir(), session+".jsonl")
	input := `{"command":` + jsonString("cd "+longDir+" && git commit -m 'An earlier fix'") + `}`
	output := hookText + "[main 0e0e0e0] An earlier fix\n 1 file changed"
	offs := writeLines(t, path,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_b1","name":"Bash","input":`+input+`}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_b1","content":`+jsonString(output)+`}]}}`)
	f.cutPair(session, "claude", "Bash", "toolu_b1", at(1), input, output, fmt.Sprintf("%s@%d#0", path, offs[0]), fmt.Sprintf("%s@%d#0", path, offs[1]))
	f.owner(session, at(5), "now fix the parser")
	rq := f.groupWith(Options{Redact: fakeRedact})[0]
	if strings.Contains(kinds(rq.Results), "0e0e0e0") {
		t.Errorf("a commit from before the request: %s", kinds(rq.Results))
	}
}

// A file is one result, by its newest write, with its count of writes.
func TestAFileIsOneResultByItsNewestWrite(t *testing.T) {
	f := newFixture(t)
	f.owner("s-w", at(0), "add the cache")
	f.tool("s-w", "", "Write", at(1), `{"file_path":"/work/cache.go"}`, "File created.", false)
	f.tool("s-w", "", "Edit", at(3), `{"file_path":"/work/cache.go"}`, "File updated.", false)
	rq := f.group()[0]
	var files []Result
	for _, x := range rq.Results {
		if x.Kind == KindFile {
			files = append(files, x)
		}
	}
	if len(files) != 1 || files[0].Writes != 2 || !files[0].At.After(at(3)) {
		t.Fatalf("files: %+v", files)
	}
}

// The wrappers and shapes a push is read through.
func TestWhatAPushIsReadThrough(t *testing.T) {
	cases := []struct{ name, input, output, want string }{
		{"stdbuf", `{"command":"stdbuf -oL git push origin main"}`, "To github.com:example/x.git 1111111..2222222 main -> main",
			"push pushed main -> main (1111111..2222222) to github.com:example/x.git"},
		{"nice -N", `{"command":"nice -10 git push origin main"}`, "To github.com:example/x.git 1111111..2222222 main -> main",
			"push pushed main -> main (1111111..2222222) to github.com:example/x.git"},
		// in a text the store joined, a To word is a push's only when a remote's shape follows it
		{"a To word that names no remote", `{"command":"git push origin HEAD:main 2>&1 | tail -n 1"}`, "Note To self 1111111..2222222 HEAD -> main",
			"push pushed HEAD -> main (1111111..2222222) to origin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := artifactsAs("Bash", c.input, c.output, false); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A writer's input the store cut after its path is read as stored: no whole read, nothing counted as cut.
func TestAWriterCutAfterItsPathIsNotReadWhole(t *testing.T) {
	f := newFixture(t)
	f.owner("s-wc", at(0), "add the cache")
	gone := filepath.Join(t.TempDir(), "s-wc.jsonl")
	f.cutPair("s-wc", "claude", "Write", "toolu_w", at(1), `{"file_path":"/work/cache.go","content":"`+strings.Repeat("x", 300)+`"}`, "File created.",
		gone+"@0#0", gone+"@100#0")
	rq := f.groupWith(Options{Redact: fakeRedact})[0]
	if kinds(rq.Results) != "file /work/cache.go" || rq.CutGone != 0 {
		t.Fatalf("results %s, cut and gone %d", kinds(rq.Results), rq.CutGone)
	}
}

// A checked result that can no longer be read says why: its transcript is gone, or what was read whole could not
// be masked.
func TestALostResultSaysWhy(t *testing.T) {
	failing := func(context.Context, []string) ([]string, error) { return nil, fmt.Errorf("no scrubber") }
	for _, c := range []struct {
		name   string
		remove bool
		redact func(context.Context, []string) ([]string, error)
		want   string
	}{
		{"the transcript is gone", true, fakeRedact, "the transcript it was read from is gone"},
		{"the masking failed", false, failing, "what it was read from could not be masked"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			command := "cd " + longDir + " && git commit -m 'Fix the parser'"
			output := hookText + "[main 1a2b3c4] Fix the parser\n 1 file changed"
			path := claudeCall(t, f, t.TempDir(), "s-lost", command, output)
			f.reply("s-lost", at(3), "Committed the parser fix.")
			f.phase("s-lost", at(4), "phase", "idle", nil)
			rq := f.groupWith(Options{Redact: fakeRedact})[0]
			if _, err := Check(context.Background(), f.st, rq, at(10)); err != nil {
				t.Fatal(err)
			}
			if c.remove {
				removeFile(t, path)
			}
			reqs, err := Group(context.Background(), f.st, t0.Add(-time.Hour), Options{Redact: c.redact})
			if err != nil {
				t.Fatal(err)
			}
			if rq = reqs[0]; rq.State != Checked || !strings.Contains(rq.Why, c.want) {
				t.Errorf("state %s (%s)", rq.State, rq.Why)
			}
		})
	}
}

// A session's full id names it, though it starts another session's id.
func TestAFullSessionIdIsNeverAmbiguous(t *testing.T) {
	f := newFixture(t)
	f.owner("s-abc", at(0), "fix the parser")
	f.owner("s-abcd", at(1), "write the notes")
	f.put()
	rq, err := Find(context.Background(), f.st, "s-abc", Options{})
	if err != nil || words(rq.Asks) != "fix the parser" {
		t.Fatalf("find: %v %+v", err, rq)
	}
	if _, err := Find(context.Background(), f.st, "s-ab", Options{}); err == nil {
		t.Fatal("a start two sessions share named one")
	}
}

// compress replaces a transcript with its zstd copy, as Codex compresses a rollout.
func compress(t *testing.T, path string) {
	t.Helper()
	plain, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".zst", enc.EncodeAll(plain, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	removeFile(t, path)
}

func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}
