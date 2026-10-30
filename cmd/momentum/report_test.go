package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/claude"
	"github.com/agent-sh/momentum/sessions/proc"
)

// onMachine makes every report in the test scan the sources fresh() makes, over a machine of no processes: the
// sessions are the test's alone.
func onMachine(t *testing.T, fresh func() []sessions.Source) {
	t.Helper()
	snap, srcs := reportSnapshot, reportSources
	t.Cleanup(func() { reportSnapshot, reportSources = snap, srcs })
	reportSnapshot = func(_ context.Context, _ sessions.Env, now time.Time) (*sessions.World, error) {
		return &sessions.World{Now: now, Procs: &proc.Table{ByPID: map[int]*proc.Proc{}}}, nil
	}
	reportSources = func(sessions.Env) []sessions.Source { return fresh() }
}

// The contract the views read, spelled out apart from the code that writes it.
type contractLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type contractItem struct {
	ID     string         `json:"id"`
	Title  string         `json:"title"`
	Detail string         `json:"detail"`
	Effect string         `json:"effect"`
	At     string         `json:"at"`
	Links  []contractLink `json:"links"`
	Source string         `json:"source"`
}

type contractReport struct {
	Version     int            `json:"version"`
	GeneratedAt string         `json:"generated_at"`
	NeedsYou    []contractItem `json:"needs_you"`
	Decided     []contractItem `json:"decided"`
	Shipped     []contractItem `json:"shipped"`
	Running     []contractItem `json:"running"`
	Problems    []contractItem `json:"problems"`
	Counts      map[string]int `json:"counts"`
}

var contractSections = []string{"needs_you", "decided", "shipped", "running", "problems"}

