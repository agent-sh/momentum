package loop

import "slices"

// outcome is what came of preparing or reserving a family's run.
type outcome int

const (
	runReserved outcome = iota // a run was reserved: it starts with the pass
	runReady                   // a build was prepared: it is reserved in its family's place
	runRecorded                // a run was recorded and ended unstarted (a refusal of the budgets, a failed preparation)
	runNone                    // no run was recorded (a hold, inputs that could not be made): the family's next case may run
)

// Why a family's build waits.
const (
	anotherBuild = "another case builds in this pass"   // the pass's one build is another family's
	noRoom       = "no room for its build in this pass" // the families before it take the room the budgets leave
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
// runs take their turn. room is how many runs the budgets leave the pass.
//
// It is round-robin. Each family runs one case in its own place, in the run turn's order, so a pass with room for
// fewer runs than families starts after the family whose run the last pass reserved first. The pass's one build is
// settled before any run is reserved: the families whose best case builds, and whose place the room reaches, prepare
// their builds in the build turn's order until one is ready; a family whose build is held tries its next build. A
// family whose place the room may not reach prepares nothing then: in its place, if no build is ready and room is
// left, it prepares its builds there. A family whose build waits for another's, or whose builds came to no run (a
// hold), runs its best case that builds nothing instead, and its next one when that comes to no run. No case is
// reserved twice, no ready build is left unreserved while room is left, and a family named twice is one family.
//
// It returns the steps, in the order taken, and the turns after the pass.
func schedule(families []string, cands map[string][]candidate, turns map[string]string, room int, prepare, reserve func(fam string, c candidate) outcome) ([]planned, map[string]string) {
	families = uniqueFamilies(families)
	next := map[string]string{turnBuild: turns[turnBuild], turnRun: turns[turnRun]}
	order := inTurn(families, turns[turnRun])
	place := map[string]int{} // how many families before it in the run turn's order have a case to run
	for _, fam := range order {
		if len(cands[fam]) > 0 {
			place[fam] = len(place)
		}
	}
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
		famBuild    = iota + 1 // its build is ready: it is reserved in its place
		famDone                // its build recorded a run: it runs nothing else
		famInstead             // it runs its other cases
		famDeferred            // its place the room may not reach: it prepares its builds there, if it may
	)
	state := map[string]int{}
	ready := map[string]candidate{}
	built := false // a build is ready
	for _, fam := range inTurn(families, turns[turnBuild]) {
		switch {
		case !builder(fam):
		case built:
			steps = append(steps, planned{fam: fam, cand: builds[fam][0], skipped: anotherBuild})
			state[fam] = famInstead
		case place[fam] >= room:
			state[fam] = famDeferred
		default:
			c, out := prepareBuilds(fam)
			switch out {
			case runReady:
				built, ready[fam], state[fam] = true, c, famBuild
			case runNone:
				state[fam] = famInstead
			default:
				state[fam] = famDone
			}
		}
	}
	first, reserved := "", 0
	tryOne := func(fam string, c candidate) outcome {
		out := reserve(fam, c)
		steps = append(steps, planned{fam: fam, cand: c, out: out})
		if out == runReserved {
			reserved++
			if first == "" {
				first = fam
			}
			if c.step == StepBuild {
				next[turnBuild] = fam
			}
		}
		return out
	}
	for _, fam := range order {
		if len(cands[fam]) == 0 {
			continue
		}
		if state[fam] == famDeferred {
			switch {
			case built:
				steps = append(steps, planned{fam: fam, cand: builds[fam][0], skipped: anotherBuild})
				state[fam] = famInstead
			case reserved >= room:
				steps = append(steps, planned{fam: fam, cand: builds[fam][0], skipped: noRoom})
				state[fam] = famInstead
			default:
				c, out := prepareBuilds(fam)
				switch out {
				case runReady:
					built, ready[fam], state[fam] = true, c, famBuild
				case runNone:
					state[fam] = famInstead
				default:
					state[fam] = famDone
				}
			}
		}
		switch state[fam] {
		case famBuild:
			tryOne(fam, ready[fam])
		case famDone:
		default:
			for _, c := range others[fam] {
				if tryOne(fam, c) != runNone {
					break
				}
			}
		}
	}
	if first != "" {
		next[turnRun] = first
	}
	return steps, next
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
