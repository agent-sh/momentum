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
	anotherBuild = "another case builds in this pass"   // the pass's one build is another family's
	noRoom       = "no room for its build in this pass" // the budgets leave the pass no room
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
// It is round-robin. Before any run is reserved, the families whose best case builds prepare their builds in the build
// turn's order until one is ready; a family whose build is held tries its next build. The family whose build is ready
// takes the first place, so any room reaches it, and the others wait for it. Then each other family runs one case in
// its own place, in the run turn's order, so a pass with room for fewer runs than families starts after the family
// whose run (other than the build) the last pass reserved first. A family whose build waits for another's, or whose
// builds came to no run (a hold), runs its best case that builds nothing instead, and its next one when that comes to
// no run. With no room, no build is prepared. A family whose ready build is not reserved runs nothing else, and no
// other case builds in the pass. No case is reserved twice, no ready build is left unreserved while room is left, and
// a family named twice is one family.
//
// With room for a run each pass and every ready build reserved, a family with a buildable case goes at most b-1 passes
// without its build, b being the families with a buildable case (TestPassPlansAreFairOverRandomFamilies).
//
// It returns the steps, in the order taken, and the turns after the pass.
func schedule(families []string, cands map[string][]candidate, turns map[string]string, room int, prepare, reserve func(fam string, c candidate) outcome) ([]planned, map[string]string) {
	families = uniqueFamilies(families)
	next := map[string]string{turnBuild: turns[turnBuild], turnRun: turns[turnRun]}
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
		famDone               // its build recorded a run: it runs nothing else
		famInstead            // it runs its other cases
	)
	state := map[string]int{}
	building, ready := "", candidate{} // the family whose build is ready, and the build
	for _, fam := range inTurn(families, turns[turnBuild]) {
		switch {
		case !builder(fam):
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
	order := inTurn(families, turns[turnRun])
	if building != "" {
		order = append([]string{building}, slices.DeleteFunc(slices.Clone(order), func(fam string) bool { return fam == building })...)
	}
	first := "" // the first family to reserve a run other than the build: the run turn moves to it
	tryOne := func(fam string, c candidate) outcome {
		out := reserve(fam, c)
		steps = append(steps, planned{fam: fam, cand: c, out: out})
		switch {
		case out != runReserved:
		case c.step == StepBuild:
			next[turnBuild] = fam
		case first == "":
			first = fam
		}
		return out
	}
	for _, fam := range order {
		switch state[fam] {
		case famBuild:
			tryOne(fam, ready)
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
