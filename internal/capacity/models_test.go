package capacity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// modelsFixture is a models root with items of every kind, a nested root, a Hugging Face cache with a shared blob
// store, and a clock 40 days past the files' writes.
type modelsFixture struct {
	root, hub, runs string
	now             time.Time
	cfg             Config
}

func newModels(t *testing.T) modelsFixture {
	t.Helper()
	base := t.TempDir()
	f := modelsFixture{root: filepath.Join(base, "models"), hub: filepath.Join(base, "hf"), now: time.Now().Add(40 * 24 * time.Hour)}
	f.runs = filepath.Join(f.root, "_runs")
	blob(t, filepath.Join(f.root, "idle", "w.safetensors"), 3<<20)
	blob(t, filepath.Join(f.root, "fresh", "w.safetensors"), 3<<20)
	must(t, os.Chtimes(filepath.Join(f.root, "fresh", "w.safetensors"), f.now, f.now.Add(-24*time.Hour))) // written a day ago
	blob(t, filepath.Join(f.root, "single.gguf"), 2<<20)
	blob(t, filepath.Join(f.root, "kept"), 2<<20)
	write(t, filepath.Join(f.root, "token"), "secret") // smaller than MinMiB
	blob(t, filepath.Join(f.runs, "lane-a", "ckpt.pt"), 2<<20)
	blob(t, filepath.Join(f.root, "named", "w.bin"), 2<<20)
	blob(t, filepath.Join(f.root, "used", "w.bin"), 2<<20)
	// a Hugging Face cache: two repositories linking one shared blob, one linking its own
	store := filepath.Join(f.hub, "blobs")
	write(t, filepath.Join(store, hfStoreMarker), "1")
	blob(t, filepath.Join(store, "aa", "shared"), 4<<20)
	blob(t, filepath.Join(store, "bb", "sole"), 5<<20)
	write(t, filepath.Join(store, "aa", "shared.refs"), "models--org--one/blobs/e1\nmodels--org--two/blobs/e2\n")
	write(t, filepath.Join(store, "bb", "sole.refs"), "models--org--one/blobs/e3\n")
	for repo, links := range map[string]map[string]string{
		"models--org--one": {"e1": "../../blobs/aa/shared", "e3": "../../blobs/bb/sole"},
		"models--org--two": {"e2": "../../blobs/aa/shared"},
	} {
		must(t, os.MkdirAll(filepath.Join(f.hub, repo, "blobs"), 0o755))
		for e, target := range links {
			must(t, os.Symlink(target, filepath.Join(f.hub, repo, "blobs", e)))
		}
	}
	f.cfg = Config{Models: ModelsConfig{Roots: []string{f.root, f.runs, f.hub}, Keep: []string{filepath.Join(f.root, "kept")}}}.Normal()
	return f
}

func TestModelsWaitForTheSamplersRecordToCoverTheIdleTime(t *testing.T) {
	f := newModels(t)
	st := openStore(t)
	res, err := Run(context.Background(), f.cfg, st, func() Options {
		o := options(t, procRoot(t), f.now, nil)
		o.DryRun = true
		return o
	}())
	if err != nil {
		t.Fatal(err)
	}
	got := findings(res, RuleModels)
	idle := got[filepath.Join(f.root, "idle")]
	if idle.Action != ActPending || !idle.Eligible.Equal(f.now.Add(30*24*time.Hour)) {
		t.Fatalf("an item 40 days without a write, on a sampler that starts now, waits 30 days: %+v", idle)
	}
	for _, a := range got {
		if a.Action == ActDelete {
			t.Errorf("nothing qualifies before the sampler covers 30 days: %s", a.Path)
		}
	}
}

