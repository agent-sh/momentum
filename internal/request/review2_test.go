package request

// Tests from the second review of the requests change. Synthetic data only.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func TestArtifactsOfEachHarnesssShapes(t *testing.T) {
	cases := []struct{ name, tool, input, output, want string }{
		// Codex's code-mode exec: the call's input is a script, its command in an unquoted cmd key.
		{"codex code-mode commit", "exec", `text(await tools.exec_command({cmd:"git commit -m 'Fix it'","workdir":"/work/x"}))`,
			`Script completed Wall time 1.5 seconds Output: {"chunk_id":"15bcbe","exit_code":0,"output":"[main 1a2b3c4] Fix it\n 1 file changed"}`,
			"commit commit 1a2b3c4 on main: Fix it"},
		// Hermes keeps a result as JSON text: the To line follows an escaped newline.
		{"hermes push after an escaped newline", "terminal", `{"command": "git push origin main"}`,
			`{"output": "ok\nTo github.com:example/x.git\n   1111111..2222222  main -> main", "exit_code": 0}`,
			"push pushed main -> main (1111111..2222222) to github.com:example/x.git"},
		// Hermes's input is Python's JSON: a space after the colon.
		{"hermes write_file path after a space", "write_file", `{"path": "/work/a.py", "content": "x = 1"}`, `{"bytes_written": 5}`, "file /work/a.py"},
		// opencode's and Hermes's patch: the patch is a JSON string, its newlines escaped.
		{"patch header inside a JSON string", "patch", `{"patchText":"*** Begin Patch\n*** Update File: /work/a.go\n@@\n-a\n+b\n*** End Patch"}`, "Success.", "file /work/a.go"},
		// A real push that failed and a dry run in one line: the dry run's ref lines are no push.
		{"a dry run's ref lines beside a failed push", "Bash", `{"command":"git push 2>&1 | head -n 2; git push origin topic --dry-run 2>&1 | head -n 2"}`,
			"fatal: 'nowhere' does not appear to be a git repository To https://github.com/example/x.git * [new branch] topic -> topic", ""},
		// gh pr create --dry-run creates nothing; a pull request that already exists was not made by this call.
		{"gh pr create --dry-run, already exists", "Bash", `{"command":"gh pr create --dry-run --head topic --fill; echo exit $?"}`,
			`a pull request for branch "topic" into branch "main" already exists: https://github.com/example/x/pull/270 exit 0`, ""},
		// timeout runs the command after it.
		{"timeout git push", "Bash", `{"command":"timeout 120 git push origin main"}`, "To github.com:example/x.git 1111111..2222222 main -> main",
			"push pushed main -> main (1111111..2222222) to github.com:example/x.git"},
		// A loop's body runs gh pr create.
		{"gh pr create in a loop body", "Bash", `{"command":"for r in a b; do gh pr create -R example/$r --fill; done"}`, "https://github.com/example/a/pull/20",
			"pr pull request https://github.com/example/a/pull/20"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := artifactsAs(c.tool, c.input, c.output, false); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A Codex subagent's own session (it holds its subagents' events) is a thread of the request in a list; a handoff by
// that session's id must find the same request (SPEC: ID is ... a session's id ..., for the request its thread is in
// now).
func TestAHandoffByASubagentsSessionAgreesWithTheList(t *testing.T) {
	f := newFixture(t)
	f.owner("s-main", at(0), "ship the exporter")
	f.event("s-main", "t-child", "agent_text", store.ActorAgent, at(3), "child working", nil)
	f.tool("t-child", "t-grandchild", "exec_command", at(4), `{"cmd":"git push"}`,
		"To github.com:example/exporter.git 1111111..2222222 feature -> feature", false)
	reqs := f.group()
	if len(reqs) != 1 {
		t.Fatalf("want one request, got %d", len(reqs))
	}
	inList := false
	for _, th := range reqs[0].Threads {
		inList = inList || th.ID == "t-child" && th.Current
	}
	if !inList {
		t.Fatalf("the list does not hold t-child: %+v", reqs[0].Threads)
	}
	rq, err := Find(context.Background(), f.st, "t-child", Options{Redact: fakeRedact})
	if err != nil {
		t.Fatalf("handoff t-child: %v (the list holds it in %s)", err, reqs[0].ID)
	}
	if rq.ID != reqs[0].ID {
		t.Fatalf("handoff t-child found %s, the list %s", rq.ID, reqs[0].ID)
	}
}

// A list and a handoff of one request must say the same of its sessions' phases. s-a's last phase was seen before it
// pasted B's handoff; a list that also holds an older request reads that phase, a handoff of B reads phases only from
// B's own first thread on.
func TestListAndHandoffAgreeOnAPhaseFromBefore(t *testing.T) {
	f := newFixture(t)
	f.owner("s-old", at(-30), "an older request")
	f.reply("s-old", at(-29), "On it.")
	f.owner("s-a", at(0), "request a")
	f.phase("s-a", at(5), "phase", "idle", nil)
	f.owner("s-b", at(10), "request b")
	f.reply("s-b", at(11), "Started b.")
	reqs := f.group()
	var b *Request
	for _, rq := range reqs {
		if rq.ID == ID(at(10), "request b") {
			b = rq
		}
	}
	if b == nil {
		t.Fatal("no request b")
	}
	f.owner("s-a", at(20), "carry on\n"+pasted(b.ID, "..."))
	reqs = f.group()
	for _, rq := range reqs {
		if rq.ID == b.ID {
			b = rq
		}
	}
	h, err := Find(context.Background(), f.st, b.ID, Options{Redact: fakeRedact})
	if err != nil {
		t.Fatal(err)
	}
	ph := func(rq *Request) string {
		var out []string
		for _, th := range rq.Threads {
			out = append(out, th.ID+"="+th.Phase)
		}
		return strings.Join(out, ",") + " / " + rq.Why
	}
	if ph(b) != ph(h) {
		t.Fatalf("list: %s\nhandoff: %s", ph(b), ph(h))
	}
}

// Links that form cycles (two sessions naming each other as parent, two handoffs naming each other) settle.
func TestLinksThatFormCyclesSettle(t *testing.T) {
	f := newFixture(t)
	f.owner("s-1", at(0), "first ask")
	f.owner("s-2", at(5), "second ask")
	f.event("s-1", "", "harness_text", store.ActorHarness, at(1), "x", map[string]any{"parent": "s-2"})
	f.event("s-2", "", "harness_text", store.ActorHarness, at(6), "y", map[string]any{"parent": "s-1"})
	f.put()
	a, b := ID(at(0), "first ask"), ID(at(5), "second ask")
	f.owner("s-1", at(10), Line(b)+" - a handoff of the owner request go")
	f.owner("s-2", at(11), Line(a)+" - a handoff of the owner request go")
	done := make(chan error, 1)
	go func() {
		f.put()
		_, err := List(context.Background(), f.st, t0.Add(-time.Hour), Options{Redact: fakeRedact})
		if err == nil {
			_, err = Find(context.Background(), f.st, a, Options{Redact: fakeRedact})
		}
		if err == nil {
			_, err = Find(context.Background(), f.st, "s-2", Options{Redact: fakeRedact})
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("did not settle in 20 s")
	}
}

// A turn that opens with momentum's own marker is a program's: it is no follow-up of his, and opens nothing.
func TestMomentumsOwnTurnIsAPrograms(t *testing.T) {
	f := newFixture(t)
	f.owner("s-mark", at(0), "fix the flaky test")
	f.reply("s-mark", at(1), "Stopped on an API error.")
	f.owner("s-mark", at(5), "[momentum] Your last turn stopped on a transient API error. Continue where you left off.")
	f.reply("s-mark", at(6), "Continuing.")
	f.owner("s-auto", at(10), "[momentum] Continue where you left off.")
	reqs := f.group()
	if len(reqs) != 1 || words(reqs[0].Asks) != "fix the flaky test" || !reqs[0].LastTurn.Equal(at(0)) {
		t.Fatalf("requests %d, asks %q", len(reqs), words(reqs[0].Asks))
	}
}

// What a dry run or an existing pull request prints is no artifact, and a pull request's address after one the
// call names is.
func TestDryRunsAndExistingPullRequestsMakeNothing(t *testing.T) {
	cases := []struct{ name, input, output, want string }{
		{"a dry push beside a real one", `{"command":"git push origin main && git push -n origin topic"}`,
			"To github.com:example/x.git 1111111..2222222 main -> main", ""},
		{"gh pr create --dry-run", `{"command":"gh pr create --dry-run --fill"}`, "https://github.com/example/x/pull/9", ""},
		{"an existing pull request", `{"command":"gh pr create --fill"}`,
			`a pull request for branch "topic" into branch "main" already exists: https://github.com/example/x/pull/270`, ""},
		{"gh pr create in a subshell", `{"command":"(cd x && gh pr create --fill)"}`, "https://github.com/example/x/pull/9",
			"pr pull request https://github.com/example/x/pull/9"},
		{"git push in a command substitution", `{"command":"echo $(git push origin main 2>&1)"}`,
			"To github.com:example/x.git 1111111..2222222 main -> main", "push pushed main -> main (1111111..2222222) to github.com:example/x.git"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := artifactsAs("Bash", c.input, c.output, false); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A merge that makes no commit makes none; a push whose output a tail cut to its ref line pushed; a fetch's ref
// lines beside a push that printed none are no push.
func TestWhatAMergeOrAPushPrints(t *testing.T) {
	cases := []struct{ name, input, output, want string }{
		{"git merge --no-commit", `{"command":"git merge --no-commit --no-ff topic"}`, "[main 1a2b3c4] Merge topic", ""},
		{"git merge --abort", `{"command":"git merge --abort"}`, "[main 1a2b3c4] Merge topic", ""},
		{"git cherry-pick -n", `{"command":"git cherry-pick -n 9f9f9f9"}`, "[main 1a2b3c4] Pick", ""},
		{"a push a tail cut to its ref line", `{"command":"git push origin HEAD:main 2>&1 | tail -n 1"}`, "1675ef4..710d01a HEAD -> main",
			"push pushed HEAD -> main (1675ef4..710d01a) to origin"},
		{"a fetch's ref line beside a quiet push", `{"command":"git fetch origin && git push -q origin main"}`,
			"From github.com:example/x 1111111..2222222 main -> origin/main", ""},
		{"a quiet fetch beside a push a tail cut", `{"command":"git fetch -q origin && git push origin HEAD:main 2>&1 | tail -n 1"}`,
			"20f6298..f5127b7 HEAD -> main", "push pushed HEAD -> main (20f6298..f5127b7) to origin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := artifactsAs("Bash", c.input, c.output, false); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
