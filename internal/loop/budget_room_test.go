package loop

import (
	"context"
	"database/sql"
	"math"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// stackHas says whether the calling goroutine's stack holds fn: a test's hook in a clock or a gate uses it to act at
// one point of a pass.
func stackHas(fn string) bool {
	buf := make([]byte, 1<<16)
	return strings.Contains(string(buf[:runtime.Stack(buf, false)]), fn)
}

// throughAfter reserves runs, up to most, until the budgets refuse one, and says how many went through.
func throughAfter(t *testing.T, r *Runner, c store.Case, most int) int {
	t.Helper()
	n := 0
	for ; n < most; n++ {
		if _, why := r.reserve(context.Background(), c.Family, c.ID, StepInvestigate); why != "" {
			break
		}
	}
	return n
}

// The room is the number of reservations the budget check lets through also after runs the day already holds, each
// charged at the run's whole budget, where the store's sum of them and a running sum of runs differ in the last bits.
func TestTheRoomAfterRunsTheDayHoldsIsWhatTheCheckLetsThrough(t *testing.T) {
	for _, tc := range []struct {
		prior int
		b     Budgets
	}{
		{2, Budgets{Run: 0.1, Day: 0.7, Week: 200}},
		{2, Budgets{Run: 0.05, Day: 0.35, Week: 200}},
		{2, Budgets{Run: 0.37, Day: 2.59, Week: 200}},
		{2, Budgets{Run: 0.39, Day: 2.73, Week: 200}},
	} {
		ctx := context.Background()
		st := testStore(t)
		c := openCase(t, st, "failures", "marker-7f3a", 1)
		r := &Runner{Store: st, Budgets: tc.b, Dir: t.TempDir()}
		if prior := throughAfter(t, r, c, tc.prior); prior != tc.prior {
			t.Fatalf("run $%v, day $%v: %d of %d earlier runs went through", tc.b.Run, tc.b.Day, prior, tc.prior)
		}
		room, _, err := r.room(ctx, 5)
		if err != nil {
			t.Fatal(err)
		}
		if through := throughAfter(t, r, c, 5); room != through {
			t.Errorf("%d earlier runs, run $%v, day $%v: the room is %d runs, the budgets let %d through", tc.prior, tc.b.Run, tc.b.Day,
				room, through)
		}
	}
}

// The room is what the budget check lets through over a search: every run budget in whole cents up to $1 and a few
// that are not, 0 to 3 earlier runs the day holds at the run's whole budget, with and without a finished run whose cost
// is not a whole micro-dollar, a day budget of 1 to 5 more runs as one would write it, and 1 to 5 families.
func TestTheRoomIsWhatTheCheckLetsThroughOverASearch(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	c := openCase(t, st, "failures", "marker-7f3a", 1)
	db, err := sql.Open("sqlite", st.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var runs []float64
	for cents := 1; cents <= 100; cents++ {
		runs = append(runs, float64(cents)/100)
	}
	runs = append(runs, 0.1234567, 1.0000005, 3.3333333)
	typed := func(usd float64) float64 { return math.Round(usd*1e7) / 1e7 } // a budget as one writes it
	checked, wrong := 0, 0
	for _, run := range runs {
		for _, extra := range []float64{0, 0.0123456789} {
			for prior := 0; prior <= 3; prior++ {
				for more := 1; more <= 5; more++ {
					if _, err := db.Exec(`DELETE FROM loop_runs`); err != nil {
						t.Fatal(err)
					}
					if extra > 0 {
						if err := st.PutLoopRun(ctx, store.LoopRun{ID: "earlier", Case: c.ID, Family: c.Family, Step: StepInvestigate,
							Started: time.Now().Add(-time.Hour), Ended: time.Now().Add(-time.Hour), Status: store.RunOK, USD: extra}); err != nil {
							t.Fatal(err)
						}
					}
					b := Budgets{Run: run, Day: typed(float64(prior+more)*run + extra), Week: 1e6}
					r := &Runner{Store: st, Budgets: b, Dir: t.TempDir()}
					if got := throughAfter(t, r, c, prior); got != prior {
						t.Fatalf("run $%v, day $%v: %d of %d earlier runs went through", run, b.Day, got, prior)
					}
					rooms := make([]int, 6)
					for most := 1; most <= 5; most++ {
						if rooms[most], _, err = r.room(ctx, most); err != nil {
							t.Fatal(err)
						}
					}
					through := throughAfter(t, r, c, 5)
					for most := 1; most <= 5; most++ {
						checked++
						if want := min(through, most); rooms[most] != want {
							if wrong++; wrong <= 10 {
								t.Errorf("run $%v, %d earlier runs and $%v, day $%v, %d families: the room is %d runs, the budgets let %d through",
									run, prior, extra, b.Day, most, rooms[most], want)
							}
						}
					}
				}
			}
		}
	}
	if wrong > 0 {
		t.Errorf("%d of %d rooms differ from what the budgets let through", wrong, checked)
	}
}

// A pass whose room the budget check agrees with: two runs of $0.10 already in the day's $0.70, five families, the
// build's family last. The build runs, or waits unprepared for room; it is never prepared and then refused at its place.
func TestABuildTheRoomReachesIsNotRefusedAtItsPlace(t *testing.T) {
	r, cs := turnRunner(t, "owner-burden")
	ctx := context.Background()
	r.Budgets.Run, r.Budgets.Day = 0.1, 0.7
	prior := openCase(t, r.Store, "outside", "marker-7f3a-prior", 1) // a family the pass does not run
	if got := throughAfter(t, r, prior, 2); got != 2 {
		t.Fatalf("%d of 2 earlier runs went through", got)
	}
	var opens []string
	for _, fam := range []string{"sessions", "manual-work", "blind-spots", "pressure"} {
		opens = append(opens, openCase(t, r.Store, fam, "marker-7f3a-"+fam, 1).ID)
	}
	r.Families = []string{"sessions", "manual-work", "blind-spots", "pressure", "owner-burden"}
	done, err := r.Once(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range opens {
		if ran, why := turnStep(done, id, StepInvestigate); !ran {
			t.Fatalf("%s did not investigate: %q", id, why)
		}
	}
	if built, why := turnStep(done, cs[0].ID, StepBuild); !built && !strings.Contains(why, noRoom) {
		t.Fatalf("the build was prepared and refused at its place: %q", why)
	}
}

// A pass counts its budgets back from one moment: a run that leaves the day's window while the pass reserves its
// runs leaves the room and every check as they were at the pass's start. Here the day's $10 holds an earlier $4 run
// at the start, so one $4 run fits; the second family's run is refused though the checks run hours later.
func TestAPassCountsItsBudgetsFromOneMoment(t *testing.T) {
	r, _ := turnRunner(t)
	ctx := context.Background()
	first := openCase(t, r.Store, "sessions", "marker-7f3a-first", 2)
	second := openCase(t, r.Store, "owner-burden", "marker-7f3a-second", 1)
	r.Families = []string{"sessions", "owner-burden"}
	r.Budgets.Run, r.Budgets.Day = 4, 10
	start := time.Now()
	if err := r.Store.PutLoopRun(ctx, store.LoopRun{ID: "earlier", Case: first.ID, Family: first.Family, Step: StepInvestigate,
		Started: start.Add(-23 * time.Hour), Ended: start.Add(-23 * time.Hour), Status: store.RunOK, USD: 4}); err != nil {
		t.Fatal(err)
	}
	r.Now = func() time.Time {
		if stackHas("(*Runner).affordable") { // a check that reads the clock reads it two hours on
			return start.Add(2 * time.Hour)
		}
		return start
	}
	done, err := r.Once(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ran, why := turnStep(done, first.ID, StepInvestigate); !ran {
		t.Fatalf("the first family did not investigate: %q", why)
	}
	if ran, why := turnStep(done, second.ID, StepInvestigate); ran || !strings.Contains(why, "day budget") {
		t.Fatalf("the second family's run was not refused by the day's budget as the pass's start counts it: %v %q", ran, why)
	}
}

// A pass that cannot read the budgets prepares as if they had room, so the budget check refuses the build with what
// failed, rather than the build waiting for room.
func TestABudgetReadFailurePreparesAsIfThereWereRoom(t *testing.T) {
	r, cs := turnRunner(t, "sessions")
	ctx := context.Background()
	other := openCase(t, r.Store, "outside", "marker-7f3a-other", 1) // a family the pass does not run
	db, err := sql.Open("sqlite", r.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO case_log (case_id, seq, at, actor, action, detail) VALUES (?, 9999, ?, 'fixture', 'loop hold', 'marker-7f3a')`,
		other.ID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.LoopSpend(ctx, time.Now().Add(-time.Hour), 4); err == nil {
		t.Fatal("the fixture's hold leaves the budgets readable")
	}
	done, err := r.Once(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	built, why := turnStep(done, cs[0].ID, StepBuild)
	if built || strings.Contains(why, noRoom) || !strings.Contains(why, "JSON") {
		t.Fatalf("the build with unreadable budgets: built %v, %q", built, why)
	}
}

// A reservation whose row is written and cannot be read back ends that row, unstarted, uncharged and naming no
// directory, before the pass removes what it prepared: no running row stays charged to the budgets.
func TestAReservationWhoseReadFailsEndsItsRun(t *testing.T) {
	r, cs := turnRunner(t, "sessions")
	ctx := context.Background()
	db, err := sql.Open("sqlite", r.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// a running row's start becomes text, which no read of the row can scan
	if _, err := db.Exec(`CREATE TRIGGER unreadable_reservation AFTER INSERT ON loop_runs WHEN NEW.status = 'running'
	  BEGIN UPDATE loop_runs SET started = 'marker-7f3a' WHERE id = NEW.id; END`); err != nil {
		t.Fatal(err)
	}
	done, err := r.Once(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if built, why := turnStep(done, cs[0].ID, StepBuild); built || !strings.Contains(why, "reservation") {
		t.Fatalf("the build's reservation did not fail on its read: %v %q", built, why)
	}
	var status, dir string
	var started any
	var usd float64
	if err := db.QueryRow(`SELECT status, started, cost_usd, dir FROM loop_runs WHERE case_id = ? AND step = ?`, cs[0].ID, StepBuild).
		Scan(&status, &started, &usd, &dir); err != nil {
		t.Fatal(err)
	}
	if status != store.RunFailed || started != int64(0) || usd != 0 || dir != "" {
		t.Fatalf("the reservation's row is %s, started %v, $%v, dir %q; want failed, unstarted, uncharged, no directory", status, started, usd, dir)
	}
	if left := runDirs(t, r); len(left) != 0 {
		t.Fatalf("the failed reservation left %v on disk", left)
	}
}

// The pass's one clock reading for its budgets ends with the pass: a budget check after it counts back from the clock.
func TestABudgetCheckAfterAPassCountsFromTheClock(t *testing.T) {
	r, _ := turnRunner(t)
	ctx := context.Background()
	start := time.Now()
	r.Now = func() time.Time { return start }
	if _, err := r.Once(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	c := openCase(t, r.Store, "sessions", "marker-7f3a", 1)
	if err := r.Store.PutLoopRun(ctx, store.LoopRun{ID: "marker-7f3a-spend", Case: c.ID, Family: c.Family, Step: StepInvestigate,
		Started: start.Add(-time.Hour), Ended: start.Add(-time.Hour), Status: store.RunOK, USD: r.Budgets.Day}); err != nil {
		t.Fatal(err)
	}
	r.Now = func() time.Time { return start.Add(25 * time.Hour) } // the day's spend left the window
	if why, err := r.affordable(ctx); err != nil || why != "" {
		t.Fatalf("a check a day after the pass counted from the pass's start: %q %v", why, err)
	}
}
