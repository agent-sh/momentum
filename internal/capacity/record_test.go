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
