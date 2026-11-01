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

// A branch the server deleted (a closed pull request) while this clone kept its remote-tracking branch: its commit
// is on no branch the server has, so the worktree is only reported, and its branch stays.
func TestAWorktreeWhoseBranchTheServerDeletedKeepsItsCommits(t *testing.T) {
	gitEnv(t)
	base := t.TempDir()
	remote, clone, wts := filepath.Join(base, "remote.git"), filepath.Join(base, "clone"), filepath.Join(base, "cache")
	gitT(t, base, "init", "-q", "--bare", remote)
	gitT(t, base, "clone", "-q", remote, clone)
	write(t, filepath.Join(clone, "README"), "one\n")
	gitT(t, clone, "add", ".")
	gitT(t, clone, "commit", "-qm", "one")
	gitT(t, clone, "push", "-q", "origin", "main")
	gitT(t, clone, "remote", "set-head", "origin", "main")
	w := filepath.Join(wts, "closed")
	gitT(t, clone, "worktree", "add", "-q", "-b", "feat/closed", w, "origin/main")
	write(t, filepath.Join(w, "work"), "only here\n")
	gitT(t, w, "add", ".")
	gitT(t, w, "commit", "-qm", "work")
	gitT(t, w, "push", "-q", "-u", "origin", "feat/closed")
	gitT(t, remote, "branch", "-D", "feat/closed") // on the server; the clone still has origin/feat/closed
	cfg := Config{Worktrees: WorktreesConfig{Roots: []string{wts}}}.Normal()
	cfg.Live = []string{RuleWorktrees}
	res, err := Run(context.Background(), cfg, openStore(t), options(t, procRoot(t), time.Now().Add(2*time.Hour), nil))
	if err != nil {
		t.Fatal(err)
	}
	f := findings(res, RuleWorktrees)[w]
	if f.Action != ActReport || f.class != "unpushed" {
		t.Fatalf("its commit is on no branch the server has: %+v", f)
	}
	if _, err := os.Stat(w); err != nil {
		t.Fatal("the worktree was removed")
	}
	if !strings.Contains(gitT(t, clone, "branch", "--list"), "feat/closed") {
		t.Fatal("the branch was deleted")
	}
}

// Ignored files git status hides, and edits a skip-worktree mark hides, keep a merged worktree; ignored caches do not.
func TestIgnoredWorkAndHiddenEditsKeepAMergedWorktree(t *testing.T) {
	gitEnv(t)
	base := t.TempDir()
	remote, clone, wts := filepath.Join(base, "remote.git"), filepath.Join(base, "clone"), filepath.Join(base, "cache")
	gitT(t, base, "init", "-q", "--bare", remote)
	gitT(t, base, "clone", "-q", remote, clone)
	write(t, filepath.Join(clone, ".gitignore"), ".env\nresults/\ntarget/\nnode_modules/\n")
	write(t, filepath.Join(clone, "config.yaml"), "a: 1\n")
	gitT(t, clone, "add", ".")
	gitT(t, clone, "commit", "-qm", "one")
	gitT(t, clone, "push", "-q", "origin", "main")
	gitT(t, clone, "remote", "set-head", "origin", "main")
	merged := func(name string) string {
		w := filepath.Join(wts, name)
		gitT(t, clone, "worktree", "add", "-q", "-b", "feat/"+name, w, "origin/main")
		write(t, filepath.Join(w, name), name+"\n")
		gitT(t, w, "add", ".")
		gitT(t, w, "commit", "-qm", name)
		gitT(t, w, "push", "-q", "-u", "origin", "feat/"+name)
		gitT(t, w, "push", "-q", "origin", "feat/"+name+":main")
		gitT(t, clone, "fetch", "-q", "origin")
		return w
	}
	secrets := merged("secrets")
	write(t, filepath.Join(secrets, ".env"), "TOKEN=x\n")
	write(t, filepath.Join(secrets, "results", "run1.json"), "{}\n")
	caches := merged("caches")
	cargoTarget(t, filepath.Join(caches, "target"))
	write(t, filepath.Join(caches, "node_modules", "x", "index.js"), "1\n")
	hidden := merged("hidden")
	gitT(t, hidden, "update-index", "--skip-worktree", "config.yaml")
	write(t, filepath.Join(hidden, "config.yaml"), "a: 2\n")
	cfg := Config{Worktrees: WorktreesConfig{Roots: []string{wts}}}.Normal()
	cfg.Live = []string{RuleWorktrees}
	res, err := Run(context.Background(), cfg, openStore(t), options(t, procRoot(t), time.Now().Add(2*time.Hour), nil))
	if err != nil {
		t.Fatal(err)
	}
	got := findings(res, RuleWorktrees)
	if f := got[secrets]; f.Action != ActReport || f.class != "ignored" || !strings.Contains(f.Why, ".env") {
		t.Errorf("ignored files that are not a cache keep it: %+v", f)
	}
	if f := got[hidden]; f.Action != ActReport || f.class != "uncommitted" {
		t.Errorf("an edit a skip-worktree mark hides keeps it: %+v", f)
	}
	if f := got[caches]; !f.Done {
		t.Errorf("ignored caches (a tagged target, node_modules) do not keep it: %+v", f)
	}
	for _, w := range []string{secrets, hidden} {
		if _, err := os.Stat(w); err != nil {
			t.Errorf("%s was removed", w)
		}
	}
}

