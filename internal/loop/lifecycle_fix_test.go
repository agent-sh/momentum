package loop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A person's commit on the loop's pull request stays: a case whose fix moves to another repository holds and publishes
// nothing, and a same-repository comeback, which keeps working the package, publishes nothing over it.
func TestAPersonsPushToTheLoopsRequestStays(t *testing.T) {
	for _, then := range [][]string{{"new episode, another repository"}, {"new episode, the same repository"},
		{"a person closes the request", "new episode, the same repository"}} {
		t.Run(strings.Join(then, ", "), func(t *testing.T) {
			w := newLifeWorld(t)
			steps := append(append([]string{"owner prepare", "check", "propose", "review blocking", "a person pushes to the request"}, then...),
				"build", "check", "propose", "pass", "build")
			for _, e := range steps {
				w.step(e, 0) // the invariants check the person's head after every step
			}
		})
	}
}

// A request whose creation went through while its response was lost is the package's all the same: the package stays
// while the case names another repository, and the next proposal finds and keeps the request.
func TestALostCreateIsStillThePackagesRequest(t *testing.T) {
	w := newLifeWorld(t)
	for _, e := range []string{"owner prepare", "check", "the next create's response is lost", "propose", "new episode, another repository", "build", "pass"} {
		w.step(e, 0)
	}
	if p, _ := w.load(); p.Kind != KindRepo || !strings.EqualFold(p.Repo, "agent-sh/fake") {
		t.Fatalf("the package of the lost request gave way: %s in %s", p.State, p.Repo)
	}
}

// A package prepared from a linked worktree of its repository is that repository's: the pass's build from the same
// source keeps it, with its reviews, rather than take it as stale and replace it.
func TestAWorktreeSourceKeepsItsPackage(t *testing.T) {
	ctx := context.Background()
	r, c, f := buildCase(t)
	wt := filepath.Join(f.dir, "wt")
	run(t, f.main, "git", "worktree", "add", "-q", "-b", "wt", wt)
	r.Inputs.ReadDirs = []string{wt}
	setCaseRepo(t, r.Store, c, "wt")
	newPass(t, r)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatal(err)
	}
	p, _, _ := r.Ship.Load(c.ID)
	p.State = Revising
	p.Reviews = []Review{{Round: 1, Head: "x", Medium: 1, Text: "a finding", Comment: "posted", At: time.Now()}}
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	newPass(t, r)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatalf("the build from the worktree it was prepared from: %v", err)
	}
	if again, _, _ := r.Ship.Load(c.ID); !again.Created.Equal(p.Created) || len(again.Reviews) != 1 {
		t.Fatalf("the worktree's package was replaced: %+v", again)
	}
}

// The filed note's line for a change made by hand quotes the file's path as a shell word: only the hash's command
// substitution stays in double quotes, so a name that holds shell syntax runs nothing when the line is pasted.
func TestTheByHandCloseLineQuotesItsPath(t *testing.T) {
	home := t.TempDir()
	s := &Ship{Dir: t.TempDir(), ConfigTargets: ConfigTargets{Enabled: true, Home: home}}
	target := ".config/agents/a$(touch${IFS}PWNED).md" // past the form's refusals, as a record may hold it
	c := store.Case{ID: "c0ffee", Title: "t", Status: store.CaseTesting, Episode: 1}
	p := Package{Case: c.ID, Kind: KindConfig, State: Proposed, Target: target, TargetBase: strings.Repeat("a", 64),
		TargetNew: strings.Repeat("b", 64), Head: strings.Repeat("c", 40), Filed: time.Now(), Episode: 1}
	w, ok := s.configWait(c, p)
	if !ok {
		t.Fatal("no wait")
	}
	_, rest, _ := strings.Cut(w.detail, "close with the hash it then holds: ")
	line, _, _ := strings.Cut(rest, " --why")
	_, word, _ := strings.Cut(line, "--fix ")
	dir := t.TempDir()
	cmd := exec.Command("bash", "-c", "printf '%s\\n' "+word)
	cmd.Dir = dir
	out, _ := cmd.Output()
	if _, err := os.Stat(filepath.Join(dir, "PWNED")); err == nil || !strings.HasPrefix(string(out), "config:~/"+target+"@") {
		t.Fatalf("the pasted line ran a command from the file's name, or named another file: %s", out)
	}
}

