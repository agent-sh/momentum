package request

// Tests from the first review of the requests change. Synthetic data only.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// livePhase adds a sessions run's event as it writes one: keyed by the session's key, its harness the key's, and
// its transcript in the reference when the run knew it.
func (f *fixture) livePhase(key, harness, transcript string, ts time.Time, kind, to string, extra map[string]any) {
	f.phase(key, ts, kind, to, extra)
	e := &f.evs[len(f.evs)-1]
	e.Harness = harness
	if transcript != "" {
		e.Ref = transcript + "@4096"
	}
}

func (f *fixture) groupWith(opt Options) []*Request {
	f.t.Helper()
	f.put()
	reqs, err := Group(context.Background(), f.st, t0.Add(-time.Hour), opt)
	if err != nil {
		f.t.Fatal(err)
	}
	return reqs
}

// A sessions run keys a session by its harness's key: hermes:ID, opencode:ID, pi: and the transcript's base name,
// a Claude Code background job by its job. Each is read as the session the transcripts file its events under.
func TestEachHarnesssPhaseIsRead(t *testing.T) {
	cases := []struct {
		name, session, key, harness, transcript string
	}{
		{"hermes", "20261107_100000_abc123", "hermes:20261107_100000_abc123", "hermes", ""},
		{"opencode", "ses_probe0001", "opencode:ses_probe0001", "opencode", ""},
		{"pi", "0b9c7d2e-1111-4222-8333-944455556666", "pi:2026-11-07T10-00-00-000Z_0b9c7d2e-1111-4222-8333-944455556666", "pi",
			"/work/pi/sessions/--work--/2026-11-07T10-00-00-000Z_0b9c7d2e-1111-4222-8333-944455556666.jsonl"},
		{"claude background job", "6f1e2d3c-aaaa-4bbb-8ccc-0d1e2f3a4b5c", "job-7f3e", "claude",
			"/work/claude/projects/-work/6f1e2d3c-aaaa-4bbb-8ccc-0d1e2f3a4b5c.jsonl"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.owner(c.session, at(0), "summarize the inbox")
			f.reply(c.session, at(1), "Which folder?")
			f.livePhase(c.key, c.harness, c.transcript, at(2), "phase", "needs_you", map[string]any{"ask": "Which folder?", "ask_kind": "question"})
			rq := f.group()[0]
			if rq.State != NeedsYou || !strings.Contains(rq.Open, "Which folder?") {
				t.Fatalf("a %s session a sessions run saw waiting on him shows %q, open step %q", c.name, rq.State, rq.Open)
			}
		})
	}
}

// The same for an opencode session a sessions run saw working.
func TestAnOpencodeSessionsWorkingPhaseIsRead(t *testing.T) {
	f := newFixture(t)
	id := "ses_probe0001"
	f.owner(id, at(0), "rename the module")
	f.livePhase("opencode:"+id, "opencode", "", at(2), "phase", "working", nil)
	rq := f.group()[0]
	if rq.State != Working {
		t.Fatalf("an opencode session a sessions run saw working shows %q", rq.State)
	}
}

// A session's phase counts only once a sessions run saw it after the last turn in it: one from before shows unknown.
func TestAPhaseFromBeforeTheLastTurnIsUnknown(t *testing.T) {
	f := newFixture(t)
	f.owner("s-old-phase", at(0), "draft the notes")
	f.reply("s-old-phase", at(1), "Drafted.")
	f.phase("s-old-phase", at(2), "phase", "idle", nil)
	f.owner("s-old-phase", at(5), "and the summary")
	f.reply("s-old-phase", at(6), "Added.")
	rq := f.group()[0]
	if rq.State != Unknown || !strings.Contains(stateLine(rq), "before") {
		t.Fatalf("a phase from before his last turn: %s (%s)", rq.State, stateLine(rq))
	}
}