// A root added to the configuration is watched from the run that first watches it: its items wait their own idle
// time. A sighting under the key a nested root's directory had as an item still counts for its items, and time the
// sampler did not run does not count as watched.
func TestARootAddedLaterWaitsItsOwnIdleTime(t *testing.T) {
	base := t.TempDir()
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	now := time.Now().Add(40 * 24 * time.Hour)
	blob(t, filepath.Join(a, "old", "w.bin"), 2<<20)
	blob(t, filepath.Join(b, "old", "w.bin"), 2<<20)
	blob(t, filepath.Join(a, "hub", "models--o--m", "w.bin"), 2<<20)
	st := openStore(t)
	ctx := context.Background()
	watchedSince(t, st, Config{Models: ModelsConfig{Roots: []string{a}}}, now.Add(-35*24*time.Hour), now)
	// a/hub was a root of its own 35 days ago, then for a while only an item of a, seen 10 days ago under that key
	must(t, st.SetMark(ctx, "watch:"+filepath.Join(a, "hub"), now.Add(-35*24*time.Hour)))
	must(t, st.PutCapacityRun(ctx, store.CapacityRun{At: now.Add(-10*24*time.Hour - time.Second)}, nil,
		[]store.Seen{{Path: filepath.Join(a, "hub"), Last: now.Add(-10 * 24 * time.Hour), How: "map", Proc: "x 1"}}))
	cfg := Config{Models: ModelsConfig{Roots: []string{a, b, filepath.Join(a, "hub")}}}.Normal()
	cfg.Live = []string{RuleModels}
	res, err := Run(ctx, cfg, st, options(t, procRoot(t), now, nil))
	if err != nil {
		t.Fatal(err)
	}
	got := findings(res, RuleModels)
	if f := got[filepath.Join(a, "old")]; !f.Done {
		t.Errorf("an item of the root watched for 35 days goes: %+v", f)
	}
	if f := got[filepath.Join(b, "old")]; f.Action != ActPending || !strings.Contains(f.Why, "watched") {
		t.Errorf("an item of the root added now waits: %+v", f)
	}
	if f := got[filepath.Join(a, "hub", "models--o--m")]; f.Action != ActPending || !strings.Contains(f.Why, "seen in use") {
		t.Errorf("an item under a key seen 10 days ago waits: %+v", f)
	}

	// a record with a hole: watched 10 days, then 20 days without a run, then 5
	c := filepath.Join(t.TempDir(), "c")
	blob(t, filepath.Join(c, "old", "w.bin"), 2<<20)
	ccfg := Config{Models: ModelsConfig{Roots: []string{c}}}.Normal()
	ccfg.Live = []string{RuleModels}
	st2 := openStore(t)
	from := now.Add(-35 * 24 * time.Hour)
	watchedSince(t, st2, ccfg, from, from.Add(10*24*time.Hour))
	watchedSince(t, st2, ccfg, now.Add(-5*24*time.Hour), now)
	must(t, st2.SetMark(ctx, "watch:"+c, from)) // the root was first watched 35 days ago
	res, err = Run(ctx, ccfg, st2, options(t, procRoot(t), now, nil))
	if err != nil {
		t.Fatal(err)
	}
	if f := findings(res, RuleModels)[filepath.Join(c, "old")]; f.Action != ActPending || !strings.Contains(f.Why, "missed") {
		t.Errorf("15 days watched of 35 is not 30: %+v", f)
	}
}

