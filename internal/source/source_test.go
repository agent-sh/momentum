package source

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func all(t *testing.T, st *store.Store, src string) map[string]store.Event {
	t.Helper()
	evs, err := st.Events(context.Background(), store.Query{Since: time.Unix(0, 0), Source: src, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.Event{}
	for _, e := range evs {
		out[e.SourceKey] = e
	}
	return out
}

func byKind(t *testing.T, st *store.Store, src string) map[string]store.Event {
	t.Helper()
	out := map[string]store.Event{}
	for _, e := range all(t, st, src) {
		out[e.Kind+"/"+e.Agent] = e
	}
	return out
}

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func ingest(t *testing.T, s Source, st *store.Store) Result {
	t.Helper()
	r, err := s.Ingest(context.Background(), st)
	if err != nil {
		t.Fatalf("%s: %v", s.Name(), err)
	}
	return r
}

func cursor(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	c, err := st.Cursor(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Synthetic rows in the shapes the mistakes mod writes today.
const (
	rowErr        = `{"ts":"2026-01-02T10:00:00","session":"s1","kind":"tool-error","tool":"Bash","input":"x","detail":"Exit code 1\nmore"}` + "\n"
	rowDeny       = `{"ts":"2026-01-02T10:01:00","session":"s1","kind":"deny","tool":"Bash","detail":"guard: no"}` + "\n"
	rowFix        = `{"ts":"2026-01-02T10:02:00","session":"s1","kind":"correction","heuristic":true,"input":"no, use the staging config","detail":"I deployed the change to production."}` + "\n"
	rowStop       = `{"ts":"2026-01-02T10:03:00","session":"s1","kind":"interrupt","detail":"after 41s"}` + "\n"
	rowStopSub    = `{"ts":"2026-01-02T10:04:00","session":"s1","kind":"interrupt","detail":"after 7s","agentId":"a1"}` + "\n"
	rowClassifier = `{"ts":"2026-01-02T10:05:00","session":"s1","kind":"tool-error","tool":"Bash","cause":"classifier","detail":"denied by the classifier"}` + "\n"
	rowTurn       = `{"ts":"2026-01-02T10:06:00","session":"s1","kind":"turn-error"}` + "\n"
	rowNew        = `{"ts":"2026-01-02T10:07:00","session":"s1","kind":"brand-new-kind","detail":"?"}` + "\n"
	rowOffset     = `{"ts":"2026-07-01T09:00:00+03:00","session":"s1","kind":"tool-error","tool":"Read","detail":"no such file"}` + "\n"
)

func jerusalem(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Jerusalem")
	if err != nil {
		t.Skip("no zoneinfo:", err)
	}
	return loc
}

func TestMistakesReadsCompleteLinesOnceAndSavesTheOffset(t *testing.T) {
	st := openStore(t)
	log := filepath.Join(t.TempDir(), "events.jsonl")
	appendFile(t, log, rowErr+rowDeny+rowFix[:30]) // the last row is still being written
	m := Mistakes{Path: log, Loc: jerusalem(t)}
	if r := ingest(t, m, st); r.New != 2 || r.Read != 2 {
		t.Fatalf("first run %+v, want 2 new of 2 read", r)
	}
	c, ok := parseLogCursor(cursor(t, st, "mistakes"))
	if !ok || c.offset != int64(len(rowErr+rowDeny)) {
		t.Fatalf("cursor %+v, want the offset after the two complete lines", c)
	}
	appendFile(t, log, rowFix[30:])
	if r := ingest(t, m, st); r.New != 1 || r.Read != 1 {
		t.Fatalf("second run %+v, want only the completed line read", r)
	}
	if r := ingest(t, m, st); r.Read != 0 {
		t.Fatalf("third run read %d lines, want 0", r.Read)
	}
}

func TestMistakesAttributesByKindAgentAndCause(t *testing.T) {
	st := openStore(t)
	log := filepath.Join(t.TempDir(), "events.jsonl")
	appendFile(t, log, rowErr+rowDeny+rowFix+rowStop+rowStopSub+rowClassifier+rowTurn+rowNew)
	r := ingest(t, Mistakes{Path: log, Loc: jerusalem(t)}, st)
	if len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "brand-new-kind") {
		t.Fatalf("an unknown kind must be reported: %+v", r.Notes)
	}
	got := byKind(t, st, "mistakes")
	fix := got["correction/"]
	if fix.Actor != store.ActorOwner || fix.Summary != "no, use the staging config" || !fix.Heuristic {
		t.Fatalf("a correction keeps only what the owner typed: %+v", fix)
	}
	if strings.Contains(fix.Summary+fix.Meta, "production") {
		t.Fatalf("the agent's previous answer leaked into the owner's event: %+v", fix)
	}
	if e := got["interrupt/"]; e.Actor != store.ActorOwner || e.Summary != "" || !strings.Contains(e.Meta, "41s") {
		t.Fatalf("a main-session interrupt is the owner's, with the harness text in meta: %+v", e)
	}
	if e := got["interrupt/a1"]; e.Actor != store.ActorHarness {
		t.Fatalf("a subagent's interrupt cannot be credited to the owner: %+v", e)
	}
	want := map[string]store.Actor{"deny/": store.ActorHarness, "turn-error/": store.ActorHarness, "brand-new-kind/": store.ActorUnknown}
	for k, a := range want {
		if got[k].Actor != a {
			t.Fatalf("%s: actor %q, want %q", k, got[k].Actor, a)
		}
	}
	for _, e := range got {
		if e.Kind == "tool-error" && strings.Contains(e.Meta, "classifier") && e.Actor != store.ActorHarness {
			t.Fatalf("a classifier refusal is a deny, not a tool outcome: %+v", e)
		}
		if e.Kind == "tool-error" && !strings.Contains(e.Meta, "classifier") && (e.Actor != store.ActorTool || strings.Contains(e.Summary, "more")) {
			t.Fatalf("a tool error keeps the first line of its detail, as a tool outcome: %+v", e)
		}
	}
}

func TestMistakesTimesWithAndWithoutAnOffset(t *testing.T) {
	st := openStore(t)
	log := filepath.Join(t.TempDir(), "events.jsonl")
	appendFile(t, log, rowErr+rowOffset)
	ingest(t, Mistakes{Path: log, Loc: jerusalem(t)}, st)
	got := map[string]time.Time{}
	for _, e := range all(t, st, "mistakes") {
		got[e.Tool] = e.TS.UTC()
	}
	if want := time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC); !got["Bash"].Equal(want) { // naive, Jerusalem winter UTC+2
		t.Fatalf("naive time = %v, want %v", got["Bash"], want)
	}
	if want := time.Date(2026, 7, 1, 6, 0, 0, 0, time.UTC); !got["Read"].Equal(want) {
		t.Fatalf("time with an offset = %v, want %v", got["Read"], want)
	}
}

func TestMistakesRecordsUnparsedLinesAndKeepsGoing(t *testing.T) {
	ctx, st := context.Background(), openStore(t)
	log := filepath.Join(t.TempDir(), "events.jsonl")
	appendFile(t, log, rowErr+"{not json\n"+`{"ts":"yesterday","kind":"deny"}`+"\n"+rowDeny)
	r := ingest(t, Mistakes{Path: log}, st)
	if r.New != 2 || r.Skipped != 2 || r.Read != 4 {
		t.Fatalf("result %+v, want 2 stored and 2 recorded as unparsed", r)
	}
	if n, _ := st.Skipped(ctx, "mistakes"); n != 2 {
		t.Fatalf("store skipped = %d, want 2", n)
	}
}

func TestMistakesRereadsARotatedLogWithoutDuplicates(t *testing.T) {
	st := openStore(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "events.jsonl")
	appendFile(t, log, rowErr+rowDeny)
	m := Mistakes{Path: log}
	ingest(t, m, st)
	if err := os.Rename(log, filepath.Join(dir, "old.jsonl")); err != nil {
		t.Fatal(err)
	}
	// the new file is longer than the old offset and starts with an old row
	appendFile(t, log, rowDeny+rowFix+rowStop+rowTurn)
	if r := ingest(t, m, st); r.New != 3 || r.Read != 4 {
		t.Fatalf("after rotation %+v, want all 4 read and the 3 new stored", r)
	}
}

func TestMistakesRereadsALogTruncatedInPlaceThatGrewPastTheOffset(t *testing.T) {
	st := openStore(t)
	log := filepath.Join(t.TempDir(), "events.jsonl")
	appendFile(t, log, rowErr+rowDeny)
	m := Mistakes{Path: log}
	ingest(t, m, st)
	if err := os.Truncate(log, 0); err != nil { // same inode, like copytruncate or `: > log`
		t.Fatal(err)
	}
	appendFile(t, log, rowFix+rowStop+rowStopSub+rowTurn)
	if r := ingest(t, m, st); r.New != 4 || r.Read != 4 {
		t.Fatalf("after truncation %+v, want the whole new content read", r)
	}
}

func TestMistakesMissingLogIsNotAnError(t *testing.T) {
	if r := ingest(t, Mistakes{Path: filepath.Join(t.TempDir(), "none.jsonl")}, openStore(t)); r.Read != 0 {
		t.Fatalf("%+v", r)
	}
}

func radarDB(t *testing.T, withFeedback bool) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "radar.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	schema := `CREATE TABLE findings (id TEXT PRIMARY KEY, batch_id TEXT, ts REAL NOT NULL, type TEXT NOT NULL, key TEXT NOT NULL,
	  title TEXT NOT NULL, why TEXT, evidence TEXT, state TEXT NOT NULL DEFAULT 'new', vote INTEGER, spawned INTEGER, updated REAL NOT NULL);`
	if withFeedback {
		schema += `CREATE TABLE feedback (id INTEGER PRIMARY KEY AUTOINCREMENT, finding_id TEXT NOT NULL, ts REAL NOT NULL, kind TEXT NOT NULL, text TEXT, meta TEXT);`
	}
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return path, db
}

func TestRadarReadsOnlyFindingsPastTheCursorWithAMargin(t *testing.T) {
	st := openStore(t)
	path, db := radarDB(t, false)
	if _, err := db.Exec(`INSERT INTO findings (id, ts, type, key, title, state, updated) VALUES
	  ('f-1', 100, 'infra', 'infra:a', 'disk fills up', 'new', 100), ('f-2', 1000, 'research', 'research:b', 'a paper', 'new', 1000)`); err != nil {
		t.Fatal(err)
	}
	r := Radar{Path: path}
	if res := ingest(t, r, st); res.New != 2 || res.Read != 2 {
		t.Fatalf("first run %+v", res)
	}
	if res := ingest(t, r, st); res.Read != 1 {
		t.Fatalf("second run read %d findings, want only the one inside the margin", res.Read)
	}
	// a commit that lands late, with an updated time just under the cursor
	if _, err := db.Exec(`INSERT INTO findings (id, ts, type, key, title, updated) VALUES ('f-3', 990, 'infra', 'infra:c', 'late', 990)`); err != nil {
		t.Fatal(err)
	}
	if res := ingest(t, r, st); res.New != 1 {
		t.Fatalf("a late commit inside the margin was missed: %+v", res)
	}
	if e := all(t, st, "radar")["finding:f-1"]; e.Actor != store.ActorDetector || e.Summary != "[infra] disk fills up" {
		t.Fatalf("a finding keeps what the detector wrote, not its state: %+v", e)
	}
}

func TestRadarFeedbackIsItsOwnEventWithItsActorAndTestTag(t *testing.T) {
	st := openStore(t)
	path, db := radarDB(t, true)
	if _, err := db.Exec(`INSERT INTO findings (id, ts, type, key, title, updated) VALUES ('f-1', 100, 'infra', 'infra:a', 'disk', 100);
	  INSERT INTO feedback (finding_id, ts, kind, text, meta) VALUES
	    ('f-1', 200, 'feedback', 'too generic, show the unit name', '{"device": "phone"}'),
	    ('f-1', 201, 'dismiss', NULL, '{"device": "radar-check"}'),
	    ('f-1', 202, 'dismiss', 'relayed by an agent', NULL);`); err != nil {
		t.Fatal(err)
	}
	r := Radar{Path: path, TestDevice: "radar-check"}
	if res := ingest(t, r, st); res.New != 4 {
		t.Fatalf("first run %+v, want the finding and three feedback events", res)
	}
	fb := all(t, st, "radar-feedback")
	if e := fb["1"]; e.Actor != store.ActorOwner || e.Kind != "feedback:feedback" || e.Summary != "too generic, show the unit name" || !e.TS.Equal(time.Unix(200, 0)) {
		t.Fatalf("a phone-page note is the owner's, at the time he wrote it: %+v", e)
	}
	if e := fb["2"]; e.Experiment != "radar-check" {
		t.Fatalf("the verification device's action must be tagged as a test: %+v", e)
	}
	if e := fb["3"]; e.Actor != store.ActorAgent || e.Experiment != "" {
		t.Fatalf("an action without a device came from the CLI, an agent's: %+v", e)
	}
	if res := ingest(t, r, st); res.Read != 1 { // only the finding inside the margin
		t.Fatalf("second run read %d records, want feedback past its id cursor only", res.Read)
	}
}

func TestRadarWithoutAFindingsTableIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "radar.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE other (x)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := (Radar{Path: path}).Ingest(context.Background(), openStore(t)); err == nil {
		t.Fatal("a radar store without findings must be reported")
	}
}

func writeIncident(t *testing.T, dir, sub, id, status, lastSeen string, mtime time.Time) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, sub, id+".json")
	raw, _ := json.Marshal(map[string]any{"incident_id": id, "status": status, "summary": "a failure", "last_seen": lastSeen, "repeat_count": 3})
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestIncidentsParseOnlyNewOrChangedFiles(t *testing.T) {
	st := openStore(t)
	dir := t.TempDir()
	t0 := time.Now().Add(-time.Hour)
	writeIncident(t, dir, "open", "inc-1", "reported", "2026-01-02T10:00:00+02:00", t0)
	writeIncident(t, dir, "closed", "inc-2", "closed", "2026-01-02T10:00:00+02:00", t0)
	inc := Incidents{Dir: dir}
	if r := ingest(t, inc, st); r.New != 4 || r.Read != 2 { // two incidents, and a first sighting of each
		t.Fatalf("first run %+v", r)
	}
	if r := ingest(t, inc, st); r.Read != 0 {
		t.Fatalf("unchanged files were parsed again: %+v", r)
	}
	writeIncident(t, dir, "open", "inc-1", "resolved-now", "2026-01-02T11:00:00+02:00", t0) // same mtime, new size
	// a file that arrives late with an old mtime (mv, cp -p, rsync -a, a restore)
	writeIncident(t, dir, "open", "inc-3", "new", "2026-01-01T09:00:00+02:00", t0.Add(-24*time.Hour))
	if r := ingest(t, inc, st); r.Read != 2 || r.New != 3 { // inc-1's new sighting; inc-3 and its first
		t.Fatalf("third run %+v, want the changed file and the late file", r)
	}
	got := all(t, st, "incidents")
	if e := got["inc-1"]; !strings.Contains(e.Meta, "resolved-now") || !e.TS.Equal(time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("a changed incident must be refreshed with its offset time: %+v", e)
	}
}

func TestIncidentsRetryAFileThatWasUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	st := openStore(t)
	dir := t.TempDir()
	p := writeIncident(t, dir, "open", "inc-1", "new", "2026-01-02T10:00:00+02:00", time.Now())
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	inc := Incidents{Dir: dir}
	if r := ingest(t, inc, st); r.Read != 0 || len(r.Notes) != 1 {
		t.Fatalf("an unreadable file must be reported: %+v", r)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := ingest(t, inc, st); r.New != 2 || r.Read != 1 { // the incident and its first sighting
		t.Fatalf("the file was not retried: %+v", r)
	}
}

type failing struct{}

func (failing) Name() string { return "broken" }
func (failing) Ingest(context.Context, *store.Store) (Result, error) {
	return Result{}, os.ErrPermission
}

func TestRunAllKeepsGoingPastAFailingSource(t *testing.T) {
	st := openStore(t)
	log := filepath.Join(t.TempDir(), "events.jsonl")
	appendFile(t, log, rowErr)
	out := RunAll(context.Background(), st, []Source{failing{}, Mistakes{Path: log}})
	if out[0].Err == nil || out[1].Err != nil || out[1].New != 1 {
		t.Fatalf("outcomes %+v", out)
	}
}

// Each change of an incident's last sighting or count of repeats is a sighting of its own, keyed by both, with the
// repeats since the one before: the incident's one event says only where it stands. A file read again with
// nothing new adds none, and neither does a change of anything else, nor a time the file writes to the
// microsecond; a count that moves while the time stays is a sighting; a count that started again counts all of
// it. A first sighting says so, and when the incident was first seen.
func TestAnIncidentsSightingsAreEvents(t *testing.T) {
	st := openStore(t)
	dir := t.TempDir()
	at := time.Now().Add(-time.Hour)
	n := 0
	write := func(f map[string]any) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, "open"), 0o700); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "open", "inc-9.json")
		f["incident_id"], f["status"], f["summary"] = "inc-9", "reported", "a network drops"
		raw, _ := json.Marshal(f)
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		n++
		if err := os.Chtimes(p, at.Add(time.Duration(n)*time.Minute), at.Add(time.Duration(n)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	inc := Incidents{Dir: dir}
	for _, f := range []map[string]any{
		{"last_seen": "2026-01-02T10:00:00Z", "repeat_count": 12, "first_seen": "2026-01-01T08:00:00Z", "created_at": "2026-01-01T09:00:00Z"},
		{"last_seen": "2026-01-02T11:00:00Z", "repeat_count": 30},
		{"last_seen": "2026-01-02T11:00:00Z", "repeat_count": 30, "attempts": 2}, // a change of nothing it counts
		{"last_seen": "2026-01-02T12:00:00.391086+00:00", "repeat_count": 35},
		{"last_seen": "2026-01-02T12:00:00.391086+00:00", "repeat_count": 35, "attempts": 3}, // the same, to the microsecond
		{"last_seen": "2026-01-02T12:00:00.391086+00:00", "repeat_count": 38},                // the count alone moves
		{"last_seen": "2026-01-02T12:00:00.391086+00:00", "repeat_count": 41},
		{"last_seen": "2026-01-02T13:00:00Z", "repeat_count": 4}, // the detector started its count again
	} {
		write(f)
		ingest(t, inc, st)
	}
	var news []string
	for k, e := range all(t, st, "incidents") {
		if e.Kind != "incident-seen" {
			continue
		}
		var m struct {
			New, Repeat int
			Incident    string
			First       bool
			FirstSeen   string `json:"first_seen"`
		}
		json.Unmarshal([]byte(e.Meta), &m)
		line := fmt.Sprintf("%s %d/%d %s", strings.TrimPrefix(k, "inc-9@"), m.New, m.Repeat, m.Incident)
		if m.First {
			line += " first " + m.FirstSeen
		}
		news = append(news, line)
	}
	sort.Strings(news)
	want := []string{"2026-01-02T10:00:00Z#12 12/12 inc-9 first 2026-01-01T08:00:00Z", "2026-01-02T11:00:00Z#30 18/30 inc-9",
		"2026-01-02T12:00:00.391086Z#35 5/35 inc-9", "2026-01-02T12:00:00.391086Z#38 3/38 inc-9", "2026-01-02T12:00:00.391086Z#41 3/41 inc-9",
		"2026-01-02T13:00:00Z#4 4/4 inc-9"}
	if !slices.Equal(news, want) {
		t.Fatalf("sightings\n%s\nwant\n%s", strings.Join(news, "\n"), strings.Join(want, "\n"))
	}
	if e := all(t, st, "incidents")["inc-9"]; !strings.Contains(e.Meta, `"repeat":4`) {
		t.Fatalf("the incident %+v", e)
	}
	// a first sighting of no repeats counts itself; one with no first sighting of its own says when it was made
	st2 := openStore(t)
	dir2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir2, "open"), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"incident_id": "inc-1", "status": "new", "summary": "x", "last_seen": "2026-01-02T10:00:00Z",
		"created_at": "2026-01-01T09:00:00Z", "repeat_count": 0})
	if err := os.WriteFile(filepath.Join(dir2, "open", "inc-1.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ingest(t, Incidents{Dir: dir2}, st2)
	if e := all(t, st2, "incidents")["inc-1@2026-01-02T10:00:00Z#0"]; !strings.Contains(e.Meta, `"new":1`) || !strings.Contains(e.Meta, `"first_seen":"2026-01-01T09:00:00Z"`) {
		t.Fatalf("a first sighting of none: %+v", e)
	}
}

// A source-wide successful read is saved even with no events. A failed read cannot advance it.
func TestOnlyACompleteIngestAdvancesItsReadTime(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	src := readFixture{}
	before := time.Now().Add(-time.Hour)
	if err := st.MarkRead(ctx, src.Name(), before); err != nil {
		t.Fatal(err)
	}
	out := RunAll(ctx, st, []Source{src})
	if len(out) != 1 || out[0].Err != nil {
		t.Fatalf("complete: %+v", out)
	}
	read, err := st.ReadThrough(ctx, src.Name())
	if err != nil || !read.After(before) {
		t.Fatalf("read time: %v %v", read, err)
	}
	src.fail = true
	out = RunAll(ctx, st, []Source{src})
	if len(out) != 1 || out[0].Err == nil {
		t.Fatalf("failed: %+v", out)
	}
	after, err := st.ReadThrough(ctx, src.Name())
	if err != nil || !after.Equal(read) {
		t.Fatalf("failed read advanced: %v %v", after, err)
	}
}

type readFixture struct{ fail bool }

func (readFixture) Name() string { return "synthetic" }
func (r readFixture) Ingest(context.Context, *store.Store) (Result, error) {
	if r.fail {
		return Result{}, fmt.Errorf("read failed")
	}
	return Result{}, nil
}
