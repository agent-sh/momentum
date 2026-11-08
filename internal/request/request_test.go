package request

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A synthetic store: every session, turn and result here is made up.

var t0 = time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

func at(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }

type fixture struct {
	t   *testing.T
	st  *store.Store
	n   int
	evs []store.Event
}

func newFixture(t *testing.T) *fixture {
	st, err := store.Open(filepath.Join(t.TempDir(), "momentum.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &fixture{t: t, st: st}
}

func metaJSON(kv map[string]any) string {
	if len(kv) == 0 {
		return ""
	}
	b, _ := json.Marshal(kv)
	return string(b)
}

// event adds a transcript event of a session's main thread (agent "") or of one of its subagents.
func (f *fixture) event(session, agent, kind string, actor store.Actor, ts time.Time, summary string, meta map[string]any) store.Event {
	f.n++
	e := store.Event{Source: store.Transcripts, SourceKey: fmt.Sprintf("%s.jsonl@%012d#%03d", session, f.n, 0), TS: ts, Session: session,
		Agent: agent, Harness: "claude", Cwd: "/work/" + session, Actor: actor, Kind: kind, Summary: summary, Meta: metaJSON(meta)}
	f.evs = append(f.evs, e)
	return e
}

func (f *fixture) owner(session string, ts time.Time, words string) store.Event {
	return f.event(session, "", "owner_text", store.ActorOwner, ts, words, map[string]any{"input_kind": "typed"})
}

func (f *fixture) reply(session string, ts time.Time, text string) store.Event {
	return f.event(session, "", "agent_text", store.ActorAgent, ts, text, nil)
}

// tool adds a call and its result, the call's input and the result's output as the store keeps them.
func (f *fixture) tool(session, agent, tool string, ts time.Time, input, output string, failed bool) {
	f.n++
	id := fmt.Sprintf("call-%d", f.n)
	f.event(session, agent, "tool_call", store.ActorAgent, ts, input, map[string]any{"tool_id": id})
	f.evs[len(f.evs)-1].Tool = tool
	m := map[string]any{"tool_id": id}
	if failed {
		m["is_error"] = true
	}
	f.event(session, agent, "tool_result", store.ActorTool, ts.Add(time.Second), lined(output), m)
	f.evs[len(f.evs)-1].Tool = tool
}

// lined is a tool's output as it printed it, with its lines: a fixture's one-line text ends its line, as a
// transcript keeps it. (The store joins a plain output's lines; a request reads that whole from the transcript.)
func lined(s string) string {
	if s == "" || strings.Contains(s, "\n") || strings.HasSuffix(s, "…") {
		return s
	}
	return s + "\n"
}

func (f *fixture) phase(session string, ts time.Time, kind, to string, extra map[string]any) {
	f.n++
	m := map[string]any{"to": to}
	for k, v := range extra {
		m[k] = v
	}
	if to == "" {
		delete(m, "to")
	}
	f.evs = append(f.evs, store.Event{Source: store.Sessions, SourceKey: fmt.Sprintf("%s@%d", session, f.n), TS: ts, Session: session,
		Harness: "claude", Actor: store.ActorHarness, Kind: kind, Meta: metaJSON(m)})
}

func (f *fixture) put() {
	f.t.Helper()
	if _, err := f.st.Put(context.Background(), f.evs); err != nil {
		f.t.Fatal(err)
	}
	f.evs = nil
}

func (f *fixture) group() []*Request {
	f.t.Helper()
	f.put()
	reqs, err := Group(context.Background(), f.st, t0.Add(-time.Hour), Options{})
	if err != nil {
		f.t.Fatal(err)
	}
	return reqs
}

func kinds(rs []Result) string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Kind+" "+r.Text)
	}
	return strings.Join(out, " | ")
}

func words(as []Ask) string {
	var out []string
	for _, a := range as {
		out = append(out, a.Words)
	}
	return strings.Join(out, " | ")
}

