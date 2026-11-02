package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/source"
	"github.com/agent-sh/momentum/internal/store"
)

// A trial's door runs the evidence commands with MOMENTUM_NOW, the end of its store, and as a loop run
// (MOMENTUM_LOOP_RUN), whose commands scrub all they print: trialEnvAt sets a store, a home and a configuration of the
// test's own and that clock.
func trialEnvAt(t *testing.T, end time.Time) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	t.Setenv("MOMENTUM_NOW", end.Format(time.RFC3339Nano))
	t.Setenv("MOMENTUM_LOOP_RUN", "fixture-run")
}

func datedGit(t *testing.T, dir string, at time.Time, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+at.Format(time.RFC3339), "GIT_COMMITTER_DATE="+at.Format(time.RFC3339),
		"GIT_AUTHOR_NAME=r", "GIT_AUTHOR_EMAIL=r@example.com", "GIT_COMMITTER_NAME=r", "GIT_COMMITTER_EMAIL=r@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A scan place shows the next record only when that record predates the trial's end.
func TestEvidenceShowWithholdsRecordsAtOrAfterTheEnd(t *testing.T) {
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	trialEnvAt(t, end)
	ctx := context.Background()
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	before := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":"the job failed again; stopping"}}`+"\n", end.Add(-3*time.Minute).Format(time.RFC3339Nano))
	after := fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"it reads the lockfile before it takes the lock, fix that"}}`+"\n", end.Add(20*time.Minute).Format(time.RFC3339Nano))
	if err := os.WriteFile(transcript, []byte(before+after), 0o600); err != nil {
		t.Fatal(err)
	}
	place := fmt.Sprintf("%s@%d", transcript, len(before)) // what a scan that read the whole file records
	src := filepath.Join(t.TempDir(), "source.db")
	st, err := store.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	evs := []store.Event{
		{Source: store.Supervise, SourceKey: "s1@1#promise#hold#0", TS: end.Add(-2 * time.Minute), Session: "s1", Harness: "claude", Actor: store.ActorDetector,
			Kind: "hold", Summary: "promise: hold: the owner typed into it within the owner window", Ref: place},
		{Source: store.Sessions, SourceKey: "s1@2", TS: end.Add(-time.Minute), Session: "s1", Harness: "claude", Actor: store.ActorHarness,
			Kind: "phase", Summary: "s1: working -> idle", Ref: place},
	}
	if _, err := st.Put(ctx, evs); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := store.AsOf(ctx, src, paths.DB(), end.Add(-30*24*time.Hour), end); err != nil {
		t.Fatal(err)
	}
	for _, record := range []struct{ line, reason string }{
		{after, "at or after"},
		{strings.ReplaceAll(after, end.Add(20*time.Minute).Format(time.RFC3339Nano), end.Format(time.RFC3339Nano)), "at or after"},
		{`{"message":"undated answer"}` + "\n", "timestamp"},
		{`{"timestamp":"bad","message":"undated answer"}` + "\n", "timestamp"},
	} {
		if err := os.WriteFile(transcript, []byte(before+record.line), 0600); err != nil {
			t.Fatal(err)
		}
		for _, ev := range evs {
			code, out, diag := momentum("evidence", "show", ev.ID())
			if code != 0 || strings.Contains(out, "lockfile") || strings.Contains(out, "undated answer") || !strings.Contains(out, record.reason) {
				t.Fatalf("%s: %d %s %s", ev.Source, code, out, diag)
			}
		}
	}
	if err := os.WriteFile(transcript, []byte(before+before), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, diag := momentum("evidence", "show", evs[0].ID())
	if code != 0 || !strings.Contains(out, "the job failed again") {
		t.Fatalf("before: %d %s %s", code, out, diag)
	}
	t.Setenv("MOMENTUM_NOW", "")
	if err := os.WriteFile(transcript, []byte(before+after), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, diag = momentum("evidence", "show", evs[1].ID())
	if code != 0 || !strings.Contains(out, "lockfile") {
		t.Fatalf("live place: %d %s %s", code, out, diag)
	}
}

// An event's own record is shown in a trial whatever its format dates it by: the snapshot already bounds the event,
// and only a scan place names a record that is not the event's.
func TestEvidenceShowKeepsAnEventsOwnRecordInATrial(t *testing.T) {
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	trialEnvAt(t, end)
	ctx := context.Background()
	log := filepath.Join(t.TempDir(), "events.jsonl")
	stamp := func(d time.Duration) string { return end.Add(d).Local().Format("2006-01-02T15:04:05") }
	first := `{"ts":"` + stamp(-2*time.Hour) + `","kind":"tool-error","session":"s1","cwd":"/w","model":"m"}` + "\n"
	own := `{"ts":"` + stamp(-time.Hour) + `","kind":"tool-error","session":"s1","cwd":"/w","model":"m","detail":"the lock was taken twice"}` + "\n"
	if err := os.WriteFile(log, []byte(first+own), 0o600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "source.db")
	st, err := store.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	// the row's event as its adapter makes it, keyed by its line
	if _, err := (source.Mistakes{Path: log}).Ingest(ctx, st); err != nil {
		t.Fatal(err)
	}
	evs, err := st.Events(ctx, store.Query{Source: "mistakes", Until: time.Now(), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var ev store.Event
	for _, e := range evs {
		if e.Ref == fmt.Sprintf("%s@%d", log, len(first)) {
			ev = e
		}
	}
	if ev.Ref == "" {
		t.Fatalf("the adapter did not store the row: %+v", evs)
	}
	st.Close()
	if err := store.AsOf(ctx, src, paths.DB(), end.Add(-30*24*time.Hour), end); err != nil {
		t.Fatal(err)
	}
	code, out, diag := momentum("evidence", "show", ev.ID())
	if code != 0 || !strings.Contains(out, `"detail":"the lock was taken twice"`) || strings.Contains(out, "withheld") {
		t.Fatalf("own record: %d %s %s", code, out, diag)
	}
}

// A mistakes log rotated after the end gives its rows new places, so an old event's place names another row: the
// line there is shown only when it is the event's own, by the key its adapter made of it.
func TestEvidenceShowRefusesARowTheLogRewrote(t *testing.T) {
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Hour)
	trialEnvAt(t, end)
	ctx := context.Background()
	log := filepath.Join(t.TempDir(), "mistakes.jsonl")
	stamp := func(d time.Duration) string { return end.Add(d).Local().Format("2006-01-02T15:04:05") }
	row := func(at time.Duration, kind, detail string) string {
		return fmt.Sprintf(`{"ts":%q,"kind":%q,"session":"s1","cwd":"/w","model":"m","tool":"Bash","detail":%q}`+"\n", stamp(at), kind, detail)
	}
	r1, r2 := row(-2*time.Hour, "tool-error", "lockfile busy"), row(-time.Hour, "tool-error", "lockfile busy, twice")
	os.WriteFile(log, []byte(r1+r2), 0o600)
	src := filepath.Join(t.TempDir(), "source.db")
	st, err := store.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (source.Mistakes{Path: log}).Ingest(ctx, st); err != nil {
		t.Fatal(err)
	}
	evs, err := st.Events(ctx, store.Query{Since: end.Add(-24 * time.Hour), Until: end, Source: "mistakes", Limit: 10})
	if err != nil || len(evs) != 2 {
		t.Fatalf("%v %v", evs, err)
	}
	var second store.Event
	for _, e := range evs {
		if strings.HasSuffix(e.Ref, fmt.Sprintf("@%d", len(r1))) {
			second = e
		}
	}
	// after the end the log is recreated: the same error again (r1's length), then the owner's correction
	n1 := row(2*time.Hour, "tool-error", "lockfile busy")
	n2 := fmt.Sprintf(`{"ts":%q,"kind":"correction","session":"s1","cwd":"/w","model":"m","input":"it reads the lockfile before it takes the lock, fix that","detail":"x"}`+"\n", stamp(3*time.Hour))
	os.Remove(log)
	os.WriteFile(log, []byte(n1+n2), 0o600)
	if _, err := (source.Mistakes{Path: log}).Ingest(ctx, st); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := store.AsOf(ctx, src, paths.DB(), end.Add(-30*24*time.Hour), end); err != nil {
		t.Fatal(err)
	}
	code, out, diag := momentum("evidence", "show", second.ID())
	if code != 0 || strings.Contains(out, "fix that") || !strings.Contains(out, "rewritten since") {
		t.Fatalf("a rewritten log's row: exit %d %q %s", code, out, diag)
	}
	t.Setenv("MOMENTUM_NOW", "")
	if _, out, _ := momentum("evidence", "show", second.ID()); strings.Contains(out, "fix that") {
		t.Fatalf("live, another row shown as the event's own: %q", out)
	}
}

// A Hermes event's reference names its message row by the row's id: evidence show never reads the database file at
// that offset as text.
func TestEvidenceShowDoesNotReadAHermesStoreAsText(t *testing.T) {
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Hour)
	trialEnvAt(t, end)
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "state.db")
	h, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: db}).String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Exec(`CREATE TABLE messages (id INTEGER PRIMARY KEY, session_id TEXT, role TEXT, content TEXT, timestamp REAL)`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 400; i++ {
		h.Exec(`INSERT INTO messages VALUES (?, 'h1', 'user', ?, ?)`, i, fmt.Sprintf("line %d\nit reads the lockfile before it takes the lock, fix that", i), float64(end.Add(time.Hour).Unix()))
	}
	h.Close()
	src := filepath.Join(t.TempDir(), "source.db")
	st, err := store.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(db)
	var evs []store.Event
	for off := 1; off < len(raw) && len(evs) < 50; off++ { // every place just after a newline, as a row id would land
		if raw[off-1] == '\n' {
			evs = append(evs, store.Event{Source: store.Transcripts, SourceKey: fmt.Sprint("h", off), TS: end.Add(-time.Hour), Session: "h1", Harness: "hermes",
				Actor: store.ActorAgent, Kind: "agent_text", Summary: "a step", Ref: fmt.Sprintf("%s@%d#0", db, off)})
		}
	}
	if len(evs) == 0 {
		t.Fatal("no newline in the database file")
	}
	if _, err := st.Put(ctx, evs); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := store.AsOf(ctx, src, paths.DB(), end.Add(-30*24*time.Hour), end); err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		code, out, diag := momentum("evidence", "show", e.ID())
		// its row read by its id: later than the end, or not a row at all, never the file's bytes
		if code != 0 || strings.Contains(out, "fix that") || !strings.Contains(out, "withheld") && !strings.Contains(out, "its record is gone") {
			t.Fatalf("a Hermes reference at %s: exit %d %q %s", e.Ref, code, out, diag)
		}
	}
}

// Search accepts every stored kind, including kinds outside transcript events.
func TestEvidenceSearchAcceptsStoredKinds(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	at := time.Now().Add(-time.Hour)
	putEvents(t, store.Event{Source: "incidents", SourceKey: "seen", TS: at, Actor: store.ActorDetector, Kind: "incident-seen", Summary: "backup volume not mounted"},
		store.Event{Source: "mistakes", SourceKey: "result", TS: at, Actor: store.ActorTool, Kind: "result", Summary: "lockfile busy"})
	for _, args := range [][]string{
		{"evidence", "search", "not mounted", "--kind", "incident-seen"},
		{"evidence", "search", "lockfile busy", "--kind", "result"},
		{"evidence", "events", "--kind", "incident-seen", "--since", "1d"},
		{"evidence", "search", "not mounted", "--source", "incidents"},
	} {
		code, out, diag := momentum(args...)
		if code != 0 || strings.TrimSpace(out) == "" {
			t.Fatalf("%v: %d %s %s", args, code, out, diag)
		}
	}
}

// An unknown source is an error naming all sources present, even outside the search window.
func TestEvidenceSearchRefusesAbsentSources(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	fakeScrubber(t)
	putEvents(t, store.Event{Source: "incidents", SourceKey: "old", TS: time.Now().Add(-90 * 24 * time.Hour), Actor: store.ActorDetector, Kind: "incident-seen", Summary: "volume not mounted"},
		store.Event{Source: "mistakes", SourceKey: "result", TS: time.Now(), Actor: store.ActorTool, Kind: "result", Summary: "lockfile busy"})
	code, out, diag := momentum("evidence", "search", "not mounted", "--source", "incident")
	if code != 2 || out != "" || !strings.Contains(diag, "incidents") || !strings.Contains(diag, "mistakes") {
		t.Fatalf("%d %s %s", code, out, diag)
	}
	code, out, diag = momentum("evidence", "search", "not mounted", "--source", "incidents")
	if code != 0 || out != "" {
		t.Fatalf("existing source outside window: %d %s %s", code, out, diag)
	}
}

// Search consumes flag values before its one phrase, validates filters, and keeps session matches newest first.
func TestEvidenceSearchParsesFlagsAnywhereAndOrdersSessionMatches(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	at := time.Now().Add(-time.Hour)
	putEvents(t, store.Event{Source: store.Transcripts, SourceKey: "old", TS: at, Session: "s1", Actor: store.ActorOwner, Kind: "owner_text", Summary: "queue stalled older"},
		store.Event{Source: store.Transcripts, SourceKey: "new", TS: at.Add(time.Minute), Session: "s1", Actor: store.ActorOwner, Kind: "owner_text", Summary: "queue stalled newer"})
	for _, args := range [][]string{{"--actor", "owner", "queue stalled"}, {"--kind=owner_text", "queue stalled", "--actor", "owner"}, {"queue stalled", "--session", "s1", "--limit", "1"}} {
		code, out, diag := momentum(append([]string{"evidence", "search"}, args...)...)
		if code != 0 || !strings.Contains(out, "newer") || strings.Contains(strings.Join(args, " "), "--limit") && strings.Contains(out, "older") {
			t.Fatalf("%v: %d %s %s", args, code, out, diag)
		}
	}
	for _, args := range [][]string{{"queue stalled", "extra"}, {"--actor", "typo", "queue stalled"}, {"--actor", "owner"}} {
		code, _, _ := momentum(append([]string{"evidence", "search"}, args...)...)
		if code != 2 {
			t.Fatalf("accepted %v", args)
		}
	}
}

// Search matches incident and mistakes summaries by default, and a source or recorded actor narrows the matches.
func TestEvidenceSearchReadsEverySource(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	at := time.Now().Add(-time.Hour)
	putEvents(t, store.Event{Source: "incidents", SourceKey: "seen", TS: at, Actor: store.ActorDetector, Kind: "incident-seen", Summary: "backup volume not mounted"},
		store.Event{Source: "mistakes", SourceKey: "result", TS: at, Actor: store.ActorTool, Kind: "result", Summary: "lockfile busy"})
	for _, phrase := range []string{"not mounted", "lockfile busy"} {
		code, out, diag := momentum("evidence", "search", phrase)
		if code != 0 || !strings.Contains(out, phrase) {
			t.Fatalf("search %q: %d %s %s", phrase, code, out, diag)
		}
	}
	for _, args := range [][]string{{"not mounted", "--source", "incidents", "--actor", "detector"}, {"--source=mistakes", "lockfile busy", "--actor", "tool"}} {
		code, out, diag := momentum(append([]string{"evidence", "search"}, args...)...)
		if code != 0 || out == "" {
			t.Fatalf("filter %v: %d %s %s", args, code, out, diag)
		}
	}
	code, out, diag := momentum("evidence", "search", "lockfile busy", "--source", "incidents")
	if code != 0 || out != "" {
		t.Fatalf("source ignored: %d %s %s", code, out, diag)
	}
	for _, actor := range []string{"owner", "agent", "tool", "harness", "detector", "unknown"} {
		code, _, diag = momentum("evidence", "search", "lockfile busy", "--actor", actor)
		if code != 0 {
			t.Fatalf("recorded actor %s: %s", actor, diag)
		}
	}
	code, _, _ = momentum("evidence", "search", "lockfile busy", "--actor", "invented")
	if code != 2 {
		t.Fatal("unknown actor accepted")
	}
}

// Default evidence windows follow a snapshot forty days back, never later records.
func TestEvidenceCommandsUseTheSnapshotClock(t *testing.T) {
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	trialEnvAt(t, end)
	hermes := t.TempDir()
	t.Setenv("HERMES_HOME", hermes)
	if err := os.WriteFile(filepath.Join(hermes, "state.db"), []byte("not a database"), 0600); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	before := end.Add(-2 * time.Hour)
	first := fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"queue stalled"}}`+"\n", before.Format(time.RFC3339Nano))
	later := fmt.Sprintf(`{"type":"api-request","timestamp":%q}`+"\n", end.Add(time.Hour).Format(time.RFC3339Nano))
	if err := os.WriteFile(transcript, []byte(first+later), 0600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "source.db")
	st, err := store.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.Put(context.Background(), []store.Event{
		{Source: store.Transcripts, SourceKey: "owner", TS: before, Session: "fixture", Actor: store.ActorOwner, Kind: "owner_text", Harness: "claude", Summary: "queue stalled", Ref: transcript + "@0#0"},
		{Source: store.Transcripts, SourceKey: "future", TS: end.Add(time.Hour), Session: "fixture", Actor: store.ActorAgent, Kind: "agent_text", Harness: "claude", Summary: "future answer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err = store.AsOf(context.Background(), src, paths.DB(), end.Add(-30*24*time.Hour), end); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"evidence", "search", "queue stalled"}, {"evidence", "events"}, {"evidence", "status"}, {"evidence", "status", "--since", end.Add(-24 * time.Hour).Format(time.RFC3339)}} {
		code, out, diag := momentum(args...)
		if code != 0 || out == "" || strings.Contains(out+diag, "future answer") {
			t.Fatalf("%v: %d %s %s", args, code, out, diag)
		}
		if args[1] == "search" || args[1] == "events" {
			if !strings.Contains(out, "queue stalled") {
				t.Fatalf("historical evidence lost: %s", out)
			}
		}
		if args[1] == "status" && !strings.Contains(out, "owner_text 1") {
			t.Fatalf("historical status lost: %s", out)
		}
	}
	from, err := parseWhen("1d")
	if err != nil || !from.Equal(end.Add(-24*time.Hour)) {
		t.Fatalf("parseWhen %s %v", from, err)
	}
	t.Setenv("MOMENTUM_NOW", "invalid")
	if _, err = commandNow(); err == nil {
		t.Fatal("bad clock accepted")
	}
}

// An explicit upper bound cannot move search beyond the trial clock, even in a store with later rows.
func TestEvidenceSearchClampsUntilToTheTrialClock(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	t.Setenv("MOMENTUM_NOW", end.Format(time.RFC3339))
	putEvents(t, store.Event{Source: store.Transcripts, SourceKey: "before", TS: end.Add(-time.Hour), Actor: store.ActorTool, Kind: "tool_result", Summary: "signal before"},
		store.Event{Source: store.Transcripts, SourceKey: "later", TS: end.Add(time.Hour), Actor: store.ActorTool, Kind: "tool_result", Summary: "signal later"})
	code, out, diag := momentum("evidence", "search", "signal", "--until", end.Add(2*time.Hour).Format(time.RFC3339))
	if code != 0 || !strings.Contains(out, "signal before") || strings.Contains(out, "signal later") {
		t.Fatalf("clock escaped: %d %s %s", code, out, diag)
	}
}

// In a trial evidence events stops at the trial's clock, an explicit --until too.
func TestEvidenceEventsStopsAtTheTrialClock(t *testing.T) {
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	trialEnvAt(t, end)
	putEvents(t, store.Event{Source: store.Transcripts, SourceKey: "before", TS: end.Add(-time.Hour), Session: "s1", Actor: store.ActorAgent, Kind: "agent_text", Summary: "before the end"},
		store.Event{Source: store.Transcripts, SourceKey: "after", TS: end.Add(time.Hour), Session: "s1", Actor: store.ActorAgent, Kind: "agent_text", Summary: "after the end"})
	code, out, diag := momentum("evidence", "events", "--since", end.Add(-2*time.Hour).Format(time.RFC3339), "--until", end.Add(2*time.Hour).Format(time.RFC3339))
	if code != 0 || !strings.Contains(out, "before the end") || strings.Contains(out, "after the end") {
		t.Fatalf("exit %d %q %s", code, out, diag)
	}
}

// Live evidence windows stop at the wall clock, so future-dated rows do not appear or count by default.
func TestLiveEvidenceStopsAtTheWallClock(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	t.Setenv("MOMENTUM_NOW", "")
	fakeScrubber(t)
	putEvents(t, store.Event{Source: store.Transcripts, SourceKey: "future", TS: time.Now().Add(90 * time.Minute), Actor: store.ActorOwner, Kind: "owner_text", Summary: "future signal"})
	for _, args := range [][]string{{"evidence", "events", "--since", "1d"}, {"evidence", "status", "--since", "1d"}, {"evidence", "search", "future signal"}} {
		code, out, diag := momentum(args...)
		if code != 0 || strings.Contains(out, "future signal") || strings.Contains(out, "owner_text 1") {
			t.Fatalf("future row: %v %d %s %s", args, code, out, diag)
		}
	}
}

// In a trial evidence unit reads the unit's log up to the end of store, never its state now.
func TestATrialReadsAUnitsLogUpToItsEnd(t *testing.T) {
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	trialEnvAt(t, end)
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	for _, name := range []string{"journalctl", "systemctl"} {
		os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nprintf '%s %s\\n' "+name+" \"$*\" >> "+calls+"\necho a line of the unit\n"), 0o755)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	code, out, diag := momentum("evidence", "unit", "backup.service", "--lines", "50")
	raw, _ := os.ReadFile(calls)
	if code != 0 || !strings.Contains(out, "a line of the unit") || strings.Contains(string(raw), "systemctl") ||
		!strings.Contains(string(raw), "--until @"+strconv.FormatInt(end.Unix(), 10)) {
		t.Fatalf("exit %d %q %s; calls %q", code, out, diag, raw)
	}
}

