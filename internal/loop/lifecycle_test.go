package loop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// errCrash is the crash a test arms at one write of a lifecycle transition (Ship.Crash): the hook panics with it, as
// a process that dies there stops every step after it.
var errCrash = errors.New("crash")

// lifeWorld is one case's world for the lifecycle property test: two repositories (agent-sh/fake, checked out as main,
// and agent-sh/other, checked out as other) behind a fake gh that keeps its pull requests, a home with a configuration
// file, the ship tool and a runner whose read_dirs are both checkouts. The case's investigation names one of them at a
// time (target).
type lifeWorld struct {
	t       *testing.T
	ctx     context.Context
	st      *store.Store
	f, g    repoFixture
	other   string // agent-sh/other's checkout
	home    string
	s       *Ship
	r       *Runner
	c, twin store.Case // the case, and a second case of its family it may merge into
	target  string     // "main", "other" or "config"
	ghState string
	crashAt int // the write a crash stops at, 0 for none
	writes  int
	agent   string            // a file the agent or the owner wrote in the step: no write of momentum's
	person  map[string]string // a branch a person pushed to, and the head they left ("" for one they deleted)
	// the making of the package whose branch it was: a package made since reuses the case's branch as any preparation
	// after an ended package does, and leases what it holds
	personOf map[string]time.Time
	before   Package       // the case's package before the step
	ahead    time.Duration // how far the world's clock runs ahead of the real one: a week passing moves it
	runs     int
	steps    []string
}

func newLifeWorld(t *testing.T) *lifeWorld {
	t.Helper()
	w := &lifeWorld{t: t, ctx: context.Background(), st: testStore(t), f: newRepo(t, nil), g: newRepo(t, nil), home: fakeHome(t), person: map[string]string{},
		personOf: map[string]time.Time{}}
	w.other = filepath.Join(w.g.dir, "other")
	run(t, w.g.dir, "git", "clone", "-q", w.g.origin, w.other)
	w.ghState = filepath.Join(w.f.dir, "gh-state")
	if err := os.Mkdir(w.ghState, 0o700); err != nil {
		t.Fatal(err)
	}
	gh := script(t, w.f.dir, "gh-stateful", `state="`+w.ghState+`"
echo "$@" >> "$state/calls"
[ -e "$state/fail" ] && { echo "gh: the network is down" >&2; exit 1; }
origin() { case "$1" in agent-sh/other) echo "`+w.g.origin+`";; *) echo "`+w.f.origin+`";; esac; }
flag() { want="$1"; shift; while [ $# -gt 0 ]; do if [ "$1" = "$want" ]; then echo "$2"; return; fi; shift; done; }
case "$1 $2" in
"repo view") echo '{"isPrivate":true,"visibility":"PRIVATE"}';;
"label create") ;;
"pr list")
  repo=$(flag --repo "$@"); head=$(flag --head "$@"); only=$(flag --state "$@"); out="["; sep=""
  for f in "$state"/pr-*; do
    case "$f" in *.title) continue;; esac
    [ -e "$f" ] || continue
    read st r h by < "$f"
    if [ "$r" = "$repo" ] && [ "$h" = "$head" ] && { [ "$only" != open ] || [ "$st" = OPEN ]; }; then
      n=${f##*pr-}; oid=$(git --git-dir="$(origin "$r")" rev-parse "refs/heads/$h" 2>/dev/null)
      out="$out$sep{\"number\":$n,\"url\":\"https://github.com/$r/pull/$n\",\"headRefOid\":\"$oid\",\"state\":\"$st\"}"; sep=","
    fi
  done
  echo "$out]";;
"pr create")
  repo=$(flag --repo "$@"); head=$(flag --head "$@")
  n=$(( $(cat "$state/n" 2>/dev/null || echo 6) + 1 )); echo "$n" > "$state/n"
  echo "OPEN $repo $head -" > "$state/pr-$n"; flag --title "$@" > "$state/pr-$n.title"
  [ -e "$state/lose-create" ] && { rm "$state/lose-create"; echo "gh: the response was lost" >&2; exit 1; }
  echo "https://github.com/$repo/pull/$n";;
"pr edit") n=${3##*/}; flag --title "$@" > "$state/pr-$n.title";;
"pr view")
  n=${3##*/}; st=""; r=""; h=""
  [ -e "$state/pr-$n" ] && read st r h by < "$state/pr-$n"
  oid=$(git --git-dir="$(origin "$r")" rev-parse "refs/heads/$h" 2>/dev/null)
  merged='null'; [ "$st" = MERGED ] && merged='"2026-11-08T00:00:00Z"'
  echo "{\"state\":\"$st\",\"headRefOid\":\"$oid\",\"baseRefName\":\"main\",\"mergedAt\":$merged}";;
"pr close") n=${3##*/}; read st r h by < "$state/pr-$n"; echo "CLOSED $r $h momentum" > "$state/pr-$n";;
esac
exit 0`)
	w.r = localRunner(t, w.st)
	w.r.Inputs.ReadDirs = []string{w.f.main, w.other}
	w.s = shipWithSandbox(w.st, w.f, testSandbox(t))
	w.s.GH, w.s.Live, w.s.Dir = gh, false, w.r.Dir
	w.s.Gate.Resolve = func(_ context.Context, dir string) (string, error) {
		if under(dir, w.g.dir) {
			return "agent-sh/other", nil
		}
		return "agent-sh/fake", nil
	}
	w.s.ConfigTargets = ConfigTargets{Enabled: true, Allow: []string{ruleTarget}, Home: w.home}
	w.s.Crash = func(string) error {
		if w.crashAt == 0 {
			return nil
		}
		if w.writes++; w.writes == w.crashAt {
			panic(errCrash)
		}
		return nil
	}
	w.r.Ship = w.s
	w.r.Post = func(_ context.Context, _, pr, _ string) (string, error) { return pr + "#review", nil }
	w.r.Now = func() time.Time { return time.Now().Add(w.ahead) }
	w.s.Now = w.r.Now
	w.c = testingCase(t, w.st)
	w.twin = otherTestingCase(t, w.st)
	w.name("main")
	return w
}

