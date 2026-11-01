package capacity

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// onDisk is what these paths hold on disk, each file once, as du counts it.
func onDisk(t *testing.T, paths ...string) int64 {
	t.Helper()
	seen := map[[2]uint64]bool{}
	var n int64
	for _, p := range paths {
		filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			s := fi.Sys().(*syscall.Stat_t)
			if k := [2]uint64{uint64(s.Dev), s.Ino}; !seen[k] {
				seen[k] = true
				n += s.Blocks * 512
			}
			return nil
		})
	}
	return n
}

// findingsJSON are a rule's findings as `momentum capacity --json` prints them, by path.
func findingsJSON(t *testing.T, res *Result, rule string) map[string]map[string]any {
	t.Helper()
	b, err := json.Marshal(res)
	must(t, err)
	var out struct {
		Rules []struct {
			Rule     string           `json:"rule"`
			Findings []map[string]any `json:"findings"`
		} `json:"rules"`
	}
	must(t, json.Unmarshal(b, &out))
	got := map[string]map[string]any{}
	for _, r := range out.Rules {
		if r.Rule == rule {
			for _, f := range r.Findings {
				got[f["path"].(string)] = f
			}
		}
	}
	return got
}

// twoLinkedTargets is a repository that ignores its build output, with two idle target dirs: the first holds 8 MiB
// only it links, 8 MiB a file outside both links too, and 4 MiB it shares with the second, which holds 2 MiB of its
// own beside it. Each holds Cargo's 2 MiB library as well.
func twoLinkedTargets(t *testing.T) (cfg Config, a, b, outside string) {
	t.Helper()
	gitEnv(t)
	base := t.TempDir()
	t.Setenv("HOME", base)
	root := filepath.Join(base, "projects")
	app := filepath.Join(root, "app")
	write(t, filepath.Join(app, ".gitignore"), "target/\n")
	write(t, filepath.Join(app, "src", "main.rs"), "fn main() {}\n")
	gitT(t, app, "init", "-q")
	gitT(t, app, "add", ".")
	gitT(t, app, "commit", "-qm", "init")
	a, b = filepath.Join(app, "target"), filepath.Join(app, "crates", "b", "target")
	cargoTarget(t, a)
	cargoTarget(t, b)
	deps := filepath.Join(a, "debug", "deps")
	blob(t, filepath.Join(deps, "libown.rlib"), 8<<20)
	blob(t, filepath.Join(deps, "libout.rlib"), 8<<20)
	outside = filepath.Join(base, "release", "libout.rlib")
	must(t, os.MkdirAll(filepath.Dir(outside), 0o755))
	must(t, os.Link(filepath.Join(deps, "libout.rlib"), outside))
	blob(t, filepath.Join(deps, "libshared.rlib"), 4<<20)
	must(t, os.Link(filepath.Join(deps, "libshared.rlib"), filepath.Join(b, "debug", "deps", "libshared.rlib")))
	blob(t, filepath.Join(b, "debug", "deps", "libb.rlib"), 2<<20)
	cfg = Config{Targets: TargetsConfig{Roots: []string{root}}}.Normal()
	return cfg, a, b, outside
}

