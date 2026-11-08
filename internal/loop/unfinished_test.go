package loop

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A case whose package has not ended holds at selection while its investigation names another place, with a reason
// that reads nothing: no pass reads the package's request, a gh outage changes nothing, and the hold is one held line
// and one note however many passes see it. The package stays as it was.
func TestAPackageOfAnotherPlaceHoldsItsCaseWithoutAGhRead(t *testing.T) {
	for _, next := range []string{"new episode, another repository", "new episode, a configuration file"} {
		t.Run(next, func(t *testing.T) {
			w := newLifeWorld(t)
			for _, e := range []string{"owner prepare", "check", "propose", "review blocking", next} {
				w.step(e, 0)
			}
			old, _ := w.load()
			calls := filepath.Join(w.ghState, "calls")
			before, _ := os.ReadFile(calls)
			for _, e := range []string{"pass", "gh fails", "pass", "pass", "gh works", "pass"} {
				w.step(e, 0)
			}
			after, _ := os.ReadFile(calls)
			c, _ := w.st.CaseByID(w.ctx, w.c.ID)
			log, _ := w.st.CaseLog(w.ctx, w.c.ID)
			held := 0
			for _, e := range log {
				if e.Action == store.ActionHeld {
					held++
				}
			}
			notes, _ := w.st.Notes(w.ctx)
			holdNotes := 0
			for _, n := range notes {
				if strings.HasPrefix(n.ID, holdNotePrefix(w.c.ID)) {
					holdNotes++
				}
			}
			why := store.HoldOf(c, log)
			p, _ := w.load()
			if !strings.HasPrefix(why, unfinishedPrefix+old.Repo+" must end first") || !strings.Contains(why, "momentum ship withdraw --case "+w.c.ID) {
				t.Fatalf("the hold: %q", why)
			}
			if held != 1 || holdNotes != 1 || !bytes.Equal(before, after) {
				t.Fatalf("%d held lines and %d hold notes; the passes called gh: %q", held, holdNotes, after[len(before):])
			}
			if !p.Created.Equal(old.Created) || p.State != old.State || p.PR != old.PR {
				t.Fatalf("the package changed: %+v", p)
			}
		})
	}
}

// A same repository's comeback is not held: its build keeps working the package and its open request.
func TestASameRepositoryComebackKeepsWorkingItsPackage(t *testing.T) {
	w := newLifeWorld(t)
	for _, e := range []string{"owner prepare", "check", "propose", "review blocking", "new episode, the same repository"} {
		w.step(e, 0)
	}
	old, _ := w.load()
	c, _ := w.st.CaseByID(w.ctx, w.c.ID)
	if why, _ := w.r.heldTarget(w.ctx, c, "main", ""); why != "" {
		t.Fatalf("the same repository's comeback is held: %q", why)
	}
	if picked, step, _, err := w.r.pick(w.ctx, "failures", "", false); err != nil || picked.ID != w.c.ID || step != StepBuild {
		t.Fatalf("the comeback's pick: %s %s %v", picked.ID, step, err)
	}
	w.step("build", 0)
	if p, _ := w.load(); !p.Created.Equal(old.Created) || p.PR != old.PR {
		t.Fatalf("the comeback's build replaced the package: %+v", p)
	}
}

// The path a hold names ends the package: the owner's withdrawal refuses while its request is open, and says to close
// it; once a person closed it, the withdrawal ends the package and the next build prepares the fix where the
// investigation names it, archiving the withdrawn package. Nothing closes the request.
func TestTheOwnersWithdrawalEndsTheHeldPackage(t *testing.T) {
	for _, next := range []string{"new episode, another repository", "new episode, a configuration file"} {
		t.Run(next, func(t *testing.T) {
			w := newLifeWorld(t)
			for _, e := range []string{"owner prepare", "check", "propose", "review blocking", next, "pass"} {
				w.step(e, 0)
			}
			old, _ := w.load()
			if _, err := w.s.Withdraw(w.ctx, w.c.ID, "its fix moved"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), old.PR+" is open") {
				t.Fatalf("the withdrawal over an open request: %v", err)
			}
			w.step("a person closes the request", 0)
			w.step("the owner withdraws the package", 0)
			p, _ := w.load()
			log, _ := w.st.CaseLog(w.ctx, w.c.ID)
			if p.State != Withdrawn || p.PR != "" || !logHas(log, ActionWithdrawn, old.PR+" was closed without merging") {
				t.Fatalf("the withdrawn package: %+v", p)
			}
			// a package that ended is not withdrawn again
			if _, err := w.s.Withdraw(w.ctx, w.c.ID, "once more"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "ended already") {
				t.Fatalf("a second withdrawal: %v", err)
			}
			if again, _ := w.st.CaseLog(w.ctx, w.c.ID); len(again) != len(log) {
				t.Fatalf("a second withdrawal wrote %d lines", len(again)-len(log))
			}
			w.step("pass", 0)
			c, _ := w.st.CaseByID(w.ctx, w.c.ID)
			log, _ = w.st.CaseLog(w.ctx, w.c.ID)
			if why := store.HoldOf(c, log); why != "" {
				t.Fatalf("the case still holds: %q", why)
			}
			w.step("build", 0)
			p, _ = w.load()
			want := map[string]string{"new episode, another repository": "agent-sh/other", "new episode, a configuration file": ""}[next]
			if p.State != Prepared || p.Created.Equal(old.Created) || !strings.EqualFold(p.Repo, want) {
				t.Fatalf("the next build's package: %s %s in %q", p.Kind, p.State, p.Repo)
			}
			if _, err := os.Stat(filepath.Join(w.s.pkgDir(w.c.ID), "past", archiveName(old), "package.json")); err != nil {
				t.Fatalf("the withdrawn package is not archived: %v", err)
			}
			if calls, _ := os.ReadFile(filepath.Join(w.ghState, "calls")); strings.Contains(string(calls), "pr close") {
				t.Fatalf("the loop closed a request:\n%s", calls)
			}
		})
	}
}

