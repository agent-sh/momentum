package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/jail"
	"github.com/agent-sh/momentum/internal/store"
)

// Preparation completes before the build starts; the sandbox can use all of its independent Git clone.
func TestABuildSeesTheCompleteCloneBeforeItStarts(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c := testingCase(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	harness := script(t, dir, "harness", `printf '%s\n' "$@" > "$(dirname "$0")/args"
 ls -A '`+p.Work+`' > listing
 git -C '`+p.Work+`' log -1 > history
 echo marker-7f3a > '`+filepath.Join(p.Work, "from-the-run.txt")+`'
 git -C '`+p.Work+`' add -A || exit 1
 git -C '`+p.Work+`' diff --cached --binary '`+p.BaseCommit+`' > "$MOMENTUM_LOOP_DIR/change.patch" || exit 1
 test -z "$(git -C '`+p.Work+`' remote)" || exit 1
 test ! -e '`+p.Host+`' || exit 1
 `+modelResult(okResult))
	r := runnerFor(t, st, []string{harness})
	r.Ship = s
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 || done[0].Run.Status != store.RunOK {
		t.Fatalf("%+v %v", done, err)
	}
	listing, _ := os.ReadFile(filepath.Join(done[0].Run.Dir, "listing"))
	for _, want := range []string{".git", "fake.go", "go.mod"} {
		if !strings.Contains(string(listing), want) {
			t.Fatal("incomplete clone")
		}
	}
	history, _ := os.ReadFile(filepath.Join(done[0].Run.Dir, "history"))
	if !strings.Contains(string(history), "init") {
		t.Fatal("sandbox cannot read its own Git")
	}
	b, err := os.ReadFile(filepath.Join(p.Work, "from-the-run.txt"))
	if err != nil || strings.TrimSpace(string(b)) != "marker-7f3a" {
		t.Fatal("clone was not writable")
	}
	if b, err := os.ReadFile(filepath.Join(done[0].Run.Dir, "change.patch")); err != nil || !strings.Contains(string(b), "+marker-7f3a") {
		t.Fatalf("the run could not produce its patch with writable Git: %q %v", b, err)
	}
	if args, _ := agentArgs(t, r.Agent.Runner); !strings.Contains(args, "BASE is "+p.BaseCommit) || !strings.Contains(args, "git diff --cached --binary "+p.BaseCommit) {
		t.Fatalf("the prepared build prompt lost its patch base: %s", args)
	}
}

// A first preparation run starts without a clone or cache mount.
func TestTheFirstBuildOnAMachineStarts(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := runnerFor(t, st, fakeAgent(t, okResult))
	cache := filepath.Join(t.TempDir(), "loop", "cache")
	r.Toolchains = Toolchains{Cache: cache}
	c := testingCase(t, st)
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 || done[0].Case != c.ID || done[0].Step != StepBuild || done[0].Run.Status != store.RunOK {
		t.Fatalf("the first build: %+v %v", done, err)
	}
	if _, err := os.Stat(cache); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a preparation run mounts no cache or clone")
	}
	if args, _ := agentArgs(t, r.Agent.Runner); !strings.Contains(args, "No clone is mounted yet") ||
		!strings.Contains(args, "End this run after preparation with the report") || !strings.Contains(args, "Preparation counts toward all budgets and the stop rule") ||
		strings.Contains(args, "Your independent clone is") {
		t.Fatalf("the first build did not ask only for preparation: %s", args)
	}
	system, err := os.ReadFile(filepath.Join(HostDir(done[0].Run.Dir), "primary", "system.md"))
	if err != nil || !strings.Contains(string(system), "End this preparation run with the report") {
		t.Fatalf("the skill did not ask the preparation run to end: %s %v", system, err)
	}
}

// A mount whose source is missing is an error that names it, and a run that never started spends nothing.
func TestAMountThatIsMissingIsAClearErrorAndCostsNothing(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := runnerFor(t, st, fakeAgent(t, okResult))
	missing := filepath.Join(t.TempDir(), "toolchain-that-is-gone")
	r.Toolchains = Toolchains{ReadOnly: []string{missing}, Cache: filepath.Join(t.TempDir(), "cache")}
	c := testingCase(t, st)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	if _, err := s.Prepare(ctx, c.ID, f.main); err != nil {
		t.Fatal(err)
	}
	r.Ship = s
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 {
		t.Fatalf("%+v %v", done, err)
	}
	run := done[0].Run
	// the failure is a short key the stop rule compares; the diagnostic that names the mount is the run's summary
	if run.Status != store.RunFailed || !strings.Contains(run.Summary, missing) || !strings.Contains(run.Summary, "does not exist") || strings.Contains(run.Summary, "bwrap") {
		t.Fatalf("the summary does not name the mount: %+v", run)
	}
	if got, _ := st.CaseByID(ctx, c.ID); run.USD != 0 || got.Cost.USD != 0 {
		t.Fatalf("a run that never started was charged %v, its case %v", run.USD, got.Cost.USD)
	}
}

