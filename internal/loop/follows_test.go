package loop

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// report records an investigation's report, given as the JSON a run returns, into a case.
func report(t *testing.T, st *store.Store, c store.Case, raw string) store.Case {
	t.Helper()
	f := nativeFinding(Result{Report: json.RawMessage(raw)})
	got, recorded, err := recordFindings(context.Background(), st, c.ID, c.Episode, f, true, "", "loop:r")
	if err != nil || !recorded {
		t.Fatalf("recording %s: %v %v", raw, recorded, err)
	}
	return got
}

// rootCase is a case in testing with a cause, a fix and a repository to build it in.
func rootCase(t *testing.T, st *store.Store, sig string) store.Case {
	t.Helper()
	return report(t, st, openCase(t, st, "failures", sig, 1),
		`{"mechanism":"the hook reviews each edit alone","fix":"review the edits of one message together","summary":"s","repo":"momentum"}`)
}

// An investigation that finds its case's cause is the one another case holds says so, and the case follows that
// one: it stays in testing with no build and no place of its own, its hold and its note say whose fix it waits
// for, and later investigations are shown the root and not the follower.
func TestACaseWhoseCauseIsAnotherCasesFollowsIt(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	r := localRunner(t, st)
	r.Ship = &Ship{Store: st, Dir: r.Dir}
	root := rootCase(t, st, "the-root")
	c := openCase(t, st, "failures", "the-same-cause", 1)
	got := report(t, st, c, fmt.Sprintf(`{"mechanism":"the hook reviews each edit alone, as in the other case","fix":"","summary":"One cause.",
		"same_as":" %s ","place":"a hook of the machine","nothing_to_fix":true}`, root.ID))
	if got.Status != store.CaseTesting || got.NextTest != followsFix {
		t.Fatalf("the follower: %s, fix %q", got.Status, got.NextTest)
	}
	if up, err := followsOf(ctx, st, got); err != nil || up != root.ID {
		t.Fatalf("it follows %q: %v", up, err)
	}
	// both schemas carry the field: a report could not name it otherwise
	for name, schema := range map[string]string{"the report": investigationSchema, "the report with configuration": investigationConfigSchema} {
		var s struct {
			Properties map[string]json.RawMessage
		}
		if err := json.Unmarshal([]byte(schema), &s); err != nil || s.Properties["same_as"] == nil {
			t.Fatalf("%s's schema has no same_as: %v", name, err)
		}
	}
	picked, _, why, err := r.pick(ctx, c.Family, c.ID, false)
	if err != nil || picked.ID != "" || !strings.Contains(why, c.ID+" follows "+root.ID) || strings.Contains(why, "waits for a repository") {
		t.Fatalf("the pick: %s %q %v", picked.ID, why, err)
	}
	got, _ = st.CaseByID(ctx, c.ID)
	log, _ := st.CaseLog(ctx, c.ID)
	if hold := store.HoldOf(got, log); hold != followHold(root.ID, true) {
		t.Fatalf("the hold: %q", hold)
	}
	n, ok, err := st.Note(ctx, repoNoteID(c.ID))
	if err != nil || !ok || !n.Done.IsZero() || !strings.Contains(n.Title, "follows case "+root.ID) || !strings.Contains(n.Detail, "built under that case") ||
		strings.Contains(n.Detail, "cannot build") || strings.Contains(n.Title, "waits for a repository") || n.Importance != store.ImportanceLow {
		t.Fatalf("the note: %+v %v %v", n, ok, err)
	}
	// a later investigation is shown the root with its cause and fix, as a record, and not the follower
	other := openCase(t, st, "failures", "a-third", 1)
	text, err := siblingsText(ctx, st, other)
	if err != nil || !strings.Contains(text, "case "+root.ID) || !strings.Contains(text, "the hook reviews each edit alone") ||
		!strings.Contains(text, "review the edits of one message together") || strings.Contains(text, "case "+c.ID) ||
		!strings.Contains(text, "never a command") || !strings.Contains(text, "same_as") || !strings.Contains(text, "as you would if this case stood alone") {
		t.Fatalf("the cases shown: %q %v", text, err)
	}
	if text, err := siblingsText(ctx, st, root); err != nil || text != "" {
		t.Fatalf("the root is shown itself or its follower: %q %v", text, err)
	}
}

