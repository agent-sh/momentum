package source

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// Ingest reads a shell result's wrapper by its top-level fields: a program's own nested "ok": false, under a
// wrapper that says it succeeded, stores no error flag; the wrapper's own failure still does.
func TestIngestReadsAWrapperByItsTopLevel(t *testing.T) {
	home := testHome(t)
	path := filepath.Join(home, ".hermes", "state.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TACIT_HERMES_DB", path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(hermesSchema + `
	  insert into sessions (id, source, created_source, model, started_at) values ('desk', 'cli', 'cli', 'gpt-6.1-sol', 100);
	  insert into messages (session_id, role, content, timestamp) values ('desk', 'user', 'check the health endpoint', 101);
	  insert into messages (session_id, role, content, tool_calls, finish_reason, timestamp) values ('desk', 'assistant', 'Checking.',
	    '[{"id":"call_e1","type":"function","function":{"name":"execute_code","arguments":"{}"}},{"id":"call_e2","type":"function","function":{"name":"terminal","arguments":"{}"}}]', 'tool_calls', 102);
	  insert into messages (session_id, role, content, tool_call_id, timestamp) values ('desk', 'tool', '{"status": "success", "output": "probed", "result": {"ok": false, "reason": "degraded"}}', 'call_e1', 103);
	  insert into messages (session_id, role, content, tool_call_id, timestamp) values ('desk', 'tool', '{"output": "wrapped", "exit_code": 2}', 'call_e2', 104);`); err != nil {
		t.Fatal(err)
	}
	st := openStore(t)
	ingest(t, transcripts(home), st)
	evs := all(t, st, "transcripts")
	if res := find(t, evs, "tool_result", "probed"); metaOf(t, res)["is_error"] == true {
		t.Errorf("a program's nested ok false stored as a failure: %s", res.Meta)
	}
	if res := find(t, evs, "tool_result", "wrapped"); metaOf(t, res)["is_error"] != true {
		t.Errorf("the wrapper's exit 2 stored as no failure: %s", res.Meta)
	}
}