// One conversation is one request: the turn that opens it, the follow-ups after it, and what its tools recorded.
func TestAThreadIsOneRequestWithItsResults(t *testing.T) {
	f := newFixture(t)
	f.owner("s-alpha", at(0), "fix the date parser")
	f.reply("s-alpha", at(1), "Looking at the parser.")
	f.tool("s-alpha", "", "Edit", at(2), `{"file_path":"/work/s-alpha/date.go","old_string":"a","new_string":"b"}`, "The file was updated.", false)
	f.tool("s-alpha", "", "Bash", at(3), `{"command":"git commit -am 'Fix the date parser'"}`, "[main 1a2b3c4] Fix the date parser\n 1 file changed, 2 insertions(+)", false)
	f.owner("s-alpha", at(10), "also add a test")
	f.tool("s-alpha", "", "Write", at(11), `{"file_path":"/work/s-alpha/date_test.go","content":"package x"}`, "File created.", false)
	f.reply("s-alpha", at(12), "Done: the parser is fixed and tested.")
	reqs := f.group()
	if len(reqs) != 1 {
		t.Fatalf("want one request, got %d", len(reqs))
	}
	rq := reqs[0]
	if rq.ID != ID(at(0), "fix the date parser") || !rq.Opened.Equal(at(0)) {
		t.Fatalf("id %s opened %v", rq.ID, rq.Opened)
	}
	if words(rq.Asks) != "fix the date parser | also add a test" {
		t.Fatalf("asks: %s", words(rq.Asks))
	}
	if got := kinds(rq.Results); got != "file /work/s-alpha/date_test.go | commit commit 1a2b3c4 on main: Fix the date parser | file /work/s-alpha/date.go" {
		t.Fatalf("results: %s", got)
	}
	if rq.Report == nil || rq.Report.Text != "Done: the parser is fixed and tested." {
		t.Fatalf("report: %+v", rq.Report)
	}
	// No sessions run recorded its phase: whether it still runs is not known.
	if rq.State != Unknown {
		t.Fatalf("state %s", rq.State)
	}
}

// A subagent's work, a thread named as an agent of it, and a session that names it as its parent are the same
// request.
func TestSubagentsThreadsAndParentsJoin(t *testing.T) {
	f := newFixture(t)
	f.owner("s-main", at(0), "ship the exporter")
	f.tool("s-main", "agent-1", "Bash", at(2), `{"command":"git commit -m x"}`, "[feature 2b3c4d5] Add the exporter", false)
	// a Codex subagent thread inside s-main, whose own subagent's events are in a session of its id
	f.event("s-main", "t-child", "agent_text", store.ActorAgent, at(3), "child working", nil)
	f.tool("t-child", "t-grandchild", "exec_command", at(4), `{"cmd":"git push"}`,
		"To github.com:example/exporter.git 1111111..2222222 feature -> feature", false)
	// a session that continues s-main (its events name it as their parent)
	f.event("s-next", "", "harness_text", store.ActorHarness, at(20), "continued", map[string]any{"parent": "s-main"})
	f.owner("s-next", at(21), "now write the release notes")
	f.event("s-next", "", "tool_call", store.ActorAgent, at(22), "{}", map[string]any{"parent": "s-main"})
	f.tool("s-next", "", "Bash", at(23), `{"command":"git commit"}`, "[feature 3c4d5e6] Release notes", false)
	reqs := f.group()
	if len(reqs) != 1 {
		t.Fatalf("want one request, got %d: %+v", len(reqs), reqs)
	}
	rq := reqs[0]
	if words(rq.Asks) != "ship the exporter | now write the release notes" {
		t.Fatalf("asks: %s", words(rq.Asks))
	}
	got := kinds(rq.Results)
	for _, want := range []string{"commit 2b3c4d5", "pushed feature -> feature (1111111..2222222) to github.com:example/exporter.git", "commit 3c4d5e6"} {
		if !strings.Contains(got, want) {
			t.Fatalf("results lack %q: %s", want, got)
		}
	}
}

// A fork carries its turns over with their time: the same words at the same millisecond in two threads are one
// turn, and the fork continues its source's request. The same words in two sessions a moment apart are a
// program's and open nothing.
func TestCopiesJoinAndTwinsAreAProgramsTurns(t *testing.T) {
	f := newFixture(t)
	f.owner("s-orig", at(0), "build the cache")
	f.reply("s-orig", at(1), "Built.")
	// the fork: the copied turn, then its own
	f.owner("s-fork", at(0), "build the cache")
	f.reply("s-fork", at(1), "Built.")
	f.owner("s-fork", at(30), "now try an LRU")
	f.tool("s-fork", "", "Bash", at(31), `{"command":"git commit"}`, "[lru 4d5e6f7] Try an LRU", false)
	// a program typed the same words into two sessions
	f.owner("s-cron-a", at(50), "run the nightly check")
	f.owner("s-cron-b", at(50).Add(700*time.Millisecond), "run the nightly check")
	reqs := f.group()
	if len(reqs) != 1 {
		t.Fatalf("want one request, got %d", len(reqs))
	}
	rq := reqs[0]
	if rq.ID != ID(at(0), "build the cache") || words(rq.Asks) != "build the cache | now try an LRU" {
		t.Fatalf("id %s asks %s", rq.ID, words(rq.Asks))
	}
	if len(rq.Threads) != 2 || !strings.Contains(kinds(rq.Results), "commit 4d5e6f7") {
		t.Fatalf("threads %+v results %s", rq.Threads, kinds(rq.Results))
	}
}

