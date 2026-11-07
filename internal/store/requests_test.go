package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A request's checks are found by their index, by its id and by the start of its id: the table is never read whole.
func TestRequestChecksAreFoundByTheirIndex(t *testing.T) {
	st, _ := open(t)
	rows, err := st.db.QueryContext(context.Background(), `EXPLAIN QUERY PLAN SELECT request, result, at, undone FROM request_checks
	  WHERE request IN (?) OR (request >= ? AND request < ? AND length(request) = ?) ORDER BY at, id`, "rabc0000", "rabc", "rabc~", 8)
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
	if p := strings.Join(plan, " / "); !strings.Contains(p, "request_checks_request") || strings.Contains(p, "SCAN request_checks") {
		t.Fatalf("plan: %s", p)
	}
}

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

// An uncheck undoes every check of the request that stands, so two made at once by an older build leave none.
func TestAnUncheckUndoesEveryStandingCheck(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := st.PutRequestCheck(ctx, RequestCheck{Request: "rabc12345678", Result: "commit:1a2b3c4", At: time.UnixMilli(int64(1000 + i))}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.UndoRequestCheck(ctx, []string{"rabc12345678"}, time.UnixMilli(2000)); err != nil {
		t.Fatal(err)
	}
	checks, err := st.RequestChecks(ctx, []string{"rabc12345678"}, nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range checks {
		if c.Undone.IsZero() {
			t.Fatalf("a check stands: %+v", checks)
		}
	}
	if ok, err := st.CheckOnce(ctx, RequestCheck{Request: "rabc12345678", Result: "commit:1a2b3c4", At: time.UnixMilli(3000)}, nil); err != nil || !ok {
		t.Fatalf("check again: %v %v", ok, err)
	}
	if ok, _ := st.CheckOnce(ctx, RequestCheck{Request: "rabc12345678", Result: "commit:1a2b3c4", At: time.UnixMilli(3001)}, nil); ok {
		t.Fatal("a second check of the same result was recorded")
	}
}
