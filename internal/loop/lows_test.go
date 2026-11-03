package loop

import (
	"context"
	"database/sql"
	"errors"
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

// A split signal joined by a + whose line alone changed is a changed literal: the operator joins the fragments, and
// editing it touches the sequence even when neither fragment's line changed.
func TestChangedLiteralsJoinsAcrossAnEditedPlus(t *testing.T) {
	lines := []string{`msg := "permission "`, `+`, `"denied"`}
	got := changedLiterals(lines, []bool{false, true, false})
	if len(got) != 1 || got[0] != "permission denied" {
		t.Fatalf("an edited + between unchanged fragments: %q", got)
	}
	if got := changedLiterals(lines, []bool{false, false, false}); len(got) != 0 {
		t.Fatalf("an unchanged sequence: %q", got)
	}
}

// A resumed landing whose head already reached the default branch finishes its record without a push, so the gate
// call at the top of the resume is what keeps a repository the gate now refuses (busy, excluded) from being recorded
// as landed and deployed.
func TestAResumedLandingChecksTheGateBeforeFinishing(t *testing.T) {
	fakeManager(t)
	s, f, c, p := b4Reviewed(t)
	run(t, p.Host, "git", "push", "--quiet", "origin", p.Head+":refs/heads/"+p.Base)
	p.State, p.LandingHead, p.PublishedHead = Landing, p.Head, p.Head
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	s.Gate.Busy = func(context.Context, string) (string, error) { return "a session works here", nil }
	if _, err := s.Land(context.Background(), c.ID); !errors.Is(err, ErrRefused) {
		t.Fatalf("a resumed landing crossed the gate: %v", err)
	}
	got, _, err := s.Load(c.ID)
	if err != nil || got.State != Landing {
		t.Fatalf("the resumed landing finished past the gate: %s %v", got.State, err)
	}
	if head := originHead(t, f, p.Base); head != p.Head {
		t.Fatalf("the default branch is %s", head)
	}
}

// A note marked done after a task's end is not done in that task's copy, whether the copy is direct or cut from the
// master; one done before the end stays done. PutNote clears done, so the times are set in the store itself.
func TestAMasterCutClearsADoneAfterTheEnd(t *testing.T) {
	ctx := context.Background()
	goodInputScrubber(t)
	end := time.Date(2026, 2, 20, 12, 0, 0, 0, time.UTC)
	source := inputStore(t, []store.Event{{Source: "fixture", SourceKey: "k", TS: end.Add(-6 * 24 * time.Hour), Kind: "record", Summary: "marker-7f3a"}})
	st, err := store.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	taskEnd := end.Add(-5 * 24 * time.Hour)
	done := map[string]time.Time{"before": taskEnd.Add(-24 * time.Hour), "after": end.Add(-time.Hour)}
	for id := range done {
		if _, err := st.PutNote(ctx, store.Note{ID: id, Kind: store.NoteDecided, Title: id, At: taskEnd.Add(-2 * 24 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	db, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	for id, at := range done {
		if _, err := db.Exec("UPDATE notes SET done = ? WHERE id = ?", at.UnixMilli(), id); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	dir := t.TempDir()
	master, direct, cut := filepath.Join(dir, "master.sqlite"), filepath.Join(dir, "direct.sqlite"), filepath.Join(dir, "cut.sqlite")
	for _, o := range []StoreCopyOptions{
		{Source: source, Destination: master, Since: end.Add(-40 * 24 * time.Hour), Until: end, OmitInPlace: true},
		{Source: source, Destination: direct, Since: taskEnd.Add(-StoreWindow), Until: taskEnd, OmitInPlace: true},
		{Source: master, Destination: cut, Since: taskEnd.Add(-StoreWindow), Until: taskEnd, OmitInPlace: true, Master: true},
	} {
		if _, err := StoreCopy(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{direct, cut} {
		got := map[string]int64{}
		rows, err := inputPlainDB(t, path).Query("SELECT id, done FROM notes")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id string
			var d int64
			rows.Scan(&id, &d)
			got[id] = d
		}
		rows.Close()
		if got["after"] != 0 || got["before"] != done["before"].UnixMilli() {
			t.Errorf("%s: done %v", filepath.Base(path), got)
		}
	}
}

// A task whose end cannot be found (its symptom is not in the store) is left out of the master's window and fails
// alone: the master is still made for the others, and their inputs are built from it.
func TestATaskWithoutAnEndFailsAlone(t *testing.T) {
	ctx := context.Background()
	e, good := evalFixture(t)
	if err := e.pinRecordsBinary(); err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.ID = "end-not-found"
	bad.Symptom.Query.EventIDs = []string{strings.Repeat("0", 24)}
	dir, err := e.buildMaster(ctx, []GoldenTask{bad, good})
	if err != nil || dir == "" || e.master == "" {
		t.Fatalf("the master for the other tasks: %q %v", dir, err)
	}
	defer removeInputTree(dir)
	for _, task := range []GoldenTask{bad, good} {
		in := &taskInputs{remaining: 1}
		e.makeTask(ctx, in, task, nil)
		in.close()
		if (in.err != nil) != (task.ID == bad.ID) {
			t.Errorf("%s: %v", task.ID, in.err)
		}
	}
}

// A pass freezes the host binary once: the records child of every run in it is that copy, so an install or rebuild
// during the pass changes no run's records.
func TestALivePassRunsItsPinnedRecordsChild(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	r := runnerFor(t, st, fakeAgent(t, `{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":1}`))
	ev := store.Event{Source: "fixture", SourceKey: "pinned", Kind: "tool_result", Actor: store.ActorTool, TS: time.Now().Add(-time.Hour), Session: "s", Summary: "a failure"}
	if _, err := st.Put(ctx, []store.Event{ev}); err != nil {
		t.Fatal(err)
	}
	c, _, err := st.ObserveCase(ctx, store.Finding{Family: "failures", Signature: "pinned", Title: "case pinned",
		Evidence: []store.Evidence{{Ref: "ev:" + ev.ID(), At: ev.TS}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	r.pass = &passInputs{}
	defer r.pass.close()
	if err := r.prepareInputs(ctx); err != nil {
		t.Fatal(err)
	}
	script(t, filepath.Dir(r.Inputs.Momentum), filepath.Base(r.Inputs.Momentum), "printf 'changed program\\n'")
	run, err := r.prepareRun(ctx, c, c.Family, StepInvestigate)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(HostDir(run.Dir), "records", "ev-"+ev.ID()+".txt"))
	if err != nil || !strings.Contains(string(raw), "record") || strings.Contains(string(raw), "changed program") {
		t.Fatalf("the records child was not the pass's copy: %q %v", raw, err)
	}
}
