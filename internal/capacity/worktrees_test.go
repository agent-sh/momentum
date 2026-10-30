package capacity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// worktreesFixture is a remote, a clone of it, and linked worktrees of the clone in every state.
func worktreesFixture(t *testing.T) (clone, wts string) {
	t.Helper()
	gitEnv(t)
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	clone, wts = filepath.Join(base, "clone"), filepath.Join(base, "cache")
	gitT(t, base, "init", "-q", "--bare", remote)
	gitT(t, base, "clone", "-q", remote, clone)
	write(t, filepath.Join(clone, "README"), "one\n")
	gitT(t, clone, "add", ".")
	gitT(t, clone, "commit", "-qm", "one")
	gitT(t, clone, "push", "-q", "origin", "main")
	gitT(t, clone, "remote", "set-head", "origin", "main")
	add := func(name, branch string) string {
		p := filepath.Join(wts, name)
		gitT(t, clone, "worktree", "add", "-q", "-b", branch, p, "origin/main")
		return p
	}
	commit := func(wt, file string) {
		write(t, filepath.Join(wt, file), file+"\n")
		gitT(t, wt, "add", ".")
		gitT(t, wt, "commit", "-qm", file)
	}
	// merged: pushed under its own name and fast-forwarded into main
	merged := add("merged", "feat/merged")
	commit(merged, "a")
	gitT(t, merged, "push", "-q", "-u", "origin", "feat/merged")
	gitT(t, merged, "push", "-q", "origin", "feat/merged:main")
	// gone: pushed, then its branch deleted on the remote, not merged
	gone := add("gone", "feat/gone")
	commit(gone, "b")
	gitT(t, gone, "push", "-q", "-u", "origin", "feat/gone")
	gitT(t, clone, "push", "-q", "origin", "--delete", "feat/gone")
	// squashed: its changes squash-merged into main, then its branch deleted
	squashed := add("squashed", "feat/squashed")
	commit(squashed, "s1")
	commit(squashed, "s2")
	gitT(t, squashed, "push", "-q", "-u", "origin", "feat/squashed")
	gitT(t, clone, "fetch", "-q", "origin")
	gitT(t, clone, "merge", "-q", "--ff-only", "origin/main")
	gitT(t, clone, "merge", "-q", "--squash", "feat/squashed")
	gitT(t, clone, "commit", "-qm", "squashed")
	gitT(t, clone, "push", "-q", "origin", "main")
	gitT(t, clone, "push", "-q", "origin", "--delete", "feat/squashed")
	// pushed and fast-forwarded into main, then never committed to again: still merged
	// a new branch pushed under its own name with no commit yet: work about to begin
	pushedNew := add("pushed-new", "feat/pushed-new")
	gitT(t, pushedNew, "push", "-q", "-u", "origin", "feat/pushed-new")
	// gone, with a commit made after the push
	ahead := add("ahead", "feat/ahead")
	commit(ahead, "c")
	gitT(t, ahead, "push", "-q", "-u", "origin", "feat/ahead")
	commit(ahead, "d")
	gitT(t, clone, "push", "-q", "origin", "--delete", "feat/ahead")
	// gone, with an uncommitted file
	dirty := add("dirty", "feat/dirty")
	commit(dirty, "e")
	gitT(t, dirty, "push", "-q", "-u", "origin", "feat/dirty")
	gitT(t, clone, "push", "-q", "origin", "--delete", "feat/dirty")
	write(t, filepath.Join(dirty, "notes.txt"), "wip\n")
	// work in progress: a new branch that still tracks main, one never pushed, and a detached one
	add("fresh", "feat/fresh")
	gitT(t, filepath.Join(wts, "fresh"), "branch", "-q", "--set-upstream-to", "origin/main")
	local := add("local", "feat/local")
	gitT(t, local, "branch", "-q", "--unset-upstream")
	commit(local, "f")
	gitT(t, clone, "worktree", "add", "-q", "--detach", filepath.Join(wts, "detached"), "origin/main")
	// merged, in a lane whose branch names an excluded project
	lane := add("lane", "secretlane/T1")
	commit(lane, "g")
	gitT(t, lane, "push", "-q", "-u", "origin", "secretlane/T1")
	gitT(t, lane, "push", "-q", "origin", "secretlane/T1:main")
	gitT(t, clone, "fetch", "-q", "origin")
	return clone, wts
}