func TestADeletionFreesOnlyTheFilesWhoseLastLinkItRemoves(t *testing.T) {
	ctx := context.Background()
	cfg, a, b, outside := twoLinkedTargets(t)
	shared := filepath.Join(a, "debug", "deps", "libshared.rlib")
	// a frees what only it holds: a file outside links libout, and b still links libshared when a goes (the larger
	// goes first); b then frees all it holds, libshared's last link among it
	wantA := onDisk(t, a) - onDisk(t, outside) - onDisk(t, shared)
	wantB := onDisk(t, b)
	st := openStore(t)
	now := time.Now().Add(72 * time.Hour)
	dry := options(t, procRoot(t), now, nil)
	dry.DryRun = true
	res, err := Run(ctx, cfg, st, dry)
	must(t, err)
	est := findingsJSON(t, res, RuleTargets)
	for p, want := range map[string]int64{a: wantA, b: wantB} {
		if got, _ := est[p]["would_free"].(float64); est[p]["action"] != ActDelete || int64(got) != want {
			t.Errorf("a dry run says what deleting %s would free, %d, the files whose last link it removes: %v", p, want, est[p])
		}
	}
	cfg.Live = []string{RuleTargets}
	live := options(t, procRoot(t), now, nil)
	writer := filepath.Join(filepath.Dir(outside), "writer")
	live.duringDelete = func(f Finding) { // another process writes on the disk while each goes
		blob(t, filepath.Join(writer, filepath.Base(filepath.Dir(f.Path))), 16<<20)
	}
	res, err = Run(ctx, cfg, st, live)
	must(t, err)
	got := findings(res, RuleTargets)
	if !got[a].Done || !got[b].Done || got[a].Freed != wantA || got[b].Freed != wantB {
		t.Errorf("the run frees what the dry run said, whatever else the disk does meanwhile: %d and %d, want %d and %d",
			got[a].Freed, got[b].Freed, wantA, wantB)
	}
	if fi, err := os.Stat(outside); err != nil || fi.Size() != 8<<20 {
		t.Errorf("the file outside stays whole: %v", err)
	}
	ns := decidedNotes(t, st)
	if len(ns) != 1 || ns[0].Title != "Freed "+gib(wantA+wantB)+": 2 Rust build outputs" ||
		!strings.Contains(ns[0].Detail, short(a)+" "+gib(got[a].Bytes)+" ("+gib(wantA)+" freed)") ||
		!strings.Contains(ns[0].Detail, "Files still hardlinked elsewhere stay on disk.") {
		t.Errorf("the note's title says what the disk gained, its detail each item's size and what it freed: %+v", ns)
	}
	evs, err := st.Events(ctx, store.Query{Source: store.Capacity, Kinds: []string{KindDelete}})
	must(t, err)
	for _, e := range evs {
		var m map[string]any
		must(t, json.Unmarshal([]byte(e.Meta), &m))
		if want := map[string]int64{a: wantA, b: wantB}[e.Ref]; m["freed"] != float64(want) {
			t.Errorf("%s: the event keeps what it freed, %d: %v", e.Ref, want, m["freed"])
		}
	}
}

// cacheCounting answers uv's cache size from the cache itself, as uv counts it: what is in it, whatever links its
// files from outside.
type cacheCounting struct {
	lockCheckingUV
	cache string
}

func (c cacheCounting) UV(ctx context.Context, d time.Duration, args ...string) (string, error) {
	if strings.HasPrefix(strings.Join(args, " "), "cache size") {
		var n int64
		filepath.WalkDir(c.cache, func(_ string, e fs.DirEntry, err error) error {
			if fi, ierr := e.Info(); err == nil && ierr == nil && fi.Mode().IsRegular() {
				n += fi.Size()
			}
			return nil
		})
		return strconv.FormatInt(n, 10) + "\n", nil
	}
	return c.lockCheckingUV.UV(ctx, d, args...)
}

func TestAPruneAroundRunningEnvironmentsFreesNoWheelAnEnvironmentOutsideLinks(t *testing.T) {
	f := newUVLock(t, byPATH("archive-v0/envx"), 4, 6, 8)
	uvEntries(t, f.cache, "archive-v0/envx") // a process runs from it
	// two wheels no process runs from, one of them linked by a project's environment outside the cache, as uv's
	// default link mode makes it: the cache's link to it goes, and its file stays on disk
	own, linked := filepath.Join(f.cache, "archive-v0", "wheel-own"), filepath.Join(f.cache, "archive-v0", "wheel-linked")
	blob(t, filepath.Join(own, "pkg", "core.so"), 8<<20)
	blob(t, filepath.Join(linked, "pkg", "core.so"), 8<<20)
	venv := filepath.Join(t.TempDir(), "proj", ".venv", "lib", "python3.12", "site-packages", "pkg", "core.so")
	must(t, os.MkdirAll(filepath.Dir(venv), 0o755))
	must(t, os.Link(filepath.Join(linked, "pkg", "core.so"), venv))
	want := onDisk(t, own, linked) - onDisk(t, venv)
	o := options(t, f.proc, time.Now(), nil)
	o.Tools, o.Rules = cacheCounting{f.tools(nil), f.cache}, []string{RuleUV}
	st := openStore(t)
	res, err := Run(context.Background(), Config{Live: []string{RuleUV}}.Normal(), st, o)
	must(t, err)
	g := findings(res, RuleUV)[f.cache]
	if !f.around(g) || g.Freed != want {
		t.Fatalf("the prune around frees the wheel only the cache held (%d), not the one an environment outside links: %d %+v",
			want, g.Freed, g)
	}
	if fi, err := os.Stat(venv); err != nil || fi.Size() != 8<<20 {
		t.Errorf("the environment's file stays whole: %v", err)
	}
	ns := decidedNotes(t, st)
	if len(ns) != 1 || ns[0].Title != "Freed 8 MiB: pruned the uv cache" ||
		!strings.HasPrefix(ns[0].Detail, "Pruned "+short(f.cache)+" 16 MiB (8 MiB freed). Files still hardlinked elsewhere stay on disk.") {
		t.Errorf("the note's title says what the disk gained, and its detail what went out of the cache: %+v", ns)
	}
}

