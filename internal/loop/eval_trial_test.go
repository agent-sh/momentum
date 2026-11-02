package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// standInScrubber is a directory with a scrump that copies its input, which a door's momentum, a program of its own,
// finds first on its path: the real scrubber is not in a test, and the commands run in a loop's run scrub their
// output whole.
func standInScrubber(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script(t, dir, "scrump", "cat")
	return dir
}

// realDoor makes the evaluation's door run momentum's own binary, so a trial's commands are the commands themselves,
// on the trial's store and clock.
func realDoor(t *testing.T, e *Eval) {
	t.Helper()
	e.Runner.Momentum = realMomentum(t)
	e.Runner.Bins = []string{standInScrubber(t)}
}

// A trial runs the loop's investigation or the native path in the sandbox: the same system prompt, report and
// commands, asked the question with no procedure or skill. The loop's prompt holds its case's evidence and the runner
// writes its report into the case, which the judge reads; the native arm's report is what the judge reads. Neither
// may change a case itself, and neither reads the machine's rules as they are now, which hold the lessons of later
// fixes.
func TestATrialRunsTheLoopOrTheNativePath(t *testing.T) {
	for _, arm := range []string{"loop", "native"} {
		e, task := evalFixture(t)
		rules := filepath.Join(t.TempDir(), "rules.md")
		write(t, rules, "Delete with literal paths.")
		e.Runner.Rules, e.Arms = []string{rules}, []string{arm}
		harness := filepath.Dir(e.Runner.Agent.Runner[0])
		out := `{"mechanism":"the loop mechanism","alternatives":["another"],"fix":"the loop fix","summary":"looked"}`
		if arm == "native" {
			out = `{"mechanism":"the native mechanism","alternatives":["another"],"fix":"the native fix","summary":"found it"}`
		}
		body := `cat "$(printf '%s\n' "$@" | sed -n '/--append-system-prompt-file/{n;p}')" > system.txt
printf '%s\n' "$@" > args.txt
momentum case show "$MOMENTUM_LOOP_CASE" > case-shown.txt 2>&1; echo $? > case-shown.code
` + modelResult(fmt.Sprintf(`{"type":"result","subtype":"success","is_error":false,"num_turns":3,"total_cost_usd":0.5,"structured_output":%s}`, out))
		e.Runner.Agent.Runner = []string{script(t, harness, "arm-"+arm, body)}
		trs, err := e.Run(context.Background(), []GoldenTask{task})
		if err != nil || len(trs) != 1 || trs[0].Err != "" || trs[0].Grade.Verdict != "reached" || trs[0].Run.USD != 0.5 {
			t.Fatalf("%s: %+v %v", arm, trs, err)
		}
		tr := trs[0]
		system, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "system.txt"))
		args, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "args.txt"))
		judged, _ := os.ReadFile(filepath.Join(tr.JudgeDir, "prompt.txt"))
		shown, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "case-shown.txt"))
		code, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "case-shown.code"))
		switch {
		case !strings.Contains(string(system), "never wake him") || strings.Contains(string(system), "Delete with literal paths."):
			t.Errorf("%s: the system prompt lacks the owner's constraints, or holds the machine's rules as they are now:\n%s", arm, system)
		case !strings.HasPrefix(string(system), nativeBrief) || strings.Contains(string(system), "momentum's improvement loop"):
			t.Errorf("%s: not the native brief alone:\n%s", arm, system)
		case !strings.Contains(string(args), task.Symptom.Query.EventIDs[0]) || !strings.Contains(string(args), "--dangerously-skip-permissions") ||
			strings.Contains(string(args), "Follow the loop's procedure"):
			t.Errorf("%s: it is not given its events, or it is given a procedure:\n%s", arm, args)
		case strings.TrimSpace(string(code)) != "126" || !strings.Contains(string(shown), "not among the commands of an evaluation's trial"):
			t.Errorf("%s: it may read a case: exit %s %s", arm, code, shown)
		case arm == "loop" && (!strings.Contains(string(args), "The case's evidence, newest first") || !strings.Contains(string(args), "the worker failed") ||
			tr.Status != string(store.CaseTesting) || tr.Mechanism != "the loop mechanism" ||
			!strings.Contains(string(judged), "the loop mechanism") || !strings.Contains(string(judged), "the loop fix")):
			t.Errorf("loop: its case's evidence is not in its prompt, or its report is not on its case (status %q):\n%s\n%s", tr.Status, args, judged)
		case arm == "native" && (!strings.Contains(string(judged), "the native mechanism") || tr.Status != "native"):
			t.Errorf("native: the judge did not read its report (status %q):\n%s", tr.Status, judged)
		}
	}
}