func TestLeftoverWorktreesGoWhenCleanAndPushed(t *testing.T) {
	clone, wts := worktreesFixture(t)
	now := time.Now().Add(2 * time.Hour)
	cfg := Config{Worktrees: WorktreesConfig{Roots: []string{wts}}, Exclude: Exclude{Names: []string{"secretlane"}}}.Normal()
	cfg.Live = []string{RuleWorktrees}
	st := openStore(t)
	res, err := Run(context.Background(), cfg, st, options(t, procRoot(t), now, nil))
	if err != nil {
		t.Fatal(err)
	}
	got := findings(res, RuleWorktrees)
	want := map[string]string{
		"merged":   ActDelete,
		"squashed": ActDelete, // its changes are in main
		"gone":     ActReport, // deleted on the remote, its commit on no remote and not in main
		"ahead":    ActReport, // a commit on no remote
		"dirty":    ActReport, // an untracked file
		"lane":     ActReport, // an excluded project's branch
	}
	for name, a := range want {
		f := got[filepath.Join(wts, name)]
		if f.Action != a {
			t.Errorf("%s: %q (%s %s), want %q", name, f.Action, f.Detail, f.Why, a)
		}
	}
	for _, name := range []string{"fresh", "local", "detached", "pushed-new"} {
		if f, ok := got[filepath.Join(wts, name)]; ok {
			t.Errorf("%s is work in progress, not a leftover: %+v", name, f)
		}
	}
	for name, a := range want {
		_, err := os.Stat(filepath.Join(wts, name))
		if gone := os.IsNotExist(err); gone != (a == ActDelete) {
			t.Errorf("%s: gone %v, want gone %v", name, gone, a == ActDelete)
		}
	}
	branches := gitT(t, clone, "branch", "--list")
	if strings.Contains(branches, "feat/merged") || strings.Contains(branches, "feat/squashed") {
		t.Errorf("a removed worktree's branch goes with it: %s", branches)
	}
	if !strings.Contains(branches, "feat/gone") || !strings.Contains(branches, "feat/ahead") || !strings.Contains(branches, "feat/dirty") {
		t.Errorf("a reported worktree keeps its branch: %s", branches)
	}
	if got[filepath.Join(wts, "ahead")].class != "unpushed" || got[filepath.Join(wts, "gone")].class != "unpushed" ||
		got[filepath.Join(wts, "dirty")].class != "uncommitted" {
		t.Error("each report-only worktree says why")
	}
}

func TestAWorktreeChangedWithinTheHourOrInUseStays(t *testing.T) {
	_, wts := worktreesFixture(t)
	now := time.Now().Add(2 * time.Hour)
	must(t, os.Chtimes(filepath.Join(wts, "merged"), now, now.Add(-10*time.Minute)))
	// an add in squashed itself: git's index changed, no directory did
	must(t, os.Chtimes(filepath.Join(filepath.Dir(wts), "clone/.git/worktrees/squashed/index"), now, now.Add(-10*time.Minute)))
	proc := procRoot(t, fakeProc{pid: 3, ppid: 1, argv: []string{"bash"}, cwd: filepath.Join(wts, "gone")})
	cfg := Config{Worktrees: WorktreesConfig{Roots: []string{wts}}}.Normal()
	cfg.Live = []string{RuleWorktrees}
	res, err := Run(context.Background(), cfg, openStore(t), options(t, proc, now, nil))
	if err != nil {
		t.Fatal(err)
	}
	got := findings(res, RuleWorktrees)
	if f := got[filepath.Join(wts, "merged")]; f.Action != ActKeep {
		t.Errorf("a worktree changed 10 minutes ago stays: %+v", f)
	}
	if f := got[filepath.Join(wts, "squashed")]; f.Action != ActKeep || f.Why != "changed in the last 1h0m0s" {
		t.Errorf("a worktree whose own index changed 10 minutes ago stays, for that: %+v", f)
	}
	for _, name := range []string{"merged", "gone", "squashed"} {
		if _, err := os.Stat(filepath.Join(wts, name)); err != nil {
			t.Errorf("%s was removed", name)
		}
	}
}

