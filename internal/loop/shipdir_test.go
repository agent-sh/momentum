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

// Two steps of one case do not overlap: they share its work tree, its scratch trees and its package file. A step that
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
		_, err := s.Check(ctx, c.ID, "go test ./...", nil)
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
		_, err := s.Check(ctx, c.ID, "go test ./...", nil)
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
		"check":   func() error { _, err := s.Check(ctx, c.ID, "go test ./...", nil); return err },
		"propose": func() error { _, err := s.Propose(ctx, c.ID, Proposal{}); return err },
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

// A work tree's directory is emptied in place, never removed: a sandbox that mounts it holds that directory. What is in
// it goes, links among it without what they name, and a missing directory or a file or a link in its place is made a
// directory.
func TestEmptyDirKeepsTheDirectoryItself(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	write(t, filepath.Join(outside, "kept.txt"), "marker-7f3a")
	dir := filepath.Join(root, "work")
	write(t, filepath.Join(dir, ".git"), "gitdir: nowhere")
	write(t, filepath.Join(dir, "a", "b", "file.go"), "package a")
	write(t, filepath.Join(dir, ".hidden"), "x")
	if err := os.Symlink(outside, filepath.Join(dir, "link-to-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "kept.txt"), filepath.Join(dir, "link-to-file")); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := emptyDir(dir); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(dir)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("the directory was replaced: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("the directory holds %d entries", len(entries))
	}
	if b, err := os.ReadFile(filepath.Join(outside, "kept.txt")); err != nil || string(b) != "marker-7f3a" {
		t.Fatalf("what a link named was removed: %q %v", b, err)
	}
	// nothing there, a file there, a link there
	missing := filepath.Join(root, "missing", "deeper")
	file := filepath.Join(root, "file")
	write(t, file, "x")
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{missing, file, link} {
		if err := emptyDir(d); err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		if st, err := os.Lstat(d); err != nil || !st.IsDir() {
			t.Fatalf("%s is no directory: %v", d, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(outside, "kept.txt")); err != nil || string(b) != "marker-7f3a" {
		t.Fatalf("what the link named was changed: %q %v", b, err)
	}
}

// A repository's submodule names are read as git lists them: the subsection of a name is case sensitive. A capitalised
// name with a mode git writes is no program, and a capitalised name with a command is one even beside a lower-case
// name with a mode.
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