// An item that holds a mount point is only reported: a deletion would reach through it.
func TestAnItemHoldingAMountPointIsOnlyReported(t *testing.T) {
	f := newModels(t)
	st := openStore(t)
	watchedSince(t, st, f.cfg, f.now.Add(-35*24*time.Hour), f.now)
	cfg := f.cfg
	cfg.Live = []string{RuleModels}
	o := options(t, procRoot(t), f.now, nil)
	write(t, o.MountInfo, "29 1 259:3 / / rw - ext4 /dev/sda2 rw\n50 29 259:3 /elsewhere "+filepath.Join(f.root, "idle", "share")+" rw - ext4 /dev/sda2 rw\n")
	res, err := Run(context.Background(), cfg, st, o)
	if err != nil {
		t.Fatal(err)
	}
	if got := findings(res, RuleModels)[filepath.Join(f.root, "idle")]; got.Action != ActReport || got.class != "mount" {
		t.Fatalf("an item with a mount point under it is reported: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(f.root, "idle", "w.safetensors")); err != nil {
		t.Fatal("it was deleted")
	}
}

// What changes between the judgment and the deletion keeps the item: a write into a model, a commit in a worktree.
func TestAChangeAfterTheJudgmentKeepsTheItem(t *testing.T) {
	f := newModels(t)
	st := openStore(t)
	watchedSince(t, st, f.cfg, f.now.Add(-35*24*time.Hour), f.now)
	cfg := f.cfg
	cfg.Live = []string{RuleModels}
	o := options(t, procRoot(t), f.now, nil)
	o.beforeAct = func() { // a download resumes into the item
		p := filepath.Join(f.root, "idle", "w.safetensors")
		must(t, os.Chtimes(p, f.now, f.now.Add(time.Minute)))
	}
	res, err := Run(context.Background(), cfg, st, o)
	if err != nil {
		t.Fatal(err)
	}
	if got := findings(res, RuleModels)[filepath.Join(f.root, "idle")]; got.Action != ActKeep || got.Done {
		t.Fatalf("a write after the judgment keeps it: %+v", got)
	}

	_, wts := worktreesFixture(t)
	wcfg := Config{Worktrees: WorktreesConfig{Roots: []string{wts}}}.Normal()
	wcfg.Live = []string{RuleWorktrees}
	ow := options(t, procRoot(t), time.Now().Add(2*time.Hour), nil)
	merged := filepath.Join(wts, "merged")
	ow.beforeAct = func() {
		write(t, filepath.Join(merged, "late"), "late work\n")
		gitT(t, merged, "add", ".")
		gitT(t, merged, "commit", "-qm", "late work")
	}
	res, err = Run(context.Background(), wcfg, openStore(t), ow)
	if err != nil {
		t.Fatal(err)
	}
	if got := findings(res, RuleWorktrees)[merged]; got.Done || got.Action == ActDelete {
		t.Fatalf("a commit after the judgment keeps it: %+v", got)
	}
	if !strings.Contains(gitT(t, merged, "log", "--oneline", "-1"), "late work") {
		t.Fatal("the late commit is gone")
	}
}

// A look at the processes that is not the machine's (another PID namespace, a table far smaller than usual)
// deletes nothing.
func TestALookThatIsNotTheMachinesDeletesNothing(t *testing.T) {
	for _, c := range []string{"namespace", "few processes"} {
		t.Run(c, func(t *testing.T) {
			f := newModels(t)
			st := openStore(t)
			watchedSince(t, st, f.cfg, f.now.Add(-35*24*time.Hour), f.now)
			proc := procRoot(t, fakeProc{pid: 1, argv: []string{"init"}})
			if c == "namespace" {
				must(t, os.MkdirAll(filepath.Join(proc, "self", "ns"), 0o755))
				must(t, os.Symlink("pid:[4026532524]", filepath.Join(proc, "self", "ns", "pid")))
			} else {
				var runs []store.CapacityRun
				for i := 0; i < 20; i++ {
					runs = append(runs, store.CapacityRun{At: f.now.Add(-time.Duration(i+1) * time.Minute), Procs: 600})
				}
				must(t, st.PutRuns(context.Background(), runs))
			}
			cfg := f.cfg
			cfg.Live = []string{RuleModels}
			res, err := Run(context.Background(), cfg, st, options(t, proc, f.now, nil))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(f.root, "idle")); err != nil {
				t.Fatal("it deleted with a look it cannot trust")
			}
			if len(res.Problems) == 0 {
				t.Error("and says why")
			}
			if n, ok, _ := st.Note(context.Background(), "capacity:hold:look"); !ok || !n.Done.IsZero() {
				t.Errorf("a held run leaves a problem note: %+v", n)
			}
			ts, err := st.RunTimes(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if last := ts[len(ts)-1]; !last.Before(f.now.Truncate(time.Millisecond)) {
				t.Error("a blind run watched nothing: its time is no watched time")
			}
		})
	}
}

// A target dir with more dep-info files than are read is of an unknown project, and only reported.
func TestATargetWithMoreSourcesThanAreReadIsReported(t *testing.T) {
	was := maxDepFiles
	maxDepFiles = 2
	t.Cleanup(func() { maxDepFiles = was })
	root := filepath.Join(t.TempDir(), "cache")
	tgt := filepath.Join(root, "shared-target")
	cargoTarget(t, tgt)
	for _, n := range []string{"a", "b", "c"} {
		write(t, filepath.Join(tgt, "debug", "deps", n+".d"), "x: /src/"+n+"/src/lib.rs\n")
	}
	cfg := Config{Targets: TargetsConfig{Roots: []string{root}}}.Normal()
	cfg.Live = []string{RuleTargets}
	res, err := Run(context.Background(), cfg, openStore(t), options(t, procRoot(t), time.Now().Add(72*time.Hour), nil))
	if err != nil {
		t.Fatal(err)
	}
	if f := findings(res, RuleTargets)[tgt]; f.Action != ActReport || f.class != "unknown" {
		t.Fatalf("%+v", f)
	}
}

// A shared blob is unlinked under its lock, its lock file stays, and a lock another holder has keeps the blob.
func TestASharedBlobGoesOnlyUnderItsLock(t *testing.T) {
	f := newModels(t)
	st := openStore(t)
	watchedSince(t, st, f.cfg, f.now.Add(-35*24*time.Hour), f.now)
	cfg := f.cfg
	cfg.Live = []string{RuleModels}
	sole := filepath.Join(f.hub, "blobs", "bb", "sole")
	lock, err := os.OpenFile(sole+".lock", os.O_RDWR|os.O_CREATE, 0o644)
	must(t, err)
	must(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX))
	if _, err := Run(context.Background(), cfg, st, options(t, procRoot(t), f.now, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sole); err != nil {
		t.Fatal("a blob whose lock another holds stays")
	}
	lock.Close()
	// its repository went; the next sweep of a repository linking it would take it: here, sweep it directly
	if err := sweepBlob(sole, f.hub); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sole); !os.IsNotExist(err) {
		t.Error("an unlinked blob goes once its lock is free")
	}
	if _, err := os.Stat(sole + ".lock"); err != nil {
		t.Error("its lock file stays")
	}
}