// reportJSON runs `momentum report --json` and checks its output against the contract: one JSON object on one
// line, every section an array of at most 20 items with a count, every item with an id, a plain one-line title of
// at most 120 characters, an RFC 3339 time, links (http or https) and a source, and no id twice.
func reportJSON(t *testing.T) (contractReport, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"report", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	raw := out.String()
	if strings.Count(raw, "\n") != 1 || !strings.HasSuffix(raw, "\n") {
		t.Fatalf("not one line: %q", raw)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range top {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if strings.Join(keys, " ") != "counts decided generated_at needs_you problems running shipped version" {
		t.Fatalf("keys %v", keys)
	}
	var r contractReport
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339, r.GeneratedAt); err != nil || r.Version != 1 {
		t.Fatalf("version %d, generated_at %q: %v", r.Version, r.GeneratedAt, err)
	}
	seen := map[string]bool{}
	for _, name := range contractSections {
		if !bytes.HasPrefix(top[name], []byte("[")) {
			t.Fatalf("%s is not an array: %s", name, top[name])
		}
		var items []map[string]json.RawMessage
		json.Unmarshal(top[name], &items)
		if len(items) > 20 || r.Counts[name] < len(items) {
			t.Fatalf("%s: %d items, count %d", name, len(items), r.Counts[name])
		}
		for _, it := range items {
			for k := range it {
				if !slices.Contains([]string{"id", "title", "detail", "effect", "at", "links", "source"}, k) {
					t.Fatalf("%s: an item has %q", name, k)
				}
			}
			var c contractItem
			raw, _ := json.Marshal(it)
			json.Unmarshal(raw, &c)
			_, atErr := time.Parse(time.RFC3339, c.At)
			switch {
			case c.ID == "" || seen[c.ID]:
				t.Fatalf("%s: id %q is empty or not unique", name, c.ID)
			case c.Title == "" || len([]rune(c.Title)) > 120:
				t.Fatalf("%s: title %q", name, c.Title)
			case plain.Line(c.ID) != c.ID || plain.Line(c.Title) != c.Title || plain.Line(c.Detail) != c.Detail || plain.Line(c.Effect) != c.Effect ||
				plain.Line(c.Source) != c.Source:
				t.Fatalf("%s: an item's text is not plain: %+v", name, c)
			case atErr != nil || c.Source == "" || !bytes.HasPrefix(it["links"], []byte("[")):
				t.Fatalf("%s: item %s", name, raw)
			}
			for _, l := range c.Links {
				if u, err := url.Parse(l.URL); l.Label == "" || plain.Line(l.Label) != l.Label || err != nil || (u.Scheme != "http" && u.Scheme != "https") {
					t.Fatalf("%s: link %+v", name, l)
				}
			}
			seen[c.ID] = true
		}
	}
	return r, raw
}

func ids(items []contractItem) string {
	var out []string
	for _, it := range items {
		out = append(out, it.ID)
	}
	return strings.Join(out, " ")
}

// newStore makes the store the commands use, empty, as the first sessions run or ingest would.
func newStore(t *testing.T) {
	t.Helper()
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
}

// putNotes stores notes in the store the commands use.
func putNotes(t *testing.T, ns ...store.Note) {
	t.Helper()
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, n := range ns {
		if _, err := st.PutNote(context.Background(), n); err != nil {
			t.Fatal(err)
		}
		if !n.Done.IsZero() {
			if _, _, err := st.DoneNote(context.Background(), n.ID, n.Done); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The report holds the notes that stand, by importance and then newest first: needs and problems until done,
// decided and shipped from the last 7 days. Running is what the scan finds in flight: the sessions working, and
// the workflows, agents, background jobs and monitors of any session, an automated run's last.
func TestTheReportHoldsTheNotesThatStandAndWhatRuns(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	now := time.Now()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	putNotes(t,
		store.Note{ID: "ask-low", Kind: store.NoteNeeds, Title: "Rename the tool?", Importance: store.ImportanceLow, At: ago(time.Minute)},
		store.Note{ID: "ask-old", Kind: store.NoteNeeds, Title: "Merge the parser fix?", Importance: store.ImportanceHigh, At: ago(30 * 24 * time.Hour)},
		store.Note{ID: "ask-gpu", Kind: store.NoteNeeds, Title: "Rent a GPU for the eval?", Detail: "The eval waits on it.", At: ago(2 * time.Hour)},
		store.Note{ID: "ask-done", Kind: store.NoteNeeds, Title: "Answered already?", At: ago(time.Hour), Done: ago(time.Minute)},
		store.Note{ID: "dec-old", Kind: store.NoteDecided, Title: "Kept the old cache", At: ago(8 * 24 * time.Hour)},
		store.Note{ID: "dec-new", Kind: store.NoteDecided, Title: "Paused the nightly scout", Detail: "Say resume to undo it.", At: ago(2 * 24 * time.Hour)},
		store.Note{ID: "shp-new", Kind: store.NoteShipped, Title: "Scans resume from checkpoints", Effect: "warm scan 2.1 s -> 0.2 s (7 d)",
			Links: []store.Link{{Label: "PR #3", URL: "https://example.com/pr/3"}}, At: ago(24 * time.Hour)},
		store.Note{ID: "shp-gone", Kind: store.NoteShipped, Title: "Withdrawn", At: ago(3 * time.Hour), Done: ago(2 * time.Hour)},
		store.Note{ID: "prb-open", Kind: store.NoteProblem, Title: "Gave up on a session", Source: "supervise", At: ago(time.Hour)},
		store.Note{ID: "prb-done", Kind: store.NoteProblem, Title: "Fixed by hand", At: ago(time.Hour), Done: ago(time.Minute)},
	)
	src := fixed{
		{Key: "sess-a", Tool: "claude", Name: "fix the parser", Project: "agent-sh/momentum", TurnOpen: true, Tasks: []sessions.Task{
			{ID: "ag1", Kind: "agent", Label: "Review the diff", Type: "reviewer", Started: ago(10 * time.Minute)},
			{ID: "bg1", Kind: "bash", Label: "go test ./...", Started: ago(5 * time.Minute)}}},
		{Key: "thread-b", Tool: "codex", Name: "port the cli", Project: "agent-sh/tools", Tasks: []sessions.Task{
			{ID: "wf1", Kind: "workflow", Label: "Port every command", Started: ago(20 * time.Minute)}}},
		{Key: "h-idle", Tool: "hermes", Name: "chat", Project: "home", Tasks: []sessions.Task{}},
		{Key: "cron-1", Tool: "hermes", Name: "nightly digest", Project: "home", Auto: true, TurnOpen: true},
		{Key: "sess-c", Tool: "claude", Name: "tune the kernel", TurnOpen: true, AskKind: sessions.AskPermission, Ask: "Bash: rm -rf build",
			Tasks: []sessions.Task{{ID: "m1", Kind: "monitor", Label: "wait for the build", Started: ago(2 * time.Minute)}}},
	}
	onMachine(t, func() []sessions.Source { return []sessions.Source{src} })

	r, _ := reportJSON(t)
	for _, c := range []struct{ name, got, want string }{
		{"needs_you", ids(r.NeedsYou), "ask-old ask-gpu ask-low"},
		{"decided", ids(r.Decided), "dec-new"},
		{"shipped", ids(r.Shipped), "shp-new"},
		{"problems", ids(r.Problems), "prb-open"},
		{"running", ids(r.Running), "session:sess-a task:thread-b/wf1 task:sess-a/ag1 task:sess-c/m1 task:sess-a/bg1 session:cron-1"},
	} {
		if c.got != c.want {
			t.Errorf("%s: %s\nwant %s", c.name, c.got, c.want)
		}
	}
	want := map[string]int{"needs_you": 3, "decided": 1, "shipped": 1, "running": 6, "problems": 1}
	if fmt.Sprint(r.Counts) != fmt.Sprint(want) {
		t.Errorf("counts %v, want %v", r.Counts, want)
	}
	if s := r.Shipped[0]; s.Effect != "warm scan 2.1 s -> 0.2 s (7 d)" || len(s.Links) != 1 || s.Links[0].URL != "https://example.com/pr/3" || s.Source != "momentum" {
		t.Errorf("shipped: %+v", s)
	}
	if at, _ := time.Parse(time.RFC3339, r.NeedsYou[0].At); at.Unix() != ago(30*24*time.Hour).Unix() {
		t.Errorf("needs at %s", r.NeedsYou[0].At)
	}
	if p := r.Problems[0]; p.Source != "supervise" {
		t.Errorf("problem: %+v", p)
	}
	for _, c := range []struct {
		i             int
		title, detail string
	}{
		{0, "fix the parser", "Claude Code session in agent-sh/momentum, working, with 1 agent and 1 background job."},
		{1, "Port every command", `Workflow of the Codex session "port the cli" in agent-sh/tools.`},
		{2, "Review the diff", `Agent (reviewer) of the Claude Code session "fix the parser" in agent-sh/momentum.`},
		{3, "wait for the build", `Monitor of the Claude Code session "tune the kernel".`},
		{5, "nightly digest", "Hermes automated run in the home directory, working."},
	} {
		if it := r.Running[c.i]; it.Title != c.title || it.Detail != c.detail || it.Source != "sessions" {
			t.Errorf("running %d: %q %q %q", c.i, it.Title, it.Detail, it.Source)
		}
	}
	if at, _ := time.Parse(time.RFC3339, r.Running[1].At); at.Unix() != ago(20*time.Minute).Unix() {
		t.Errorf("a task is dated when it started: %s", r.Running[1].At)
	}

	// The terminal's form: the same sections, in the views' order.
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"report"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	text := out.String()
	var at []int
	for _, h := range []string{"needs you (3)", "problems (1)", "running (6)", "shipped (1)", "decided (1)"} {
		at = append(at, strings.Index(text, h))
	}
	if slices.Contains(at, -1) || !slices.IsSorted(at) || !strings.Contains(text, "warm scan 2.1 s -> 0.2 s (7 d)") {
		t.Fatalf("text:\n%s", text)
	}
}

// A section's array holds the first 20 of its items, by importance and then newest first; its count holds them all.
// A store with nothing to show gives every section, empty.
func TestEachSectionHoldsTwentyAndCountsAll(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	var ss fixed
	onMachine(t, func() []sessions.Source { return []sessions.Source{ss} })
	newStore(t)
	r, raw := reportJSON(t)
	if !strings.Contains(raw, `"needs_you":[],"decided":[],"shipped":[],"running":[],"problems":[]`) || fmt.Sprint(r.Counts) != "map[decided:0 needs_you:0 problems:0 running:0 shipped:0]" {
		t.Fatalf("an empty report: %s", raw)
	}

	now := time.Now()
	var ns []store.Note
	for i := range 25 {
		ns = append(ns, store.Note{ID: fmt.Sprintf("ask-%02d", i), Kind: store.NoteNeeds, Title: fmt.Sprintf("Question %d?", i), At: now.Add(time.Duration(i-30) * time.Minute)})
	}
	ns[0].Importance = store.ImportanceHigh // the oldest, and first
	putNotes(t, ns...)
	var tasks []sessions.Task
	for i := range 23 {
		tasks = append(tasks, sessions.Task{ID: fmt.Sprintf("bg%02d", i), Kind: "bash", Label: "job", Started: now.Add(time.Duration(i-30) * time.Minute)})
	}
	ss = fixed{{Key: "s", Tool: "claude", Name: "many jobs", Tasks: tasks}}
	r, _ = reportJSON(t)
	if len(r.NeedsYou) != 20 || r.Counts["needs_you"] != 25 || r.NeedsYou[0].ID != "ask-00" || r.NeedsYou[1].ID != "ask-24" || r.NeedsYou[19].ID != "ask-06" {
		t.Fatalf("needs_you: %d of %d: %s", len(r.NeedsYou), r.Counts["needs_you"], ids(r.NeedsYou))
	}
	if len(r.Running) != 20 || r.Counts["running"] != 23 || r.Running[0].ID != "task:s/bg22" {
		t.Fatalf("running: %d of %d: %s", len(r.Running), r.Counts["running"], ids(r.Running))
	}
}

// dumpSessionState is everything a sessions run saves: its events, its stamps and its trackers' checkpoints.
func dumpSessionState(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var b strings.Builder
	for _, q := range []string{
		`SELECT id, ts, kind, summary, meta FROM events ORDER BY id`,
		`SELECT key, phase, since, seq, seen, name, ask FROM session_stamps ORDER BY key`,
		`SELECT tool, key, build, hex(state), log_base, log_seq, seen FROM trackers ORDER BY tool, key`,
		`SELECT tool, key, seq, role, text FROM tracker_log ORDER BY tool, key, seq`,
	} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(&b, vals...)
		}
		rows.Close()
	}
	return b.String()
}

// The report changes nothing. Its scan is the one a sessions run makes, resumed from the checkpoints the last
// sessions run of the same build saved, so it reads only what each transcript gained since; it saves no
// checkpoint, stamp or event, and the next sessions run still records the phase change the report saw.
func TestTheReportResumesTheSessionsScanAndChangesNothing(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	dir := t.TempDir()
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	cl := &jsonlFile{t: t, path: filepath.Join(dir, "claude", "a.jsonl"), at: t0}
	if err := os.MkdirAll(filepath.Dir(cl.path), 0o700); err != nil {
		t.Fatal(err)
	}
	sources := func() []sessions.Source {
		return []sessions.Source{&claude.Source{Lister: listed{{Tool: "claude", Session: "a", Name: "build the docs", Transcript: cl.path, Started: t0}},
			Screen: func(context.Context, *sessions.World, *sessions.Session) string { return "" }}}
	}
	cl.user("build the docs and check every link "+strings.Repeat("with care, ", 60)).
		call("t1", "Bash", map[string]any{"command": "make docs", "description": "render the docs site", "run_in_background": true}).
		result("t1", "Command running in background with ID: bg1")
	for i := range 40 { // the transcript runs on well past the job's lines
		id := fmt.Sprintf("r%d", i)
		cl.call(id, "Read", map[string]any{"file_path": fmt.Sprintf("/w/page%d.md", i)}).result(id, "# page")
	}
	cl.say("The docs render in the background.")
	db := paths.DB()
	sessionsRunOf := func(now time.Time) { // a sessions run of the build the report runs, as a timer runs it
		t.Helper()
		st, err := store.Open(db)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		w := &sessions.World{Now: now, Procs: &proc.Table{ByPID: map[int]*proc.Proc{}}}
		if _, scanErr, err := board(context.Background(), w, nil, st, buildID(), sources()); err != nil || scanErr != nil {
			t.Fatal(err, scanErr)
		}
	}
	sessionsRunOf(cl.at.Add(time.Minute))
	before := dumpSessionState(t, db)
	if !strings.Contains(before, "bg1") && !strings.Contains(before, "background") {
		t.Fatalf("setup: the sessions run saved no state:\n%.400s", before)
	}

	// The job's label, long read and checkpointed, is edited in place: only a scan that reads the transcript from
	// its start sees it. The session goes on with a turn of its own.
	body, _ := os.ReadFile(cl.path)
	edited := strings.Replace(string(body), "render the docs site", "render the DOCS site", 1)
	if edited == string(body) || os.WriteFile(cl.path, []byte(edited), 0o600) != nil {
		t.Fatal("setup")
	}
	cl.user("also build the pdf")
	onMachine(t, sources)
	r, _ := reportJSON(t)
	if got := ids(r.Running); got != "session:a task:a/bg1" {
		t.Fatalf("running: %s", got)
	}
	if r.Running[1].Title != "render the docs site" {
		t.Fatalf("the report read the transcript from its start, not from the checkpoint: %q", r.Running[1].Title)
	}
	if after := dumpSessionState(t, db); after != before {
		t.Fatalf("the report changed the store:\nbefore\n%s\nafter\n%s", before, after)
	}

	// A store with no checkpoints reads the transcript whole, and sees the edit.
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	newStore(t)
	if r, _ := reportJSON(t); len(r.Running) != 2 || r.Running[1].Title != "render the DOCS site" {
		t.Fatalf("a cold report: %+v", r.Running)
	}

	// The sessions run after the report records the change of phase the report saw and did not save.
	sessionsRunOf(time.Now())
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	evs, err := st.Events(context.Background(), store.Query{Since: t0.Add(-time.Hour), Source: store.Sessions, Kinds: []string{"phase"}})
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Meta, `"to":"working"`) {
		t.Fatalf("the next sessions run: %+v %v", evs, err)
	}
}

// A report that cannot be made prints nothing and exits 3, so a view keeps its last good copy, and it makes and
// migrates nothing: with no store (which stays missing), with a store that is not this build's schema yet (which
// stays as it was), with a store a newer build migrated, and without a process table.
func TestAReportThatCannotBeMadeExits3AndPrintsNothing(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", filepath.Join(t.TempDir(), "none"))
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	onMachine(t, func() []sessions.Source { return []sessions.Source{fixed{}} })
	refused := func(name, says string) {
		t.Helper()
		var out, errOut bytes.Buffer
		if code := run(context.Background(), []string{"report", "--json"}, &out, &errOut); code != 3 || out.Len() != 0 || !strings.Contains(errOut.String(), says) {
			t.Fatalf("%s: exit %d\n%s%s", name, code, out.String(), errOut.String())
		}
	}
	refused("no store", "no store to read")
	if _, err := os.Stat(paths.Data()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the report made the store's directory: %v", err)
	}

	putNotes(t, store.Note{Kind: store.NoteNeeds, Title: "Anything?"})
	db, err := sql.Open("sqlite", "file:"+paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`ALTER TABLE notes RENAME TO notes_aside`); err != nil { // a store from before the notes
		t.Fatal(err)
	}
	refused("a store from before the notes", "migrates it")
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'notes'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the report migrated the store: %d %v", n, err)
	}
	if _, err := db.Exec(`ALTER TABLE notes_aside RENAME TO notes; INSERT INTO schema_steps VALUES (1000, 0)`); err != nil {
		t.Fatal(err)
	}
	refused("a store from a newer build", "newer")
	if _, err := db.Exec(`DELETE FROM schema_steps WHERE version = 1000`); err != nil {
		t.Fatal(err)
	}

	reportSnapshot = func(context.Context, sessions.Env, time.Time) (*sessions.World, error) {
		return nil, errors.New("process table: no /proc")
	}
	refused("without a process table", "process table")
}

