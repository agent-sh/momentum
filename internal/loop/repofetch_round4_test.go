package loop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/momentum/internal/store"
)

// clonesCounted puts on PATH a Git that counts the clones it makes, and an ssh whose connection the network closes,
// and returns how many clones there were so far.
func clonesCounted(t *testing.T) func() int {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git here")
	}
	bin := t.TempDir()
	clones := filepath.Join(bin, "clones")
	script(t, bin, "git", `case " $* " in *" clone "*) echo clone >> '`+clones+`';; esac
exec '`+real+`' "$@"`)
	script(t, bin, "ssh", `printf 'Connection closed by 192.0.2.1 port 22\r\n' >&2; exit 255`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return func() int {
		raw, err := os.ReadFile(clones)
		if os.IsNotExist(err) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(raw), "clone")
	}
}

// A preparation held for its base, pass after pass, clones the repository once: the next fetches into the clone the
// held one left. An earlier package's clone goes only once the remote answers.
func TestAHeldPreparationClonesOnce(t *testing.T) {
	for _, kind := range []string{"no clone yet", "an earlier package's clone"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			r, c, f := buildCase(t)
			newPass(t, r)
			want := 1
			var old Package
			if kind == "an earlier package's clone" {
				if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
					t.Fatal(err)
				}
				var err error
				if old, _, err = r.Ship.Load(c.ID); err != nil {
					t.Fatal(err)
				}
				old.State = RolledBack
				if err := r.Ship.save(&old); err != nil {
					t.Fatal(err)
				}
				want = 0
			}
			clones := clonesCounted(t)
			run(t, f.main, "git", "remote", "set-url", "origin", "ssh://git@fixture.invalid/fixture/fake.git")
			for range 3 {
				if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); !held(err) {
					t.Fatalf("an outage did not hold the preparation: %v", err)
				}
			}
			if n := clones(); n != want {
				t.Fatalf("three held preparations made %d clones, want %d", n, want)
			}
			if kind == "an earlier package's clone" {
				run(t, old.Host, "git", "rev-parse", "--verify", "refs/heads/"+old.Branch)
			}
			// the remote answers again: the preparation completes
			run(t, f.main, "git", "remote", "set-url", "origin", f.origin)
			if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
				t.Fatal(err)
			}
			p, _, err := r.Ship.Load(c.ID)
			if err != nil || p.State != Prepared || p.BaseCommit != originHead(t, f, "main") {
				t.Fatalf("the preparation after the outage: %+v %v", p, err)
			}
			if n := clones(); n != 1 {
				t.Fatalf("the preparation and its holds made %d clones, want 1", n)
			}
			if _, err := os.Stat(filepath.Join(p.Host, ".git", heldCloneFile)); !os.IsNotExist(err) {
				t.Fatalf("a completed preparation left its hold's mark: %v", err)
			}
		})
	}
}

// A base note a person took out of the report stays out while its hold lasts; once a build fetched its base, the
// next hold of its class opens it again.
func TestABaseNoteTakenOutStaysOutWhileItsHoldLasts(t *testing.T) {
	ctx := context.Background()
	r, c, f := buildCase(t)
	newPass(t, r)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatal(err)
	}
	p, _, err := r.Ship.Load(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script(t, bin, "ssh", `printf 'Connection closed by 192.0.2.1 port 22\r\n' >&2; exit 255`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	baseNotes := func() (open, done []store.Note) {
		notes, err := r.Store.Notes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range notes {
			if strings.HasPrefix(n.ID, "loop-build:"+store.IDPart(c.ID)+":base:") {
				if n.Done.IsZero() {
					open = append(open, n)
				} else {
					done = append(done, n)
				}
			}
		}
		return open, done
	}
	hold := func() {
		t.Helper()
		run(t, p.Host, "git", "remote", "set-url", "origin", "ssh://git@fixture.invalid/fixture/fake.git")
		_, err := r.prepareRun(ctx, c, c.Family, StepBuild)
		if !held(err) {
			t.Fatalf("a closed connection did not hold the build: %v", err)
		}
		r.buildSkipped(ctx, c, "prepare", err)
	}
	hold()
	open, _ := baseNotes()
	if len(open) != 1 {
		t.Fatalf("the hold's notes: %+v", open)
	}
	if _, _, err := r.Store.DoneNote(ctx, open[0].ID, r.now()); err != nil { // a person takes it out of the report
		t.Fatal(err)
	}
	hold()
	if open, _ := baseNotes(); len(open) != 0 {
		t.Fatalf("a note taken out of the report came back while its hold lasts: %+v", open)
	}
	// a build fetches its base: the hold ended, and the next one is a new hold
	run(t, p.Host, "git", "remote", "set-url", "origin", f.origin)
	if _, err := r.prepareRun(ctx, c, c.Family, StepBuild); err != nil {
		t.Fatal(err)
	}
	hold()
	if open, _ := baseNotes(); len(open) != 1 || !strings.Contains(open[0].Detail, "cannot be fetched now (the connection broke off)") ||
		strings.Contains(open[0].Detail, "It ended") {
		t.Fatalf("a new hold did not open its note again: %+v", open)
	}
}