// A note the rule resolved itself stands again when its items come back; one the owner resolved does not, while the
// same problem lasts. uv's prune that a uv process holds off for a day is a problem, and the hold's note follows it.
func TestANoteTheRuleResolvedStandsAgainForTheSameItems(t *testing.T) {
	ctx := context.Background()
	cfg := Config{Live: []string{RuleUV}}.Normal()
	st := openStore(t)
	now := time.Now()
	held := fakeTools{answers: map[string]string{"uv cache dir": "/home/u/.cache/uv\n", "uv cache size": "40000000000\n",
		"uv cache prune": "ERR-INUSE", "docker": "ERR"}}
	runWith := func(at time.Time, tools Tools, procs ...fakeProc) {
		t.Helper()
		o := options(t, procRoot(t, procs...), at, nil)
		o.Now, o.Tools, o.Rules = true, tools, []string{RuleUV}
		if _, err := Run(ctx, cfg, st, o); err != nil {
			t.Fatal(err)
		}
	}
	id := "capacity:" + RuleUV + ":busy"
	runWith(now, held)
	runWith(now.Add(24*time.Hour), held)
	if n, ok, _ := st.Note(ctx, id); !ok || !n.Done.IsZero() || !strings.Contains(n.Title, "waits to be pruned") {
		t.Fatalf("setup: a prune a uv process holds off for a day is a problem note: %+v", n)
	}
	runWith(now.Add(25*time.Hour), uvTools("40000000000", nil)) // the prune goes through: the rule resolves the note
	if n, _, _ := st.Note(ctx, id); n.Done.IsZero() {
		t.Fatal("setup: the rule resolves a note none of whose items it finds")
	}
	runWith(now.Add(26*time.Hour), held)
	runWith(now.Add(50*time.Hour), held)
	if n, _, _ := st.Note(ctx, id); !n.Done.IsZero() {
		t.Fatal("a note the rule resolved stands again when its item is back")
	}
	if _, _, err := st.DoneNote(ctx, id, now.Add(50*time.Hour)); err != nil {
		t.Fatal(err)
	}
	runWith(now.Add(51*time.Hour), held)
	if n, _, _ := st.Note(ctx, id); n.Done.IsZero() {
		t.Error("a note the owner resolved stays resolved while its item is the same")
	}
	// that hold ends; the next is an install the rule waits for quietly, for a day
	runWith(now.Add(52*time.Hour), uvTools("40000000000", nil))
	install := fakeProc{pid: 4, ppid: 1, argv: []string{"/usr/bin/uv", "pip", "install", "x"}}
	runWith(now.Add(53*time.Hour), held, install)
	runWith(now.Add(77*time.Hour), held, install)
	if n, _, _ := st.Note(ctx, id); !n.Done.IsZero() {
		t.Error("the owner's resolution lasts while its hold does: the next hold that lasts a day stands")
	}
	evs, _ := st.Events(ctx, store.Query{Source: store.Capacity, Kinds: []string{KindReport}})
	if len(evs) != 1 || !evs[0].TS.Equal(now.Truncate(time.Millisecond)) {
		t.Errorf("one event per item, dated when it was first reported: %+v", evs)
	}
}