func TestUVsOwnPruneIsNamedWithWhatUVSaysItFreed(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	pruned := false
	tools := uvTools("40000000000", nil)
	tools.answers["uv cache prune"] = "Pruning cache at: /home/u/.cache/uv\nRemoved 412 files (48.0KiB)\n"
	o := options(t, procRoot(t), time.Now(), nil)
	o.Tools, o.Rules = shrinking{tools, "25000000000", &pruned}, []string{RuleUV}
	st := openStore(t)
	ctx := context.Background()
	res, err := Run(ctx, Config{Live: []string{RuleUV}}.Normal(), st, o)
	must(t, err)
	if g := findings(res, RuleUV)["/home/u/.cache/uv"]; !g.Done || g.Freed != 48<<10 {
		t.Fatalf("uv's own prune frees what uv says it freed: %+v", g)
	}
	// 15 GB left the cache: uv counts a wheel an environment still links as freeing nothing
	ns := decidedNotes(t, st)
	if len(ns) != 1 || ns[0].Title != "Pruned the uv cache (under 1 MiB by uv's count)" ||
		!strings.HasPrefix(ns[0].Detail, "Pruned ~/.cache/uv 14.0 GiB (under 1 MiB freed by uv's count).") {
		t.Errorf("the note names uv's figure as uv's, not as what the run freed: %+v", ns)
	}
	evs, err := st.Events(ctx, store.Query{Source: store.Capacity, Kinds: []string{KindPrune}})
	if err != nil || len(evs) != 1 || !strings.HasSuffix(evs[0].Summary, "14.0 GiB out of it, freed under 1 MiB by uv's count") {
		t.Errorf("the prune's event says what left the cache and what uv says it freed: %+v %v", evs, err)
	}
}

func TestASharedBlobFreesWithTheLastRepositoryThatLinksIt(t *testing.T) {
	f := newModels(t)
	st := openStore(t)
	ctx := context.Background()
	watchedSince(t, st, f.cfg, f.now.Add(-35*24*time.Hour), f.now)
	dry := options(t, procRoot(t), f.now, nil)
	dry.DryRun = true
	res, err := Run(ctx, f.cfg, st, dry)
	must(t, err)
	est := findingsJSON(t, res, RuleModels)
	two := filepath.Join(f.hub, "models--org--two")
	blob := filepath.Join(f.hub, "blobs", "aa", "shared")
	if got, _ := est[two]["would_free"].(float64); int64(got) < onDisk(t, blob, blob+".refs") {
		t.Errorf("the last repository to go frees the blob both link: %v", est[two])
	}
	cfg := f.cfg
	cfg.Live = []string{RuleModels}
	res, err = Run(ctx, cfg, st, options(t, procRoot(t), f.now, nil))
	must(t, err)
	n := 0
	for p, g := range findings(res, RuleModels) {
		if !g.Done {
			continue
		}
		n++
		if want, _ := est[p]["would_free"].(float64); g.Freed != int64(want) {
			t.Errorf("%s freed %d, the dry run said %v", p, g.Freed, want)
		}
	}
	if n != 7 {
		t.Errorf("seven items go: %d", n)
	}
}