func TestModelsDeleteWhatNoWriteAndNoUseTouchedFor30Days(t *testing.T) {
	f := newModels(t)
	st := openStore(t)
	ctx := context.Background()
	// the sampler has watched for 35 days, and it saw the "used" item 10 days ago
	watchedSince(t, st, f.cfg, f.now.Add(-35*24*time.Hour), f.now)
	must(t, st.PutCapacityRun(ctx, store.CapacityRun{At: f.now.Add(-10*24*time.Hour - time.Minute)}, nil, []store.Seen{
		{Path: filepath.Join(f.root, "used"), Last: f.now.Add(-10 * 24 * time.Hour), How: "map", Proc: "vllm 7"}}))
	proc := procRoot(t, fakeProc{pid: 7, ppid: 1, argv: []string{"server", "--model", "org/two"},
		fds: []string{filepath.Join(f.root, "single.gguf")}})
	jobs := fakeJobs{units: map[string]string{"user:x.service": "[Service]\nExecStart=/bin/run " + filepath.Join(f.root, "named", "w.bin") + "\n"}}
	o := options(t, proc, f.now, jobs)
	o.DryRun = true
	res, err := Run(ctx, f.cfg, st, o)
	if err != nil {
		t.Fatal(err)
	}
	got := findings(res, RuleModels)
	want := map[string]string{
		filepath.Join(f.root, "idle"):            ActDelete,
		filepath.Join(f.root, "single.gguf"):     ActKeep,    // open now
		filepath.Join(f.root, "named"):           ActKeep,    // a unit names a file in it
		filepath.Join(f.root, "used"):            ActPending, // seen 10 days ago
		filepath.Join(f.runs, "lane-a"):          ActDelete,  // an item of the nested root
		filepath.Join(f.hub, "models--org--one"): ActDelete,
		filepath.Join(f.hub, "models--org--two"): ActKeep, // its id is on a command line
	}
	for p, a := range want {
		if got[p].Action != a {
			t.Errorf("%s: %q (%s), want %q", p, got[p].Action, got[p].Why, a)
		}
	}
	for _, p := range []string{filepath.Join(f.root, "fresh"), filepath.Join(f.root, "kept"), filepath.Join(f.root, "token"),
		f.runs, filepath.Join(f.hub, "blobs")} {
		if _, ok := got[p]; ok {
			t.Errorf("%s is not an item to judge: written lately, kept, too small, a root or the shared store", p)
		}
	}
	one := got[filepath.Join(f.hub, "models--org--one")]
	if one.Bytes < 5<<20 || one.Bytes >= 9<<20 {
		t.Errorf("a repository counts the shared blob only it links (5 MiB), not the one it shares: %d", one.Bytes)
	}
}

func TestALiveModelsRunDeletesAnItemWithTheBlobsOnlyItLinked(t *testing.T) {
	f := newModels(t)
	st := openStore(t)
	ctx := context.Background()
	watchedSince(t, st, f.cfg, f.now.Add(-35*24*time.Hour), f.now)
	cfg := f.cfg
	cfg.Live = []string{RuleModels}
	res, err := Run(ctx, cfg, st, options(t, procRoot(t), f.now, nil))
	if err != nil {
		t.Fatal(err)
	}
	got := findings(res, RuleModels)
	for _, p := range []string{filepath.Join(f.root, "idle"), filepath.Join(f.hub, "models--org--one"), filepath.Join(f.hub, "models--org--two")} {
		if !got[p].Done {
			t.Errorf("%s was not deleted: %+v", p, got[p])
		}
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s is still there", p)
		}
	}
	if _, err := os.Stat(filepath.Join(f.root, "fresh")); err != nil {
		t.Error("an item written a day ago stays")
	}
	for _, b := range []string{"aa/shared", "bb/sole"} { // both repositories went, so both blobs
		if _, err := os.Stat(filepath.Join(f.hub, "blobs", b)); !os.IsNotExist(err) {
			t.Errorf("blob %s stays after every repository that linked it went", b)
		}
	}
	if _, err := os.Stat(filepath.Join(f.hub, "blobs", hfStoreMarker)); err != nil {
		t.Error("the store's marker stays")
	}
	deleted := 0
	for _, f := range got {
		if f.Done {
			deleted++
		}
	}
	if ns := decidedNotes(t, st); deleted != 7 || len(ns) != 1 || !strings.HasSuffix(ns[0].Title, ": 7 idle model items") {
		t.Errorf("seven items go (nothing uses or names them in this run), in the run's one decided note: %d deleted, %+v", deleted, ns)
	}
	evs, err := st.Events(ctx, store.Query{Source: store.Capacity, Kinds: []string{KindDelete}})
	if err != nil || len(evs) != deleted {
		t.Errorf("one event per deletion: %d %v", len(evs), err)
	}
}

