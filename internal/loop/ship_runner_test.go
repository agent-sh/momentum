package loop

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/agent-sh/momentum/internal/store"
)

func reviewable(t *testing.T, s *Ship, caseID string) Package {
	t.Helper()
	f := newRepo(t, map[string]string{"x.go": "package fake\n"})
	s.Gate = recoveryShip(t, s.Store, f).Gate
	s.Identity = [2]string{"Owner Name", "owner@example.com"}
	p, err := s.Prepare(context.Background(), caseID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "x.go"), "package fake\n\n// X is x.\nconst X = 1\n")
	p = fixtureCandidate(t, s, p)
	// The review gate resolves a forge-shaped source URL without using the local bare remote or the network.
	run(t, p.Dir, "git", "remote", "set-url", "origin", "https://github.com/agent-sh/fake.git")
	p.PR = "https://example.invalid/pull/1"
	return p
}

func reviewGate(t *testing.T) Gate {
	return Gate{Owners: []string{"agent-sh"}, RepoGuard: []string{script(t, t.TempDir(), "guard", `echo '{"allowed":true}'`)}}
}

func TestACaseWhoseChangeWaitsOnItsReviewIsNotBuilt(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	r := runnerFor(t, st, fakeAgent(t, okResult))
	c := openCase(t, st, "failures", "a", 1)
	mech, next := "m", "n"
	if _, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	c, _ = st.CaseByID(ctx, c.ID)
	r.Ship = &Ship{Store: st, Dir: r.Dir}
	setCaseRepo(t, st, c, "main")
	p := Package{Case: c.ID, Kind: "repo", State: Proposed, Head: "abc123"}
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if got, step, why, err := r.pick(ctx, "failures", "", false); err != nil || got.ID != "" {
		t.Fatalf("a proposed change picked for %s: %s %v", step, why, err)
	}
	p.State = Revising
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	if got, step, _, err := r.pick(ctx, "failures", "", false); err != nil || got.ID != c.ID || step != StepBuild {
		t.Fatalf("a change sent back by its review: %s %s %v", got.ID, step, err)
	}
}

func TestReviewsPassOverACaseWithARunInFlight(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{"unreviewed", "pending", "clean"} {
		t.Run(state, func(t *testing.T) {
			r, c, p := reviewFixture(t)
			if state != "unreviewed" {
				p.Reviews = []Review{{Head: p.Head, Text: "clean"}}
			}
			if state == "clean" {
				p.Reviews[0].Comment = "https://example.invalid/receipt"
			}
			if err := r.Ship.save(&p); err != nil {
				t.Fatal(err)
			}
			reservation, why := r.reserve(ctx, c.Family, c.ID, StepBuild)
			if why != "" {
				t.Fatal(why)
			}
			posts := 0
			r.Post = func(context.Context, string, string, string) (string, error) {
				posts++
				return "https://example.invalid/review", nil
			}
			done := r.reviews(ctx, "")
			runs, err := r.Store.LoopRuns(ctx, store.RunFilter{Case: c.ID})
			if err != nil || len(runs) != 1 || runs[0].ID != reservation.ID || posts != 0 {
				t.Fatalf("review crossed reservation: %+v posts=%d %v", runs, posts, err)
			}
			if len(done) != 1 || !strings.Contains(done[0].Skipped, "in flight") {
				t.Fatalf("not skipped: %+v", done)
			}
		})
	}
}

