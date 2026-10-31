package capacity

import (
	"context"
	"encoding/json"
	"fmt"
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

// fillRun is a run that sees dir fill at 45 GiB an hour, with the processes given and the written bytes the look
// reads (writes, called at the look's start and end).
func fillRun(t *testing.T, st *store.Store, dir string, now time.Time, proc string, writes func() map[int]int64) *Result {
	t.Helper()
	cfg := Config{Disks: []string{dir}}.Normal()
	o := options(t, proc, now, nil)
	o.Rules = []string{RuleUV}
	o.Writes = writes
	res, err := Run(context.Background(), cfg, st, o)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// fillFixture is a disk that held 15 GiB less 24 minutes before now (inside the 30-minute window of a run up to five
// minutes later), and a run of fake /procs: each at an uptime, on one boot.
func fillFixture(t *testing.T) (st *store.Store, dir string, now time.Time, proc func(uptime int, ps ...fakeProc) string) {
	t.Helper()
	ctx := context.Background()
	dir = t.TempDir()
	var fs syscall.Statfs_t
	must(t, syscall.Statfs(dir, &fs))
	used := int64(fs.Blocks-fs.Bfree) * int64(fs.Bsize)
	st = openStore(t)
	now = time.Now()
	must(t, st.PutCapacityRun(ctx, store.CapacityRun{At: now.Add(-24 * time.Minute)},
		[]store.DiskSample{{Disk: dir, At: now.Add(-24 * time.Minute), Size: 1 << 40, Used: used - 15<<30, Avail: 1 << 39}}, nil))
	proc = func(uptime int, ps ...fakeProc) string {
		root := procRoot(t, ps...)
		write(t, filepath.Join(root, "uptime"), fmt.Sprintf("%d.00 1000.00\n", uptime))
		write(t, filepath.Join(root, "sys", "kernel", "random", "boot_id"), "b1\n")
		return root
	}
	return st, dir, now, proc
}

func quiet() map[int]int64 { return map[int]int64{} }

func TestAFillNamesWhoWroteBetweenTheRunsAndEveryWriterItSaw(t *testing.T) {
	ctx := context.Background()
	st, dir, now, proc := fillFixture(t)
	// a training run under a shell saves checkpoints between the looks: nothing in the seconds measured
	shell := fakeProc{pid: 30, ppid: 1, argv: []string{"bash"}, cwd: dir, written: 1 << 30}
	train := fakeProc{pid: 31, ppid: 30, argv: []string{"torchrun"}, cwd: dir, written: 10 << 30}
	old := fakeProc{pid: 33, ppid: 1, argv: []string{"old"}, cwd: dir, written: 1 << 30} // its pid is used again later
	sync := fakeProc{pid: 34, ppid: 1, argv: []string{"sync"}, cwd: dir}
	if d := fillRun(t, st, dir, now.Add(-5*time.Minute), proc(1201, shell, train, old, sync), quiet).Disks[0]; !d.Fast || len(d.Writers) != 0 {
		t.Fatalf("the first run has no run before to compare with: %+v", d)
	}
	train.written, old.written = 50<<30, 9<<30
	d := fillRun(t, st, dir, now, proc(1501, shell, train, old, sync), quiet).Disks[0]
	got := map[int]Writer{}
	for _, w := range d.Writers {
		got[w.PID] = w
	}
	if w := got[31]; w.Written != 40<<30 || w.Over != 300 {
		t.Errorf("the training run wrote 40 GiB in the 5 minutes since the run before: %+v", got)
	}
	if _, ok := got[30]; ok {
		t.Errorf("a shell that wrote nothing since is no writer: %+v", got)
	}
	// five minutes on: the training run wrote 10 GiB more and ended, and its shell took in all of its 60 GiB; pid 33
	// is a python started two minutes before, 1681 s after boot; sync writes slowly, as a slow fill's writer may
	shell.written = 1<<30 + 60<<30
	sync.written = 150 << 20
	late := fakeProc{pid: 33, ppid: 1, argv: []string{"python"}, cwd: dir, written: 8 << 30, start: 168100}
	d = fillRun(t, st, dir, now.Add(5*time.Minute), proc(1801, shell, late, sync), quiet).Disks[0]
	got = map[int]Writer{}
	for _, w := range d.Writers {
		got[w.PID] = w
	}
	if w := got[30]; w.Written != 10<<30 {
		t.Errorf("the shell wrote what its child wrote since the run before, 10 GiB, not the child's whole 60: %+v", got)
	}
	if w := got[33]; w.Written != 8<<30 || w.Over != 120 || w.Start != 168100 {
		t.Errorf("a process that started since wrote all it wrote since it started: %+v", got)
	}
	if _, ok := got[34]; !ok {
		t.Errorf("0.5 MiB/s since the run before names a writer: a fill from 1 GiB an hour is reported: %+v", got)
	}
	n, _, _ := st.Note(ctx, "capacity:fill:"+dir)
	for _, want := range []string{"torchrun (pid 31) wrote 40.0 GiB in 5m0s", "bash (pid 30) wrote 10.0 GiB in 5m0s",
		"python (pid 33) wrote 8.0 GiB in 2m0s", "old (pid 33) wrote 8.0 GiB in 5m0s"} {
		if !strings.Contains(n.Detail, want) {
			t.Errorf("the note names the fill's writers, a pid used again as two (%s): %s", want, n.Detail)
		}
	}
}

func TestAFillWritersRunsAddUpTheFastestLookStandsAndANameOutlivesItsProcess(t *testing.T) {
	ctx := context.Background()
	st, dir, now, proc := fillFixture(t)
	dd := fakeProc{pid: 41, ppid: 1, argv: []string{"sh"}, cwd: dir, written: 1 << 30} // a shell that execs dd later
	look := func(mibs int64, gone string) func() map[int]int64 {                       // the look's two reads, mibs MiB/s apart
		n := 0
		return func() map[int]int64 {
			n++
			if gone != "" { // it ends during the look
				must(t, os.RemoveAll(gone))
			}
			return map[int]int64{41: int64(n) * mibs << 20 / 1000} // the look lasts a millisecond in tests
		}
	}
	p1 := proc(1201, dd)
	fillRun(t, st, dir, now.Add(-10*time.Minute), p1, look(90, ""))
	dd.written = 3 << 30
	fillRun(t, st, dir, now.Add(-5*time.Minute), proc(1501, dd), look(80, ""))
	dd.written, dd.argv = 5<<30, []string{"dd"}
	p3 := proc(1801, dd)
	fillRun(t, st, dir, now, p3, look(0, filepath.Join(p3, "41")))
	n, _, _ := st.Note(ctx, "capacity:fill:"+dir)
	if !strings.Contains(n.Detail, "dd (pid 41) wrote 4.0 GiB in 10m0s, 90 MiB/s at a look in "+dir) {
		t.Errorf("what it wrote adds up over the runs, its fastest look from the fill's first run stands, the newest run "+
			"names it, and its name and directory come from the sample when it ended during the look: %s", n.Detail)
	}
}

func TestAFillNoteReadsOnlyItsOwnFillsWriters(t *testing.T) {
	ctx := context.Background()
	st, dir, now, proc := fillFixture(t)
	// an earlier fill of this disk, and one of another disk during this fill, each with a writer
	put := func(at time.Time, disk string, pid int, comm string) {
		meta, _ := json.Marshal(DiskState{Disk: disk, Fast: true, Writers: []Writer{{PID: pid, Comm: comm, Rate: 50}}})
		_, err := st.Put(ctx, []store.Event{{Source: store.Capacity, SourceKey: fmt.Sprintf("fill/%s/%d", disk, at.UnixMilli()),
			TS: at, Actor: store.ActorDetector, Kind: KindFill, Summary: "x", Meta: string(meta)}})
		must(t, err)
	}
	put(now.Add(-3*time.Hour), dir, 51, "earlier")
	fillRun(t, st, dir, now.Add(-5*time.Minute), proc(1201), quiet)
	put(now.Add(-4*time.Minute), "/elsewhere", 52, "other")
	cp := fakeProc{pid: 53, ppid: 1, argv: []string{"cp"}, cwd: dir, written: 2 << 30, start: 145600} // 45 s before
	fillRun(t, st, dir, now, proc(1501, cp), quiet)
	n, _, _ := st.Note(ctx, "capacity:fill:"+dir)
	if strings.Contains(n.Detail, "earlier") || strings.Contains(n.Detail, "other") {
		t.Errorf("the note names this fill's writers on this disk only: %s", n.Detail)
	}
	if !strings.Contains(n.Detail, "cp (pid 53) wrote 2.0 GiB in 45s") {
		t.Errorf("under a minute reads in seconds: %s", n.Detail)
	}
}

func TestBetweenTwoSnapshots(t *testing.T) {
	snap := func(up int64, boot string, ws ...store.ProcWrite) writesSnap {
		s := writesSnap{snap: store.Snapshot{At: time.Unix(1, 0), Uptime: up, Boot: boot}, ws: map[int]store.ProcWrite{}}
		for _, w := range ws {
			s.ws[w.PID] = w
		}
		return s
	}
	prev := snap(1000, "b1", store.ProcWrite{PID: 1, Start: 10, Written: 500}, store.ProcWrite{PID: 2, Start: 10, Written: 900})
	got := between(prev, snap(31000, "b1", store.ProcWrite{PID: 1, Start: 10, Written: 400}, store.ProcWrite{PID: 2, Start: 10, Written: 950}))
	if _, ok := got[1]; ok {
		t.Errorf("a counter that went down is not known: %+v", got)
	}
	if got[2] != (wrote{50, 300}) {
		t.Errorf("the same process wrote the difference: %+v", got)
	}
	got = between(prev, snap(2000, "b2", store.ProcWrite{PID: 1, Start: 10, Written: 400}))
	if got[1] != (wrote{400, 19.9}) {
		t.Errorf("on another boot every process started since: %+v", got)
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
	if snap, _, _ := st.Writes(ctx); !snap.At.IsZero() {
		t.Error("a dry run records nothing")
	}
	o.DryRun, o.ProcRoot = false, t.TempDir() // an empty /proc: a look the run cannot trust
	if _, err := Run(ctx, cfg, st, o); err != nil {
		t.Fatal(err)
	}
	if snap, _, _ := st.Writes(ctx); !snap.At.IsZero() {
		t.Error("a blind run records no writes: the next run would take every process for new")
	}
	o.ProcRoot = procRoot(t, fakeProc{pid: 40, ppid: 1, argv: []string{"x"}, written: 1 << 30})
	if _, err := Run(ctx, cfg, st, o); err != nil {
		t.Fatal(err)
	}
	if snap, ws, _ := st.Writes(ctx); snap.At.IsZero() || len(ws) < 2 {
		t.Errorf("a run records each process's written bytes: %v %v", snap, ws)
	}
}
