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

// whenStarted calls fn in the background once the file marker exists (a fake harness touches it when it starts), and
// returns a function that waits for fn and gives what it returned: the run it watches is over by then.
func whenStarted(marker string, fn func() error) (wait func() error) {
	res := make(chan error, 1)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				res <- errors.New("the harness never started")
				return
			default:
			}
			if _, err := os.Stat(marker); err == nil {
				res <- fn()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	return func() error {
		close(stop)
		return <-res
	}
}

// A build's sandbox mounts the case's work tree before the build runs, and `ship prepare` makes the tree while it
// runs, from outside: the tree has to be made in the directory the sandbox holds, not in a new one at its path. The
// run lists the files prepare made, and what it writes there is what the host sees.
func TestABuildSeesTheWorkTreeThatPrepareMakes(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c := testingCase(t, st)
	work := s.workDir(c.ID)
	dir := t.TempDir()
	started, prepared := filepath.Join(dir, "started"), filepath.Join(dir, "prepared")
	harness := script(t, dir, "harness", `touch '`+started+`'
while [ ! -e '`+prepared+`' ]; do sleep 0.05; done
ls -A '`+work+`' > '`+filepath.Join(dir, "listing")+`' 2>&1
echo marker-7f3a > '`+filepath.Join(work, "from-the-run.txt")+`' 2> '`+filepath.Join(dir, "write-error")+`'
echo '`+okResult+`'`)
	r := runnerFor(t, st, []string{harness})
	r.Ship = s
	wait := whenStarted(started, func() error {
		_, err := s.Prepare(ctx, c.ID, f.main)
		return errors.Join(err, os.WriteFile(prepared, nil, 0o644))
	})
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if prepErr := wait(); prepErr != nil {
		t.Fatalf("prepare: %v", prepErr)
	}
	if err != nil || len(done) != 1 || done[0].Run.Status != store.RunOK {
		t.Fatalf("%+v %v", done, err)
	}
	listing, _ := os.ReadFile(filepath.Join(dir, "listing"))
	for _, want := range []string{".git", "fake.go", "go.mod"} {
		if !strings.Contains(string(listing), want) {
			writeErr, _ := os.ReadFile(filepath.Join(dir, "write-error"))
			t.Fatalf("the run lists %q in its work tree, want %s (its write said %q)", listing, want, writeErr)
		}
	}
	if b, err := os.ReadFile(filepath.Join(work, "from-the-run.txt")); err != nil || strings.TrimSpace(string(b)) != "marker-7f3a" {
		writeErr, _ := os.ReadFile(filepath.Join(dir, "write-error"))
		t.Fatalf("the host does not see what the run wrote in its work tree: %q %v (the run said %q)", b, err, writeErr)
	}
}