// name makes target what the case's investigation names, in its current episode.
func (w *lifeWorld) name(target string) {
	w.t.Helper()
	w.target = target
	record := map[string]any{"repo": target, "episode": w.c.Episode}
	if target == "config" {
		record = map[string]any{"repo": "", "config": ruleTarget, "episode": w.c.Episode}
	}
	raw, _ := json.Marshal(record)
	if err := w.st.CaseNote(w.ctx, w.c.ID, repoAction, string(raw), "fixture"); err != nil {
		w.t.Fatal(err)
	}
}

// checkout is the checkout of the repository the case names (the first, while it names a configuration file).
func (w *lifeWorld) checkout() string {
	if w.target == "other" {
		return w.other
	}
	return w.f.main
}

// closeCase is momentum case close: the note first, the close, then the settlement.
func (w *lifeWorld) closeCase(reason store.CloseReason, into, fix string) error {
	if err := ClosingCase(w.ctx, w.st, w.c.ID); err != nil {
		return err
	}
	var err error
	if fix != "" {
		_, err = w.st.CloseSolvedBy(w.ctx, w.c.ID, fix, "applied by hand", "test")
	} else {
		_, err = w.st.CloseCase(w.ctx, w.c.ID, reason, into, "closed", "test")
	}
	if err != nil {
		return err
	}
	return SettleClosed(w.ctx, w.st, w.s.Dir, w.c.ID, "test")
}