// cutEnds is how the store keeps an agent text: its first 160 and last 80 bytes (SPEC, Session history).
func cutEnds(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= 240 {
		return s
	}
	return s[:160] + "… " + s[len(s)-80:]
}

// A handoff pasted into another session links it back: the stored cut of the paste still holds the id, a fresh
// session continues the request, and a session in the middle of another request switches to it from that turn.
func TestAPastedHandoffLinksBack(t *testing.T) {
	f := newFixture(t)
	f.owner("s-first", at(0), "migrate the config loader")
	f.tool("s-first", "", "Bash", at(5), `{"command":"git commit"}`, "[main 5e6f7a8] Migrate the loader", false)
	f.reply("s-first", at(6), "Half done; the env overrides are left.")
	reqs := f.group()
	if len(reqs) != 1 {
		t.Fatalf("want one request, got %d", len(reqs))
	}
	text := Handoff(reqs[0])
	id := reqs[0].ID
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if !strings.HasPrefix(lines[0], Line(id)) || lines[len(lines)-1] != Line(id) {
		t.Fatalf("the id lines: %q ... %q", lines[0], lines[len(lines)-1])
	}
	// A fresh session: tacit cut the paste from his turn (an agent text the store clips), and his own words.
	f.event("s-fresh", "", "agent_text", store.ActorAgent, at(60), cutEnds(text), map[string]any{"why": "cut from the owner's turn"})
	f.owner("s-fresh", at(60), "continue with the env overrides")
	f.tool("s-fresh", "", "Bash", at(65), `{"command":"git commit"}`, "[main 6f7a8b9] Env overrides", false)
	// A session of another request, into which he pastes the handoff whole, as his own words the store clips.
	f.owner("s-other", at(70), "tidy the readme")
	f.tool("s-other", "", "Bash", at(71), `{"command":"git commit"}`, "[docs 7a8b9c0] Tidy the readme", false)
	pasted := strings.Join(strings.Fields("please pick this up "+text), " ")
	if len(pasted) > 2000 {
		pasted = pasted[:2000] + "…"
	}
	f.owner("s-other", at(80), pasted)
	f.tool("s-other", "", "Bash", at(81), `{"command":"git commit"}`, "[main 8b9c0d1] Loader docs", false)
	f.phase("s-other", at(82), "phase", "idle", nil)
	reqs = f.group()
	if len(reqs) != 2 {
		t.Fatalf("want two requests, got %d", len(reqs))
	}
	var mig, readme *Request
	for _, rq := range reqs {
		switch rq.ID {
		case id:
			mig = rq
		case ID(at(70), "tidy the readme"):
			readme = rq
		}
	}
	if mig == nil || readme == nil {
		t.Fatalf("requests %s, %s", reqs[0].ID, reqs[1].ID)
	}
	got := kinds(mig.Results)
	for _, want := range []string{"5e6f7a8", "6f7a8b9", "8b9c0d1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the migration lacks %s: %s", want, got)
		}
	}
	if strings.Contains(got, "7a8b9c0") || !strings.Contains(kinds(readme.Results), "7a8b9c0") || strings.Contains(kinds(readme.Results), "8b9c0d1") {
		t.Fatalf("the readme's commit stays its own: %s / %s", got, kinds(readme.Results))
	}
	if w := words(mig.Asks); w != "migrate the config loader | continue with the env overrides | please pick this up" {
		t.Fatalf("his words, the pasted handoff out of them: %q", w)
	}
	// s-other's phase counts for the migration, which its latest segment is, not for the readme
	for _, th := range readme.Threads {
		if th.Current {
			t.Fatalf("the readme's thread moved on: %+v", th)
		}
	}
	if readme.State != Unknown {
		t.Fatalf("the readme's state: %s", readme.State)
	}
}

// A handoff's id whose opening is not in the store still names a request: its start shows as unknown.
func TestAnIdWithNoOpeningInTheStore(t *testing.T) {
	f := newFixture(t)
	gone := ID(at(-600), "an opening this store never saw")
	f.owner("s-here", at(0), Line(gone)+" - a handoff of the owner's request Asked: ... "+Line(gone))
	f.reply("s-here", at(1), "Picking it up.")
	reqs := f.group()
	if len(reqs) != 1 || reqs[0].ID != gone || !reqs[0].Opened.IsZero() {
		t.Fatalf("requests: %+v", reqs)
	}
	if text := Handoff(reqs[0]); !strings.Contains(text, "unknown: the request's opening is not in this store") {
		t.Fatalf("handoff: %s", text)
	}
}