// The first build on a machine has no work tree and no build cache yet: the runner makes what the run writes before
// its sandbox starts, so the run starts and ends.
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
	if st, err := os.Stat(filepath.Join(cache, "go-build")); err != nil || !st.IsDir() {
		t.Fatalf("the build cache was not made: %v", err)
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
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 {
		t.Fatalf("%+v %v", done, err)
	}
	run := done[0].Run
	if run.Status != store.RunFailed || !strings.Contains(run.Failure, missing) || !strings.Contains(run.Failure, "does not exist") || strings.Contains(run.Failure, "bwrap") {
		t.Fatalf("the failure does not name the mount: %+v", run)
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

// A build runs through its mounts and its door, in the real sandbox: its own directory, its work tree and the loop's
// build cache are writable, the toolchains it tests with read-only and offline, and its door reads repositories in
// its work tree, in the directory it typed a command in when that is the work tree.
func TestABuildRunsThroughItsMountsAndItsDoor(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	outside := t.TempDir()
	log := filepath.Join(outside, "ran")
	dir := t.TempDir()
	toolchain := t.TempDir()
	write(t, filepath.Join(toolchain, "tool.txt"), "marker-7f3a")
	cache := filepath.Join(t.TempDir(), "cache")
	s := &Ship{Store: st, Dir: filepath.Join(t.TempDir(), "loop")}
	c := testingCase(t, st)
	work := s.workDir(c.ID)
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
echo '`+okResult+`'`)
	r := runnerFor(t, st, []string{harness})
	r.Ship = s
	r.Momentum = doorProgram(t, outside, log)
	r.Toolchains = Toolchains{ReadOnly: []string{toolchain}, Cache: cache}
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 || done[0].Step != StepBuild || done[0].Run.Status != store.RunOK {
		t.Fatalf("%+v %v", done, err)
	}
	runDir := done[0].Run.Dir
	codes, _ := os.ReadFile(filepath.Join(runDir, "codes"))
	for _, want := range []string{"cache-write=0", "offline=off", "tree-write=0", "tree-git=0", "other-git=2", "prepare=0", "lesson=126"} {
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
	for _, want := range []string{"evidence git " + work + " log pwd=" + runDir + " read=" + work, "case show here pwd=" + work, "case show there pwd=" + runDir,
		"ship prepare --repo /somewhere pwd=" + runDir} {
		if !strings.Contains(string(ran), want) {
			t.Errorf("the door ran:\n%s\nwant %q", ran, want)
		}
	}
	if strings.Contains(string(ran), "loop lesson") {
		t.Errorf("a build's door ran the lesson:\n%s", ran)
	}
}

// A review runs through its mounts and its door, in the real sandbox: its work tree is read-only and its history is
// read through the door (the work tree's .git link names a directory the sandbox does not hold), its change and
// package are in its own directory, and its door runs no ship step.
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
	write(t, filepath.Join(p.Work, "notes.txt"), "a note\n")
	run(t, p.Work, "git", "add", "-A")
	run(t, p.Work, "git", "commit", "-q", "-m", "the change")
	p.Head, p.State, p.Base = strings.TrimSpace(run(t, p.Work, "git", "rev-parse", "HEAD")), Proposed, "main"
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	harness := script(t, dir, "harness", `echo forbidden > '`+filepath.Join(p.Work, "forbidden.txt")+`' 2> /dev/null; echo "tree-write=$?" >> codes
test -s /run/momentum/input/diff.patch && echo "diff=present" >> codes
test -s /run/momentum/input/package.json && echo "package=present" >> codes
git -C '`+p.Work+`' log > inner-git 2>&1; echo "inner-git=$?" >> codes
momentum evidence git '`+p.Work+`' log > git-out 2>&1; echo "tree-git=$?" >> codes
momentum ship prepare --repo /somewhere > prepare-out 2>&1; echo "prepare=$?" >> codes
momentum case set `+c.ID+` --status open > set-out 2>&1; echo "case-set=$?" >> codes
echo '`+okResult+`'`)
	r := runnerFor(t, st, []string{harness})
	r.Ship = s
	r.Momentum = doorProgram(t, outside, log)
	review, why := r.reserve(ctx, c.Family, c.ID, StepReview)
	if why != "" {
		t.Fatal(why)
	}
	res, err := r.invokeReview(ctx, c, p, review)
	if err != nil || res.Status != store.RunOK {
		t.Fatalf("%+v %v", res, err)
	}
	codes, _ := os.ReadFile(filepath.Join(review.Dir, "codes"))
	for _, want := range []string{"diff=present", "package=present", "tree-git=0", "prepare=126", "case-set=126"} {
		if !strings.Contains(string(codes), want+"\n") {
			t.Errorf("the review's record lacks %q:\n%s", want, codes)
		}
	}
	if strings.Contains(string(codes), "tree-write=0") || strings.Contains(string(codes), "inner-git=0") {
		t.Errorf("the review changed its work tree, or read history in its sandbox:\n%s", codes)
	}
	if _, err := os.Stat(filepath.Join(p.Work, "forbidden.txt")); err == nil {
		t.Error("the review wrote in the change's work tree")
	}
	if b, _ := os.ReadFile(filepath.Join(review.Dir, "git-out")); !strings.Contains(string(b), "history of "+p.Work) {
		t.Errorf("the review read no history of its work tree through its door: %q", b)
	}
	ran, _ := os.ReadFile(log)
	if !strings.Contains(string(ran), "evidence git "+p.Work+" log pwd="+review.Dir+" read="+p.Work) {
		t.Errorf("the door ran:\n%s", ran)
	}
	if strings.Contains(string(ran), "ship prepare") || strings.Contains(string(ran), "case set") {
		t.Errorf("a review's door ran a step that changes something:\n%s", ran)
	}
}

// A door runs a command where it was typed only when that is in one of the run's roots, with the loop's own
// environment and none of the caller's, and ends it with its request or its run's time.
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
	momentum := script(t, outside, "momentum", `echo "pwd=$PWD"; env`)
	r := &Runner{Momentum: momentum, Budgets: Budgets{RunTime: time.Minute}}
	door := r.door(StepBuild, store.Case{ID: "c1"}, store.LoopRun{ID: "r1", Dir: runDir}, []string{runDir, work})
	ask := func(dir string) string {
		reply := door(context.Background(), jail.Request{Argv: []string{"momentum", "case", "show", "c1"}, Dir: dir})
		if reply.Code != 0 {
			t.Fatalf("%s: %+v", dir, reply)
		}
		return string(reply.Stdout)
	}
	for dir, want := range map[string]string{work: work, sub: sub, sibling: runDir, outside: runDir, "": runDir, "/": runDir} {
		if got := ask(dir); !strings.Contains(got, "\npwd="+want+"\n") && !strings.HasPrefix(got, "pwd="+want+"\n") {
			t.Errorf("a command typed in %q ran in:\n%s\nwant %s", dir, got, want)
		}
	}
	env := ask(work)
	if strings.Contains(env, "ZZ_HOST_ONLY") {
		t.Errorf("the caller's environment reached the command:\n%s", env)
	}
	for _, want := range []string{"MOMENTUM_LOOP_RUN=r1\n", "MOMENTUM_LOOP_CASE=c1\n", "MOMENTUM_LOOP_DIR=" + runDir + "\n", "MOMENTUM_LOOP_READ=" + work + "\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("the command's environment lacks %q:\n%s", want, env)
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