// comeBack brings the closed case back in a new episode naming target, closing it first while it is open.
func (w *lifeWorld) comeBack(target string) error {
	if cur, _ := w.st.CaseByID(w.ctx, w.c.ID); cur.Status != store.CaseClosed {
		if err := w.closeCase(store.Solved, "", ""); err != nil {
			return err
		}
	}
	cur, _ := w.st.CaseByID(w.ctx, w.c.ID)
	if cur.Into != "" {
		return refused("the case went into another")
	}
	later := store.Event{Source: store.Transcripts, SourceKey: fmt.Sprintf("again@%s-%d", w.c.ID, len(w.steps)), TS: time.Now().Add(time.Minute), Session: "again",
		Harness: "claude", Actor: store.ActorTool, Kind: "tool_result", Tool: "Bash", Summary: "Exit code 1 zz-signal-text: the thing broke", Meta: `{"is_error":true}`}
	if _, err := w.st.Put(w.ctx, []store.Event{later}); err != nil {
		return err
	}
	back, _, err := w.st.ObserveCase(w.ctx, store.Finding{Family: cur.Family, Signature: cur.Signatures[0], Title: cur.Title,
		Evidence: []store.Evidence{{Ref: "ev:" + later.ID()}}}, "test")
	if err != nil {
		return err
	}
	mech, next := "the rule says the old thing", "say the new thing"
	c, err := w.st.UpdateCase(w.ctx, back.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test")
	if err != nil {
		return refused("the case did not come back: %v", err) // a close the new evidence does not reopen
	}
	w.c = c
	w.name(target)
	return nil
}

func (w *lifeWorld) load() (Package, bool) {
	p, ok, err := w.s.Load(w.c.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	return p, ok
}

// request is the pull request file the fake gh keeps for a URL, and its fields (state, repository, branch, closer).
func (w *lifeWorld) request(url string) (string, []string) {
	file := filepath.Join(w.ghState, "pr-"+url[strings.LastIndex(url, "/")+1:])
	raw, err := os.ReadFile(file)
	if err != nil {
		return file, nil
	}
	return file, strings.Fields(string(raw))
}

// personPushes adds a person's commit on top of a branch of a repository's origin, as a push to a pull request would.
func (w *lifeWorld) personPushes(origin, branch string) {
	t := w.t
	gitDir := "--git-dir=" + origin
	head := strings.TrimSpace(run(t, w.f.dir, "git", gitDir, "rev-parse", "refs/heads/"+branch))
	tree := strings.TrimSpace(run(t, w.f.dir, "git", gitDir, "rev-parse", head+"^{tree}"))
	commit := strings.TrimSpace(run(t, w.f.dir, "git", gitDir, "commit-tree", tree, "-p", head, "-m", "a person's own commit on the loop's pull request"))
	run(t, w.f.dir, "git", gitDir, "update-ref", "refs/heads/"+branch, commit)
	w.person[origin+" "+branch] = commit
	p, _ := w.load()
	w.personOf[origin+" "+branch] = p.Created
}

// the events: momentum's, and a person's or the world's (lifePeople)
var lifeEvents = []string{"owner prepare", "pass prepare", "build", "check", "propose", "review clean", "review blocking", "pass",
	"close and come back", "new episode, the same repository", "new episode, another repository", "new episode, a configuration file",
	"apply and close", "withdraw", "a person merges the request", "a person reopens the request", "a person closes the request",
	"a person pushes to the request", "the next create's response is lost", "gh fails", "gh works", "the case merges into another",
	"the owner edits the file", "the file is gone", "the file is back", "targets off", "targets on", "allow changes",
	"close, crashing before its settlement", "a person deletes the request's branch", "the second case names the file",
	"the second case builds", "dry pass", "a week passes", "a failed preparation", "a reader resolves the notes", "a landing",
	"the owner withdraws the package"}

func (w *lifeWorld) event(name string) error {
	t, ctx, id := w.t, w.ctx, w.c.ID
	p, had := w.load()
	switch name {
	case "owner prepare":
		_, err := w.s.Prepare(ctx, id, w.checkout())
		return err
	case "pass prepare":
		c, err := w.st.CaseByID(ctx, id)
		if err != nil {
			return err
		}
		if w.target == "config" {
			_, _, err = w.s.prepareBuild(ctx, c, KindConfig, ruleTarget, nil, "")
		} else {
			_, _, err = w.s.prepareBuild(ctx, c, KindRepo, w.checkout(), nil, "")
		}
		return err
	case "build": // the pass's own build path, from its inputs as a pass makes them
		c, err := w.st.CaseByID(ctx, id)
		if err != nil {
			return err
		}
		w.r.pass = &passInputs{}
		if w.target == "config" {
			if raw, err := os.ReadFile(filepath.Join(w.home, ruleTarget)); err == nil {
				w.r.pass.manifest.Configuration.Files = []ConfigFile{{Path: ruleTarget, Root: ".config/agents", SHA256: hashOf(raw)}}
			}
		} else {
			repo := map[string]string{"main": "agent-sh/fake", "other": "agent-sh/other"}[w.target]
			tip, lock, err := w.s.mirrorTip(ctx, repo, w.checkout())
			if lock != nil {
				lock.Close()
			}
			if err != nil {
				return err
			}
			w.r.pass.manifest.Repositories = []RepoInput{{Name: w.target, Source: w.checkout(), Branch: tip.branch, Commit: tip.commit, mirror: tip.dir, identity: tip.identity}}
		}
		w.runs++
		_, err = w.r.buildPackage(ctx, c, store.LoopRun{ID: fmt.Sprintf("life-build-%d", w.runs), Case: id, Dir: filepath.Join(w.r.Dir, "runs", fmt.Sprintf("life-build-%d", w.runs)), CaseEpisode: c.Episode})
		return err
	case "check":
		if !had || p.Work == "" || (p.State != Prepared && p.State != Checked && p.State != Revising) {
			return refused("nothing to check")
		}
		if p.Kind == KindConfig {
			write(t, filepath.Join(p.Work, ruleTarget), newRule)
			write(t, filepath.Join(p.Work, "tests/test_rules.py"), ruleTest)
			_, err := w.s.CheckArgs(ctx, id, []string{"python3", "tests/test_rules.py"}, nil, fixturePatch(t, w.s, id))
			return err
		}
		w.runs++ // each revision of the change is its own
		write(t, filepath.Join(p.Work, "fake.go"), fixed("42")+fmt.Sprintf("\n// revision %d\n", w.runs))
		write(t, filepath.Join(p.Work, "tests/test_answer.py"), "import sys\nsys.exit(0 if 'return 42' in open('fake.go').read() else 1)\n")
		_, err := w.s.CheckArgs(ctx, id, []string{"python3", "tests/test_answer.py"}, nil, fixturePatch(t, w.s, id))
		return err
	case "propose":
		if !had || p.State != Checked {
			return refused("nothing to propose")
		}
		pr := Proposal{Title: fmt.Sprintf("The answer, episode %d", w.c.Episode), Body: fmt.Sprintf("Episode %d's body.", w.c.Episode),
			Rollback: "revert the commit", Lesson: "The answer is 42.", Signal: goodSignal, Window: 7 * 24 * time.Hour}
		if p.Kind == KindConfig {
			pr = configProposal
		}
		_, err := proposePatch(t, w.s, ctx, id, pr)
		return err
	case "review clean", "review blocking":
		if !had || p.State != Proposed || p.Head == "" {
			return refused("nothing to review")
		}
		review := Review{Head: p.Head, Text: "Checked every changed file."}
		if name == "review blocking" {
			review.Findings = []Finding{{Severity: "medium", Where: "fake.go", Problem: "no comment", Fix: "add one"}}
		}
		if _, err := w.s.Reviewed(ctx, id, review); err != nil {
			return err
		}
		w.r.reviews(ctx, id)
		return nil
	case "pass":
		if _, _, _, err := w.r.pick(ctx, "failures", "", false); err != nil {
			return err
		}
		w.r.reviews(ctx, "")
		return w.r.noteWaiting(ctx)
	case "close and come back":
		return w.comeBack(w.target)
	case "new episode, the same repository":
		return w.comeBack("main")
	case "new episode, another repository":
		return w.comeBack("other")
	case "new episode, a configuration file":
		return w.comeBack("config")
	case "apply and close":
		c, err := w.st.CaseByID(ctx, id)
		if err != nil {
			return err
		}
		filed, ok, err := FiledConfigFix(w.s.Dir, c)
		if err != nil || !ok {
			return refused("no filed proposal")
		}
		w.agent = filepath.Join(w.home, ruleTarget)
		agentWrites(t, w.home, newRule)
		fix, err := ConfigFixNow(w.home, filed)
		if err != nil {
			return err
		}
		return w.closeCase(store.Solved, "", fix)
	case "withdraw":
		if had && p.Kind == KindConfig && p.State == Proposed {
			return w.closeCase(store.NotWorthIt, "", "")
		}
		return w.event("a person closes the request")
	case "a person merges the request", "a person reopens the request", "a person closes the request":
		if !had || p.PR == "" {
			return refused("no request")
		}
		file, f := w.request(p.PR)
		if f == nil {
			return refused("no request")
		}
		state := map[string]string{"a person merges the request": "MERGED", "a person reopens the request": "OPEN", "a person closes the request": "CLOSED"}[name]
		write(t, file, state+" "+f[1]+" "+f[2]+" person\n")
		return nil
	case "a person pushes to the request":
		if !had || p.PR == "" || p.Branch == "" {
			return refused("no request")
		}
		origin := w.f.origin
		if strings.EqualFold(p.Repo, "agent-sh/other") {
			origin = w.g.origin
		}
		if _, err := runQuiet(w.f.dir, "git", "--git-dir="+origin, "rev-parse", "--verify", "-q", "refs/heads/"+p.Branch); err != nil {
			return refused("no branch")
		}
		w.personPushes(origin, p.Branch)
		return nil
	case "the next create's response is lost":
		write(t, filepath.Join(w.ghState, "lose-create"), "")
		return nil
	case "gh fails":
		write(t, filepath.Join(w.ghState, "fail"), "")
		return nil
	case "gh works":
		os.Remove(filepath.Join(w.ghState, "fail"))
		return nil
	case "the case merges into another":
		return w.closeCase(store.Duplicate, w.twin.ID, "")
	case "the owner edits the file":
		w.agent = filepath.Join(w.home, ruleTarget)
		agentWrites(t, w.home, oldRule+ownerEdit)
		return nil
	case "the file is gone":
		w.agent = filepath.Join(w.home, ruleTarget)
		os.Rename(w.agent, w.agent+".away")
		return nil
	case "the file is back":
		w.agent = filepath.Join(w.home, ruleTarget)
		os.Rename(w.agent+".away", w.agent)
		return nil
	case "targets off":
		w.s.ConfigTargets.Enabled = false
		return nil
	case "targets on":
		w.s.ConfigTargets.Enabled = true
		return nil
	case "allow changes":
		if len(w.s.ConfigTargets.Allow) == 1 {
			w.s.ConfigTargets.Allow = []string{".config/agents/zz-other.md"}
		} else {
			w.s.ConfigTargets.Allow = []string{ruleTarget}
		}
		return nil
	case "a failed preparation": // the clone from the mirror fails once, as a full disk or a stopped pass would leave it
		if w.target == "config" {
			return refused("no mirror")
		}
		repo := map[string]string{"main": "agent-sh/fake", "other": "agent-sh/other"}[w.target]
		tip, lock, err := w.s.mirrorTip(ctx, repo, w.checkout())
		if lock != nil {
			lock.Close()
		}
		if err != nil {
			return err
		}
		objects := filepath.Join(tip.dir, "objects")
		if err := os.Rename(objects, objects+".away"); err != nil {
			return err
		}
		defer os.Rename(objects+".away", objects)
		c, err := w.st.CaseByID(ctx, id)
		if err != nil {
			return err
		}
		_, _, err = w.s.prepareBuild(ctx, c, KindRepo, w.checkout(), &tip, "")
		return err
	case "a reader resolves the notes":
		notes, err := w.st.Notes(ctx)
		if err != nil {
			return err
		}
		for _, n := range notes {
			if n.Done.IsZero() && strings.Contains(n.ID, store.IDPart(id)) {
				if _, _, err := w.st.DoneNote(ctx, n.ID, w.r.now()); err != nil {
					return err
				}
			}
		}
		return nil
	case "a landing": // the owner lands a proposal reviewed clean, while the loop is live
		if !had || p.Kind != KindRepo || p.State != Proposed || !p.Clean() {
			return refused("nothing to land")
		}
		w.s.Live = true
		defer func() { w.s.Live = false }()
		_, err := w.s.Land(ctx, id)
		return err
	case "the owner withdraws the package":
		_, err := w.s.Withdraw(ctx, id, "its fix moved")
		return err
	case "a person deletes the request's branch":
		if !had || p.Kind != KindRepo || p.Branch == "" {
			return refused("no branch")
		}
		origin := w.f.origin
		if strings.EqualFold(p.Repo, "agent-sh/other") {
			origin = w.g.origin
		}
		if _, err := runQuiet(w.f.dir, "git", "--git-dir="+origin, "update-ref", "-d", "refs/heads/"+p.Branch); err != nil {
			return refused("no branch")
		}
		w.person[origin+" "+p.Branch] = ""
		w.personOf[origin+" "+p.Branch] = p.Created
		return nil
	case "the second case names the file":
		raw, _ := json.Marshal(map[string]any{"repo": "", "config": ruleTarget, "episode": w.twin.Episode})
		return w.st.CaseNote(ctx, w.twin.ID, repoAction, string(raw), "fixture")
	case "the second case builds":
		twin, err := w.st.CaseByID(ctx, w.twin.ID)
		if err != nil {
			return err
		}
		_, _, err = w.s.prepareBuild(ctx, twin, KindConfig, ruleTarget, nil, "")
		return err
	case "dry pass": // a dry pass writes nothing anywhere and calls no gh
		before := w.written()
		if _, _, _, err := w.r.pick(ctx, "failures", "", true); err != nil {
			return err
		}
		if after := w.written(); after != before {
			t.Fatalf("steps %q: the dry pass wrote: %s", w.steps, firstDiff(before, after))
		}
		return nil
	case "a week passes":
		w.ahead += 8 * 24 * time.Hour
		return nil
	case "close, crashing before its settlement":
		if err := ClosingCase(ctx, w.st, id); err != nil {
			return err
		}
		if _, err := w.st.CloseCase(ctx, id, store.NotWorthIt, "", "declined", "test"); err != nil {
			return err
		}
		panic(errCrash) // the process dies between the close and its settlement
	}
	t.Fatalf("no event %q", name)
	return nil
}

// tree is what a step must leave as it was: the home and both checkouts' working trees, by path, mode and content.
func (w *lifeWorld) tree() map[string]string {
	out := map[string]string{}
	for _, root := range []string{w.home, w.f.main, w.other} {
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && d.Name() == ".git" {
				return filepath.SkipDir
			}
			info, err := os.Lstat(path)
			if err != nil {
				return nil
			}
			sum := ""
			if info.Mode().IsRegular() {
				raw, _ := os.ReadFile(path)
				h := sha256.Sum256(raw)
				sum = hex.EncodeToString(h[:])
			}
			out[path] = fmt.Sprintf("%v %d %s", info.Mode(), info.Size(), sum)
			return nil
		})
	}
	return out
}