// A harness whose scan fails is named on stderr, and running holds what its scan did read; the report still prints
// and exits 0, so the notes reach the views.
func TestAReportWhoseScanFailsStillPrints(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	putNotes(t, store.Note{ID: "ask", Kind: store.NoteNeeds, Title: "Merge it?"})
	onMachine(t, func() []sessions.Source {
		return []sessions.Source{&flaky{tool: "codex", fail: true}, fixed{{Key: "w", Tool: "claude", Name: "work", TurnOpen: true}},
			partial{tool: "pi", ss: []sessions.Session{{Key: "p", Tool: "pi", Name: "read in part", TurnOpen: true}}, err: errors.New("one file unreadable")}}
	})
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"report", "--json"}, &out, &errOut); code != 0 ||
		!strings.Contains(errOut.String(), "codex: database is locked") || !strings.Contains(errOut.String(), "pi: one file unreadable") {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var r contractReport
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || ids(r.NeedsYou) != "ask" || ids(r.Running) != "session:p session:w" { // one time, so by id
		t.Fatalf("report %s: %v", out.String(), err)
	}
}

// partial is a harness's source that reads some of its sessions and fails on the rest, as the native sources do:
// they return what they read with the error.
type partial struct {
	tool string
	ss   []sessions.Session
	err  error
}

