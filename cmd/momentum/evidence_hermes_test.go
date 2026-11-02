package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/source"
	"github.com/agent-sh/momentum/internal/store"
)

// hermesMessage is one row of a synthetic Hermes messages table; a nil content or time is NULL.
type hermesMessage struct {
	Session string
	Content any
	Calls   any
	At      any
}

// hermesStore makes a synthetic Hermes messages table at dir/state.db, row ids from 1, and returns its path.
func hermesStore(t *testing.T, dir string, rows ...hermesMessage) string {
	t.Helper()
	db := filepath.Join(dir, "state.db")
	h, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: db}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.Exec(`CREATE TABLE messages (id INTEGER PRIMARY KEY, session_id TEXT, role TEXT, content TEXT, tool_calls TEXT, tool_name TEXT, timestamp REAL)`); err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		if _, err := h.Exec(`INSERT INTO messages VALUES (?, ?, 'assistant', ?, ?, NULL, ?)`, i+1, r.Session, r.Content, r.Calls, r.At); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func hermesTS(at time.Time) float64 { return float64(at.UnixMilli()) / 1000 }

// hermesEvent is the event the ingest stores for row id of the store at db: the row's place, and a digest of the row's
// content and calls as the ingest read them.
func hermesEvent(db string, id int, session string, at time.Time, content, calls string) store.Event {
	digest, _ := json.Marshal(map[string]string{source.HermesDigestKey: source.HermesRowDigest(content, calls)})
	return store.Event{Source: store.Transcripts, SourceKey: "h" + strconv.Itoa(id), TS: at, Session: session, Harness: "hermes", Actor: store.ActorAgent,
		Kind: "agent_text", Summary: "x", Meta: string(digest), Ref: fmt.Sprintf("%s@%d#0", db, id)}
}

func hermesHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	return home
}

// trialEnds puts the command in a trial whose clock stands at end.
func trialEnds(t *testing.T, end time.Time) {
	t.Helper()
	t.Setenv("MOMENTUM_NOW", end.Format(time.RFC3339Nano))
}

// A Hermes event's record is its message row.
func TestEvidenceShowReadsAHermesEventsMessageRow(t *testing.T) {
	home := hermesHome(t)
	fakeScrubber(t)
	at := time.Now().Add(-time.Hour)
	db := hermesStore(t, home, hermesMessage{"h1", "the gateway kept retrying the dead host", nil, hermesTS(at)})
	e := hermesEvent(db, 1, "h1", at, "the gateway kept retrying the dead host", "")
	putEvents(t, e)
	code, out, errOut := momentum("evidence", "show", e.ID())
	if code != 0 || !strings.Contains(out, "the gateway kept retrying the dead host") || !strings.Contains(out, `"session_id":"h1"`) || strings.Contains(out, "rewritten") {
		t.Errorf("a Hermes row: exit %d %q %s", code, out, errOut)
	}
}

// A Hermes row reaches the scrubber as an agent wrote it: no HTML escapes, and each call's arguments one level deep
// as the JSON they are, so a secret the scrubber masks as written is masked there too.
func TestAHermesRowReachesTheScrubberAsWritten(t *testing.T) {
	home := hermesHome(t)
	// it masks a URL's password with its & as written, and a quoted value one JSON level deep, as in a transcript's line
	scrubbing(t, `s/pa&ss-value/**********/g; s/\\"zz-quoted-value\\"/\\"**********\\"/g`)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	text := "cloning: https://builder:pa&ss-value@example.com/repo.git"
	calls := `[{"id":"c1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"echo db_password = \\\"zz-quoted-value\\\"\"}"}}]`
	db := hermesStore(t, home, hermesMessage{"h1", text, nil, hermesTS(at)}, hermesMessage{"h1", "", calls, hermesTS(at)})
	a, b := hermesEvent(db, 1, "h1", at, text, ""), hermesEvent(db, 2, "h1", at, "", calls)
	putEvents(t, a, b)
	code, out, diag := momentum("evidence", "show", a.ID(), b.ID())
	if code != 0 || strings.Contains(out, "pa&ss-value") || strings.Contains(out, `&`) || strings.Contains(out, "zz-quoted-value") || strings.Count(out, "**********") != 2 {
		t.Fatalf("exit %d %q %s", code, out, diag)
	}
}