// An evaluation reserves, for each arm of a trial, its investigation and every attempt of its judge, so a judge
// that is tried again does not take the evaluation past its budget: here every judge fails, and a run costs its
// whole budget.
func TestARetriedJudgeStaysWithinTheBudget(t *testing.T) {
	e, task := evalFixture(t)
	other := task
	other.ID = "second-task"
	third := task
	third.ID = "third-task"
	harness := filepath.Dir(e.Runner.Agent.Runner[0])
	e.Runner.Agent.Runner = []string{script(t, harness, "costly", modelResult(`{"type":"result","subtype":"success","total_cost_usd":4,"structured_output":{"mechanism":"m","fix":"f","summary":"s"}}`))}
	e.Judge.Runner = []string{script(t, harness, "failing-judge", `exit 1`)} // no result line: the judge's whole budget counts
	e.Parallel = 3
	// a pair takes a run ($4) and two judges ($1 each) for each of two arms: $12; $30 admits two pairs of the three
	e.Budget = 30
	trs, err := e.Run(context.Background(), []GoldenTask{task, other, third})
	if err != nil {
		t.Fatal(err)
	}
	spent, ran := 0.0, 0
	for _, tr := range trs {
		spent += tr.Run.USD + tr.JudgeUSD
		if !tr.Run.Started.IsZero() {
			ran++
		}
	}
	if spent > e.Budget || ran != 4 {
		t.Fatalf("spent $%.2f of $%.0f on %d runs", spent, e.Budget, ran)
	}
}

// The first of the three runs here is the loop's own door at work: a trial's momentum is the one the door runs, on
// the trial's store and clock, in its host-only command directory, with the repositories it may read and its case.
func TestATrialsDoorRunsOnTheTrialsClockAndStore(t *testing.T) {
	e, task := evalFixture(t)
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
	}
	end, err := e.snapshotEnd(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	lines := doorLines(t, e)
	if len(lines) != 2 {
		t.Fatalf("the door ran %q", lines)
	}
	for i, line := range lines {
		f := strings.Split(line, "|")
		if len(f) != 6 || f[0] != "evidence status --since 1d" || f[1] != end.Format(time.RFC3339Nano) {
			t.Fatalf("the door's command %d: %q (the clock should be %s)", i, line, end.Format(time.RFC3339Nano))
		}
		tr := trs[i]
		home := filepath.Join(e.Dir, "private", tr.Key)
		if !strings.HasPrefix(f[2], home+string(filepath.Separator)) || f[2] != filepath.Join(home, filepath.Base(tr.Run.Dir)) {
			t.Errorf("the store the door's command read: %q, want one of the trial's own under %s", f[2], home)
		}
		if f[3] != tr.Run.Dir+"-repos" || f[5] != HostDir(tr.Run.Dir) {
			t.Errorf("the repositories it may read %q and its directory %q, want %s and %s", f[3], f[5], tr.Run.Dir+"-repos", HostDir(tr.Run.Dir))
		}
		answer, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "door-answer.txt"))
		if !strings.Contains(string(answer), "the door answered: momentum evidence status") {
			t.Errorf("the harness read %q", answer)
		}
	}
	// the arm's store was its own and goes with the trial; the trial's record stays
	if _, err := os.Stat(filepath.Join(e.Dir, "private", trs[0].Key)); err == nil {
		t.Error("the arm's store outlived its trial")
	}
}

