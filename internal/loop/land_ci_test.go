package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// suites is a commit's check suites as the forge lists them: one of the repository's workflows for each
// status/conclusion pair given, beside an app that opened a suite and reported nothing.
func suites(states ...string) string {
	list := []string{`{"status":"queued","conclusion":null,"app":{"slug":"some-bot"}}`}
	for _, s := range states {
		status, conclusion, _ := strings.Cut(s, "/")
		if conclusion = `"` + conclusion + `"`; conclusion == `""` {
			conclusion = "null"
		}
		list = append(list, `{"status":"`+status+`","conclusion":`+conclusion+`,"app":{"slug":"github-actions"}}`)
	}
	return `{"total_count":` + string(rune('0'+len(list))) + `,"check_suites":[` + strings.Join(list, ",") + `]}`
}

var (
	ciRunning = suites("completed/success", "in_progress/")
	ciFailed  = suites("completed/failure", "completed/success")
	ciNone    = suites()
)

// A landing waits for the repository's own CI on the reviewed head: a workflow that has not finished or did not pass
// refuses it and nothing reaches the default branch; another app's suite is not waited for; and in the loop's own
// repository a head no workflow has reported on does not land.
func TestALandingWaitsForTheRepositorysCI(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c, p := reviewedChange(t, s, st, f)
	s.Live = true
	s.Wait = func(context.Context, time.Duration) error {
		t.Error("the reviewed head's checks are read once before the landing starts: nothing waits on them")
		return nil
	}
	base := originHead(t, f, "main")
	checks := filepath.Join(f.dir, "checks")
	for want, body := range map[string]string{
		"has not finished":  ciRunning,
		"did not pass":      ciFailed,
		"could not be read": `not json`,
		"more check suites": `{"total_count":101,"check_suites":[{"status":"completed","conclusion":"success","app":{"slug":"github-actions"}}]}`,
	} {
		write(t, checks, body)
		if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), want) || originHead(t, f, "main") != base {
			t.Fatalf("checks %q: want a refusal with %q and nothing pushed: %v", body, want, err)
		}
		if kept, _, _ := s.Load(c.ID); kept.State != Proposed {
			t.Fatalf("checks %q: the package left the proposed state: %s", body, kept.State)
		}
	}
	// what a suite of the repository's workflows can end as
	for state, want := range map[string]string{
		"completed/success": "", "completed/neutral": "", "completed/skipped": "",
		"completed/failure": "did not pass", "completed/cancelled": "did not pass", "completed/timed_out": "did not pass",
		"completed/action_required": "did not pass", "completed/stale": "did not pass", "completed/startup_failure": "did not pass", "completed/": "did not pass",
		"queued/": "has not finished", "in_progress/": "has not finished", "pending/": "has not finished",
	} {
		write(t, checks, suites(state))
		n, err := s.ciPassed(ctx, p, p.Head, false)
		if n != 1 || want == "" && err != nil || want != "" && (!errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), want)) {
			t.Errorf("a suite %s: %d suites, %v, want %q", state, n, err, want)
		}
		if w := (ciWaits{}); errors.As(err, &w) != (want == "has not finished") {
			t.Errorf("a suite %s: a refusal that time ends: %v", state, errors.As(err, &w))
		}
	}
	// a commit with no suite of the repository's workflows: nothing to wait for, unless the repository is known to
	// have CI (the loop's own, or one whose reviewed head had a suite)
	write(t, checks, ciNone)
	if n, err := s.ciPassed(ctx, p, p.Head, false); err != nil || n != 0 {
		t.Fatalf("another app's suite alone: %v %v", n, err)
	}
	own := p
	own.Repo = SelfRepo
	for name, err := range map[string]error{"the loop's own repository": second(s.ciPassed(ctx, own, p.Head, false)), "a repository with CI": second(s.ciPassed(ctx, p, p.Head, true))} {
		if w := (ciWaits{}); !errors.As(err, &w) || !strings.Contains(err.Error(), "has not reported") {
			t.Fatalf("%s, and a commit with no suite: %v", name, err)
		}
	}
	write(t, checks, suites("completed/skipped", "completed/success"))
	if n, err := s.ciPassed(ctx, own, p.Head, true); err != nil || n != 2 {
		t.Fatalf("the loop's own repository with two suites passed: %v %v", n, err)
	}
	// a closed request is settled whatever its CI says: a red request is what a person closes
	write(t, checks, ciFailed)
	write(t, filepath.Join(f.dir, "pr-closed"), "")
	if got, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "was closed") || got.State != Checked || got.ClosedProposals != 1 {
		t.Fatalf("a closed request whose CI failed: %+v %v", got, err)
	}
}

