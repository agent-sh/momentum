package capacity

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

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

// threeTargets is a repository that ignores its build output, with three idle Cargo target dirs of 8, 5 and 3 MiB.
func threeTargets(t *testing.T) (Config, []string) {
	t.Helper()
	gitEnv(t)
	base := t.TempDir()
	t.Setenv("HOME", base) // the notes name ~/projects/app/target, whatever the test's directory is
	root := filepath.Join(base, "projects")
	app := filepath.Join(root, "app")
	write(t, filepath.Join(app, ".gitignore"), "target/\n")
	write(t, filepath.Join(app, "src", "main.rs"), "fn main() {}\n")
	gitT(t, app, "init", "-q")
	gitT(t, app, "add", ".")
	gitT(t, app, "commit", "-qm", "init")
	var dirs []string
	for i, rel := range []string{"target", "crates/a/target", "crates/b/target"} {
		d := filepath.Join(app, rel)
		cargoTarget(t, d)
		blob(t, filepath.Join(d, "debug", "deps", "libbig.rlib"), []int{6, 3, 1}[i]<<20)
		dirs = append(dirs, d)
	}
	cfg := Config{Targets: TargetsConfig{Roots: []string{root}}}.Normal()
	cfg.Live = []string{RuleTargets}
	return cfg, dirs
}

// decidedNotes are the store's decided notes from capacity.
func decidedNotes(t *testing.T, st *store.Store) []store.Note {
	t.Helper()
	ns, err := st.Notes(context.Background())
	must(t, err)
	var out []store.Note
	for _, n := range ns {
		if n.Kind == store.NoteDecided && n.Source == store.Capacity {
			out = append(out, n)
		}
	}
	return out
}

func TestARunsDeletionsAreOneDecidedNoteRewrittenAsEachGoes(t *testing.T) {
	ctx := context.Background()
	cfg, dirs := threeTargets(t)
	st := openStore(t)
	now := time.Now().Add(72 * time.Hour)
	o := options(t, procRoot(t), now, nil)
	var partial []string // the decided notes at each deletion, before it is recorded: what a run stopped there leaves
	o.afterDelete = func(Finding) {
		var titles []string
		for _, n := range decidedNotes(t, st) {
			titles = append(titles, n.Title)
		}
		partial = append(partial, strings.Join(titles, " | "))
	}
	res, err := Run(ctx, cfg, st, o)
	must(t, err)
	got := findings(res, RuleTargets)
	for _, d := range dirs {
		if !got[d].Done {
			t.Fatalf("%s goes: %+v", d, got[d])
		}
	}
	ns := decidedNotes(t, st)
	if len(ns) != 1 {
		t.Fatalf("three deletions in one run are one decided note, not one each: %d notes", len(ns))
	}
	n := ns[0]
	if n.ID != fmt.Sprintf("capacity:run:%d", now.UnixMilli()) {
		t.Errorf("the note is keyed by the run's start: %s", n.ID)
	}
	if !strings.HasPrefix(n.Title, "Freed ") || !strings.HasSuffix(n.Title, ": 3 Rust build outputs") {
		t.Errorf("the title is the run's effect, counted by rule: %q", n.Title)
	}
	if len(partial) != 3 || partial[0] != "" || !strings.HasPrefix(partial[1], "Freed ") || !strings.HasSuffix(partial[1], ": 1 Rust build output") ||
		strings.Contains(partial[1], " | ") || !strings.HasSuffix(partial[2], ": 2 Rust build outputs") || strings.Contains(partial[2], " | ") {
		t.Errorf("the run's one note is rewritten as each item goes, so a run stopped midway leaves what it did: %q", partial)
	}
	a, b, c := strings.Index(n.Detail, short(dirs[0])+" 8 MiB"), strings.Index(n.Detail, short(dirs[1])+" 5 MiB"),
		strings.Index(n.Detail, short(dirs[2])+" 3 MiB")
	if a < 0 || b < a || c < b {
		t.Errorf("the detail names the items with their sizes, the largest first: %q", n.Detail)
	}
	if strings.Count(n.Detail, "capacity.exclude.paths") != 1 {
		t.Errorf("the detail says once how to keep a project's build output: %q", n.Detail)
	}
	evs, err := st.Events(ctx, store.Query{Source: store.Capacity, Kinds: []string{KindDelete}})
	if err != nil || len(evs) != 3 {
		t.Errorf("each deletion keeps its own event: %d %v", len(evs), err)
	}
	o2 := options(t, procRoot(t), now.Add(time.Hour), nil)
	o2.Now = true
	_, err = Run(ctx, cfg, st, o2)
	must(t, err)
	if ns := decidedNotes(t, st); len(ns) != 1 {
		t.Errorf("a run that deletes nothing writes no note: %d notes", len(ns))
	}
}