// step runs an event, crashing (a panic) at the crashAt'th write when it is set, and checks every invariant after it.
func (w *lifeWorld) step(name string, crashAt int) {
	w.t.Helper()
	before := w.tree()
	w.crashAt, w.writes, w.agent = crashAt, 0, ""
	label := name
	if crashAt > 0 {
		label = fmt.Sprintf("%s, crashing at write %d", name, crashAt)
	}
	w.steps = append(w.steps, label)
	w.before, _ = w.load()
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				if r != errCrash {
					panic(r)
				}
				err = errCrash
			}
		}()
		return w.event(name)
	}()
	w.crashAt = 0
	if err != nil && !errors.Is(err, ErrRefused) && !held(err) && !errors.Is(err, errCrash) && !errors.Is(err, store.ErrBadCase) &&
		!strings.Contains(err.Error(), "network is down") && !strings.Contains(err.Error(), "response was lost") &&
		!strings.Contains(err.Error(), "(stale info)") && // a push the lease refused: the branch moved under the loop
		!(name == "a failed preparation" && strings.Contains(err.Error(), "clone failed")) {
		w.t.Fatalf("steps %q: %v", w.steps, err)
	}
	after := w.tree()
	for path, was := range before {
		if path != w.agent && path != w.agent+".away" && after[path] != was {
			w.t.Fatalf("steps %q: %s changed outside the loop's directories: %s -> %s", w.steps, path, was, after[path])
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok && path != w.agent && path != w.agent+".away" {
			w.t.Fatalf("steps %q: %s was written outside the loop's directories", w.steps, path)
		}
	}
	w.invariants()
}