// A reviewed head whose CI passed lands, whatever another app's suite says, and the read is of that head.
func TestALandingReadsTheReviewedHeadsCI(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c, p := reviewedChange(t, s, st, f)
	s.Live = true
	s.Wait = func(context.Context, time.Duration) error {
		t.Error("the reviewed head has been listed since its review: one read, and no pause")
		return nil
	}
	base := originHead(t, f, "main")
	checks := filepath.Join(f.dir, "checks")
	// its CI passed, and another app's suite still waits: it lands
	write(t, checks, suites("completed/success", "completed/skipped"))
	if p, err := s.Land(ctx, c.ID); err != nil || p.State != Landed || originHead(t, f, "main") == base {
		t.Fatalf("land with CI passed: %+v %v", p, err)
	}
	calls, _ := os.ReadFile(filepath.Join(f.dir, "gh-calls"))
	if !strings.Contains(string(calls), "api repos/"+p.Repo+"/commits/"+p.Head+"/check-suites") {
		t.Fatalf("the checks were not asked for the reviewed head: %s", calls)
	}
}

func second(_ int, err error) error { return err }

// rebasedLanding is a reviewed change whose base moved, so its landing rebases: the reviewed head's CI passed, and
// every other commit's checks answer with body. waits counts the landing's pauses, each of which moves its clock.
func rebasedLanding(t *testing.T, body string) (s *Ship, st *store.Store, f repoFixture, id string, p Package, waits *int) {
	t.Helper()
	st = testStore(t)
	f = newRepo(t, nil)
	s = shipFor(t, st, f)
	c, p := reviewedChange(t, s, st, f)
	s.Live = true
	pushOther(t, f, "main", map[string]string{"README": "a readme\n"})
	write(t, filepath.Join(f.dir, "checks-"+p.Head), passedChecks)
	write(t, filepath.Join(f.dir, "checks"), body)
	start, n := s.now(), 0
	s.Now = func() time.Time { return start.Add(time.Duration(n) * ciPoll) }
	s.Wait = func(_ context.Context, d time.Duration) error {
		if d != ciPoll {
			t.Errorf("a pause of %s", d)
		}
		n++
		return nil
	}
	return s, st, f, c.ID, p, &n
}

// The head a rebase made is not the head the CI read. A landing publishes it on the proposal branch and waits for
// its workflows before the default branch takes it: while they run, nothing lands, and the wait ends. The next
// landing resumes from the journaled head, and reads its checks again before the push.
func TestARebasedHeadWaitsForItsOwnCI(t *testing.T) {
	ctx := context.Background()
	s, _, f, id, p, waits := rebasedLanding(t, ciRunning)
	base := originHead(t, f, "main")
	got, err := s.Land(ctx, id)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "has not finished") || originHead(t, f, "main") != base {
		t.Fatalf("the rebased head's CI still runs: want a refusal and nothing pushed: %v", err)
	}
	if want := int(ciWait / ciPoll); *waits != want {
		t.Fatalf("the landing paused %d times, want %d: the wait is bounded", *waits, want)
	}
	if got.State != Landing || got.LandingHead == "" || got.LandingHead == p.Head || originHead(t, f, p.Branch) != got.LandingHead {
		t.Fatalf("the rebased head is journaled and on the proposal branch: %+v, branch at %s", got, originHead(t, f, p.Branch))
	}
	// resumed: the journaled head does not reach the base while its workflows run, have not reported (the reviewed
	// head had one, so the repository has CI), or failed; and a resumed landing does not wait for them
	for want, body := range map[string]string{"has not finished": ciRunning, "has not reported": ciNone, "did not pass": ciFailed} {
		*waits = 0
		write(t, filepath.Join(f.dir, "checks"), body)
		_, err := s.Land(ctx, id)
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), want) || originHead(t, f, "main") != base || *waits != 0 {
			t.Fatalf("a resumed landing, checks %s: %v (%d pauses)", body, err, *waits)
		}
	}
	// passed: the list of a head a rebase made is read twice, a pause apart, before the push
	write(t, filepath.Join(f.dir, "checks"), passedChecks)
	landed, err := s.Land(ctx, id)
	if err != nil || landed.State != Landed || originHead(t, f, "main") != got.LandingHead || *waits != 1 {
		t.Fatalf("resumed with CI passed: %+v %v, main at %s (%d pauses)", landed, err, originHead(t, f, "main"), *waits)
	}
}