func TestARunsNoteCountsByRuleAndSaysOnceHowToKeepEach(t *testing.T) {
	t.Setenv("HOME", "/h")
	const G = int64(1 << 30)
	items := []runItem{
		{rule: RuleTargets, path: "/h/p/a/target", size: 11 * G, freed: 11 * G},
		{rule: RuleWorktrees, path: "/h/.cache/wt", size: 9 << 20, freed: 9 << 20},
		{rule: RuleTargets, path: "/h/p/b/target", size: 3 * G, freed: 3 * G},
		{rule: RuleScratch, path: "/h/.cache/x", size: 6 * G, freed: 6 * G, model: true}, // x's model data, then x
		{rule: RuleScratch, path: "/h/.cache/x", size: G, freed: G, offload: true},
		{rule: RuleScratch, path: "/h/.cache/y", size: 2 * G, freed: 2 * G, offload: true},
		{rule: RuleUV, path: "/h/.cache/uv", size: 2 * G, freed: 2 * G},
	}
	check := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s\n got %q\nwant %q", what, got, want)
		}
	}
	check("the title counts by rule, a folder with its model data once", runTitle(items),
		"Freed 25.0 GiB: 2 Rust build outputs, 1 leftover worktree, 2 task folders (3.0 GiB to restic); pruned the uv cache")
	// every sentence would pass the limit: the prune's sentence is left out
	check("the detail", runDetail(items), "Largest: ~/p/a/target 11.0 GiB, model data in ~/.cache/x 6.0 GiB, ~/p/b/target 3.0 GiB. "+
		"Each offloaded folder's NAME.restic.txt beside it says how to restore it. "+modelDataGone+" To keep one next time, "+
		"list a build output's project in capacity.exclude.paths, lock a worktree (git worktree lock) or list a task folder in "+
		"capacity.scratch.keep.")
	// a folder whose model data alone went (an excluded project's, or one whose offload failed) is not offloaded
	check("folders offloaded apart from those whose model data alone went", runTitle([]runItem{
		{rule: RuleScratch, path: "/h/.cache/y", size: G, freed: G, offload: true},
		{rule: RuleScratch, path: "/h/.cache/e", size: 4 * G, freed: 4 * G, model: true},
		{rule: RuleScratch, path: "/h/.cache/f", size: 5 * G, freed: 5 * G, model: true},
	}), "Freed 10.0 GiB: 1 task folder (1.0 GiB to restic), model data of 2 task folders")
	// a title too long for a line names the prunes by their tools, then names the items shorter, then counts the
	// prunes, then ends at the last whole clause that fits. What Docker says it freed is Docker's figure, not in what
	// the run freed.
	docker := append(items[:len(items):len(items)], runItem{rule: RuleDocker, path: "docker build cache", size: G, freed: G, by: "Docker"})
	check("the prunes by their tools", runTitle(docker),
		"Freed 25.0 GiB: 2 Rust build outputs, 1 leftover worktree, 2 task folders (3.0 GiB to restic); pruned uv and Docker")
	check("a tool's figure, named as the tool's", runTitle(docker[3:]),
		"Freed 11.0 GiB: 2 task folders (3.0 GiB to restic); pruned the uv cache and Docker (1.0 GiB by Docker's count)")
	unsaid := append(items[:len(items):len(items)], runItem{rule: RuleDocker, path: "docker build cache", unsaid: true, by: "Docker"},
		runItem{rule: RuleDocker, path: "docker dangling images", size: G, freed: G, by: "Docker"})
	check("a prune with no total, and a figure of its tool's, are not in what the run freed", runTitle(unsaid),
		"Freed 25.0 GiB: 2 Rust build outputs, 1 leftover worktree, 2 task folders (3.0 GiB to restic); pruned uv and Docker")
	check("what Docker printed of two prunes, one of which printed nothing", runTitle(unsaid[len(unsaid)-2:]),
		"Pruned Docker (at least 1.0 GiB by Docker's count)")
	var models []runItem
	for i := range 10 {
		models = append(models, runItem{rule: RuleModels, path: "/h/m/" + strconv.Itoa(i)})
	}
	check("the items shorter", runTitle(append(models, unsaid...)),
		"Freed 25.0 GiB: 10 model items, 2 build outputs, 1 worktree, 2 folders (3.0 GiB to restic); pruned uv and Docker")
	var counted []runItem
	for i := range 12 {
		counted = append(counted, runItem{rule: RuleTargets, path: "/h/p/" + strconv.Itoa(i) + "/target", size: G, freed: G})
	}
	counted = append(counted, runItem{rule: RuleWorktrees, path: "/h/w/a"}, runItem{rule: RuleWorktrees, path: "/h/w/b"},
		runItem{rule: RuleScratch, path: "/h/s/a", offload: true, size: G}, runItem{rule: RuleScratch, path: "/h/s/c", model: true,
			size: 10 * G, freed: 10 * G})
	check("the prunes counted", runTitle(append(counted, unsaid[len(unsaid)-3:]...)),
		"Freed 24.0 GiB: 12 build outputs, 2 worktrees, 1 folder (1.0 GiB to restic), model data of 1 folder; 3 prunes")
	if d := runDetail(unsaid); strings.Contains(d, "docker build cache") || !strings.HasPrefix(d, "Largest: ~/p/a/target 11.0 GiB, ") {
		t.Errorf("a prune with no total names no size: %q", d)
	}
	every := func(prunes ...runItem) []runItem {
		var out []runItem
		for i := range 12 {
			out = append(out, runItem{rule: RuleTargets, path: "/h/p/" + strconv.Itoa(i) + "/target", size: G, freed: G})
		}
		for _, p := range []string{"/h/m/a", "/h/m/b", "/h/m/c"} {
			out = append(out, runItem{rule: RuleModels, path: p, size: G, freed: G})
		}
		for _, p := range []string{"/h/w/a", "/h/w/b", "/h/w/c", "/h/w/d"} {
			out = append(out, runItem{rule: RuleWorktrees, path: p})
		}
		out = append(out, runItem{rule: RuleScratch, path: "/h/s/a", offload: true, size: G},
			runItem{rule: RuleScratch, path: "/h/s/b", offload: true, size: G},
			runItem{rule: RuleScratch, path: "/h/s/c", model: true, size: 10 * G, freed: 10 * G})
		return append(out, prunes...)
	}
	check("the items shorter", runTitle(every()),
		"Freed 25.0 GiB: 3 model items, 12 build outputs, 4 worktrees, 2 folders (2.0 GiB to restic), model data of 1 folder")
	check("the last whole clauses that fit", runTitle(every(runItem{rule: RuleUV, path: "/h/.cache/uv", freed: G},
		runItem{rule: RuleDocker, path: "docker build cache", unsaid: true, by: "Docker"},
		runItem{rule: RuleDocker, path: "docker dangling images", by: "Docker"})),
		"Freed 26.0 GiB: 3 model items, 12 build outputs, 4 worktrees, 2 folders (2.0 GiB to restic) and more")
	check("a run whose one prune printed no total", runTitle(unsaid[len(unsaid)-2:len(unsaid)-1]),
		"Pruned Docker (Docker printed no total)")
	wt := []runItem{{rule: RuleWorktrees, path: "/h/.cache/wt", size: 300 << 10, freed: 300 << 10}}
	check("an item under 1 MiB", runTitle(wt), "Freed under 1 MiB: 1 leftover worktree")
	check("a removed worktree comes back", runDetail(wt), "~/.cache/wt under 1 MiB. To keep one next time, lock a worktree "+
		"(git worktree lock). A removed worktree's commits are merged or on its remote: git worktree add brings it back.")
	// one offload names its pointer
	check("one offload", runDetail([]runItem{{rule: RuleScratch, path: "/h/.cache/y", size: 2 * G, freed: 2 * G, offload: true}}),
		"~/.cache/y 2.0 GiB to restic. ~/.cache/y.restic.txt says how to restore it. To keep one next time, list a task folder "+
			"in capacity.scratch.keep.")
	// runs of four rules with a home's ordinary paths: three items, or two and how to keep one in fewer words
	t.Setenv("HOME", "/home/user")
	runs := []runItem{
		{rule: RuleModels, path: "/data/models/some-llm-8b", size: 16 * G, freed: 16 * G},
		{rule: RuleTargets, path: "/home/user/projects/billing-service/target", size: 11 * G, freed: 11 * G},
		{rule: RuleTargets, path: "/home/user/.cache/lane-review-482/target", size: 3 * G, freed: 3 * G},
		{rule: RuleWorktrees, path: "/home/user/.cache/pr-1290-review", size: 300 << 20, freed: 300 << 20},
		{rule: RuleScratch, path: "/home/user/.cache/train-run-7", size: G, freed: G, offload: true},
		{rule: RuleDocker, path: "docker build cache", size: 2 * G, freed: 2 * G, by: "Docker"},
	}
	check("four rules and a prune name the prune by its tool", runTitle(runs),
		"Freed 31.3 GiB: 1 model item, 2 build outputs, 1 worktree, 1 folder (1.0 GiB to restic); pruned Docker")
	d := runDetail(runs)
	if !strings.HasPrefix(d, "Largest: /data/models/some-llm-8b 16.0 GiB, ~/projects/billing-service/target 11.0 GiB, "+
		"~/.cache/lane-review-482/target 3.0 GiB. ") || utf8.RuneCountInString(d) > detailMax ||
		!strings.Contains(d, "list model data in capacity.models.keep, ") || strings.Contains(d, "Docker builds") {
		t.Errorf("three items, how to keep one, within %d characters: %d %q", detailMax, utf8.RuneCountInString(d), d)
	}
	hub := []runItem{
		{rule: RuleModels, path: "/home/user/.cache/huggingface/hub/models--org--llm-8b", size: 16 * G, freed: 16 * G},
		{rule: RuleTargets, path: "/home/user/projects/billing-service/target", size: 11 * G, freed: 11 * G},
		{rule: RuleWorktrees, path: "/home/user/.cache/pr-1290-review", size: 300 << 20, freed: 300 << 20},
		{rule: RuleScratch, path: "/home/user/.cache/train-run-7", size: 6 * G, freed: 6 * G, model: true},
		{rule: RuleScratch, path: "/home/user/.cache/train-run-7", size: G, freed: G, offload: true},
		{rule: RuleScratch, path: "/home/user/.cache/train-run-8", size: G, freed: G, offload: true},
	}
	brief := "To keep one next time: capacity.models.keep, capacity.exclude.paths (the project), git worktree lock, capacity.scratch.keep."
	srv := append([]runItem{{rule: RuleModels, path: "/srv/models/llm-8b", size: 16 * G, freed: 16 * G}}, hub[1:]...)
	if d := runDetail(srv); !strings.HasPrefix(d, "Largest: /srv/models/llm-8b 16.0 GiB, ~/projects/billing-service/target 11.0 GiB, "+
		"model data in ~/.cache/train-run-7 6.0 GiB. ") || !strings.HasSuffix(d, " "+brief) || utf8.RuneCountInString(d) > detailMax {
		t.Errorf("three items again once how to keep one is in fewer words, within %d characters: %d %q", detailMax,
			utf8.RuneCountInString(d), d)
	}
	if d := runDetail(hub); !strings.HasPrefix(d, "Largest: ~/.cache/huggingface/hub/models--org--llm-8b 16.0 GiB, "+
		"~/projects/billing-service/target 11.0 GiB. ") || !strings.HasSuffix(d, " "+modelDataGone+" "+brief) ||
		utf8.RuneCountInString(d) > detailMax {
		t.Errorf("two items, the model data's sentence, how to keep one in fewer words, within %d characters: %d %q", detailMax,
			utf8.RuneCountInString(d), d)
	}
	// names too long for any of that: one item, then no pointer, then the item clipped; how to keep one stays
	for _, n := range []int{14, 30} {
		var long []runItem
		for i := range 4 {
			p := "/home/user/.cache/" + strings.Repeat("a-long-task-name-", n) + strconv.Itoa(i)
			long = append(long, runItem{rule: RuleScratch, path: p, size: int64(4-i) * G, freed: int64(4-i) * G, offload: true})
		}
		if d := runDetail(long); utf8.RuneCountInString(d) > detailMax || !strings.HasPrefix(d, "Largest: ") ||
			!strings.HasSuffix(d, " To keep one next time: capacity.scratch.keep.") {
			t.Errorf("%d: how to keep one stays, within %d characters: %d %q", n, detailMax, utf8.RuneCountInString(d), d)
		}
	}
}