func (p partial) Tool() string { return p.tool }
func (p partial) Close() error { return nil }
func (p partial) Scan(context.Context, *sessions.World) ([]*sessions.Session, error) {
	var out []*sessions.Session
	for _, s := range p.ss {
		s := s
		out = append(out, &s)
	}
	return out, p.err
}

// stuck is a harness's source whose scan hangs until the test lets it go, past its context's end, as a harness
// command that does not answer and notices the kill late.
type stuck struct {
	release chan struct{}
	closed  *bool
}

func (s stuck) Tool() string { return "codex" }
func (s stuck) Close() error {
	*s.closed = true
	return nil
}
func (s stuck) Scan(ctx context.Context, _ *sessions.World) ([]*sessions.Session, error) {
	<-ctx.Done()
	<-s.release
	return []*sessions.Session{{Key: "late", Tool: "codex", TurnOpen: true}}, ctx.Err()
}

// A harness that does not finish within the scan's budget counts as failed: the report prints by then without it,
// names it on stderr, and does not close it. The other harnesses run beside it, so what they read is in running.
func TestAScanPastItsBudgetStillPrints(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	putNotes(t, store.Note{ID: "ask", Kind: store.NoteNeeds, Title: "Merge it?"})
	budget := reportScanBudget
	t.Cleanup(func() { reportScanBudget = budget })
	reportScanBudget = 200 * time.Millisecond
	closed := false
	release := make(chan struct{})
	defer close(release)
	onMachine(t, func() []sessions.Source {
		return []sessions.Source{fixed{{Key: "w", Tool: "claude", Name: "work", TurnOpen: true}}, stuck{release, &closed},
			partial{tool: "hermes", ss: []sessions.Session{{Key: "h", Tool: "hermes", TurnOpen: true}}}}
	})
	start := time.Now()
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"report", "--json"}, &out, &errOut)
	took := time.Since(start)
	var r contractReport
	if err := json.Unmarshal(out.Bytes(), &r); code != 0 || err != nil || ids(r.NeedsYou) != "ask" || ids(r.Running) != "session:h session:w" || took > 2*time.Second {
		t.Fatalf("exit %d after %v: %s %s %v", code, took, out.String(), errOut.String(), err)
	}
	if !strings.Contains(errOut.String(), "codex: not done by the scan's deadline") || strings.Contains(errOut.String(), "hermes") {
		t.Fatalf("stderr: %s", errOut.String())
	}
	if closed {
		t.Fatal("the report closed the source the deadline left reading")
	}
}

