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

func waitingFixture(t *testing.T) (*Runner, store.Case, Package) {
	t.Helper()
	st := testStore(t)
	r := localRunner(t, st)
	r.Ship = &Ship{Store: st, Dir: r.Dir}
	c := testingCase(t, st)
	p := Package{Case: c.ID, Kind: "repo", Repo: "agent-sh/fake", Base: "main", State: Proposed, Head: "abc", PR: "https://example.invalid/pull/9"}
	reviewRounds(&p, "abc", time.Now().Add(-time.Hour))
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	return r, c, p
}

func reviewRounds(p *Package, head string, last time.Time) {
	p.Reviews = nil
	for i := 1; i <= MaxRounds; i++ {
		p.Reviews = append(p.Reviews, Review{Round: i, Medium: 1, Head: head, Comment: fmt.Sprint("https://example.invalid/comment/", i),
			At: last.Add(-time.Duration(MaxRounds-i) * time.Minute)})
	}
}

func standingNotes(t *testing.T, st *store.Store) (standing []store.Note, all int) {
	t.Helper()
	ns, err := st.Notes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range ns {
		if n.Done.IsZero() {
			standing = append(standing, n)
		}
	}
	return standing, len(ns)
}

func TestAChangeThatUsedItsReviewRoundsLeavesOnePassiveNote(t *testing.T) {
	ctx := context.Background()
	r, c, p := waitingFixture(t)
	if _, err := r.Once(ctx, Options{Dry: true, Families: []string{"failures"}}); err != nil {
		t.Fatal(err)
	}
	if _, all := standingNotes(t, r.Store); all != 0 {
		t.Fatalf("a dry run wrote %d notes", all)
	}
	for i := 0; i < 3; i++ {
		if err := r.noteWaiting(ctx); err != nil {
			t.Fatal(err)
		}
	}
	standing, all := standingNotes(t, r.Store)
	if len(standing) != 1 || all != 1 {
		t.Fatalf("%d notes stand of %d after three loop runs: %+v", len(standing), all, standing)
	}
	n := standing[0]
	if n.ID != "loop-rounds:"+c.ID || n.Kind != store.NoteProblem || n.Importance != store.ImportanceLow || !strings.Contains(n.Title, c.ID) ||
		len(n.Links) != 1 || n.Links[0].URL != p.PR || !strings.Contains(n.Detail, "close the case") {
		t.Fatalf("the note: %+v", n)
	}
	if d, _, _, _ := r.pick(ctx, c.Family, "", false); d.ID != "" {
		t.Fatalf("a change that used its rounds was picked: %+v", d)
	}
	// its reader resolves it: it stays resolved while the change waits
	if _, _, err := r.Store.DoneNote(ctx, n.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.noteWaiting(ctx); err != nil {
		t.Fatal(err)
	}
	if standing, all := standingNotes(t, r.Store); len(standing) != 0 || all != 1 {
		t.Fatalf("a note its reader resolved stands again: %+v", standing)
	}
	// the change is revised and uses its rounds again: a new wait, a note that stands again
	p.Head = "def"
	reviewRounds(&p, "def", time.Now())
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if err := r.noteWaiting(ctx); err != nil {
		t.Fatal(err)
	}
	if standing, all := standingNotes(t, r.Store); len(standing) != 1 || all != 1 {
		t.Fatalf("the second wait: %d stand of %d", len(standing), all)
	}
	// the case leaves that state: the note is done
	if _, err := r.Store.CloseCase(ctx, c.ID, store.NotWorthIt, "", "the change is not worth more rounds", "test"); err != nil {
		t.Fatal(err)
	}
	if err := r.noteWaiting(ctx); err != nil {
		t.Fatal(err)
	}
	if standing, all := standingNotes(t, r.Store); len(standing) != 0 || all != 1 {
		t.Fatalf("the case left the state and its note stands: %+v", standing)
	}
}

func TestOnlyAChangeWithNoRoundLeftIsWaiting(t *testing.T) {
	ctx := context.Background()
	for name, edit := range map[string]func(p *Package){
		"a round left":       func(p *Package) { p.Reviews = p.Reviews[:MaxRounds-1] },
		"clean":              func(p *Package) { p.Reviews[MaxRounds-1].Medium = 0 },
		"clean pending post": func(p *Package) { p.Reviews[MaxRounds-1].Medium = 0; p.Reviews[MaxRounds-1].Comment = "" },
	} {
		t.Run(name, func(t *testing.T) {
			r, _, p := waitingFixture(t)
			edit(&p)
			if err := r.Ship.save(&p); err != nil {
				t.Fatal(err)
			}
			if err := r.noteWaiting(ctx); err != nil {
				t.Fatal(err)
			}
			if _, all := standingNotes(t, r.Store); all != 0 {
				t.Fatalf("a change that waits for nothing has %d notes", all)
			}
		})
	}
}

func TestAPackageReadErrorPreservesItsWaitingNote(t *testing.T) {
	ctx := context.Background()
	r, c, p := waitingFixture(t)
	if err := r.noteWaiting(ctx); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.Ship.pkgDir(c.ID), "package.json")
	if err := os.WriteFile(path, []byte("{marker-7f3a"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs []string
	r.Log = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	// Narrowing case work must not lose another family's unreadable waiting package.
	if err := r.noteWaiting(ctx); err != nil {
		r.logf("waiting packages: %v", err)
	} else {
		t.Fatal("unreadable package was not reported")
	}
	if text := strings.Join(logs, "\n"); !strings.Contains(text, c.ID) || !strings.Contains(text, "package") {
		t.Errorf("package read error was not reported: %q", text)
	}
	if standing, all := standingNotes(t, r.Store); len(standing) != 1 || all != 1 {
		t.Errorf("package read error resolved its note: %+v", standing)
	}
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if err := r.noteWaiting(ctx); err != nil {
		t.Fatal(err)
	}
	if standing, all := standingNotes(t, r.Store); len(standing) != 1 || all != 1 {
		t.Fatalf("unchanged package did not recover its note: %+v", standing)
	}
	// A revised head with no round left still waits: the note stands.
	p.Head = "def"
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if err := r.noteWaiting(ctx); err != nil {
		t.Fatal(err)
	}
	if standing, all := standingNotes(t, r.Store); len(standing) != 1 || all != 1 {
		t.Fatalf("a revised head with no round left stopped waiting: %+v", standing)
	}
	// Only a known transition out of the waiting state resolves it: the last review comes back clean on the head.
	p.Reviews[MaxRounds-1].Head, p.Reviews[MaxRounds-1].Medium = p.Head, 0
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if err := r.noteWaiting(ctx); err != nil {
		t.Fatal(err)
	}
	if standing, all := standingNotes(t, r.Store); len(standing) != 0 || all != 1 {
		t.Fatalf("a clean last review kept the waiting note: %+v", standing)
	}
}
