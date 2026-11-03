package loop

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func preparedEval(t *testing.T) (*Eval, GoldenTask, *taskInputs) {
	t.Helper()
	e, task := evalFixture(t)
	if err := e.pinRecordsBinary(); err != nil {
		t.Fatal(err)
	}
	in := &taskInputs{remaining: 1}
	e.makeTask(context.Background(), in, task, e.Repositories)
	t.Cleanup(in.close)
	if in.err != nil {
		t.Fatal(in.err)
	}
	return e, task, in
}

func TestEvaluationInputsPinTheStoreWindowAndMutableRows(t *testing.T) {
	e, task := evalFixture(t)
	ctx := context.Background()
	end, _ := e.snapshotEnd(ctx, task)
	st, err := store.Open(e.Source)
	if err != nil {
		t.Fatal(err)
	}
	events := []store.Event{
		{Source: "fixture", SourceKey: "too-old", TS: end.Add(-StoreWindow - time.Millisecond)},
		{Source: "fixture", SourceKey: "start", TS: end.Add(-StoreWindow)},
		{Source: "fixture", SourceKey: "tie", TS: end},
		{Source: "fixture", SourceKey: "later", TS: end.Add(time.Hour)},
		{Source: "incidents", SourceKey: "mutable", Kind: "incident", TS: end.Add(-time.Hour)},
		{Source: "radar", SourceKey: "mutable", Kind: "finding", TS: end.Add(-time.Hour)},
		{Source: "incidents", SourceKey: "sighting", Kind: "incident-seen", TS: end.Add(-time.Hour)},
	}
	for i := range events {
		events[i].Actor = store.ActorTool
		if events[i].Kind == "" {
			events[i].Kind = "tool_result"
		}
	}
	if _, err := st.Put(ctx, events); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := e.pinRecordsBinary(); err != nil {
		t.Fatal(err)
	}
	in := &taskInputs{remaining: 1}
	e.makeTask(ctx, in, task, nil)
	defer in.close()
	if in.err != nil {
		t.Fatal(in.err)
	}
	db, err := sql.Open("sqlite", sqliteInputURI(in.store, true))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT source_key FROM events ORDER BY source_key")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if !reflect.DeepEqual(keys, []string{"sighting", "start", "symptom"}) {
		t.Fatalf("task store bounds: %v", keys)
	}
	var sideTables int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT IN ('events','notes') AND name NOT LIKE 'sqlite_%'").Scan(&sideTables); err != nil || sideTables != 0 {
		t.Fatalf("answer-bearing tables remain: %d %v", sideTables, err)
	}
}

func TestEvaluationInputsFreezeRecordsUnitsAndPublishHashes(t *testing.T) {
	e, task := evalFixture(t)
	task.CaseTitle += " fixture.service"
	log := filepath.Join(t.TempDir(), "calls")
	e.Runner.Inputs.Momentum = script(t, filepath.Dir(log), "records", "printf 'call\\n' >> '"+log+"'\nprintf 'marker-7f3a frozen\\n'")
	if err := e.pinRecordsBinary(); err != nil {
		t.Fatal(err)
	}
	in := &taskInputs{remaining: 2}
	in.once.Do(func() { e.makeTask(context.Background(), in, task, nil) })
	if in.err != nil {
		t.Fatal(in.err)
	}
	dir := in.dir
	first := readText(t, filepath.Join(dir, "records", "unit-fixture.service.txt"))
	write(t, e.Runner.Inputs.Momentum, "#!/bin/sh\nprintf 'later answer\\n'\n")
	in.once.Do(func() { t.Fatal("task inputs were rebuilt") })
	sameTexts(t, first, readText(t, filepath.Join(dir, "records", "unit-fixture.service.txt")))
	if len(strings.Fields(readText(t, log))) != 2 {
		t.Fatal("records or unit logs captured more than once")
	}
	if in.hashes["/ro/records/unit-fixture.service.txt"] != digest(first) || in.hashes["/ro/store.sqlite"] == "" {
		t.Fatal("full capture and unit hash were not recorded")
	}
	in.release()
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("inputs removed while another trial still needed them")
	}
	in.release()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("task inputs remain after its last trial")
	}
}

func TestEvaluationCloneExcludesTheEndsSecondWithAndWithoutReflog(t *testing.T) {
	end := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	repo, before, _ := evalRepository(t, end, "example")
	atEnd := commitAt(t, repo, end, "answer", "marker-7f3a tie answer", "tie")
	for _, clear := range []bool{false, true} {
		if clear {
			historyGit(t, repo, time.Time{}, "reflog", "expire", "--expire=now", "--all")
		}
		dst := filepath.Join(t.TempDir(), "clone")
		made, err := repositoryAsOf(context.Background(), repo, end, dst)
		if err != nil || !made {
			t.Fatalf("clone: %v %v", made, err)
		}
		if got := gitRun(t, dst, "git", "rev-parse", "HEAD"); got != before {
			t.Fatal("commit at the end leaked through a reflog or date")
		}
		absentObject(t, dst, atEnd)
	}
}

func TestEvaluationTacitBeforeItsFirstCommitIsNotBound(t *testing.T) {
	e, task := evalFixture(t)
	end, _ := e.snapshotEnd(context.Background(), task)
	repo := filepath.Join(t.TempDir(), "facts")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	historyGit(t, repo, end.Add(time.Hour), "init", "-q", "-b", "main")
	commitAt(t, repo, end.Add(time.Hour), "fact", "marker-7f3a later answer", "later")
	e.Tacit = repo
	if err := e.pinRecordsBinary(); err != nil {
		t.Fatal(err)
	}
	in := &taskInputs{remaining: 1}
	e.makeTask(context.Background(), in, task, nil)
	defer in.close()
	if in.err != nil {
		t.Fatal(in.err)
	}
	if in.manifest.Tacit.Commit != "" || in.manifest.Tacit.Error == "" {
		t.Fatal("tacit was offered before it existed")
	}
	for _, bind := range in.binds {
		if bind.Dst == "/ro/clone" || bind.Src == repo {
			t.Fatal("today's tacit was bound")
		}
	}
}