func TestATaskFoldersActCountsWhatOnlyItHeldWhateverElseTheDiskDoes(t *testing.T) {
	f := newScratch(t)
	t.Setenv("HOME", f.base)
	f.cfg.Live = []string{RuleScratch}
	p := f.task(t, "zq-linked", "s1", map[string]int{"w.safetensors": 32 << 20, "notes.md": -1, "env/lib/core.so": 8 << 20})
	outside := filepath.Join(f.base, "proj", "core.so") // an environment outside the folder links one of its files
	must(t, os.MkdirAll(filepath.Dir(outside), 0o755))
	must(t, os.Link(filepath.Join(p, "env", "lib", "core.so"), outside))
	model := onDisk(t, filepath.Join(p, "w.safetensors"))
	rest := onDisk(t, p) - model - onDisk(t, outside)
	o := options(t, procRoot(t), f.now, nil)
	o.beforeModelDelete = func(Finding) { // another process writes on the disk meanwhile
		blob(t, filepath.Join(f.base, "writer", "out.bin"), 64<<20)
	}
	g := findings(f.run(t, o), RuleScratch)[p]
	if !g.Done || g.Freed != model+rest {
		t.Fatalf("the act frees its model data and what only the folder held: %d, want %d + %d: %+v", g.Freed, model, rest, g)
	}
	if fi, err := os.Stat(outside); err != nil || fi.Size() != 8<<20 {
		t.Errorf("the file outside stays whole: %v", err)
	}
	ctx := context.Background()
	for kind, want := range map[string]int64{KindDelete: model, KindOffload: rest} {
		evs, err := f.st.Events(ctx, store.Query{Source: store.Capacity, Kinds: []string{kind}})
		must(t, err)
		var m map[string]any
		if len(evs) == 1 {
			must(t, json.Unmarshal([]byte(evs[0].Meta), &m))
		}
		if len(evs) != 1 || m["freed"] != float64(want) {
			t.Errorf("%s: its event keeps what it freed, %d: %+v", kind, want, evs)
		}
	}
}

func TestAnItemHoldingOneDeletedBeforeItCountsWhatIsLeft(t *testing.T) {
	ctx := context.Background()
	clone, wts := worktreesFixture(t)
	// a leftover worktree with an idle build of its own: the targets rule takes the build, then the worktrees rule
	// the worktree, and what each frees counts once
	merged := filepath.Join(wts, "merged")
	target := filepath.Join(merged, "target")
	write(t, filepath.Join(clone, ".git", "info", "exclude"), "target/\n")
	cargoTarget(t, target)
	b, err := os.ReadFile(filepath.Join(merged, ".git"))
	must(t, err)
	admin := strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir: ")
	wantT, wantW := onDisk(t, target), onDisk(t, merged)-onDisk(t, target)+onDisk(t, admin)
	now := time.Now().Add(49 * time.Hour)
	cfg := Config{Worktrees: WorktreesConfig{Roots: []string{wts}}, Targets: TargetsConfig{Roots: []string{wts}},
		Exclude: Exclude{Names: []string{"secretlane"}}}.Normal()
	st := openStore(t)
	dry := options(t, procRoot(t), now, nil)
	dry.DryRun = true
	res, err := Run(ctx, cfg, st, dry)
	must(t, err)
	if got, _ := findingsJSON(t, res, RuleTargets)[target]["would_free"].(float64); int64(got) != wantT {
		t.Errorf("the build would free all it holds, %d: %v", wantT, got)
	}
	if got, _ := findingsJSON(t, res, RuleWorktrees)[merged]["would_free"].(float64); int64(got) != wantW {
		t.Errorf("the worktree would free what is left of it once its build went, with its administrative directory, %d: %v", wantW, got)
	}
	cfg.Live = []string{RuleTargets, RuleWorktrees}
	res, err = Run(ctx, cfg, st, options(t, procRoot(t), now, nil))
	must(t, err)
	if g := findings(res, RuleTargets)[target]; !g.Done || g.Freed != wantT {
		t.Errorf("the build frees %d: %+v", wantT, g)
	}
	if g := findings(res, RuleWorktrees)[merged]; !g.Done || g.Freed != wantW {
		t.Errorf("the worktree frees %d, what the dry run said: %d %+v", wantW, g.Freed, g)
	}
}

