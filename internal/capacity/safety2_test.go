package capacity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// addProc adds a process to a synthetic /proc made by procRoot.
func addProc(t *testing.T, root string, p fakeProc) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(p.pid))
	must(t, os.MkdirAll(filepath.Join(dir, "fd"), 0o755))
	write(t, filepath.Join(dir, "cmdline"), strings.Join(p.argv, "\x00")+"\x00")
	write(t, filepath.Join(dir, "comm"), filepath.Base(p.argv[0])+"\n")
	write(t, filepath.Join(dir, "stat"), strconv.Itoa(p.pid)+" (x) S "+strconv.Itoa(p.ppid)+" 0 0 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 100 0 0\n")
	if p.cwd != "" {
		must(t, os.Symlink(p.cwd, filepath.Join(dir, "cwd")))
	}
}

// A unit, or a keep entry, that names a model through its Hugging Face snapshot keeps the repository, though the
// snapshot's file resolves into the shared blob store.
func TestASnapshotsFileNamedOrKeptKeepsItsRepository(t *testing.T) {
	for _, c := range []string{"unit", "keep"} {
		t.Run(c, func(t *testing.T) {
			f := newModels(t)
			snap := filepath.Join(f.hub, "models--org--one", "snapshots", "rev1", "model.gguf")
			must(t, os.MkdirAll(filepath.Dir(snap), 0o755))
			must(t, os.Symlink("../../blobs/e3", snap))
			st := openStore(t)
			watchedSince(t, st, f.cfg, f.now.Add(-35*24*time.Hour), f.now)
			cfg := f.cfg
			cfg.Live = []string{RuleModels}
			var jobs Jobs
			if c == "unit" {
				jobs = fakeJobs{units: map[string]string{"user:llama.service": "ExecStart=/usr/bin/llama-server -m " + snap + "\n"}}
			} else {
				cfg.Models.Keep = append(cfg.Models.Keep, snap)
			}
			res, err := Run(context.Background(), cfg, st, options(t, procRoot(t), f.now, jobs))
			if err != nil {
				t.Fatal(err)
			}
			if got := findings(res, RuleModels)[filepath.Join(f.hub, "models--org--one")]; got.Done {
				t.Fatalf("the repository was deleted: %+v", got)
			}
			if _, err := os.Stat(filepath.Join(f.hub, "blobs", "bb", "sole")); err != nil {
				t.Fatal("its blob was deleted")
			}
		})
	}
}

// A branch the remote rewrote (a stacked branch rebased and force-pushed without its base): this clone's stale
// remote-tracking branch still holds the base's commit, which proves nothing.
func TestACommitOnlyAStaleTrackingRefHoldsIsUnpushed(t *testing.T) {
	gitEnv(t)
	base := t.TempDir()
	remote, clone, other, wts := filepath.Join(base, "remote.git"), filepath.Join(base, "clone"), filepath.Join(base, "other"), filepath.Join(base, "cache")
	gitT(t, base, "init", "-q", "--bare", remote)
	gitT(t, base, "clone", "-q", remote, clone)
	write(t, filepath.Join(clone, "README"), "one\n")
	gitT(t, clone, "add", ".")
	gitT(t, clone, "commit", "-qm", "one")
	gitT(t, clone, "push", "-q", "origin", "main")
	gitT(t, clone, "remote", "set-head", "origin", "main")
	a := filepath.Join(wts, "a")
	gitT(t, clone, "worktree", "add", "-q", "-b", "feat/a", a, "origin/main")
	write(t, filepath.Join(a, "a"), "a\n")
	gitT(t, a, "add", ".")
	gitT(t, a, "commit", "-qm", "a")
	gitT(t, a, "push", "-q", "-u", "origin", "feat/a")
	gitT(t, a, "checkout", "-q", "-b", "feat/b")
	write(t, filepath.Join(a, "b"), "b\n")
	gitT(t, a, "add", ".")
	gitT(t, a, "commit", "-qm", "b")
	gitT(t, a, "push", "-q", "-u", "origin", "feat/b")
	gitT(t, a, "checkout", "-q", "feat/a")
	gitT(t, clone, "fetch", "-q", "origin")
	// elsewhere: feat/a is closed and deleted, feat/b rebased onto main without it and force-pushed
	gitT(t, base, "clone", "-q", remote, other)
	gitT(t, other, "push", "-q", "origin", "--delete", "feat/a")
	gitT(t, other, "checkout", "-q", "feat/b")
	gitT(t, other, "rebase", "-q", "--onto", "origin/main", "HEAD~1")
	gitT(t, other, "push", "-q", "-f", "origin", "feat/b")
	cfg := Config{Worktrees: WorktreesConfig{Roots: []string{wts}}}.Normal()
	cfg.Live = []string{RuleWorktrees}
	res, err := Run(context.Background(), cfg, openStore(t), options(t, procRoot(t), time.Now().Add(2*time.Hour), nil))
	if err != nil {
		t.Fatal(err)
	}
	if f := findings(res, RuleWorktrees)[a]; f.Action != ActReport || f.class != "unpushed" {
		t.Fatalf("feat/a's commit is on no branch the remote has now: %+v", f)
	}
	if _, err := os.Stat(a); err != nil {
		t.Fatal("the worktree was removed")
	}
}