// doorProgram is a fake momentum for a run's door: it logs what it was asked and where, with the roots the run reads
// repositories under, and answers evidence git as the real command does for a directory outside them.
func doorProgram(t *testing.T, dir, log string) string {
	t.Helper()
	return script(t, dir, "momentum", `echo "$* pwd=$PWD read=$MOMENTUM_LOOP_READ run=$MOMENTUM_LOOP_RUN" >> '`+log+`'
if [ "$1 $2" = "evidence git" ]; then
  case ":$MOMENTUM_LOOP_READ:" in
    *":$3:"*) echo "history of $3"; exit 0 ;;
  esac
  echo "$3 is not where this run reads repositories" >&2
  exit 2
fi
echo "answered $1"`)
}

// A build runs through its mounts and its door, in the real sandbox: its own directory, its clone and the loop's
// build cache are writable, the toolchains read-only and offline. Its door uses a host-only command directory
// and reads only the configured read repositories.
func TestABuildRunsThroughItsMountsAndItsDoor(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	outside := t.TempDir()
	log := filepath.Join(outside, "ran")
	dir := t.TempDir()
	toolchain := t.TempDir()
	write(t, filepath.Join(toolchain, "tool.txt"), "marker-7f3a")
	cache := filepath.Join(t.TempDir(), "cache")
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c := testingCase(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	work := p.Work
	harness := script(t, dir, "harness", `echo own > own.txt
cat '`+filepath.Join(toolchain, "tool.txt")+`' > seen-tool.txt
echo changed > '`+filepath.Join(toolchain, "new.txt")+`' 2> /dev/null; echo "toolchain-write=$?" >> codes
echo cached > "$GOCACHE/marker-7f3a"; echo "cache-write=$?" >> codes
echo "offline=$GOPROXY" >> codes
echo built > '`+filepath.Join(work, "built.txt")+`'; echo "tree-write=$?" >> codes
momentum evidence git '`+work+`' log > git-out 2>&1; echo "tree-git=$?" >> codes
momentum evidence git '`+toolchain+`' log > other-out 2>&1; echo "other-git=$?" >> codes
(cd '`+work+`' && momentum case show here > /dev/null); (cd /usr && momentum case show there > /dev/null)
momentum ship prepare --repo /somewhere > prepare-out 2>&1; echo "prepare=$?" >> codes
momentum loop lesson --text x > lesson-out 2>&1; echo "lesson=$?" >> codes
`+modelResult(okResult))
	r := runnerFor(t, st, []string{harness})
	r.Ship = s
	r.ReadDirs = []string{f.main}
	r.Momentum = doorProgram(t, outside, log)
	r.Toolchains = Toolchains{ReadOnly: []string{toolchain}, Cache: cache}
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 || done[0].Step != StepBuild || done[0].Run.Status != store.RunOK {
		t.Fatalf("%+v %v", done, err)
	}
	runDir := done[0].Run.Dir
	codes, _ := os.ReadFile(filepath.Join(runDir, "codes"))
	for _, want := range []string{"cache-write=0", "offline=off", "tree-write=0", "tree-git=2", "other-git=2", "prepare=0", "lesson=126"} {
		if !strings.Contains(string(codes), want+"\n") {
			t.Errorf("the run's record lacks %q:\n%s", want, codes)
		}
	}
	if strings.Contains(string(codes), "toolchain-write=0") {
		t.Errorf("the run wrote in a toolchain:\n%s", codes)
	}
	if _, err := os.Stat(filepath.Join(toolchain, "new.txt")); err == nil {
		t.Error("the toolchain changed")
	}
	for path, want := range map[string]string{filepath.Join(runDir, "own.txt"): "own", filepath.Join(runDir, "seen-tool.txt"): "marker-7f3a",
		filepath.Join(cache, "go-build", "marker-7f3a"): "cached", filepath.Join(work, "built.txt"): "built"} {
		if b, err := os.ReadFile(path); err != nil || strings.TrimSpace(string(b)) != want {
			t.Errorf("%s: %q %v, want %q", path, b, err, want)
		}
	}
	ran, _ := os.ReadFile(log)
	for _, want := range []string{"evidence git " + work + " log pwd=" + HostDir(runDir) + " read=" + f.main, "case show here pwd=" + HostDir(runDir), "case show there pwd=" + HostDir(runDir),
		"ship prepare --repo /somewhere pwd=" + HostDir(runDir)} {
		if !strings.Contains(string(ran), want) {
			t.Errorf("the door ran:\n%s\nwant %q", ran, want)
		}
	}
	if strings.Contains(string(ran), "loop lesson") {
		t.Errorf("a build's door ran the lesson:\n%s", ran)
	}
}

// A review runs through its mounts and its door, in the real sandbox: the proposed export is read-only with no .git.
// Its history is read through the door in configured repositories. Its change and package are read-only inputs,
// and its door runs no ship step.
func TestAReviewReadsItsChangesWorkTreeThroughItsDoor(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	outside := t.TempDir()
	log := filepath.Join(outside, "ran")
	dir := t.TempDir()
	c := testingCase(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "notes.txt"), "marker-7f3a\n")
	tree, _, err := s.applyPatch(ctx, p, []Patch{fixturePatch(t, s, c.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.commitTree(ctx, p, tree, p.BaseCommit, "fixture"); err != nil {
		t.Fatal(err)
	}
	p.Head, p.State = strings.TrimSpace(run(t, p.Host, "git", "rev-parse", "HEAD")), Proposed
	export := filepath.Join(s.Dir, "exports", ".review-"+c.ID)
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	harness := script(t, dir, "harness", `cat '`+filepath.Join(export, "notes.txt")+`' > seen-tree; echo "tree-read=$?" >> codes
test ! -e '`+filepath.Join(export, ".git")+`' && test ! -L '`+filepath.Join(export, ".git")+`'; echo "git-absent=$?" >> codes
test ! -e '`+p.Work+`' && test ! -L '`+p.Work+`'; echo "clone-absent=$?" >> codes
if echo forbidden > '`+filepath.Join(export, "forbidden.txt")+`' 2> /dev/null; then echo "tree-write=allowed" >> codes
else echo "tree-write=denied" >> codes; fi
test -s /run/momentum/input/diff.patch && echo "diff=present" >> codes
test -s /run/momentum/input/package.json && echo "package=present" >> codes
git -C '`+export+`' log > inner-git 2>&1; echo "inner-git=$?" >> codes
momentum evidence git '`+f.main+`' log > git-out 2>&1; echo "tree-git=$?" >> codes
momentum ship prepare --repo /somewhere > prepare-out 2>&1; echo "prepare=$?" >> codes
momentum case set `+c.ID+` --status open > set-out 2>&1; echo "case-set=$?" >> codes
`+modelResult(okResult))
	r := runnerFor(t, st, []string{harness})
	r.Ship = s
	r.ReadDirs = []string{f.main}
	r.Momentum = doorProgram(t, outside, log)
	review, why := r.reserve(ctx, c.Family, c.ID, StepReview)
	if why != "" {
		t.Fatal(why)
	}
	res, err := r.invokeReview(ctx, c, p, review)
	if err != nil || res.Status != store.RunOK {
		t.Fatalf("%+v %v", res, err)
	}
	if b, err := os.ReadFile(filepath.Join(review.Dir, "seen-tree")); err != nil || string(b) != "marker-7f3a\n" {
		t.Fatalf("the review could not read the proposed tree: %q %v", b, err)
	}
	codes, _ := os.ReadFile(filepath.Join(review.Dir, "codes"))
	for _, want := range []string{"tree-read=0", "tree-write=denied", "git-absent=0", "clone-absent=0", "diff=present", "package=present", "tree-git=0", "prepare=126", "case-set=126"} {
		if !strings.Contains(string(codes), want+"\n") {
			t.Errorf("the review's record lacks %q:\n%s", want, codes)
		}
	}
	if strings.Contains(string(codes), "inner-git=0") {
		t.Errorf("the review read Git history from its export:\n%s", codes)
	}
	if b, _ := os.ReadFile(filepath.Join(review.Dir, "git-out")); !strings.Contains(string(b), "history of "+f.main) {
		t.Errorf("the review read no history of its work tree through its door: %q", b)
	}
	ran, _ := os.ReadFile(log)
	if !strings.Contains(string(ran), "evidence git "+f.main+" log pwd="+HostDir(review.Dir)+" read="+f.main) {
		t.Errorf("the door ran:\n%s", ran)
	}
	if strings.Contains(string(ran), "ship prepare") || strings.Contains(string(ran), "case set") {
		t.Errorf("a review's door ran a step that changes something:\n%s", ran)
	}
}

// Every door command runs in a host-only directory, regardless of the client's directory.
func TestADoorRunsACommandInTheRunsRootsWithTheLoopsEnvironment(t *testing.T) {
	outside := t.TempDir()
	runDir, work := t.TempDir(), t.TempDir()
	sibling := work + "-sibling"
	sub := filepath.Join(work, "sub")
	for _, d := range []string{sibling, sub} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ZZ_HOST_ONLY", "marker-7f3a")
	momentum := script(t, outside, "momentum", `echo "pwd=$PWD"; `+maskedEnv("MOMENTUM_LOOP_RUN", "MOMENTUM_LOOP_CASE", "MOMENTUM_LOOP_DIR", "MOMENTUM_LOOP_READ"))
	r := &Runner{Momentum: momentum, Budgets: Budgets{RunTime: time.Minute}}
	door := r.door(StepBuild, store.Case{ID: "c1"}, store.LoopRun{ID: "r1", Dir: runDir}, []string{runDir, work})
	ask := func(dir string) string {
		reply := door(context.Background(), jail.Request{Argv: []string{"momentum", "case", "show", "c1"}, Dir: dir})
		if reply.Code != 0 {
			t.Fatalf("%s: %+v", dir, reply)
		}
		return string(reply.Stdout)
	}
	for dir, want := range map[string]string{work: HostDir(runDir), sub: HostDir(runDir), sibling: HostDir(runDir), outside: HostDir(runDir), "": HostDir(runDir), "/": HostDir(runDir)} {
		if got := ask(dir); !strings.Contains(got, "\npwd="+want+"\n") && !strings.HasPrefix(got, "pwd="+want+"\n") {
			t.Errorf("a command typed in %q ran in:\n%s\nwant %s", dir, got, want)
		}
	}
	// the listing names every variable and holds the values of the loop's own alone
	env := ask(work)
	if strings.Contains(env, "ZZ_HOST_ONLY") {
		t.Error("the caller's variable ZZ_HOST_ONLY reached the command")
	}
	for _, want := range []string{"MOMENTUM_LOOP_RUN=r1\n", "MOMENTUM_LOOP_CASE=c1\n", "MOMENTUM_LOOP_DIR=" + runDir + "\n", "MOMENTUM_LOOP_READ=\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("the command's environment lacks the line %q", want)
		}
	}
}

// A command the door runs ends when its request does (the client was stopped, or the run ended), and at the run's
// time: it does not run on for a client that is gone.
func TestADoorEndsACommandWithItsRequestAndItsRunsTime(t *testing.T) {
	outside := t.TempDir()
	runDir := t.TempDir()
	for name, c := range map[string]struct {
		cancelAfter time.Duration
		runTime     time.Duration
	}{"request": {300 * time.Millisecond, time.Minute}, "time": {0, 300 * time.Millisecond}} {
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(outside, "finished-"+name)
			slow := script(t, outside, "slow-"+name, `sleep 1.5
touch '`+marker+`'`)
			r := &Runner{Momentum: slow, Budgets: Budgets{RunTime: c.runTime}}
			door := r.door(StepBuild, store.Case{ID: "c1"}, store.LoopRun{ID: "r1", Dir: runDir}, []string{runDir})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if c.cancelAfter > 0 {
				time.AfterFunc(c.cancelAfter, cancel)
			}
			start := time.Now()
			reply := door(ctx, jail.Request{Argv: []string{"momentum", "case", "show", "c1"}})
			if took := time.Since(start); took > time.Second || reply.Code == 0 {
				t.Fatalf("the command ran %v and ended with %+v", took, reply)
			}
			time.Sleep(1500 * time.Millisecond)
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the command ran on after its request ended")
			}
		})
	}
}

// A run keeps its session in its own directory only when it is a live run's: an evaluation's runs keep none.
func TestARunKeepsNoSessionUnlessItIsAskedTo(t *testing.T) {
	for _, persist := range []bool{false, true} {
		agent := fakeAgent(t, okResult)
		a := testJail(t, Agent{Runner: agent, Model: "m"})
		if _, err := a.Run(context.Background(), Invocation{Dir: filepath.Join(t.TempDir(), "run"), Schema: runSchema, MaxTurns: 1, BudgetUSD: 1,
			Timeout: time.Minute, Persist: persist}); err != nil {
			t.Fatal(err)
		}
		if args, _ := agentArgs(t, agent); strings.Contains(args, "--no-session-persistence") == persist {
			t.Errorf("persist %v, and the arguments hold --no-session-persistence: %v:\n%s", persist, !persist, args)
		}
	}
}
