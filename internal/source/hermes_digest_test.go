package source

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/tacit/client/pkg/evidence"
)

func TestHermesDigestKeepsTheVersionParsedBeforeRedaction(t *testing.T) {
	home := testHome(t)
	path := filepath.Join(home, "records.db")
	t.Setenv("TACIT_HERMES_DB", path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(hermesSchema + `
	  insert into sessions (id, source, created_source, started_at) values ('session', 'mobile', 'mobile', 100);
	  insert into messages (session_id, role, content, timestamp) values ('session', 'user', 'marker-7f3a before', 101);`); err != nil {
		t.Fatal(err)
	}
	src := transcripts(home)
	st := openStore(t)
	src.records = recordsVia(func(ctx context.Context, evs []evidence.Event) ([]evidence.Event, error) {
		snapshots, err := filepath.Glob(filepath.Join(filepath.Dir(st.Path()), "momentum-hermes-*"))
		if err != nil || len(snapshots) != 1 {
			t.Fatalf("raw snapshot is not private scratch beside the store: %v %v", snapshots, err)
		}
		if _, err := db.Exec(`UPDATE messages SET content = 'marker-7f3a after' WHERE id = 1`); err != nil {
			t.Fatal(err)
		}
		return masked(ctx, evs)
	})
	ingest(t, src, st)
	if snapshots, _ := filepath.Glob(filepath.Join(filepath.Dir(st.Path()), "momentum-hermes-*")); len(snapshots) != 0 {
		t.Fatalf("raw snapshots left after ingest: %v", snapshots)
	}
	e := find(t, all(t, st, store.Transcripts), "owner_text", "marker-7f3a before")
	if got := metaOf(t, e)[HermesDigestKey]; got != HermesRowDigest("marker-7f3a before", "") {
		t.Fatalf("digest certified a later row: %v", got)
	}
}

// Hermes rewrites a message row in place, so an event keeps a digest of its row's content and tool calls as the
// ingest read them (the version a trial may be shown), and a later rewrite of the row does not change it. An event of
// another harness keeps none.
func TestTranscriptsKeepAHermesRowsDigestWithItsEvent(t *testing.T) {
	home := testHome(t)
	path := filepath.Join(home, ".hermes", "state.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TACIT_HERMES_DB", path)
	writeLines(t, claudePath(home), claudeSession()...)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	calls := `[{"id":"call_h1","type":"function","function":{"name":"terminal","arguments":"{}"}}]`
	output := `{"output": "permission denied", "exit_code": 1}`
	if _, err := db.Exec(hermesSchema + `
	  insert into sessions (id, source, created_source, model, started_at) values ('phone', 'mobile', 'mobile', 'gpt-6.1-sol', 100);
	  insert into messages (session_id, role, content, timestamp) values ('phone', 'user', 'prune the docker images', 101);
	  insert into messages (session_id, role, content, tool_calls, finish_reason, timestamp) values ('phone', 'assistant', 'Pruning.', '` + calls + `', 'tool_calls', 102);
	  insert into messages (session_id, role, content, tool_call_id, timestamp) values ('phone', 'tool', '` + output + `', 'call_h1', 103);`); err != nil {
		t.Fatal(err)
	}
	st := openStore(t)
	src := transcripts(home)
	ingest(t, src, st)
	evs := all(t, st, "transcripts")
	var call store.Event
	for _, e := range evs {
		if e.Kind == "tool_call" && e.Harness == "hermes" {
			call = e
		}
	}
	own := find(t, evs, "owner_text", "prune the docker images")
	for name, c := range map[string]struct {
		e    store.Event
		want string
	}{
		"owner":  {own, HermesRowDigest("prune the docker images", "")},
		"agent":  {find(t, evs, "agent_text", "Pruning."), HermesRowDigest("Pruning.", calls)},
		"call":   {call, HermesRowDigest("Pruning.", calls)}, // of the same row as the agent's text
		"result": {find(t, evs, "tool_result", "permission denied"), HermesRowDigest(output, "")},
	} {
		if got := metaOf(t, c.e)[HermesDigestKey]; got != c.want {
			t.Errorf("the %s event's digest %v, want %s", name, got, c.want)
		}
	}
	others := 0
	for _, e := range evs {
		if e.Harness == "hermes" {
			continue
		}
		others++
		if _, held := metaOf(t, e)[HermesDigestKey]; held {
			t.Errorf("an event of %s holds a digest: %+v", e.Harness, e)
		}
	}
	if others == 0 {
		t.Fatal("no event of another harness")
	}

	// the row is rewritten in place and a row is added: the event of the old row keeps the version the ingest read
	if _, err := db.Exec(`update messages set content = 'prune the docker images and the volumes' where id = 1;
	  insert into messages (session_id, role, content, timestamp) values ('phone', 'user', 'and the logs', 105);`); err != nil {
		t.Fatal(err)
	}
	ingest(t, src, st)
	evs = all(t, st, "transcripts")
	if got := metaOf(t, find(t, evs, "owner_text", "prune the docker images"))[HermesDigestKey]; got != HermesRowDigest("prune the docker images", "") {
		t.Errorf("the rewritten row's event changed its digest to %v", got)
	}
	if got := metaOf(t, find(t, evs, "owner_text", "and the logs"))[HermesDigestKey]; got != HermesRowDigest("and the logs", "") {
		t.Errorf("the new row's digest %v", got)
	}
}