func TestAWorktreeOfARepositorySomeoneWorksInStays(t *testing.T) {
	for _, c := range []struct {
		name     string
		proc     []fakeProc
		sessions map[int]Session
		change   func(t *testing.T, base string, now time.Time) // what someone did 10 minutes ago
	}{
		{"a shell in the main checkout", []fakeProc{{pid: 3, ppid: 1, argv: []string{"bash"}}}, nil, nil},
		{"an agent session in another worktree", nil, map[int]Session{-1: {Key: "s9", Name: "fixing", Tool: "codex"}}, nil},
		// an agent between two commands runs nothing there, but its last command changed something
		{"a file made in another worktree 10 minutes ago", nil, nil, touched("cache/ahead")},
		{"a commit in the main checkout 10 minutes ago", nil, nil, func(t *testing.T, base string, now time.Time) {
			logged(t, filepath.Join(base, "clone/.git/logs/HEAD"), now.Add(-10*time.Minute))
		}},
		{"an add in another worktree 10 minutes ago", nil, nil, touched("clone/.git/worktrees/gone/index")},
	} {
		t.Run(c.name, func(t *testing.T) {
			clone, wts := worktreesFixture(t)
			for i := range c.proc {
				c.proc[i].cwd = clone
			}
			for k, s := range c.sessions {
				s.Cwd = filepath.Join(wts, "ahead")
				c.sessions[k] = s
			}
			now := time.Now().Add(2 * time.Hour)
			if c.change != nil {
				c.change(t, filepath.Dir(clone), now)
			}
			cfg := Config{Worktrees: WorktreesConfig{Roots: []string{wts}}}.Normal()
			cfg.Live = []string{RuleWorktrees}
			o := options(t, procRoot(t, c.proc...), now, nil)
			o.Sessions = func(context.Context) map[int]Session { return c.sessions }
			res, err := Run(context.Background(), cfg, openStore(t), o)
			if err != nil {
				t.Fatal(err)
			}
			f := findings(res, RuleWorktrees)[filepath.Join(wts, "merged")]
			if f.Action != ActKeep || !strings.Contains(f.Why, "in use") {
				t.Errorf("a merged worktree of a repository someone works in stays: %+v", f)
			}
			if _, err := os.Stat(filepath.Join(wts, "merged")); err != nil {
				t.Error("it was removed")
			}
		})
	}
}

// touched is a change to a path under the fixture 10 minutes before the run.
func touched(rel string) func(*testing.T, string, time.Time) {
	return func(t *testing.T, base string, now time.Time) {
		must(t, os.Chtimes(filepath.Join(base, rel), now, now.Add(-10*time.Minute)))
	}
}

// logged appends an entry made at a time to a reflog, as a commit or a checkout does.
func logged(t *testing.T, reflog string, at time.Time) {
	t.Helper()
	f, err := os.OpenFile(reflog, os.O_APPEND|os.O_WRONLY, 0o644)
	must(t, err)
	zero := strings.Repeat("0", 40)
	_, err = fmt.Fprintf(f, "%s %s T <t@example.com> %d +0000\tcommit: x\n", zero, zero, at.Unix())
	must(t, err)
	must(t, f.Close())
}

