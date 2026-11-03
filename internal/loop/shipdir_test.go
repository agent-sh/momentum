package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// Two steps of one case do not overlap: they share its host clone, its exports and its package file. A step that
// finds another running fails at once and says so, and the first runs to its end.
func TestTwoShipStepsOnOneCaseDoNotOverlap(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c := testingCase(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	write(t, filepath.Join(p.Work, "answer_test.go"), answerTest)
	// the first check waits at the capacity gate, holding its lock, until the test lets it go
	var once sync.Once
	entered, release := make(chan struct{}), make(chan struct{})
	s.Sandbox.Gate = func(context.Context) error {
		once.Do(func() { close(entered) })
		<-release
		return nil
	}
	first := make(chan error, 1)
	go func() {
		_, err := checkPatch(t, s, ctx, c.ID, "go test ./...", nil)
		first <- err
	}()
	defer func() { // the first check ends whatever the test found
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	select {
	case <-entered:
	case err := <-first:
		t.Fatalf("the first check ended before its gate: %v", err)
	case <-time.After(time.Minute):
		t.Fatal("the first check never reached its gate")
	}
	second := make(chan error, 1)
	go func() {
		_, err := checkPatch(t, s, ctx, c.ID, "go test ./...", nil)
		second <- err
	}()
	select {
	case err := <-second:
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "another ship step is running on case "+c.ID) {
			t.Fatalf("the second step: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second step on the case ran beside the first, or waited for it, instead of failing at once")
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("the first check: %v", err)
	}
	if unlock, err := s.lock(ctx, c.ID); err != nil {
		t.Fatalf("the lock was not released: %v", err)
	} else {
		unlock()
	}
	if got, _, _ := s.Load(c.ID); got.State != Checked {
		t.Fatalf("the first check's package: %+v", got)
	}
}

// Each of a case's ship steps takes its lock; another case's steps are free, and an id that is no case's makes no
// directory.
func TestEveryShipStepTakesTheCasesLock(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipFor(t, st, f)
	c := testingCase(t, st)
	unlock, err := s.lock(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	steps := map[string]func() error{
		"prepare": func() error { _, err := s.Prepare(ctx, c.ID, f.main); return err },
		"check":   func() error { _, err := checkPatch(t, s, ctx, c.ID, "go test ./...", nil); return err },
		"propose": func() error { _, err := proposePatch(t, s, ctx, c.ID, Proposal{}); return err },
		"land":    func() error { _, err := s.Land(ctx, c.ID); return err },
		"rollback": func() error {
			_, err := s.RollBack(ctx, c.ID, "because")
			return err
		},
	}
	for name, step := range steps {
		if err := step(); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "another ship step is running") {
			t.Errorf("%s with the lock held: %v", name, err)
		}
	}
	if _, err := s.lock(ctx, c.ID); err == nil {
		t.Error("the lock was taken twice")
	}
	unlock()
	if _, err := s.Prepare(ctx, c.ID, f.main); err != nil {
		t.Fatalf("a step after the lock was released: %v", err)
	}
	if _, err := s.lock(ctx, "nosuchcase"); !errors.Is(err, store.ErrNoCase) {
		t.Errorf("an unknown case: %v", err)
	}
	if _, err := s.lock(ctx, "../outside"); !errors.Is(err, ErrRefused) {
		t.Errorf("an id that climbs out: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "packages", "nosuchcase")); err == nil {
		t.Error("a step on no case made a package directory")
	}
}

func TestSubmoduleNamesAreReadAsGitListsThem(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "git", "init", "-q", "-b", "main")
	run(t, dir, "git", "config", "submodule.Vendor.update", "none")
	if err := CheckGitConfig(context.Background(), dir); err != nil {
		t.Fatalf("a capitalised submodule with a mode: %v", err)
	}
	run(t, dir, "git", "config", "submodule.lib.update", "none")
	run(t, dir, "git", "config", "submodule.Lib.update", "!sh ./x")
	if err := CheckGitConfig(context.Background(), dir); !errors.Is(err, ErrRefused) {
		t.Fatalf("a capitalised submodule with a command beside a lower-case one with a mode: %v", err)
	}
}