// What a trial's door runs is what the evaluation allows: both arms read the stored past, and tacit only when the
// trial is offered the store as of its end. An investigation keeps no case, so no case command runs; replay, the
// report and the ship tool read the machine as it is now. Each is refused with exit code 126, and nothing runs.
func TestATrialsDoorRunsOnlyTheEvaluationsCommands(t *testing.T) {
	e, task := evalFixture(t)
	harness := filepath.Dir(e.Runner.Agent.Runner[0])
	var commands = []string{"evidence events", "evidence status", "evidence show x", "evidence search abc", "evidence git . log", "evidence unit u",
		"case show c", "case list", "case set c --status testing", "case evidence c", "case close c", "supervise replay", "report", "ship status", "ship check",
		"loop lesson", "evidence ingest", "evidence file x", "evidence rebuild transcripts", "note add", "capacity", "sessions"}
	body := "for c in '" + strings.Join(commands, "' '") + "'; do\n  momentum $c > /dev/null 2>&1; echo \"$c => $?\" >> codes.txt\ndone\n" +
		"tacit search abc > /dev/null 2>&1; echo \"tacit search => $?\" >> codes.txt\n" +
		modelResult(`{"type":"result","subtype":"success","total_cost_usd":1,"structured_output":{"mechanism":"m","fix":"f","summary":"s"}}`)
	e.Runner.Agent.Runner = []string{script(t, harness, "commands", body)}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
	}
	for _, tr := range trs {
		raw, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "codes.txt"))
		got := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			c, code, _ := strings.Cut(line, " => ")
			got[c] = code
		}
		allowed := map[string]bool{"evidence events": true, "evidence status": true, "evidence show x": true, "evidence search abc": true, "evidence git . log": true, "evidence unit u": true}
		for _, c := range append(commands, "tacit search") {
			want := "126"
			if allowed[c] {
				want = "0"
			}
			if got[c] != want {
				t.Errorf("%s: %q exited %q, want %s", tr.Arm, c, got[c], want)
			}
		}
	}
	// what was refused ran nothing: the door's program saw only the allowed commands, and no tacit ran
	for _, line := range doorLines(t, e) {
		cmd, _, _ := strings.Cut(line, "|")
		for _, bad := range []string{"replay", "report", "ship", "lesson", "ingest", "file", "rebuild", "note", "capacity", "sessions"} {
			if strings.HasPrefix(cmd, bad) || strings.Contains(cmd, " "+bad) && !strings.HasPrefix(cmd, "evidence unit") {
				t.Errorf("the door ran %q", cmd)
			}
		}
	}
}

