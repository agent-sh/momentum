package ro

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/evidenceview"
	"github.com/agent-sh/momentum/internal/store"
)

type fixture struct {
	loc    locations
	events []store.Event
	raw    []byte
}

// Copy the real table definitions and rows, then VACUUM INTO a plain file.
// There is no schema version or derived table for a host opener to consult.
func inputs(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	st, err := store.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	var evs []store.Event
	for i := range 205 {
		evs = append(evs, store.Event{Source: "fixture", SourceKey: fmt.Sprintf("%03d", i), TS: at.Add(time.Duration(i) * time.Second),
			Session: "run-a", Harness: "codex", Actor: store.ActorTool, Kind: "tool_result", Tool: "Read",
			Summary: fmt.Sprintf("marker-7f3a row %03d", i), Meta: `{"is_error":true}`, Ref: "/fixture/unavailable@0"})
	}
	evs = append(evs,
		store.Event{Source: "other", SourceKey: "owner", TS: at, Session: "run-b", Harness: "claude", Actor: store.ActorOwner, Kind: "owner_text", Summary: "marker-7f3a owner"},
		store.Event{Source: "other", SourceKey: "call", TS: at.Add(time.Second), Session: "run-b", Harness: "claude", Actor: store.ActorAgent, Kind: "tool_call", Summary: "marker-7f3a call"},
		store.Event{Source: "history", SourceKey: "root", TS: at.Add(-20 * 24 * time.Hour), Session: "old-session", Agent: "child", Harness: "codex", Actor: store.ActorAgent, Kind: "agent_text", Summary: "marker-7f3a parent"},
		store.Event{Source: "history", SourceKey: "child", TS: at.Add(-20*24*time.Hour + time.Second), Session: "child", Harness: "codex", Actor: store.ActorAgent, Kind: "agent_text", Summary: "marker-7f3a child"})
	if _, err := st.Put(context.Background(), evs); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	copyDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	copyDB.SetMaxOpenConns(1)
	if _, err := copyDB.Exec("ATTACH DATABASE ? AS original", source); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"events", "notes"} {
		var ddl string
		if err := db.QueryRow("SELECT sql FROM sqlite_schema WHERE type='table' AND name=?", name).Scan(&ddl); err != nil {
			t.Fatal(err)
		}
		if _, err := copyDB.Exec(ddl); err != nil {
			t.Fatal(err)
		}
		if _, err := copyDB.Exec("INSERT INTO " + name + " SELECT * FROM original." + name); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "store?#.sqlite")
	if _, err := copyDB.Exec("VACUUM main INTO ?", path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0444); err != nil {
		t.Fatal(err)
	}
	records := filepath.Join(dir, "records")
	if err := os.Mkdir(records, 0700); err != nil {
		t.Fatal(err)
	}
	for _, e := range evs[:2] {
		write(t, filepath.Join(records, "ev-"+e.ID()+".txt"), evidenceview.EventHeader(e)+"marker-7f3a full captured record\n")
	}
	write(t, filepath.Join(records, "index.txt"), fmt.Sprintf("ev-%s.txt\t%s\t%s\nev-%s.txt\t%s\t%s\nev-%s.txt\t%s\t%s\n",
		evs[0].ID(), evs[0].ID(), evs[0].TS.Format(time.RFC3339Nano),
		evs[1].ID(), evs[1].ID(), evs[1].TS.Format(time.RFC3339Nano),
		evs[205].ID(), evs[205].ID(), evs[205].TS.Format(time.RFC3339Nano)))
	write(t, filepath.Join(records, "unit-fixture.service.txt"), "marker-7f3a captured unit\n")
	c := store.Case{ID: "cfixture", Title: "marker-7f3a failure", Family: "failures", Status: store.CaseInvestigating,
		Mechanism: "marker-7f3a mechanism", Alternatives: []string{"another cause"}, NextTest: "take the lock", Evidence: 2, Signatures: []string{"fixture"}}
	raw, err := json.Marshal(struct {
		Case     store.Case        `json:"case"`
		Evidence []store.Evidence  `json:"evidence"`
		Log      []store.CaseEntry `json:"log"`
	}{c, []store.Evidence{{Ref: "ev:" + evs[0].ID(), At: at, Kind: "tool_result", Summary: "marker-7f3a evidence"}},
		[]store.CaseEntry{{At: at, Actor: "fixture", Action: "open", Detail: "marker-7f3a opened"}}})
	if err != nil {
		t.Fatal(err)
	}
	caseFile := filepath.Join(dir, "case.json")
	write(t, caseFile, string(raw))
	return fixture{locations{path, records, caseFile}, evs, raw}
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0444); err != nil {
		t.Fatal(err)
	}
}

