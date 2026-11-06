package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/loop"
)

// gitState is every file of a checkout's Git directory with its size and time: what a fetch into it would change.
func gitState(t *testing.T, dir string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(filepath.Join(dir, ".git"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		state[p] = fmt.Sprintf("%s %d %s", info.Mode(), info.Size(), info.ModTime().Format(time.RFC3339Nano))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// The loop reads a repository at its origin's default branch by a fetch of its own, into a mirror in the loop's
// storage. The checkout it reads keeps its HEAD, index, refs and every time in its Git directory, so the busy check,
// which takes a change there for someone at work, still finds it idle.
func TestReadingARepositoryLeavesItsCheckoutIdle(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	scrub := filepath.Join(t.TempDir(), "scrub")
	os.WriteFile(scrub, []byte("#!/bin/sh\nsed -e 's/AKIA[A-Z0-9]*EXAMPLE/[masked]/g' -e 's/marker-7f3a/[masked]/g'\n"), 0o755)
	was := loop.Scrubber
	loop.Scrubber = []string{scrub}
	t.Cleanup(func() { loop.Scrubber = was })
	ctx := context.Background()
	root := t.TempDir()
	origin, repo, other := filepath.Join(root, "origin.git"), filepath.Join(root, "shop"), filepath.Join(root, "other")
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", origin)
	gitIn(t, root, "clone", "-q", origin, repo)
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o600)
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "first")
	gitIn(t, repo, "push", "-q", "origin", "HEAD:main")
	gitIn(t, repo, "remote", "set-head", "origin", "main")
	gitIn(t, root, "clone", "-q", origin, other)
	os.WriteFile(filepath.Join(other, "b.txt"), []byte("b\n"), 0o600)
	gitIn(t, other, "add", "-A")
	gitIn(t, other, "commit", "-q", "-m", "merged later")
	gitIn(t, other, "push", "-q", "origin", "HEAD:main")
	tip := strings.TrimSpace(gitIn(t, other, "rev-parse", "HEAD"))
	old := time.Now().Add(-2 * time.Hour)
	filepath.WalkDir(filepath.Join(repo, ".git"), func(p string, _ fs.DirEntry, err error) error {
		if err == nil {
			os.Chtimes(p, old, old)
		}
		return nil
	})
	if got, err := busyIn(ctx, nil, repo, nil); err != nil || got != "" {
		t.Fatalf("the fixture is not idle: %q %v", got, err)
	}
	before := gitState(t, repo)
	head := strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD"))
	inputs := t.TempDir()
	got, err := loop.SnapshotRepos(ctx, loop.SnapshotOptions{Sources: []string{repo}, Destination: filepath.Join(inputs, "repos"),
		LogDir: filepath.Join(inputs, "logs"), LoopDirs: []string{loopDir()}, Mirrors: filepath.Join(loopDir(), "mirrors")})
	if err != nil || len(got) != 1 || got[0].Error != "" || got[0].Commit != tip {
		t.Fatalf("the repository was not read at origin's tip %s: %+v %v", tip, got, err)
	}
	if text, err := os.ReadFile(filepath.Join(got[0].Path, "b.txt")); err != nil || string(text) != "b\n" {
		t.Fatalf("the snapshot lacks origin's later file: %v", err)
	}
	after := gitState(t, repo)
	for p, v := range before {
		if after[p] != v {
			t.Errorf("the read changed %s: %q, was %q", strings.TrimPrefix(p, repo), after[p], v)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			t.Errorf("the read added %s", strings.TrimPrefix(p, repo))
		}
	}
	if now := strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD")); now != head {
		t.Fatal("the read moved the checkout's HEAD")
	}
	if got, err := busyIn(ctx, nil, repo, nil); err != nil || got != "" {
		t.Fatalf("the read made the checkout look busy: %q %v", got, err)
	}
}
