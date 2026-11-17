package loop

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// An investigation that concludes nothing should be changed ends its case: it closes as not worth it with the
// investigation's reason, and waits for no repository. A report that names a repository has a fix to build, and
// a run cut short, whose findings file is all it left, concluded nothing.
func TestAnInvestigationThatFindsNothingToFixClosesItsCase(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	c := openCase(t, st, "failures", "nothing-to-fix", 1)
	got, recorded, err := recordFindings(ctx, st, c.ID, c.Episode, finding{Mechanism: "the provider refused the request by its own policy",
		Report: "A provider's refusal.\nNothing on the machine causes it.", NothingToFix: true}, true, "the report named no fix", "loop:r")
	if err != nil || !recorded || got.Status != store.CaseClosed || got.Reason != store.NotWorthIt || !strings.Contains(got.Mechanism, "provider refused") {
		t.Fatalf("nothing to fix: %+v %v %v", got, recorded, err)
	}
	log, err := st.CaseLog(ctx, c.ID)
	if err != nil || len(log) == 0 {
		t.Fatal(err)
	}
	if last := log[len(log)-1]; last.Action != "closed" || !strings.Contains(last.Detail, nothingToFix+"A provider's refusal. Nothing on the machine causes it.") {
		t.Fatalf("the close's reason: %+v", last)
	}
	built := openCase(t, st, "failures", "nothing-but-a-repository", 1)
	if got, recorded, err = recordFindings(ctx, st, built.ID, built.Episode, finding{Mechanism: "one cause", Fix: "one fix", Repo: "momentum",
		NothingToFix: true}, true, "", "loop:r"); err != nil || !recorded || got.Status != store.CaseTesting {
		t.Fatalf("a report that names a repository: %+v %v %v", got, recorded, err)
	}
	cut := openCase(t, st, "failures", "nothing-in-a-findings-file", 1)
	if got, recorded, err = recordFindings(ctx, st, cut.ID, cut.Episode, finding{Mechanism: "one cause", NothingToFix: true}, false, "", "loop:r"); err != nil ||
		!recorded || got.Status != store.CaseInvestigating {
		t.Fatalf("a run cut short: %+v %v %v", got, recorded, err)
	}
}

// A fix the investigation placed outside the loop's repositories waits as before, and its hold and its note say
// where the fix lives and what it is, so an agent or the owner can make it. A place beside a repository names
// nothing, and a report with no place keeps the old wait.
func TestAFixOutsideTheLoopsRepositoriesIsNamedInItsNote(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	r.Ship = &Ship{Store: st, Dir: r.Dir}
	waiting := func(c store.Case) (string, store.Note) {
		t.Helper()
		picked, _, why, err := r.pick(ctx, c.Family, c.ID, false)
		if err != nil || picked.ID != "" || !strings.Contains(why, "waits for a repository") {
			t.Fatalf("selected a build without a repository: %s %s %v", picked.ID, why, err)
		}
		got, _ := st.CaseByID(ctx, c.ID)
		log, _ := st.CaseLog(ctx, c.ID)
		n, ok, err := st.Note(ctx, repoNoteID(c.ID))
		if err != nil || !ok || !n.Done.IsZero() {
			t.Fatalf("the note: %+v %v %v", n, ok, err)
		}
		return store.HoldOf(got, log), n
	}
	const place = "~/.config/tool/hooks/review.ts (a hook of the machine)"
	c := openCase(t, st, "failures", "fix-elsewhere", 1)
	if got, recorded, err := recordFindings(ctx, st, c.ID, c.Episode, finding{Mechanism: "the hook reviews each edit alone",
		Fix: "review the edits of one message together", Place: " ~/.config/tool/hooks/review.ts\n(a hook of the machine) "}, true, "", "loop:r"); err != nil ||
		!recorded || got.Status != store.CaseTesting {
		t.Fatalf("a fix elsewhere: %+v %v %v", got, recorded, err)
	}
	hold, n := waiting(c)
	if hold != elsewherePrefix+place {
		t.Fatalf("the hold: %q", hold)
	}
	if !strings.Contains(n.Title, c.ID) || !strings.Contains(n.Title, "~/.config/tool/hooks/review.ts") || len([]rune(n.Title)) > 120 ||
		!strings.Contains(n.Detail, "review the edits of one message together") || !strings.Contains(n.Detail, "momentum case close "+c.ID) {
		t.Fatalf("the note: %q %q", n.Title, n.Detail)
	}
	plain := openCase(t, st, "failures", "fix-nowhere", 1)
	if _, _, err := recordFindings(ctx, st, plain.ID, plain.Episode, finding{Mechanism: "one cause", Fix: "one fix"}, true, "", "loop:r"); err != nil {
		t.Fatal(err)
	}
	if hold, n := waiting(plain); hold != noRepoHold || !strings.Contains(n.Title, "waits for a repository") {
		t.Fatalf("no place named: %q %q", hold, n.Title)
	}
	both := openCase(t, st, "failures", "fix-in-a-repository", 1)
	if _, _, err := recordFindings(ctx, st, both.ID, both.Episode, finding{Mechanism: "one cause", Fix: "one fix", Repo: "momentum", Place: place}, true, "", "loop:r"); err != nil {
		t.Fatal(err)
	}
	both, _ = st.CaseByID(ctx, both.ID)
	if repo, _, named, err := r.caseFixPlace(ctx, both); err != nil || repo != "momentum" || named != "" {
		t.Fatalf("a place beside a repository: %q %q %v", repo, named, err)
	}
}