func TestAFetchOrAGcInTheRepositoryIsNoOneAtWork(t *testing.T) {
	for _, bare := range []bool{false, true} {
		clone, wts := worktreesFixture(t)
		hub := clone
		if bare { // a bare clone that holds the worktrees, as some lanes keep one
			hub = filepath.Join(filepath.Dir(clone), "hub.git")
			gitT(t, clone, "clone", "-q", "--bare", filepath.Join(filepath.Dir(clone), "remote.git"), hub)
			gitT(t, hub, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
			gitT(t, hub, "fetch", "-q", "origin")
			gitT(t, hub, "remote", "set-head", "origin", "main")
			m := filepath.Join(wts, "hubbed")
			gitT(t, hub, "worktree", "add", "-q", "-b", "feat/hubbed", m, "origin/main")
			write(t, filepath.Join(m, "h"), "h\n")
			gitT(t, m, "add", ".")
			gitT(t, m, "commit", "-qm", "h")
			gitT(t, m, "push", "-q", "-u", "origin", "feat/hubbed")
			gitT(t, m, "push", "-q", "origin", "feat/hubbed:main")
			gitT(t, hub, "fetch", "-q", "origin")
		}
		now := time.Now().Add(2 * time.Hour)
		store := filepath.Join(hub, ".git")
		if bare {
			store = hub
		}
		// a fetch writes the objects and the remote refs; a gc rewrites every HEAD log with its entries kept
		logs, _ := filepath.Glob(filepath.Join(store, "worktrees", "*", "logs", "HEAD"))
		logs = append(logs, filepath.Join(store, "logs", "HEAD"))
		for _, p := range append(logs, store, filepath.Join(store, "objects"), filepath.Join(store, "refs", "remotes", "origin")) {
			if _, err := os.Stat(p); err == nil {
				must(t, os.Chtimes(p, now, now.Add(-10*time.Minute)))
			}
		}
		cfg := Config{Worktrees: WorktreesConfig{Roots: []string{wts}}}.Normal()
		cfg.Live = []string{RuleWorktrees}
		res, err := Run(context.Background(), cfg, openStore(t), options(t, procRoot(t), now, nil))
		if err != nil {
			t.Fatal(err)
		}
		name := "merged"
		if bare {
			name = "hubbed"
		}
		if f := findings(res, RuleWorktrees)[filepath.Join(wts, name)]; f.Action != ActDelete || !f.Done {
			t.Errorf("bare %v: a fetch and a gc 10 minutes ago are no one at work: %+v", bare, f)
		}
	}
}

func TestCapacitysOwnDeletionsAreNoOneAtWork(t *testing.T) {
	now := time.Now().Add(2 * time.Hour)
	ctx := context.Background()
	st := openStore(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	someone, ours, earlier, older := filepath.Join(base, "someone"), filepath.Join(base, "ours"), filepath.Join(base, "earlier"),
		filepath.Join(base, "older")
	for _, d := range []string{someone, ours, earlier, older} {
		must(t, os.MkdirAll(d, 0o755))
		must(t, os.Chtimes(d, now, now.Add(-10*time.Minute)))
	}
	// an earlier run deleted earlier/target 10 minutes ago, and recorded which directories that changed; an older
	// build recorded its deletion in older without them
	_, err = st.Put(ctx, []store.Event{
		{Source: store.Capacity, SourceKey: "d1", TS: now.Add(-10 * time.Minute), Actor: store.ActorAgent, Kind: KindDelete,
			Summary: "x", Ref: filepath.Join(earlier, "target"), Meta: `{"own":["` + earlier + `"]}`},
		{Source: store.Capacity, SourceKey: "d2", TS: now.Add(-10 * time.Minute), Actor: store.ActorAgent, Kind: KindDelete,
			Summary: "x", Ref: filepath.Join(older, "target"), Meta: `{}`}})
	must(t, err)
	r := &runner{now: now, st: st, m: ReadMounts("")}
	r.ownDeletions(ctx)
	r.addOwnDir(ours, now.Add(-10*time.Minute)) // this run deleted ours/target
	look, since := &Sample{}, now.Add(-quietTime)
	if !r.changedIn(ctx, someone, since, look) {
		t.Error("someone's change 10 minutes ago is a change")
	}
	if r.changedIn(ctx, ours, since, look) {
		t.Error("this run's own deletion is no one at work")
	}
	if r.changedIn(ctx, earlier, since, look) {
		t.Error("an earlier run's recorded deletion is no one at work")
	}
	if !r.changedIn(ctx, older, since, look) {
		t.Error("a deletion recorded without its directories explains nothing")
	}
	must(t, os.Chtimes(ours, now, now.Add(-5*time.Minute)))
	if !(&runner{now: now, own: r.own, m: r.m}).changedIn(ctx, ours, since, look) {
		t.Error("a change after capacity's own is someone's")
	}
}

func TestAMainCheckoutWithItsGitDirApartIsReadThere(t *testing.T) {
	now := time.Now().Add(2 * time.Hour)
	base := t.TempDir()
	tree, apart := filepath.Join(base, "tree"), filepath.Join(base, "apart.git")
	write(t, filepath.Join(tree, ".git"), "gitdir: "+apart+"\n")
	write(t, filepath.Join(apart, "index"), "x")
	must(t, os.Chtimes(filepath.Join(apart, "index"), now, now.Add(-10*time.Minute)))
	r := &runner{now: now, m: ReadMounts("")}
	if !r.changedIn(context.Background(), tree, now.Add(-quietTime), &Sample{}) {
		t.Error("an add 10 minutes ago in a checkout cloned with --separate-git-dir is a change")
	}
}

func TestCapacitysOwnDeletionInAWorktreeDoesNotHoldTheRepositorysLeftovers(t *testing.T) {
	_, wts := worktreesFixture(t)
	_, target := ownTarget(t, wts)
	now := time.Now().Add(49 * time.Hour)
	cfg := Config{Worktrees: WorktreesConfig{Roots: []string{wts}}, Targets: TargetsConfig{Roots: []string{wts}}}.Normal()
	cfg.Live = []string{RuleTargets, RuleWorktrees}
	o := options(t, procRoot(t), now, nil)
	o.afterDelete = func(f Finding) {
		if f.Rule == RuleTargets { // the deletion changes the directory it was in, at the run's time
			must(t, os.Chtimes(filepath.Dir(f.Path), now, now))
		}
	}
	res, err := Run(context.Background(), cfg, openStore(t), o)
	if err != nil {
		t.Fatal(err)
	}
	if f := findings(res, RuleTargets)[target]; !f.Done {
		t.Fatalf("the idle build goes first: %+v", f)
	}
	if f := findings(res, RuleWorktrees)[filepath.Join(wts, "merged")]; !f.Done {
		t.Errorf("capacity's own deletion in another worktree is no one at work there: %+v", f)
	}
}

// ownTarget is a fixture's work-in-progress worktree with an idle build of its own, which the targets rule deletes.
func ownTarget(t *testing.T, wts string) (local, target string) {
	t.Helper()
	local = filepath.Join(wts, "local")
	target = filepath.Join(local, "target")
	write(t, filepath.Join(local, ".gitignore"), "target/\n")
	write(t, filepath.Join(target, "CACHEDIR.TAG"), cargoTag+"\n# This file is a cache directory tag created by cargo.\n")
	write(t, filepath.Join(target, ".rustc_info.json"), "{}")
	blob(t, filepath.Join(target, "debug", "app"), 2<<20)
	return local, target
}

func TestSomeonesChangeBeforeCapacitysDeletionInTheSameDirectoryStaysTheirs(t *testing.T) {
	_, wts := worktreesFixture(t)
	local, target := ownTarget(t, wts)
	now := time.Now().Add(49 * time.Hour)
	cfg := Config{Worktrees: WorktreesConfig{Roots: []string{wts}}, Targets: TargetsConfig{Roots: []string{wts}}}.Normal()
	cfg.Live = []string{RuleTargets, RuleWorktrees}
	o := options(t, procRoot(t), now, nil)
	// after the judgment, someone makes a file in local; then capacity deletes local/target, which hides that time
	o.beforeAct = func() { must(t, os.Chtimes(local, now, now.Add(-time.Minute))) }
	o.afterDelete = func(f Finding) {
		if f.Rule == RuleTargets {
			must(t, os.Chtimes(filepath.Dir(f.Path), now, now))
		}
	}
	res, err := Run(context.Background(), cfg, openStore(t), o)
	if err != nil {
		t.Fatal(err)
	}
	if f := findings(res, RuleTargets)[target]; !f.Done {
		t.Fatalf("the idle build goes: %+v", f)
	}
	if f := findings(res, RuleWorktrees)[filepath.Join(wts, "merged")]; f.Done || !strings.Contains(f.Why, "a change in") {
		t.Errorf("someone at work in the repository a minute before keeps its leftovers: %+v", f)
	}
}

func TestALaterRunTakesAnEarlierRunsDeletionAsItsOwn(t *testing.T) {
	_, wts := worktreesFixture(t)
	_, target := ownTarget(t, wts)
	tick := time.Now().Add(49 * time.Hour)
	clock := func() time.Time { tick = tick.Add(10 * time.Second); return tick } // each look at the clock is later
	st := openStore(t)
	cfg := Config{Worktrees: WorktreesConfig{Roots: []string{wts}}, Targets: TargetsConfig{Roots: []string{wts}}}.Normal()
	cfg.Live = []string{RuleTargets, RuleWorktrees}
	o := options(t, procRoot(t), tick, nil)
	o.Clock, o.Rules = clock, []string{RuleTargets}
	o.afterDelete = func(f Finding) { // the deletion changes its directory when it happens, after the run began
		at := clock()
		must(t, os.Chtimes(filepath.Dir(f.Path), at, at))
	}
	res, err := Run(context.Background(), cfg, st, o)
	if err != nil {
		t.Fatal(err)
	}
	if f := findings(res, RuleTargets)[target]; !f.Done {
		t.Fatalf("the first run deletes the build: %+v", f)
	}
	o2 := options(t, procRoot(t), tick, nil)
	o2.Clock, o2.Rules = clock, []string{RuleWorktrees}
	res, err = Run(context.Background(), cfg, st, o2)
	if err != nil {
		t.Fatal(err)
	}
	if f := findings(res, RuleWorktrees)[filepath.Join(wts, "merged")]; !f.Done {
		t.Errorf("a run minutes later takes the first run's deletion as capacity's own: %+v", f)
	}
}

func TestAReflogsTimeIsItsLastEntrysWhenItCanBeRead(t *testing.T) {
	dir := t.TempDir()
	at := time.Unix(1700000000, 0)
	for _, c := range []struct {
		name, last string
		want       time.Time
	}{
		{"an entry as git writes it", "0 1 A U Thor <a@example.com> 1700000000 +0200\tcommit: x", at},
		{"a zone git does not write", "0 1 A U Thor <a@example.com> 1700000000 X\tcommit: x", time.Time{}},
		{"no email before the time", "0 1 A U Thor a@example.com 1700000000 +0200\tcommit: x", time.Time{}},
		// a message whose tail reads like a header: only the entry's start says where its header is
		{"an entry longer than the tail read", "0 1 A <a@example.com> 1600000000 +0000\tcommit: " + strings.Repeat("y", 70<<10) +
			" from me <b@example.com> 12 +0000", time.Time{}},
	} {
		p := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "-"))
		write(t, p, "0 0 A <a@example.com> 1500000000 +0000\tclone\n"+c.last+"\n")
		st, err := os.Lstat(p)
		must(t, err)
		want := c.want
		if want.IsZero() {
			want = newest(st.Sys().(*syscall.Stat_t)) // not readable as an entry: the file's own time
		}
		if got, ok := reflogTime(p); !ok || !got.Equal(want) {
			t.Errorf("%s: %v %v, want %v", c.name, got, ok, want)
		}
	}
}

