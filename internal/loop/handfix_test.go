package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/detect"
	"github.com/agent-sh/momentum/internal/store"
)

// failing puts n sessions' tool results over a span from a time, one an hour apart at most: the first fail of them
// fail the way the failures detector keys as one signature (a web tool's error), the rest are ordinary results.
func failing(t *testing.T, st *store.Store, prefix string, from time.Time, span time.Duration, n, fail int) []store.Event {
	t.Helper()
	var evs []store.Event
	for i := 0; i < n; i++ {
		at := from.Add(time.Duration(i) * span / time.Duration(max(n, 1)))
		e := store.Event{Source: store.Transcripts, SourceKey: fmt.Sprintf("%s%d@%d", prefix, i, at.UnixNano()), TS: at,
			Session: fmt.Sprintf("%s%d", prefix, i), Harness: "claude", Actor: store.ActorTool, Kind: "tool_result", Tool: "WebSearch",
			Summary: "results", Meta: `{"is_error":false}`}
		if i < fail {
			e.Summary, e.Meta = "Error: the search backend is unavailable", `{"is_error":true}`
		}
		evs = append(evs, e)
	}
	if _, err := st.Put(context.Background(), evs); err != nil {
		t.Fatal(err)
	}
	return evs[:fail]
}

// handFixCase is a testing case of the signature the detector keys the failing events by, with them as its evidence,
// and a runner on a clock of the test's own.
func handFixCase(t *testing.T, st *store.Store, sig string, evidence []store.Event, clock *time.Time) (*Runner, store.Case) {
	t.Helper()
	ctx := context.Background()
	f := store.Finding{Family: "failures", Signature: sig, Title: "the search fails"}
	for _, e := range evidence {
		f.Evidence = append(f.Evidence, store.Evidence{Ref: "ev:" + e.ID()})
	}
	c, _, err := st.ObserveCase(ctx, f, DetectorActor)
	if err != nil {
		t.Fatal(err)
	}
	mech, next := "the backend's address is stale", "point the search at internal/search/backend.go's new address"
	if c, err = st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	r := &Runner{Store: st, Dir: dir, Budgets: DefaultBudgets, Detectors: []detect.Detector{detect.Failures{}},
		Ship: &Ship{Store: st, Dir: dir, Identity: [2]string{"Owner Name", "owner@example.com"}}}
	r.Inputs.Home = t.TempDir()
	r.Now = func() time.Time { return *clock }
	r.Ship.Now = r.Now
	return r, c
}

// keyedAs is the signature the detector keys an event by over a window.
func keyedAs(t *testing.T, st *store.Store, e store.Event, since, until time.Time) string {
	t.Helper()
	ks, err := detect.KeyedEvents(context.Background(), st, detect.Window{Since: since, Until: until}, detect.Programs{})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range ks {
		if k.Event.ID() == e.ID() {
			return k.Signature
		}
	}
	t.Fatalf("the detector keys no %s", e.Summary)
	return ""
}

// A fix made by hand closes its case as improved only by measurement: its signature's share of the session hours in
// the week after the fix measurably below the week before. One whose problem goes on is measured, recorded and left
// as it is.
func TestAFixByHandClosesItsCaseOnlyWhenItsRateFell(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		after    int
		improved bool
	}{
		"the problem stopped": {0, true},
		"the problem goes on": {20, false},
	} {
		t.Run(name, func(t *testing.T) {
			st := testStore(t)
			seedHistory(t, st)
			fix := time.Now().Add(-30 * 24 * time.Hour).Truncate(time.Hour)
			clock := fix.Add(HandFixWindow + 2*time.Hour)
			before := failing(t, st, "b", fix.Add(-HandFixWindow), HandFixWindow, 100, 20)
			failing(t, st, "a", fix.Add(time.Minute), HandFixWindow-time.Hour, 100, c.after)
			if err := st.MarkRead(ctx, store.Transcripts, clock); err != nil {
				t.Fatal(err)
			}
			sig := keyedAs(t, st, before[0], fix.Add(-HandFixWindow), fix)
			r, cs := handFixCase(t, st, sig, before, &clock)
			if _, err := st.RecordHandFix(ctx, cs.ID, "git:agent-sh/fake@abcdef1", fix, store.HowNamed, "test"); err != nil {
				t.Fatal(err)
			}
			// a day before its week after has passed, nothing is decided
			early := fix.Add(HandFixWindow - 24*time.Hour)
			r.Now = func() time.Time { return early }
			if ms, err := r.measureWorking(ctx, Options{}); err != nil || len(ms) != 1 || ms[0].Status != "measuring" {
				t.Fatalf("before its week after: %+v %v", ms, err)
			}
			r.Now = func() time.Time { return clock }
			ms, err := r.measureWorking(ctx, Options{})
			if err != nil || len(ms) != 1 {
				t.Fatalf("measured: %+v %v", ms, err)
			}
			got, err := st.CaseByID(ctx, cs.ID)
			if err != nil {
				t.Fatal(err)
			}
			if c.improved {
				if got.Status != store.CaseClosed || got.Reason != store.Improved || !strings.Contains(got.Measurement.Effect, "fixed by hand: git:agent-sh/fake@abcdef1") ||
					got.Measurement.Before != 20 || got.Measurement.After != 0 {
					t.Fatalf("a fix that cut the problem: %+v", got)
				}
				return
			}
			fixes, err := st.HandFixes(ctx, cs.ID)
			if err != nil || got.Status != store.CaseTesting || len(fixes) != 1 || !fixes[0].Measured || ms[0].Status != "no measured effect" {
				t.Fatalf("a fix that changed nothing: %+v %+v %+v %v", got, fixes, ms, err)
			}
			if ms, _ := r.measureWorking(ctx, Options{}); len(ms) != 0 {
				t.Fatalf("a fix measured once is measured again: %+v", ms)
			}
		})
	}
}