// A Hermes row is the event's own only while it is of the event's session at the event's time: a store made again
// starts its ids over, and the row then at an old event's id is another's. In a trial, a row is shown before the end
// and withheld from it on, and a row at the end itself is withheld.
func TestAHermesRowIsTheEventsOwnAndDatedAsOfTheEnd(t *testing.T) {
	home := hermesHome(t)
	fakeScrubber(t)
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Hour)
	before, after := end.Add(-time.Hour), end.Add(time.Hour)
	db := hermesStore(t, home,
		hermesMessage{"h2", "a later conversation about something else", nil, hermesTS(before)},
		hermesMessage{"h1", "the gateway kept retrying the dead host", nil, hermesTS(before)},
		hermesMessage{"h1", "after the end: retry the host first", nil, hermesTS(after)},
		hermesMessage{"h1", "at the end itself: retry the host first", nil, hermesTS(end)},
		hermesMessage{"h1", "a store restored from a backup holds this", nil, hermesTS(before.Add(-time.Hour))})
	other := hermesEvent(db, 1, "h1", before, "a later conversation about something else", "")
	own := hermesEvent(db, 2, "h1", before, "the gateway kept retrying the dead host", "")
	late := hermesEvent(db, 3, "h1", after, "after the end: retry the host first", "")
	atEnd := hermesEvent(db, 4, "h1", end, "at the end itself: retry the host first", "")
	elsewhen := hermesEvent(db, 5, "h1", before, "a store restored from a backup holds this", "") // the row is an hour older than its event
	putEvents(t, other, own, late, atEnd, elsewhen)
	if _, out, _ := momentum("evidence", "show", other.ID()); strings.Contains(out, "something else") || !strings.Contains(out, "another's") {
		t.Fatalf("another session's row: %q", out)
	}
	if _, out, _ := momentum("evidence", "show", elsewhen.ID()); strings.Contains(out, "restored from a backup") || !strings.Contains(out, "another's") {
		t.Fatalf("a row of the session at another time: %q", out)
	}
	trialEnds(t, end)
	_, out, _ := momentum("evidence", "show", own.ID(), late.ID(), atEnd.ID())
	if !strings.Contains(out, "kept retrying the dead host") || strings.Contains(out, "retry the host first") || strings.Count(out, "withheld") != 2 {
		t.Fatalf("a trial's rows: %q", out)
	}
}

// A subagent's row is its event's own: the event names the delegation in Agent, and its row is of that session.
func TestAHermesDelegationsRowIsTheEventsOwn(t *testing.T) {
	home := hermesHome(t)
	fakeScrubber(t)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	db := hermesStore(t, home, hermesMessage{"h1-sub", "the subagent's finding about the lock", nil, hermesTS(at)})
	e := hermesEvent(db, 1, "h1", at, "the subagent's finding about the lock", "")
	e.Agent = "h1-sub"
	putEvents(t, e)
	if code, out, diag := momentum("evidence", "show", e.ID()); code != 0 || !strings.Contains(out, "the subagent's finding about the lock") {
		t.Fatalf("exit %d %q %s", code, out, diag)
	}
}

// A row of no session is no event's: an event of no delegation does not take an empty session_id for its own.
func TestAHermesRowOfNoSessionIsNoEventsOwn(t *testing.T) {
	home := hermesHome(t)
	fakeScrubber(t)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	db := hermesStore(t, home, hermesMessage{"", "a row of no session", nil, hermesTS(at)})
	e := hermesEvent(db, 1, "h1", at, "a row of no session", "")
	putEvents(t, e)
	if code, out, diag := momentum("evidence", "show", e.ID()); code != 0 || strings.Contains(out, "a row of no session") || !strings.Contains(out, "another's") {
		t.Fatalf("exit %d %q %s", code, out, diag)
	}
}

