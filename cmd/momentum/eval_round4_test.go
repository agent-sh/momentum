package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/source"
	"github.com/agent-sh/momentum/internal/store"
)

func evalEnvAt(t *testing.T, end time.Time) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	t.Setenv("MOMENTUM_NOW", end.Format(time.RFC3339Nano))
	t.Setenv("MOMENTUM_EVAL", "fixture")
	t.Setenv("MOMENTUM_LOOP_RUN", "fixture-run")
}

// A mistakes log rotated after the end gives its rows new places, so an old event's place names another row: the
// line there is shown only when it is the event's own, by the key its adapter made of it.
func TestEvidenceShowRefusesARowTheLogRewrote(t *testing.T) {
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Hour)
	evalEnvAt(t, end)
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
	t.Setenv("MOMENTUM_EVAL", "")
	if _, out, _ := momentum("evidence", "show", second.ID()); strings.Contains(out, "fix that") {
		t.Fatalf("live, another row shown as the event's own: %q", out)
	}
}

// A Hermes event's reference names its message row by the row's id: evidence show never reads the database file
// at that offset as text.
func TestEvidenceShowDoesNotReadAHermesStoreAsText(t *testing.T) {
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Hour)
	evalEnvAt(t, end)
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
		if code != 0 || strings.Contains(out, "fix that") || !strings.Contains(out, "row of the Hermes store") {
			t.Fatalf("a Hermes reference at %s: exit %d %q %s", e.Ref, code, out, diag)
		}
	}
}

// In an evaluation a work tree whose .git file names a git directory outside the trial is refused, as a work tree
// outside it is.
func TestEvidenceGitRefusesAGitDirectoryOutsideTheTrial(t *testing.T) {
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	evalEnvAt(t, end)
	base := t.TempDir()
	other := filepath.Join(base, "other")
	datedGit(t, base, end, "init", "-q", "-b", "main", other)
	os.WriteFile(filepath.Join(other, "job"), []byte("after"), 0o600)
	datedGit(t, other, end.Add(72*time.Hour), "add", "-A")
	datedGit(t, other, end.Add(72*time.Hour), "commit", "-qm", "fix: take the lock before reading the lockfile")
	trial := filepath.Join(base, "eval", "key", "arm")
	notes := filepath.Join(trial, "notes")
	os.MkdirAll(notes, 0o700)
	os.WriteFile(filepath.Join(notes, ".git"), []byte("gitdir: "+filepath.Join(other, ".git")+"\n"), 0o600)
	t.Setenv("MOMENTUM_LOOP_DIR", trial)
	code, out, diag := momentum("evidence", "git", notes, "log", "--oneline")
	if code != 2 || strings.Contains(out, "take the lock") || !strings.Contains(diag, "git directory") {
		t.Fatalf("a .git file naming another repository: exit %d %q %s", code, out, diag)
	}
}

// In an evaluation evidence events stops at the trial's clock, an explicit --until too.
func TestEvidenceEventsStopsAtTheTrialClock(t *testing.T) {
	end := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	evalEnvAt(t, end)
	putEvents(t, store.Event{Source: store.Transcripts, SourceKey: "before", TS: end.Add(-time.Hour), Session: "s1", Actor: store.ActorAgent, Kind: "agent_text", Summary: "before the end"},
		store.Event{Source: store.Transcripts, SourceKey: "after", TS: end.Add(time.Hour), Session: "s1", Actor: store.ActorAgent, Kind: "agent_text", Summary: "after the end"})
	code, out, diag := momentum("evidence", "events", "--since", end.Add(-2*time.Hour).Format(time.RFC3339), "--until", end.Add(2*time.Hour).Format(time.RFC3339))
	if code != 0 || !strings.Contains(out, "before the end") || strings.Contains(out, "after the end") {
		t.Fatalf("exit %d %q %s", code, out, diag)
	}
}