func TestASharedBlobAnotherRepositoryStillLinksStays(t *testing.T) {
	f := newModels(t)
	st := openStore(t)
	ctx := context.Background()
	watchedSince(t, st, f.cfg, f.now.Add(-35*24*time.Hour), f.now)
	cfg := f.cfg
	cfg.Live = []string{RuleModels}
	// models--org--two is in use, so only models--org--one goes
	proc := procRoot(t, fakeProc{pid: 9, ppid: 1, argv: []string{"x"}, cwd: filepath.Join(f.hub, "models--org--two")})
	if _, err := Run(ctx, cfg, st, options(t, proc, f.now, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.hub, "blobs", "aa", "shared")); err != nil {
		t.Error("the blob the repository in use links stays")
	}
	if _, err := os.Stat(filepath.Join(f.hub, "blobs", "bb", "sole")); !os.IsNotExist(err) {
		t.Error("the blob only the deleted repository linked goes")
	}
}

func TestAScheduledJobsFailureDeletesNothing(t *testing.T) {
	f := newModels(t)
	st := openStore(t)
	ctx := context.Background()
	watchedSince(t, st, f.cfg, f.now.Add(-35*24*time.Hour), f.now)
	cfg := f.cfg
	cfg.Live = []string{RuleModels}
	res, err := Run(ctx, cfg, st, options(t, procRoot(t), f.now, fakeJobs{err: os.ErrPermission}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "idle")); err != nil {
		t.Fatal("an unreadable job list keeps every item")
	}
	if len(res.Problems) == 0 {
		t.Error("the run says why it deleted nothing")
	}
	if n, ok, _ := st.Note(ctx, "capacity:jobs"); !ok || n.Kind != store.NoteProblem {
		t.Errorf("and leaves a problem note: %+v", n)
	}
	o := options(t, procRoot(t), f.now.Add(time.Hour), nil)
	o.Now = true
	if _, err := Run(ctx, cfg, st, o); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := st.Note(ctx, "capacity:jobs"); n.Done.IsZero() {
		t.Error("the next run that reads the jobs resolves it")
	}
	if _, err := os.Stat(filepath.Join(f.root, "idle")); !os.IsNotExist(err) {
		t.Error("and deletes what it held back")
	}
}

func TestAContainersMountKeepsWhatItHolds(t *testing.T) {
	for _, c := range []struct{ name, mount string }{{"an item", "idle"}, {"the whole root", ""}} {
		t.Run(c.name, func(t *testing.T) {
			f := newModels(t)
			st := openStore(t)
			ctx := context.Background()
			watchedSince(t, st, f.cfg, f.now.Add(-35*24*time.Hour), f.now)
			cfg := f.cfg
			cfg.Live = []string{RuleModels}
			o := options(t, procRoot(t), f.now, nil)
			o.Tools = fakeTools{answers: map[string]string{"uv": "ERR", "docker ps": "c1\n",
				"docker inspect": "/vllm\t/etc/x\t" + filepath.Join(f.root, c.mount) + "\n"}}
			res, err := Run(ctx, cfg, st, o)
			if err != nil {
				t.Fatal(err)
			}
			got := findings(res, RuleModels)[filepath.Join(f.root, "idle")]
			if got.Action != ActKeep || !strings.Contains(got.Why, "container vllm") {
				t.Fatalf("a running container's bind mount keeps the item: %+v", got)
			}
			if _, err := os.Stat(filepath.Join(f.root, "idle")); err != nil {
				t.Fatal("it was deleted")
			}
			seen, _ := st.SeenPaths(ctx)
			if _, ok := seen[filepath.Join(f.root, "idle")]; !ok {
				t.Error("and the mount is a sighting")
			}
		})
	}
}
