package loop

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/agent-sh/momentum/internal/store"
)

// simCase is a case of a simulated family, fixed for its trial: its candidate and what trying it comes to.
type simCase struct {
	cand      candidate
	held      bool // its build is held at its preparation: no run
	failsPrep bool // its build's preparation fails: a run recorded and ended
	noInputs  bool // its investigation's inputs cannot be made: no run
	unread    bool // its record cannot be read: its pick skips it
}

// simFamily is a family of a trial and its cases, best first. Every attempt on a failing family's cases comes to no
// run.
type simFamily struct {
	name    string
	failing bool
	cases   []simCase
}

// canInvestigate says whether the family has a case that builds nothing and can run.
func (f simFamily) canInvestigate() bool {
	for _, c := range f.cases {
		if !f.failing && c.cand.step != StepBuild && !c.unread && !c.noInputs {
			return true
		}
	}
	return false
}

// canBuild says whether the family has a build that comes to a run: one that is not held.
func (f simFamily) canBuild() bool {
	for _, c := range f.cases {
		if !f.failing && c.cand.step == StepBuild && !c.unread && !c.held {
			return true
		}
	}
	return false
}

// buildable says whether the family's builds come to a reserved build: the first one not held prepares (a build
// whose preparation fails ends the family's builds for the pass).
func (f simFamily) buildable() bool {
	for _, c := range f.cases {
		if !f.failing && c.cand.step == StepBuild && !c.unread && !c.held {
			return !c.failsPrep
		}
	}
	return false
}

// describe is a trial's families in short: each case's step and what stops it.
func describe(fams []simFamily) string {
	var b strings.Builder
	for _, f := range fams {
		b.WriteString(f.name)
		if f.failing {
			b.WriteString(" failing")
		}
		for _, c := range f.cases {
			b.WriteString(" " + c.cand.step)
			for _, flag := range []struct {
				on   bool
				name string
			}{{c.held, "held"}, {c.failsPrep, "fails"}, {c.noInputs, "noinputs"}, {c.unread, "unread"}} {
				if flag.on {
					b.WriteString("/" + flag.name)
				}
			}
		}
		b.WriteString("; ")
	}
	return b.String()
}

// plan is a pass's steps in short: each case tried and what came of it, or why it waited.
func plan(steps []planned) string {
	var parts []string
	for _, s := range steps {
		what := [...]string{runReserved: "reserved", runReady: "ready", runRecorded: "recorded", runNone: "none"}[s.out]
		if s.skipped != "" {
			what = "waits"
		}
		parts = append(parts, s.cand.c.ID+" "+s.cand.step+" "+what)
	}
	return strings.Join(parts, ", ")
}

func randomFamilies(rng *rand.Rand) []simFamily {
	fams := make([]simFamily, 1+rng.IntN(5))
	for i := range fams {
		f := simFamily{name: fmt.Sprint("f", i), failing: rng.IntN(6) == 0}
		builds := rng.IntN(3) // testing cases come first in a family's pick
		for j := range builds + rng.IntN(3) {
			step, status := StepInvestigate, store.CaseOpen
			if j < builds {
				step, status = StepBuild, store.CaseTesting
			}
			c := simCase{cand: candidate{c: store.Case{ID: fmt.Sprintf("%s-c%d", f.name, j), Family: f.name, Status: status}, step: step}}
			if step == StepBuild {
				c.held, c.failsPrep = rng.IntN(4) == 0, rng.IntN(5) == 0
			} else {
				c.noInputs = rng.IntN(5) == 0
			}
			c.unread = rng.IntN(8) == 0
			f.cases = append(f.cases, c)
		}
		fams[i] = f
	}
	return fams
}

