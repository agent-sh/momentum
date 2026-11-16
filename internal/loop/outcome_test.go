package loop

import (
	"context"
	"strings"
	"testing"

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