// A configuration path holds no character a shell reads inside double quotes, and no parenthesis.
func TestAConfigurationPathHoldsNoShellSyntax(t *testing.T) {
	for _, name := range []string{".config/agents/a$b.md", ".config/agents/a`b`.md", ".config/agents/a\"b.md", ".config/agents/a(b).md",
		".config/agents/a\\b.md", ".config/agents/a\nb.md"} {
		if err := store.CheckConfigPath(name); err == nil {
			t.Errorf("%q passed the configuration path's form", name)
		}
	}
}

// A crash at any write of a withdrawal leaves no note standing for a package that waits on nobody: the note is
// resolved first, then the line, then the state.
func TestACrashInAWithdrawalLeavesNoNoteStanding(t *testing.T) {
	ctx := context.Background()
	for _, point := range []string{"withdraw: note", "withdraw: line", "withdraw: saved"} {
		t.Run(point, func(t *testing.T) {
			r, c, _, _ := filedConfigCase(t)
			r.Ship.ConfigTargets.Enabled = false // the owner turns configuration targets off
			r.Ship.Crash = func(at string) error {
				if at == point {
					panic(errCrash)
				}
				return nil
			}
			func() {
				defer func() { recover() }()
				r.Ship.reconcileConfig(ctx, c.ID)
			}()
			r.Ship.Crash = nil
			p, _, _ := r.Ship.Load(c.ID)
			cur, _ := r.Store.CaseByID(ctx, c.ID)
			if n, ok := noteByID(t, r.Store, configNoteID(c.ID)); ok && n.Done.IsZero() {
				if _, waits := r.Ship.configWait(cur, p); !waits {
					t.Fatalf("after a crash at %s the note stands for a package that waits on nobody (%s)", point, p.State)
				}
			}
		})
	}
}

// A filed proposal's file that cannot be read as a target, for any reason the code has, still closes as already
// solved: the reference names the reason it could not be checked.
func TestEveryUncheckableFileHasACloseForm(t *testing.T) {
	home := fakeHome(t)
	file := filepath.Join(home, ruleTarget)
	for _, tc := range []struct{ how, reason string }{{"executable", "executable"}, {"hard link", "hard-link"}, {"link", "link"}} {
		t.Run(tc.how, func(t *testing.T) {
			switch tc.how {
			case "executable":
				chmod(t, file, 0o750)
				t.Cleanup(func() { chmod(t, file, 0o640) })
			case "hard link":
				if err := os.Link(file, file+"-zz-twin"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Remove(file + "-zz-twin") })
			case "link":
				if err := os.Rename(file, file+".kept"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(file+".kept", file); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Remove(file); os.Rename(file+".kept", file) })
			}
			fix, err := ConfigFixNow(home, "config:~/"+ruleTarget+"@"+strings.Repeat("a", 64))
			if err != nil || fix != "config:~/"+ruleTarget+"@unchecked:"+tc.reason || store.CheckConfigFix(fix) != nil {
				t.Fatalf("the close form of a file that is %s: %q %v", tc.how, fix, err)
			}
		})
	}
}

// A dry pass reports a waiting configuration proposal as a live pass does, and writes nothing.
func TestADryPassReportsAWaitingProposal(t *testing.T) {
	ctx := context.Background()
	r, c, _, _ := filedConfigCase(t)
	before, _ := r.Store.CaseLog(ctx, c.ID)
	_, _, dry, _ := r.pick(ctx, "failures", c.ID, true)
	after, _ := r.Store.CaseLog(ctx, c.ID)
	_, _, live, _ := r.pick(ctx, "failures", c.ID, false)
	if !strings.Contains(dry, store.ConfigStage) || dry != live || len(after) != len(before) {
		t.Fatalf("the dry pass: %q, the live one: %q; lines %d -> %d", dry, live, len(before), len(after))
	}
}