// The pass's plan over random families, with a fixed seed. On every pass:
//  1. no case is reserved twice;
//  2. at most one build is reserved;
//  3. a family with a case that can run runs within n passes, n the families, while the budget has room for two runs
//     a pass, and within 2n with room for one (when a family has a buildable case, the build and the run turn take
//     the passes in turn), through held builds, failed preparations, investigations whose inputs fail, unreadable
//     records and families whose every attempt fails;
//  4. a family whose every attempt fails, though the scheduler sees its cases, stops no other family's runs (3 and 5
//     hold for the others);
//  5. when room is left after the pass, every family that ran nothing had nothing it could run;
//  6. no build is prepared without room for it or left unreserved, and none is reserved unprepared;
//  7. a family whose ready build's reservation fails (none, as in a pass that stops, or a run recorded) reserves
//     nothing else in the pass, and no other build is reserved;
//  8. a family with a buildable case goes at most b-1 passes without its build, b the families with a buildable case,
//     with room for two runs a pass, and at most 2b-1 with room for one;
//  9. a family records or reserves at most one run in a pass.
//
// The room is the budget check's own count (runsThatFit) of a random run budget, earlier spend and limit, in whole
// micro-dollars, and the model's check of each reservation is that count too. In a quarter of the trials a ready
// build's reservation fails at random, and an investigation's records a run and ends (its reservation could not be
// read back); 3 and 8 are claimed only while every reservation goes through.
func TestPassPlansAreFairOverRandomFamilies(t *testing.T) {
	rng := rand.New(rand.NewPCG(102, 4))
	for trial := range 4000 {
		fams := randomFamilies(rng)
		n := len(fams)
		names := make([]string, 0, n+1)
		cases, family := map[string]simCase{}, map[string]simFamily{}
		m, b := 0, 0 // the families with a case, and those with a buildable one
		for _, f := range fams {
			names = append(names, f.name)
			family[f.name] = f
			for _, c := range f.cases {
				cases[c.cand.c.ID] = c
			}
			if slices.ContainsFunc(f.cases, func(c simCase) bool { return !c.unread }) {
				m++
			}
			if f.buildable() {
				b++
			}
		}
		if rng.IntN(5) == 0 { // a family named twice
			names = append(names, names[rng.IntN(n)])
		}
		run := int64(1 + rng.IntN(5_000_000))                                 // a run's budget
		spent := int64(rng.IntN(20_000_000))                                  // what the day has spent
		limit := spent + int64(1+rng.IntN(3))*run + int64(rng.IntN(int(run))) // room for 1 to 3 runs
		room := runsThatFit(spent, run, limit, 2)                             // no run, one or more, as the runner counts it
		faults := rng.IntN(4) == 0                                            // a reservation may fail
		// the most passes a family with a buildable case goes without its build (8), and one with a case that can run
		// goes without a run (3)
		gap, runGap := b-1, n
		if room == 1 && b > 0 {
			gap, runGap = 2*b-1, 2*n
		}
		single := [...]string{"", turnBuild, turnRun}[rng.IntN(3)] // the turn the last pass that took exactly one took, if any
		turns := map[string]string{}                               // the turns a store with none starts at, or any two families'
		if rng.IntN(2) == 0 {
			turns = map[string]string{turnBuild: names[rng.IntN(n)], turnRun: names[rng.IntN(n)]}
		}
		last, built := map[string]int{}, map[string]int{}
		for _, f := range fams {
			last[f.name], built[f.name] = -1, -1
		}
		var history []string // each pass's plan, for a failure's message
		for pass := range 4 * n {
			fail := func(format string, args ...any) {
				t.Helper()
				t.Fatalf("trial %d pass %d: %s (%d families, room %d): %s (named %v)\n%s", trial, pass, fmt.Sprintf(format, args...), n, room,
					describe(fams), names, strings.Join(history, "\n"))
			}
			cands := map[string][]candidate{}
			for _, f := range fams {
				for _, c := range f.cases {
					if !c.unread {
						cands[f.name] = append(cands[f.name], c.cand)
					}
				}
			}
			counted, builds, builder := spent, 0, ""
			fits := func() bool { return runsThatFit(counted, run, limit, 1) == 1 } // the budget check
			prepared, reserved, ran := map[string]bool{}, map[string]bool{}, map[string]bool{}
			refused := map[string]bool{} // by family: its ready build's reservation failed
			runs := map[string]int{}     // by family: the runs it recorded or reserved
			once := func(fam string, out outcome) outcome {
				if out == runReserved || out == runRecorded {
					if runs[fam]++; runs[fam] > 1 {
						fail("%s recorded or reserved %d runs", fam, runs[fam])
					}
				}
				return out
			}
			prepare := func(fam string, c candidate) outcome {
				sc := cases[c.c.ID]
				switch {
				case c.step != StepBuild:
					fail("%s prepared as a build", c.c.ID)
				case !fits():
					fail("%s prepared with no room left", c.c.ID)
				case family[fam].failing || sc.held:
					return runNone
				case sc.failsPrep:
					ran[fam] = true
					return runRecorded
				}
				prepared[c.c.ID] = true
				return runReady
			}
			reserve := func(fam string, c candidate) outcome {
				sc := cases[c.c.ID]
				switch {
				case refused[fam]:
					fail("%s reserved after its ready build's reservation failed", c.c.ID)
				case family[fam].failing || c.step != StepBuild && sc.noInputs:
					return runNone
				case c.step == StepBuild && !prepared[c.c.ID]:
					fail("%s reserved unprepared", c.c.ID)
				case !fits():
					return runRecorded // the budgets refuse it
				case reserved[c.c.ID]:
					fail("%s reserved twice", c.c.ID)
				case c.step == StepBuild && faults && rng.IntN(3) == 0:
					refused[fam] = true
					if rng.IntN(2) == 0 {
						return runNone
					}
					ran[fam] = true
					return runRecorded
				case c.step != StepBuild && faults && rng.IntN(6) == 0: // its row ends unstarted and uncharged
					ran[fam] = true
					return runRecorded
				}
				reserved[c.c.ID], ran[fam] = true, true
				counted = store.AddMicros(counted, run)
				if c.step == StepBuild {
					builds++
					builder = fam
				}
				return runReserved
			}
			runsFirst := single != turnRun // as the runner reads the row
			steps, after, took := schedule(names, cands, turns, room, runsFirst,
				func(fam string, c candidate) outcome { return once(fam, prepare(fam, c)) },
				func(fam string, c candidate) outcome { return once(fam, reserve(fam, c)) })
			history = append(history, fmt.Sprintf("pass %d (turns %v, runs first %v): %s", pass, turns, runsFirst, plan(steps)))
			turns = after
			switch { // as the runner writes the row: only a pass that took exactly one turn
			case took[turnBuild] && !took[turnRun]:
				single = turnBuild
			case took[turnRun] && !took[turnBuild]:
				single = turnRun
			}
			if builds > 1 || builds > 0 && len(refused) > 0 {
				fail("%d builds, %d ready builds refused", builds, len(refused))
			}
			for id := range prepared {
				if !reserved[id] && !refused[cases[id].cand.c.Family] {
					fail("%s prepared and left unreserved", id)
				}
			}
			taken := builds > 0 || len(refused) > 0 // the pass's build went to a family
			for _, f := range fams {
				switch {
				case ran[f.name]:
					last[f.name] = pass
				case refused[f.name]:
				case fits() && (f.canInvestigate() || !taken && f.canBuild()):
					fail("%s ran nothing with room left", f.name)
				case !faults && (f.canInvestigate() || f.canBuild()) && pass-last[f.name] >= runGap:
					fail("%s ran nothing for %d passes", f.name, pass-last[f.name])
				}
				switch {
				case builder == f.name:
					built[f.name] = pass
				case !faults && f.buildable() && pass-built[f.name] > gap:
					fail("%s did not build for %d passes (%d families with a case, %d with a buildable one)", f.name, pass-built[f.name], m, b)
				}
			}
		}
	}
}