// invariants are what every step leaves true of the case's packages, its log, its notes, the pull requests and their
// branches.
func (w *lifeWorld) invariants() {
	t, ctx := w.t, w.ctx
	t.Helper()
	p, had := w.load()
	// a package that has not ended is never replaced: the package before the step is the live one still, or it ended
	if w.before.Case != "" && !ended(w.before) && (!had || !p.Created.Equal(w.before.Created)) {
		t.Fatalf("steps %q: the %s package %s, made %s, gave way unended to %s %s made %s", w.steps, w.before.Kind, w.before.State,
			w.before.Created.Format(time.RFC3339Nano), p.Kind, p.State, p.Created.Format(time.RFC3339Nano))
	}
	// only a package that ended is archived
	for _, id := range []string{w.c.ID, w.twin.ID} {
		past, _ := filepath.Glob(filepath.Join(w.s.pkgDir(id), "past", "*", "package.json"))
		for _, file := range past {
			raw, _ := os.ReadFile(file)
			var old Package
			if json.Unmarshal(raw, &old) == nil && !ended(old) {
				t.Fatalf("steps %q: an archived package had not ended: %s %s at %s", w.steps, old.Kind, old.State, file)
			}
		}
	}
	// every open pull request is the live package's, in its repository: named by it, or at its branch (a create
	// whose response was lost); momentum closes none
	files, _ := filepath.Glob(filepath.Join(w.ghState, "pr-*"))
	for _, file := range files {
		if strings.HasSuffix(file, ".title") {
			continue
		}
		raw, _ := os.ReadFile(file)
		f := strings.Fields(string(raw))
		url := "https://github.com/" + f[1] + "/pull/" + strings.TrimPrefix(filepath.Base(file), "pr-")
		switch {
		case f[0] == "OPEN" && (!had || !strings.EqualFold(p.Repo, f[1]) || p.PR != url && p.Branch != f[2]):
			t.Fatalf("steps %q: the open pull request %s is no live package's (%s %s in %q, PR %q)", w.steps, url, p.Kind, p.State, p.Repo, p.PR)
		case f[0] == "CLOSED" && f[3] == "momentum":
			t.Fatalf("steps %q: momentum closed %s", w.steps, url)
		}
	}
	// the loop never moves a branch head it did not publish, nor brings back a branch a person deleted, while the package
	// whose branch it is lives; a package made after it ended reuses the case's branch, as every preparation does
	for key, head := range w.person {
		origin, branch, _ := strings.Cut(key, " ")
		if !had || !p.Created.Equal(w.personOf[key]) {
			continue
		}
		if got, _ := runQuiet(w.f.dir, "git", "--git-dir="+origin, "rev-parse", "--verify", "-q", "refs/heads/"+branch); strings.TrimSpace(got) != head {
			t.Fatalf("steps %q: the branch %s held a person's commit %s; it holds %q", w.steps, branch, head, strings.TrimSpace(got))
		}
	}
	for _, id := range []string{w.c.ID, w.twin.ID} {
		c, err := w.st.CaseByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		p, had, err := w.s.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		log, err := w.st.CaseLog(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		// a standing configuration note is a waiting configuration package's
		if n, ok := noteByID(t, w.st, configNoteID(c.ID)); ok && n.Done.IsZero() {
			if _, waits := w.s.configWait(c, p); !had || !waits {
				t.Fatalf("steps %q: case %s's configuration note stands for a package that waits on nobody (%s %s): %s", w.steps, id, p.Kind, p.State, plainClip(n.Title, 80))
			}
		}
		// the stage the log shows is the package's
		if target := store.ConfigOf(c, log); target != "" && (!had || p.Kind != KindConfig || p.Target != target || p.State != Proposed) {
			t.Fatalf("steps %q: case %s's log shows %s as proposed (configuration), the package is %s %s", w.steps, id, target, p.Kind, p.State)
		}
		// one review line per round of a package
		seen := map[string]bool{}
		for _, e := range log {
			key := ""
			if e.Action == "review posted" {
				key = "review " + e.Detail
			}
			if key == "" {
				continue
			}
			if seen[key] {
				t.Fatalf("steps %q: case %s's line twice: %s: %s", w.steps, id, e.Action, e.Detail)
			}
			seen[key] = true
		}
	}
	// the hold of a package of another place that lasted a week has its note raised once a pass has seen it, standing,
	// or resolved by its reader at that importance
	c, _ := w.st.CaseByID(ctx, w.c.ID)
	log, _ := w.st.CaseLog(ctx, w.c.ID)
	if strings.HasPrefix(lastOf(w.steps), "pass") {
		notes, _ := w.st.Notes(ctx)
		for _, n := range notes {
			if strings.HasPrefix(n.ID, holdNotePrefix(w.c.ID)) && strings.HasPrefix(n.Detail, unfinishedPrefix) && n.Detail == store.HoldOf(c, log) &&
				w.r.now().Sub(n.At) >= configRaise && n.Importance == store.ImportanceLow {
				t.Fatalf("steps %q: a week-old hold's note is not raised (done %v): %s", w.steps, !n.Done.IsZero(), n.Detail)
			}
		}
	}
}

// written is everything a step of momentum's could write, for a dry pass to leave as it was: the gh calls, both
// cases' logs and packages, and every note.
func (w *lifeWorld) written() string {
	var b strings.Builder
	calls, _ := os.ReadFile(filepath.Join(w.ghState, "calls"))
	b.Write(calls)
	for _, id := range []string{w.c.ID, w.twin.ID} {
		log, _ := w.st.CaseLog(w.ctx, id)
		for _, e := range log {
			fmt.Fprintf(&b, "log %s %s %s\n", id, e.Action, e.Detail)
		}
		raw, _ := os.ReadFile(filepath.Join(w.s.pkgDir(id), "package.json"))
		fmt.Fprintf(&b, "package %s %s\n", id, raw)
		c, _ := w.st.CaseByID(w.ctx, id)
		fmt.Fprintf(&b, "case %s %s %d\n", id, c.Status, c.Episode)
	}
	notes, _ := w.st.Notes(w.ctx)
	for _, n := range notes {
		fmt.Fprintf(&b, "note %s %v %d %s\n", n.ID, n.Done.IsZero(), n.Importance, n.Detail)
	}
	return b.String()
}

// firstDiff is the first line two texts differ at.
func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) || i < len(bl); i++ {
		if i >= len(al) || i >= len(bl) || al[i] != bl[i] {
			return fmt.Sprintf("line %d: %q, then %q", i, at(al, i), at(bl, i))
		}
	}
	return ""
}