func TestEvaluationHistoricalTacitAndRepositoryCopiesAreShared(t *testing.T) {
	e, task := evalFixture(t)
	end, _ := e.snapshotEnd(context.Background(), task)
	repo, repoBefore, repoAfter := evalRepository(t, end, "example")
	tacit, tacitBefore, tacitAfter := evalRepository(t, end, "facts")
	e.Tacit = tacit
	if err := e.pinRecordsBinary(); err != nil {
		t.Fatal(err)
	}
	in := &taskInputs{remaining: 1}
	e.makeTask(context.Background(), in, task, []string{repo})
	defer in.close()
	if in.err != nil {
		t.Fatal(in.err)
	}
	if in.manifest.Tacit.Commit != tacitBefore || in.manifest.Repositories[0].Commit != repoBefore {
		t.Fatal("as-of heads differ")
	}
	absentObject(t, filepath.Join(in.dir, "repos", "repo-1"), repoAfter)
	absentObject(t, filepath.Join(in.dir, "clone"), tacitAfter)
}

func TestEvaluationConfigurationLeaksAreInResults(t *testing.T) {
	e, task, in := preparedEval(t)
	tr := e.newTrial(task, 1, "loop")
	tr.Inputs = in.manifest
	raw, err := json.Marshal(e.Results([]Trial{tr}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Historical Hermes message rows are withheld") ||
		!strings.Contains(string(raw), `"leaks"`) || !strings.Contains(string(raw), ".config/agents") {
		t.Fatal("historical limitations or configuration leaks missing")
	}
}

func TestEvaluationRecordsRunThePinnedEvaluatingBinary(t *testing.T) {
	e, task := evalFixture(t)
	before := readText(t, e.Runner.Inputs.Momentum)
	if err := e.pinRecordsBinary(); err != nil {
		t.Fatal(err)
	}
	write(t, e.Runner.Inputs.Momentum, "#!/bin/sh\nprintf 'changed program\\n'\n")
	in := &taskInputs{remaining: 1}
	e.makeTask(context.Background(), in, task, nil)
	defer in.close()
	if in.err != nil {
		t.Fatal(in.err)
	}
	record := readText(t, filepath.Join(in.dir, "records", "ev-"+task.Symptom.Query.EventIDs[0]+".txt"))
	if record != "marker-7f3a record\n" || e.provenance().Programs["momentum"] != digest(before) {
		t.Fatal("a rebuild changed the records child or its recorded hash")
	}
}

func TestEvaluationBoundedCaptureIsRecordedAndExcluded(t *testing.T) {
	e, task := evalFixture(t)
	// a record over its bound (and the slack for the child's header and marker) is withheld and counted; the pair is
	// still equal on both arms and is compared
	over := strconv.Itoa(MaxRecordBytes + recordSlack + 1)
	e.Runner.Inputs.Momentum = script(t, t.TempDir(), "records", "python3 - <<'PY'\nprint('x'*"+over+")\nPY")
	if err := e.pinRecordsBinary(); err != nil {
		t.Fatal(err)
	}
	in := &taskInputs{remaining: 1}
	e.makeTask(context.Background(), in, task, nil)
	defer in.close()
	if in.err != nil {
		t.Fatal(in.err)
	}
	if in.manifest.Records.Withheld != 1 || in.manifest.Records.Stopped != "" {
		t.Fatalf("an oversized record stopped the capture or was kept: %+v", in.manifest.Records)
	}
	// a capture that reached its count or byte bound lacks evidence the task may need: its pairs leave the comparison
	in.manifest.Records.Stopped = "record count"
	tr := e.trial(context.Background(), in, task, 1, "loop", 1)
	if tr.Inputs.Records.Stopped != "record count" || tr.ComparisonExcluded == "" {
		t.Fatal("full capture was not recorded in the trial")
	}
	tr.Run.Started, tr.Run.Ended, tr.Run.USD, tr.Run.Status = time.Now(), time.Now().Add(time.Minute), 1, store.RunOK
	tr.Err, tr.Grade.Verdict = "", "reached"
	other := tr
	other.Arm = "native"
	for _, arm := range e.Results([]Trial{tr, other}).Arms {
		if arm.ComparedTasks != 0 || arm.Excluded != 1 || arm.USD != 1 {
			t.Fatal("bounded capture entered comparison or lost accounting")
		}
	}
}

func TestEvaluationRecordChildReceivesTheTaskClock(t *testing.T) {
	e, task := evalFixture(t)
	end, err := e.snapshotEnd(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	body := `python3 - <<'PY'
import os, datetime
record=datetime.datetime.fromisoformat('` + end.Format(time.RFC3339Nano) + `')
clock=os.environ.get('MOMENTUM_NOW')
if clock is not None and record >= datetime.datetime.fromisoformat(clock):
    print('(record withheld: fixture dated at the end)')
else:
    print('later answer')
PY`
	e.Runner.Inputs.Momentum = script(t, t.TempDir(), "records", body)
	if err := e.pinRecordsBinary(); err != nil {
		t.Fatal(err)
	}
	in := &taskInputs{remaining: 1}
	e.makeTask(context.Background(), in, task, nil)
	defer in.close()
	if in.err != nil || in.manifest.Records.Withheld != 1 || in.manifest.Records.Written != 0 {
		t.Fatal("records child did not withhold a record at the task's end")
	}
}