// A request every session of which moved on to another request says so.
func TestARequestWhoseSessionsMovedOnSaysSo(t *testing.T) {
	f := newFixture(t)
	f.owner("s-moved", at(0), "write the migration")
	first := f.group()[0]
	other := ID(at(-600), "an older request")
	f.owner("s-moved", at(10), Line(other)+" - a handoff ... "+Line(other))
	f.phase("s-moved", at(11), "phase", "idle", nil)
	for _, rq := range f.group() {
		if rq.ID == first.ID {
			if rq.State != Unknown || !strings.Contains(stateLine(rq), "moved on") {
				t.Fatalf("its state: %s (%s)", rq.State, stateLine(rq))
			}
			return
		}
	}
	t.Fatal("the request is gone")
}

// Results are what the tools printed: a file read, a log or a search result that holds text shaped like a commit
// line is no commit.
func TestOutputNobodyCommittedWithIsNoCommit(t *testing.T) {
	f := newFixture(t)
	f.owner("s-read", at(0), "why did the job fail")
	f.tool("s-read", "", "Read", at(1), `{"file_path":"/work/notes.txt"}`, "[main 1a2b3c4] Fix the date parser 1 file changed", false)
	f.tool("s-read", "", "Bash", at(2), `{"command":"cat job.log"}`, "[INFO 20261107] job started", false)
	rq := f.group()[0]
	for _, x := range rq.Results {
		if x.Kind == KindCommit {
			t.Fatalf("an output nobody committed with is a commit: %s", kinds(rq.Results))
		}
	}
}

// A subagent's report is the agent's claim, not what a tool printed: an Agent call's result that quotes a commit
// line, a push and a pull request's address is no artifact.
func TestASubagentsReportIsNoArtifact(t *testing.T) {
	f := newFixture(t)
	f.owner("s-agent", at(0), "ship the fix")
	f.tool("s-agent", "", "Agent", at(1), `{"prompt":"commit, push, then open it with gh pr create"}`,
		"Done. [main 2b3c4d5] Fix the parser. To github.com:example/x.git 1111111..2222222 main -> main. https://github.com/example/x/pull/9", false)
	rq := f.group()[0]
	if len(rq.Results) != 0 {
		t.Fatalf("a subagent's words are artifacts: %s", kinds(rq.Results))
	}
}

// git push --dry-run pushes nothing.
func TestADryRunIsNoPush(t *testing.T) {
	f := newFixture(t)
	f.owner("s-dry", at(0), "see what a push would do")
	f.tool("s-dry", "", "Bash", at(1), `{"command":"git push --dry-run origin main"}`, "To github.com:example/x.git 1111111..2222222 main -> main", false)
	rq := f.group()[0]
	if len(rq.Results) != 0 {
		t.Fatalf("a dry run is a push: %s", kinds(rq.Results))
	}
}