// fakeRoute writes a Bedrock broker's env file, as the broker writes it, for a test that starts no run, and
// returns the loop configuration that names it.
func fakeRoute(t *testing.T) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "bedrock.env")
	if err := os.WriteFile(file, []byte("CLAUDE_CODE_USE_BEDROCK=\"1\"\nANTHROPIC_BEDROCK_BASE_URL=\"http://127.0.0.1:9/nonce\"\nCLAUDE_CODE_SKIP_BEDROCK_AUTH=\"1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// No trial starts in this fixture; the tiny client is still required by the command's preflight.
	client := filepath.Join(t.TempDir(), "momentum-door")
	if out, err := exec.Command("go", "build", "-o", client, "../momentum-door").CombinedOutput(); err != nil {
		t.Fatalf("build client: %v: %s", err, out)
	}
	return `{"loop":{"bedrock_env":"` + file + `","door_client":"` + client + `"}}`
}

// With no Bedrock route the evaluation does not start: no run, no directory.
func TestAnEvaluationWithNoBedrockRouteDoesNotStart(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	conf := `{"loop":{"bedrock_env":"` + filepath.Join(t.TempDir(), "absent.env") + `"}}`
	if err := os.WriteFile(filepath.Join(os.Getenv("MOMENTUM_CONFIG"), "sources.json"), []byte(conf), 0600); err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join(t.TempDir(), "golden.jsonl")
	if err := os.WriteFile(golden, nil, 0600); err != nil {
		t.Fatal(err)
	}
	code, _, diag := momentum("loop", "eval", "--golden", golden, "--budget", "0", "--json")
	if code == 0 || !strings.Contains(diag, "no Bedrock route") {
		t.Fatalf("exit %d %s", code, diag)
	}
	if _, err := os.Stat(filepath.Join(loopDir(), "eval")); err == nil {
		t.Fatal("an evaluation directory was made")
	}
}

// The CLI compares both arms by default and persists an empty synthetic evaluation without starting a runner.
func TestEvaluationCLIWritesAnEmptyPairedResult(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	if err := os.WriteFile(filepath.Join(os.Getenv("MOMENTUM_CONFIG"), "sources.json"), []byte(fakeRoute(t)), 0600); err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join(t.TempDir(), "golden.jsonl")
	if err := os.WriteFile(golden, nil, 0600); err != nil {
		t.Fatal(err)
	}
	code, out, diag := momentum("loop", "eval", "--golden", golden, "--budget", "0", "--json")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, diag)
	}
	var result loop.EvaluationResults
	if err := json.Unmarshal([]byte(out), &result); err != nil || len(result.Arms) != 2 || result.K != 3 || result.Provenance.GoldenHash == "" {
		t.Fatalf("%s %v", out, err)
	}
	if len(result.Provenance.Programs["momentum"]) != 64 {
		t.Errorf("the result does not name the build the trials would run: %v", result.Provenance.Programs)
	}
	raw, err := os.ReadFile(filepath.Join(loopDir(), "eval", result.ID, "results.json"))
	if err != nil || len(raw) == 0 {
		t.Fatalf("saved result %v", err)
	}
	for _, arms := range []string{"loop", "native"} {
		code, out, diag = momentum("loop", "eval", "--golden", golden, "--budget", "0", "--arms", arms, "--json")
		if code != 0 {
			t.Fatalf("%d %s %s", code, out, diag)
		}
		if err = json.Unmarshal([]byte(out), &result); err != nil || len(result.Arms) != 1 || result.Arms[0].Arm != arms {
			t.Fatalf("%s %v", out, err)
		}
	}
}

