package capacity

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
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
	f.event(t, p, store.Event{Session: session, Kind: kind, Tool: "Bash", Summary: summary, Cwd: cwd}, off)
}

// event puts a transcript event in the store, off after p's birth: an agent's in a claude session but where e says.
func (f *scratchFixture) event(t *testing.T, p string, e store.Event, off time.Duration) {
	t.Helper()
	born, ok := birth(p)
	if !ok {
		t.Fatalf("%s has no birth time", p)
	}
	e.Source, e.TS = store.Transcripts, born.Add(off)
	e.SourceKey = p + "@" + e.Kind + off.String() + e.Summary
	if e.Harness == "" {
		e.Harness = "claude"
	}
	if e.Actor == "" {
		e.Actor = store.ActorAgent
	}
	_, err := f.st.Put(context.Background(), []store.Event{e})
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
	tmp := f.folder(t, "zq-mk.Ab12Cd", small) // mktemp's name is in the output of its call only
	f.event(t, tmp, store.Event{Session: "s1", Kind: toolCall, Tool: "Bash", Meta: `{"tool_id":"t1"}`,
		Summary: `{"command":"mktemp -d ` + filepath.Join(f.root, "zq-mk.XXXXXX") + `"}`}, -time.Second)
	f.event(t, tmp, store.Event{Session: "s1", Kind: toolResult, Tool: "Bash", Meta: `{"tool_id":"t1"}`, Actor: store.ActorTool,
		Summary: tmp + "\n"}, time.Second)
	cwd := f.folder(t, "zq-cwd-only", small) // a call that works in it made nothing
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
	for _, p := range []string{task, tmp, tilde} {
		if got[p].Action != ActDelete {
			t.Errorf("%s is a task's idle folder, which goes: %q (%s)", p, got[p].Action, got[p].Why)
		}
	}
	if g := got[filepath.Join(f.root, "zq-back")]; g.Action != ActReport || g.class != "pointer" || !strings.Contains(g.Why, "which left it") {
		t.Errorf("a task's folder beside the pointer of its offload is only reported: %+v", g)
	}
	_ = cwd
	for _, n := range []string{"zq-tagged", "zq-mytool", "zq-unmade", "zq-late", "zq-early", "zq-hf", "zq-tgt",
		"zq-task-2", "zq-empty", "zq-lw", "zq-holds", "zq-lanes", "zq-cwd-only"} {
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
	dels, err := f.st.Events(context.Background(), store.Query{Source: store.Capacity, Kinds: []string{KindDelete}})
	must(t, err)
	var del map[string]any
	for _, e := range dels {
		if e.Ref == run {
			must(t, json.Unmarshal([]byte(e.Meta), &del))
		}
	}
	if del == nil || del["bytes"] != del["model_deleted"] || del["bytes"].(float64)+meta["bytes"].(float64) != float64(got[run].Bytes) {
		t.Errorf("the model data's deletion is an event of its own, and the offload's size is the rest: %v %v", del, meta)
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
	f.cfg.Scratch.Keep = []string{filepath.Join(kept, "out.log")} // a kept path in it keeps the folder
	f.cfg.Targets.Roots = []string{projects}                      // the shared walk finds the repositories elsewhere
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

func TestAFolderIsATasksOnlyWhenAnAgentsCallCreatesIt(t *testing.T) {
	f := newScratch(t)
	small := map[string]int{"out.log": 2 << 20}
	cmd := func(c string) func(string) store.Event {
		return func(p string) store.Event {
			return store.Event{Summary: `{"command":"` + strings.ReplaceAll(c, "P", p) + `"}`}
		}
	}
	want := map[string]bool{}
	for _, c := range []struct {
		name string
		e    func(p string) store.Event
		off  time.Duration
		task bool
	}{
		{"mkdir", cmd("mkdir -p P/a"), 0, true},
		{"sudo", cmd("sudo mkdir -p P"), 0, true},
		{"nice", cmd("nice -n 10 mkdir -p P"), 0, true},
		{"clone", cmd("git clone -q https://x/y P"), 0, true},
		{"clone-then", cmd("git clone -q https://x/y P && cd P"), 0, true},
		{"env-mkdir", cmd("FOO=1 mkdir -p P"), 0, true},
		{"newline", cmd(`cd /x\nmkdir -p P`), 0, true},
		{"clone-source", cmd("git clone P /elsewhere/x"), 0, false},
		{"init", cmd("git init -q P"), 0, true},
		{"worktree", cmd("git -C /repo worktree add -b x P"), 0, true},
		{"git-cd", cmd("git -C P status"), 0, false},
		{"copy-dest", cmd("cp -r /src P"), 0, true},
		{"copy-source", cmd("cp -r P /elsewhere"), 0, false},
		{"rsync", cmd("rsync -a host:dir/ P/"), 0, true},
		{"tar", cmd("tar -C P -xf a.tar"), 0, true},
		{"curl", cmd("curl -sL -o P/f https://x"), 0, true},
		{"option", cmd("python train.py --output_dir P --epochs 3"), 0, true},
		{"option-eq", cmd("tool --cache-dir=P"), 0, true},
		{"variable", cmd("HF_HOME=P python dl.py"), 0, true},
		{"script", cmd(`python3 -c 'import os; os.makedirs(\"P/a\")'`), 0, true},
		{"quoted", cmd(`cat \"P/out.log\"`), 0, false},
		{"rm-script", cmd(`rm -rf \"P\" && python3 -c 'import os; os.makedirs(\"/elsewhere\")'`), 0, false},
		{"exec-cmd", func(p string) store.Event {
			return store.Event{Tool: "exec", Summary: `text(await tools.exec_command({cmd:"mkdir -p ` + p + `"}))`}
		}, 0, true},
		{"write", func(p string) store.Event {
			return store.Event{Tool: "Write", Summary: `{"file_path":"` + p + `/notes.md","content":"x"}`}
		}, 0, true},
		{"patch", func(p string) store.Event {
			return store.Event{Tool: "exec", Summary: `text(await tools.apply_patch("*** Begin Patch\n*** Add File: ` + p + `/task.md\n+x"))`}
		}, 0, true},
		{"list", cmd("ls -la P"), 0, false},
		{"size", cmd("du -sh P"), 0, false},
		{"after-mkdir", cmd("mkdir -p /elsewhere && du -sh P"), 0, false},
		{"cd", cmd("cd P && make"), 0, false},
		{"rm", cmd("rm -rf P && mkdir -p /other"), 0, false},
		{"owner", func(p string) store.Event {
			return store.Event{Actor: store.ActorOwner, Summary: `{"command":"mkdir -p ` + p + `"}`}
		}, 0, false},
		{"cut", func(p string) store.Event { return store.Event{Summary: `{"command":"mkdir -p ` + p + `…`} }, 0, false},
		// a call is recorded as it is issued: a command makes its output minutes later; a later call adds to it
		{"before", cmd("mkdir -p P"), -9 * time.Minute, true},
		{"too-early", cmd("mkdir -p P"), -11 * time.Minute, false},
		{"after", cmd("mkdir -p P/sub"), 9 * time.Minute, true},
		{"too-late", cmd("mkdir -p P/sub"), 11 * time.Minute, false},
	} {
		p := f.folder(t, "zq-"+c.name, small)
		e := c.e(p)
		e.Session, e.Kind = "s1", toolCall
		if e.Tool == "" {
			e.Tool = "Bash"
		}
		f.event(t, p, e, c.off)
		want[c.name] = c.task
	}
	writer := f.folder(t, "zq-writer", small) // a writer that says it made directories
	f.event(t, writer, store.Event{Session: "s1", Kind: toolCall, Tool: "write_file", Harness: "hermes", Meta: `{"tool_id":"w1"}`,
		Summary: `{"path": "notes/x.md"}`}, 0)
	f.event(t, writer, store.Event{Session: "s1", Kind: toolResult, Tool: "write_file", Harness: "hermes", Meta: `{"tool_id":"w1"}`,
		Actor: store.ActorTool, Summary: `{"bytes_written": 10, "dirs_created": true, "resolved_path": "` + writer + `/notes/x.md"}`}, time.Second)
	want["writer"] = true
	listed := f.folder(t, "zq-listed", small) // a listing's output names it
	f.event(t, listed, store.Event{Session: "s1", Kind: toolCall, Tool: "Bash", Meta: `{"tool_id":"l1"}`,
		Summary: `{"command":"du -sh ` + f.root + `/* | sort -h"}`}, time.Minute)
	f.event(t, listed, store.Event{Session: "s1", Kind: toolResult, Tool: "Bash", Meta: `{"tool_id":"l1"}`, Actor: store.ActorTool,
		Summary: "2.0M\t" + listed + "\n"}, time.Minute+time.Second)
	want["listed"] = false
	owned := f.folder(t, "zq-ownmk.Xy12", small) // what the owner's own mktemp printed
	f.event(t, owned, store.Event{Session: "s1", Kind: toolCall, Tool: "Bash", Meta: `{"tool_id":"o1"}`, Actor: store.ActorOwner,
		Summary: `{"command":"mktemp -d ` + filepath.Join(f.root, "zq-ownmk.XXXX") + `"}`}, -time.Second)
	f.event(t, owned, store.Event{Session: "s1", Kind: toolResult, Tool: "Bash", Meta: `{"tool_id":"o1"}`, Actor: store.ActorTool,
		Summary: owned + "\n"}, time.Second)
	want["ownmk.Xy12"] = false
	o := options(t, procRoot(t), f.now, nil)
	o.DryRun = true
	got := findings(f.run(t, o), RuleScratch)
	for n, w := range want {
		if _, task := got[filepath.Join(f.root, "zq-"+n)]; task != w {
			t.Errorf("%s: a task's folder %v, want %v", n, task, w)
		}
	}
}

func TestAToolsCacheAnAgentRemovedOrListedStays(t *testing.T) {
	f := newScratch(t)
	f.cfg.Live = []string{RuleScratch}
	pw := f.folder(t, "zq-browsers", map[string]int{"chromium-1/chrome": 2 << 20})
	f.named(t, pw, "s1", toolCall, -5*time.Second, `{"command":"rm -rf `+pw+` && npx playwright install chromium"}`, "")
	wheels := f.folder(t, "zq-wheels", map[string]int{"http-v2/a/b": 2 << 20})
	f.event(t, wheels, store.Event{Session: "s2", Kind: toolCall, Tool: "Bash", Meta: `{"tool_id":"d1"}`,
		Summary: `{"command":"du -sh ` + f.root + `/* | sort -h"}`}, 4*time.Minute)
	f.event(t, wheels, store.Event{Session: "s2", Kind: toolResult, Tool: "Bash", Meta: `{"tool_id":"d1"}`, Actor: store.ActorTool,
		Summary: "2.0M\t" + wheels + "\n"}, 4*time.Minute+time.Second)
	got := findings(f.run(t, options(t, procRoot(t), f.now, nil)), RuleScratch)
	for _, p := range []string{pw, wheels} {
		if g, ok := got[p]; ok || !exists(p) {
			t.Errorf("a tool's cache an agent removed or listed is no task's folder: %s %+v", filepath.Base(p), g)
		}
	}
}

// slowOffload is the stand-in offload, but while it offloads zq-big a process starts in zq-small (in the
// synthetic /proc) and a mark says a session started there.
const slowOffload = `#!/bin/sh
set -eu
for d in "$@"; do
  case "$d" in *zq-big*)
    mkdir -p "$FX_PROC/77/fd"
    printf 'python\0' > "$FX_PROC/77/cmdline"
    echo python > "$FX_PROC/77/comm"
    echo "77 (python) S 1 0 0 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 100 0 0" > "$FX_PROC/77/stat"
    ln -s "$FX_SMALL/out.log" "$FX_PROC/77/fd/3"
    : > "$FX_MARK";;
  esac
  printf 'snapshot  0123abcd\n' > "$d.restic.txt"
  rm -rf "$d"
  echo "$d -> $d.restic.txt (snapshot 0123abcd)"
done
`

func TestEachOffloadIsJudgedByALookOfItsOwn(t *testing.T) {
	for _, session := range []bool{false, true} {
		f := newScratch(t)
		f.cfg.Live = []string{RuleScratch}
		big := f.task(t, "zq-big", "s1", map[string]int{"out.log": 8 << 20})
		small := f.task(t, "zq-small", "s1", map[string]int{"out.log": 2 << 20})
		write(t, f.offload, slowOffload)
		must(t, os.Chmod(f.offload, 0o755))
		proc, mark := procRoot(t), filepath.Join(f.base, "session-started")
		t.Setenv("FX_PROC", proc)
		t.Setenv("FX_SMALL", small)
		t.Setenv("FX_MARK", mark)
		o := options(t, proc, f.now, nil)
		if session { // the process the first offload starts is not read; a session is
			o = options(t, procRoot(t), f.now, nil)
			o.Sessions = func(context.Context) map[int]Session {
				if exists(mark) {
					return map[int]Session{-1: {Key: "late", Name: "late", Tool: "claude", Cwd: small}}
				}
				return nil
			}
		}
		got := findings(f.run(t, o), RuleScratch)
		if !got[big].Done {
			t.Fatalf("the bigger folder goes first: %+v", got[big])
		}
		if g := got[small]; g.Done || !exists(small) {
			t.Errorf("a folder a process or session took up while another was offloaded stays (session %v): %+v", session, g)
		}
	}
}

func TestModelDataDeletedBeforeAFailedOffloadIsADeletionOnRecord(t *testing.T) {
	f := newScratch(t)
	f.cfg.Live = []string{RuleScratch}
	p := f.task(t, "zq-will-fail", "s1", map[string]int{"w.safetensors": 2 << 20, "notes.md": -1})
	f.run(t, options(t, procRoot(t), f.now, nil))
	if exists(filepath.Join(p, "w.safetensors")) || !exists(filepath.Join(p, "notes.md")) {
		t.Fatal("the model data goes before the offload, which fails and leaves the rest")
	}
	ctx := context.Background()
	evs, err := f.st.Events(ctx, store.Query{Source: store.Capacity, Kinds: []string{KindDelete}})
	must(t, err)
	deleted := false
	for _, e := range evs {
		deleted = deleted || e.Ref == p
	}
	ns, err := f.st.Notes(ctx)
	must(t, err)
	decided := false
	for _, n := range ns {
		decided = decided || (n.Kind == store.NoteDecided && strings.HasPrefix(n.Title, "Deleted the model data of task folder"))
	}
	if !deleted || !decided {
		t.Errorf("the model data went: a capacity-delete event (%v) and a decided note (%v)", deleted, decided)
	}
}

func TestAnOffloadThatFailedButLeftItsPointerWent(t *testing.T) {
	f := newScratch(t)
	f.cfg.Live = []string{RuleScratch}
	write(t, f.offload, "#!/bin/sh\nfor d in \"$@\"; do printf 'snapshot  89abcdef\\n' > \"$d.restic.txt\"; rm -rf \"$d\"; done\nexit 1\n")
	must(t, os.Chmod(f.offload, 0o755))
	p := f.task(t, "zq-task", "s1", map[string]int{"out.log": 2 << 20})
	g := findings(f.run(t, options(t, procRoot(t), f.now, nil)), RuleScratch)[p]
	if !g.Done || !g.offloaded || g.snapshot != "89abcdef" || g.Err != "" {
		t.Errorf("a folder gone with its pointer in its place is offloaded, its snapshot read from the pointer: %+v", g)
	}
}

func TestAToolsTimeoutEndsItsProcessGroup(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tool")
	write(t, p, "#!/bin/sh\nsleep 4\necho done\n")
	must(t, os.Chmod(p, 0o755))
	start := time.Now()
	_, err := runTool(context.Background(), 500*time.Millisecond, p, "tool")
	if took := time.Since(start); took > 2*time.Second || !errors.Is(err, errTimeout) {
		t.Errorf("a 0.5 s timeout ends the tool and its children: it returned after %s (%v)", took.Round(100*time.Millisecond), err)
	}
}

func TestAToolsWaitEndsThoughAChildLeftItsGroup(t *testing.T) {
	defer func(d time.Duration) { toolWaitDelay = d }(toolWaitDelay)
	toolWaitDelay = 200 * time.Millisecond
	p := filepath.Join(t.TempDir(), "tool")
	write(t, p, "#!/bin/sh\nsetsid sleep 6\necho done\n") // a child in a session of its own holds the output
	must(t, os.Chmod(p, 0o755))
	start := time.Now()
	_, err := runTool(context.Background(), 300*time.Millisecond, p, "tool")
	if took := time.Since(start); took > 3*time.Second || !errors.Is(err, errTimeout) {
		t.Errorf("a tool's wait ends a moment after its timeout: it returned after %s (%v)", took.Round(100*time.Millisecond), err)
	}
}

func TestAnOffloadGoesOnlyToTheDirectoryJudged(t *testing.T) {
	f := newScratch(t)
	p := f.folder(t, "zq-task", map[string]int{"out.log": 2 << 20})
	ino, err := inodeOf(p)
	must(t, err)
	r := &runner{cfg: f.cfg}
	g := &Finding{Rule: RuleScratch, Path: p, ino: inode{ino.dev, ino.ino + 1}} // another directory was judged
	if err := r.removeScratch(context.Background(), g, nil); !errors.Is(err, errChanged) || !exists(filepath.Join(p, "out.log")) {
		t.Errorf("a folder that is not the directory judged stays: %v", err)
	}
	if f.offloadLog(t) != "" {
		t.Errorf("no offload ran:\n%s", f.offloadLog(t))
	}
}

func TestALinkAtTheTopOfTheCacheIsNoFolder(t *testing.T) {
	f := newScratch(t)
	f.cfg.Live = []string{RuleScratch}
	write(t, f.offload, "#!/bin/sh\nset -eu\nfor d in \"$@\"; do\n  d=$(readlink -f \"$d\")\n"+
		"  printf 'snapshot  0123abcd\\n' > \"$d.restic.txt\"\n  rm -rf \"$d\"\ndone\n")
	must(t, os.Chmod(f.offload, 0o755))
	proj := filepath.Join(f.base, "projects", "work")
	blob(t, filepath.Join(proj, "src.go"), 2<<20)
	link := filepath.Join(f.root, "zq-link")
	must(t, os.Symlink(proj, link))
	f.named(t, link, "s1", toolCall, 0, `{"command":"ln -s `+proj+` `+link+` && mkdir -p `+link+`/out"}`, "")
	if _, ok := findings(f.run(t, options(t, procRoot(t), f.now, nil)), RuleScratch)[link]; ok {
		t.Errorf("a link at the top of the cache is no folder")
	}
	if !exists(filepath.Join(proj, "src.go")) {
		t.Errorf("a link at the top of the cache led the offload to %s", proj)
	}
}

func TestATaggedHubInATaskFolderIsModelData(t *testing.T) {
	f := newScratch(t)
	f.cfg.Live = []string{RuleScratch}
	p := f.task(t, "zq-hfhome", "s1", map[string]int{"notes.md": -1})
	write(t, filepath.Join(p, "hub", "CACHEDIR.TAG"), cargoTag+"\n# huggingface_hub\n")
	gguf(t, filepath.Join(p, "hub", "models--o--m", "blobs", "abc"), 2<<20)
	cargoTarget(t, filepath.Join(p, "build-target")) // a Cargo target holds no model a task kept
	blob(t, filepath.Join(p, "build-target", "debug", "fixture.safetensors"), 2<<20)
	g := findings(f.run(t, options(t, procRoot(t), f.now, nil)), RuleScratch)[p]
	logged := f.offloadLog(t)
	if !g.Done || strings.Contains(logged, "blobs/abc") || !strings.Contains(logged, "build-target/debug/fixture.safetensors") {
		t.Errorf("a hub's blob is model data and a Cargo target's file is not: %+v\n%s", g, logged)
	}
}

func TestTheSessionThatMadeAFolderIsKnownByItsHarnesssID(t *testing.T) {
	f := newScratch(t)
	p := f.folder(t, "zq-task", map[string]int{"out.log": 2 << 20})
	f.named(t, p, "s0", toolCall, -time.Minute, `{"command":"mkdir -p `+p+`"}`, "") // another session made it first
	f.event(t, p, store.Event{Session: "20261030_1", Harness: "hermes", Kind: toolCall, Tool: "terminal",
		Summary: `{"command": "mkdir -p ` + p + `/out"}`}, 0)
	o := options(t, procRoot(t), f.now, nil)
	o.DryRun = true
	o.Sessions = func(context.Context) map[int]Session {
		return map[int]Session{-1: {Key: "hermes:20261030_1", ID: "20261030_1", Name: "gateway", Tool: "hermes"}}
	}
	if g := findings(f.run(t, o), RuleScratch)[p]; g.Action != ActKeep || !strings.Contains(g.Why, "the session that made it") {
		t.Errorf("a live session keyed apart from its transcripts' id keeps the folder it made: %+v", g)
	}
}

func TestMoreThatHoldsATaskFolder(t *testing.T) {
	gitEnv(t)
	f := newScratch(t)
	small := map[string]int{"out.log": 2 << 20}
	projects := filepath.Join(f.base, "projects")
	// a repository elsewhere borrowing objects from a task folder, and a bare one whose remote is a path in another
	alt := f.task(t, "zq-alt", "s1", small)
	repo := filepath.Join(projects, "repo")
	write(t, filepath.Join(repo, "README"), "x\n")
	gitT(t, repo, "init", "-q")
	write(t, filepath.Join(repo, ".git", "objects", "info", "alternates"), filepath.Join(alt, "objects")+"\n")
	mirrored := f.task(t, "zq-mirrored", "s1", small)
	hub := filepath.Join(projects, "hub.git")
	gitT(t, projects, "init", "-q", "--bare", hub)
	gitT(t, hub, "remote", "add", "up", filepath.Join(mirrored, "up.git"))
	// a repository in a folder that names only itself
	self := f.task(t, "zq-self", "s1", map[string]int{"r/README": -1})
	gitT(t, filepath.Join(self, "r"), "init", "-q")
	gitT(t, filepath.Join(self, "r"), "remote", "add", "own", filepath.Join(self, "r2.git"))
	// a folder in a kept place, one with a model file the models rule keeps, and the maker's checkout of an excluded remote
	f.cfg.Scratch.Keep = []string{f.root + "-never"}
	keptModel := f.task(t, "zq-kept-model", "s1", map[string]int{"w.safetensors": 2 << 20})
	f.cfg.Models.Keep = []string{filepath.Join(keptModel, "w.safetensors")}
	checkout := filepath.Join(projects, "secret-lane")
	write(t, filepath.Join(checkout, "README"), "x\n")
	gitT(t, checkout, "init", "-q")
	gitT(t, checkout, "remote", "add", "origin", "git@github.com:owner/hidden.git")
	lane := f.folder(t, "zq-lane", small)
	f.named(t, lane, "s1", toolCall, -time.Minute, `{"command":"mkdir -p `+lane+`"}`, filepath.Join(f.base, "work"))
	f.named(t, lane, "s2", toolCall, 0, `{"command":"mkdir -p `+lane+`/out"}`, checkout) // a lane of the excluded project too
	f.cfg.Exclude.Remotes = []string{"owner/hidden"}
	f.cfg.Targets.Roots = []string{projects, f.root} // the shared walk finds the repositories, those in the cache too
	f.cfg = f.cfg.Normal()
	o := options(t, procRoot(t), f.now, nil)
	o.DryRun = true
	got := findings(f.run(t, o), RuleScratch)
	for p, why := range map[string]string{alt: "references it", mirrored: "references it", keptModel: "models rule's keep"} {
		if g := got[p]; g.Action != ActKeep || !strings.Contains(g.Why, why) {
			t.Errorf("%s stays (%s): %q (%s)", filepath.Base(p), why, g.Action, g.Why)
		}
	}
	if g := got[self]; g.Action != ActDelete {
		t.Errorf("a folder whose repository names only itself goes: %+v", g)
	}
	if g := got[lane]; g.Action != ActReport || g.class != "excluded" {
		t.Errorf("a folder made in a checkout of an excluded remote is only reported: %+v", g)
	}
	f.cfg.Scratch.Keep = []string{f.root} // a kept place above the folders keeps them all
	f.cfg = f.cfg.Normal()
	if g := findings(f.run(t, o), RuleScratch)[self]; g.Action != ActKeep {
		t.Errorf("a folder inside a kept place stays: %+v", g)
	}
}

func TestAContainerThatMountsWhatHoldsTheCacheUsesItsFolders(t *testing.T) {
	f := newScratch(t)
	p := f.task(t, "zq-task", "s1", map[string]int{"out.log": 2 << 20})
	box := fakeTools{answers: map[string]string{"uv": "ERR", "docker ps": "c1\n", "docker inspect": "/box\t" + f.base + "\n",
		"docker info": "ERR"}}
	o := options(t, procRoot(t), f.now, nil)
	o.DryRun, o.Tools = true, box
	if g := findings(f.run(t, o), RuleScratch)[p]; g.Action != ActKeep || !strings.Contains(g.Why, "container box") {
		t.Errorf("a container mounting a directory that holds the cache uses its folders: %+v", g)
	}
	o = options(t, procRoot(t), f.now.Add(-10*24*time.Hour), nil) // a run 10 days ago saw the container
	o.Tools, o.Rules = box, []string{RuleScratch}
	f.run(t, o)
	o = options(t, procRoot(t), f.now, nil)
	o.DryRun = true
	if g := findings(f.run(t, o), RuleScratch)[p]; g.Action != ActPending || !strings.Contains(g.Why, "seen in use") {
		t.Errorf("a container's mount of what holds the cache is a sighting of each folder: %+v", g)
	}
}

func TestAModelFileSwappedAfterTheJudgmentKeepsTheFolderWhole(t *testing.T) {
	f := newScratch(t)
	f.cfg.Live = []string{RuleScratch}
	p := f.task(t, "zq-task", "s1", map[string]int{"a.safetensors": 2 << 20, "b.safetensors": 2 << 20, "notes.md": -1})
	o := options(t, procRoot(t), f.now, nil)
	o.beforeModelDelete = func(Finding) { // another file in b's place, made while b is there so its inode is another
		blob(t, filepath.Join(p, "b.new"), 2<<20)
		must(t, os.Rename(filepath.Join(p, "b.new"), filepath.Join(p, "b.safetensors")))
	}
	g := findings(f.run(t, o), RuleScratch)[p]
	if g.Done || !exists(filepath.Join(p, "a.safetensors")) || !strings.Contains(g.Why, "changed since it was judged") {
		t.Errorf("model data another file took a place in since the judgment goes not at all: %+v", g)
	}
}

func TestAFolderBesideItsPointerIsOnlyReported(t *testing.T) {
	f := newScratch(t)
	write(t, filepath.Join(f.root, "zq-again"+pointerSuffix), "snapshot 1\n")
	must(t, os.Chtimes(filepath.Join(f.root, "zq-again"+pointerSuffix), time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)))
	again := f.task(t, "zq-again", "s1", map[string]int{"out.log": 2 << 20}) // made again after its offload
	o := options(t, procRoot(t), f.now, nil)
	o.DryRun = true
	if g := findings(f.run(t, o), RuleScratch)[again]; g.Action != ActReport || !strings.Contains(g.Why, "made again") {
		t.Errorf("a folder made again beside its pointer is reported, not offloaded over it: %+v", g)
	}
}

func TestARepositoryThatNamesItsOwnFolderThroughAnotherMountNamesOnlyItself(t *testing.T) {
	gitEnv(t)
	f := newScratch(t)
	data := filepath.Join(f.base, "data") // the cache is a bind mount of the data filesystem's /cache
	must(t, os.MkdirAll(data, 0o755))
	must(t, os.Symlink(f.root, filepath.Join(data, "cache"))) // so the canonical names lead to the files, as on a machine
	canon := func(p string) string { return filepath.Join(data, "cache", strings.TrimPrefix(p, f.root+"/")) }
	// a repository whose remote is a path in the folder, written by its canonical name
	remote := f.task(t, "zq-remote", "s1", map[string]int{"r/README": -1})
	gitT(t, filepath.Join(remote, "r"), "init", "-q")
	gitT(t, filepath.Join(remote, "r"), "remote", "add", "own", canon(filepath.Join(remote, "r2.git")))
	// a repository and its worktree in one folder, git's links between them written by the canonical names
	wt := f.task(t, "zq-wt", "s1", map[string]int{"r/README": -1})
	r := filepath.Join(wt, "r")
	gitT(t, r, "init", "-q")
	gitT(t, r, "add", ".")
	gitT(t, r, "commit", "-qm", "init")
	gitT(t, r, "worktree", "add", "-q", "-b", "side", filepath.Join(wt, "side"))
	write(t, filepath.Join(r, ".git", "worktrees", "side", "gitdir"), canon(filepath.Join(wt, "side", ".git"))+"\n")
	write(t, filepath.Join(wt, "side", ".git"), "gitdir: "+canon(filepath.Join(r, ".git", "worktrees", "side"))+"\n")
	f.cfg.Targets.Roots = []string{f.root}
	f.cfg = f.cfg.Normal()
	o := options(t, procRoot(t), f.now, nil)
	o.DryRun = true
	write(t, o.MountInfo, "29 1 259:3 / / rw - ext4 /dev/sda2 rw\n40 29 252:0 / "+data+" rw - ext4 /dev/mapper/data rw\n"+
		"41 29 252:0 /cache "+f.root+" rw - ext4 /dev/mapper/data rw\n")
	must(t, f.st.SetMark(context.Background(), "watch:"+filepath.Join(data, "cache"), f.now.Add(-35*24*time.Hour)))
	got := findings(f.run(t, o), RuleScratch)
	for _, p := range []string{remote, wt} {
		if g := got[p]; g.Action != ActDelete {
			t.Errorf("%s: what git names in the folder by another mount's name is in the folder: %q (%s)", filepath.Base(p), g.Action, g.Why)
		}
	}
}

func TestTheModelDatasDeletionRecordsTheDirectoriesItChanged(t *testing.T) {
	f := newScratch(t)
	failed := f.task(t, "zq-will-fail", "s1", map[string]int{"ckpt/w.safetensors": 2 << 20, "notes.md": -1})
	only := f.task(t, "zq-secret-run", "s1", map[string]int{"ckpt/w.safetensors": 2 << 20, "notes.md": -1})
	f.cfg.Exclude.Names = []string{"*secret*"}
	f.cfg = f.cfg.Normal()
	f.cfg.Live = []string{RuleScratch}
	f.run(t, options(t, procRoot(t), f.now, nil))
	evs, err := f.st.Events(context.Background(), store.Query{Source: store.Capacity, Kinds: []string{KindDelete}})
	must(t, err)
	own := map[string][]string{}
	for _, e := range evs {
		var m struct {
			Own []ownDir `json:"own"`
		}
		must(t, json.Unmarshal([]byte(e.Meta), &m))
		for _, d := range m.Own {
			own[e.Ref] = append(own[e.Ref], d.Dir)
		}
	}
	for _, p := range []string{failed, only} {
		if exists(filepath.Join(p, "ckpt", "w.safetensors")) || !contains(own[p], filepath.Join(p, "ckpt")) {
			t.Errorf("%s: a deletion in a folder's own directory is capacity's change, on record: %v", filepath.Base(p), own[p])
		}
	}
}
