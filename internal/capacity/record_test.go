package capacity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// excludedTargets is a targets root with idle build output of an excluded project, which a run only reports.
func excludedTargets(t *testing.T, names ...string) (Config, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "cache")
	for _, n := range names {
		cargoTarget(t, filepath.Join(root, "secret-"+n, "target"))
	}
	cfg := Config{Targets: TargetsConfig{Roots: []string{root}}, Exclude: Exclude{Names: []string{"secret-*"}}}.Normal()
	cfg.Live = []string{RuleTargets}
	return cfg, root
}

func TestAReportOnlyNoteStaysResolvedUntilItsEvidenceChanges(t *testing.T) {
	ctx := context.Background()
	cfg, root := excludedTargets(t, "a")
	st := openStore(t)
	now := time.Now().Add(72 * time.Hour)
	runAt := func(at time.Time) {
		t.Helper()
		o := options(t, procRoot(t), at, nil)
		o.Now = true
		if _, err := Run(ctx, cfg, st, o); err != nil {
			t.Fatal(err)
		}
	}
	id := "capacity:" + RuleTargets + ":excluded"
	runAt(now)
	n, ok, err := st.Note(ctx, id)
	if err != nil || !ok || n.Kind != store.NoteProblem || !strings.Contains(n.Title, "1 Rust build output") {
		t.Fatalf("the first report-only item puts its group's note: %+v %v %v", n, ok, err)
	}
	if _, _, err := st.DoneNote(ctx, id, now); err != nil {
		t.Fatal(err)
	}
	runAt(now.Add(time.Hour))
	if n, _, _ := st.Note(ctx, id); n.Done.IsZero() {
		t.Fatal("the same items again leave a note the owner resolved resolved")
	}
	cargoTarget(t, filepath.Join(root, "secret-b", "target"))
	runAt(now.Add(2 * time.Hour))
	if n, _, _ := st.Note(ctx, id); !n.Done.IsZero() || !strings.Contains(n.Title, "2 Rust build output") {
		t.Fatalf("a new item makes the note stand again, with both: %+v", n)
	}
	evs, err := st.Events(ctx, store.Query{Source: store.Capacity, Kinds: []string{KindReport}})
	if err != nil || len(evs) != 2 {
		t.Errorf("one event per item, not per run: %d %v", len(evs), err)
	}
	must(t, os.RemoveAll(filepath.Join(root, "secret-a")))
	must(t, os.RemoveAll(filepath.Join(root, "secret-b")))
	runAt(now.Add(3 * time.Hour))
	if n, _, _ := st.Note(ctx, id); n.Done.IsZero() {
		t.Error("a rule that finds none of the note's items any more resolves it")
	}
}

func TestAFastFillNamesItsWritersInANoteOncePerFill(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	var fs syscall.Statfs_t
	must(t, syscall.Statfs(dir, &fs))
	used := int64(fs.Blocks-fs.Bfree) * int64(fs.Bsize)
	st := openStore(t)
	now := time.Now()
	// 20 minutes ago the disk held 15 GiB less: 45 GiB an hour
	must(t, st.PutCapacityRun(ctx, store.CapacityRun{At: now.Add(-20 * time.Minute)},
		[]store.DiskSample{{Disk: dir, At: now.Add(-20 * time.Minute), Size: 1 << 40, Used: used - 15<<30, Avail: 1 << 39}}, nil))
	cfg := Config{Disks: []string{dir}}.Normal()
	proc := procRoot(t, fakeProc{pid: 21, ppid: 20, argv: []string{"cp"}, cwd: dir}, fakeProc{pid: 20, ppid: 1, argv: []string{"claude"}})
	calls := 0
	o := options(t, proc, now, nil)
	o.Rules = []string{RuleUV} // the deleting rules are not what this test is about
	o.Writes = func() map[int]int64 { calls++; return map[int]int64{21: int64(calls) * 200 << 20, 20: 0} }
	o.Sessions = func(context.Context) map[int]Session {
		return map[int]Session{20: {Key: "s1", Name: "copy job", Tool: "claude"}}
	}
	res, err := Run(ctx, cfg, st, o)
	if err != nil {
		t.Fatal(err)
	}
	d := res.Disks[0]
	if !d.Fast || d.Rate < 40 || len(d.Writers) != 1 || d.Writers[0].PID != 21 || !d.Writers[0].OnDisk ||
		!strings.Contains(d.Writers[0].Session, "copy job") {
		t.Fatalf("a 45 GiB/h fill is fast, and its writer, its directory and session are named: %+v", d)
	}
	id := "capacity:fill:" + dir
	n, ok, _ := st.Note(ctx, id)
	if !ok || n.Kind != store.NoteProblem || n.Importance != store.ImportanceHigh || !strings.Contains(n.Detail, "copy job") {
		t.Fatalf("the fill is a high problem note naming the writer: %+v", n)
	}
	if _, _, err := st.DoneNote(ctx, id, now); err != nil {
		t.Fatal(err)
	}
	o.Clock = func() time.Time { return now.Add(5 * time.Minute) }
	if _, err := Run(ctx, cfg, st, o); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := st.Note(ctx, id); n.Done.IsZero() {
		t.Error("the same fill five minutes on leaves the resolved note resolved")
	}
	evs, _ := st.Events(ctx, store.Query{Source: store.Capacity, Kinds: []string{KindFill}})
	if len(evs) != 2 {
		t.Errorf("each run that sees the fill records it: %d", len(evs))
	}
}