func at(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return ""
}

// lifeStarts are the states the exhaustive pass starts from, each made by real steps.
var lifeStarts = map[string][]string{
	"no package":              nil,
	"repo checked":            {"owner prepare", "check"},
	"repo revising with a PR": {"owner prepare", "check", "propose", "review blocking"},
	"repo proposed with a PR": {"owner prepare", "check", "propose"},
	"config proposed":         {"new episode, a configuration file", "pass prepare", "check", "propose"},
	"config filed":            {"new episode, a configuration file", "pass prepare", "check", "propose", "review clean"},
}

// The lifecycle of a case's packages keeps its invariants after every step (lifeWorld.invariants): a package that has
// not ended is never replaced and only an ended one is archived, every open pull request is the live package's and
// momentum closes none, the loop never moves a branch head it did not publish while its package lives, a standing
// configuration note is a waiting package's, the stage the log shows is the package's, a review posts one line per
// round, a week-old hold's note is raised, and nothing is written outside the loop's directories. An exhaustive pass
// runs every event, and every crash point of the transitions that write, from each start state; seeded random runs go
// longer.
func TestAPackagesLifecycleKeepsItsInvariants(t *testing.T) {
	var starts []string
	for name := range lifeStarts {
		starts = append(starts, name)
	}
	slices.Sort(starts)
	reached := map[string]func(p Package) bool{
		"no package":              func(p Package) bool { return p.Case == "" },
		"repo checked":            func(p Package) bool { return p.Kind == KindRepo && p.State == Checked && p.PR == "" },
		"repo revising with a PR": func(p Package) bool { return p.Kind == KindRepo && p.State == Revising && p.PR != "" },
		"repo proposed with a PR": func(p Package) bool { return p.Kind == KindRepo && p.State == Proposed && p.PR != "" },
		"config proposed":         func(p Package) bool { return p.Kind == KindConfig && p.State == Proposed && p.Filed.IsZero() },
		"config filed":            func(p Package) bool { return p.Kind == KindConfig && p.State == Proposed && !p.Filed.IsZero() },
	}
	begin := func(t *testing.T, start string) *lifeWorld {
		w := newLifeWorld(t)
		for _, e := range lifeStarts[start] {
			w.step(e, 0)
		}
		if p, _ := w.load(); !reached[start](p) {
			t.Fatalf("the start %q was not reached: %s %s, PR %q", start, p.Kind, p.State, p.PR)
		}
		return w
	}
	for _, start := range starts {
		for _, e := range lifeEvents {
			t.Run(start+"/"+e, func(t *testing.T) {
				t.Parallel()
				w := begin(t, start)
				w.step(e, 0)
				w.step("gh works", 0)
				w.step("pass prepare", 0)
				w.step("pass", 0)
			})
		}
		// every crash point of a transition that writes, and the passes after it
		for _, e := range []string{"build", "pass", "review clean", "review blocking", "propose", "withdraw", "apply and close", "the owner withdraws the package"} {
			for _, before := range []string{"", "a person closes the request", "new episode, the same repository", "new episode, another repository",
				"new episode, a configuration file", "targets off"} {
				t.Run(start+"/crash/"+before+"/"+e, func(t *testing.T) {
					t.Parallel()
					// how many writes the transition makes, then a crash at each
					count := begin(t, start)
					if before != "" {
						count.step(before, 0)
					}
					count.crashAt, count.writes = 1<<30, 0
					func() {
						defer func() { recover() }()
						count.event(e)
					}()
					writes := count.writes
					count.crashAt = 0
					for k := 1; k <= writes; k++ {
						w := begin(t, start)
						if before != "" {
							w.step(before, 0)
						}
						w.step(e, k)
						w.step("pass", 0)
						w.step("pass prepare", 0)
						w.step("pass", 0)
					}
				})
			}
		}
	}
	for seed := uint64(1); seed <= 8; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewPCG(seed, 89))
			w := newLifeWorld(t)
			for i := 0; i < 24; i++ {
				crash := 0
				if rng.IntN(6) == 0 {
					crash = 1 + rng.IntN(4)
				}
				w.step(lifeEvents[rng.IntN(len(lifeEvents))], crash)
			}
		})
	}
}

// runQuiet runs a command and returns its standard output, with no test failure when it fails.
func runQuiet(dir string, args ...string) (string, error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}