// Hermes rewrites a message row in place (a turn's prologue rewrites the prompt it stored, a prune rewrites old rows),
// so a trial reads a row only in the version the ingest read: the event keeps a digest of the row's content and
// calls, and a row whose digest has changed is withheld. Live, the row stands as it is, with a note that it was
// rewritten; an event with no digest (an older ingest's) has its row withheld in a trial, which cannot tell.
func TestAHermesRowRewrittenSinceItWasReadIsWithheldInATrial(t *testing.T) {
	home := hermesHome(t)
	fakeScrubber(t)
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Hour)
	before := end.Add(-time.Hour)
	calls := `[{"id":"c1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"ls\"}"}}]`
	db := hermesStore(t, home,
		hermesMessage{"h1", "the version at the end: the gateway kept retrying", nil, hermesTS(before)},
		hermesMessage{"h1", "the second row is not rewritten", nil, hermesTS(before)},
		hermesMessage{"h1", "", calls, hermesTS(before)},
		hermesMessage{"h1", "a row an older ingest read", nil, hermesTS(before)})
	rewritten := hermesEvent(db, 1, "h1", before, "the version at the end: the gateway kept retrying", "")
	kept := hermesEvent(db, 2, "h1", before, "the second row is not rewritten", "")
	called := hermesEvent(db, 3, "h1", before, "", calls)
	older := hermesEvent(db, 4, "h1", before, "a row an older ingest read", "")
	older.Meta = "" // no digest was kept
	putEvents(t, rewritten, kept, called, older)

	h, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: db}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, q := range []string{`UPDATE messages SET content = 'rewritten after the end: alpha-later-text' WHERE id = 1`,
		`UPDATE messages SET tool_calls = '[{"id":"c1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"rm -r later\"}"}}]' WHERE id = 3`} {
		if _, err := h.Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	_, out, _ := momentum("evidence", "show", rewritten.ID(), called.ID(), older.ID())
	if !strings.Contains(out, "alpha-later-text") || !strings.Contains(out, "rm -r later") || strings.Count(out, "the row was rewritten since it was read: this is its text now") != 2 ||
		!strings.Contains(out, "a row an older ingest read") {
		t.Errorf("live, the rows stand as they are: %q", out)
	}

	trialEnds(t, end)
	code, out, diag := momentum("evidence", "show", rewritten.ID(), kept.ID(), called.ID(), older.ID())
	if code != 0 || strings.Contains(out, "alpha-later-text") || strings.Contains(out, "rm -r later") || strings.Contains(out, "a row an older ingest read") ||
		strings.Count(out, "(record withheld: the row was rewritten since it was read)") != 2 ||
		strings.Count(out, "(record withheld: no digest of the row was kept when it was read") != 1 || !strings.Contains(out, "the second row is not rewritten") {
		t.Fatalf("a trial's rows: exit %d %q %s", code, out, diag)
	}
}

// A Hermes row whose timestamp is no time (not a number, past any date, before 1970) is withheld, before any
// conversion that could wrap it under a trial's end, and the call's other ids are still shown.
func TestAHermesRowDatedNoTimeIsWithheld(t *testing.T) {
	home := hermesHome(t)
	fakeScrubber(t)
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Hour)
	before := end.Add(-time.Hour)
	db := hermesStore(t, home, hermesMessage{"h1", "a row of 1e19 seconds", nil, 1e19}, hermesMessage{"h1", "a row of infinite time", nil, math.Inf(1)},
		hermesMessage{"h1", "a row before the end", nil, hermesTS(before)}, hermesMessage{"h1", "a row before 1970", nil, -5.0})
	far := hermesEvent(db, 1, "h1", before, "a row of 1e19 seconds", "")
	inf := hermesEvent(db, 2, "h1", before, "a row of infinite time", "")
	own := hermesEvent(db, 3, "h1", before, "a row before the end", "")
	neg := hermesEvent(db, 4, "h1", before, "a row before 1970", "")
	putEvents(t, far, inf, own, neg)
	trialEnds(t, end)
	code, out, diag := momentum("evidence", "show", far.ID(), inf.ID(), own.ID(), neg.ID())
	if code != 0 || strings.Contains(out, "1e19 seconds") || strings.Contains(out, "infinite time") || strings.Contains(out, "before 1970") ||
		!strings.Contains(out, "a row before the end") || strings.Count(out, "its timestamp is no time") != 3 {
		t.Fatalf("exit %d %q %s", code, out, diag)
	}
}