// An artifact is read only from the result of the call that made it: a commit from a shell call that ran a git
// command that makes commits, a push from a git push that is no dry run, a pull request from gh pr create, a file
// from a file-writing call. Every tool kind against every command kind.
func TestArtifactsComeOnlyFromTheCallThatMadeThem(t *testing.T) {
	commit := "[main 1a2b3c4] Fix it 1 file changed"
	push := "To github.com:example/x.git 1111111..2222222 main -> main"
	pr := "https://github.com/example/x/pull/7"
	cases := []struct {
		name, tool, input, output string
		failed                    bool
		want                      string
	}{
		{"git commit in Bash", "Bash", `{"command":"git add -A && git commit -m 'Fix it'"}`, commit, false, "commit commit 1a2b3c4 on main: Fix it"},
		{"git -C DIR commit", "Bash", `{"command":"git -C /work/x commit -am fix"}`, commit, false, "commit commit 1a2b3c4 on main: Fix it"},
		{"git cherry-pick in Codex", "exec_command", `{"cmd":"git cherry-pick 9f9f9f9"}`, commit, false, "commit commit 1a2b3c4 on main: Fix it"},
		{"git revert in Hermes", "terminal", `{"command":"git revert --no-edit HEAD"}`, commit, false, "commit commit 1a2b3c4 on main: Fix it"},
		{"git merge in pi", "bash", `{"command":"git merge --no-ff topic"}`, commit, false, "commit commit 1a2b3c4 on main: Fix it"},
		{"git am", "Bash", `{"command":"git am fix.patch"}`, commit, false, "commit commit 1a2b3c4 on main: Fix it"},
		{"git rebase", "Bash", `{"command":"git rebase -i HEAD~2"}`, commit, false, "commit commit 1a2b3c4 on main: Fix it"},
		{"a commit that failed", "Bash", `{"command":"git commit -m x"}`, commit, true, ""},
		{"git log printing a commit line", "Bash", `{"command":"git log -1"}`, commit, false, ""},
		{"git show", "Bash", `{"command":"git show HEAD"}`, commit, false, ""},
		{"cat of a file", "Bash", `{"command":"cat notes.txt"}`, commit, false, ""},
		{"Read", "Read", `{"file_path":"/work/notes.txt"}`, commit, false, ""},
		{"Grep", "Grep", `{"pattern":"git commit"}`, commit, false, ""},
		{"WebFetch", "WebFetch", `{"url":"https://example.com/git commit"}`, commit + " " + push + " " + pr, false, ""},
		{"Agent", "Agent", `{"prompt":"git commit, git push, gh pr create"}`, commit + " " + push + " " + pr, false, ""},
		{"Task", "Task", `{"prompt":"git commit, git push, gh pr create"}`, commit + " " + push + " " + pr, false, ""},
		{"git push", "Bash", `{"command":"git push origin main"}`, push, false, "push pushed main -> main (1111111..2222222) to github.com:example/x.git"},
		{"git push -n", "Bash", `{"command":"git push -n origin main"}`, push, false, ""},
		{"git push --dry-run", "Bash", `{"command":"git push --dry-run"}`, push, false, ""},
		{"git push -nv", "Bash", `{"command":"git push -nv origin main"}`, push, false, ""},
		{"git fetch", "Bash", `{"command":"git fetch origin"}`, "From github.com:example/x 1111111..2222222 main -> origin/main", false, ""},
		{"a push that failed", "Bash", `{"command":"git push"}`, push, true, ""},
		{"gh pr create", "Bash", `{"command":"gh pr create --fill"}`, pr, false, "pr pull request " + pr},
		{"gh pr view", "Bash", `{"command":"gh pr view 7"}`, pr, false, ""},
		{"gh pr create's text in an echo", "Bash", `{"command":"echo run gh pr create"}`, pr, false, ""},
		{"a commit's text echoed", "Bash", `{"command":"echo 'git commit'"}`, commit, false, ""},
		{"git's name as an argument", "Bash", `{"command":"echo git commit done"}`, commit, false, ""},
		{"a push's words as an argument", "Bash", `{"command":"grep -rn git push docs"}`, push, false, ""},
		{"a tool with a command field that is no shell", "mcp__runner__run", `{"command":"git commit -m x && git push && gh pr create"}`,
			commit + " " + push + " " + pr, false, ""},
		{"Write", "Write", `{"file_path":"/work/a.go","content":"[main 1a2b3c4] x"}`, "File created.", false, "file /work/a.go"},
		{"an Edit that failed", "Edit", `{"file_path":"/work/a.go"}`, "denied", true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// the rule alone, on the call and its result
			res := store.Event{Source: store.Transcripts, SourceKey: "r", TS: at(1).Add(time.Second), Tool: c.tool, Kind: "tool_result", Summary: c.output}
			if c.failed {
				res.Meta = `{"is_error":true}`
			}
			call := store.Event{Source: store.Transcripts, SourceKey: "c", TS: at(1), Tool: c.tool, Kind: "tool_call", Summary: c.input}
			if got := kinds(Artifacts(call, res)); got != c.want {
				t.Fatalf("Artifacts: got %q, want %q", got, c.want)
			}
			// and as a request reads it from the store
			f := newFixture(t)
			f.owner("s-art", at(0), "do the thing")
			f.tool("s-art", "", c.tool, at(1), c.input, c.output, c.failed)
			if got := kinds(f.group()[0].Results); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A request handed off twice. Its id and the newest session's id must name the same request, with the same newest
// result.
func TestItsIDAndItsSessionNameTheSameRequest(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.owner("s-one", at(0), "port the scheduler")
	f.tool("s-one", "", "Bash", at(1), `{"command":"git commit"}`, "[main 1111aaa] Port the scheduler", false)
	f.phase("s-one", at(2), "phase", "idle", nil)
	f.put()
	rq, err := Find(ctx, f.st, "s-one", Options{})
	if err != nil {
		t.Fatal(err)
	}
	text := Handoff(rq)
	// the second session continues it and commits; it ends before the third starts
	f.owner("s-two", at(100), "continue: "+text)
	f.tool("s-two", "", "Bash", at(101), `{"command":"git commit"}`, "[main 2222bbb] Port the timers", false)
	f.phase("s-two", at(102), "phase", "idle", nil)
	// the third pastes the same handoff later and has only replied so far
	f.owner("s-three", at(300), "pick this up: "+text)
	f.reply("s-three", at(301), "Reading it.")
	f.phase("s-three", at(302), "phase", "idle", nil)
	f.put()
	byID, err := Find(ctx, f.st, rq.ID, Options{})
	if err != nil {
		t.Fatal(err)
	}
	bySession, err := Find(ctx, f.st, "s-three", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if byID.ID != bySession.ID || byID.Newest().Key != bySession.Newest().Key || Handoff(byID) != Handoff(bySession) {
		t.Fatalf("by id %s newest %s; by session %s newest %s (threads %d vs %d)", byID.ID, byID.Newest().Key, bySession.ID,
			bySession.Newest().Key, len(byID.Threads), len(bySession.Threads))
	}
}

// The same request in a list whose window holds only the third session: the list and the handoff must agree on
// its state once the owner checked its newest result.
func TestListAndHandoffAgreeOnACheck(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.owner("s-one", at(0), "port the scheduler")
	f.tool("s-one", "", "Bash", at(1), `{"command":"git commit"}`, "[main 1111aaa] Port the scheduler", false)
	f.phase("s-one", at(2), "phase", "idle", nil)
	f.put()
	rq, _ := Find(ctx, f.st, "s-one", Options{})
	text := Handoff(rq)
	f.owner("s-two", at(100), "continue: "+text)
	f.tool("s-two", "", "Bash", at(101), `{"command":"git commit"}`, "[main 2222bbb] Port the timers", false)
	f.phase("s-two", at(102), "phase", "idle", nil)
	f.put()
	full, _ := Find(ctx, f.st, rq.ID, Options{})
	if err := f.st.PutRequestCheck(ctx, store.RequestCheck{Request: full.ID, Result: full.Newest().Key, At: at(103)}); err != nil {
		t.Fatal(err)
	}
	f.owner("s-three", at(300), "pick this up: "+text)
	f.phase("s-three", at(302), "phase", "idle", nil)
	f.put()
	full, _ = Find(ctx, f.st, rq.ID, Options{})
	list, err := List(ctx, f.st, at(200), Options{})
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %d", err, len(list))
	}
	if list[0].State != full.State || Handoff(list[0]) != Handoff(full) {
		t.Fatalf("handoff says %s, list says %s; the list's newest is %s, the check holds %s", full.State, list[0].State,
			list[0].Newest().Key, full.Newest().Key)
	}
}

// A Codex parent thread gets its subagent's report as user-role text ("message from another agent"). That report
// is an agent's reply: a handoff line it quotes must not move the parent session to another request.
func TestACodexSubagentsReportDoesNotLink(t *testing.T) {
	f := newFixture(t)
	f.owner("s-old", at(0), "write the backup script")
	other := f.group()[0]
	f.owner("s-codex", at(10), "check the disk alerts")
	f.event("s-codex", "", "agent_text", store.ActorAgent, at(11), "Report: I printed it. "+Line(other.ID)+" - a handoff ... "+Line(other.ID),
		map[string]any{"why": "message from another agent"})
	f.owner("s-codex", at(12), "now tune the alert thresholds")
	reqs := f.group()
	for _, rq := range reqs {
		if rq.ID == other.ID && strings.Contains(words(rq.Asks), "tune the alert") {
			t.Fatalf("a subagent's report moved the session to %s: %s", rq.ID, words(rq.Asks))
		}
	}
}

// A brief another agent sent to start a session links like a paste.
func TestABriefLinks(t *testing.T) {
	f := newFixture(t)
	f.owner("s-src", at(0), "index the archive")
	src := f.group()[0]
	f.event("s-headless", "", "agent_text", store.ActorAgent, at(10), "Continue this. "+Line(src.ID)+" ... "+Line(src.ID),
		map[string]any{"why": "entrypoint sdk-cli"})
	f.tool("s-headless", "", "Bash", at(11), `{"command":"git commit -m index"}`, "[main 7c7c7c7] Index the archive", false)
	rq := f.group()[0]
	if len(f.group()) != 1 || !strings.Contains(kinds(rq.Results), "7c7c7c7") {
		t.Fatalf("the brief did not link: %s", kinds(rq.Results))
	}
}

// The id hashes the stored words of the opening. A read again that stores other words for it (tacit's cut of a
// paste reads the prompt history at read time) keeps the owner's check, by the time alone while one opening has it.
func TestAReadAgainKeepsTheCheck(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	e := f.owner("s-re", at(0), "fix the importer")
	f.tool("s-re", "", "Bash", at(1), `{"command":"git commit"}`, "[main 3333ccc] Fix the importer", false)
	f.phase("s-re", at(2), "phase", "idle", nil)
	rq := f.group()[0]
	if err := f.st.PutRequestCheck(ctx, store.RequestCheck{Request: rq.ID, Result: rq.Newest().Key, At: at(3)}); err != nil {
		t.Fatal(err)
	}
	e.Summary = "fix the importer [the paste it cut before]"
	f.evs = append(f.evs, e)
	again := f.group()[0]
	if again.ID == rq.ID || again.State != Checked {
		t.Fatalf("after a read again the id is %s (was %s) and the state %s", again.ID, rq.ID, again.State)
	}
	found, err := Find(ctx, f.st, rq.ID, Options{})
	if err != nil || found.ID != again.ID {
		t.Fatalf("the old id finds %+v, %v", found, err)
	}
}

// A handoff pasted with the id from before a read again still links, by the time alone.
func TestAReadAgainKeepsTheLinks(t *testing.T) {
	f := newFixture(t)
	e := f.owner("s-re", at(0), "fix the importer")
	rq := f.group()[0]
	f.owner("s-cont", at(20), Line(rq.ID)+" - a handoff ... "+Line(rq.ID))
	f.tool("s-cont", "", "Bash", at(21), `{"command":"git commit"}`, "[main 4444ddd] Import the rest", false)
	e.Summary = "fix the importer [the paste it cut before]"
	f.evs = append(f.evs, e)
	reqs := f.group()
	if len(reqs) != 1 || !strings.Contains(kinds(reqs[0].Results), "4444ddd") {
		t.Fatalf("the handoff's link is lost: %d requests", len(reqs))
	}
}

// He types the same short turn twice in a thread within two minutes, after the agent stopped: his last turn has
// no reply, so the open step is that turn, not unknown.
func TestARepeatedTurnKeepsTheOpenStep(t *testing.T) {
	f := newFixture(t)
	f.owner("s-rep", at(0), "continue")
	f.reply("s-rep", at(0).Add(10*time.Second), "Stopped: the tests need a database.")
	f.owner("s-rep", at(1), "continue")
	f.phase("s-rep", at(2), "phase", "idle", nil)
	rq := f.group()[0]
	if rq.Open == "" || len(rq.Asks) != 2 {
		t.Fatalf("open step %q; asks %q, last turn %v", rq.Open, words(rq.Asks), rq.LastTurn)
	}
}

// The same turn recorded twice, nothing between, is one turn, and still the open step.
func TestATurnRecordedTwiceIsOneAndTheOpenStep(t *testing.T) {
	f := newFixture(t)
	f.owner("s-twice", at(0), "continue")
	f.owner("s-twice", at(0).Add(30*time.Second), "continue")
	f.phase("s-twice", at(2), "phase", "idle", nil)
	rq := f.group()[0]
	if len(rq.Asks) != 1 || rq.Open != "the last turn has no reply recorded: continue" {
		t.Fatalf("asks %q, open step %q", words(rq.Asks), rq.Open)
	}
}

// A fork carries the source's history over: a file written once must count one write, a new branch pushed once
// one push.
func TestAForkDoesNotDoubleResults(t *testing.T) {
	f := newFixture(t)
	for _, s := range []string{"s-src", "s-forked"} {
		f.owner(s, at(0), "add the cache")
		f.tool(s, "", "Write", at(1), `{"file_path":"/work/cache.go"}`, "File created.", false)
		f.tool(s, "", "Bash", at(2), `{"command":"git push -u origin cache"}`, "To github.com:example/x.git * [new branch] cache -> cache", false)
	}
	rq := f.group()[0]
	pushes := 0
	for _, x := range rq.Results {
		if x.Kind == KindFile && x.Writes != 1 {
			t.Errorf("a file written once counts %d writes", x.Writes)
		}
		if x.Kind == KindPush {
			pushes++
		}
	}
	if pushes != 1 {
		t.Errorf("one push shows %d times: %s", pushes, kinds(rq.Results))
	}
}

// A program types the same words into three sessions a few milliseconds apart; two of them land in the same
// millisecond. The turn is a program's in all three, so it is no copy that joins their requests.
func TestAProgramsTurnJoinsNothing(t *testing.T) {
	f := newFixture(t)
	f.owner("s-p1", at(0), "refactor the parser")
	f.owner("s-p2", at(1), "write the changelog")
	f.owner("s-p3", at(2), "bump the deps")
	f.owner("s-p1", at(30), "run the nightly check")
	f.owner("s-p2", at(30), "run the nightly check")
	f.owner("s-p3", at(30).Add(5*time.Millisecond), "run the nightly check")
	reqs := f.group()
	if len(reqs) != 3 {
		t.Fatalf("want three requests, got %d", len(reqs))
	}
}

// A turn a configured prefix opens is a program's: it opens no request.
func TestAProgramPrefixOpensNothing(t *testing.T) {
	f := newFixture(t)
	f.owner("s-wake", at(0), "[wake] continue the loop")
	f.reply("s-wake", at(1), "Continuing.")
	if reqs := f.groupWith(Options{Prefixes: []string{"[wake]"}}); len(reqs) != 0 {
		t.Fatalf("a program's turn opened %d requests", len(reqs))
	}
	if reqs := f.groupWith(Options{}); len(reqs) != 1 {
		t.Fatalf("without the prefix it is his: %d requests", len(reqs))
	}
}

// A handoff a program pasted still links, though its words are not his.
func TestAProgramsPastedHandoffLinks(t *testing.T) {
	f := newFixture(t)
	f.owner("s-src", at(0), "tune the cache")
	src := f.groupWith(Options{Prefixes: []string{"[relay]"}})[0]
	f.owner("s-relayed", at(10), "[relay] "+Line(src.ID)+" - a handoff ... "+Line(src.ID))
	f.tool("s-relayed", "", "Bash", at(11), `{"command":"git commit -m tune"}`, "[main 5d5d5d5] Tune the cache", false)
	reqs := f.groupWith(Options{Prefixes: []string{"[relay]"}})
	if len(reqs) != 1 || !strings.Contains(kinds(reqs[0].Results), "5d5d5d5") {
		t.Fatalf("the program's paste did not link: %d requests", len(reqs))
	}
	if w := words(reqs[0].Asks); strings.Contains(w, "[relay]") {
		t.Fatalf("a program's words are his: %q", w)
	}
	if a := reqs[0].Asks[len(reqs[0].Asks)-1]; a.Handoff != src.ID || !reqs[0].LastTurn.Equal(at(10)) {
		t.Fatalf("the paste is no turn of it: %+v, last turn %v", a, reqs[0].LastTurn)
	}
}

// A request found by its id reaches a session that continues its own from the links that name it: the
// continuation names its parent, not the other way round.
func TestFindReachesASessionThatContinuesIt(t *testing.T) {
	f := newFixture(t)
	f.owner("s-parent", at(0), "rewrite the importer")
	f.reply("s-parent", at(1), "Started.")
	f.event("s-child", "", "agent_text", store.ActorAgent, at(30), "Continuing after the compression.", map[string]any{"parent": "s-parent"})
	f.event("s-child", "", "tool_call", store.ActorAgent, at(31), `{"command":"git commit -m importer"}`, map[string]any{"parent": "s-parent", "tool_id": "c-child"})
	f.evs[len(f.evs)-1].Tool = "terminal"
	f.event("s-child", "", "tool_result", store.ActorTool, at(32), "[main 8a8a8a8] Rewrite the importer", map[string]any{"parent": "s-parent", "tool_id": "c-child"})
	f.evs[len(f.evs)-1].Tool = "terminal"
	f.put()
	rq, err := Find(context.Background(), f.st, ID(at(0), "rewrite the importer"), Options{})
	if err != nil || !strings.Contains(kinds(rq.Results), "8a8a8a8") {
		t.Fatalf("the continuation's commit: %v %+v", err, rq)
	}
}

// longHandoff is a request with enough results that its handoff passes 2,000 bytes.
func longHandoff(f *fixture) *Request {
	f.owner("s-long", at(0), "split the monolith")
	for i := 0; i < 12; i++ {
		f.tool("s-long", "", "Bash", at(1+i), `{"command":"git commit"}`,
			fmt.Sprintf("[main %07x] Move part %d of the monolith into its own package with its tests and docs", 0xa000000+i, i), false)
	}
	f.reply("s-long", at(20), "Twelve parts moved; the gateway is left.")
	return f.group()[0]
}

// A handoff over 2,000 bytes pasted as his words is cut by the store with one id line left: it still links, and
// the rest of the cut text is the handoff, not his words.
func TestAPastedHandoffOverTwoThousandBytesLinks(t *testing.T) {
	f := newFixture(t)
	rq := longHandoff(f)
	text := Handoff(rq)
	pasted := strings.Join(strings.Fields("please continue: "+text), " ")
	if len(pasted) <= 2000 {
		t.Fatalf("the handoff is only %d bytes", len(pasted))
	}
	pasted = pasted[:2000] + "…"
	if strings.Count(pasted, Line(rq.ID)) != 1 {
		t.Fatalf("the cut holds %d id lines", strings.Count(pasted, Line(rq.ID)))
	}
	f.owner("s-next", at(60), pasted)
	f.tool("s-next", "", "Bash", at(61), `{"command":"git commit"}`, "[main 6e6e6e6] Move the gateway", false)
	reqs := f.group()
	if len(reqs) != 1 || !strings.Contains(kinds(reqs[0].Results), "6e6e6e6") {
		t.Fatalf("the cut paste did not link: %d requests", len(reqs))
	}
	if w := words(reqs[0].Asks); w != "split the monolith | please continue:" {
		t.Fatalf("his words: %q", w)
	}
}

// A check stored under an id that resolves to the request (an opening joined into it) counts for it.
func TestACheckUnderAnAliasCounts(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.owner("s-main", at(0), "ship the exporter")
	f.event("s-next", "", "tool_call", store.ActorAgent, at(19), "{}", map[string]any{"parent": "s-main"})
	f.owner("s-next", at(20), "now write the release notes")
	f.tool("s-next", "", "Bash", at(21), `{"command":"git commit"}`, "[main 3c4d5e6] Release notes", false)
	f.phase("s-next", at(22), "phase", "idle", nil)
	rq := f.group()[0]
	if len(rq.Aliases) != 1 {
		t.Fatalf("aliases: %v", rq.Aliases)
	}
	if err := f.st.PutRequestCheck(ctx, store.RequestCheck{Request: rq.Aliases[0], Result: rq.Newest().Key, At: at(23)}); err != nil {
		t.Fatal(err)
	}
	if rq = f.group()[0]; rq.State != Checked {
		t.Fatalf("a check under its alias: %s", rq.State)
	}
}

// A session seen waiting on him or working comes before a check.
func TestALiveSessionComesBeforeACheck(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.owner("s-live", at(0), "bump the version")
	f.tool("s-live", "", "Bash", at(1), `{"command":"git commit"}`, "[main 9c0d1e2] Bump the version", false)
	f.phase("s-live", at(2), "phase", "idle", nil)
	rq := f.group()[0]
	if err := f.st.PutRequestCheck(ctx, store.RequestCheck{Request: rq.ID, Result: rq.Newest().Key, At: at(3)}); err != nil {
		t.Fatal(err)
	}
	f.phase("s-live", at(4), "phase", "working", nil)
	if rq = f.group()[0]; rq.State != Working {
		t.Fatalf("working after a check: %s", rq.State)
	}
	f.phase("s-live", at(5), "phase", "needs_you", map[string]any{"ask": "Tag it?"})
	if rq = f.group()[0]; rq.State != NeedsYou {
		t.Fatalf("waiting after a check: %s", rq.State)
	}
}

// A start of two sessions' ids names neither.
func TestAnAmbiguousSessionPrefixIsRefused(t *testing.T) {
	f := newFixture(t)
	f.owner("s-dup-1", at(0), "one thing")
	f.owner("s-dup-2", at(1), "another thing")
	f.put()
	if _, err := Find(context.Background(), f.st, "s-dup", Options{}); err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("an ambiguous start: %v", err)
	}
	if rq, err := Find(context.Background(), f.st, "s-dup-2", Options{}); err != nil || rq.ID != ID(at(1), "another thing") {
		t.Fatalf("a whole id: %v", err)
	}
}

// Uncheck undoes a check stored under an id that resolves to the request.
func TestUncheckUndoesACheckUnderAnAlias(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.owner("s-main", at(0), "ship the exporter")
	f.event("s-next", "", "tool_call", store.ActorAgent, at(19), "{}", map[string]any{"parent": "s-main"})
	f.owner("s-next", at(20), "now write the release notes")
	f.tool("s-next", "", "Bash", at(21), `{"command":"git commit"}`, "[main 3c4d5e6] Release notes", false)
	rq := f.group()[0]
	if err := f.st.PutRequestCheck(ctx, store.RequestCheck{Request: rq.Aliases[0], Result: rq.Newest().Key, At: at(23)}); err != nil {
		t.Fatal(err)
	}
	c, err := Uncheck(ctx, f.st, f.group()[0], at(24))
	if err != nil || c.Request != rq.Aliases[0] {
		t.Fatalf("uncheck: %+v %v", c, err)
	}
	if _, standing, _ := f.group()[0].Standing(); standing {
		t.Fatal("the check under the alias still stands")
	}
}

// What a thread did before a request began is not the request's: a request found by its id, which reads from its
// start, and one found by a list hold the same.
func TestRequestStartBoundsItsResults(t *testing.T) {
	f := newFixture(t)
	f.tool("s-pre", "", "Bash", at(0), `{"command":"git commit -m before"}`, "[main 0f0f0f0] Before anyone asked", false)
	f.owner("s-pre", at(5), "tidy the docs")
	f.tool("s-pre", "", "Bash", at(6), `{"command":"git commit -m docs"}`, "[main 1f1f1f1] Tidy the docs", false)
	rq := f.group()[0]
	if got := kinds(rq.Results); strings.Contains(got, "0f0f0f0") || !strings.Contains(got, "1f1f1f1") {
		t.Fatalf("results: %s", got)
	}
	if rq.Threads[0].First.Before(at(5)) {
		t.Fatalf("the session's span in it starts at %v", rq.Threads[0].First)
	}
}