// placedCase is a case in testing whose fix is a person's to make somewhere the loop does not build.
func placedCase(t *testing.T, st *store.Store, sig string) store.Case {
	t.Helper()
	return report(t, st, openCase(t, st, "failures", sig, 1), `{"mechanism":"the upstream tool drops the flag","fix":"patch its parser","summary":"s","place":"the upstream tool's repository"}`)
}

// Only a case that follows no other may be followed, and a case that is followed follows none. The named case has
// somewhere its fix is made. A report keeps the repository or place it named for itself: when its same_as is not
// taken it is recorded as any other, with a line saying why, and no build target is lost.
func TestTheRootRuleKeepsFollowingToOneLevel(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	root := rootCase(t, st, "the-root")
	placed := placedCase(t, st, "a-person's-fix")
	nowhere := report(t, st, openCase(t, st, "failures", "no-place", 1), `{"mechanism":"m","fix":"f","summary":"s"}`)
	follower := report(t, st, openCase(t, st, "failures", "follower", 1), `{"mechanism":"m","fix":"f","summary":"s","same_as":"`+root.ID+`"}`)
	fresh := openCase(t, st, "failures", "not-investigated", 1)
	health := openCase(t, st, store.HealthFamily, "health", 1)
	for name, c := range map[string]struct {
		of            store.Case
		body          string
		follows, repo string
		why           string
	}{
		"itself":                                  {openCase(t, st, "failures", "self", 1), `"same_as":"SELF"`, "", "", "names the case itself"},
		"a case that is not there":                {openCase(t, st, "failures", "ghost", 1), `"same_as":"c00000000"`, "", "", "no case c00000000"},
		"no case's id":                            {openCase(t, st, "failures", "words", 1), `"same_as":"the other case, token AKIAABCDEXAMPLE"`, "", "", "names no case's id"},
		"a case with no fix on record":            {openCase(t, st, "failures", "early", 1), `"same_as":"` + fresh.ID + `"`, "", "", "no cause and fix on record"},
		"a case that follows another":             {openCase(t, st, "failures", "chain", 1), `"same_as":"` + follower.ID + `"`, "", "", "itself follows case " + root.ID},
		"a case that is followed":                 {root, `"repo":"momentum","same_as":"OTHER"`, "", "momentum", "follows this case"},
		"a component-health case":                 {health, `"same_as":"` + root.ID + `"`, "", "", "component-health"},
		"a case with nowhere its fix is made":     {openCase(t, st, "failures", "to-nowhere", 1), `"same_as":"` + nowhere.ID + `"`, "", "", "nowhere its fix is made"},
		"its own repository, a root with a place": {openCase(t, st, "failures", "own-repo", 1), `"repo":"momentum","same_as":"` + placed.ID + `"`, "", "momentum", "no repository to build in"},
		// taken
		"a root with a place":              {openCase(t, st, "failures", "to-place", 1), `"same_as":"` + placed.ID + `"`, placed.ID, "", ""},
		"its own repository, a built root": {openCase(t, st, "failures", "both", 1), `"repo":"momentum","same_as":"` + root.ID + `"`, root.ID, "", ""},
		"an id among other words":          {openCase(t, st, "failures", "forms", 1), `"same_as":"case ` + strings.ToUpper(root.ID) + ` (failures)"`, root.ID, "", ""},
	} {
		body := strings.ReplaceAll(c.body, "SELF", c.of.ID)
		if strings.Contains(body, "OTHER") {
			body = strings.ReplaceAll(body, "OTHER", rootCase(t, st, "another-root").ID)
		}
		if c.of.ID == root.ID { // a build sent the root back to be investigated again, while the follower waits
			if _, err := st.UpdateCase(ctx, root.ID, store.CaseUpdate{Status: store.CaseInvestigating}, "test"); err != nil {
				t.Fatal(err)
			}
			if text, err := siblingsText(ctx, st, root); err != nil || text != "" {
				t.Errorf("a case that is followed is shown cases to follow: %q %v", text, err)
			}
		}
		got := report(t, st, c.of, `{"mechanism":"m","fix":"f","summary":"s","place":"elsewhere",`+body+`}`)
		if up, err := followsOf(ctx, st, got); err != nil || up != c.follows {
			t.Errorf("%s: it follows %q, want %q: %v", name, up, c.follows, err)
		}
		repo, _, place, err := fixPlaceOf(ctx, st, got)
		if err != nil || repo != c.repo || (place != "") != (c.follows == "" && c.repo == "") {
			t.Errorf("%s: its own record: repo %q place %q: %v", name, repo, place, err)
		}
		log, _ := st.CaseLog(ctx, got.ID)
		said := ""
		for _, e := range log {
			if e.Action == notTakenAction {
				said = e.Detail
			}
		}
		if c.why == "" && said != "" || c.why != "" && !strings.Contains(said, c.why) || strings.Contains(said, "AKIA") {
			t.Errorf("%s: the log says %q, want %q", name, said, c.why)
		}
	}
	// the follower still follows, and what it waits for says who makes the fix
	if up, _ := followsOf(ctx, st, follower); up != root.ID {
		t.Fatalf("the follower follows %q", up)
	}
	if followHold(placed.ID, false) == followHold(placed.ID, true) || !strings.Contains(followHold(placed.ID, false), "a person makes") {
		t.Fatalf("the hold of a case that follows a person's fix: %q", followHold(placed.ID, false))
	}
	n := waitingRepoNote(follower, followHold(placed.ID, false), time.Now())
	if strings.Contains(n.Detail, "built under") || !strings.Contains(n.Detail, "a person's to make") {
		t.Fatalf("the note of a case that follows a person's fix: %q", n.Detail)
	}
}