// The snapshot is inside the scan's budget too: a tmux that does not answer takes it, and the report prints the
// notes with no harness scanned, every one named.
func TestASnapshotPastTheBudgetStillPrints(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	putNotes(t, store.Note{ID: "ask", Kind: store.NoteNeeds, Title: "Merge it?"})
	budget := reportScanBudget
	t.Cleanup(func() { reportScanBudget = budget })
	reportScanBudget = 200 * time.Millisecond
	scanned := false
	onMachine(t, func() []sessions.Source { return []sessions.Source{recorded{&scanned}} })
	reportSnapshot = func(ctx context.Context, _ sessions.Env, now time.Time) (*sessions.World, error) {
		select { // a tmux that hangs until its context ends
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
		return &sessions.World{Now: now, Procs: &proc.Table{ByPID: map[int]*proc.Proc{}}}, errors.New("tmux panes: " + context.Cause(ctx).Error())
	}
	start := time.Now()
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"report", "--json"}, &out, &errOut)
	if took := time.Since(start); code != 0 || took > 2*time.Second || !strings.Contains(out.String(), `"id":"ask"`) || !strings.Contains(out.String(), `"running":[]`) {
		t.Fatalf("exit %d after %v: %s %s", code, took, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "tmux panes: context deadline exceeded") || !strings.Contains(errOut.String(), "test: not scanned") || scanned {
		t.Fatalf("stderr %s, scanned %v", errOut.String(), scanned)
	}
}