func TestJobsNameWhatTheyRun(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "models", "m1", "w.gguf")
	viaEnv := filepath.Join(dir, "models", "m2")
	viaScript := filepath.Join(dir, "models", "m3")
	viaHermes := filepath.Join(dir, "models", "m4")
	viaLink := filepath.Join(dir, "models", "m5", "bin", "tool")
	blob(t, viaLink, 10)
	env := filepath.Join(dir, "serve.env")
	write(t, env, "MODEL="+viaEnv+"\n")
	script := filepath.Join(dir, "job.sh")
	write(t, script, "#!/bin/sh\nexec run --model "+viaScript+" org/repo-x\n")
	scripts := filepath.Join(dir, "hermes-scripts")
	write(t, filepath.Join(scripts, "scan.py"), "PATH_X = '"+viaHermes+"'\n")
	jobsFile := filepath.Join(dir, "jobs.json")
	write(t, jobsFile, `{"jobs":[{"name":"scan","enabled":true,"script":"scan.py"},{"name":"off","enabled":false,"workdir":"`+filepath.Join(dir, "models", "m6")+`"}]}`)
	links := filepath.Join(dir, "bin")
	must(t, os.MkdirAll(links, 0o755))
	must(t, os.Symlink(viaLink, filepath.Join(links, "tool")))
	envText, _ := os.ReadFile(env) // a unit's environment files are part of its text, as SystemJobs reads it
	jobs := fakeJobs{units: map[string]string{"user:a.service": "[Service]\nEnvironmentFile=-" + env + "\nExecStart=/usr/bin/srv -m " + model + "\n" + string(envText)},
		crontab: "# a comment naming /nowhere\n*/5 * * * * " + script + "\n0 * * * * cd " + dir + " && ./run.sh --out results/x\n"}
	n, err := LoadNamed(context.Background(), jobs, NamesConfig{HermesJobs: jobsFile, HermesScripts: scripts, Links: []string{links}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(dir, "models", "m1"), viaEnv, viaScript, viaHermes, filepath.Join(dir, "models", "m5"),
		filepath.Join(dir, "run.sh"), filepath.Join(dir, "results")} {
		if _, ok := n.Protects(p); !ok {
			t.Errorf("%s is named, through a unit, its environment file, a cron script, a Hermes script or a link", p)
		}
	}
	for _, p := range []string{filepath.Join(dir, "models", "m6"), "/nowhere"} {
		if j, ok := n.Protects(p); ok {
			t.Errorf("%s is not named (a disabled job, a comment): %s", p, j)
		}
	}
	if _, ok := n.RepoIDs["org/repo-x"]; !ok {
		t.Error("a repository id in a job's script is named")
	}
}

