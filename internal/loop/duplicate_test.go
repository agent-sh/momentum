package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// An investigation is shown the family's other cases that hold a cause and a fix, and a report that names one of
// them as the case it duplicates sends its case into it: the evidence moves, and the case closes as a duplicate.
func TestAnInvestigationCanFindItsCaseToBeAnothers(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	root := investigated(t, st, "failures", "schema-root", "the tool's schema asks for a field the model never gives", "make the field optional in the tool's schema")
	other := investigated(t, st, "failures", "other-cause", "the network filter resets the connection", "ask the owner to change the filter")
	bare := openCase(t, st, "failures", "no-findings-yet", 3)                // open, nothing found: no sibling
	elsewhere := investigated(t, st, "sessions", "another-family", "m", "f") // another family: no sibling
	c := openCase(t, st, "failures", "schema-again", 4)
	sibs, err := siblingsOf(ctx, st, c)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]sibling{}
	for _, s := range sibs {
		ids[s.ID] = s
	}
	if len(ids) != 2 || ids[root.ID].Mechanism == "" || ids[root.ID].Fix == "" || ids[other.ID].ID == "" {
		t.Fatalf("the siblings shown: %+v (want %s and %s, not %s or %s)", sibs, root.ID, other.ID, bare.ID, elsewhere.ID)
	}
	got, recorded, err := recordFindings(ctx, st, c.ID, c.Episode, finding{Mechanism: "the same schema field", Fix: "as the other case",
		Report: "Same cause as the schema case.", DuplicateOf: root.ID}, true, "", "loop:r")
	if err != nil || !recorded || got.Status != store.CaseClosed || got.Reason != store.Duplicate || got.Into != root.ID {
		t.Fatalf("a duplicate of a sibling: %+v %v %v", got, recorded, err)
	}
	into, err := st.CaseByID(ctx, root.ID)
	if err != nil || into.Evidence != 6 || into.Status != store.CaseTesting {
		t.Fatalf("the case it went into holds both cases' evidence and stays as it was: %+v %v", into, err)
	}
	log, _ := st.CaseLog(ctx, c.ID)
	said := false
	for _, e := range log {
		said = said || strings.Contains(e.Detail, duplicateOf+root.ID+": Same cause as the schema case.")
	}
	if !said {
		t.Fatalf("the close's reason: %+v", log)
	}
}

// A report may name only a case its run was shown. Any other id names none, and the report is recorded as one
// without it: the case itself, a case with no findings, one of another family, a closed one, an unknown id. A
// component-health case is shown no sibling and goes into none.
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
	if after, _ := st.CaseByID(ctx, root.ID); after.Evidence != 2 {
		t.Fatalf("a case nobody went into gained evidence: %+v", after)
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
