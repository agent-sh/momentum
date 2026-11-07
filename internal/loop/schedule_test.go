package loop

import (
	"fmt"
	"math/rand/v2"
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
//  3. a family with a case that can run runs within as many passes as there are families, while the budget has
//     room for a run a pass, through held builds, failed preparations, investigations whose inputs fail, unreadable
//     records and families whose every attempt fails;
//  4. a family whose every attempt fails, though the scheduler sees its cases, stops no other family's runs (3 and 5
//     hold for the others);
//  5. when room is left after the pass, every family that ran nothing had nothing it could run;
//  6. no build is prepared without room for it or left unreserved, and none is reserved unprepared.
func TestPassPlansAreFairOverRandomFamilies(t *testing.T) {
	rng := rand.New(rand.NewPCG(102, 4))
	for trial := range 4000 {
		fams := randomFamilies(rng)
		n := len(fams)
		names := make([]string, 0, n+1)
		cases, family := map[string]simCase{}, map[string]simFamily{}
		for _, f := range fams {
			names = append(names, f.name)
			family[f.name] = f
			for _, c := range f.cases {
				cases[c.cand.c.ID] = c
			}
		}
		if rng.IntN(5) == 0 { // a family named twice
			names = append(names, names[rng.IntN(n)])
		}
		room := 1 + rng.IntN(3)
		turns := map[string]string{}
		last := map[string]int{}
		for _, f := range fams {
			last[f.name] = -1
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
			left, builds := room, 0
			prepared, reserved, ran := map[string]bool{}, map[string]bool{}, map[string]bool{}
			prepare := func(fam string, c candidate) outcome {
				sc := cases[c.c.ID]
				switch {
				case c.step != StepBuild:
					fail("%s prepared as a build", c.c.ID)
				case left == 0:
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
				case family[fam].failing || c.step != StepBuild && sc.noInputs:
					return runNone
				case c.step == StepBuild && !prepared[c.c.ID]:
					fail("%s reserved unprepared", c.c.ID)
				case left == 0:
					return runRecorded // the budgets refuse it
				case reserved[c.c.ID]:
					fail("%s reserved twice", c.c.ID)
				}
				reserved[c.c.ID], ran[fam] = true, true
				left--
				if c.step == StepBuild {
					builds++
				}
				return runReserved
			}
			steps, after := schedule(names, cands, turns, room, prepare, reserve)
			history = append(history, fmt.Sprintf("pass %d (turns %v): %s", pass, turns, plan(steps)))
			turns = after
			if builds > 1 {
				fail("%d builds", builds)
			}
			for id := range prepared {
				if !reserved[id] {
					fail("%s prepared and left unreserved", id)
				}
			}
			for _, f := range fams {
				switch {
				case ran[f.name]:
					last[f.name] = pass
				case left > 0 && (f.canInvestigate() || builds == 0 && f.canBuild()):
					fail("%s ran nothing with room left", f.name)
				case (f.canInvestigate() || f.canBuild()) && pass-last[f.name] >= n:
					fail("%s ran nothing for %d passes", f.name, pass-last[f.name])
				}
			}
		}
	}
}