// A root dropped from the configuration and added back starts its watch again, and a run that could not trust its
// look watched nothing.
func TestOnlyTrustedRunsOfAConfiguredRootCountAsWatched(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Add(80 * 24 * time.Hour)
	a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	blob(t, filepath.Join(a, "old", "w.bin"), 2<<20)
	must(t, os.MkdirAll(b, 0o755))
	st := openStore(t)
	cfgA := Config{Models: ModelsConfig{Roots: []string{a}}}.Normal()
	cfgB := Config{Models: ModelsConfig{Roots: []string{b}}}.Normal()
	watchedSince(t, st, cfgA, now.Add(-80*24*time.Hour), now.Add(-40*24*time.Hour))
	o := options(t, procRoot(t), now.Add(-40*24*time.Hour), nil)
	if _, err := Run(ctx, cfgB, st, o); err != nil { // a recording run that does not watch a
		t.Fatal(err)
	}
	watchedSince(t, st, cfgB, now.Add(-40*24*time.Hour), now)
	cfgA.Live = []string{RuleModels}
	res, err := Run(ctx, cfgA, st, options(t, procRoot(t), now, nil))
	if err != nil {
		t.Fatal(err)
	}
	if f := findings(res, RuleModels)[filepath.Join(a, "old")]; f.Action != ActPending {
		t.Fatalf("a root added back waits again: %+v", f)
	}

	st2 := openStore(t)
	var runs []store.CapacityRun
	for at := now.Add(-35 * 24 * time.Hour); at.Before(now); at = at.Add(20 * time.Minute) {
		runs = append(runs, store.CapacityRun{At: at, Procs: 600, Blind: true})
	}
	must(t, st2.PutRuns(ctx, runs))
	must(t, st2.SetMark(ctx, "watch:"+a, now.Add(-35*24*time.Hour)))
	res, err = Run(ctx, cfgA, st2, options(t, procRoot(t), now, nil))
	if err != nil {
		t.Fatal(err)
	}
	if f := findings(res, RuleModels)[filepath.Join(a, "old")]; f.Action != ActPending {
		t.Fatalf("35 days of blind runs watched nothing: %+v", f)
	}
}

// A run with no process at all deletes nothing, and a blind run prunes nothing.
func TestABlindRunNeitherDeletesNorPrunes(t *testing.T) {
	f := newModels(t)
	st := openStore(t)
	watchedSince(t, st, f.cfg, f.now.Add(-35*24*time.Hour), f.now)
	cfg := f.cfg
	cfg.Live = []string{RuleModels, RuleUV}
	empty := filepath.Join(t.TempDir(), "proc")
	must(t, os.MkdirAll(empty, 0o755))
	var ran []string
	o := options(t, empty, f.now, nil)
	o.Tools = uvTools("40000000000", &ran)
	if _, err := Run(context.Background(), cfg, st, o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "idle")); err != nil {
		t.Fatal("a run that read no process deleted")
	}
	if strings.Contains(strings.Join(ran, "\n"), "prune") {
		t.Fatalf("a blind run pruned: %v", ran)
	}
}

