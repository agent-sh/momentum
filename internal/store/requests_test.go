package store

import (
	"context"
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