// The workflows of a rebased head that finish during the wait let the same landing through; a head with no suite
// yet is waited for when the reviewed head had one; and one that failed there refuses at once.
func TestARebasedHeadsCIEndsTheWait(t *testing.T) {
	ctx := context.Background()
	for name, first := range map[string]string{"running": ciRunning, "not reported": ciNone} {
		s, _, f, id, _, waits := rebasedLanding(t, first)
		pause := s.Wait
		s.Wait = func(ctx context.Context, d time.Duration) error {
			if *waits == 1 {
				write(t, filepath.Join(f.dir, "checks"), passedChecks)
			}
			return pause(ctx, d)
		}
		base := originHead(t, f, "main")
		p, err := s.Land(ctx, id)
		if err != nil || p.State != Landed || originHead(t, f, "main") == base || *waits != 3 {
			t.Fatalf("%s, then passed: %+v %v (%d pauses)", name, p, err, *waits)
		}
	}
	s, _, f, id, _, waits := rebasedLanding(t, ciFailed)
	base := originHead(t, f, "main")
	if _, err := s.Land(ctx, id); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "did not pass") || originHead(t, f, "main") != base || *waits != 0 {
		t.Fatalf("the rebased head's CI failed: %v (%d pauses)", err, *waits)
	}
	// A new commit lists its workflows one after another. A rebased head whose first read shows one workflow,
	// skipped at once, is not through: the next read shows the second running, and the head lands only after two
	// reads that list the same suites, all passed.
	s, _, f, id, _, waits = rebasedLanding(t, suites("completed/skipped"))
	pause := s.Wait
	s.Wait = func(ctx context.Context, d time.Duration) error {
		switch *waits {
		case 0:
			write(t, filepath.Join(f.dir, "checks"), suites("completed/skipped", "in_progress/"))
		case 2:
			write(t, filepath.Join(f.dir, "checks"), suites("completed/skipped", "completed/success"))
		}
		return pause(ctx, d)
	}
	base = originHead(t, f, "main")
	if got, err := s.Land(ctx, id); err != nil || got.State != Landed || originHead(t, f, "main") == base || *waits != 4 {
		t.Fatalf("one workflow listed, then two: %+v %v (%d pauses)", got, err, *waits)
	}
	// a read that stops passing starts the count again: passed, running, then two passing reads
	s, _, f, id, _, waits = rebasedLanding(t, passedChecks)
	pause = s.Wait
	s.Wait = func(ctx context.Context, d time.Duration) error {
		switch *waits {
		case 0:
			write(t, filepath.Join(f.dir, "checks"), ciRunning)
		case 1:
			write(t, filepath.Join(f.dir, "checks"), passedChecks)
		}
		return pause(ctx, d)
	}
	if got, err := s.Land(ctx, id); err != nil || got.State != Landed || *waits != 3 {
		t.Fatalf("passed, running, passed: %+v %v (%d pauses)", got, err, *waits)
	}
	// and a list that grows between two passing reads is read once more
	s, _, f, id, _, waits = rebasedLanding(t, suites("completed/skipped"))
	pause = s.Wait
	s.Wait = func(ctx context.Context, d time.Duration) error {
		if *waits == 0 {
			write(t, filepath.Join(f.dir, "checks"), suites("completed/skipped", "completed/success"))
		}
		return pause(ctx, d)
	}
	if got, err := s.Land(ctx, id); err != nil || got.State != Landed || *waits != 2 {
		t.Fatalf("a list that grew between two passing reads: %+v %v (%d pauses)", got, err, *waits)
	}
}