func TestEveryDeletionOfTheLastHourIsRead(t *testing.T) {
	now := time.Now().Add(2 * time.Hour)
	ctx := context.Background()
	st := openStore(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	first := filepath.Join(base, "first")
	must(t, os.MkdirAll(first, 0o755))
	must(t, os.Chtimes(first, now, now.Add(-50*time.Minute)))
	evs := []store.Event{{Source: store.Capacity, SourceKey: "d0", TS: now.Add(-50 * time.Minute), Actor: store.ActorAgent,
		Kind: KindDelete, Summary: "x", Meta: `{"own":["` + first + `"]}`}}
	for i := 1; i <= 300; i++ { // a run that deleted many builds after it
		evs = append(evs, store.Event{Source: store.Capacity, SourceKey: fmt.Sprintf("d%d", i), TS: now.Add(-40 * time.Minute),
			Actor: store.ActorAgent, Kind: KindDelete, Summary: "x", Meta: fmt.Sprintf(`{"own":["%s/n%d"]}`, base, i)})
	}
	_, err = st.Put(ctx, evs)
	must(t, err)
	r := &runner{now: now, st: st, m: ReadMounts("")}
	if r.changedIn(ctx, first, now.Add(-quietTime), &Sample{}) {
		t.Error("the first of 301 deletions in the hour is read too")
	}
}