// The owner's prepare over a package that has not ended, for another place, refuses with what ends it, and each path
// it names works: the withdrawal once the request is closed, after which the same prepare goes through. A landed
// change ends with its measurement or a rollback.
func TestTheOwnersPrepareNamesWhatEndsAPackage(t *testing.T) {
	w := newLifeWorld(t)
	for _, e := range []string{"owner prepare", "check", "propose", "review clean"} {
		w.step(e, 0)
	}
	_, err := w.s.Prepare(w.ctx, w.c.ID, w.other)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "momentum ship withdraw --case "+w.c.ID) || !strings.Contains(err.Error(), "momentum ship land "+w.c.ID) {
		t.Fatalf("the owner's prepare over an open proposal for another repository: %v", err)
	}
	if _, err := w.s.Prepare(w.ctx, w.c.ID, w.f.main); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "a package already") {
		t.Fatalf("the owner's prepare for the package's own repository: %v", err)
	}
	w.step("a person closes the request", 0)
	w.step("the owner withdraws the package", 0)
	p, err := w.s.Prepare(w.ctx, w.c.ID, w.other)
	if err != nil || !strings.EqualFold(p.Repo, "agent-sh/other") || p.State != Prepared {
		t.Fatalf("the owner's prepare after the withdrawal: %+v %v", p, err)
	}
	p.State = Landed
	if err := w.s.save(&p); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.Prepare(w.ctx, w.c.ID, w.f.main); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "measurement ends it") {
		t.Fatalf("the owner's prepare over a landed change: %v", err)
	}
}

// A preparation that fails after the package ended leaves the case workable: no read of a host clone it removed holds
// the case, and the next build prepares the package.
func TestAFailedPreparationLeavesTheCaseWorkable(t *testing.T) {
	w := newLifeWorld(t)
	for _, e := range []string{"owner prepare", "check", "propose", "review blocking", "a person closes the request", "new episode, another repository",
		"the owner withdraws the package", "a failed preparation", "pass"} {
		w.step(e, 0)
	}
	c, _ := w.st.CaseByID(w.ctx, w.c.ID)
	if why, _ := w.r.heldTarget(w.ctx, c, "other", ""); why != "" {
		t.Fatalf("after the failed preparation the case holds: %q", why)
	}
	w.step("build", 0)
	if p, _ := w.load(); !strings.EqualFold(p.Repo, "agent-sh/other") || p.State != Prepared {
		t.Fatalf("the build after the failed preparation: %s %s in %s", p.Kind, p.State, p.Repo)
	}
}

// A dry pass settles a configuration package as a live pass does, and writes nothing: a proposal the live pass
// withdraws (targets off, or a file that changed under it) is reported as the live pass reports it.
func TestADryPassSettlesAConfigurationPackageAsALivePassDoes(t *testing.T) {
	for _, how := range []string{"targets off", "the file changed"} {
		t.Run(how, func(t *testing.T) {
			ctx := context.Background()
			r, c, home, p := proposedConfigCase(t)
			switch how {
			case "targets off":
				r.Ship.ConfigTargets.Enabled = false
			case "the file changed":
				agentWrites(t, home, oldRule+ownerEdit)
			}
			state := func() string {
				log, _ := r.Store.CaseLog(ctx, c.ID)
				raw, _ := os.ReadFile(filepath.Join(r.Ship.pkgDir(c.ID), "package.json"))
				notes, _ := r.Store.Notes(ctx)
				var b strings.Builder
				for _, e := range log {
					b.WriteString(e.Action + " " + e.Detail + "\n")
				}
				for _, n := range notes {
					b.WriteString(n.ID + " " + n.Detail + " " + n.Done.String() + "\n")
				}
				return b.String() + string(raw)
			}
			before := state()
			dc, dstep, dry, err := r.pick(ctx, "failures", c.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			if after := state(); after != before {
				t.Fatalf("the dry pass wrote:\n%s", firstDiff(before, after))
			}
			lc, lstep, live, err := r.pick(ctx, "failures", c.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			if dc.ID != lc.ID || dstep != lstep || dry != live {
				t.Fatalf("the dry pass: %s %s %q; the live one: %s %s %q", dc.ID, dstep, dry, lc.ID, lstep, live)
			}
			if got := loadPackage(t, r.Ship, c.ID); got.State != Withdrawn || p.State != Proposed {
				t.Fatalf("the live pass did not withdraw the proposal: %s", got.State)
			}
		})
	}
}

// Two packages of a case made within one second keep two archives: an archive is named by its package's making to the
// nanosecond.
func TestTwoPackagesMadeInOneSecondKeepTwoArchives(t *testing.T) {
	st := testStore(t)
	s := &Ship{Store: st, Dir: t.TempDir()}
	at := time.Date(2026, 11, 8, 12, 0, 0, 100, time.UTC)
	for i, made := range []time.Time{at, at.Add(time.Millisecond)} {
		p := Package{Case: "c0ffee123", Kind: KindRepo, State: Withdrawn, Created: made, Repo: "agent-sh/fake", Head: strings.Repeat("ab", 20)[:39] + string(rune('0'+i))}
		if err := s.save(&p); err != nil {
			t.Fatal(err)
		}
		if err := s.archive(p); err != nil {
			t.Fatal(err)
		}
	}
	past, _ := filepath.Glob(filepath.Join(s.pkgDir("c0ffee123"), "past", "*", "package.json"))
	if len(past) != 2 {
		t.Fatalf("archives: %q", past)
	}
}