// A request a person merged is not the loop's to land or to refuse on its CI: the landing finds the reviewed head
// on the base and finishes, as it did before the CI was read.
func TestARequestAPersonMergedIsFinished(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c, p := reviewedChange(t, s, st, f)
	s.Live = true
	run(t, p.Host, "git", "push", "-q", "origin", p.Head+":refs/heads/main")
	write(t, filepath.Join(f.dir, "pr-merged"), "")
	write(t, filepath.Join(f.dir, "checks"), ciFailed)
	if got, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "not open") || got.LandingHead != p.Head {
		t.Fatalf("the first landing after a person's merge: %+v %v", got, err)
	}
	if got, err := s.Land(ctx, c.ID); err != nil || got.State != Landed {
		t.Fatalf("the landing that finds its head on the base: %+v %v", got, err)
	}
}

// A closed pull request gets no run on the head a rebase publishes. The landing reads the request before it waits,
// so the closure is seen and settled, on a new landing and on a resumed one, and nothing is waited for.
func TestAClosedRequestIsSeenBeforeTheWaitForCI(t *testing.T) {
	ctx := context.Background()
	s, _, f, id, reviewed, waits := rebasedLanding(t, ciNone)
	write(t, filepath.Join(f.dir, "pr-closed"), "")
	base := originHead(t, f, "main")
	p, err := s.Land(ctx, id)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "was closed") || p.State != Checked || p.ClosedProposals != 1 || *waits != 0 || originHead(t, f, "main") != base {
		t.Fatalf("a closed request, base moved: %+v %v (%d pauses)", p, err, *waits)
	}
	// seen at the landing's first read: nothing was rebased or published for it
	if at := originHead(t, f, reviewed.Branch); at != reviewed.Head {
		t.Fatalf("the proposal branch moved from the reviewed head %s to %s", reviewed.Head, at)
	}
	if kept, _, _ := s.Load(id); kept.State != Checked || kept.LandingHead != "" {
		t.Fatalf("the package after the closure: %+v", kept)
	}
	// closed while a journaled head waited for its CI
	s, _, f, id, _, waits = rebasedLanding(t, ciRunning)
	if _, err := s.Land(ctx, id); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "has not finished") {
		t.Fatalf("setup: %v", err)
	}
	*waits = 0
	write(t, filepath.Join(f.dir, "pr-closed"), "")
	if p, err := s.Land(ctx, id); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "was closed") || p.State != Checked || p.ClosedProposals != 1 || *waits != 0 {
		t.Fatalf("a request closed while its head waited: %+v %v (%d pauses)", p, err, *waits)
	}
}

// A case that leaves testing while its landing waits for the CI is not landed when the CI passes.
func TestACaseThatLeftTestingDuringTheWaitDoesNotLand(t *testing.T) {
	ctx := context.Background()
	s, st, f, id, _, waits := rebasedLanding(t, ciRunning)
	pause := s.Wait
	s.Wait = func(ctx context.Context, d time.Duration) error {
		if *waits == 0 {
			if _, err := st.UpdateCase(ctx, id, store.CaseUpdate{Status: store.CaseOpen}, "fixture"); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(f.dir, "checks"), passedChecks)
		}
		return pause(ctx, d)
	}
	base := originHead(t, f, "main")
	if _, err := s.Land(ctx, id); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "testing") || originHead(t, f, "main") != base {
		t.Fatalf("the case left testing during the wait: %v", err)
	}
}