// fillRun is a run that sees dir fill at 45 GiB an hour, with the processes and the written bytes given.
func fillRun(t *testing.T, st *store.Store, dir string, now time.Time, proc string, writes map[int]int64) *Result {
	t.Helper()
	cfg := Config{Disks: []string{dir}}.Normal()
	o := options(t, proc, now, nil)
	o.Rules = []string{RuleUV}
	o.Writes = func() map[int]int64 { return writes }
	res, err := Run(context.Background(), cfg, st, o)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestAFillNamesWhoWroteBetweenTheRunsAndEveryWriterItSaw(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	var fs syscall.Statfs_t
	must(t, syscall.Statfs(dir, &fs))
	used := int64(fs.Blocks-fs.Bfree) * int64(fs.Bsize)
	st := openStore(t)
	now := time.Now()
	must(t, st.PutCapacityRun(ctx, store.CapacityRun{At: now.Add(-25 * time.Minute)},
		[]store.DiskSample{{Disk: dir, At: now.Add(-25 * time.Minute), Size: 1 << 40, Used: used - 15<<30, Avail: 1 << 39}}, nil))
	// a training run (pid 31, under a shell) saves checkpoints between the looks: nothing in the seconds measured
	quiet := map[int]int64{}
	run := func(at time.Time, ps ...fakeProc) DiskState {
		proc := procRoot(t, ps...)
		write(t, filepath.Join(proc, "uptime"), "1201.00 1000.00\n")
		return fillRun(t, st, dir, at, proc, quiet).Disks[0]
	}
	shell := fakeProc{pid: 30, ppid: 1, argv: []string{"bash"}, cwd: dir, written: 1 << 30}
	editor := fakeProc{pid: 32, ppid: 1, argv: []string{"editor"}, cwd: dir, written: 1 << 29}
	train := fakeProc{pid: 31, ppid: 30, argv: []string{"torchrun"}, cwd: dir, written: 10 << 30}
	gone := fakeProc{pid: 33, ppid: 1, argv: []string{"old"}, cwd: dir, written: 9 << 30} // its pid is used again later
	if d := run(now.Add(-5*time.Minute), shell, editor, train, gone); !d.Fast || len(d.Writers) != 0 {
		t.Fatalf("the first run has no run before to compare with: %+v", d)
	}
	train.written = 50 << 30
	// a process two minutes old (started 1081 s after boot, 1201 s ago) that wrote 8 GiB
	late := fakeProc{pid: 33, ppid: 1, argv: []string{"python"}, cwd: dir, written: 8 << 30, start: 108100}
	d := run(now, shell, editor, train, late)
	got := map[int]Writer{}
	for _, w := range d.Writers {
		got[w.PID] = w
	}
	if w := got[31]; w.Written != 40<<30 || w.Over < 299.9 || w.Over > 300.1 {
		t.Errorf("the training run wrote 40 GiB in the 5 minutes since the run before: %+v", got)
	}
	if w := got[33]; w.Written != 8<<30 || w.Over < 119 || w.Over > 121 {
		t.Errorf("a process that started since wrote all it wrote since it started: %+v", got)
	}
	if _, ok := got[32]; ok {
		t.Errorf("a process that wrote nothing since is no writer: %+v", got)
	}
	id := "capacity:fill:" + dir
	if n, _, _ := st.Note(ctx, id); !strings.Contains(n.Detail, "torchrun (pid 31) wrote 40.0 GiB in 5m0s") ||
		!strings.Contains(n.Detail, "python (pid 33) wrote 8.0 GiB in 2m0s") {
		t.Fatalf("the note says who wrote what, and in how long: %s", n.Detail)
	}
	// five minutes on the training run has ended: its shell took in what it wrote last; python wrote 8 GiB more
	shell.written, late.written = 31<<30, 16<<30
	run(now.Add(5*time.Minute), shell, editor, late)
	n, _, _ := st.Note(ctx, id)
	for _, want := range []string{"bash (pid 30) wrote 30.0 GiB in 5m0s", "torchrun (pid 31) wrote 40.0 GiB in 5m0s",
		"python (pid 33) wrote 16.0 GiB in 7m0s"} {
		if !strings.Contains(n.Detail, want) {
			t.Errorf("the note names every writer the fill's runs saw, with all it wrote in them (%s): %s", want, n.Detail)
		}
	}
}

func TestOnlyARunThatTrustsItsLookRecordsTheWrites(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	cfg := Config{Disks: []string{t.TempDir()}}.Normal()
	o := options(t, procRoot(t, fakeProc{pid: 40, ppid: 1, argv: []string{"x"}, written: 1 << 30}), time.Now(), nil)
	o.Rules, o.DryRun = []string{RuleUV}, true
	if _, err := Run(ctx, cfg, st, o); err != nil {
		t.Fatal(err)
	}
	if at, _, _ := st.Writes(ctx); !at.IsZero() {
		t.Error("a dry run records nothing")
	}
	o.DryRun, o.ProcRoot = false, t.TempDir() // an empty /proc: a look the run cannot trust
	if _, err := Run(ctx, cfg, st, o); err != nil {
		t.Fatal(err)
	}
	if at, _, _ := st.Writes(ctx); !at.IsZero() {
		t.Error("a blind run records no writes: the next run would take every process for new")
	}
	o.ProcRoot = procRoot(t, fakeProc{pid: 40, ppid: 1, argv: []string{"x"}, written: 1 << 30})
	if _, err := Run(ctx, cfg, st, o); err != nil {
		t.Fatal(err)
	}
	if at, ws, _ := st.Writes(ctx); at.IsZero() || len(ws) < 2 {
		t.Errorf("a run records each process's written bytes: %v %v", at, ws)
	}
}
