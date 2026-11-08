package loop

import "slices"

// outcome is what came of preparing or reserving a family's run.
type outcome int

const (
	runReserved outcome = iota // a run was reserved: it starts with the pass
	runReady                   // a build was prepared: it is reserved first
	runRecorded                // a run was recorded and ended unstarted (a refusal of the budgets, a failed preparation, an unreadable reservation)
	// no run was recorded (a hold, inputs that could not be made, a pass that stops): the family's next case may run,
	// but none after its ready build
	runNone
)

// Why a family's build waits.
const (
	anotherBuild = "another case builds in this pass"         // the pass's one build is another family's
	noRoom       = "no room for its build in this pass"       // the budgets leave the pass no room
	runsTurn     = "this pass's one run goes to the run turn" // with room for one run, the last such pass built
)

// planned is one step of a pass's plan: a family's case and what came of it, or why it waited.
type planned struct {
	fam     string
	cand    candidate
	out     outcome
	skipped string
}

// schedule plans a pass's runs, with no effect of its own. prepare readies a build without reserving it; reserve
// reserves a run (a build prepare readied, or any other case whole); each says what came of it. Each family's
// candidates come best first. turns name, by turnBuild and turnRun, the families after which the pass's build and its
// runs take their turn. room is whether the budgets leave the pass no run (0), one (1) or more (2). runsFirst says a
// pass with room for one run serves the run turn first (the last pass that took exactly one turn took the build's,
// or no such pass is known).
//
// It is round-robin. Before any run is reserved, the families whose best case builds prepare their builds in the build
// turn's order until one is ready; a family whose build is held tries its next build. The family whose build is ready
// takes the first place, so any room reaches it, and the others wait for it. Then each other family runs one case in
// its own place, in the run turn's order, so a pass with room for fewer runs than families starts after the family
// whose run (other than the build) the last pass reserved first. A family whose build waits for another's, or whose
// builds came to no run (a hold), runs its best case that builds nothing instead, and its next one when that comes to
// no run. With no room, no build is prepared. With room for one run, the build and the run turn take the pass in
// turn: when runsFirst, the families run their cases that build nothing first, in the run turn's order, and the builds
// are prepared only if none of them is reserved; otherwise the build comes first. A family whose ready build is not
// reserved runs nothing else, and no other case builds in the pass. No case is reserved twice, no ready build is left
// unreserved while room is left, and a family named twice is one family.
//
// With room for two runs or more each pass and every ready build reserved, a family with a buildable case goes at most
// b-1 passes without its build, b being the families with a buildable case; with room for one, at most 2b-1
// (TestPassPlansAreFairOverRandomFamilies).
//
// It returns the steps, in the order taken, the turns after the pass, and which of them a reserved run took.
func schedule(families []string, cands map[string][]candidate, turns map[string]string, room int, runsFirst bool,
	prepare, reserve func(fam string, c candidate) outcome) ([]planned, map[string]string, map[string]bool) {
	families = uniqueFamilies(families)
	next := map[string]string{turnBuild: turns[turnBuild], turnRun: turns[turnRun]}
	took := map[string]bool{}
	builds, others := map[string][]candidate{}, map[string][]candidate{}
	for _, fam := range families {
		for _, c := range cands[fam] {
			if c.step == StepBuild {
				builds[fam] = append(builds[fam], c)
			} else {
				others[fam] = append(others[fam], c)
			}
		}
	}
	builder := func(fam string) bool { cs := cands[fam]; return len(cs) > 0 && cs[0].step == StepBuild }
	var steps []planned
	// prepareBuilds prepares the family's builds, best first, until one is ready or comes to a run; a hold tries the next
	prepareBuilds := func(fam string) (candidate, outcome) {
		for _, c := range builds[fam] {
			out := prepare(fam, c)
			steps = append(steps, planned{fam: fam, cand: c, out: out})
			if out != runNone {
				return c, out
			}
		}
		return candidate{}, runNone
	}
	const (
		famBuild   = iota + 1 // its build is ready: it is reserved first
		famDone               // it recorded a run: it runs nothing else
		famInstead            // it runs its other cases
	)
	state := map[string]int{}
	building, ready := "", candidate{} // the family whose build is ready, and the build
	// settle prepares the builds in the build turn's order until one is ready; the families after it wait
	settle := func() {
		for _, fam := range inTurn(families, turns[turnBuild]) {
			switch {
			case !builder(fam) || state[fam] == famDone:
			case building != "":
				steps = append(steps, planned{fam: fam, cand: builds[fam][0], skipped: anotherBuild})
				state[fam] = famInstead
			case room < 1:
				steps = append(steps, planned{fam: fam, cand: builds[fam][0], skipped: noRoom})
				state[fam] = famInstead
			default:
				c, out := prepareBuilds(fam)
				switch out {
				case runReady:
					building, ready, state[fam] = fam, c, famBuild
				case runNone:
					state[fam] = famInstead
				default:
					state[fam] = famDone
				}
			}
		}
	}
	first, reservedAny := "", false // the first family to reserve a run other than the build, and whether any run was reserved
	tryOne := func(fam string, c candidate) outcome {
		out := reserve(fam, c)
		steps = append(steps, planned{fam: fam, cand: c, out: out})
		switch {
		case out == runRecorded:
			state[fam] = famDone
		case out != runReserved:
		case c.step == StepBuild:
			reservedAny, next[turnBuild], took[turnBuild] = true, fam, true
		default:
			reservedAny = true
			if first == "" {
				first = fam
			}
		}
		return out
	}
	// runOthers runs the family's best case that builds nothing, and its next one when that comes to no run
	runOthers := func(fam string) {
		for _, c := range others[fam] {
			if tryOne(fam, c) != runNone {
				break
			}
		}
	}
	order := inTurn(families, turns[turnRun])
	if room == 1 && runsFirst {
		for _, fam := range order {
			runOthers(fam)
		}
		if reservedAny {
			for _, fam := range inTurn(families, turns[turnBuild]) {
				if builder(fam) {
					steps = append(steps, planned{fam: fam, cand: builds[fam][0], skipped: runsTurn})
				}
			}
		} else if settle(); building != "" {
			tryOne(building, ready)
		}
	} else {
		settle()
		if building != "" {
			order = append([]string{building}, slices.DeleteFunc(slices.Clone(order), func(fam string) bool { return fam == building })...)
		}
		for _, fam := range order {
			switch state[fam] {
			case famBuild:
				tryOne(fam, ready)
			case famDone:
			default:
				runOthers(fam)
			}
		}
	}
	if first != "" {
		next[turnRun], took[turnRun] = first, true
	}
	return steps, next, took
}

// uniqueFamilies is families without repeats, in their order.
func uniqueFamilies(families []string) []string {
	var out []string
	for _, fam := range families {
		if !slices.Contains(out, fam) {
			out = append(out, fam)
		}
	}
	return out
}