// probeCands are a family's candidates, best first, one case a step.
func probeCands(fam string, steps ...string) []candidate {
	var cs []candidate
	for i, s := range steps {
		status := store.CaseOpen
		if s == StepBuild {
			status = store.CaseTesting
		}
		cs = append(cs, candidate{c: store.Case{ID: fam + "-" + string(rune('a'+i)), Family: fam, Status: status}, step: s})
	}
	return cs
}

// A family whose build's preparation records a run runs nothing else in the pass, whether its place came before any
// run (room 2: prepared up front) or after (room 1: its build waits for its place): one run a family.
func TestAFailedPreparationIsTheFamilysOneRun(t *testing.T) {
	for _, room := range []int{1, 2} {
		cands := map[string][]candidate{"f0": probeCands("f0", StepInvestigate), "f1": probeCands("f1", StepBuild, StepInvestigate)}
		left := room
		prepare := func(fam string, c candidate) outcome { return runRecorded }
		reserve := func(fam string, c candidate) outcome {
			if fam == "f0" {
				return runNone // f0's investigation cannot be made: the room stays for f1
			}
			if left == 0 {
				return runRecorded
			}
			left--
			return runReserved
		}
		steps, _, _ := schedule([]string{"f0", "f1"}, cands, map[string]string{}, room, false, prepare, reserve)
		runs := 0
		for _, s := range steps {
			if s.fam == "f1" && s.skipped == "" && (s.out == runReserved || s.out == runRecorded) {
				runs++
			}
		}
		if runs != 1 {
			t.Errorf("room %d: f1 recorded or reserved %d runs, want 1: %s", room, runs, plan(steps))
		}
	}
}

