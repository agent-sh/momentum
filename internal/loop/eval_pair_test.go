package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/source"
	"github.com/agent-sh/momentum/internal/store"
)

func TestBothArmsOfATrialReadCapturedRawRecords(t *testing.T) {
	for _, kind := range []string{"transcript", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			e, task := evalFixture(t)
			ctx := context.Background()
			at := task.Cutoff.Add(-time.Hour)
			dir := filepath.Dir(e.Source)
			path := filepath.Join(dir, "records.jsonl")
			event := store.Event{Source: store.Transcripts, SourceKey: "raw", TS: at, Session: "session", Harness: "claude",
				Actor: store.ActorAgent, Kind: "agent_text", Summary: "raw record", Ref: path + "@0"}
			write(t, path, `{"timestamp":"`+at.Format(time.RFC3339)+`","text":"marker-7f3a"}`+"\n")
			if kind == "hermes" {
				path = filepath.Join(dir, "rows.db")
				cmd := script(t, dir, "make-rows", `python3 -I -c 'import sqlite3,sys; c=sqlite3.connect(sys.argv[1]); c.execute("create table messages (id integer primary key, session_id text, role text, content text, tool_calls text, timestamp real)"); c.execute("insert into messages values (1,?,?,?,NULL,?)",("session","assistant","marker-7f3a",float(sys.argv[2]))); c.commit()' "`+path+`" "`+fmt.Sprint(float64(at.UnixMilli())/1000)+`"`)
				if out := run(t, dir, cmd); strings.TrimSpace(out) != "" {
					t.Fatal(out)
				}
				event.Harness, event.Ref = "hermes", path+"@1#0"
				event.Meta = `{"row_digest":"` + source.HermesRowDigest("marker-7f3a", "") + `"}`
			}
			st, err := store.Open(e.Source)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.Put(ctx, []store.Event{event}); err != nil {
				t.Fatal(err)
			}
			st.Close()
			realDoor(t, e)
			first := filepath.Join(dir, "first-read")
			log := filepath.Join(dir, "raw-reads")
			mutation := `rm "` + path + `"`
			if kind == "hermes" {
				mutation = `python3 -I -c 'import sqlite3,sys; c=sqlite3.connect(sys.argv[1]); c.execute("delete from messages"); c.commit()' "` + path + `"`
			}
			// The door runs outside the sandbox. Prune the source after the first arm has read it.
			real := e.Runner.Momentum
			e.Runner.Momentum = script(t, dir, "read-and-prune", `"`+real+`" "$@" > "`+first+`"
cat "`+first+`"; cat "`+first+`" >> "`+log+`"
if test -f "`+path+`"; then `+mutation+`; fi`)
			harness := filepath.Dir(e.Runner.Agent.Runner[0])
			e.Runner.Agent.Runner = []string{script(t, harness, "read-raw", `momentum evidence show `+event.ID()+` > door-answer.txt
`+modelResult(investigatedResult))}
			trials, err := e.Run(ctx, []GoldenTask{task})
			if err != nil || len(trials) != 2 {
				t.Fatalf("trials: %d %v", len(trials), err)
			}
			for _, trial := range trials {
				if trial.Err != "" {
					t.Fatal(trial.Err)
				}
				raw, err := os.ReadFile(filepath.Join(trial.Run.Dir, "door-answer.txt"))
				if err != nil || !strings.Contains(string(raw), "marker-7f3a") || strings.Contains(string(raw), "gone") {
					t.Fatalf("%s read different raw evidence: %s %v", trial.Arm, raw, err)
				}
			}
		})
	}
}

