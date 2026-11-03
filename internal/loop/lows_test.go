package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A report whose mechanism holds only an invisible character names no mechanism: the case store keeps a plain line,
// which drops it, so the run decided nothing. The case closes as insufficient evidence; the run does not fail as
// `findings` on a testing case with no mechanism.
func TestAnInvisibleOnlyFindingIsUndecided(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	report := `{"mechanism":"\u200b","alternatives":[],"fix":"take the lock first","summary":"the evidence cannot decide"}`
	result := `{"type":"result","subtype":"success","is_error":false,"num_turns":4,"total_cost_usd":1,"session_id":"s1","structured_output":` + report + `}`
	p := script(t, dir, "agent", `echo '{"type":"system","subtype":"init"}'
`+fixtureResult(result))
	r := runnerFor(t, st, []string{p})
	c := openCase(t, st, "failures", "a", 2)
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 {
		t.Fatalf("%+v %v", done, err)
	}
	if done[0].Run.Status != store.RunOK || done[0].Run.Failure != "" {
		t.Fatalf("the run: %s %q", done[0].Run.Status, done[0].Run.Failure)
	}
	got, _ := st.CaseByID(ctx, c.ID)
	if got.Status != store.CaseClosed || got.Reason != store.Insufficient || got.NextTest != "take the lock first" {
		t.Fatalf("the case: %s %s, mechanism %q, next test %q", got.Status, got.Reason, got.Mechanism, got.NextTest)
	}
}

// index.txt lists a withheld record too, under the name `withheld`, so an investigation knows the record exists and why
// it has no file; a later append of unit logs keeps the line, and the bytes count it.
func TestTheRecordIndexListsWithheldRecords(t *testing.T) {
	goodInputScrubber(t)
	at := time.Date(2025, 4, 10, 12, 0, 0, 0, time.UTC)
	events := []store.Event{{Source: "fixture", SourceKey: "kept", TS: at, Kind: "record"},
		{Source: "fixture", SourceKey: "held", TS: at.Add(time.Second), Kind: "record"}}
	o := recordFixture(t, events, `case "$5" in "`+events[1].ID()+`") printf '(record withheld: fixture)\n';; *) printf 'record\n';; esac`)
	o.Neighbours = -1
	o.IDs = []string{events[0].ID(), events[1].ID()}
	m, err := Records(context.Background(), o)
	if err != nil || m.Written != 1 || m.Withheld != 1 {
		t.Fatalf("manifest %+v: %v", m, err)
	}
	line := "withheld\t" + events[1].ID() + "\t2025-04-10T12:00:01Z\n"
	index, err := os.ReadFile(filepath.Join(o.Destination, "index.txt"))
	if err != nil || !strings.Contains(string(index), line) {
		t.Fatalf("the index does not list the withheld record: %q %v", index, err)
	}
	o.Units = []string{"fixture.service"}
	again, err := UnitLogs(context.Background(), o)
	if err != nil || again.Written != 2 {
		t.Fatalf("unit append %+v: %v", again, err)
	}
	index, err = os.ReadFile(filepath.Join(o.Destination, "index.txt"))
	if err != nil || !strings.Contains(string(index), line) {
		t.Fatalf("an append dropped the withheld line: %q %v", index, err)
	}
	total := len(index)
	for _, file := range again.Files {
		info, err := os.Stat(filepath.Join(o.Destination, file.Name))
		if err != nil {
			t.Fatal(err)
		}
		total += int(info.Size())
	}
	if again.Bytes != total {
		t.Fatalf("bytes %d, on disk %d", again.Bytes, total)
	}
}
