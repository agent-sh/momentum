package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// investigated is a case in testing that holds a mechanism and a fix.
func investigated(t *testing.T, st *store.Store, family, sig, mechanism, fix string) store.Case {
	t.Helper()
	c := openCase(t, st, family, sig, 2)
	got, recorded, err := recordFindings(context.Background(), st, c.ID, c.Episode, finding{Mechanism: mechanism, Fix: fix}, true, "", "loop:r")
	if err != nil || !recorded || got.Status != store.CaseTesting {
		t.Fatalf("setup: %+v %v %v", got, recorded, err)
	}
	return got
}

// An investigation is shown the family's other cases in testing, which hold a cause and a fix, and a report that
// names one of them as the case whose cause is this one's makes that case the place of this one's fix: the case
// waits in testing with a note naming the sibling. Nothing moves or closes, and the sibling is untouched.
func TestAnInvestigationCanFindItsCaseToBeAnothers(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	root := investigated(t, st, "failures", "schema-root", "the tool's schema asks for a field the model never gives", "make the field optional in the tool's schema")
	other := investigated(t, st, "failures", "other-cause", "the network filter resets the connection", "ask the owner to change the filter")
	bare := openCase(t, st, "failures", "no-findings-yet", 3)                // open, nothing found: no sibling
	elsewhere := investigated(t, st, "sessions", "another-family", "m", "f") // another family: no sibling
	placed := openCase(t, st, "failures", "place-and-no-fix", 2)             // in testing with a place and no fix of its own: no sibling
	if got, _, err := recordFindings(ctx, st, placed.ID, placed.Episode, finding{Mechanism: "a cause", Place: "another project"}, true, "", "loop:r"); err != nil ||
		got.Status != store.CaseTesting || got.NextTest != fixNotStated {
		t.Fatalf("setup: %+v %v", got, err)
	}
	long := investigated(t, st, "failures", "long-findings", strings.Repeat("a long mechanism ", 100), "a fix")
	c := openCase(t, st, "failures", "schema-again", 4)
	sibs, err := siblingsOf(ctx, st, c)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]sibling{}
	for _, s := range sibs {
		ids[s.ID] = s
	}
	if len(ids) != 3 || ids[root.ID].Mechanism == "" || ids[root.ID].Fix == "" || ids[other.ID].ID == "" {
		t.Fatalf("the siblings shown: %+v (want %s, %s and %s, not %s, %s or %s)", sibs, root.ID, other.ID, long.ID, bare.ID, elsewhere.ID, placed.ID)
	}
	if n := len([]rune(ids[long.ID].Mechanism)); n == 0 || n > siblingMax {
		t.Fatalf("a sibling's mechanism is shown at %d runes, over %d", n, siblingMax)
	}
	got, recorded, err := recordFindings(ctx, st, c.ID, c.Episode, finding{Mechanism: "the same schema field", Fix: "as the other case",
		Report: "Same cause as the schema case.", Repo: "momentum", DuplicateOf: root.ID}, true, "", "loop:r")
	if err != nil || !recorded || got.Status != store.CaseTesting || got.Mechanism != "the same schema field" || got.Evidence != 4 {
		t.Fatalf("a case with a sibling's cause waits in testing with its own findings and evidence: %+v %v %v", got, recorded, err)
	}
	r := localRunner(t, st)
	r.Ship = &Ship{Store: st, Dir: r.Dir}
	if repo, config, place, err := r.caseFixPlace(ctx, got); err != nil || repo != "" || config != "" || place != siblingPlace(root.ID) {
		t.Fatalf("its fix lives with the sibling, and it names no repository of its own: %q %q %q %v", repo, config, place, err)
	}
	n := waitingRepoNote(got, repoHold(siblingPlace(root.ID)), time.Now())
	if !strings.Contains(n.Title, root.ID) || !strings.Contains(n.Detail, "momentum case close "+got.ID) {
		t.Fatalf("the note names the sibling and how to close the case: %q %q", n.Title, n.Detail)
	}
	after, err := st.CaseByID(ctx, root.ID)
	if err != nil || after.Evidence != root.Evidence || after.Status != store.CaseTesting || after.Cost != root.Cost {
		t.Fatalf("the sibling is untouched: %+v %v", after, err)
	}
	// it comes before the report's other outcomes: beside nothing to fix, and with no fix text, the case still waits
	// with the sibling and is neither dismissed nor closed as undecided
	for i, f := range []finding{{Mechanism: "the same schema field", NothingToFix: true, DuplicateOf: root.ID},
		{Mechanism: "the same schema field", DuplicateOf: " " + root.ID + " "}} {
		n := openCase(t, st, "failures", fmt.Sprintf("schema-more-%d", i), 2)
		if got, recorded, err = recordFindings(ctx, st, n.ID, n.Episode, f, true, "the investigation named a mechanism and no fix", "loop:r"); err != nil ||
			!recorded || got.Status != store.CaseTesting || got.NextTest != fixNotStated {
			t.Fatalf("a sibling's cause beside %+v: %+v %v %v", f, got, recorded, err)
		}
		if _, _, place, _ := r.caseFixPlace(ctx, got); place != siblingPlace(root.ID) {
			t.Fatalf("its place: %q", place)
		}
	}
	// with no mechanism the report found no cause, a sibling's or its own: it is undecided as before
	bad := openCase(t, st, "failures", "no-mechanism", 2)
	if got, recorded, err = recordFindings(ctx, st, bad.ID, bad.Episode, finding{Fix: "a fix", DuplicateOf: root.ID}, true,
		"the investigation named a fix and no mechanism", "loop:r"); err != nil || !recorded || got.Status != store.CaseClosed || got.Reason == store.NotWorthIt {
		t.Fatalf("a sibling named with no mechanism: %+v %v %v", got, recorded, err)
	}
}

