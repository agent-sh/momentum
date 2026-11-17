package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	ciRunning = `{"total_count":1,"check_runs":[{"name":"build","status":"in_progress","conclusion":null,"app":{"slug":"github-actions"}}]}`
	ciFailed  = `{"total_count":2,"check_runs":[{"name":"build","status":"completed","conclusion":"failure","app":{"slug":"github-actions"}},{"name":"lint","status":"completed","conclusion":"success","app":{"slug":"github-actions"}}]}`
	ciBotOnly = `{"total_count":1,"check_runs":[{"name":"bot","status":"queued","conclusion":null,"app":{"slug":"some-bot"}}]}`
)

// A landing waits for the repository's own CI on the reviewed head: a check that has not finished or did not pass
// refuses it and nothing reaches the default branch; another app's check is not waited for; and in the loop's own
// repository a head no check has reported on does not land.
func TestALandingWaitsForTheRepositorysCI(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c, p := reviewedChange(t, s, st, f)
	s.Live = true
	s.Wait = func(context.Context, time.Duration) error {
		t.Error("the reviewed head's checks are read once: a landing does not wait on them")
		return nil
	}
	base := originHead(t, f, "main")
	checks := filepath.Join(f.dir, "checks")
	for want, body := range map[string]string{
		"has not finished":  ciRunning,
		"did not pass":      ciFailed,
		"could not be read": `not json`,
		"more checks than":  `{"total_count":101,"check_runs":[{"name":"build","status":"completed","conclusion":"success","app":{"slug":"github-actions"}}]}`,
	} {
		write(t, checks, body)
		if _, err := s.Land(ctx, c.ID); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), want) || originHead(t, f, "main") != base {
			t.Fatalf("checks %q: want a refusal with %q and nothing pushed: %v", body, want, err)
		}
		if kept, _, _ := s.Load(c.ID); kept.State != Proposed {
			t.Fatalf("checks %q: the package left the proposed state: %s", body, kept.State)
		}
	}
	// in the loop's own repository, a head with no check of its CI is not landed; elsewhere there is nothing to wait for
	write(t, checks, ciBotOnly)
	own := p
	own.Repo = SelfRepo
	if _, err := s.ciPassed(ctx, own, p.Head, nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "no check") {
		t.Fatalf("the loop's own repository with no CI check: %v", err)
	}
	if names, err := s.ciPassed(ctx, p, p.Head, nil); err != nil || len(names) != 0 {
		t.Fatalf("another app's unfinished check, and no CI of the repository's own: %v %v", names, err)
	}
	// its CI passed, and another app's check still waits: it lands
	write(t, checks, `{"total_count":2,"check_runs":[{"name":"build","status":"completed","conclusion":"success","app":{"slug":"github-actions"}},{"name":"bot","status":"queued","conclusion":null,"app":{"slug":"some-bot"}}]}`)
	if p, err := s.Land(ctx, c.ID); err != nil || p.State != Landed || originHead(t, f, "main") == base {
		t.Fatalf("land with CI passed: %+v %v", p, err)
	}
	calls, _ := os.ReadFile(filepath.Join(f.dir, "gh-calls"))
	if !strings.Contains(string(calls), "api repos/"+p.Repo+"/commits/"+p.Head+"/check-runs") {
		t.Fatalf("the checks were not asked for the reviewed head: %s", calls)
	}
}

// rebasedLanding is a reviewed change whose base moved, so its landing rebases: the reviewed head's CI passed, and
// every other commit's checks answer with body. waits counts the landing's pauses, each of which moves its clock.
func rebasedLanding(t *testing.T, body string) (s *Ship, f repoFixture, id string, p Package, waits *int) {
	t.Helper()
	st := testStore(t)
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
	return s, f, c.ID, p, &n
}

// The head a rebase made is not the head the CI read. A landing publishes it on the proposal branch and waits for
// its checks before the default branch takes it: while they run, nothing lands, and the wait ends. The next landing
// resumes from the journaled head, and reads its checks again before the push.
func TestARebasedHeadWaitsForItsOwnCI(t *testing.T) {
	ctx := context.Background()
	s, f, id, p, waits := rebasedLanding(t, ciRunning)
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
	// resumed with the checks still running: the journaled head does not reach the base
	if _, err := s.Land(ctx, id); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "has not finished") || originHead(t, f, "main") != base {
		t.Fatalf("a resumed landing whose head's CI still runs: %v", err)
	}
	// resumed with a check failed: refused, and not waited on
	*waits = 0
	write(t, filepath.Join(f.dir, "checks"), ciFailed)
	if _, err := s.Land(ctx, id); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "did not pass") || originHead(t, f, "main") != base || *waits != 0 {
		t.Fatalf("a resumed landing whose head's CI failed: %v (%d pauses)", err, *waits)
	}
	write(t, filepath.Join(f.dir, "checks"), passedChecks)
	landed, err := s.Land(ctx, id)
	if err != nil || landed.State != Landed || originHead(t, f, "main") != got.LandingHead {
		t.Fatalf("resumed with CI passed: %+v %v, main at %s", landed, err, originHead(t, f, "main"))
	}
}

// The checks of a rebased head that finish during the wait let the same landing through; a head with no check yet
// is waited for when the reviewed head had one (a new commit lists none for its first seconds); and a check that
// failed there refuses at once.
func TestARebasedHeadsCIEndsTheWait(t *testing.T) {
	ctx := context.Background()
	for name, first := range map[string]string{"running": ciRunning, "not reported": `{"total_count":0,"check_runs":[]}`} {
		s, f, id, _, waits := rebasedLanding(t, first)
		pause := s.Wait
		s.Wait = func(ctx context.Context, d time.Duration) error {
			if *waits == 1 {
				write(t, filepath.Join(f.dir, "checks"), passedChecks)
			}
			return pause(ctx, d)
		}
		base := originHead(t, f, "main")
		p, err := s.Land(ctx, id)
		if err != nil || p.State != Landed || originHead(t, f, "main") == base || *waits != 2 {
			t.Fatalf("%s, then passed: %+v %v (%d pauses)", name, p, err, *waits)
		}
	}
	s, f, id, _, waits := rebasedLanding(t, ciFailed)
	base := originHead(t, f, "main")
	if _, err := s.Land(ctx, id); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "did not pass") || originHead(t, f, "main") != base || *waits != 0 {
		t.Fatalf("the rebased head's CI failed: %v (%d pauses)", err, *waits)
	}
}