// Setup names every allow entry that is never a target, and the README's example of allow is a target.
func TestAnAllowEntryThatIsNeverATargetIsReported(t *testing.T) {
	ct := ConfigTargets{Allow: []string{ruleTarget, "~/.config/agents/notes/RULES.md", ".ssh/config"}}
	got := ct.RefusedAllow()
	if len(got) != 2 || !strings.Contains(got[0], "notes/RULES.md") || !strings.Contains(got[1], ".ssh/config") {
		t.Fatalf("the refused allow entries: %q", got)
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, _ := strings.Cut(string(readme), `"allow": ["`)
	example, _, _ := strings.Cut(rest, `"`)
	if example == "" || len((ConfigTargets{Allow: []string{example}}).RefusedAllow()) != 0 {
		t.Fatalf("the README's allow example %q is never a target", example)
	}
}

// A pull request is the package's from its creation: when a step after gh pr create fails (the scrubber here), the
// package still names the request it opened.
func TestARequestIsSavedTheMomentItIsCreated(t *testing.T) {
	w := newLifeWorld(t)
	for _, e := range []string{"owner prepare", "check"} {
		w.step(e, 0)
	}
	flag := filepath.Join(w.f.dir, "scrubber-down")
	scrub := script(t, w.f.dir, "scrub-zz", `[ -e "`+flag+`" ] && { echo "scrubber timed out" >&2; exit 1; }
exec sed -e 's/AKIA[A-Z0-9]*EXAMPLE/[masked]/g'`)
	saved := Scrubber
	Scrubber = []string{scrub}
	t.Cleanup(func() { Scrubber = saved })
	gh := w.s.GH
	w.s.GH = script(t, w.f.dir, "gh-zz", `out=$("`+gh+`" "$@"); st=$?; [ "$1 $2" = "pr create" ] && touch "`+flag+`"; printf '%s\n' "$out"; exit $st`)
	pr := Proposal{Title: "The answer", Body: "Body.", Rollback: "revert the commit", Lesson: "The answer is 42.", Signal: goodSignal, Window: 7 * 24 * time.Hour}
	if _, err := proposePatch(t, w.s, w.ctx, w.c.ID, pr); err == nil {
		t.Fatal("the proposal went through with the scrubber down")
	}
	os.Remove(flag)
	if p, _ := w.load(); p.PR != "https://github.com/agent-sh/fake/pull/7" {
		t.Fatalf("the package does not name the request it created: %q", p.PR)
	}
}

// A dry pass holds a case whose package works on another place without a gh call, a scope check or a busy check.
func TestADryPassHoldsWithoutGhOrChecks(t *testing.T) {
	w := newLifeWorld(t)
	for _, e := range []string{"owner prepare", "check", "propose", "review blocking", "new episode, another repository"} {
		w.step(e, 0)
	}
	busy := 0
	w.s.Gate.Busy = func(context.Context, string) (string, error) { busy++; return "", nil }
	guardLog := filepath.Join(w.f.dir, "guard-calls")
	guard := w.s.Gate.RepoGuard[0]
	w.s.Gate.RepoGuard = []string{script(t, w.f.dir, "guard-logging", `echo "$@" >> "`+guardLog+`"; exec "`+guard+`" "$@"`)}
	calls := filepath.Join(w.ghState, "calls")
	before, _ := os.ReadFile(calls)
	_, _, why, err := w.r.pick(w.ctx, "failures", "", true)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(calls)
	guarded, _ := os.ReadFile(guardLog)
	if !strings.Contains(why, unfinishedPrefix+"agent-sh/fake must end first") || len(after) != len(before) || len(guarded) > 0 || busy > 0 {
		t.Fatalf("the dry pass: %q; gh %q, the scope check %q, the busy check %d times", why, after[len(before):], guarded, busy)
	}
}

// A hold for a package of another place raises its note after a week of the hold, counting from its first note, as a
// base hold's note is raised: a standing low note is raised, and one its reader resolved low is put back raised, once;
// one resolved raised stays resolved.
func TestAWeekOldUnfinishedHoldIsRaised(t *testing.T) {
	for _, resolved := range []bool{false, true} {
		t.Run(map[bool]string{false: "standing", true: "resolved"}[resolved], func(t *testing.T) {
			w := newLifeWorld(t)
			for _, e := range []string{"owner prepare", "check", "propose", "review blocking", "new episode, another repository", "pass"} {
				w.step(e, 0)
			}
			note := func() store.Note {
				notes, _ := w.st.Notes(w.ctx)
				for _, n := range notes {
					if strings.HasPrefix(n.ID, holdNotePrefix(w.c.ID)) && strings.HasPrefix(n.Detail, unfinishedPrefix) {
						return n
					}
				}
				t.Fatal("no hold note")
				return store.Note{}
			}
			if n := note(); n.Importance != store.ImportanceLow || !n.Done.IsZero() {
				t.Fatalf("a new hold's note: %+v", n)
			}
			if resolved {
				w.step("a reader resolves the notes", 0)
			}
			w.ahead += 3 * 24 * time.Hour
			w.step("pass", 0)
			if n := note(); n.Importance != store.ImportanceLow || n.Done.IsZero() == resolved {
				t.Fatalf("a hold of three days: %+v", n)
			}
			w.ahead += 5 * 24 * time.Hour
			w.step("pass", 0)
			n := note()
			if n.Importance != store.ImportanceNormal || !n.Done.IsZero() || !strings.Contains(n.Title, "a week") {
				t.Fatalf("a week-old hold's note: %+v", n)
			}
			w.step("a reader resolves the notes", 0)
			w.step("pass", 0)
			if n := note(); n.Done.IsZero() {
				t.Fatalf("a raised note its reader resolved came back: %+v", n)
			}
		})
	}
}

// A file the close cannot read is recorded as unchecked, unreadable, whatever the file system's reason.
func TestAnUnreadableFileClosesAsUnchecked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	home := fakeHome(t)
	file := filepath.Join(home, ruleTarget)
	chmod(t, file, 0)
	t.Cleanup(func() { chmod(t, file, 0o640) })
	fix, err := ConfigFixNow(home, "config:~/"+ruleTarget+"@"+strings.Repeat("a", 64))
	if err != nil || fix != "config:~/"+ruleTarget+"@unchecked:unreadable" || store.CheckConfigFix(fix) != nil {
		t.Fatalf("the close form of an unreadable file: %q %v", fix, err)
	}
}

