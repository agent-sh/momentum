package capacity

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// fakeOffload stands in for evidence-offload: it logs each file of a folder it is given with the tag, leaves the
// pointer in the folder's place and removes the folder, as the real one does after its restic backup. A folder
// whose name holds "fail" fails, as a locked repository does, before anything is removed; one whose name holds
// "noop" is skipped with success, as the real one skips what is not a directory.
const fakeOffload = `#!/bin/sh
set -eu
log="$(dirname "$0")/offload.log"
for d in "$@"; do
  case "$d" in *fail*) echo "restic: repository is already locked" >&2; exit 1;; esac
  case "$d" in *noop*) echo "skip $d: not a directory"; continue;; esac
  (cd "$d" && find . -type f | sort | sed "s|^\./|$EVIDENCE_TAG $(basename "$d") |") >> "$log"
  printf 'evidence moved to restic\nsnapshot  0123abcd\n' > "$d.restic.txt"
  rm -rf "$d"
  echo "$d -> $d.restic.txt (snapshot 0123abcd, 1 files, 0 MiB)"
done
`

// scratchFixture is a cache directory of tasks' folders and tools' caches, a store whose transcripts say which
// folders an agent made, a sampler record of 35 days, and a clock 40 days past the files' writes.
type scratchFixture struct {
	base, root, links, offload string
	now                        time.Time
	cfg                        Config
	st                         *store.Store
}

func newScratch(t *testing.T) *scratchFixture {
	t.Helper()
	base := t.TempDir()
	f := &scratchFixture{base: base, root: filepath.Join(base, "cache"), links: filepath.Join(base, "bin"),
		offload: filepath.Join(base, "evidence-offload"), now: time.Now().Add(40 * 24 * time.Hour)}
	must(t, os.MkdirAll(f.root, 0o755))
	must(t, os.MkdirAll(f.links, 0o755))
	write(t, f.offload, fakeOffload)
	must(t, os.Chmod(f.offload, 0o755))
	f.cfg = Config{Scratch: ScratchConfig{Roots: []string{f.root}, Offload: f.offload}, Names: NamesConfig{Links: []string{f.links}}}.Normal()
	f.st = openStore(t)
	watchedSince(t, f.st, f.cfg, f.now.Add(-35*24*time.Hour), f.now)
	return f
}

// folder makes a folder at the top of the cache with files: a size of at least 0 is a file of that many bytes; a
// negative one, a short text.
func (f *scratchFixture) folder(t *testing.T, name string, files map[string]int) string {
	t.Helper()
	p := filepath.Join(f.root, name)
	must(t, os.MkdirAll(p, 0o755))
	for rel, n := range files {
		if n < 0 {
			write(t, filepath.Join(p, rel), "x\n")
		} else {
			blob(t, filepath.Join(p, rel), n)
		}
	}
	return p
}

// task is a folder an agent made: a tool call in the store names it at its birth.
func (f *scratchFixture) task(t *testing.T, name, session string, files map[string]int) string {
	t.Helper()
	p := f.folder(t, name, files)
	f.named(t, p, session, toolCall, 0, `{"command":"mkdir -p `+p+` && cd `+p+`"}`, "")
	return p
}

// named puts a transcript event in the store, off after p's birth.
func (f *scratchFixture) named(t *testing.T, p, session, kind string, off time.Duration, summary, cwd string) {
	t.Helper()
	born, ok := birth(p)
	if !ok {
		t.Fatalf("%s has no birth time", p)
	}
	_, err := f.st.Put(context.Background(), []store.Event{{Source: store.Transcripts, SourceKey: p + "@" + kind + off.String(),
		TS: born.Add(off), Session: session, Harness: "claude", Actor: store.ActorAgent, Kind: kind, Tool: "Bash",
		Summary: summary, Cwd: cwd}})
	must(t, err)
}

// ago sets the times of everything under p to d before the fixture's clock.
func (f *scratchFixture) ago(t *testing.T, p string, d time.Duration) {
	t.Helper()
	at := f.now.Add(-d)
	must(t, filepath.Walk(p, func(q string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(q, at, at)
	}))
}