// A row over the bound is withheld without being read whole.
func TestAHermesRowOverTheBoundIsWithheld(t *testing.T) {
	home := hermesHome(t)
	fakeScrubber(t)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	big := strings.Repeat("x", readMax+1)
	db := hermesStore(t, home, hermesMessage{"h1", big, nil, hermesTS(at)}, hermesMessage{"h1", strings.Repeat("y", readMax/2), strings.Repeat("z", readMax/2), hermesTS(at)})
	over := hermesEvent(db, 1, "h1", at, big, "")
	within := hermesEvent(db, 2, "h1", at, strings.Repeat("y", readMax/2), strings.Repeat("z", readMax/2))
	putEvents(t, over, within)
	if code, out, diag := momentum("evidence", "show", over.ID()); code != 0 || strings.Contains(out, "xxxx") || !strings.Contains(out, "oversized record withheld") {
		t.Errorf("a row over the bound: exit %d %d bytes %s", code, len(out), diag)
	}
	if code, out, diag := momentum("evidence", "show", within.ID()); code != 0 || !strings.Contains(out, "yyyy") || strings.Contains(out, "oversized") {
		t.Errorf("a row within the bound: exit %d %d bytes %s", code, len(out), diag)
	}
}

// A scan place names the next record, not the event: in a Hermes store it names none.
func TestAScanPlaceInAHermesStoreNamesNoRecord(t *testing.T) {
	home := hermesHome(t)
	fakeScrubber(t)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	db := hermesStore(t, home, hermesMessage{"h1", "the next message of the session", nil, hermesTS(at)})
	e := store.Event{Source: store.Sessions, SourceKey: "h1@1", TS: at, Session: "h1", Harness: "hermes", Actor: store.ActorHarness, Kind: "appeared",
		Summary: "a session appeared", Ref: db + "@1#0"}
	putEvents(t, e)
	if code, out, diag := momentum("evidence", "show", e.ID()); code != 0 || strings.Contains(out, "the next message") || !strings.Contains(out, "a scan place in a Hermes store names no record") {
		t.Fatalf("exit %d %q %s", code, out, diag)
	}
}

// A Hermes store is read read-only: a WAL store with a row still in its log keeps its database file and its log as
// they were, as a writer would not (it checkpoints the row into the file as it closes, and removes the log).
func TestEvidenceShowLeavesAHermesStoreAsItWas(t *testing.T) {
	home := hermesHome(t)
	fakeScrubber(t)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	live := filepath.Join(t.TempDir(), "live.db")
	w, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: live}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.SetMaxOpenConns(1)
	for _, q := range []string{`PRAGMA journal_mode=WAL`, `PRAGMA wal_autocheckpoint=0`,
		`CREATE TABLE messages (id INTEGER PRIMARY KEY, session_id TEXT, role TEXT, content TEXT, tool_calls TEXT, tool_name TEXT, timestamp REAL)`,
		`INSERT INTO messages VALUES (1, 'h1', 'assistant', 'the row still in the log', NULL, NULL, ` + strconv.FormatFloat(hermesTS(at), 'f', -1, 64) + `)`} {
		if _, err := w.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db := filepath.Join(home, "state.db")
	for _, ext := range []string{"", "-wal"} { // a copy taken while the writer holds the row in its log
		raw, err := os.ReadFile(live + ext)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(db+ext, raw, 0o600)
	}
	before, _ := os.ReadFile(db)
	log, _ := os.ReadFile(db + "-wal")
	if len(log) == 0 {
		t.Fatal("the copy holds no log")
	}
	e := hermesEvent(db, 1, "h1", at, "the row still in the log", "")
	putEvents(t, e)
	if code, out, diag := momentum("evidence", "show", e.ID()); code != 0 || !strings.Contains(out, "the row still in the log") {
		t.Fatalf("exit %d %q %s", code, out, diag)
	}
	after, _ := os.ReadFile(db)
	logAfter, err := os.ReadFile(db + "-wal")
	if err != nil || string(after) != string(before) || string(logAfter) != string(log) {
		t.Fatalf("the store changed: file %d to %d bytes, log %d to %d bytes (%v)", len(before), len(after), len(log), len(logAfter), err)
	}
}