// A disk above its full percent has its own note, put once, not a fast fill at every run.
func TestAFullDiskIsNotAFastFill(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st := openStore(t)
	now := time.Now()
	var fs syscall.Statfs_t
	must(t, syscall.Statfs(dir, &fs))
	used := int64(fs.Blocks-fs.Bfree) * int64(fs.Bsize)
	must(t, st.PutCapacityRun(ctx, store.CapacityRun{At: now.Add(-15 * time.Minute)},
		[]store.DiskSample{{Disk: dir, At: now.Add(-15 * time.Minute), Used: used - 1<<20}}, nil))
	cfg := Config{Disks: []string{dir}, Fill: FillConfig{Full: 0.001}}.Normal()
	o := options(t, procRoot(t), now, nil)
	o.Rules = []string{RuleUV}
	res, err := Run(ctx, cfg, st, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Disks[0].Fast {
		t.Fatalf("1 MiB in 15 minutes is no fast fill: %+v", res.Disks[0])
	}
	if n, ok, _ := st.Note(ctx, "capacity:full:"+dir); !ok || n.Kind != store.NoteProblem {
		t.Fatalf("a full disk has a note: %+v", n)
	}
	cfg.Fill.Full = 100
	o.Clock = func() time.Time { return now.Add(5 * time.Minute) }
	if _, err := Run(ctx, cfg, st, o); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := st.Note(ctx, "capacity:full:"+dir); n.Done.IsZero() {
		t.Error("a disk below its full percent again resolves it")
	}
}

// A disk's notes name it by its path, escaped where an id may not hold a character of it: a disk whose path holds a
// space has its fill, full and filled notes, and the run goes on. The notes' text names the disk as it is. An older
// build, whose run failed on the id, left the full stretch's mark with no note: the note comes, dated from the mark.
func TestADiskWhosePathHoldsASpaceHasItsNotes(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "my disk")
	must(t, os.MkdirAll(dir, 0o755))
	st := openStore(t)
	now := time.Now()
	var fs syscall.Statfs_t
	must(t, syscall.Statfs(dir, &fs))
	used := int64(fs.Blocks-fs.Bfree) * int64(fs.Bsize)
	// 24 minutes ago the disk held 15 GiB less: it fills fast, and it is past a full percent this low
	must(t, st.PutCapacityRun(ctx, store.CapacityRun{At: now.Add(-24 * time.Minute)},
		[]store.DiskSample{{Disk: dir, At: now.Add(-24 * time.Minute), Size: 1 << 40, Used: used - 15<<30, Avail: 1 << 39}}, nil))
	cfg := Config{Disks: []string{dir}, Fill: FillConfig{Full: 0.001}}.Normal()
	o := options(t, procRoot(t), now, nil)
	o.Rules = []string{RuleUV}
	stretch := now.Add(-time.Hour) // the older build's mark of the full stretch
	must(t, st.SetMark(ctx, "full-open:"+dir, stretch))
	if _, err := Run(ctx, cfg, st, o); err != nil {
		t.Fatalf("a disk whose path holds a space fails the run: %v", err)
	}
	key := strings.ReplaceAll(dir, " ", "%20")
	for _, id := range []string{"capacity:fill:" + key, "capacity:full:" + key} {
		if n, ok, _ := st.Note(ctx, id); !ok || n.Kind != store.NoteProblem || !n.Done.IsZero() || !strings.HasPrefix(n.Title, dir+" is ") {
			t.Errorf("%s: a problem note that names the disk as it is: %+v", id, n)
		}
	}
	if n, _, _ := st.Note(ctx, "capacity:full:"+key); n.At.UnixMilli() != stretch.UnixMilli() {
		t.Errorf("the full note is dated from its stretch's mark: %+v", n)
	}
	// 40 minutes on the fill is over, and the disk is below a full percent of 100
	cfg.Fill.Full = 100
	o.Clock = func() time.Time { return now.Add(40 * time.Minute) }
	if _, err := Run(ctx, cfg, st, o); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"capacity:fill:" + key, "capacity:full:" + key} {
		if n, _, _ := st.Note(ctx, id); n.Done.IsZero() {
			t.Errorf("%s: resolved: %+v", id, n)
		}
	}
	// a run that died between a new stretch's mark and its note, the last stretch's note resolved: the next run that
	// finds the disk full puts it, dated from the mark
	stretch = now.Add(50 * time.Minute)
	must(t, st.SetMark(ctx, "full-open:"+dir, stretch))
	cfg.Fill.Full = 0.001
	o.Clock = func() time.Time { return now.Add(55 * time.Minute) }
	if _, err := Run(ctx, cfg, st, o); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := st.Note(ctx, "capacity:full:"+key); !n.Done.IsZero() || n.At.UnixMilli() != stretch.UnixMilli() {
		t.Errorf("a new stretch's mark with no note of its own: the note comes, dated from the mark: %+v", n)
	}
	// a run that died once the stretch's mark went and before its note did: below the line, the note goes
	must(t, st.ClearMark(ctx, "full-open:"+dir))
	cfg.Fill.Full = 100
	o.Clock = func() time.Time { return now.Add(60 * time.Minute) }
	if _, err := Run(ctx, cfg, st, o); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := st.Note(ctx, "capacity:full:"+key); n.Done.IsZero() {
		t.Errorf("a full note that stands with no mark goes once the disk is below the line: %+v", n)
	}
	if n, ok, _ := st.Note(ctx, "capacity:filled:"+key); !ok || n.Kind != store.NoteDecided ||
		!strings.HasPrefix(n.Title, dir+" filled fast from ") {
		t.Errorf("the fill that ended is a decided note: %+v", n)
	}
}

