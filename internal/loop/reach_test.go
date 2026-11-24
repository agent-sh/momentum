package loop

import (
	"context"
	"strings"
	"testing"

	"github.com/agent-sh/momentum/internal/store"
)

// A case whose investigation placed its fix in paths of the loop's own repository that no build could change then
// waits for good, though a build may change those paths now: its record names no repo. At selection it goes back to
// be investigated, once an episode, and is the family's next investigation. An investigation that reads today's
// reach and still names no repo leaves it waiting, and a place that needs a path no build may change, or anything
// outside the repository, waits as before.
func TestAFixPlacedInPathsNowInReachIsInvestigatedAgain(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	r.Ship = &Ship{Store: st, Dir: r.Dir}
	const place = "internal/detect/failures.go (toolKey) in " + SelfRepo
	c := openCase(t, st, "failures", "fix-in-own-detector", 1)
	record := func() {
		t.Helper()
		if got, recorded, err := recordFindings(ctx, st, c.ID, c.Episode, finding{Mechanism: "the detector keys a harness's own error record",
			Fix: "drop that record in toolKey", Place: place}, true, "", "loop:r"); err != nil || !recorded || got.Status != store.CaseTesting {
			t.Fatalf("the investigation's record: %+v %v %v", got, recorded, err)
		}
	}
	record()
	// an investigation told today's reach that named no repo waits
	if _, _, why, err := r.pick(ctx, c.Family, c.ID, true); err != nil || strings.Contains(why, "goes back to be investigated") {
		t.Fatalf("a record under today's reach goes back: %q %v", why, err)
	}
	// the record of an investigation told another reach (an older list, or none recorded)
	older := func() {
		t.Helper()
		if err := st.CaseNote(ctx, c.ID, repoAction, `{"repo":"","place":"`+place+`","episode":1,"reach":"older"}`, "loop:r"); err != nil {
			t.Fatal(err)
		}
	}
	older()
	// a case with no room for a run keeps its wait: it would only wait again as investigating
	full := r.Budgets.Case
	r.Budgets.Case = 0
	if picked, _, why, err := r.pick(ctx, c.Family, c.ID, false); err != nil || picked.ID != "" || !strings.Contains(why, "out of the loop's reach") {
		t.Fatalf("a case with no room goes back: %s %q %v", picked.ID, why, err)
	}
	r.Budgets.Case = full
	if _, _, why, err := r.pick(ctx, c.Family, c.ID, true); err != nil || !strings.Contains(why, "goes back to be investigated") {
		t.Fatalf("a dry pass says the case goes back: %q %v", why, err)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Status != store.CaseTesting {
		t.Fatalf("a dry pass moved the case: %s", got.Status)
	}
	picked, _, why, err := r.pick(ctx, c.Family, c.ID, false)
	if err != nil || picked.ID != "" || !strings.Contains(why, c.ID+" goes back to be investigated") {
		t.Fatalf("the case is not sent back: %s %q %v", picked.ID, why, err)
	}
	got, _ := st.CaseByID(ctx, c.ID)
	log, _ := st.CaseLog(ctx, c.ID)
	if got.Status != store.CaseInvestigating || store.HoldOf(store.Case{Status: store.CaseTesting, Episode: got.Episode}, log) != "" {
		t.Fatalf("the case is investigated again with no hold: %s %q", got.Status, store.HoldOf(got, log))
	}
	if picked, step, _, err := r.pick(ctx, c.Family, c.ID, true); err != nil || picked.ID != c.ID || step != StepInvestigate {
		t.Fatalf("the next pass investigates it: %s %s %v", picked.ID, step, err)
	}
	// the new investigation still names the place and no repo, under another reach again: once an episode, the case waits
	record()
	older()
	if picked, _, why, err := r.pick(ctx, c.Family, c.ID, false); err != nil || picked.ID != "" || !strings.Contains(why, "out of the loop's reach") {
		t.Fatalf("a second time in the episode: %s %q %v", picked.ID, why, err)
	}

	for _, p := range []string{place, "internal/detect/ in " + SelfRepo, "internal/detect/failures.go:120 (toolKey) in " + SelfRepo,
		"internal/detect/failures.go and the internal key in " + SelfRepo, "README.md and internal/supervise/rules.go in agent-sh/Momentum"} {
		if !ownPlaceInReach(p) {
			t.Errorf("%q is in reach", p)
		}
	}
	for _, p := range []string{
		"internal/loop/gate.go in " + SelfRepo,                             // what the loop's authority rests on
		"internal/detect/failures.go and internal/measure/ in " + SelfRepo, // one path of two
		"~/.claude/mods/x/hooks/register.ts",                               // outside the repository
		"internal/detect/failures.go in " + SelfRepo + " and ~/.claude/mods/x.ts",
		"agent-sh/tacit internal/x.go, with " + SelfRepo, // another repository
		"go.mod in " + SelfRepo,
		"the detectors of " + SelfRepo,                // no path
		"internal/detect/failures.go",                 // no repository named
		"internal/capacity/run.go:120 in " + SelfRepo, // a protected file with a line after it
		"internal/capacity/record.go#L5 in " + SelfRepo,
		"internal/capacity in " + SelfRepo,      // a directory that holds protected files
		"internal/ in " + SelfRepo,              // the same
		"internal/capacity/*.go in " + SelfRepo, // a glob
		"internal/detect/../loop/gate.go in " + SelfRepo,
		"internal/detect/x.go and go.mod:3 in " + SelfRepo,
		"internal/detect/x.go and https://github.com/" + SelfRepo + "/blob/main/internal/loop/gate.go", // a URL
		"internal/detect/x.go, " + SelfRepo + ":internal/loop/gate.go",
		"internal/detect/x.go and file:internal/loop/gate.go in " + SelfRepo,
		"internal/detect/x.go|internal/loop/gate.go in " + SelfRepo,                                  // two paths in one word
		"internal/capacity/run in " + SelfRepo,                                                       // a protected file without its extension
		"internal/detect/failures.go in " + SelfRepo + " and …",                                      // a place cut short
		"internal/detect/failures.go in " + SelfRepo + " " + strings.Repeat("and the detector ", 10), // as long as the clip
		"internal/./loop/gate.go in " + SelfRepo,                                                     // not clean
		"internal/capacity/run.go/ in " + SelfRepo,
		"internal/detect/x.go and cmd/ in " + SelfRepo,
	} {
		if ownPlaceInReach(p) {
			t.Errorf("%q is out of reach", p)
		}
	}
}