func TestAReviewPostRechecksRepositoryAuthority(t *testing.T) {
	ctx := context.Background()
	for _, reason := range []string{"owner", "excluded", "busy", "scope", "identity"} {
		t.Run(reason, func(t *testing.T) {
			r, c, p := reviewFixture(t)
			p.Reviews = []Review{{Head: p.Head, Text: "clean"}}
			if err := r.Ship.save(&p); err != nil {
				t.Fatal(err)
			}
			gate := r.Ship.Gate
			switch reason {
			case "owner":
				r.Ship.Gate.Owners = nil
			case "excluded":
				r.Ship.Gate.Exclude = []string{p.Repo}
			case "busy":
				r.Ship.Gate.Busy = func(context.Context, string) (string, error) { return "fixture", nil }
			case "scope":
				r.Ship.Gate.RepoGuard = []string{"false"}
			case "identity":
				r.Ship.Gate.Resolve = func(context.Context, string) (string, error) { return "agent-sh/other", nil }
			}
			posts := 0
			r.Post = func(context.Context, string, string, string) (string, error) {
				posts++
				return "https://example.invalid/receipt", nil
			}
			// The first post and the scan's retry use the same boundary.
			if _, err := r.postReview(ctx, p); err == nil {
				t.Fatal("first post was authorized")
			}
			r.reviews(ctx, "")
			got, _, err := r.Ship.Load(c.ID)
			runs, runErr := r.Store.LoopRuns(ctx, store.RunFilter{Case: c.ID})
			if err != nil || runErr != nil || posts != 0 || got.Clean() || len(got.Reviews) != 1 || len(runs) != 0 {
				t.Fatalf("refusal spent or posted: posts=%d reviews=%d runs=%d %v %v", posts, len(got.Reviews), len(runs), err, runErr)
			}
			r.Ship.Gate = gate
			r.reviews(ctx, "")
			got, _, err = r.Ship.Load(c.ID)
			if err != nil || posts != 1 || !got.Clean() {
				t.Fatalf("pending review lost: posts=%d clean=%v %v", posts, got.Clean(), err)
			}
		})
	}
}

func TestALandedPackageFinishesItsDeployment(t *testing.T) {
	ctx := context.Background()
	for _, resumed := range []bool{false, true} {
		t.Run(fmt.Sprint(resumed), func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "store.db")
			st, err := store.Open(file)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			f := newRepo(t, nil)
			s := shipFor(t, st, f)
			c, p := reviewedChange(t, s, st, f)
			if resumed {
				run(t, p.Host, "git", "push", "origin", "HEAD:main")
				p.State, p.LandingHead = Landing, p.Head
				if err := s.save(&p); err != nil {
					t.Fatal(err)
				}
			}
			db, err := sql.Open("sqlite", file)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`CREATE TRIGGER fail_deploy BEFORE UPDATE OF status ON cases WHEN NEW.status='deployed' BEGIN SELECT RAISE(ABORT, 'fixture interruption'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Land(ctx, c.ID); err == nil {
				t.Fatal("deployment should fail")
			}
			landed, _, err := s.Load(c.ID)
			if err != nil || landed.State != Landed || landed.LandedAt.IsZero() {
				t.Fatalf("landing not saved: %+v %v", landed, err)
			}
			if _, err := db.Exec(`DROP TRIGGER fail_deploy`); err != nil {
				t.Fatal(err)
			}
			r := runnerFor(t, st, nil)
			r.Ship = s
			if picked, _, _, err := r.pick(ctx, c.Family, c.ID, true); err != nil || picked.ID != "" {
				t.Fatalf("dry pick: %s %v", picked.ID, err)
			}
			got, err := st.CaseByID(ctx, c.ID)
			if err != nil || got.Status != store.CaseTesting {
				t.Fatalf("dry deployed: %s %v", got.Status, err)
			}
			if picked, _, _, err := r.pick(ctx, c.Family, c.ID, false); err != nil || picked.ID != "" {
				t.Fatalf("picked landed case: %s %v", picked.ID, err)
			}
			got, err = st.CaseByID(ctx, c.ID)
			if err != nil || got.Status != store.CaseDeployed || got.Deployed.UnixMilli() != landed.LandedAt.UnixMilli() {
				t.Fatalf("deployment lost: %+v %v", got, err)
			}
			if picked, _, _, err := r.pick(ctx, c.Family, c.ID, false); err != nil || picked.ID != "" {
				t.Fatalf("picked deployed case: %s %v", picked.ID, err)
			}
		})
	}
}