// A trial's agent runs where what it must not reach does not exist: the golden file with its answers, the store the
// snapshots are made from, the evaluation's other directories and the pair's inputs are not in its sandbox, its home
// is empty, and its checkouts are mounted read-only. Each of these reads is expected to fail inside, and each
// succeeds outside.
func TestATrialsSandboxHoldsOnlyWhatItIsGiven(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "private-notes"), "marker-7f3a")
	t.Setenv("HOME", home)
	e, task := evalFixture(t)
	fixture := filepath.Dir(filepath.Dir(e.Source))
	golden := filepath.Join(fixture, "golden", "golden.jsonl")
	write(t, golden, "the answers: marker-7f3a")
	src := filepath.Join(fixture, "checkout-source")
	run(t, fixture, "git", "init", "-q", "-b", "main", src)
	commitAt(t, src, time.Now().Add(-100*24*time.Hour), "job", "before", "before")
	e.Repositories = []string{src}
	probe := func(path string) string {
		return `if [ -e '` + path + `' ]; then echo 'present ` + path + `'; else echo 'absent ` + path + `'; fi >> reach.txt` + "\n"
	}
	body := probe(filepath.Join(home, "private-notes")) + probe(golden) + probe(e.Source) + probe(filepath.Join(e.Dir, "pairs")) + probe(filepath.Join(e.Dir, "bin")) + probe(filepath.Join(e.Dir, "private")) +
		probe(filepath.Join(e.Dir, "judge")) + probe(src) + probe(filepath.Dir(e.Runner.Agent.Runner[0])) +
		`ls -A "$HOME" | wc -l > home.txt
cat "$PWD-repos/repo-1/job" > checkout.txt 2>&1
touch "$PWD-repos/repo-1/new" 2> write-checkout.txt; echo $? >> write-checkout.txt
touch "$PWD/new" ; echo $? > write-own.txt
` + modelResult(`{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":1,"structured_output":{"mechanism":"m","fix":"f","summary":"s"}}`)
	e.Runner.Agent.Runner = []string{script(t, filepath.Dir(e.Runner.Agent.Runner[0]), "reaches", body)}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
	}
	for _, p := range []string{filepath.Join(home, "private-notes"), golden, e.Source, src} { // outside, the same paths are there
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("outside the sandbox %s is not there either: %v", p, err)
		}
	}
	for _, tr := range trs {
		if tr.Err != "" {
			t.Fatal(tr.Err)
		}
		reach, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "reach.txt"))
		for _, want := range []string{"absent " + filepath.Join(home, "private-notes"), "absent " + golden, "absent " + e.Source, "absent " + filepath.Join(e.Dir, "pairs"), "absent " + filepath.Join(e.Dir, "bin"),
			"absent " + filepath.Join(e.Dir, "private"), "absent " + filepath.Join(e.Dir, "judge"), "absent " + src, "present " + filepath.Dir(e.Runner.Agent.Runner[0])} {
			if !strings.Contains(string(reach), want+"\n") {
				t.Errorf("%s: want %q in\n%s", tr.Arm, want, reach)
			}
		}
		for f, want := range map[string]string{"home.txt": "0\n", "checkout.txt": "before", "write-own.txt": "0\n"} {
			if got, _ := os.ReadFile(filepath.Join(tr.Run.Dir, f)); string(got) != want {
				t.Errorf("%s: %s is %q, want %q", tr.Arm, f, got, want)
			}
		}
		if got, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "write-checkout.txt")); !strings.HasSuffix(string(got), "1\n") {
			t.Errorf("%s: a write to its checkout was not refused: %q", tr.Arm, got)
		}
		if _, err := os.Stat(filepath.Join(tr.Run.Dir+"-repos", "repo-1", "new")); err == nil {
			t.Errorf("%s: it wrote into its checkout", tr.Arm)
		}
	}
}

// An investigation that never started is no arm's miss: it costs nothing, has no start, keeps its failure as the
// trial's error, and its pair leaves the comparison as a one-armed trial. A scrubber that fails on the native arm's
// prompt (it names the symptom events, where the loop's lists its case's evidence) only stops that arm before its
// harness starts.
func TestAnInvestigationThatNeverStartedIsNoArmsMiss(t *testing.T) {
	old := Scrubber
	t.Cleanup(func() { Scrubber = old })
	Scrubber = []string{script(t, t.TempDir(), "scrub", `input=$(cat); case "$input" in *"Symptom events:"*) exit 1;; esac; printf '%s' "$input"`)}
	e, task := evalFixture(t)
	e.Trials = 2
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 4 {
		t.Fatalf("%+v %v", trs, err)
	}
	for _, tr := range trs {
		switch tr.Arm {
		case "native":
			if !tr.Run.Started.IsZero() || tr.Run.USD != 0 || !strings.Contains(tr.Err, "did not start") || tr.Grade.Verdict != "" || tr.JudgeUSD != 0 {
				t.Errorf("native trial %d: %+v", tr.N, tr)
			}
		case "loop":
			if tr.Run.Started.IsZero() || tr.Err != "" || tr.Grade.Verdict != "reached" {
				t.Errorf("loop trial %d: %+v", tr.N, tr)
			}
		}
	}
	r := e.Results(trs)
	loop, native := r.Arms[0], r.Arms[1]
	// a trial one arm ran without the other is skipped in the comparison in both arms, and not counted as a miss; the
	// failure stays on the trial that did not start
	if native.Ran != 0 || native.USD != 0 || native.Graded != 0 || native.Missed != 0 || native.Skipped != 2 || loop.Ran != 2 || loop.Skipped != 2 || loop.Graded != 0 ||
		loop.PassAtK != 0 || loop.ComparedTasks != 0 || native.ComparedTasks != 0 {
		t.Fatalf("a native arm that never started is read as a miss, or the loop's trial stays compared:\n%+v\n%+v", loop, native)
	}
	if len(r.Comparison.Tasks) != 0 || r.Comparison.ReachedPerDollar != nil {
		t.Errorf("the pair stays in the comparison: %+v", r.Comparison)
	}
}

