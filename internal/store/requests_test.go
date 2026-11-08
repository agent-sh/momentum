package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// A session's spans are read by the session's index. Without the index named, SQLite reads every transcript event
// by the source's index for the GROUP BY, once for each session a request reads.
func TestASessionsSpansAreReadByItsIndex(t *testing.T) {
	st, _ := open(t)
	rows, err := st.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+threadSpansSQL, "s-x", Transcripts)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if p := strings.Join(plan, " / "); !strings.Contains(p, "USING INDEX events_session") {
		t.Fatalf("plan: %s", p)
	}
}

// A thread's read selects a shell's result only when it may show an artifact's line (one of ResultMarks) or the
// store cut it, and only of a call it selects: what shows nothing stays in the database.
func TestAThreadReadSelectsTheResultsThatMayShowAnArtifact(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	var evs []Event
	add := func(i int, input, output string) {
		at := time.UnixMilli(int64(1000 + 10*i))
		id := `{"tool_id":"call-` + string(rune('a'+i)) + `"}`
		evs = append(evs,
			Event{Source: Transcripts, SourceKey: "s-x.jsonl@" + string(rune('a'+i)) + "1", TS: at, Session: "s-x", Harness: "claude",
				Actor: ActorAgent, Kind: "tool_call", Tool: "Bash", Summary: input, Meta: id},
			Event{Source: Transcripts, SourceKey: "s-x.jsonl@" + string(rune('a'+i)) + "2", TS: at.Add(time.Millisecond), Session: "s-x",
				Harness: "claude", Actor: ActorTool, Kind: "tool_result", Tool: "Bash", Summary: output, Meta: id})
	}
	add(0, `{"command":"git commit -m x"}`, "[main 1a2b3c4] x")
	add(1, `{"command":"git commit -qm x"}`, "")
	add(2, `{"command":"git push origin main"}`, "To github.com:example/x.git 1111111..2222222 main -> main")
	add(3, `{"command":"gh pr create --fill"}`, "https://github.com/example/x/pull/9")
	add(4, `{"command":"git push origin main"}`, "hook: checking the tree…")
	add(5, `{"command":"git status"}`, "nothing to commit, working tree clean")
	add(6, `{"command":"ls"}`, "a -> b")
	if _, err := st.Put(ctx, evs); err != nil {
		t.Fatal(err)
	}
	read := ThreadRead{Shells: []string{"bash"}, CallMarks: []string{"git", "gh"}, ResultMarks: []string{"] ", " -> ", "/pull/"}, CutMark: "…"}
	var got []string
	if err := st.ThreadEvents(ctx, "s-x", read, func(e Event) error {
		if e.Kind == "tool_result" {
			got = append(got, e.Summary)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := "[main 1a2b3c4] x | To github.com:example/x.git 1111111..2222222 main -> main | https://github.com/example/x/pull/9 | hook: checking the tree…"
	if g := strings.Join(got, " | "); g != want {
		t.Fatalf("results:\n got %s\nwant %s", g, want)
	}
}

// The owner's checks are a kept table: a store from before them gains it at its next open and keeps its events, the
// read-only open refuses such a store until then, and a rebuild keeps every check, its proof and its undo's.
func TestTheOwnersChecksAreKept(t *testing.T) {
	ctx := context.Background()
	st, path := open(t)
	if _, err := st.Put(ctx, []Event{{Source: "mistakes", SourceKey: "k", TS: time.Now(), Actor: ActorTool, Kind: "tool-error"}}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tbl := range requestTableNames {
		if _, err := db.Exec(`DROP TABLE ` + tbl); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	if _, err := OpenReadOnly(path); !errors.Is(err, ErrNotCurrent) {
		t.Fatalf("a store without the checks read as current: %v", err)
	}
	if st, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if evs, err := st.Events(ctx, Query{Source: "mistakes"}); err != nil || len(evs) != 1 {
		t.Fatalf("the events of a store from before the checks: %d %v", len(evs), err)
	}
	c := RequestCheck{Request: "rabc12345678", Result: "commit:1a2b3c4", ResultAt: time.UnixMilli(900), Proof: "e1", ProofAt: time.UnixMilli(1000),
		Words: "the rename reads right to me", At: time.UnixMilli(1100)}
	if ok, err := st.CheckOnce(ctx, c, nil); err != nil || !ok {
		t.Fatalf("check: %v %v", ok, err)
	}
	// two checks of the same result on his same turn, made at once, leave one; one under an alias stands in the way
	again := c
	again.Request, again.At = "rold00000000", time.UnixMilli(1200)
	if ok, err := st.CheckOnce(ctx, again, []string{"rold00000000", "rabc12345678"}); err != nil || ok {
		t.Fatalf("a second check on the same turn: %v %v", ok, err)
	}
	if _, err := st.Rebuild(ctx, "mistakes", false); err != nil {
		t.Fatal(err)
	}
	u := RequestUndo{Proof: "e2", ProofAt: time.UnixMilli(1900), Words: "take my check back please"}
	undone, err := st.UndoRequestCheck(ctx, []string{"rabc12345678"}, time.UnixMilli(2000), u)
	if err != nil || undone.Proof != "e1" || undone.Words != c.Words || undone.Undo != u || !undone.ProofAt.Equal(c.ProofAt) {
		t.Fatalf("undo: %+v %v", undone, err)
	}
	if _, err := st.UndoRequestCheck(ctx, []string{"rabc12345678"}, time.UnixMilli(2100), RequestUndo{Proof: "e3"}); !errors.Is(err, ErrNoCheck) {
		t.Fatalf("an undo with none standing: %v", err)
	}
	checks, err := st.RequestChecks(ctx, nil, []string{"rabc1234"}, 4)
	if err != nil || len(checks) != 1 {
		t.Fatalf("checks: %+v %v", checks, err)
	}
	if got := checks[0]; got.Result != c.Result || !got.ResultAt.Equal(c.ResultAt) || got.Proof != "e1" || !got.ProofAt.Equal(c.ProofAt) ||
		got.Words != c.Words || !got.At.Equal(c.At) || !got.Undone.Equal(time.UnixMilli(2000)) || got.Undo.Proof != "e2" ||
		!got.Undo.ProofAt.Equal(u.ProofAt) || got.Undo.Words != u.Words {
		t.Fatalf("the check read back: %+v", got)
	}
}

// Only a standing check of the same result on a proof turn no older than the new one's stands in the way: a check on
// a later turn of his records, one on an older turn does not.
func TestACheckOnALaterTurnIsNotHeldBack(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	c := RequestCheck{Request: "rabc12345678", Result: "commit:1a2b3c4", ResultAt: time.UnixMilli(900), Proof: "e1", ProofAt: time.UnixMilli(1000),
		Words: "the rename reads right to me", At: time.UnixMilli(1100)}
	if ok, err := st.CheckOnce(ctx, c, nil); err != nil || !ok {
		t.Fatalf("check: %v %v", ok, err)
	}
	older := c
	older.Proof, older.ProofAt = "e0", time.UnixMilli(500)
	if ok, err := st.CheckOnce(ctx, older, nil); err != nil || ok {
		t.Fatalf("a check on an older turn: %v %v", ok, err)
	}
	later := c
	later.Proof, later.ProofAt = "e2", time.UnixMilli(2000)
	if ok, err := st.CheckOnce(ctx, later, nil); err != nil || !ok {
		t.Fatalf("a check on a later turn: %v %v", ok, err)
	}
}

// A request's checks are found by their index, by its id and by the start of its id: the table is never read whole.
func TestTheOwnersChecksAreFoundByTheirIndex(t *testing.T) {
	st, _ := open(t)
	rows, err := st.db.QueryContext(context.Background(), `EXPLAIN QUERY PLAN SELECT request FROM request_owner_checks
	  WHERE request IN (?) OR (request >= ? AND request < ? AND length(request) = ?) ORDER BY proof_at, id`, "rabc0000", "rabc", "rabc~", 8)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if p := strings.Join(plan, " / "); !strings.Contains(p, "request_owner_checks_request") || strings.Contains(p, "SCAN request_owner_checks") {
		t.Fatalf("plan: %s", p)
	}
}

// A store whose checks table is the first draft's (no words, no undo's turn time or words) gains those columns at
// its next writable open and keeps its rows, which keep no words; the read-only open refuses it until then.
func TestAFirstDraftChecksTableIsUpgraded(t *testing.T) {
	ctx := context.Background()
	st, path := open(t)
	st.Close()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TABLE request_owner_checks`, `CREATE TABLE request_owner_checks (id INTEGER PRIMARY KEY, request TEXT NOT NULL,
	  result TEXT NOT NULL, result_at INTEGER NOT NULL, proof TEXT NOT NULL, proof_at INTEGER NOT NULL, at INTEGER NOT NULL,
	  undone INTEGER NOT NULL DEFAULT 0, undo_proof TEXT NOT NULL DEFAULT '')`,
		`CREATE INDEX request_owner_checks_request ON request_owner_checks(request)`,
		`INSERT INTO request_owner_checks (request, result, result_at, proof, proof_at, at) VALUES ('rabc12345678', 'commit:1a2b3c4', 900, 'e1', 1000, 1100)`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	if _, err := OpenReadOnly(path); !errors.Is(err, ErrNotCurrent) {
		t.Fatalf("a store with the first draft's table read as current: %v", err)
	}
	if st, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	checks, err := st.RequestChecks(ctx, []string{"rabc12345678"}, nil, 4)
	if err != nil || len(checks) != 1 || checks[0].Proof != "e1" || checks[0].Words != "" || !checks[0].Undo.ProofAt.IsZero() {
		t.Fatalf("the first draft's check: %+v %v", checks, err)
	}
	if ok, err := st.CheckOnce(ctx, RequestCheck{Request: "rabc12345678", Result: "commit:1a2b3c4", ResultAt: time.UnixMilli(900), Proof: "e2",
		ProofAt: time.UnixMilli(2000), Words: "looks good to me", At: time.UnixMilli(2100)}, nil); err != nil || !ok {
		t.Fatalf("a check on the upgraded table: %v %v", ok, err)
	}
}
