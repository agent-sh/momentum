package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/measure"
	"github.com/agent-sh/momentum/internal/store"
)

func TestMeasurePreservesARollbackJournalStartedDuringThePass(t *testing.T) {
	m := measuredCase(t)
	fired := false
	m.r.Now = func() time.Time {
		if !fired {
			fired = true
			p, _, err := m.r.Ship.Load(m.c.ID)
			if err != nil {
				t.Fatal(err)
			}
			p.State, p.Undo = RollingBack, &RollbackJournal{Head: p.Landed[0], Why: "synthetic rollback"}
			if err := m.r.Ship.save(&p); err != nil {
				t.Fatal(err)
			}
		}
		return m.clock
	}
	m.measure(t, Options{})
	p, _, err := m.r.Ship.Load(m.c.ID)
	if err != nil || p.State != RollingBack || p.Undo == nil || p.Undo.Why != "synthetic rollback" {
		t.Fatalf("the journal of a rollback begun during the pass was overwritten: state %q, journal %+v, error %v", p.State, p.Undo, err)
	}
	if m.theCase(t).Status != store.CaseDeployed {
		t.Fatal("the journaled change started measuring")
	}
}

func TestKeepingAChangeTakesTheCaseLock(t *testing.T) {
	m := measuredCase(t)
	ctx := context.Background()
	unlock, err := m.r.Ship.lock(ctx, m.c.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := m.r.Ship.Keep(m.c.ID, measure.Result{}); !held(err) {
		t.Fatalf("keeping a change bypassed its case lock: %v", err)
	}
	p, _, err := m.r.Ship.Load(m.c.ID)
	if err != nil || p.State != Landed {
		t.Fatalf("a held keep changed the package: %q %v", p.State, err)
	}
}

func TestMeasureAndWatchHoldTheCaseLock(t *testing.T) {
	for _, watch := range []bool{false, true} {
		t.Run(map[bool]string{false: "measure", true: "watch"}[watch], func(t *testing.T) {
			m := measuredCase(t)
			if watch {
				m.measure(t, Options{})
				hours(t, m.st, "after", m.theCase(t).Deployed, 24*time.Hour, 100, 0)
				m.clock = m.clock.Add(25 * time.Hour)
				if got := m.measure(t, Options{}); got.Status != "improved" {
					t.Fatalf("fixture did not close: %+v", got)
				}
				m.clock = m.clock.Add(25 * time.Hour)
			}
			ctx := context.Background()
			unlock, err := m.r.Ship.lock(ctx, m.c.ID)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				got := m.measure(t, Options{})
				if got.Status != "held" || !strings.Contains(got.Why, "another ship step") {
					t.Fatalf("a busy case was measured: %+v", got)
				}
			}
			holdNotes := func() []store.Note {
				t.Helper()
				var out []store.Note
				for _, n := range m.notes(t) {
					if strings.HasPrefix(n.ID, "loop-measure:") {
						out = append(out, n)
					}
				}
				return out
			}
			ns := holdNotes()
			if len(ns) != 1 || ns[0].Importance != store.ImportanceLow {
				t.Fatalf("hold needs one passive note: %+v", ns)
			}
			if _, _, err := m.st.DoneNote(ctx, ns[0].ID, m.clock); err != nil {
				t.Fatal(err)
			}
			m.measure(t, Options{})
			if ns := holdNotes(); len(ns) != 0 {
				t.Fatalf("resolved cause was repeated: %+v", ns)
			}
			unlock()
			calls := 0
			m.r.Now = func() time.Time {
				calls++
				// The first clock read precedes loading the package. Later reads belong to the locked operation.
				if calls > 1 {
					free, err := m.r.Ship.lock(ctx, m.c.ID)
					if err == nil {
						free()
						t.Fatal("the measure operation released its case lock")
					}
					if !held(err) {
						t.Fatal(err)
					}
				}
				return m.clock
			}
			m.r.Measure(ctx, Options{})
			if calls < 2 {
				t.Fatal("fixture never reached a clock read during the operation")
			}
		})
	}
}

func TestOwnerHourHitsUseThePlansActor(t *testing.T) {
	ctx := context.Background()
	st, _ := testStoreAt(t)
	now := time.Now().Truncate(time.Hour)
	sig := Signal{Source: store.Transcripts, Kinds: []string{"text"}, Match: []string{"matching"}, Harness: "claude"}
	evs := []store.Event{
		{Source: store.Transcripts, SourceKey: "owner", TS: now, Session: "owner", Harness: "claude", Actor: store.ActorOwner, Kind: "text", Summary: "matching"},
		{Source: store.Transcripts, SourceKey: "agent", TS: now, Session: "agent", Harness: "claude", Actor: store.ActorAgent, Kind: "text", Summary: "matching"},
	}
	if _, err := st.Put(ctx, evs); err != nil {
		t.Fatal(err)
	}
	want := measure.Counts{Hits: 1, Base: 1}
	if got, err := Tally(ctx, st, sig, OwnerHours, now, now.Add(time.Hour), ""); err != nil || got != want {
		t.Fatalf("owner hours counted another actor: %+v %v", got, err)
	}
	if got, err := tallyPlan(ctx, st, Plan{Signal: sig, Units: OwnerHours}, now, now.Add(time.Hour), ""); err != nil || got != want {
		t.Fatalf("a later plan window counted another actor: %+v %v", got, err)
	}
	// Hits from a reader omitted from the base still supply their own activity. Events without a session supply none.
	evs = append(evs[:1], store.Event{Source: store.Capacity, TS: now, Kind: "text", Summary: "matching"})
	if got, err := tallySources(ctx, st, sig, SessionHours, evs, now, now.Add(time.Hour), "", []string{store.Capacity}); err != nil || got != want {
		t.Fatalf("hit activity or the sessionless skip was lost: %+v %v", got, err)
	}
}