// A case whose evidence of the last week the detector no longer keys by its signature closes as a detector fix,
// never as improved, and says which: re-keyed (keyed by another signature now) or dropped (no failure now).
func TestACaseTheDetectorNoLongerKeysClosesAsADetectorFix(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedHistory(t, st)
	clock := time.Now().Truncate(time.Hour)
	evs := failing(t, st, "w", clock.Add(-3*24*time.Hour), 24*time.Hour, 10, 5)
	r, cs := handFixCase(t, st, "failures/v2:tool:websearch:an older form of its words", evs, &clock)
	ms, err := r.measureWorking(ctx, Options{})
	if err != nil || len(ms) != 1 || ms[0].Status != "detector fix" {
		t.Fatalf("measured: %+v %v", ms, err)
	}
	got, err := st.CaseByID(ctx, cs.ID)
	if err != nil || got.Status != store.CaseClosed || got.Reason != store.DetectorFix || !strings.Contains(got.Measurement.Effect, "re-keyed: 5 of its 5 events") {
		t.Fatalf("a re-keyed case: %+v %v", got, err)
	}
	// a case whose events the detector keys as it does stays as it is
	sig := keyedAs(t, st, evs[0], clock.Add(-HandFixWindow), clock)
	_, kept := handFixCase(t, st, sig, evs, &clock)
	if ms, err := r.measureWorking(ctx, Options{}); err != nil || len(ms) != 0 {
		t.Fatalf("a case still keyed: %+v %v", ms, err)
	}
	if got, _ := st.CaseByID(ctx, kept.ID); got.Status != store.CaseTesting {
		t.Fatalf("a case still keyed closed: %+v", got)
	}
	// only the measure tool closes a case so
	if _, err := st.CloseCase(ctx, kept.ID, store.DetectorFix, "", "by hand", "test"); err == nil {
		t.Fatal("a detector fix closed by hand")
	}
}

// A testing case whose investigation names a file outside the loop's repositories sees a fix by hand when that file
// changes after the case went to testing, and records it, once.
func TestAFileTheInvestigationNamesChangingIsAFixByHand(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	clock := time.Now()
	r, cs := handFixCase(t, st, "failures/v2:tool:websearch:x", nil, &clock)
	raw, _ := json.Marshal(map[string]any{"repo": "", "place": "~/mods/hook/register.ts (its Read hook)", "episode": cs.Episode})
	if err := st.CaseNote(ctx, cs.ID, repoAction, string(raw), "fixture"); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(r.Inputs.Home, "mods", "hook", "register.ts")
	write(t, hook, "the hook, fixed")
	for i := 0; i < 2; i++ {
		if err := r.seeHandFixes(ctx, cs); err != nil {
			t.Fatal(err)
		}
	}
	fixes, err := st.HandFixes(ctx, cs.ID)
	if err != nil || len(fixes) != 1 || fixes[0].Ref != "file:~/mods/hook/register.ts" || fixes[0].How != store.HowSeen {
		t.Fatalf("the fix seen: %+v %v", fixes, err)
	}
	// a file that changed before the case went to testing is no fix of it
	old := clock.Add(-time.Hour)
	if err := os.Chtimes(hook, old, old); err != nil {
		t.Fatal(err)
	}
	_, other := handFixCase(t, st, "failures/v2:tool:websearch:y", nil, &clock)
	raw, _ = json.Marshal(map[string]any{"repo": "", "place": "~/mods/hook/register.ts", "episode": other.Episode})
	if err := st.CaseNote(ctx, other.ID, repoAction, string(raw), "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := r.seeHandFixes(ctx, other); err != nil {
		t.Fatal(err)
	}
	if fixes, _ := st.HandFixes(ctx, other.ID); len(fixes) != 0 {
		t.Fatalf("a change from before the case: %+v", fixes)
	}
}