// A golden task of no family of cases is refused before any trial runs: a trial opens its case in the store as of the
// end, which refuses it for both arms.
func TestAGoldenTaskOfNoCaseFamilyIsRefused(t *testing.T) {
	_, task := evalFixture(t)
	p := filepath.Join(t.TempDir(), "golden.jsonl")
	for _, family := range []string{"loop", "review", "no-such-family"} {
		task.Family = family
		raw, _ := json.Marshal(task)
		write(t, p, string(raw))
		if _, err := ReadGolden(p); err == nil {
			t.Errorf("a task of the family %q was read", family)
		}
	}
	for _, family := range []string{"failures", "owner-burden", "sessions", "capacity"} {
		task.Family = family
		raw, _ := json.Marshal(task)
		write(t, p, string(raw))
		if _, err := ReadGolden(p); err != nil {
			t.Errorf("a task of the family %q was refused: %v", family, err)
		}
	}
}

// What the evaluation reads from a directory its agent wrote in, it reads as a regular file and never through a
// link; what it writes there it writes over whatever the agent left at the name, a link included, so neither
// follows a link out of the directory.
func TestWhatTheEvaluationReadsAndWritesInAnAgentsDirectoryNeverFollowsALink(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "marker-file")
	write(t, target, "marker-7f3a: outside the run's directory")
	if err := os.Symlink(target, filepath.Join(dir, "findings.md")); err != nil {
		t.Fatal(err)
	}
	if raw, err := readRunFile(dir, "findings.md"); err == nil {
		t.Errorf("a link was read: %q", raw)
	}
	write(t, filepath.Join(dir, "plain.md"), "## Mechanism\ncause\n")
	if raw, err := readRunFile(dir, "plain.md"); err != nil || string(raw) != "## Mechanism\ncause\n" {
		t.Errorf("a regular file: %q %v", raw, err)
	}
	if err := os.Mkdir(filepath.Join(dir, "a-directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readRunFile(dir, "a-directory"); err == nil {
		t.Error("a directory was read")
	}
	write(t, filepath.Join(dir, "big.md"), strings.Repeat("x", runFileMax+1))
	if _, err := readRunFile(dir, "big.md"); err == nil {
		t.Error("a file over the bound was read")
	}
	// the trial's record goes over a link at its name, and its target stays as it was
	if err := os.Symlink(target, filepath.Join(dir, "trial.json")); err != nil {
		t.Fatal(err)
	}
	if err := writeOwn(filepath.Join(dir, "trial.json"), []byte(`{"kept":true}`)); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); !strings.HasPrefix(string(got), "marker-7f3a") {
		t.Errorf("a write followed a link out of the directory: %q", got)
	}
	if st, err := os.Lstat(filepath.Join(dir, "trial.json")); err != nil || !st.Mode().IsRegular() {
		t.Errorf("the record is not a regular file: %v %v", st, err)
	}
}

