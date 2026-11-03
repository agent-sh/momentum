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

// A review whose recording meets the case lock a person's step holds is a hold, not the reviewer's failure: the stop
// rule does not count it, so two of them leave the case reviewable, and the next pass reviews it again.
func TestAReviewUnderAHeldLockIsAHoldTheStopRuleDoesNotCount(t *testing.T) {
	ctx := context.Background()
	r, c, _ := reviewFixture(t)
	r.Post = func(context.Context, string, string, string) (string, error) {
		return "https://example.invalid/c/1", nil
	}
	unlock, err := r.Ship.lock(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		done := r.reviews(ctx, "")
		if len(done) != 1 || done[0].Run.Status != store.RunFailed || done[0].Run.Failure != "hold" {
			t.Fatalf("review %d under a held lock: %+v", i, done)
		}
	}
	if stop, err := r.stopped(ctx, c, true); err != nil || stop != "" {
		t.Fatalf("two holds stopped the case: %q %v", stop, err)
	}
	unlock()
	done := r.reviews(ctx, "")
	p, _, _ := r.Ship.Load(c.ID)
	if len(done) != 1 || done[0].Run.Status != store.RunOK || len(p.Reviews) != 1 {
		t.Fatalf("the review after the hold: %+v, %d reviews", done, len(p.Reviews))
	}
}

// A package head that moved while the review ran (a revision, or the owner's proposal) refuses the verdict, and that
// is a hold too: the reviewer read what it was given, and the next pass reviews the new head.
func TestAHeadThatMovedDuringTheReviewIsAHold(t *testing.T) {
	ctx := context.Background()
	r, c, p := reviewFixture(t)
	r.Post = func(context.Context, string, string, string) (string, error) {
		return "https://example.invalid/c/1", nil
	}
	if err := r.prepareInputs(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := r.prepareRun(ctx, c, c.Family, StepReview)
	if err != nil {
		t.Fatal(err)
	}
	run, refusal := r.reservePrepared(ctx, run)
	if refusal != "" {
		t.Fatal(refusal)
	}
	moved := p
	moved.Head = p.BaseCommit
	if err := r.Ship.save(&moved); err != nil {
		t.Fatal(err)
	}
	d := r.review(ctx, c, p, run)
	got, _, _ := r.Ship.Load(c.ID)
	if d.Run.Status != store.RunFailed || d.Run.Failure != "hold" || len(got.Reviews) != 0 {
		t.Fatalf("a moved head: %s %q, %d reviews", d.Run.Status, d.Run.Failure, len(got.Reviews))
	}
}

// requestFields refuses each ambiguous body by itself: the relay's later JSON check would refuse some of them too,
// which hides a guard removed here.
func TestRequestFieldsRefusesEachAmbiguousBody(t *testing.T) {
	for _, body := range []string{
		`{"max_tokens":1,"messages":[]} {"x":1}`,
		`{"max_tokens":1,"messages":[]} 1`,
		`{"messages":[],"Messages":[]}`,
		`{"max_tokens":1,"max_tokens":1}`,
		`{"Max_tokens":1}`,
		`{"max_tokens":1.5}`,
		`[1]`,
	} {
		if _, err := requestFields([]byte(body)); err == nil {
			t.Errorf("%s was read", body)
		}
	}
	got, err := requestFields([]byte(`{"max_tokens":7,"messages":[]}`))
	if err != nil || got.MaxTokens == nil || *got.MaxTokens != 7 {
		t.Fatalf("an honest body: %v %v", got.MaxTokens, err)
	}
}

// An evaluation's unit set is the machine's units active in the three days before the task's end, never after it:
// the journal is asked for that window.
func TestEvaluationActiveUnitsReadTheJournalBeforeTheEnd(t *testing.T) {
	e, task := evalFixture(t)
	end, err := e.snapshotEnd(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(e.Runner.Inputs.Home, ".config/systemd/user", "busy.service"), "[Unit]\n")
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	script(t, bin, "journalctl", `printf '%s\n' "$@" > "`+argsFile+`"
printf 'busy.service\n'`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	e.Runner.Inputs.Momentum = script(t, t.TempDir(), "records", "printf 'record\\n'")
	if err := e.pinRecordsBinary(); err != nil {
		t.Fatal(err)
	}
	in := &taskInputs{remaining: 1}
	e.makeTask(context.Background(), in, task, nil)
	defer in.close()
	if in.err != nil {
		t.Fatal(in.err)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal("the journal was not asked", err)
	}
	args := "\n" + string(raw)
	format := "2006-01-02 15:04:05"
	if !strings.Contains(args, "\n--until\n"+end.Local().Format(format)+"\n") ||
		!strings.Contains(args, "\n--since\n"+end.Add(-ActiveUnitWindow).Local().Format(format)+"\n") {
		t.Fatalf("the journal's window is not the end's: %q (end %s)", raw, end)
	}
}