// Only user-role text links: an agent's reply or a tool's output that holds a handoff joins nothing.
func TestRepliesAndToolOutputDoNotLink(t *testing.T) {
	f := newFixture(t)
	f.owner("s-a", at(0), "write the backup script")
	reqs := f.group()
	text := Handoff(reqs[0])
	f.owner("s-b", at(10), "check the disk alerts")
	f.reply("s-b", at(11), cutEnds(text))
	f.tool("s-b", "", "Bash", at(12), `{"command":"momentum request handoff s-a"}`, text[:240], false)
	reqs = f.group()
	if len(reqs) != 2 {
		t.Fatalf("want two requests, got %d", len(reqs))
	}
	for _, rq := range reqs {
		if len(rq.Threads) != 1 || !rq.Threads[0].Current {
			t.Fatalf("%s holds %+v", rq.ID, rq.Threads)
		}
	}
}

// The state comes from the sessions runs' observations: a thread that waits on him, works, or neither.
func TestStateFromObservedPhases(t *testing.T) {
	cases := []struct {
		name     string
		phase    func(f *fixture)
		replied  bool
		want     string
		wantOpen string
	}{
		{"never seen", func(f *fixture) {}, true, Unknown, ""},
		{"waits on him", func(f *fixture) {
			f.phase("s-x", at(9), "phase", "needs_you", map[string]any{"ask": "Run the migration on prod?", "ask_kind": "question"})
		}, true, NeedsYou, "waits on the owner (question): Run the migration on prod?"},
		{"working", func(f *fixture) { f.phase("s-x", at(9), "phase", "working", nil) }, true, Working, ""},
		{"a background job runs", func(f *fixture) { f.phase("s-x", at(9), "phase", "background", nil) }, true, Working, ""},
		{"a run is scheduled", func(f *fixture) { f.phase("s-x", at(9), "phase", "scheduled", nil) }, true, Working, ""},
		// a phase seen at the last turn's time is not seen after it
		{"idle at the last turn's time", func(f *fixture) { f.phase("s-x", at(5), "phase", "idle", nil) }, true, Unknown, ""},
		{"idle after a result", func(f *fixture) { f.phase("s-x", at(9), "phase", "idle", nil) }, true, Ready, ""},
		{"gone with no reply", func(f *fixture) { f.phase("s-x", at(9), "vanished", "", map[string]any{"from": "idle"}) }, false, Open,
			"the last turn has no reply recorded: and the summary"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.owner("s-x", at(0), "draft the report")
			f.reply("s-x", at(1), "Drafted.")
			f.owner("s-x", at(5), "and the summary")
			if c.replied {
				f.reply("s-x", at(6), "Summary added.")
			}
			c.phase(f)
			reqs := f.group()
			if len(reqs) != 1 || reqs[0].State != c.want || !strings.Contains(reqs[0].Open, c.wantOpen) || c.wantOpen == "" && reqs[0].Open != "" {
				t.Fatalf("state %q open %q", reqs[0].State, reqs[0].Open)
			}
		})
	}
}

// A check binds the newest result. A newer artifact reopens it, and the owner can undo it.
func TestACheckIsReopenedByANewerResultAndUndone(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.owner("s-c", at(0), "bump the version")
	f.tool("s-c", "", "Bash", at(1), `{"command":"git commit"}`, "[main 9c0d1e2] Bump the version", false)
	f.reply("s-c", at(2), "Bumped.")
	f.phase("s-c", at(3), "phase", "idle", nil)
	rq := f.group()[0]
	if rq.State != Ready || rq.Newest().Key != "commit:9c0d1e2" {
		t.Fatalf("state %s newest %+v", rq.State, rq.Newest())
	}
	if err := f.st.PutRequestCheck(ctx, store.RequestCheck{Request: rq.ID, Result: rq.Newest().Key, At: at(4)}); err != nil {
		t.Fatal(err)
	}
	if rq = f.group()[0]; rq.State != Checked {
		t.Fatalf("checked: %s", rq.State)
	}
	// a newer report alone does not reopen a request with an artifact
	f.reply("s-c", at(5), "Anything else?")
	if rq = f.group()[0]; rq.State != Checked {
		t.Fatalf("a reply reopened it: %s", rq.State)
	}
	f.tool("s-c", "", "Bash", at(6), `{"command":"git commit --amend"}`, "[main 0d1e2f3] Bump the version to 2", false)
	if rq = f.group()[0]; rq.State != Ready || !strings.Contains(Handoff(rq), "is reopened by a newer result") {
		t.Fatalf("reopened: %s\n%s", rq.State, Handoff(rq))
	}
	c, err := f.st.UndoRequestCheck(ctx, []string{rq.ID}, at(7))
	if err != nil || c.Result != "commit:9c0d1e2" {
		t.Fatalf("undo: %+v %v", c, err)
	}
	if _, standing, _ := f.group()[0].Standing(); standing {
		t.Fatal("the undone check still stands")
	}
	if _, err := f.st.UndoRequestCheck(ctx, []string{rq.ID}, at(8)); err != store.ErrNoCheck {
		t.Fatalf("undo with none standing: %v", err)
	}
}