func TestADetailShortOfRoomLeavesItsLastSentencesOutTheLastFirst(t *testing.T) {
	t.Setenv("HOME", "/home/user")
	const G = int64(1 << 30)
	items := []runItem{
		{rule: RuleWorktrees, path: "/home/user/.cache/pr-review", size: 4 * G, freed: G, went: 4 * G},
		{rule: RuleTargets, path: "/home/user/projects/billing-service/target", size: 11 * G, freed: 11 * G, went: 11 * G},
		{rule: RuleUV, path: "/home/user/.cache/uv", size: 12 * G, freed: 3 * G, went: 12 * G},
	}
	d := runDetail(items)
	if utf8.RuneCountInString(d) > detailMax || !strings.Contains(d, "Files still hardlinked elsewhere stay on disk.") ||
		!strings.Contains(d, comesBack[RuleWorktrees]) ||
		strings.Contains(d, comesBack[RuleUV]) {
		t.Errorf("the uv sentence, last, goes first; why less was freed than went stays: %d %q", utf8.RuneCountInString(d), d)
	}
	if !strings.HasPrefix(d, "Pruned ~/.cache/uv 12.0 GiB (3.0 GiB freed), ~/projects/billing-service/target 11.0 GiB, "+
		"~/.cache/pr-review 4.0 GiB (1.0 GiB freed). ") {
		t.Errorf("each item says what it freed where that is another size: %q", d)
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
	o.Sessions = func(context.Context) (map[int]Session, error) {
		return map[int]Session{20: {Key: "s1", Name: "copy job", Tool: "claude"}}, nil
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
	if !strings.Contains(n.Detail, "dd (pid 41) wrote 4.0 GiB in 10m0s, 90 MiB/s at a look in "+short(dir)) {
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
	// a process that started in the very tick the look before read the clock is after it
	got = between(snap(1000, "b1"), snap(31000, "b1", store.ProcWrite{PID: 5, Start: 1000, Written: 7}))
	if got[5] != (wrote{7, 300}) {
		t.Errorf("a start in the tick of the look before counts as since: %+v", got)
	}
	// without boot ids, an uptime that went down is another boot
	got = between(snap(5000, "", store.ProcWrite{PID: 1, Start: 10, Written: 500}), snap(100, "", store.ProcWrite{PID: 1, Start: 10, Written: 600}))
	if got[1] != (wrote{600, 0.9}) {
		t.Errorf("an uptime that went down is another boot: %+v", got)
	}
	const g = 1 << 30
	// a shell ran uv, which ran python; python wrote 10 GiB more and ended, then uv: the shell took in both whole
	prev = snap(1000, "b1", store.ProcWrite{PID: 30, Start: 1, Written: 1 * g}, store.ProcWrite{PID: 31, PPID: 30, Start: 2, Written: 100 << 20},
		store.ProcWrite{PID: 32, PPID: 31, Start: 3, Written: 50 * g})
	got = between(prev, snap(31000, "b1", store.ProcWrite{PID: 30, Start: 1, Written: 1*g + 2*g + 150<<20 + 60*g}))
	if got[30].written != 2*g+50<<20+10*g {
		t.Errorf("the shell wrote its own 2 GiB, uv's last 50 MiB and python's last 10 GiB: %d", got[30].written)
	}
	// a parent's pid used again by another process since: the ended child's record is not taken from it
	prev = snap(1000, "b1", store.ProcWrite{PID: 10, Start: 1, Written: 5 * g}, store.ProcWrite{PID: 11, PPID: 10, Start: 2, Written: 50 * g})
	got = between(prev, snap(31000, "b1", store.ProcWrite{PID: 10, Start: 2000, Written: 3 * g}))
	if got[10].written != 3*g {
		t.Errorf("another process on the parent's pid counts all it wrote: %+v", got[10])
	}
	// the child's pid used again while the parent lives: the child ended, and the new one counts all it wrote
	got = between(prev, snap(31000, "b1", store.ProcWrite{PID: 10, Start: 1, Written: 5*g + 60*g},
		store.ProcWrite{PID: 11, PPID: 10, Start: 2000, Written: 1 * g}))
	if got[10].written != 10*g || got[11].written != 1*g {
		t.Errorf("the parent wrote the ended child's last 10 GiB, and the new process its 1 GiB: %+v", got)
	}
}

func TestAZombieIsNotYetEnded(t *testing.T) {
	const g = 1 << 30
	root := t.TempDir()
	// pid 31 has ended but its parent has not waited for it: /proc still has its stat, and no io this user can read
	write(t, filepath.Join(root, "31", "stat"), "31 (torchrun) Z 30 0 0 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 200 0 0\n")
	// pid 33 is another process now, one this user cannot read: its start is not the record's
	write(t, filepath.Join(root, "33", "stat"), "33 (sudo) S 1 0 0 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 999 0 0\n")
	snap := func(up int64, ws ...store.ProcWrite) writesSnap {
		s := writesSnap{snap: store.Snapshot{At: time.Unix(1, 0), Uptime: up, Boot: "b1"}, ws: map[int]store.ProcWrite{}}
		for _, w := range ws {
			s.ws[w.PID] = w
		}
		return s
	}
	prev := snap(1000, store.ProcWrite{PID: 30, Start: 100, Written: 1 * g}, store.ProcWrite{PID: 31, PPID: 30, Start: 200, Written: 50 * g},
		store.ProcWrite{PID: 33, PPID: 1, Start: 300, Written: 2 * g})
	cur := snap(31000, store.ProcWrite{PID: 30, Start: 100, Written: 1 * g})
	carry(root, prev, &cur)
	if w, ok := cur.ws[31]; !ok || w.Written != 50*g || w.PPID != 30 {
		t.Fatalf("the zombie stays on record as it was: %+v", cur.ws)
	}
	if _, ok := cur.ws[33]; ok {
		t.Errorf("a pid another process holds now is not carried: %+v", cur.ws)
	}
	if got := between(prev, cur); got[30].written != 0 {
		t.Errorf("nothing is taken from a parent that has not waited yet: %+v", got)
	}
	// then the parent waits: it takes in all 60 GiB, of which the record held 50
	next := snap(61000, store.ProcWrite{PID: 30, Start: 100, Written: 61 * g})
	if got := between(cur, next); got[30].written != 10*g {
		t.Errorf("the parent wrote the child's last 10 GiB: %+v", got)
	}
}

func TestAFillAfterARebootCountsAllEachProcessWrote(t *testing.T) {
	st, dir, now, proc := fillFixture(t)
	x := fakeProc{pid: 41, ppid: 1, argv: []string{"x"}, cwd: dir, written: 2 << 30}
	fillRun(t, st, dir, now.Add(-5*time.Minute), proc(1201, x), quiet)
	x.written = 3 << 30
	p2 := proc(5000, x) // a later uptime on another boot: pid 41 and its start are another process's
	write(t, filepath.Join(p2, "sys", "kernel", "random", "boot_id"), "b2\n")
	d := fillRun(t, st, dir, now, p2, quiet).Disks[0]
	if len(d.Writers) != 1 || d.Writers[0].Written != 3<<30 {
		t.Errorf("after a reboot every process counts all it wrote: %+v", d.Writers)
	}
}

// snapOf is a snapshot at an uptime on a boot, with these records.
func snapOf(up int64, boot string, ws ...store.ProcWrite) writesSnap {
	s := writesSnap{snap: store.Snapshot{At: time.Unix(1, 0), Uptime: up, Boot: boot}, ws: map[int]store.ProcWrite{}}
	for _, w := range ws {
		s.ws[w.PID] = w
	}
	return s
}

func TestAChildThatEndedUnderAZombieLauncherIsCountedOnce(t *testing.T) {
	const g = 1 << 30
	root := t.TempDir()
	// python ended under uv, uv waited for it and ended, and bash has not waited for uv yet
	write(t, filepath.Join(root, "31", "stat"), "31 (uv) Z 30 0 0 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 150 0 0\n")
	prev := snapOf(1000, "b1", store.ProcWrite{PID: 30, Start: 100, Written: 1 * g}, store.ProcWrite{PID: 31, PPID: 30, Start: 150, Written: 10 << 20},
		store.ProcWrite{PID: 32, PPID: 31, Start: 200, Written: 50 * g})
	cur := snapOf(31000, "b1", store.ProcWrite{PID: 30, Start: 100, Written: 1 * g})
	carry(root, prev, &cur)
	if w := cur.ws[31]; w.Written != 50*g+10<<20 {
		t.Errorf("uv's record keeps python's 50 GiB: %+v", w)
	}
	next := snapOf(61000, "b1", store.ProcWrite{PID: 30, Start: 100, Written: 1*g + 10<<20 + 60*g})
	if got := between(cur, next); got[30].written != 10*g {
		t.Errorf("bash wrote python's last 10 GiB: %+v", got[30])
	}
	// and when another process already holds python's pid, python has still ended
	cur = snapOf(31000, "b1", store.ProcWrite{PID: 30, Start: 100, Written: 1 * g}, store.ProcWrite{PID: 32, PPID: 1, Start: 30000, Written: 1 << 20})
	carry(root, prev, &cur)
	if w := cur.ws[31]; w.Written != 50*g+10<<20 {
		t.Errorf("uv's record keeps python's 50 GiB though its pid is another process's now: %+v", w)
	}
}

func TestTheWalkAndTheCarryKeepTheirGuards(t *testing.T) {
	const g = 1 << 30
	// the walk passes a pid another process holds now: python's record is taken from the shell, not the newcomer
	prev := snapOf(1000, "b1", store.ProcWrite{PID: 30, Start: 1, Written: 1 * g}, store.ProcWrite{PID: 31, PPID: 30, Start: 2, Written: 100 << 20},
		store.ProcWrite{PID: 32, PPID: 31, Start: 3, Written: 50 * g})
	got := between(prev, snapOf(31000, "b1", store.ProcWrite{PID: 30, Start: 1, Written: 1*g + 2*g + 150<<20 + 60*g},
		store.ProcWrite{PID: 31, PPID: 1, Start: 2000, Written: 1 * g}))
	if got[30].written != 2*g+50<<20+10*g || got[31].written != 1*g {
		t.Errorf("the shell wrote 12 GiB and 50 MiB, the newcomer 1 GiB: %+v", got)
	}
	root := t.TempDir()
	// a carried process keeps the parent its stat names now: one reparented since is no longer under its old parent
	write(t, filepath.Join(root, "31", "stat"), "31 (x) S 1 0 0 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 200 0 0\n")
	cur := snapOf(31000, "b1")
	carry(root, snapOf(1000, "b1", store.ProcWrite{PID: 30, Start: 100}, store.ProcWrite{PID: 31, PPID: 30, Start: 200, Written: 5 * g}), &cur)
	if w, ok := cur.ws[31]; !ok || w.PPID != 1 {
		t.Errorf("carried with the parent its stat names now: %+v", cur.ws)
	}
	// after a reboot, a pid and start on record are another process's: nothing is carried
	cur = snapOf(31000, "b2")
	carry(root, snapOf(1000, "b1", store.ProcWrite{PID: 31, PPID: 30, Start: 200, Written: 5 * g}), &cur)
	if _, ok := cur.ws[31]; ok {
		t.Errorf("nothing is carried across a boot: %+v", cur.ws)
	}
}

func TestAZombieOnARunStaysOnRecordUntilItsParentWaits(t *testing.T) {
	st, dir, now, proc := fillFixture(t)
	shell := fakeProc{pid: 30, ppid: 1, argv: []string{"bash"}, cwd: dir, written: 1 << 30}
	train := fakeProc{pid: 31, ppid: 30, argv: []string{"torchrun"}, cwd: dir, written: 50 << 30, start: 200}
	fillRun(t, st, dir, now.Add(-10*time.Minute), proc(1201, shell, train), quiet)
	// it ended with 60 GiB, and its parent has not waited yet: /proc has its stat only
	p2 := proc(1501, shell)
	write(t, filepath.Join(p2, "31", "stat"), "31 (torchrun) Z 30 0 0 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 200 0 0\n")
	fillRun(t, st, dir, now.Add(-5*time.Minute), p2, quiet)
	shell.written = 1<<30 + 60<<30 // then it waits, and takes in all 60 GiB
	d := fillRun(t, st, dir, now, proc(1801, shell), quiet).Disks[0]
	if len(d.Writers) != 1 || d.Writers[0].PID != 30 || d.Writers[0].Written != 10<<30 {
		t.Errorf("the shell wrote the child's last 10 GiB, not all 60: %+v", d.Writers)
	}
}

func TestALookWriterThatWroteNothingSinceSaysOnlyItsLook(t *testing.T) {
	ctx := context.Background()
	st, dir, now, proc := fillFixture(t)
	cp := fakeProc{pid: 42, ppid: 1, argv: []string{"cp"}, cwd: dir, written: 1 << 30}
	fillRun(t, st, dir, now.Add(-5*time.Minute), proc(1201, cp), quiet)
	n := 0
	look := func() map[int]int64 { n++; return map[int]int64{42: int64(n) * 50 << 20 / 1000} }
	fillRun(t, st, dir, now, proc(1501, cp), look)
	note, _, _ := st.Note(ctx, "capacity:fill:"+dir)
	if !strings.Contains(note.Detail, "cp (pid 42), 50 MiB/s at a look") || strings.Contains(note.Detail, "wrote 0") {
		t.Errorf("a look writer that wrote nothing since the run before says its look only: %s", note.Detail)
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
