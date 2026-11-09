package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/agent-sh/momentum/internal/store"
)

// showOne runs evidence show on one event and fails the test on a nonzero exit.
func showOne(t *testing.T, args ...string) string {
	t.Helper()
	code, out, diag := momentum(append([]string{"evidence", "show"}, args...)...)
	if code != 0 {
		t.Fatalf("evidence show %v: exit %d %q %s", args, code, out, diag)
	}
	return out
}

// A Codex rollout that Codex compressed after it was read is still the session's record: show reads it at the
// same offset of its JSONL bytes, scrubbed whole and clipped after, and the events of one copy in any order. A
// record that is gone with no compressed copy is still gone; a compressed copy that lies where a run can write,
// by its name or through a directory link, is withheld; a copy that does not decode is withheld; an offset past a
// copy's end is no record boundary; and a link at a plain record's name is not followed.
func TestEvidenceShowReadsACompressedRollout(t *testing.T) {
	fakeScrubber(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	t.Setenv("MOMENTUM_NOW", "")
	dir := t.TempDir()
	first := `{"timestamp":"2026-03-02T09:00:00Z","type":"session_meta","payload":{"id":"first-line-marker"}}` + "\n"
	second := `{"timestamp":"2026-03-02T09:00:01Z","type":"response_item","payload":{"type":"function_call_output","output":"secret-value packed-marker"}}` + "\n"
	third := `{"timestamp":"2026-03-02T09:00:02Z","type":"response_item","payload":{"output":"` + strings.Repeat("z", readMax) + `"}}` + "\n"
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	pack := func(path string) {
		t.Helper()
		if err := os.WriteFile(path+".zst", enc.EncodeAll([]byte(first+second+third), nil), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rollout := filepath.Join(dir, "rollout-2026-03-02T09-00-00-0000.jsonl")
	pack(rollout)
	run := filepath.Join(loopDir(), "runs", "earlier")
	if err := os.MkdirAll(run, 0o700); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(run, "rollout-planted.jsonl")
	pack(planted)
	aliased := filepath.Join(dir, "rollout-aliased.jsonl")
	if err := os.Symlink(planted+".zst", aliased+".zst"); err != nil {
		t.Fatal(err)
	}
	// a directory link into the run's root: only the copy's resolved target lies there, since the plain name is gone
	linked := filepath.Join(dir, "linked")
	if err := os.Symlink(run, linked); err != nil {
		t.Fatal(err)
	}
	// a copy that decodes into its second line and then holds no zstd frame
	broken := filepath.Join(dir, "rollout-broken.jsonl")
	if err := os.WriteFile(broken+".zst", append(enc.EncodeAll([]byte(first+`{"timestamp":"2026-03-02T09:00:01Z","cut`), nil), "not a zstd frame"...), 0o600); err != nil {
		t.Fatal(err)
	}
	// a link at a plain record's name, to a record no run can write
	plainRecord := filepath.Join(dir, "plain-target.jsonl")
	if err := os.WriteFile(plainRecord, []byte(`{"timestamp":"2026-03-02T09:00:00Z","payload":"plain-link-marker"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plainLink := filepath.Join(dir, "rollout-plain-link.jsonl")
	if err := os.Symlink(plainRecord, plainLink); err != nil {
		t.Fatal(err)
	}
	event := func(key, path string, off int) store.Event {
		return store.Event{Source: store.Transcripts, SourceKey: key, TS: time.Now().Add(-time.Minute), Session: "s1", Harness: "codex",
			Actor: store.ActorTool, Kind: "tool_result", Summary: "fixture summary", Ref: fmt.Sprintf("%s@%d#0", path, off)}
	}
	packed := event("packed", rollout, len(first))
	head := event("head", rollout, 0)
	long := event("long", rollout, len(first)+len(second))
	inside := event("inside", rollout, 5)
	pastEnd := event("past-end", rollout, len(first+second+third)+2)
	gone := event("gone", filepath.Join(dir, "rollout-gone.jsonl"), 0)
	writable := event("writable", aliased, len(first))
	viaLink := event("via-link", filepath.Join(linked, "rollout-planted.jsonl"), len(first))
	undecoded := event("undecoded", broken, len(first))
	linkedPlain := event("linked-plain", plainLink, 0)
	putEvents(t, packed, head, long, inside, pastEnd, gone, writable, viaLink, undecoded, linkedPlain)

	out := showOne(t, packed.ID())
	if !strings.Contains(out, "packed-marker") || !strings.Contains(out, "************") || strings.Contains(out, "secret-value") ||
		strings.Contains(out, "first-line-marker") || strings.Contains(out, "its record is gone") {
		t.Fatalf("compressed rollout's record: %q", out)
	}
	if out := showOne(t, "--bytes", "60", packed.ID()); strings.Contains(out, "secret-value") || !strings.Contains(out, "(its first 60 of") {
		t.Fatalf("compressed record not scrubbed whole before its cut: %q", out)
	}
	if out := showOne(t, long.ID()); !strings.Contains(out, "(oversized record withheld)") || strings.Contains(out, "zzzz") {
		t.Fatalf("oversized compressed record: %q", out)
	}
	if code, _, diag := momentum("evidence", "show", inside.ID()); code != 2 || !strings.Contains(diag, "not a record boundary") {
		t.Fatalf("an offset inside a compressed record: %d %s", code, diag)
	}
	if out := showOne(t, gone.ID()); !strings.Contains(out, "(its record is gone)") {
		t.Fatalf("a gone record: %q", out)
	}
	if code, _, diag := momentum("evidence", "show", pastEnd.ID()); code != 2 || !strings.Contains(diag, "not a record boundary") {
		t.Fatalf("an offset past a compressed copy's end: %d %s", code, diag)
	}
	for _, e := range []store.Event{viaLink, writable} {
		if out := showOne(t, e.ID()); !strings.Contains(out, "(record withheld: it lies where a run can write)") || strings.Contains(out, "packed-marker") {
			t.Fatalf("%s: a compressed copy in a run's root: %q", e.SourceKey, out)
		}
	}
	if out := showOne(t, undecoded.ID()); !strings.Contains(out, "(record withheld: its compressed copy cannot be read)") {
		t.Fatalf("a copy that does not decode to its record's end: %q", out)
	}
	if out := showOne(t, linkedPlain.ID()); !strings.Contains(out, "(its record is gone)") || strings.Contains(out, "plain-link-marker") {
		t.Fatalf("a link at a plain record's name: %q", out)
	}
	// one child's events of one copy, given out of offset order and one twice, each print its own record in the
	// order given
	out = showOne(t, "--separate", packed.ID(), head.ID(), packed.ID())
	parts := strings.Split(out, "\x1e")
	if len(parts) != 3 || !strings.Contains(parts[0], "packed-marker") || strings.Contains(parts[0], "first-line-marker") ||
		!strings.Contains(parts[1], "first-line-marker") || strings.Contains(parts[1], "packed-marker") || !strings.Contains(parts[2], "packed-marker") {
		t.Fatalf("events of one copy out of offset order: %q", out)
	}
}

// A compressed copy is decompressed from the file openRecord opened and checked, not by its name again: another
// copy that takes the name after the open, as a swap there and back would before the read, is not what is read.
func TestACompressedCopyIsReadFromTheFileOpened(t *testing.T) {
	isolatedHome(t)
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	pack := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, enc.EncodeAll([]byte(text), nil), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plain := filepath.Join(t.TempDir(), "rollout.jsonl")
	pack(plain+".zst", "opened-marker\n")
	f, packed, err := openRecord(plain)
	if err != nil || packed != plain+".zst" {
		t.Fatalf("open the copy: %q %v", packed, err)
	}
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	pack(plain+".other", "swapped-marker\n")
	if err := os.Rename(plain+".other", packed); err != nil {
		t.Fatal(err)
	}
	var copies copyReader
	defer copies.close()
	br, err := copies.at(f, fi, 0)
	if err != nil {
		t.Fatal(err)
	}
	if line, err := br.ReadString('\n'); err != nil || line != "opened-marker\n" {
		t.Fatalf("read the copy by its name again: %q %v", line, err)
	}
}

// A Hermes event's record is its row in the Hermes store, read by the session that holds it (a delegation's own),
// scrubbed and bounded as a transcript line is, with tool calls that are no JSON as a string. A row the store no
// longer holds, or holds for another session, is gone; a store that cannot be opened or read as one is withheld; an
// as-of child still withholds every Hermes row; a store where a run can write is withheld, and a pipe is not read;
// and another source's reference into the store is not a row.
func TestEvidenceShowReadsAHermesRow(t *testing.T) {
	fakeScrubber(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	t.Setenv("MOMENTUM_NOW", "")
	hermesStore := func(path string) {
		t.Helper()
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`create table sessions (id text primary key, source text not null, created_source text, model text, model_config text, parent_session_id text, started_at real not null, ended_at real, end_reason text, cwd text, git_repo_root text, title text);
create table messages (id integer primary key autoincrement, session_id text not null references sessions(id), role text not null, content text, tool_call_id text, tool_calls text, tool_name text, timestamp real not null, finish_reason text, _compressed_summary integer default 0, display_kind text);
insert into sessions (id, source, created_source, started_at) values ('phone', 'mobile', 'mobile', 100);
insert into sessions (id, source, created_source, model_config, parent_session_id, started_at) values ('sub', 'mobile', 'subagent', '{"_delegate_from":"phone"}', 'phone', 101);
insert into messages (session_id, role, content, timestamp) values ('phone', 'user', 'prune the docker images', 101);
insert into messages (session_id, role, content, tool_calls, finish_reason, timestamp) values ('phone', 'assistant', 'Pruning.',
  '[{"id":"call_h1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"docker image prune <dangling> secret-value\"}"}}]', 'tool_calls', 102);
insert into messages (session_id, role, content, tool_call_id, tool_name, timestamp) values ('phone', 'tool', '{"output": "secret-value hermes-marker", "exit_code": 1}', 'call_h1', 'terminal', 103.25);
insert into messages (session_id, role, content, timestamp) values ('sub', 'assistant', 'delegated-marker', 104);
insert into messages (session_id, role, content, timestamp) values ('phone', 'tool', ?, 105);
insert into messages (session_id, role, content, tool_calls, timestamp) values ('phone', 'assistant', 'Calling.', 'not json [', 106);`, strings.Repeat("x", readMax+1)); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "state.db")
	hermesStore(path)
	// a pipe where the store was: opening it to read would wait on a writer. A guard that let it through would hold
	// the run, so here a writer comes and goes every tenth of a second until the test ends.
	pipe := filepath.Join(t.TempDir(), "state.db")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	t.Cleanup(func() { close(ended) })
	go func() {
		for {
			select {
			case <-ended:
				return
			case <-time.After(100 * time.Millisecond):
			}
			if w, err := os.OpenFile(pipe, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				w.Close()
			}
		}
	}()
	// a store that is no database, and a database that holds no messages
	junk := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(junk, []byte("no database\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", bare)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`create table other (x)`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(loopDir(), "runs", "earlier")
	if err := os.MkdirAll(run, 0o700); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(run, "state.db")
	hermesStore(planted)
	event := func(key, source, path string, msg int, session, agent string) store.Event {
		return store.Event{Source: source, SourceKey: key, TS: time.Now().Add(-time.Minute), Session: session, Agent: agent, Harness: "hermes",
			Actor: store.ActorTool, Kind: "tool_result", Summary: "fixture summary", Ref: fmt.Sprintf("%s@%d#0", path, msg)}
	}
	call := event("call", store.Transcripts, path, 2, "phone", "")
	result := event("result", store.Transcripts, path, 3, "phone", "")
	delegated := event("delegated", store.Transcripts, path, 4, "phone", "sub")
	other := event("other", store.Transcripts, path, 4, "phone", "")
	missing := event("missing", store.Transcripts, path, 99, "phone", "")
	long := event("long", store.Transcripts, path, 5, "phone", "")
	writable := event("writable", store.Transcripts, planted, 3, "phone", "")
	scan := event("scan", store.Supervise, path, 3, "phone", "")
	unparsed := event("unparsed", store.Transcripts, path, 6, "phone", "")
	piped := event("piped", store.Transcripts, pipe, 3, "phone", "")
	noDatabase := event("no-database", store.Transcripts, junk, 3, "phone", "")
	noMessages := event("no-messages", store.Transcripts, bare, 3, "phone", "")
	putEvents(t, call, result, delegated, other, missing, long, writable, scan, unparsed, piped, noDatabase, noMessages)

	out := showOne(t, result.ID())
	if !strings.Contains(out, "hermes-marker") || !strings.Contains(out, "call_h1") || !strings.Contains(out, `"timestamp":"1970-01-01T00:01:43.25Z"`) ||
		!strings.Contains(out, "************") || strings.Contains(out, "secret-value") || strings.Contains(out, "withheld") {
		t.Fatalf("Hermes result row: %q", out)
	}
	if out := showOne(t, call.ID()); !strings.Contains(out, "docker image prune <dangling>") || !strings.Contains(out, `"tool_calls":[{"id":"call_h1"`) ||
		strings.Contains(out, "secret-value") {
		t.Fatalf("Hermes call row: %q", out)
	}
	if out := showOne(t, "--bytes", "30", result.ID()); strings.Contains(out, "secret-value") || !strings.Contains(out, "(its first 30 of") {
		t.Fatalf("Hermes row not scrubbed whole before its cut: %q", out)
	}
	if out := showOne(t, delegated.ID()); !strings.Contains(out, "delegated-marker") {
		t.Fatalf("a delegation's row: %q", out)
	}
	for _, e := range []store.Event{other, missing} {
		if out := showOne(t, e.ID()); !strings.Contains(out, "(its record is gone)") || strings.Contains(out, "delegated-marker") {
			t.Fatalf("%s: %q", e.SourceKey, out)
		}
	}
	if out := showOne(t, long.ID()); !strings.Contains(out, "(oversized record withheld)") || strings.Contains(out, "xxxx") {
		t.Fatalf("oversized Hermes row: %q", out)
	}
	if out := showOne(t, writable.ID()); !strings.Contains(out, "(record withheld: it lies where a run can write)") || strings.Contains(out, "hermes-marker") {
		t.Fatalf("a Hermes store in a run's root: %q", out)
	}
	if out := showOne(t, scan.ID()); !strings.Contains(out, "database rows cannot be read") || strings.Contains(out, "hermes-marker") {
		t.Fatalf("a scan's reference into the Hermes store: %q", out)
	}
	if out := showOne(t, unparsed.ID()); !strings.Contains(out, `"tool_calls":"not json ["`) || strings.Contains(out, "withheld") {
		t.Fatalf("a row whose tool calls are no JSON: %q", out)
	}
	if out := showOne(t, piped.ID()); !strings.Contains(out, "(its record is not a regular file)") {
		t.Fatalf("a pipe as the Hermes store: %q", out)
	}
	for _, e := range []store.Event{noDatabase, noMessages} {
		if out := showOne(t, e.ID()); !strings.Contains(out, "(record withheld: its Hermes store cannot be read)") {
			t.Fatalf("%s: %q", e.SourceKey, out)
		}
	}
	t.Setenv("MOMENTUM_NOW", time.Now().UTC().Format(time.RFC3339Nano))
	if out := showOne(t, result.ID()); !strings.Contains(out, "historical Hermes message rows are unavailable") || strings.Contains(out, "hermes-marker") {
		t.Fatalf("as-of Hermes row: %q", out)
	}
}