// The pause between two reads is a timer that the landing's context ends.
func TestTheWaitBetweenReadsEndsWithItsContext(t *testing.T) {
	s := &Ship{}
	if err := s.wait(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.wait(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled wait: %v", err)
	}
}

// prViewAfter is a forge whose pull request answers as the fixture's for its first n reads, and is closed after.
func prViewAfter(t *testing.T, f repoFixture, n int) string {
	t.Helper()
	count := filepath.Join(t.TempDir(), "count")
	return script(t, t.TempDir(), "gh", `if [ "$1 $2" = "pr view" ]; then
  n=$(cat "`+count+`" 2>/dev/null || echo 0); echo $((n+1)) > "`+count+`"
  if [ "$n" -ge `+fmt.Sprint(n)+` ]; then echo '{"state":"CLOSED"}'; exit 0; fi
fi
exec `+f.gh+` "$@"`)
}

// A request closed after the landing's first read of it is settled at the read before the wait for the CI, in a
// landing that did not rebase and in one that did: the package is put back as a closed proposal's, and stays so.
func TestARequestClosedDuringALandingIsSettled(t *testing.T) {
	ctx := context.Background()
	settled := func(t *testing.T, s *Ship, f repoFixture, id, base string, err error) {
		t.Helper()
		kept, _, _ := s.Load(id)
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "was closed") || kept.State != Checked || kept.PR != "" || kept.LandingHead != "" ||
			kept.ClosedProposals != 1 || len(kept.Reviews) != 0 || originHead(t, f, "main") != base {
			t.Fatalf("closed during the landing: %+v %v", kept, err)
		}
	}
	t.Run("no rebase", func(t *testing.T) {
		st := testStore(t)
		f := newRepo(t, nil)
		s := shipFor(t, st, f)
		c, _ := reviewedChange(t, s, st, f)
		s.Live = true
		s.GH = prViewAfter(t, f, 1)
		base := originHead(t, f, "main")
		_, err := s.Land(ctx, c.ID)
		settled(t, s, f, c.ID, base, err)
	})
	t.Run("rebase", func(t *testing.T) {
		s, _, f, id, _, waits := rebasedLanding(t, passedChecks)
		s.GH = prViewAfter(t, f, 1)
		base := originHead(t, f, "main")
		_, err := s.Land(ctx, id)
		settled(t, s, f, id, base, err)
		if *waits != 0 {
			t.Fatalf("%d pauses for a closed request", *waits)
		}
	})
	// closed during the wait for a rebased head's CI, which then passes: the read before the push settles it
	t.Run("during the wait", func(t *testing.T) {
		s, _, f, id, _, waits := rebasedLanding(t, ciRunning)
		pause := s.Wait
		s.Wait = func(ctx context.Context, d time.Duration) error {
			if *waits == 0 {
				write(t, filepath.Join(f.dir, "pr-closed"), "")
				write(t, filepath.Join(f.dir, "checks"), passedChecks)
			}
			return pause(ctx, d)
		}
		base := originHead(t, f, "main")
		_, err := s.Land(ctx, id)
		settled(t, s, f, id, base, err)
	})
}

// A first read of the request that fails stops nothing and lets nothing through: the landing goes on, and the CI is
// read before the push all the same.
func TestAFailedFirstReadOfTheRequestStillReadsTheCI(t *testing.T) {
	ctx := context.Background()
	for name, body := range map[string]string{"passed": passedChecks, "failed": ciFailed} {
		st := testStore(t)
		f := newRepo(t, nil)
		s := shipFor(t, st, f)
		c, _ := reviewedChange(t, s, st, f)
		s.Live = true
		write(t, filepath.Join(f.dir, "checks"), body)
		count := filepath.Join(t.TempDir(), "count")
		s.GH = script(t, t.TempDir(), "gh", `if [ "$1 $2" = "pr view" ] && [ ! -e "`+count+`" ]; then : > "`+count+`"; echo "gh: network down" >&2; exit 1; fi
exec `+f.gh+` "$@"`)
		base := originHead(t, f, "main")
		_, err := s.Land(ctx, c.ID)
		kept, _, _ := s.Load(c.ID)
		if name == "passed" && (err != nil || kept.State != Landed) {
			t.Errorf("passed: %+v %v", kept, err)
		}
		if name == "failed" && (!errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "did not pass") || originHead(t, f, "main") != base) {
			t.Errorf("failed: %+v %v", kept, err)
		}
	}
}