// Evaluation help describes the single results object emitted by the JSON mode.
func TestEvaluationHelpNamesOneResultsObject(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	_, _, diag := momentum("loop", "eval", "--help")
	if !strings.Contains(diag, "one results object") || strings.Contains(diag, "per trial") {
		t.Fatal(diag)
	}
}

// The printed interval names only tasks with a matched comparison, including a zero-task result.
func TestEvaluationIntervalNamesItsComparedTaskCount(t *testing.T) {
	for _, tasks := range [][]loop.TaskComparison{nil, {{Task: "a", MoreOften: "tie"}, {Task: "b", MoreOften: "not compared"}}} {
		var out bytes.Buffer
		printEvaluation(&out, loop.EvaluationResults{Comparison: loop.Comparison{Tasks: tasks}})
		n := 0
		for _, task := range tasks {
			if task.MoreOften != "not compared" {
				n++
			}
		}
		if !strings.Contains(out.String(), fmt.Sprintf("interval (%d compared tasks;", n)) {
			t.Fatal(out.String())
		}
	}
}

// The printed summary gives each arm's investigations' tokens by kind beside its cost.
func TestEvaluationPrintsEachArmsTokensByKind(t *testing.T) {
	var out bytes.Buffer
	printEvaluation(&out, loop.EvaluationResults{Arms: []loop.ArmSummary{{Arm: "native", Usage: store.Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 5, CacheWrite1h: 4}}}})
	if !strings.Contains(out.String(), "tokens: 1 input, 2 output, 3 cache read, 5 cache write (4 of them for an hour)") {
		t.Fatal(out.String())
	}
}

// The printed summary says, for each arm, how many tasks were compared, how many root causes were reached among
// them and how many trials were left out as ungraded: pass@k, pass^k and the rates per cause read the compared
// trials alone, and a reader must see how many they are.
func TestEvaluationPrintsEachArmsComparedAndUngradedCounts(t *testing.T) {
	var out bytes.Buffer
	printEvaluation(&out, loop.EvaluationResults{Arms: []loop.ArmSummary{{Arm: "loop", K: 3, Graded: 2, Reached: 2, ComparedTasks: 1, ComparedReached: 1, Ungraded: 1}}})
	if !strings.Contains(out.String(), "compared: 1 tasks, 1 root causes reached among them; 1 trials ungraded, left out with their pairs") {
		t.Fatal(out.String())
	}
}