// A ready build whose reservation the budgets refuse takes no build turn.
func TestARefusedReadyBuildTakesNoTurn(t *testing.T) {
	cands := map[string][]candidate{"f0": probeCands("f0", StepBuild), "f1": probeCands("f1", StepInvestigate)}
	prepare := func(fam string, c candidate) outcome { return runReady }
	reserve := func(fam string, c candidate) outcome {
		if c.step == StepBuild {
			return runRecorded // refused
		}
		return runReserved
	}
	_, after, _ := schedule([]string{"f0", "f1"}, cands, map[string]string{turnBuild: "f1"}, 2, false, prepare, reserve)
	if after[turnBuild] != "f1" {
		t.Fatalf("a refused ready build moved the build turn to %q", after[turnBuild])
	}
}

// A case that built last pass and failed yields the build to another family's testing case that waits, whatever the
// room: two families build, one investigates and one comes to no run, with room for 1 to 3 runs.
func TestAFailedBuildYieldsWhateverTheRoom(t *testing.T) {
	for room := 1; room <= 3; room++ {
		cands := map[string][]candidate{"A": probeCands("A", StepBuild), "B": probeCands("B", StepBuild),
			"C": probeCands("C", StepInvestigate), "D": probeCands("D", StepInvestigate)}
		prepare := func(fam string, c candidate) outcome { return runReady }
		turns := map[string]string{turnBuild: "B", turnRun: "B"}
		last := ""
		for pass := range 4 {
			left, builder := room, ""
			reserve := func(fam string, c candidate) outcome {
				switch {
				case fam == "D":
					return runNone
				case left == 0:
					return runRecorded
				}
				left--
				if c.step == StepBuild {
					builder = fam
				}
				return runReserved
			}
			steps, after, _ := schedule([]string{"A", "B", "C", "D"}, cands, turns, room, false, prepare, reserve)
			if builder == "" || builder == last {
				t.Fatalf("room %d pass %d: %q built after %q (turns %v): %s", room, pass, builder, last, turns, plan(steps))
			}
			last, turns = builder, after
		}
	}
}

// With room for one run and the run turn served first, a builder family whose other case the budgets refused still
// says why its build waits.
func TestARefusedBuilderGetsTheRunsTurnNote(t *testing.T) {
	cands := map[string][]candidate{"A": probeCands("A", StepBuild, StepInvestigate), "B": probeCands("B", StepInvestigate)}
	left := 1
	reserve := func(fam string, c candidate) outcome {
		if left == 0 {
			return runRecorded // refused
		}
		left--
		return runReserved
	}
	steps, _, _ := schedule([]string{"A", "B"}, cands, map[string]string{turnRun: "A"}, 1, true,
		func(string, candidate) outcome { return runReady }, reserve)
	for _, s := range steps {
		if s.fam == "A" && s.cand.step == StepBuild && s.skipped == runsTurn {
			return
		}
	}
	t.Errorf("A's build has no %q step: %s", runsTurn, plan(steps))
}