// recorded is a source that records whether it was scanned.
type recorded struct{ scanned *bool }

func (r recorded) Tool() string { return "test" }
func (r recorded) Close() error { return nil }
func (r recorded) Scan(context.Context, *sessions.World) ([]*sessions.Session, error) {
	*r.scanned = true
	return nil, nil
}

// A task without an id of its own is named by its kind and start, the same from call to call, and two such tasks
// that would share one stay apart.
func TestATaskWithoutAnIDKeepsOne(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	newStore(t)
	at := time.UnixMilli(1_700_000_000_000)
	onMachine(t, func() []sessions.Source {
		return []sessions.Source{fixed{{Key: "s", Tool: "claude", Name: "jobs", Tasks: []sessions.Task{
			{Kind: "bash", Label: "one", Started: at}, {Kind: "bash", Label: "two", Started: at}}}}}
	})
	want := "task:s/bash@1700000000000 task:s/bash@1700000000000#2"
	for range 2 {
		if r, _ := reportJSON(t); ids(r.Running) != want {
			t.Fatalf("running %s, want %s", ids(r.Running), want)
		}
	}
}

// momentum sessions and evidence events show what agents and tools wrote as plain text too.
func TestTheBoardAndTheEventListArePlainText(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	var out bytes.Buffer
	ss := []*sessions.Session{{Key: "w", Tool: "claude", Name: "fix \x1b[2Jit\u202e", Phase: sessions.Working, Ask: "ok?\x07",
		Tasks: []sessions.Task{{Kind: "bash", Label: "run \x1b]0;title\x07"}}}}
	if err := printBoard(&out, ss, time.Now(), false, false); err != nil || strings.ContainsAny(out.String(), "\x1b\x07\u202e") {
		t.Fatalf("board %q: %v", out.String(), err)
	}
	putEvents(t, store.Event{Source: "mistakes", SourceKey: "k", TS: time.Now(), Actor: store.ActorTool, Kind: "tool-error", Tool: "Ba\x1bsh",
		Summary: "exit 2 \x1b[31mred\x07\u202e"})
	code, list, _ := momentum("evidence", "events", "--since", "1d")
	if code != 0 || !strings.Contains(list, "exit 2 [31mred") || strings.ContainsAny(list, "\x1b\x07\u202e") {
		t.Fatalf("events: exit %d %q", code, list)
	}
}