// The second look before a worktree goes sees who came to work in its repository since the first.
func TestSomeoneWhoStartsWorkingInTheRepositoryKeepsItsBranch(t *testing.T) {
	clone, wts := worktreesFixture(t)
	proc := procRoot(t)
	cfg := Config{Worktrees: WorktreesConfig{Roots: []string{wts}}}.Normal()
	cfg.Live = []string{RuleWorktrees}
	o := options(t, proc, time.Now().Add(2*time.Hour), nil)
	o.beforeAct = func() { addProc(t, proc, fakeProc{pid: 777, ppid: 1, argv: []string{"bash"}, cwd: clone}) }
	res, err := Run(context.Background(), cfg, openStore(t), o)
	if err != nil {
		t.Fatal(err)
	}
	if f := findings(res, RuleWorktrees)[filepath.Join(wts, "merged")]; f.Done {
		t.Fatalf("a shell that began in the main checkout keeps the branch: %+v", f)
	}
}

// A worktree whose ignored target holds Criterion's baselines is not clean.
func TestAWorktreeWhoseTargetHoldsBaselinesIsReported(t *testing.T) {
	_, wts := worktreesFixture(t)
	merged := filepath.Join(wts, "merged")
	cargoTarget(t, filepath.Join(merged, "target"))
	blob(t, filepath.Join(merged, "target", "criterion", "bench", "base", "estimates.json"), 10)
	// target is ignored in this fixture by the clone's exclude file
	gitDir := strings.TrimSpace(gitT(t, merged, "rev-parse", "--git-common-dir"))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(merged, gitDir)
	}
	write(t, filepath.Join(gitDir, "info", "exclude"), "target/\n")
	cfg := Config{Worktrees: WorktreesConfig{Roots: []string{wts}}}.Normal()
	cfg.Live = []string{RuleWorktrees}
	res, err := Run(context.Background(), cfg, openStore(t), options(t, procRoot(t), time.Now().Add(2*time.Hour), nil))
	if err != nil {
		t.Fatal(err)
	}
	if f := findings(res, RuleWorktrees)[merged]; f.Action != ActReport || f.class != "ignored" {
		t.Fatalf("%+v", f)
	}
	if _, err := os.Stat(filepath.Join(merged, "target", "criterion", "bench", "base", "estimates.json")); err != nil {
		t.Fatal("the baseline is gone")
	}
}

// Words a job resolves against its directory: a bare name that is there, a quoted assignment's value, and a
// crontab line's words against the home directory.
func TestJobsNameBareWordsQuotedValuesAndHomeRelativePaths(t *testing.T) {
	dir := t.TempDir()
	blob(t, filepath.Join(dir, "qwen.gguf"), 10)
	spaced := filepath.Join(dir, "my models", "m1")
	units := []JobText{{Name: "user:a.service", Dir: dir, Text: "ExecStart=/usr/bin/srv -m qwen.gguf\nEnvironment=\"MODEL_DIR=" + spaced + "\"\n"}}
	home := homeDir()
	rel := filepath.Join(".cache", "momentum-capacity-test-x")
	n, err := LoadNamed(context.Background(), jobsOf{units: units, crontab: "0 * * * * run " + rel + "/m\n"}, NamesConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(dir, "qwen.gguf"), spaced, filepath.Join(home, rel, "m")} {
		if _, ok := n.Protects(p); !ok {
			t.Errorf("%s is named", p)
		}
	}
}

// jobsOf are jobs given as their texts.
type jobsOf struct {
	units   []JobText
	crontab string
}

func (j jobsOf) Units(context.Context) ([]JobText, []string, error) { return j.units, nil, nil }
func (j jobsOf) Crontab(context.Context) (string, error)            { return j.crontab, nil }

