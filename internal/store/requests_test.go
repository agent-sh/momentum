package store

import (
	"context"
	"strings"
	"testing"
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