// A working session is dated when its working stretch began, as the last sessions run saved it, not when the report
// looked; a task, when it started.
func TestAWorkingSessionIsDatedWhenItsStretchBegan(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	t0 := time.Now().Add(-5 * time.Minute).Truncate(time.Second)
	src := fixed{{Key: "w", Tool: "claude", Name: "work", TurnOpen: true}}
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	if _, scanErr, err := board(context.Background(), &sessions.World{Now: t0}, nil, st, "test", []sessions.Source{src}); err != nil || scanErr != nil {
		t.Fatal(err, scanErr)
	}
	st.Close()
	onMachine(t, func() []sessions.Source { return []sessions.Source{src} })
	r, _ := reportJSON(t)
	if at, err := time.Parse(time.RFC3339, r.Running[0].At); err != nil || !at.Equal(t0) {
		t.Fatalf("a working session at %s, want %s", r.Running[0].At, rfc3339(t0))
	}
}

// What agents write reaches the report as plain text: no escape sequence, bell or bidi override in a title or a
// detail, and a label with nothing to show leaves the task its kind and id.
func TestRunningIsPlainText(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	newStore(t)
	onMachine(t, func() []sessions.Source {
		return []sessions.Source{fixed{{Key: "w", Tool: "claude", Name: "fix \x1b[2Jthe\x07 \u202eparser", Project: "a\u200bb", TurnOpen: true,
			Tasks: []sessions.Task{{ID: "bg1", Kind: "bash", Label: "\x1b\x07\u2066"}}}}}
	})
	r, raw := reportJSON(t)
	if strings.Contains(raw, `\u001b`) || strings.Contains(raw, `\u0007`) || strings.ContainsRune(raw, '\u202e') || strings.ContainsRune(raw, '\u200b') {
		t.Fatalf("raw: %s", raw)
	}
	want := []string{"fix [2Jthe parser", "Claude Code session in ab, working, with 1 background job.",
		"Background job bg1", `Background job of the Claude Code session "fix [2Jthe parser" in ab.`}
	if got := []string{r.Running[0].Title, r.Running[0].Detail, r.Running[1].Title, r.Running[1].Detail}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("running:\n%q\nwant\n%q", got, want)
	}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"report"}, &out, &errOut); code != 0 || strings.ContainsAny(out.String(), "\x1b\x07") {
		t.Fatalf("the terminal form: exit %d %q", code, out.String())
	}
}

// The report reads only the project names from sources.json: a file it cannot read is named on stderr, and the
// report still prints.
func TestABrokenSourcesFileDoesNotStopTheReport(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", cfg)
	if err := os.WriteFile(filepath.Join(cfg, "sources.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	putNotes(t, store.Note{ID: "ask", Kind: store.NoteNeeds, Title: "Merge it?"})
	onMachine(t, func() []sessions.Source { return []sessions.Source{fixed{}} })
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"report", "--json"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), `"id":"ask"`) ||
		!strings.Contains(errOut.String(), "sources.json") {
		t.Fatalf("exit %d: %s %s", code, out.String(), errOut.String())
	}
}

func TestReportTakesNoArguments(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"report", "extra"}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
}