// A target dir that holds what Cargo does not build again keeps it; Cargo's own entries go.
func TestATargetKeepsWhatCargoDoesNotBuild(t *testing.T) {
	gitEnv(t)
	root := filepath.Join(t.TempDir(), "projects")
	app := filepath.Join(root, "app")
	write(t, filepath.Join(app, ".gitignore"), "target/\n")
	gitT(t, app, "init", "-q")
	tgt := filepath.Join(app, "target")
	cargoTarget(t, tgt)
	blob(t, filepath.Join(tgt, "criterion", "bench", "base", "estimates.json"), 2<<20)
	cfg := Config{Targets: TargetsConfig{Roots: []string{root}}}.Normal()
	cfg.Live = []string{RuleTargets}
	res, err := Run(context.Background(), cfg, openStore(t), options(t, procRoot(t), time.Now().Add(72*time.Hour), nil))
	if err != nil {
		t.Fatal(err)
	}
	if f := findings(res, RuleTargets)[tgt]; !f.Done || !strings.Contains(f.Detail, "criterion") {
		t.Fatalf("%+v", f)
	}
	if _, err := os.Stat(filepath.Join(tgt, "debug")); !os.IsNotExist(err) {
		t.Error("Cargo's own output goes")
	}
	if _, err := os.Stat(filepath.Join(tgt, "criterion", "bench", "base", "estimates.json")); err != nil {
		t.Error("Criterion's saved baselines stay")
	}
}