func invoke(f fixture, args ...string) (int, string, string) {
	var out, diag bytes.Buffer
	args = append(args, "--store", f.loc.store, "--records", f.loc.records, "--case", f.loc.caseFile)
	code := Run(context.Background(), args, &out, &diag)
	return code, out.String(), diag.String()
}

func TestReadOnlyEventsFiltersOrderingJSONAndLimits(t *testing.T) {
	f := inputs(t)
	for _, tc := range []struct {
		name string
		args []string
		rows int
		want string
	}{
		{"default", nil, 50, "row 204"},
		{"zero-limit", []string{"--limit", "0"}, 200, "row 204"},
		{"negative-limit", []string{"--limit", "-1"}, 200, "row 204"},
		{"source", []string{"--source", "other", "--json"}, 2, `"Kind":"tool_call"`},
		{"kinds", []string{"--kind", "tool_call,owner_text", "--json"}, 2, `"Source":"other"`},
		{"harness", []string{"--harness", "claude"}, 2, "marker-7f3a call"},
		{"session", []string{"--session", "run-b"}, 2, "marker-7f3a owner"},
		{"old-tree", []string{"--session", "old-sess"}, 2, "marker-7f3a parent"},
		{"since", []string{"--session", "old-session", "--since", "1d"}, 0, ""},
		{"bounds", []string{"--since", f.events[0].TS.Format(time.RFC3339Nano), "--until", f.events[1].TS.Format(time.RFC3339Nano), "--source", "fixture"}, 1, "row 000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, diag := invoke(f, append([]string{"evidence", "events"}, tc.args...)...)
			if code != 0 || strings.Count(out, "\n") != tc.rows || !strings.Contains(out, tc.want) {
				t.Fatalf("%d %q %s", code, out, diag)
			}
			if tc.name == "default" || tc.name == "zero-limit" || tc.name == "negative-limit" {
				if !strings.Contains(diag, "more events than --limit") {
					t.Fatal("limit truncation was silent")
				}
			}
			if strings.Contains(strings.Join(tc.args, " "), "--json") {
				for _, row := range strings.Split(strings.TrimSpace(out), "\n") {
					if !json.Valid([]byte(row)) {
						t.Fatal("invalid event JSON")
					}
				}
			}
		})
	}
	for _, args := range [][]string{{"--harness", "unknown"}, {"--since", "invalid"}, {"--until", "invalid"}, {"stray"}, {"--limit", "bad"}} {
		if code, _, _ := invoke(f, append([]string{"evidence", "events"}, args...)...); code != 2 {
			t.Fatal("accepted invalid event arguments")
		}
	}
}

func TestReadOnlyShowCapturedMissingNeighboursAndClip(t *testing.T) {
	f := inputs(t)
	code, out, diag := invoke(f, "evidence", "show", "ev:"+f.events[0].ID())
	if code != 0 || strings.Count(out, "ev:"+f.events[0].ID()) != 1 || !strings.Contains(out, "marker-7f3a full captured record") ||
		!strings.Contains(out, "neighbour ev:"+f.events[1].ID()) || strings.Contains(out, "neighbour ev:"+f.events[205].ID()) {
		t.Fatalf("captured record: %d %q %s", code, out, diag)
	}
	code, out, diag = invoke(f, "evidence", "show", f.events[0].ID(), f.events[2].ID(), "--bytes", "4")
	if code != 0 || !strings.Contains(out, "mark\n  (its first 4 of") || !strings.Contains(out, "record is not among the run's records") {
		t.Fatalf("clip or missing: %d %q %s", code, out, diag)
	}
	args := []string{"evidence", "show"}
	for range 10 {
		args = append(args, f.events[0].ID())
	}
	if code, out, _ := invoke(f, args...); code != 0 || strings.Count(out, "full captured record") != 10 {
		t.Fatal("ten IDs not shown")
	}
	if code, _, _ := invoke(f, append(args, f.events[0].ID())...); code != 2 {
		t.Fatal("accepted eleven IDs")
	}
	for _, args := range [][]string{{}, {"../escape"}, {f.events[0].ID(), "--bytes", "-1"}, {"000000000000000000000000"},
		{"--separate", f.events[0].ID()}} { // --separate is the host's, for the records pass
		if code, _, _ := invoke(f, append([]string{"evidence", "show"}, args...)...); code != 2 {
			t.Fatal("accepted invalid or absent ID")
		}
	}
}