func TestABlobAKeptRepositoryLinksIsNotTheOtherRepositorys(t *testing.T) {
	f := newModels(t)
	two := filepath.Join(f.hub, "models--org--two")
	f.cfg.Models.Keep = append(f.cfg.Models.Keep, two) // its link to the shared blob keeps the blob
	st := openStore(t)
	ctx := context.Background()
	watchedSince(t, st, f.cfg, f.now.Add(-35*24*time.Hour), f.now)
	one := filepath.Join(f.hub, "models--org--one")
	shared := filepath.Join(f.hub, "blobs", "aa", "shared")
	dry := options(t, procRoot(t), f.now, nil)
	dry.DryRun = true
	res, err := Run(ctx, f.cfg, st, dry)
	must(t, err)
	est := findingsJSON(t, res, RuleModels)[one]
	if size, _ := est["bytes"].(float64); int64(size) != onDisk(t, one, filepath.Join(f.hub, "blobs", "bb", "sole")) {
		t.Errorf("the repository's size is its own and the blob only it links, not the one the kept repository links: %v", est)
	}
	cfg := f.cfg
	cfg.Live = []string{RuleModels}
	res, err = Run(ctx, cfg, st, options(t, procRoot(t), f.now, nil))
	must(t, err)
	g := findings(res, RuleModels)[one]
	if would, _ := est["would_free"].(float64); !g.Done || g.Freed != int64(would) {
		t.Errorf("what the dry run says it would free is what it freed: %v, %d", est["would_free"], g.Freed)
	}
	if _, err := os.Stat(shared); err != nil {
		t.Errorf("the blob the kept repository links stays: %v", err)
	}
}

func TestARuleNotLiveIsEstimatedAfterWhatTheLiveRulesDeleted(t *testing.T) {
	ctx := context.Background()
	clone, wts := worktreesFixture(t)
	merged := filepath.Join(wts, "merged")
	target := filepath.Join(merged, "target")
	write(t, filepath.Join(clone, ".git", "info", "exclude"), "target/\n")
	cargoTarget(t, target)
	b, err := os.ReadFile(filepath.Join(merged, ".git"))
	must(t, err)
	admin := strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir: ")
	want := onDisk(t, merged) - onDisk(t, target) + onDisk(t, admin)
	cfg := Config{Worktrees: WorktreesConfig{Roots: []string{wts}}, Targets: TargetsConfig{Roots: []string{wts}},
		Exclude: Exclude{Names: []string{"secretlane"}}}.Normal()
	cfg.Live = []string{RuleTargets} // the build goes; the worktree is only listed
	res, err := Run(ctx, cfg, openStore(t), options(t, procRoot(t), time.Now().Add(49*time.Hour), nil))
	must(t, err)
	if g := findings(res, RuleTargets)[target]; !g.Done {
		t.Fatalf("the build goes: %+v", g)
	}
	if got, _ := findingsJSON(t, res, RuleWorktrees)[merged]["would_free"].(float64); int64(got) != want {
		t.Errorf("the worktree would free what is left of it after the run, %d: %v", want, got)
	}
}

func TestWhatUVSaysItFreedAtLeastIsALowerBoundEverywhere(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	for _, c := range []struct {
		report, title, detail string
	}{
		{"Removed 3 files (at least 1.5MiB)\n", "Pruned the uv cache (at least 2 MiB by uv's count)",
			"Pruned ~/.cache/uv 14.0 GiB (at least 2 MiB freed by uv's count)."},
		{"Removed 3 files\n", "Pruned the uv cache (uv printed no total)", "Pruned ~/.cache/uv 14.0 GiB (uv printed no total)."},
	} {
		pruned := false
		tools := uvTools("40000000000", nil)
		tools.answers["uv cache prune"] = c.report
		o := options(t, procRoot(t), time.Now(), nil)
		o.Tools, o.Rules = shrinking{tools, "25000000000", &pruned}, []string{RuleUV}
		st := openStore(t)
		if _, err := Run(context.Background(), Config{Live: []string{RuleUV}}.Normal(), st, o); err != nil {
			t.Fatal(err)
		}
		ns := decidedNotes(t, st)
		if len(ns) != 1 || ns[0].Title != c.title || !strings.HasPrefix(ns[0].Detail, c.detail) {
			t.Errorf("%q: the note says what uv said, and what left the cache: %+v", c.report, ns)
		}
	}
}