// With momentum's own commands behind the door, a trial reads what the store held at the end and the checkouts as
// they stood, on its clock: no event after the end, no commit after it, a search that stops at it, and no repository
// but its own checkouts. The same commands outside would read the machine as it is now.
func TestATrialReadsThePastThroughItsDoor(t *testing.T) {
	e, task := evalFixture(t)
	realDoor(t, e)
	ctx := context.Background()
	end, err := e.snapshotEnd(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	src, err := store.Open(e.Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Put(ctx, []store.Event{
		{Source: store.Transcripts, SourceKey: "before", TS: end.Add(-time.Hour), Session: "s", Actor: store.ActorAgent, Kind: "agent_text", Summary: "the lock was held before the end"},
		{Source: store.Transcripts, SourceKey: "after", TS: end.Add(2 * time.Hour), Session: "s", Actor: store.ActorAgent, Kind: "agent_text", Summary: "the lock was fixed after the end"}}); err != nil {
		t.Fatal(err)
	}
	src.Close()
	repo := filepath.Join(t.TempDir(), "golden-repo")
	run(t, filepath.Dir(repo), "git", "init", "-q", "-b", "main", repo)
	commitAt(t, repo, end.Add(-3*time.Hour), "job", "before", "the job, before the end")
	commitAt(t, repo, end.Add(72*time.Hour), "job", "after", "fix: take the lock first")
	e.Repositories = []string{repo}
	e.Arms = []string{"native"}
	harness := filepath.Dir(e.Runner.Agent.Runner[0])
	e.Runner.Agent.Runner = []string{script(t, harness, "reads-the-past", `momentum evidence events --since 60d > events.txt 2>&1
momentum evidence search "the lock" --since 60d > search.txt 2>&1
momentum evidence search "the lock" --since 60d --until `+end.Add(5*time.Hour).Format(time.RFC3339)+` > search-until.txt 2>&1
momentum evidence status --since 60d > status.txt 2>&1
momentum evidence git "$PWD-repos/repo-1" log --format=%s > git-log.txt 2>&1
momentum evidence git `+repo+` log --format=%s > git-source.txt 2>&1; echo $? >> git-source.txt
`+modelResult(`{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":1,"structured_output":{"mechanism":"m","fix":"f","summary":"s"}}`))}
	trs, err := e.Run(ctx, []GoldenTask{task})
	if err != nil || len(trs) != 1 || trs[0].Err != "" {
		t.Fatalf("%+v %v", trs, err)
	}
	read := func(name string) string {
		raw, _ := os.ReadFile(filepath.Join(trs[0].Run.Dir, name))
		return string(raw)
	}
	for _, name := range []string{"events.txt", "search.txt", "search-until.txt"} {
		if got := read(name); !strings.Contains(got, "the lock was held before the end") || strings.Contains(got, "after the end") {
			t.Errorf("%s reads past the end of store:\n%s", name, got)
		}
	}
	if got := read("status.txt"); !strings.Contains(got, "agent_text 1") {
		t.Errorf("status counts what came after the end:\n%s", got)
	}
	if got := read("git-log.txt"); !strings.Contains(got, "the job, before the end") || strings.Contains(got, "fix:") {
		t.Errorf("the checkout's history:\n%s", got)
	}
	if got := read("git-source.txt"); strings.Contains(got, "fix:") || !strings.Contains(got, "evidence git reads only repositories under loop.read_dirs") || !strings.HasSuffix(got, "2\n") {
		t.Errorf("a repository of the machine's was read:\n%s", got)
	}
	// outside the trial, the repository and the store do hold the later work
	if !strings.Contains(run(t, repo, "git", "log", "--format=%s"), "fix: take the lock first") {
		t.Fatal("the fixture's repository holds no later commit")
	}
}

// An arm named twice, or one that is neither the loop's nor the native path, is refused before anything runs.
func TestAnEvaluationRefusesAnInvalidOrRepeatedArm(t *testing.T) {
	for _, arms := range [][]string{{"loop", "loop"}, {"native", "other"}, {"loop,native"}} {
		e, task := evalFixture(t)
		e.Arms = arms
		if trs, err := e.Run(context.Background(), []GoldenTask{task}); err == nil || len(trs) != 0 {
			t.Errorf("--arms %v ran: %+v %v", arms, trs, err)
		}
	}
}

// Each arm of a trial has a store of its own, a copy of the pair's: what the runner records on the loop's case is not
// in the case the native arm's store holds, though both cases are opened from the same symptom.
func TestTheArmsOfATrialKeepTheirCasesApart(t *testing.T) {
	e, task := evalFixture(t)
	harness := filepath.Dir(e.Runner.Agent.Runner[0])
	e.Runner.Agent.Runner = []string{script(t, harness, "reports", modelResult(`{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":1,"structured_output":{"mechanism":"the loop mechanism","fix":"the loop fix","summary":"s"}}`))}
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
	}
	for _, tr := range trs {
		raw, _ := os.ReadFile(filepath.Join(tr.Run.Dir, "case.json"))
		var c struct{ Mechanism, Status string }
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("%s: %v %s", tr.Arm, err, raw)
		}
		want := "the loop mechanism"
		if tr.Arm == "native" {
			want = "" // it keeps no case, and the loop's is not its
		}
		if c.Mechanism != want {
			t.Errorf("%s: its case holds %q, want %q", tr.Arm, c.Mechanism, want)
		}
	}
}