func TestReadOnlyShowKeepsStoredIDWhenSourceKeyWasScrubbed(t *testing.T) {
	f := inputs(t)
	if err := os.Chmod(f.loc.store, 0600); err != nil {
		t.Fatal(err)
	}
	uri := url.URL{Scheme: "file", Path: f.loc.store}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	id := f.events[0].ID()
	if _, err := db.Exec("UPDATE events SET source_key='scrubbed-key' WHERE id=?", id); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	code, out, diag := invoke(f, "evidence", "show", id)
	if code != 0 || strings.Count(out, "ev:"+id) != 1 {
		t.Fatalf("scrubbed key changed the visible id: %d %q %s", code, out, diag)
	}
}

func TestReadOnlyStatusUnitAndCaseForms(t *testing.T) {
	f := inputs(t)
	code, out, diag := invoke(f, "evidence", "status")
	if code != 0 || out != "fixture         tool_result 205\nother           owner_text 1, tool_call 1\n" {
		t.Fatalf("status: %d %q %s", code, out, diag)
	}
	code, out, diag = invoke(f, "evidence", "status", "--since", "30d")
	if code != 0 || !strings.Contains(out, "history         agent_text 2") {
		t.Fatalf("status duration: %d %q %s", code, out, diag)
	}
	code, out, diag = invoke(f, "evidence", "unit", "fixture.service")
	if code != 0 || out != "marker-7f3a captured unit\n" {
		t.Fatalf("unit: %d %q %s", code, out, diag)
	}
	code, out, diag = invoke(f, "case", "show")
	for _, text := range []string{"cfixture  marker-7f3a failure  [failures, investigating]", "mechanism:", "alternative 1:", "\n  fix:   ", "evidence: 2", "marker-7f3a evidence", "log: 1 lines", "marker-7f3a opened"} {
		if code != 0 || !strings.Contains(out, text) {
			t.Fatalf("case: %d %q %s; missing %s", code, out, diag, text)
		}
	}
	// the investigation's fix is prose: a build that read it as "next test" put it in its test command
	if strings.Contains(out, "next test") {
		t.Fatalf("case shows the investigation's fix as a next test: %q", out)
	}
	code, out, diag = invoke(f, "case", "show", "--json")
	if code != 0 || out != string(f.raw) {
		t.Fatalf("case JSON is not the input file: %d %q %s", code, out, diag)
	}
	for _, args := range [][]string{{"evidence", "status", "--since", "-1h"}, {"evidence", "status", "stray"},
		{"evidence", "unit", "../fixture.service"}, {"evidence", "unit", "absent.service"}, {"evidence", "unit", "fixture.service", "--lines", "1"},
		{"case", "show", "cfixture"}, {"case", "show", "--unknown"}} {
		if code, _, _ := invoke(f, args...); code != 2 {
			t.Fatalf("accepted invalid arguments %v", args)
		}
	}
}

func TestReadOnlyRefusesEveryOtherCommandBeforeOpeningAnything(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MOMENTUM_HOME", filepath.Join(dir, "home"))
	t.Setenv("MOMENTUM_CONFIG", filepath.Join(dir, "config"))
	for _, args := range [][]string{{}, {"evidence", "ingest"}, {"evidence", "rebuild", "fixture"}, {"rebuild"}, {"case", "open"},
		{"loop"}, {"ship"}, {"unknown"}, {"version"}, {"note", "add"}, {"sessions"}, {"capacity"}, {"report"}, {"supervise"}} {
		t.Run(strings.Join(args, "-"), func(t *testing.T) {
			var out, diag bytes.Buffer
			code := Run(context.Background(), args, &out, &diag)
			if code != 2 || out.Len() != 0 || strings.TrimSpace(diag.String()) != Refusal {
				t.Fatalf("refusal: %d %q %q", code, out.String(), diag.String())
			}
		})
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatal("refused command created host state")
	}
	for _, help := range []string{"help", "--help", "-h"} {
		var out, diag bytes.Buffer
		code := Run(context.Background(), []string{help}, &out, &diag)
		if code != 2 || !strings.Contains(out.String(), "for tests only") || strings.TrimSpace(diag.String()) != Refusal {
			t.Fatal("usage must keep the refusal exit and explain test overrides")
		}
	}
}