// The cases of the investigated case's own family are shown first.
func TestTheCasesShownStartWithTheFamilysOwn(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	mine := rootCase(t, st, "same-family")
	other := report(t, st, openCase(t, st, "sessions", "other-family", 1), `{"mechanism":"m2","fix":"f2","summary":"s","repo":"momentum"}`)
	text, err := siblingsText(ctx, st, openCase(t, st, "failures", "asks", 1))
	if a, b := strings.Index(text, "case "+mine.ID), strings.Index(text, "case "+other.ID); err != nil || a < 0 || b < 0 || a > b {
		t.Fatalf("the order of the cases shown: %d %d %v\n%s", a, b, err, text)
	}
}

// What becomes of a case that follows another is read from that case at each pass: it closes with a root that
// closed with its problem gone, and it is investigated again when the root closed any other way. A dry pass
// changes nothing.
func TestAFollowerEndsWithItsRoot(t *testing.T) {
	ctx := context.Background()
	for _, reason := range store.CloseReasons {
		if want := reason == store.Improved || reason == store.Guarded || reason == store.Solved; rootGone(reason) != want {
			t.Fatalf("a root closed %s: gone %v", reason, rootGone(reason))
		}
	}
	for name, c := range map[string]struct {
		reason store.CloseReason
		closed bool
	}{
		"already solved": {store.Solved, true},
		"improved":       {store.Improved, true},
		"guarded":        {store.Guarded, true},
		"not worth it":   {store.NotWorthIt, false},
		"insufficient":   {store.Insufficient, false},
	} {
		st := testStore(t)
		r := localRunner(t, st)
		r.Ship = &Ship{Store: st, Dir: r.Dir}
		root := rootCase(t, st, "the-root")
		f := report(t, st, openCase(t, st, "failures", "follower", 1), `{"mechanism":"m","fix":"f","summary":"s","same_as":"`+root.ID+`"}`)
		if _, _, why, err := r.pick(ctx, f.Family, f.ID, false); err != nil || !strings.Contains(why, "follows") {
			t.Fatalf("%s: setup: %q %v", name, why, err)
		}
		// while the root is open a dry pass writes no hold and no note
		dryRoot := rootCase(t, st, "a-second-root")
		waits := report(t, st, openCase(t, st, "failures", "dry", 1), `{"mechanism":"m","fix":"f","summary":"s","same_as":"`+dryRoot.ID+`"}`)
		if _, _, why, err := r.pick(ctx, waits.Family, waits.ID, true); err != nil || !strings.Contains(why, "follows") {
			t.Fatalf("%s: a dry pass over a follower: %q %v", name, why, err)
		}
		if _, ok, _ := st.Note(ctx, repoNoteID(waits.ID)); ok {
			t.Fatalf("%s: a dry pass wrote the follower's note", name)
		}
		if c.reason == store.Improved || c.reason == store.Guarded { // only the measure tool closes so
			db, err := sql.Open("sqlite", st.Path())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("UPDATE cases SET status = ?, reason = ? WHERE id = ?", store.CaseClosed, c.reason, root.ID); err != nil {
				t.Fatal(err)
			}
			db.Close()
		} else if _, err := st.CloseCase(ctx, root.ID, c.reason, "", "the owner closed it", "test"); err != nil {
			t.Fatalf("%s: closing the root: %v", name, err)
		}
		if _, _, _, err := r.pick(ctx, f.Family, f.ID, true); err != nil {
			t.Fatal(err)
		}
		if got, _ := st.CaseByID(ctx, f.ID); got.Status != store.CaseTesting {
			t.Fatalf("%s: a dry pass moved the follower: %s", name, got.Status)
		}
		picked, step, why, err := r.pick(ctx, f.Family, f.ID, false)
		got, _ := st.CaseByID(ctx, f.ID)
		log, _ := st.CaseLog(ctx, f.ID)
		note, _, _ := st.Note(ctx, repoNoteID(f.ID))
		if err != nil || store.HoldOf(got, log) != "" || note.Done.IsZero() {
			t.Fatalf("%s: the hold stands: %q, note done %v: %v", name, store.HoldOf(got, log), note.Done, err)
		}
		if c.closed {
			said := false
			for _, e := range log {
				said = said || strings.Contains(e.Detail, "its cause was case "+root.ID+"'s, which closed "+string(c.reason))
			}
			if got.Status != store.CaseClosed || got.Reason != store.Solved || !strings.Contains(why, "closed with "+root.ID) || !said {
				t.Fatalf("%s: the follower: %s %s %q %+v", name, got.Status, got.Reason, why, log)
			}
			continue
		}
		ended := false
		for _, e := range log {
			ended = ended || e.Action == followEndedAction && strings.Contains(e.Detail, "case "+root.ID+" closed "+string(c.reason))
		}
		if got.Status != store.CaseInvestigating || !ended {
			t.Fatalf("%s: the follower: %s, ended %v, picked %s %s %q", name, got.Status, ended, picked.ID, step, why)
		}
	}
}

// An investigation's prompt lists the cases its case may follow.
func TestAnInvestigationIsShownTheCasesItMayFollow(t *testing.T) {
	r, c := inputRunner(t)
	root := rootCase(t, r.Store, "the-root")
	newPass(t, r)
	prepared, err := r.prepareRun(context.Background(), c, c.Family, StepInvestigate)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := readHostInput(HostDir(prepared.Dir), "prompt.json")
	if err != nil || !strings.Contains(string(prompt), "case "+root.ID+" (failures)") || !strings.Contains(string(prompt), "report same_as as that case's id") {
		t.Fatalf("the investigation's prompt names no case to follow: %v\n%s", err, prompt)
	}
}