// A report may name only a sibling. Any other id names none, and the report is recorded as one without it: the
// case itself, a case with no findings, one of another family, a closed one, an unknown id. A component-health
// case has no sibling and is not closed this way.
func TestADuplicateOfACaseThatWasNotShownIsNone(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	root := investigated(t, st, "failures", "schema-root", "the tool's schema asks for a field the model never gives", "make the field optional")
	bare := openCase(t, st, "failures", "no-findings-yet", 3)
	elsewhere := investigated(t, st, "sessions", "another-family", "m", "f")
	closed := investigated(t, st, "failures", "closed-one", "m", "f")
	if _, err := st.CloseCase(ctx, closed.ID, store.NotWorthIt, "", "fixture", "fixture"); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"SELF", bare.ID, elsewhere.ID, closed.ID, "c00000000", "not an id"} {
		c := openCase(t, st, "failures", "names-a-stranger-"+string(rune('a'+i)), 2)
		if id == "SELF" {
			id = c.ID
		}
		got, recorded, err := recordFindings(ctx, st, c.ID, c.Episode, finding{Mechanism: "a cause of its own", Fix: "a fix of its own",
			DuplicateOf: id}, true, "", "loop:r")
		if err != nil || !recorded || got.Status != store.CaseTesting || got.Into != "" {
			t.Errorf("duplicate_of %q: %+v %v %v", id, got, recorded, err)
		}
	}
	h := openCase(t, st, store.HealthFamily, "health-dup", 2)
	if sibs, err := siblingsOf(ctx, st, h); err != nil || len(sibs) != 0 {
		t.Fatalf("a health case's siblings: %+v %v", sibs, err)
	}
	other := investigated(t, st, store.HealthFamily, "health-root", "m", "f")
	got, recorded, err := recordFindings(ctx, st, h.ID, h.Episode, finding{Mechanism: "a cause", Fix: "a fix", DuplicateOf: other.ID}, true, "", "loop:r")
	if err != nil || !recorded || got.Status == store.CaseClosed {
		t.Fatalf("a health case naming another: %+v %v %v", got, recorded, err)
	}
	if after, _ := st.CaseByID(ctx, root.ID); after.Evidence != 2 || after.Status != store.CaseTesting {
		t.Fatalf("the sibling changed: %+v", after)
	}
}

// The run's case file lists the siblings, and its question asks about them only when there are some.
func TestAnInvestigationIsShownItsSiblings(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := runnerFor(t, st, fakeAgent(t, `{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":1}`))
	r.pass = &passInputs{}
	defer r.pass.close()
	if err := r.prepareInputs(ctx); err != nil {
		t.Fatal(err)
	}
	alone := openCase(t, st, "failures", "alone", 2)
	run, err := r.prepareRun(ctx, alone, alone.Family, StepInvestigate)
	if err != nil {
		t.Fatal(err)
	}
	if raw := readInput(t, run, "case.json"); strings.Contains(raw, `"siblings"`) {
		t.Fatalf("a case with no sibling is shown some: %s", raw)
	}
	if q := readInput(t, run, "prompt.json"); strings.Contains(q, "duplicate_of") {
		t.Fatalf("a case with no sibling is asked about them: %s", q)
	}
	root := investigated(t, st, "failures", "schema-root", "the tool's schema asks for a field the model never gives", "make the field optional")
	c := openCase(t, st, "failures", "schema-again", 2)
	if run, err = r.prepareRun(ctx, c, c.Family, StepInvestigate); err != nil {
		t.Fatal(err)
	}
	if raw := readInput(t, run, "case.json"); !strings.Contains(raw, `"siblings"`) || !strings.Contains(raw, root.ID) ||
		!strings.Contains(raw, "make the field optional") {
		t.Fatalf("the case file does not show the sibling: %s", raw)
	}
	if q := readInput(t, run, "prompt.json"); !strings.Contains(q, "duplicate_of") {
		t.Fatalf("the question does not ask about the siblings: %s", q)
	}
}

// readInput is a file of a prepared run's inputs, with its prompt file when name is prompt.json.
func readInput(t *testing.T, run store.LoopRun, name string) string {
	t.Helper()
	p := filepath.Join(HostDir(run.Dir), "input", name)
	if name == "prompt.json" {
		p = filepath.Join(HostDir(run.Dir), name)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