// A default branch reset or rebased after the end leaves its commit at the end only in the reflog, which a clone does
// not carry: the commit dates set the clone then, and the trials run.
func TestACloneFallsBackWhenTheReflogsCommitIsGone(t *testing.T) {
	e, task := evalFixture(t)
	end, err := e.snapshotEnd(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	other := filepath.Join(dir, "other")
	historyGit(t, dir, end.Add(-48*time.Hour), "init", "-q", "-b", "main", other)
	first := commitAt(t, other, end.Add(-48*time.Hour), "job", "a", "A")
	commitAt(t, other, end.Add(-24*time.Hour), "job", "b", "B, a local commit main held at the end")
	historyGit(t, other, end.Add(24*time.Hour), "reset", "-q", "--hard", "HEAD~1")
	commitAt(t, other, end.Add(25*time.Hour), "job", "c", "C")
	e.Repositories = []string{other}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trs {
		if tr.Err != "" || tr.Run.Started.IsZero() {
			t.Fatalf("%s: %q", tr.Arm, tr.Err)
		}
		if got := historyGit(t, filepath.Join(tr.Run.Dir+"-repos", "repo-1"), time.Time{}, "rev-parse", "HEAD"); got != first {
			t.Fatalf("%s: the clone stands at %s, want %s", tr.Arm, got, first)
		}
	}
}

// moving is a door program that does what the owner's machine does between the two arms of a trial: on each call it
// runs the script it is given (a source store ingesting, a repository reset), and it logs the arm's store it read,
// how many events it holds.
func moving(t *testing.T, dir, doing string) string {
	t.Helper()
	return script(t, dir, "momentum", `n=$(python3 -I -c 'import sqlite3,sys; print(sqlite3.connect("file:"+sys.argv[1]+"?mode=ro", uri=True).execute("select count(*) from events").fetchone()[0])' "$MOMENTUM_HOME/momentum.db")
printf '%s %s\n' "$MOMENTUM_LOOP_DIR" "$n" >> "`+filepath.Join(dir, "reads")+`"
`+doing+`
echo ok`)
}

// Both arms of a trial read one store as of its end, made before either runs, whatever the source holds by the time
// the second runs; and the arms take turns going first.
func TestBothArmsOfATrialReadOneStoreAndTakeTurns(t *testing.T) {
	e, task := evalFixture(t)
	e.Trials = 2
	before := task.Cutoff.Add(-2 * time.Hour).UnixMilli() // inside every trial's window
	door := filepath.Join(filepath.Dir(filepath.Dir(e.Source)), "moving")
	// each arm's call to the door adds an event of the window to the source store, as an ingest between the arms
	// would: the arm that runs second must not see it
	e.Runner.Momentum = moving(t, door, `python3 -I -c 'import sqlite3,sys; c=sqlite3.connect(sys.argv[1]); c.execute("insert into events (id,source,source_key,ts,actor,kind,summary,ingested) values (?,?,?,?,?,?,?,?)", ("late-"+sys.argv[3],"transcripts","late-"+sys.argv[3],int(sys.argv[2]),"agent","agent_text","a rewrite between the arms",0)); c.commit()' `+e.Source+` `+fmt.Sprint(before)+` "$$"`)
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil {
		t.Fatal(err)
	}
	arms := map[string]Trial{}
	for _, tr := range trs {
		if tr.Err != "" {
			t.Fatal(tr.Err)
		}
		arms[tr.Run.Dir] = tr
	}
	raw, _ := os.ReadFile(filepath.Join(door, "reads"))
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 4 {
		t.Fatalf("runs: %q", raw)
	}
	counts := map[int][]string{}
	var first []string
	for _, line := range lines {
		dir, n, _ := strings.Cut(line, " ")
		tr := arms[dir]
		if len(counts[tr.N]) == 0 {
			first = append(first, tr.Arm)
			if tr.Position != 1 {
				t.Errorf("the arm that ran first records position %d", tr.Position)
			}
		}
		counts[tr.N] = append(counts[tr.N], n)
	}
	for n, c := range counts {
		if len(c) != 2 || c[0] != c[1] {
			t.Errorf("trial %d: its arms read different stores: %v", n, c)
		}
	}
	if len(first) != 2 || first[0] == first[1] {
		t.Errorf("the same arm went first in both trials: %v", first)
	}
}

// A source repository reset while the first arm runs gives both arms the same commit: the checkouts are made once
// for the trial, with its store, and each arm copies them.
func TestBothArmsOfATrialReadOneSetOfCheckouts(t *testing.T) {
	e, task := evalFixture(t)
	end, _ := e.snapshotEnd(context.Background(), task)
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	historyGit(t, dir, end.Add(-48*time.Hour), "init", "-q", "-b", "main", src)
	a := commitAt(t, src, end.Add(-48*time.Hour), "job", "a", "A")
	b := commitAt(t, src, end.Add(-24*time.Hour), "job_test", "the job must take its lock before it reads the lockfile", "B, a failing test")
	e.Repositories = []string{src}
	door := filepath.Join(dir, "door")
	// the first command of the first arm is the owner resetting the repository to A, which leaves B in no branch
	e.Runner.Momentum = script(t, door, "momentum", `if mkdir "`+filepath.Join(door, "moved")+`" 2>/dev/null; then git -C "`+src+`" reset -q --hard `+a+`; fi
echo ok`)
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
	}
	if got := historyGit(t, src, time.Time{}, "rev-parse", "HEAD"); got != a {
		t.Fatalf("the source was not reset: %s", got)
	}
	for _, tr := range trs {
		if tr.Err != "" {
			t.Fatal(tr.Err)
		}
		repo := filepath.Join(tr.Run.Dir+"-repos", "repo-1")
		if got := historyGit(t, repo, time.Time{}, "rev-parse", "HEAD"); got != b {
			t.Errorf("%s: its checkout stands at %s, want %s", tr.Arm, got, b)
		}
		if _, err := os.Stat(filepath.Join(repo, "job_test")); err != nil {
			t.Errorf("%s: %v", tr.Arm, err)
		}
	}
}