func TestReadOnlyOpenCannotWriteNeedsNoSchemaAndCreatesNoSidecars(t *testing.T) {
	f := inputs(t)
	before, err := os.ReadFile(f.loc.store)
	if err != nil {
		t.Fatal(err)
	}
	// Make permissions writable so only the connection's mode prevents writes.
	if err := os.Chmod(f.loc.store, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := openCopy(context.Background(), f.loc.store)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var tables, journal string
	if err := db.QueryRow("SELECT group_concat(name, ',') FROM (SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name)").Scan(&tables); err != nil || tables != "events,notes" {
		t.Fatalf("fixture tables: %s %v", tables, err)
	}
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || journal != "delete" {
		t.Fatalf("fixture journal: %s %v", journal, err)
	}
	for _, query := range []string{"UPDATE events SET summary='changed'", "CREATE TABLE door(x)", "PRAGMA user_version=42"} {
		if _, err := db.Exec(query); err == nil {
			t.Fatalf("read-only connection wrote: %s", query)
		}
	}
	db.Close()
	after, err := os.ReadFile(f.loc.store)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("copy changed")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(f.loc.store + suffix); !os.IsNotExist(err) {
			t.Fatalf("created a SQLite sidecar %s: %v", suffix, err)
		}
	}
	missing := filepath.Join(t.TempDir(), "absent.sqlite")
	if db, err := openCopy(context.Background(), missing); err == nil {
		db.Close()
		t.Fatal("opened missing store")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("created missing store")
	}
}

func TestReadOnlyImmutableCopyNeedsNoSQLiteLock(t *testing.T) {
	f := inputs(t)
	if err := os.Chmod(f.loc.store, 0600); err != nil {
		t.Fatal(err)
	}
	uri := url.URL{Scheme: "file", Path: f.loc.store}
	writer, err := sql.Open("sqlite", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	if _, err := writer.Exec("BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	defer writer.Exec("ROLLBACK")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	db, err := openCopy(ctx, f.loc.store)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var rows int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&rows); err != nil || rows != len(f.events) {
		t.Fatalf("immutable connection tried to lock the copy: %d %v", rows, err)
	}
}

func TestReadOnlyRecordsRefuseEscapesPipesAndOversize(t *testing.T) {
	for _, kind := range []string{"link", "pipe", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			f := inputs(t)
			path := filepath.Join(f.loc.records, "unit-unsafe.service.txt")
			switch kind {
			case "link":
				if err := os.Symlink(f.loc.caseFile, path); err != nil {
					t.Fatal(err)
				}
			case "pipe":
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				write(t, path, strings.Repeat("x", (256<<10)+1))
			}
			if code, _, _ := invoke(f, "evidence", "unit", "unsafe.service"); code != 3 {
				t.Fatal("read unsafe record")
			}
		})
	}
}

// A relative time counts back from the newest event of the copy, not from the machine's clock: a copy cut as of an
// old end still answers the window the prompt suggests.
func TestRelativeTimesCountBackFromTheCopysEnd(t *testing.T) {
	f := inputs(t)
	for _, args := range [][]string{{"evidence", "events", "--since", "30d", "--limit", "5"}, {"evidence", "status", "--since", "30d"}} {
		code, out, diag := invoke(f, args...)
		if code != 0 || strings.TrimSpace(out) == "" {
			t.Fatalf("%v: %d %q %s", args, code, out, diag)
		}
	}
	// the newest event is the anchor: a window of a second before it holds that event and no older one
	newest := f.events[len(f.events)-1].TS
	for _, e := range f.events {
		if e.TS.After(newest) {
			newest = e.TS
		}
	}
	code, out, _ := invoke(f, "evidence", "events", "--since", "1s", "--limit", "500")
	if code != 0 || strings.Count(out, "\n") < 1 || strings.Count(out, "\n") >= len(f.events) {
		t.Fatalf("a window of one second before the copy's end: %d rows of %d (code %d)", strings.Count(out, "\n"), len(f.events), code)
	}
}

// Ids and names are checked before they name a file, and a link in the records directory is not followed.
func TestReadOnlyRefusesBadIDsAndLinkedRecords(t *testing.T) {
	f := inputs(t)
	for _, args := range [][]string{{"evidence", "show", "ev:ZZ"}, {"evidence", "show", "../x"}, {"evidence", "unit", "../x"}, {"evidence", "unit", "a/b"}} {
		if code, out, _ := invoke(f, args...); code == 0 || out != "" {
			t.Errorf("%v was accepted: %d %q", args, code, out)
		}
	}
	id := f.events[0].ID()
	dir := f.loc.records
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "elsewhere.txt")
	if err := os.WriteFile(target, []byte("a linked record\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "ev-"+id+".txt")
	os.Remove(link)
	if err := os.Symlink(target, link); err != nil {
		t.Skip("no symlinks here")
	}
	if _, out, _ := invoke(f, "evidence", "show", id); strings.Contains(out, "a linked record") {
		t.Fatal("a linked record was followed")
	}
}
