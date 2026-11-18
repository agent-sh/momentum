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
	closed := rootCase(t, st, "closed-root")
	if _, err := st.CloseCase(ctx, closed.ID, store.NotWorthIt, "", "declined", "test"); err != nil {
		t.Fatal(err)
	}
	blank, fixless := rootCase(t, st, "no-mechanism"), rootCase(t, st, "no-fix")
	db, err := sql.Open("sqlite", st.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for id, column := range map[string]string{blank.ID: "mechanism", fixless.ID: "next_test"} {
		if res, err := db.Exec("UPDATE cases SET "+column+" = '' WHERE id = ?", id); err != nil {
			t.Fatal(err)
		} else if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("setup: %s of %s", column, id)
		}
	}
	for name, c := range map[string]struct {
		of            store.Case
		body          string
		follows, repo string
		why           string
	}{
		"itself":                                  {openCase(t, st, "failures", "self", 1), `"same_as":"SELF"`, "", "", "names the case itself"},
		"a case that is not there":                {openCase(t, st, "failures", "ghost", 1), `"same_as":"c00000000"`, "", "", "no case c00000000"},
		"no case's id":                            {openCase(t, st, "failures", "words", 1), `"same_as":"the other case, token AKIAABCDEXAMPLE"`, "", "", "names no case's id"},
		"a case not investigated yet":             {openCase(t, st, "failures", "early", 1), `"same_as":"` + fresh.ID + `"`, "", "", "is open"},
		"a closed case":                           {openCase(t, st, "failures", "late", 1), `"same_as":"` + closed.ID + `"`, "", "", "is closed"},
		"a case with no cause on record":          {openCase(t, st, "failures", "blank", 1), `"same_as":"` + blank.ID + `"`, "", "", "no cause and fix on record"},
		"a case with no fix on record":            {openCase(t, st, "failures", "fixless", 1), `"same_as":"` + fixless.ID + `"`, "", "", "no cause and fix on record"},
		"two cases":                               {openCase(t, st, "failures", "two", 1), `"same_as":"not ` + root.ID + ` but ` + placed.ID + `"`, "", "", "more than one case"},
		"a case that follows another":             {openCase(t, st, "failures", "chain", 1), `"same_as":"` + follower.ID + `"`, "", "", "itself follows case " + root.ID},
		"a case that is followed":                 {root, `"repo":"momentum","same_as":"OTHER"`, "", "momentum", "follows this case"},
		"a component-health case":                 {health, `"same_as":"` + root.ID + `"`, "", "", "component-health"},
		"a case with nowhere its fix is made":     {openCase(t, st, "failures", "to-nowhere", 1), `"same_as":"` + nowhere.ID + `"`, "", "", "nowhere its fix is made"},
		"its own repository, a root with a place": {openCase(t, st, "failures", "own-repo", 1), `"repo":"momentum","same_as":"` + placed.ID + `"`, "", "momentum", "no repository to build in"},
		// taken
		"a root with a place":              {openCase(t, st, "failures", "to-place", 1), `"same_as":"` + placed.ID + `"`, placed.ID, "", ""},
		"its own repository, a built root": {openCase(t, st, "failures", "both", 1), `"repo":"momentum","same_as":"` + root.ID + `"`, root.ID, "", ""},
		"an id among other words":          {openCase(t, st, "failures", "forms", 1), `"same_as":"case_` + strings.ToUpper(root.ID) + ` (failures), ` + root.ID + `."`, root.ID, "", ""},
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
		// a follow that left the report's own repository out keeps it in a line
		kept := ""
		for _, e := range log {
			if e.Action == ownPlaceAction {
				kept = e.Detail
			}
		}
		if want := c.follows != "" && strings.Contains(c.body, `"repo"`); want != strings.Contains(kept, "repo momentum") {
			t.Errorf("%s: the line of what the report named for itself: %q", name, kept)
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
	if err != nil || !strings.Contains(string(prompt), "case "+root.ID+" (failures, testing; its fix is made in repository momentum)") ||
		!strings.Contains(string(prompt), "report same_as as that one case's id") {
		t.Fatalf("the investigation's prompt names no case to follow: %v\n%s", err, prompt)
	}
}

// A case with nowhere its fix is made is not shown as one to follow.
func TestACaseWithNowhereItsFixIsMadeIsNotShown(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	placed := placedCase(t, st, "a-person's-fix")
	nowhere := report(t, st, openCase(t, st, "failures", "no-place", 1), `{"mechanism":"m","fix":"f","summary":"s"}`)
	text, err := siblingsText(ctx, st, openCase(t, st, "failures", "asks", 1))
	if err != nil || strings.Contains(text, nowhere.ID) || !strings.Contains(text, "case "+placed.ID+" (failures, testing; its fix is made in a place outside the loop's repositories, for a person)") {
		t.Fatalf("the cases shown: %v\n%s", err, text)
	}
}

// A follower follows its root as the root stood when it was followed. It waits through a root sent back to be
// investigated, stays with a root that concluded the same, and is investigated again when the root's investigation
// records another cause or fix, when the root reopened, or when the root came to have nowhere its fix is made.
// Its hold says who makes the root's fix.
func TestAFollowerIsReadAgainstWhatItsRootHoldsNow(t *testing.T) {
	ctx := context.Background()
	follow := func(t *testing.T, root func(*store.Store) store.Case, own string) (*Runner, *store.Store, store.Case, store.Case, func() (string, string, store.Case)) {
		t.Helper()
		st := testStore(t)
		r := localRunner(t, st)
		r.Ship = &Ship{Store: st, Dir: r.Dir}
		o := root(st)
		f := report(t, st, openCase(t, st, "failures", "follower", 1), `{"mechanism":"m","fix":"f","summary":"s",`+own+`"same_as":"`+o.ID+`"}`)
		if up, _ := followsOf(ctx, st, f); up != o.ID {
			t.Fatalf("setup: the follower follows %q", up)
		}
		return r, st, o, f, func() (string, string, store.Case) {
			t.Helper()
			_, _, why, err := r.pick(ctx, f.Family, f.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := st.CaseByID(ctx, f.ID)
			log, _ := st.CaseLog(ctx, f.ID)
			return why, store.HoldOf(got, log), got
		}
	}
	built := func(st *store.Store) store.Case { return rootCase(t, st, "the-root") }
	sentBack := func(st *store.Store, o store.Case, body string) {
		t.Helper()
		if _, err := st.UpdateCase(ctx, o.ID, store.CaseUpdate{Status: store.CaseInvestigating}, "test"); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			o, _ = st.CaseByID(ctx, o.ID)
			report(t, st, o, body)
		}
	}
	// a root with a place: the hold says a person makes its fix
	_, _, o, _, pick := follow(t, func(st *store.Store) store.Case { return placedCase(t, st, "a-person's-fix") }, `"place":"somewhere",`)
	if why, hold, _ := pick(); !strings.Contains(why, "follows "+o.ID) || hold != followHold(o.ID, false) {
		t.Fatalf("a follower of a person's fix: %q %q", why, hold)
	}
	// a root sent back: the follower waits; the same conclusion again: it stays
	_, st, o, _, pick := follow(t, built, `"repo":"momentum",`)
	sentBack(st, o, "")
	if why, hold, got := pick(); !strings.Contains(why, "follows "+o.ID) || hold != followHold(o.ID, true) || got.Status != store.CaseTesting {
		t.Fatalf("a root being investigated again: %q %q %s", why, hold, got.Status)
	}
	// a run cut short leaves the root investigating with another mechanism in its findings: it has not concluded
	if got, recorded, err := recordFindings(ctx, st, o.ID, o.Episode, finding{Mechanism: "a mechanism half found"}, false, "", "loop:cut"); err != nil || !recorded ||
		got.Status != store.CaseInvestigating || causeSum(got) == causeSum(o) {
		t.Fatalf("setup: the cut run: %s %v %v", got.Status, recorded, err)
	}
	if why, _, got := pick(); !strings.Contains(why, "follows "+o.ID) || got.Status != store.CaseTesting {
		t.Fatalf("a root whose investigation was cut short: %q %s", why, got.Status)
	}
	o, _ = st.CaseByID(ctx, o.ID)
	report(t, st, o, `{"mechanism":"the hook reviews each edit alone","fix":"review the edits of one message together","summary":"s","repo":"momentum"}`)
	if why, _, got := pick(); !strings.Contains(why, "follows "+o.ID) || got.Status != store.CaseTesting {
		t.Fatalf("a root that concluded the same: %q %s", why, got.Status)
	}
	// another cause: the follower is investigated again, and does not close with the root later
	sentBack(st, o, `{"mechanism":"a different cause entirely","fix":"another fix","summary":"s","repo":"momentum"}`)
	if why, hold, got := pick(); !strings.Contains(why, "holds another cause or fix now") || hold != "" || got.Status != store.CaseInvestigating {
		t.Fatalf("a root that holds another cause: %q %q %s", why, hold, got.Status)
	}
	// a root that came to have nowhere its fix is made, with the cause it had
	_, st, o, _, pick = follow(t, built, `"repo":"momentum",`)
	if err := st.CaseNote(ctx, o.ID, repoAction, fmt.Sprintf(`{"repo":"","episode":%d}`, o.Episode), "test"); err != nil {
		t.Fatal(err)
	}
	if why, _, got := pick(); !strings.Contains(why, "nowhere its fix is made now") || got.Status != store.CaseInvestigating {
		t.Fatalf("a root with nowhere its fix is made: %q %s", why, got.Status)
	}
	// a root that closed and reopened: another episode
	_, st, o, _, pick = follow(t, built, `"repo":"momentum",`)
	if _, err := st.CloseCase(ctx, o.ID, store.Solved, "", "fixed by hand", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReopenCase(ctx, o.ID, []store.Evidence{{Ref: "file:/t/again", At: time.Now()}}, "it came back", "test"); err != nil {
		t.Fatal(err)
	}
	if why, _, got := pick(); !strings.Contains(why, "reopened since") || got.Status != store.CaseInvestigating {
		t.Fatalf("a root that reopened: %q %s", why, got.Status)
	}
	// a root that is gone from the store, and one that came to follow a case itself
	_, st, o, f, pick := follow(t, built, `"repo":"momentum",`)
	if err := st.CaseNote(ctx, f.ID, repoAction, fmt.Sprintf(`{"repo":"","follows":"c00000000","episode":%d}`, f.Episode), "test"); err != nil {
		t.Fatal(err)
	}
	if why, _, got := pick(); !strings.Contains(why, "is not in the store") || got.Status != store.CaseInvestigating {
		t.Fatalf("a root that is not there: %q %s", why, got.Status)
	}
	_, st, o, f, pick = follow(t, built, `"repo":"momentum",`)
	other := rootCase(t, st, "another-root")
	if err := st.CaseNote(ctx, o.ID, repoAction, fmt.Sprintf(`{"repo":"","follows":"%s","episode":%d}`, other.ID, o.Episode), "test"); err != nil {
		t.Fatal(err)
	}
	if why, _, got := pick(); !strings.Contains(why, "now follows case "+other.ID) || got.Status != store.CaseInvestigating {
		t.Fatalf("a root that follows a case itself: %q %s", why, got.Status)
	}
	_ = f
}