// The arms take turns going first with the trial's number and the task's place together, so each goes first in half
// the pairs for any k, whatever order --arms names them; the order is the same for both spellings, and each trial
// records its position.
func TestEachArmGoesFirstInHalfTheTrials(t *testing.T) {
	for _, arms := range [][]string{{"loop", "native"}, {"native", "loop"}} {
		e, a := evalFixture(t)
		b := a
		b.ID = "second-task"
		e.Trials, e.Arms = 4, arms
		trs, err := e.Run(context.Background(), []GoldenTask{a, b})
		if err != nil || len(trs) != 16 {
			t.Fatalf("%v: %d trials %v", arms, len(trs), err)
		}
		first := map[string]int{}
		for _, tr := range trs {
			if tr.Err != "" {
				t.Fatal(tr.Err)
			}
			if tr.Position == 1 {
				first[tr.Arm]++
			}
			if tr.Position != 1 && tr.Position != 2 {
				t.Errorf("%s trial %d of %s: position %d", tr.Arm, tr.N, tr.Task, tr.Position)
			}
		}
		if first["loop"] != 4 || first["native"] != 4 {
			t.Errorf("--arms %v: first in %v of 8 pairs", arms, first)
		}
	}
	// an odd k over two tasks: the task's place balances what the trial's number cannot, so each arm still goes first in
	// half the pairs
	e, a := evalFixture(t)
	b := a
	b.ID = "second-task"
	e.Trials = 3
	trs, err := e.Run(context.Background(), []GoldenTask{a, b})
	if err != nil || len(trs) != 12 {
		t.Fatalf("%d trials %v", len(trs), err)
	}
	first := map[string]int{}
	for _, tr := range trs {
		if tr.Position == 1 {
			first[tr.Arm]++
		}
	}
	if first["loop"] != 3 || first["native"] != 3 {
		t.Errorf("k=3 over two tasks: first in %v of 6 pairs", first)
	}
	// k odd and one task: the first-named arm does not go first in two of three trials of every task
	got := ""
	for n := 1; n <= 3; n++ {
		for _, arms := range [][]string{{"loop", "native"}, {"native", "loop"}} {
			got += armOrder(canonicalArms(arms), n, 0)[0][:1]
		}
	}
	if got != "llnnll" {
		t.Errorf("who goes first in three trials: %s", got)
	}
}

func canonicalArms(arms []string) []string {
	e := &Eval{Arms: arms}
	return e.arms()
}