// uv's refusal for its lock is told from a failure in either of its messages.
func TestUVsRefusalIsToldByEitherMessage(t *testing.T) {
	for _, m := range []string{
		"error: Cache is currently in-use, waiting for other uv processes to finish",
		"error: Timeout (10s) when waiting for lock on `/home/u/a/very/long/cache/path/uv` at `/home/u/a/very/long/cache/path/uv/.lock`",
	} {
		if !uvInUse(errors.New("uv cache prune: exit status 2: " + m)) {
			t.Errorf("not told: %s", m)
		}
	}
	if uvInUse(errors.New("uv cache prune: exit status 2: error: Permission denied")) {
		t.Error("a failure is not a refusal")
	}
}

// A Docker that cannot be asked which containers run holds the deletions; one not running does not.
func TestADockerThatCannotBeAskedHoldsTheDeletions(t *testing.T) {
	for _, c := range []struct {
		answer string
		holds  bool
	}{
		{"ERR-DENIED", true},
		{"ERR-DOWN", false},
		{"ERR", false}, // not installed
	} {
		t.Run(c.answer, func(t *testing.T) {
			f := newModels(t)
			st := openStore(t)
			watchedSince(t, st, f.cfg, f.now.Add(-35*24*time.Hour), f.now)
			cfg := f.cfg
			cfg.Live = []string{RuleModels}
			o := options(t, procRoot(t), f.now, nil)
			o.Tools = fakeTools{answers: map[string]string{"uv": "ERR", "docker ps": c.answer, "docker": "ERR"}}
			res, err := Run(context.Background(), cfg, st, o)
			if err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(filepath.Join(f.root, "idle"))
			if kept := statErr == nil; kept != c.holds {
				t.Fatalf("kept %v, want %v: %v", kept, c.holds, res.Problems)
			}
		})
	}
}

// A dry run lists an item that holds a mount point as reported, as the live run would leave it.
func TestADryRunReportsAnItemHoldingAMountPoint(t *testing.T) {
	f := newModels(t)
	st := openStore(t)
	watchedSince(t, st, f.cfg, f.now.Add(-35*24*time.Hour), f.now)
	o := options(t, procRoot(t), f.now, nil)
	o.DryRun = true
	write(t, o.MountInfo, "29 1 259:3 / / rw - ext4 /dev/sda2 rw\n50 29 259:3 /x "+filepath.Join(f.root, "idle", "m")+" rw - ext4 /dev/sda2 rw\n")
	res, err := Run(context.Background(), f.cfg, st, o)
	if err != nil {
		t.Fatal(err)
	}
	if got := findings(res, RuleModels)[filepath.Join(f.root, "idle")]; got.Action != ActReport || got.class != "mount" {
		t.Fatalf("%+v", got)
	}
}

// A slow creep toward the full percent is no fast fill.
func TestASlowCreepIsNoFastFill(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	var fs syscall.Statfs_t
	must(t, syscall.Statfs(dir, &fs))
	used := int64(fs.Blocks-fs.Bfree) * int64(fs.Bsize)
	avail := int64(fs.Bavail) * int64(fs.Bsize)
	pct := 100 * float64(used) / float64(used+avail)
	st := openStore(t)
	now := time.Now()
	must(t, st.PutCapacityRun(ctx, store.CapacityRun{At: now.Add(-20 * time.Minute)},
		[]store.DiskSample{{Disk: dir, At: now.Add(-20 * time.Minute), Used: used - 1<<26}}, nil)) // 64 MiB in 20 min
	cfg := Config{Disks: []string{dir}, Fill: FillConfig{Full: min(pct+1, 99.9)}}.Normal() // the line is near: due in hours
	o := options(t, procRoot(t), now, nil)
	o.Rules = []string{RuleUV}
	res, err := Run(ctx, cfg, st, o)
	if err != nil {
		t.Fatal(err)
	}
	d := res.Disks[0]
	if d.FullIn <= 0 || d.FullIn > 6*time.Hour {
		t.Skipf("setup: the line is not due within 6 hours here (%s)", d.FullIn)
	}
	if d.Fast {
		t.Fatalf("0.2 GiB an hour is no fast fill, even with the line due in %s: %+v", d.FullIn, d)
	}
}