// A report that says nothing is to be fixed and also says where a fix lives contradicts itself: the place wins, and
// the case waits with it. A word that only says there is no place is none. A report with a fix text and no place
// is still ended by its conclusion: the schema makes every report name a fix, if only to say there is none.
func TestNothingToFixBesideAPlaceWaitsWithThePlace(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	c := openCase(t, st, "failures", "upstream-fix", 1)
	got, recorded, err := recordFindings(ctx, st, c.ID, c.Episode, finding{Mechanism: "the upstream tool drops the flag",
		Fix: "patch the upstream tool's parser to keep the flag", Place: "the upstream tool's repository (another project)", NothingToFix: true,
		Report: "Nothing to change on this machine: the bug is upstream."}, true, "", "loop:r")
	if err != nil || !recorded || got.Status != store.CaseTesting {
		t.Fatalf("nothing to fix beside a place: %+v %v %v", got, recorded, err)
	}
	log, err := st.CaseLog(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	named := false
	for _, e := range log {
		named = named || strings.Contains(e.Detail, "the upstream tool's repository (another project)")
	}
	if !named {
		t.Fatalf("the place is not on record: %+v", log)
	}
	for _, filler := range []string{"N/A", "none.", " - ", "Unknown"} {
		if p := placeOf(finding{Place: filler}); p != "" {
			t.Errorf("%q is taken for a place: %q", filler, p)
		}
	}
	said := openCase(t, st, "failures", "no-change-needed", 1)
	if got, recorded, err = recordFindings(ctx, st, said.ID, said.Episode, finding{Mechanism: "the tool refused a call it is meant to refuse",
		Fix: "No change: the refusal is the tool working.", Place: "n/a", NothingToFix: true, Report: "The tool worked as meant."}, true, "", "loop:r"); err != nil ||
		!recorded || got.Status != store.CaseClosed || got.Reason != store.NotWorthIt {
		t.Fatalf("nothing to fix with a fix text that says so: %+v %v %v", got, recorded, err)
	}
}

// A component-health case is not ended by its investigation's conclusion: its stop may hold cases that only its
// fix resumes. And the note of one whose fix lives elsewhere gives the close command its family takes.
func TestAComponentHealthCaseIsNotEndedByNothingToFix(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	h := openCase(t, st, store.HealthFamily, "health-nothing", 1)
	got, recorded, err := recordFindings(ctx, st, h.ID, h.Episode, finding{Mechanism: "the provider failed by itself",
		Fix: "none", NothingToFix: true, Report: "A provider outage."}, true, "", "loop:r")
	if err != nil || !recorded || got.Status == store.CaseClosed {
		t.Fatalf("a health case with nothing to fix: %+v %v %v", got, recorded, err)
	}
	fix := "restart the relay"
	got.NextTest = fix
	n := waitingRepoNote(got, elsewherePrefix+"the relay's unit file", time.Now())
	if !strings.Contains(n.Detail, "--fix REF") || !strings.Contains(n.Detail, "The fix: restart the relay.") ||
		strings.Index(n.Detail, "momentum case close") > strings.Index(n.Detail, "The fix:") {
		t.Fatalf("the health case's note: %q", n.Detail)
	}
	other := waitingRepoNote(store.Case{ID: "c1", Family: "failures", NextTest: strings.Repeat("long fix ", 200)}, elsewherePrefix+"a file", time.Now())
	if strings.Contains(other.Detail, "--fix REF") || len([]rune(other.Detail)) > 700 {
		t.Fatalf("another family's note: %d runes: %q", len([]rune(other.Detail)), other.Detail)
	}
}

// A report with no fix text is not closed as undecided once it says where the fix lives, or once a component-health
// case says nothing is to change: the place would be lost, and the health case could not be resumed from.
func TestABlankFixBesideAPlaceOrAHealthCaseIsNotClosed(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	const noFix = "the investigation named a mechanism and no fix"
	c := openCase(t, st, "failures", "place-and-no-fix", 1)
	got, recorded, err := recordFindings(ctx, st, c.ID, c.Episode, finding{Mechanism: "the upstream tool drops the flag",
		Place: "the upstream tool's repository (another project)", NothingToFix: true, Report: "The bug is upstream."}, true, noFix, "loop:r")
	if err != nil || !recorded || got.Status != store.CaseTesting {
		t.Fatalf("a place with no fix text: %+v %v %v", got, recorded, err)
	}
	log, _ := st.CaseLog(ctx, c.ID)
	named := false
	for _, e := range log {
		named = named || strings.Contains(e.Detail, "the upstream tool's repository (another project)")
	}
	if !named {
		t.Fatalf("the place is not on record: %+v", log)
	}
	n := waitingRepoNote(got, elsewherePrefix+"the upstream tool's repository", time.Now())
	if strings.Contains(n.Detail, "The fix:") || !strings.Contains(n.Detail, "Where: the upstream tool's repository.") {
		t.Fatalf("the note of a place with no fix: %q", n.Detail)
	}
	for i, fix := range []string{"", "  "} {
		h := openCase(t, st, store.HealthFamily, fmt.Sprintf("health-blank-fix-%d", i), 1)
		if got, recorded, err = recordFindings(ctx, st, h.ID, h.Episode, finding{Mechanism: "the provider failed by itself", Fix: fix,
			NothingToFix: true, Report: "A provider outage."}, true, noFix, "loop:r"); err != nil || !recorded || got.Status == store.CaseClosed {
			t.Fatalf("a health case with nothing to fix and fix %q: %+v %v %v", fix, got, recorded, err)
		}
	}
	// with neither, a report with no fix is undecided, as before
	u := openCase(t, st, "failures", "no-fix-no-place", 1)
	if got, recorded, err = recordFindings(ctx, st, u.ID, u.Episode, finding{Mechanism: "one cause"}, true, noFix, "loop:r"); err != nil ||
		!recorded || got.Status != store.CaseClosed || got.Reason == store.NotWorthIt {
		t.Fatalf("a mechanism and nothing else: %+v %v %v", got, recorded, err)
	}
}

// A word that only says there is none names no place. A text that goes on after such a word may name one.
func TestAWordForNoneNamesNoPlace(t *testing.T) {
	for _, w := range []string{"", " ", "N/A", "(none)", "None.", "not applicable", "No place", "none needed", "nil", "no", "N.A.", "TBD", "?", "—", "\"\"", "-"} {
		if p := placeOf(finding{Place: w}); p != "" {
			t.Errorf("%q is taken for a place: %q", w, p)
		}
	}
	for _, w := range []string{"~/.config/tool/hook.ts", "nonesuch repository", "the owner's router", "notes",
		"None of the loop's repositories: the upstream tool's repository", "n/a here, see ~/.bashrc"} {
		if p := placeOf(finding{Place: w}); p == "" {
			t.Errorf("%q is taken for a word for none", w)
		}
	}
}

// A live investigation is told which paths of the loop's own repository no build may change, in the gate's own
// words, and to report a fix that needs one as a place. The evaluation's question is the plain one.
func TestAnInvestigationIsToldWhatNoBuildMayChange(t *testing.T) {
	st := testStore(t)
	r := localRunner(t, st)
	c := openCase(t, st, "failures", "own-reach", 1)
	q := r.investigationQuestion(c)
	for _, want := range append(append([]string{SelfRepo, "report place as that path"}, ProposalProtected()...), ciPaths...) {
		if !strings.Contains(q, want) {
			t.Errorf("the question does not name %q", want)
		}
	}
	if !strings.HasPrefix(q, liveQuestion) || strings.Contains(liveQuestion, SelfRepo) {
		t.Fatalf("the plain question changed, or is not where the live one begins")
	}
}