// A configuration proposal's review is posted to its case's log once per round: a second round whose review says the
// same is posted too, and a post that stopped before its save is not posted twice (the lifecycle's crash cells).
func TestEveryRoundOfAConfigurationReviewIsPosted(t *testing.T) {
	w := newLifeWorld(t)
	for _, e := range []string{"new episode, a configuration file", "pass prepare", "check", "propose", "review blocking"} {
		w.step(e, 0)
	}
	p, _ := w.load()
	if p.State != Revising {
		t.Fatalf("after a blocking review: %s", p.State)
	}
	// the revision that answers the review, then the same review again
	write(t, filepath.Join(p.Work, ruleTarget), newRule+"\nline e\n")
	if _, err := w.s.CheckArgs(w.ctx, w.c.ID, []string{"python3", "tests/test_rules.py"}, nil, fixturePatch(t, w.s, w.c.ID)); err != nil {
		t.Fatal(err)
	}
	for _, e := range []string{"propose", "review blocking"} {
		w.step(e, 0)
	}
	log, _ := w.st.CaseLog(w.ctx, w.c.ID)
	posted := 0
	for _, e := range log {
		if e.Action == "review posted" {
			posted++
		}
	}
	if p, _ := w.load(); posted != 2 || len(p.Reviews) != 2 {
		t.Fatalf("%d review lines for %d rounds", posted, len(p.Reviews))
	}
}