// A lock another process holds on one of a target dir's own entries keeps them all, so no part goes before it.
func TestALockOnOnePartOfATargetKeepsEveryPart(t *testing.T) {
	gitEnv(t)
	root := filepath.Join(t.TempDir(), "projects")
	app := filepath.Join(root, "app")
	write(t, filepath.Join(app, ".gitignore"), "target/\n")
	gitT(t, app, "init", "-q")
	tgt := filepath.Join(app, "target")
	cargoTarget(t, tgt)
	blob(t, filepath.Join(tgt, "criterion", "bench", "base", "estimates.json"), 2<<20)
	blob(t, filepath.Join(tgt, "release", "app"), 2<<20)
	held, err := os.Open(filepath.Join(tgt, "release"))
	must(t, err)
	defer held.Close()
	must(t, syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	cfg := Config{Targets: TargetsConfig{Roots: []string{root}}}.Normal()
	cfg.Live = []string{RuleTargets}
	res, err := Run(context.Background(), cfg, openStore(t), options(t, procRoot(t), time.Now().Add(72*time.Hour), nil))
	if err != nil {
		t.Fatal(err)
	}
	if f := findings(res, RuleTargets)[tgt]; f.Done || f.Action != ActKeep {
		t.Fatalf("a lock on a part keeps the target dir: %+v", f)
	}
	for _, p := range []string{"debug", "CACHEDIR.TAG", ".rustc_info.json", "release/app"} {
		if _, err := os.Stat(filepath.Join(tgt, p)); err != nil {
			t.Errorf("%s stays while another process holds a lock on a part: %v", p, err)
		}
	}
}

// A typo in the capacity section is refused: it would drop a protection without a word.
func TestAnUnknownKeyInTheCapacitySectionIsRefused(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("MOMENTUM_CONFIG", cfgDir)
	write(t, filepath.Join(cfgDir, "sources.json"), `{"radar": {"path": "/x"}, "capacity": {"exclude": {"path": ["/p"]}}}`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("got %v", err)
	}
	write(t, filepath.Join(cfgDir, "sources.json"), `{"radar": {"path": "/x"}, "capacity": {"exclude": {"paths": ["/p"]}}}`)
	if c, err := Load(); err != nil || len(c.Exclude.Paths) != 1 {
		t.Fatalf("%+v %v", c, err)
	}
}

// systemctl's listings and its show output give each unit's files and expanded commands, a template's instances,
// what a timer starts, the environment files and the scripts the commands run; a user unit's file that cannot be
// read is an error.
func TestUnitsAreReadAsSystemdExpandsThem(t *testing.T) {
	dir := t.TempDir()
	frag := filepath.Join(dir, "serve@.service")
	write(t, frag, "[Service]\nExecStart=/usr/bin/serve %i\n")
	env := filepath.Join(dir, "serve.env")
	write(t, env, "MODEL=/data/models/m1\n")
	script := filepath.Join(dir, "job.sh")
	write(t, script, "#!/bin/sh\nrun /data/models/m2\n")
	secret := filepath.Join(dir, "secret.env")
	write(t, secret, "X=/data/models/m3\n")
	must(t, os.Chmod(secret, 0))
	if _, err := os.ReadFile(secret); err == nil {
		t.Skip("running as a user who reads every file")
	}
	systemctl := func(args ...string) (string, error) {
		switch {
		case contains(args, "list-unit-files"):
			return "serve@.service enabled enabled\nnightly.timer enabled enabled\n", nil
		case contains(args, "list-units"):
			return "serve@m1.service loaded active running Serve m1\n", nil
		case contains(args, "Triggers"):
			return "Triggers=nightly.service\n\n", nil
		}
		return "Id=serve@m1.service\nFragmentPath=" + frag + "\nEnvironmentFiles=" + env + " (ignore_errors=no)\n" +
			"ExecStart={ path=/usr/bin/serve ; argv[]=/usr/bin/serve m1 /data/glob/*.bin ; ignore_errors=no }\n" +
			"WorkingDirectory=/srv/work\n\n" +
			"Id=nightly.service\nExecStart={ path=" + script + " ; argv[]=" + script + " ; ignore_errors=no }\n" +
			"EnvironmentFiles=" + secret + " (ignore_errors=no)\n\n", nil
	}
	units, unread, err := unitsOf(context.Background(), []string{"--user"}, systemctl)
	if err != nil {
		t.Fatal(err)
	}
	text := map[string]string{}
	for _, u := range units {
		text[u.Name] = u.Text
	}
	if !strings.Contains(text["user:serve@m1.service"], "/data/models/m1") || !strings.Contains(text["user:nightly.service"], "/data/models/m2") {
		t.Fatalf("an instance's environment file and a timer's script are read: %v", text)
	}
	if len(unread) != 1 || !strings.Contains(unread[0], "secret.env") {
		t.Fatalf("the unreadable environment file is named: %v", unread)
	}
	var dirOf string
	for _, u := range units {
		if u.Name == "user:serve@m1.service" {
			dirOf = u.Dir
		}
	}
	if dirOf != "/srv/work" {
		t.Errorf("a unit's relative words are relative to its working directory: %q", dirOf)
	}
}