func (f *scratchFixture) run(t *testing.T, o Options) *Result {
	t.Helper()
	res, err := Run(context.Background(), f.cfg, f.st, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Rules {
		if r.Rule == RuleScratch && r.Err != "" {
			t.Fatalf("the scratch rule failed: %s", r.Err)
		}
	}
	return res
}

func (f *scratchFixture) offloadLog(t *testing.T) string {
	b, err := os.ReadFile(filepath.Join(f.base, "offload.log"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// gguf is a file of n bytes that starts as a GGUF file does: an Ollama blob, named by its hash.
func gguf(t *testing.T, p string, n int) {
	b := make([]byte, n)
	copy(b, "GGUF\x03\x00\x00\x00")
	must(t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(t, os.WriteFile(p, b, 0o644))
}

// torchZip is a file of n bytes that starts as a PyTorch archive does: a zip whose first entry is its data.pkl.
func torchZip(t *testing.T, p string, n int) {
	name := "archive/data.pkl"
	b := make([]byte, n)
	copy(b, "PK\x03\x04")
	binary.LittleEndian.PutUint16(b[26:28], uint16(len(name)))
	copy(b[30:], name)
	must(t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(t, os.WriteFile(p, b, 0o644))
}

func TestATaskFolderIsOneAnAgentMadeAndAToolsCacheIsLeft(t *testing.T) {
	f := newScratch(t)
	t.Setenv("HOME", f.base) // the cache is ~/cache: a command may name it through ~
	small := map[string]int{"notes.md": -1, "out.log": 2 << 20}
	task := f.task(t, "zq-task", "s1", small)
	tagged := f.task(t, "zq-tagged", "s1", map[string]int{"x": 2 << 20})
	write(t, filepath.Join(tagged, "CACHEDIR.TAG"), cargoTag+"\n# a tool's cache\n")
	f.task(t, "zq-mytool", "s1", small)
	write(t, filepath.Join(f.links, "zq-mytool"), "#!/bin/sh\n")
	f.task(t, "zq-back", "s1", small)
	write(t, filepath.Join(f.root, "zq-back"+pointerSuffix), "snapshot 1\n")
	f.folder(t, "zq-unmade", small)
	late := f.folder(t, "zq-late", small)
	f.named(t, late, "s1", toolCall, 20*time.Minute, `{"command":"ls `+late+`"}`, "")
	early := f.folder(t, "zq-early", small)
	f.named(t, early, "s1", toolCall, -20*time.Minute, `{"command":"rm -rf `+early+`"}`, "")
	f.task(t, "zq-hf", "s1", map[string]int{"hub/x": 2 << 20})
	cargoTarget(t, filepath.Join(f.root, "zq-tgt"))
	f.named(t, filepath.Join(f.root, "zq-tgt"), "s1", toolCall, 0, `{"command":"cargo build --target-dir `+filepath.Join(f.root, "zq-tgt")+`"}`, "")
	tmp := f.folder(t, "zq-mk.Ab12Cd", small) // mktemp's name is in its output only
	f.named(t, tmp, "s1", toolResult, 2*time.Minute, tmp+"\n", "")
	cwd := f.folder(t, "zq-cwd-only", small)
	f.named(t, cwd, "s1", toolCall, time.Minute, `{"command":"make"}`, cwd)
	tilde := f.folder(t, "zq-tilde", small)
	f.named(t, tilde, "s1", toolCall, 0, `{"command":"mkdir -p ~/cache/zq-tilde/out"}`, "")
	f.folder(t, "zq-task-2", small) // named by nothing but its prefix
	f.folder(t, "zq-empty", nil)
	f.named(t, filepath.Join(f.root, "zq-empty"), "s1", toolCall, 0, `{"command":"mkdir `+filepath.Join(f.root, "zq-empty")+`"}`, "")
	gitEnv(t)
	repo := filepath.Join(f.base, "projects", "repo")
	write(t, filepath.Join(repo, "README"), "x\n")
	gitT(t, repo, "init", "-q")
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-qm", "init")
	lw := filepath.Join(f.root, "zq-lw") // a linked worktree at the top of the cache is the worktrees rule's
	gitT(t, repo, "worktree", "add", "-q", "-b", "lw", lw)
	f.named(t, lw, "s1", toolCall, 0, `{"command":"git worktree add `+lw+`"}`, "")
	f.task(t, "zq-holds", "s1", map[string]int{"models/m/w.gguf": 2 << 20}) // holds a models root
	f.task(t, "zq-lanes", "s1", small)                                      // is a worktrees root
	f.cfg.Models.Roots = []string{filepath.Join(f.root, "zq-hf"), filepath.Join(f.root, "zq-holds", "models")}
	f.cfg.Worktrees.Roots = []string{filepath.Join(f.root, "zq-lanes")}
	f.cfg = f.cfg.Normal()

	o := options(t, procRoot(t), f.now, nil)
	o.DryRun = true
	got := findings(f.run(t, o), RuleScratch)
	for _, p := range []string{task, tmp, cwd, tilde} {
		if got[p].Action != ActDelete {
			t.Errorf("%s is a task's idle folder, which goes: %q (%s)", p, got[p].Action, got[p].Why)
		}
	}
	for _, n := range []string{"zq-tagged", "zq-mytool", "zq-back", "zq-unmade", "zq-late", "zq-early", "zq-hf", "zq-tgt",
		"zq-task-2", "zq-empty", "zq-lw", "zq-holds", "zq-lanes"} {
		if g, ok := got[filepath.Join(f.root, n)]; ok {
			t.Errorf("%s is no task's folder (a tool's cache, another rule's, made by no agent, or empty): %+v", n, g)
		}
	}
	if d := got[task].Detail; !strings.Contains(d, "made by claude session s1") {
		t.Errorf("a task folder names the session that made it: %q", d)
	}
}

func TestAnIdleTaskFoldersModelDataGoesAndTheRestIsOffloaded(t *testing.T) {
	f := newScratch(t)
	run := f.task(t, "zq-run", "s1", map[string]int{"ckpt/model.safetensors": 2 << 20, "results.jsonl": 2 << 20,
		"notes.md": -1, "small.safetensors": 512 << 10, ".venv/pyvenv.cfg": -1, ".venv/share/pkg/w.safetensors": 2 << 20,
		"env/lib/site-packages/pkg/w.safetensors": 2 << 20, "arrays.npy": 2 << 20})
	gguf(t, filepath.Join(run, "ollama/blobs/sha256-aa"), 2<<20)
	torchZip(t, filepath.Join(run, "pytorch_model.bin"), 2<<20)
	mid := f.task(t, "zq-mid-models", "s1", map[string]int{"w.gguf": 2 << 20, "log.txt": -1})
	plain := f.task(t, "zq-mid-plain", "s1", map[string]int{"log.txt": -1, "out.json": 2 << 20})
	fresh := f.task(t, "zq-fresh", "s1", map[string]int{"log.txt": -1})
	f.ago(t, mid, 25*24*time.Hour) // 25 days of watching: past the offload's 21, short of the models rule's 30
	f.ago(t, plain, 25*24*time.Hour)
	f.ago(t, fresh, 15*24*time.Hour)
	f.cfg.Live = []string{RuleScratch}
	res := f.run(t, options(t, procRoot(t), f.now, nil))
	got := findings(res, RuleScratch)
	if g := got[fresh]; g.Action != ActPending || !g.Eligible.Equal(f.now.Add(6*24*time.Hour)) || !exists(fresh) {
		t.Errorf("a folder written 15 days ago waits 6 more: %+v", g)
	}

	if !got[run].Done || exists(run) || !exists(run+pointerSuffix) {
		t.Fatalf("an idle task folder is offloaded and its pointer left in its place: %+v", got[run])
	}
	logged := f.offloadLog(t)
	for _, rel := range []string{"results.jsonl", "notes.md", "small.safetensors", ".venv/share/pkg/w.safetensors",
		"env/lib/site-packages/pkg/w.safetensors", "arrays.npy"} {
		if !strings.Contains(logged, "scratch-evidence zq-run "+rel+"\n") {
			t.Errorf("%s is no model data the task kept, so restic keeps it:\n%s", rel, logged)
		}
	}
	for _, rel := range []string{"ckpt/model.safetensors", "ollama/blobs/sha256-aa", "pytorch_model.bin"} {
		if strings.Contains(logged, rel) {
			t.Errorf("model data never goes into restic: %s was offloaded", rel)
		}
	}
	if got[mid].Action != ActPending || !exists(mid) {
		t.Errorf("a folder with model data waits for the models rule's 30 days: %+v", got[mid])
	}
	if !got[plain].Done || exists(plain) {
		t.Errorf("a folder without model data goes after 21 days: %+v", got[plain])
	}
	evs, err := f.st.Events(context.Background(), store.Query{Source: store.Capacity, Kinds: []string{KindOffload}})
	must(t, err)
	var meta map[string]any
	for _, e := range evs {
		if e.Ref == run {
			must(t, json.Unmarshal([]byte(e.Meta), &meta))
		}
	}
	if meta["snapshot"] != "0123abcd" || meta["model_files"] != float64(3) {
		t.Errorf("the offload's event keeps its snapshot and the model data it deleted: %v", meta)
	}
	ns, err := f.st.Notes(context.Background())
	must(t, err)
	found := false
	for _, n := range ns {
		found = found || (n.Kind == store.NoteDecided && strings.HasPrefix(n.Title, "Offloaded task folder") &&
			strings.HasPrefix(n.Detail, "Restic snapshot 0123abcd;"))
	}
	if !found {
		t.Errorf("an offload is a decided note that names its snapshot: %+v", ns)
	}
}

func TestATaskFolderStaysWhileSomethingHoldsIt(t *testing.T) {
	gitEnv(t)
	f := newScratch(t)
	small := map[string]int{"out.log": 2 << 20}
	open := f.task(t, "zq-open", "s1", small)
	named := f.task(t, "zq-named", "s1", small)
	kept := f.task(t, "zq-kept", "s1", small)
	worked := f.task(t, "zq-worked", "s1", small)
	maker := f.task(t, "zq-maker", "live-1", small)
	// a repository elsewhere with a worktree in a task folder, one in a task folder with a worktree elsewhere, and
	// one elsewhere whose remote is a path in a task folder
	projects := filepath.Join(f.base, "projects")
	repo := filepath.Join(projects, "repo")
	write(t, filepath.Join(repo, "README"), "x\n")
	gitT(t, repo, "init", "-q")
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-qm", "init")
	wt := f.task(t, "zq-wt", "s1", small)
	gitT(t, repo, "worktree", "add", "-q", "-b", "feat", filepath.Join(wt, "inner"))
	inner := f.task(t, "zq-repo-wt", "s1", small)
	gitT(t, repo, "clone", "-q", repo, filepath.Join(inner, "clone"))
	gitT(t, filepath.Join(inner, "clone"), "worktree", "add", "-q", "-b", "side", filepath.Join(projects, "side-wt"))
	elsewhere := filepath.Join(f.base, "elsewhere", "repo2") // under no root the shared walk takes
	write(t, filepath.Join(elsewhere, "README"), "x\n")
	gitT(t, elsewhere, "init", "-q")
	gitT(t, elsewhere, "add", ".")
	gitT(t, elsewhere, "commit", "-qm", "init")
	wt2 := f.task(t, "zq-wt2", "s1", small)
	gitT(t, elsewhere, "worktree", "add", "-q", "-b", "w2", filepath.Join(wt2, "inner"))
	remote := f.task(t, "zq-remote", "s1", small)
	gitT(t, repo, "init", "-q", "--bare", filepath.Join(remote, "mirror.git"))
	gitT(t, repo, "remote", "add", "mirror", filepath.Join(remote, "mirror.git"))
	free := f.task(t, "zq-free", "s1", small)
	f.cfg.Scratch.Keep = []string{kept}
	f.cfg.Targets.Roots = []string{projects} // the shared walk finds the repositories elsewhere
	f.cfg = f.cfg.Normal()

	proc := procRoot(t, fakeProc{pid: 10, ppid: 1, argv: []string{"python"}, fds: []string{filepath.Join(open, "out.log")}})
	jobs := fakeJobs{units: map[string]string{"user:x.service": "[Service]\nExecStart=/bin/run " + filepath.Join(named, "out.log") + "\n"}}
	o := options(t, proc, f.now, jobs)
	o.DryRun = true
	o.Sessions = func(context.Context) map[int]Session {
		return map[int]Session{-1: {Key: "live-1", Name: "lane", Tool: "claude"}, -2: {Key: "w", Name: "worker", Tool: "codex", Cwd: worked}}
	}
	got := findings(f.run(t, o), RuleScratch)
	for p, why := range map[string]string{open: "in use now", named: "named by", kept: "in keep", worked: "works in it",
		maker: "the session that made it", wt: "a repository references it", wt2: "a repository references it",
		inner:  "a repository references it",
		remote: "a repository references it"} {
		if g := got[p]; g.Action != ActKeep || !strings.Contains(g.Why, why) {
			t.Errorf("%s stays (%s): %q (%s)", filepath.Base(p), why, g.Action, g.Why)
		}
	}
	if got[free].Action != ActDelete {
		t.Errorf("a folder nothing holds goes: %+v", got[free])
	}
}

func TestAnExcludedProjectsTaskFolderKeepsAllButItsModelData(t *testing.T) {
	f := newScratch(t)
	gitEnv(t)
	run := f.task(t, "zq-secret-run", "s1", map[string]int{"w.safetensors": 2 << 20, "notes.md": -1})
	plain := f.task(t, "zq-secret-plain", "s1", map[string]int{"out.log": 2 << 20})
	clone := f.task(t, "zq-clone", "s1", map[string]int{"repo/README": -1}) // holds a checkout of an excluded remote
	gitT(t, filepath.Join(clone, "repo"), "init", "-q")
	gitT(t, filepath.Join(clone, "repo"), "remote", "add", "origin", "git@github.com:owner/hidden.git")
	lane := f.folder(t, "zq-lane", map[string]int{"build.log": 2 << 20}) // made by a session at work in an excluded project
	f.named(t, lane, "s2", toolCall, 0, `{"command":"mkdir -p `+lane+`"}`, filepath.Join(f.base, "projects", "hush", "wt"))
	f.cfg.Exclude.Names = []string{"*secret*"}
	f.cfg.Exclude.Paths = []string{filepath.Join(f.base, "projects", "hush")}
	f.cfg.Exclude.Remotes = []string{"owner/hidden"}
	f.cfg = f.cfg.Normal()
	f.cfg.Live = []string{RuleScratch}
	res := f.run(t, options(t, procRoot(t), f.now, nil))
	got := findings(res, RuleScratch)
	if g := got[clone]; g.Action != ActReport || g.class != "excluded" || !exists(clone) {
		t.Errorf("a folder holding an excluded project's checkout is only reported: %+v", g)
	}
	if g := got[lane]; g.Action != ActReport || g.class != "excluded" || !exists(lane) {
		t.Errorf("a folder made by a session at work in an excluded project is only reported: %+v", g)
	}
	if !got[run].Done || exists(filepath.Join(run, "w.safetensors")) || !exists(filepath.Join(run, "notes.md")) || exists(run+pointerSuffix) {
		t.Errorf("an excluded project's model data goes (the owner ruled on models directly) and the rest stays: %+v", got[run])
	}
	if got[plain].Action != ActReport || got[plain].class != "excluded" || !exists(plain) {
		t.Errorf("an excluded project's folder is only reported: %+v", got[plain])
	}
	if f.offloadLog(t) != "" {
		t.Errorf("nothing of an excluded project is offloaded:\n%s", f.offloadLog(t))
	}
	if n, ok, err := f.st.Note(context.Background(), "capacity:scratch:excluded"); err != nil || !ok || !n.Done.IsZero() {
		t.Errorf("the reported folder is a problem note: %+v %v %v", n, ok, err)
	}
}

func TestATaskFolderIsJudgedAgainBeforeItGoes(t *testing.T) {
	f := newScratch(t)
	files := map[string]int{"w.safetensors": 2 << 20, "notes.md": -1}
	written := f.task(t, "zq-written", "s1", files)
	used := f.task(t, "zq-used", "s1", files)
	locked := f.task(t, "zq-locked", "s1", files)
	failing := f.task(t, "zq-will-fail", "s1", files)
	skipped := f.task(t, "zq-noop", "s1", map[string]int{"notes.md": -1})
	joined := f.task(t, "zq-joined", "s1", files)
	f.cfg.Live = []string{RuleScratch}
	proc := procRoot(t)
	o := options(t, proc, f.now, nil)
	acting := false
	o.Sessions = func(context.Context) map[int]Session {
		if !acting {
			return nil
		}
		return map[int]Session{-1: {Key: "late", Name: "late", Tool: "codex", Cwd: joined}}
	}
	o.beforeAct = func() {
		acting = true // a session starts working in one after the judgment
		write(t, filepath.Join(written, "new.log"), "a write after the judgment\n")
		dir := filepath.Join(proc, "11")
		must(t, os.MkdirAll(filepath.Join(dir, "fd"), 0o755))
		write(t, filepath.Join(dir, "cmdline"), "python\x00")
		write(t, filepath.Join(dir, "stat"), "11 (python) S 1 0 0 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 100 0 0\n")
		must(t, os.Symlink(filepath.Join(used, "notes.md"), filepath.Join(dir, "fd", "3")))
	}
	lk, err := os.Open(locked)
	must(t, err)
	defer lk.Close()
	must(t, syscall.Flock(int(lk.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)) // another holder of a lock on it
	got := findings(f.run(t, o), RuleScratch)
	for p, why := range map[string]string{written: "written since it was judged", used: "in use now", locked: "lock",
		joined: "works in it"} {
		g := got[p]
		if g.Action != ActKeep || !strings.Contains(g.Why, why) || !exists(filepath.Join(p, "w.safetensors")) {
			t.Errorf("%s stays whole (%s): %q (%s)", filepath.Base(p), why, g.Action, g.Why)
		}
	}
	g := got[failing]
	if g.Err == "" || g.Done || !exists(failing) || exists(filepath.Join(failing, "w.safetensors")) || !strings.Contains(g.Detail, "deleted 2 MiB of its model data") {
		t.Errorf("a failed offload leaves the folder, its model data gone, and says so: %+v", g)
	}
	if g := got[skipped]; g.Err == "" || g.Done || !exists(skipped) || !strings.Contains(g.Err, "did not leave") {
		t.Errorf("an offload that ran but left the folder is a failure: %+v", g)
	}
	if n, ok, err := f.st.Note(context.Background(), "capacity:scratch:failed"); err != nil || !ok {
		t.Errorf("a failed offload is a problem note: %+v %v %v", n, ok, err)
	}
}

func TestAScratchRootIsWatchedFromItsFirstRun(t *testing.T) {
	f := newScratch(t)
	p := f.task(t, "zq-task", "s1", map[string]int{"out.log": 2 << 20})
	ctx := context.Background()
	must(t, f.st.ClearMark(ctx, "watch:"+f.root)) // the sampler ran for 35 days, but not with this root
	f.cfg.Live = []string{RuleScratch}
	if g := findings(f.run(t, options(t, procRoot(t), f.now, nil)), RuleScratch)[p]; g.Action != ActPending ||
		!g.Eligible.Equal(f.now.Add(21*24*time.Hour)) {
		t.Fatalf("a folder 40 days without a write waits 21 days from when the sampler began to watch its root: %+v", g)
	}
	if g := findings(f.run(t, options(t, procRoot(t), f.now.Add(24*time.Hour), nil)), RuleScratch)[p]; g.Idle.UnixMilli() != f.now.UnixMilli() {
		t.Errorf("the next run keeps the root's watch where it began: %+v", g)
	}
}

func TestALaterRunTakesAnEarlierRunsOffloadAsItsOwn(t *testing.T) {
	_, wts := worktreesFixture(t)
	// a scratch root inside a work tree of the repository: an offload there changes that work tree
	scratch := filepath.Join(wts, "local", "scratch")
	write(t, filepath.Join(wts, "local", ".gitignore"), "scratch/\n")
	f := &scratchFixture{base: filepath.Dir(wts), root: scratch, offload: filepath.Join(filepath.Dir(wts), "evidence-offload"),
		now: time.Now().Add(40 * 24 * time.Hour), st: openStore(t)}
	write(t, f.offload, fakeOffload)
	must(t, os.Chmod(f.offload, 0o755))
	p := f.task(t, "zq-task", "s1", map[string]int{"out.log": 2 << 20})
	f.cfg = Config{Scratch: ScratchConfig{Roots: []string{scratch}, Offload: f.offload}, Worktrees: WorktreesConfig{Roots: []string{wts}}}.Normal()
	f.cfg.Live = []string{RuleScratch, RuleWorktrees}
	watchedSince(t, f.st, f.cfg, f.now.Add(-35*24*time.Hour), f.now)
	tick := f.now
	clock := func() time.Time { tick = tick.Add(10 * time.Second); return tick }
	o := options(t, procRoot(t), tick, nil)
	o.Clock, o.Rules = clock, []string{RuleScratch}
	o.afterDelete = func(fd Finding) { // the offload changes the root when it happens, after the run began
		at := clock()
		must(t, os.Chtimes(filepath.Dir(fd.Path), at, at))
	}
	if g := findings(f.run(t, o), RuleScratch)[p]; !g.Done {
		t.Fatalf("the first run offloads the folder: %+v", g)
	}
	o2 := options(t, procRoot(t), tick, nil)
	o2.Clock, o2.Rules = clock, []string{RuleWorktrees}
	if g := findings(f.run(t, o2), RuleWorktrees)[filepath.Join(wts, "merged")]; !g.Done {
		t.Errorf("a run minutes later takes the first run's offload as capacity's own: %+v", g)
	}
}

func TestADryRunOrASpentRunOffloadsNothing(t *testing.T) {
	f := newScratch(t)
	p := f.task(t, "zq-task", "s1", map[string]int{"w.safetensors": 2 << 20, "notes.md": -1})
	f.cfg.Live = []string{RuleScratch}
	o := options(t, procRoot(t), f.now, nil)
	o.DryRun = true
	if g := findings(f.run(t, o), RuleScratch)[p]; g.Action != ActDelete || g.Done || !exists(filepath.Join(p, "w.safetensors")) {
		t.Errorf("a dry run lists what it would do and does nothing: %+v", g)
	}
	saved := scratchBudget
	scratchBudget = 0
	defer func() { scratchBudget = saved }()
	g := findings(f.run(t, options(t, procRoot(t), f.now, nil)), RuleScratch)[p]
	if g.Done || !strings.Contains(g.Why, "waits for the next run") || !exists(filepath.Join(p, "w.safetensors")) {
		t.Errorf("a run whose time for offloads is spent leaves the rest to the next: %+v", g)
	}
	if f.offloadLog(t) != "" {
		t.Errorf("nothing was offloaded:\n%s", f.offloadLog(t))
	}
}

func TestAUseInATaskFolderIsASighting(t *testing.T) {
	f := newScratch(t)
	p := f.task(t, "zq-task", "s1", map[string]int{"out.log": 2 << 20})
	hf := f.folder(t, "zq-hf", map[string]int{"models--o--m/blobs/b": 2 << 20})
	f.cfg.Models.Roots = []string{hf}
	f.cfg = f.cfg.Normal()
	// a run 10 days ago saw a shell in the folder, and one in the models root's own directory
	o := options(t, procRoot(t, fakeProc{pid: 12, ppid: 1, argv: []string{"bash"}, cwd: filepath.Join(p, "sub")},
		fakeProc{pid: 13, ppid: 1, argv: []string{"bash"}, cwd: hf}), f.now.Add(-10*24*time.Hour), nil)
	o.Rules = []string{RuleScratch}
	f.run(t, o)
	seen, err := f.st.SeenPaths(context.Background())
	must(t, err)
	if _, ok := seen[p]; !ok {
		t.Fatalf("a use in a task folder is a sighting of the folder: %v", seen)
	}
	if _, ok := seen[hf]; ok {
		t.Errorf("a use of a models root is no sighting of a task folder, which would count for every model in it")
	}
	o = options(t, procRoot(t), f.now, nil)
	o.DryRun = true
	if g := findings(f.run(t, o), RuleScratch)[p]; g.Action != ActPending || !strings.Contains(g.Why, "seen in use") {
		t.Errorf("a folder seen in use 10 days ago waits: %+v", g)
	}
}

func TestModelDataIsKnownByItsNameOrItsFirstBytes(t *testing.T) {
	dir := t.TempDir()
	st := func(name string, head []byte, n int) (string, int64) {
		b := make([]byte, n)
		copy(b, head)
		p := filepath.Join(dir, name)
		must(t, os.WriteFile(p, b, 0o644))
		return p, int64(n)
	}
	hdr := make([]byte, 9)
	binary.LittleEndian.PutUint64(hdr, 100)
	hdr[8] = '{'
	zip := func(entry string) []byte {
		b := make([]byte, 30+len(entry))
		copy(b, "PK\x03\x04")
		binary.LittleEndian.PutUint16(b[26:28], uint16(len(entry)))
		copy(b[30:], entry)
		return b
	}
	for _, c := range []struct {
		name string
		head []byte
		want bool
	}{
		{"w.pt", nil, true},
		{"x.Safetensors", nil, true},
		{"sha256-1", []byte("GGUF\x03"), true},
		{"blob", hdr, true},
		{"pytorch_model.bin", zip("archive/data.pkl"), true},
		{"bundle.zip", zip("README.md"), false},
		{"table", []byte("PAR1"), true},
		{"results.jsonl", []byte(`{"a":1}`), false},
		{"arrays.npy", []byte("\x93NUMPY"), false},
		{"lib.so", []byte("\x7fELF"), false},
		{"hdr-too-long", func() []byte { h := bytes.Clone(hdr); binary.LittleEndian.PutUint64(h, 1<<40); return h }(), false},
	} {
		p, n := st(c.name, c.head, 4096)
		if got := modelFormat(p, c.name, n); got != c.want {
			t.Errorf("%s: model data %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAFolderIsNamedOnlyAsAWholeName(t *testing.T) {
	for _, c := range []struct {
		text string
		want bool
	}{
		{`{"command":"cd /c/app && make"}`, true},
		{`{"command":"ls /c/app"}`, true},
		{`{"command":"cat \"/c/app/x\""}`, true},
		{`{"command":"ls /c/app-2"}`, false},
		{`{"command":"ls /c/app.old /c/apps"}`, false},
		{`{"command":"ls /c/app-2 /c/app"}`, true},
	} {
		if got := namesPath(c.text, []string{"/c/app"}); got != c.want {
			t.Errorf("%s names /c/app: %v, want %v", c.text, got, c.want)
		}
	}
}

func TestAnExcludedNameOnTheLinkToAScratchRootExcludes(t *testing.T) {
	f := newScratch(t)
	link := filepath.Join(f.base, "lanes-cache") // the root as configured is a link to the cache
	must(t, os.Symlink(f.root, link))
	p := f.task(t, "zq-task", "s1", map[string]int{"out.log": 2 << 20})
	f.cfg.Scratch.Roots = []string{link}
	f.cfg.Exclude.Names = []string{"lanes-*"}
	f.cfg = f.cfg.Normal()
	o := options(t, procRoot(t), f.now, nil)
	o.DryRun = true
	got := findings(f.run(t, o), RuleScratch)
	g := got[filepath.Join(link, "zq-task")]
	if g.Action != ActReport || g.class != "excluded" {
		t.Errorf("a name only the root's link carries excludes its folders: %+v (%v)", g, got)
	}
	_ = p
}

func TestAFolderThatCannotBeReadWholeIsOnlyReported(t *testing.T) {
	f := newScratch(t)
	p := f.task(t, "zq-task", "s1", map[string]int{"out.log": 2 << 20, "locked/x": -1})
	must(t, os.Chmod(filepath.Join(p, "locked"), 0))
	t.Cleanup(func() { os.Chmod(filepath.Join(p, "locked"), 0o755) })
	if _, err := os.ReadDir(filepath.Join(p, "locked")); err == nil {
		t.Skip("the test runs as a user that reads any directory")
	}
	o := options(t, procRoot(t), f.now, nil)
	o.DryRun = true
	if g := findings(f.run(t, o), RuleScratch)[p]; g.Action != ActReport || g.class != "unreadable" {
		t.Errorf("a folder whose last write is not known is only reported: %+v", g)
	}
}

func TestWithoutTheOffloadNothingOfAFolderGoes(t *testing.T) {
	f := newScratch(t)
	p := f.task(t, "zq-task", "s1", map[string]int{"w.safetensors": 2 << 20, "notes.md": -1})
	f.cfg.Scratch.Offload = filepath.Join(f.base, "no-such-tool")
	f.cfg.Live = []string{RuleScratch}
	g := findings(f.run(t, options(t, procRoot(t), f.now, nil)), RuleScratch)[p]
	if g.Done || !strings.Contains(g.Err, "not installed") || !exists(filepath.Join(p, "w.safetensors")) {
		t.Errorf("a folder whose offload cannot run keeps its model data too: %+v", g)
	}
}