func TestInstalledFileRetriesATransientCheckoutError(t *testing.T) {
	m := measuredCase(t)
	p, _, _ := m.r.Ship.Load(m.c.ID)
	file := filepath.Join(t.TempDir(), "installed.txt")
	write(t, file, "installed change")
	built := time.Now().Add(time.Hour)
	if err := os.Chtimes(file, built, built); err != nil {
		t.Fatal(err)
	}
	p.Plan.Installed, p.CheckoutAt = file, m.deployed
	p.Dir = t.TempDir()
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if got := m.measure(t, Options{}); got.Status != "waiting" || !strings.Contains(got.Why, "cannot check") {
		t.Fatalf("fixture did not meet a transient checkout error: %+v", got)
	}
	p, _, _ = m.r.Ship.Load(m.c.ID)
	p.Dir = m.repo
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if got := m.measure(t, Options{}); got.Status != "measuring" {
		t.Fatalf("the checkout recovered but its build stayed rejected: %+v", got)
	}
}

func TestWaitingProposalHoldsThePaidBuild(t *testing.T) {
	ctx := context.Background()
	r, c := inputRunner(t)
	cause := &waitingPlan{sources: []string{store.Transcripts}}
	for i := 0; i < 2; i++ {
		run, why := r.reserve(ctx, c.Family, c.ID, StepBuild)
		if why != "" {
			t.Fatal(why)
		}
		res, err := stageFailure(Result{Started: true, CostKnown: true, Status: store.RunOK,
			RelayAdmitted: 1, RelayCharged: 1, RelayUSD: .032, Tokens: 12, Turns: 1}, "build propose", cause)
		done := r.completeStep(ctx, c, run, res, err)
		if done.Run.ID != "" || done.Skipped == "" {
			t.Fatalf("waiting proposal became a failed build: %+v", done)
		}
	}
	runs, err := r.Store.LoopRuns(ctx, store.RunFilter{})
	if err != nil || len(runs) != 0 {
		t.Fatalf("hold left runs: %+v %v", runs, err)
	}
	got, err := r.Store.CaseByID(ctx, c.ID)
	if err != nil || got.Cost.Runs != 0 || got.Cost.USD != .064 || got.Cost.Tokens != 24 {
		t.Fatalf("held spend was lost or counted as runs: %+v %v", got.Cost, err)
	}
	if why, err := r.stopped(ctx, got, false); err != nil || why != "" {
		t.Fatalf("waiting plans reached the stop rule: %s %v", why, err)
	}
}

func TestFailedRollbackIsLoggedOncePerCause(t *testing.T) {
	m := measuredCase(t)
	m.measure(t, Options{})
	hours(t, m.st, "after", m.theCase(t).Deployed, 24*time.Hour, 100, 6)
	m.clock = m.clock.Add(25 * time.Hour)
	m.r.Ship.Live = true
	for i := 0; i < 3; i++ {
		if got := m.measure(t, Options{}); got.Status != "failed" {
			t.Fatalf("fixture did not fail its rollback: %+v", got)
		}
	}
	count := func() int {
		t.Helper()
		log, err := m.st.CaseLog(context.Background(), m.c.ID)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range log {
			if e.Action == "not rolled back" {
				n++
			}
		}
		return n
	}
	if n := count(); n != 1 {
		t.Fatalf("unchanged rollback failure added %d log notes, want one", n)
	}
	p, _, _ := m.r.Ship.Load(m.c.ID)
	p.Host, p.BaseCommit = m.repo, p.Landed[0]
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	m.measure(t, Options{})
	if n := count(); n != 2 {
		t.Fatalf("a changed rollback cause did not add one note: %d", n)
	}
}

func TestLegacyMeasurementPlansWaitWithOnePassiveNote(t *testing.T) {
	for _, missing := range []string{"time", "units"} {
		t.Run(missing, func(t *testing.T) {
			m := measuredCase(t)
			p, _, _ := m.r.Ship.Load(m.c.ID)
			if missing == "time" {
				p.Plan.At = time.Time{}
			} else {
				p.Plan.Units = ""
			}
			if err := m.r.Ship.save(&p); err != nil {
				t.Fatal(err)
			}
			m.measure(t, Options{Dry: true})
			if ns := m.notes(t); len(ns) != 0 {
				t.Fatal("dry measure wrote a legacy note")
			}
			for i := 0; i < 3; i++ {
				if got := m.measure(t, Options{}); got.Status != "held" || !strings.Contains(got.Why, "measured by hand") {
					t.Fatalf("legacy plan repeatedly fails: %+v", got)
				}
			}
			ns := m.notes(t)
			if len(ns) != 1 || ns[0].Importance != store.ImportanceLow || m.theCase(t).Status != store.CaseDeployed {
				t.Fatalf("legacy plan changed its case or lacks a passive note: %+v", ns)
			}
			if _, _, err := m.st.DoneNote(context.Background(), ns[0].ID, m.clock); err != nil {
				t.Fatal(err)
			}
			m.measure(t, Options{})
			if ns := m.notes(t); len(ns) != 0 {
				t.Fatal("legacy plan repeated its resolved note")
			}
		})
	}
}