// A request with no artifact has its report as its result: a newer report reopens its check.
func TestAReportIsTheResultOfARequestWithNoArtifact(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.owner("s-q", at(0), "why is the build slow")
	f.reply("s-q", at(1), "The cache is cold.")
	f.phase("s-q", at(2), "phase", "idle", nil)
	rq := f.group()[0]
	if n := rq.Newest(); n == nil || n.Kind != KindReport {
		t.Fatalf("newest: %+v", n)
	}
	if err := f.st.PutRequestCheck(ctx, store.RequestCheck{Request: rq.ID, Result: rq.Newest().Key, At: at(3)}); err != nil {
		t.Fatal(err)
	}
	f.reply("s-q", at(4), "Also the linker is slow.")
	if rq = f.group()[0]; rq.State != Ready {
		t.Fatalf("a newer report reopens it: %s", rq.State)
	}
}

// Results are what the tools printed: a failed call's push, a failed write, a fetch's ref lines and a pull request
// only viewed are none.
func TestResultsComeFromToolOutput(t *testing.T) {
	f := newFixture(t)
	f.owner("s-r", at(0), "open the pull request")
	f.tool("s-r", "", "Bash", at(1), `{"command":"git push"}`, "To github.com:example/x.git 1e2f3a4..2e2f3a4 main -> main error: hook declined", true)
	f.tool("s-r", "", "Bash", at(2), `{"command":"git fetch"}`, "From github.com:example/x 2222222..3333333 main -> origin/main", false)
	f.tool("s-r", "", "Bash", at(3), `{"command":"gh pr view 7"}`, "https://github.com/example/x/pull/7", false)
	f.tool("s-r", "", "Write", at(4), `{"file_path":"/work/s-r/a.txt"}`, "permission denied", true)
	f.tool("s-r", "", "Bash", at(5), `{"command":"git push -u origin feat"}`,
		"To github.com:example/x.git\n * [new branch]      feat -> feat\nbranch 'feat' set up to track 'origin/feat'.", false)
	f.tool("s-r", "", "Bash", at(6), `{"command":"gh pr create --fill"}`, "https://github.com/example/x/pull/8", false)
	f.tool("s-r", "", "apply_patch", at(7), "*** Begin Patch *** Update File: src/b.go @@ *** Add File: src/c.go +x *** End Patch", "Success.", false)
	f.tool("s-r", "", "Bash", at(8), `{"command":"git commit"}`, "[main (root-commit) 2f3a4b5] First\n 1 file changed", false)
	f.tool("s-r", "", "Bash", at(9), `{"command":"git commit"}`, "[detached HEAD 3a4b5c6] Fix", false)
	rq := f.group()[0]
	want := "commit commit 3a4b5c6 on detached HEAD: Fix | commit commit 2f3a4b5 on main: First | file src/b.go | file src/c.go | " +
		"pr pull request https://github.com/example/x/pull/8 | push pushed new branch feat -> feat to github.com:example/x.git"
	if got := kinds(rq.Results); got != want {
		t.Fatalf("results:\n got %s\nwant %s", got, want)
	}
}

// The id says where its opening is: it reads back to the opening's time.
func TestIDsReadBack(t *testing.T) {
	id := ID(at(0), "anything")
	if got, ok := ParseID(id); !ok || !got.Equal(at(0)) {
		t.Fatalf("%s reads %v %v", id, got, ok)
	}
	for _, bad := range []string{"", "r", "request", "rzz", "s-alpha", "r" + strings.Repeat("z", 20) + "abcd"} {
		if _, ok := ParseID(bad); ok {
			t.Fatalf("%q read as an id", bad)
		}
	}
}
