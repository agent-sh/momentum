package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type gitDirectoryKey struct{}
type gitDirectory struct{ work, registration string }

// WithGitDirectory pins host Git to a registration held outside the run's mounts.
func WithGitDirectory(ctx context.Context, work, registration string) context.Context {
	return context.WithValue(ctx, gitDirectoryKey{}, gitDirectory{work, registration})
}

func GitLocation(ctx context.Context, dir string) []string {
	if pin, ok := ctx.Value(gitDirectoryKey{}).(gitDirectory); ok && under(dir, pin.work) {
		return []string{"--git-dir=" + pin.registration, "--work-tree=" + pin.work, "-C", dir}
	}
	return []string{"-C", dir}
}

func gitLocation(ctx context.Context, dir string) []string { return GitLocation(ctx, dir) }

// VerifyGitDirectory opens the link once, checks that exact descriptor and the host-owned backlink.
// Git itself never reads this run-writable link.
func VerifyGitDirectory(work, registration string) error {
	if registration == "" {
		return refused("the package has no recorded Git registration; prepare it again")
	}
	f, err := OpenFile(work, ".git")
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := ReadFile(f, 64<<10)
	if err != nil {
		return err
	}
	if string(b) != "gitdir: "+registration+"\n" {
		return refused("the work tree's .git is not its recorded registration")
	}
	back, err := os.ReadFile(filepath.Join(registration, "gitdir"))
	if err != nil || strings.TrimSpace(string(back)) != filepath.Join(work, ".git") {
		return refused("the Git registration does not link back to this work tree")
	}
	return nil
}

// recordedRegistration reads only Git's host-owned registration directories.
// It does not discover Git by reading the run's .git file, even during prepare.
func recordedRegistration(repo, work string) (string, error) {
	base := filepath.Join(repo, ".git", "worktrees")
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		d := filepath.Join(base, e.Name())
		b, err := os.ReadFile(filepath.Join(d, "gitdir"))
		if err == nil && strings.TrimSpace(string(b)) == filepath.Join(work, ".git") {
			return d, nil
		}
	}
	return "", fmt.Errorf("Git made no registration for this work tree")
}

func (s *Ship) gitContext(ctx context.Context, dir string) (context.Context, error) {
	if !under(dir, filepath.Join(s.Dir, "work")) {
		return ctx, nil
	}
	id := filepath.Base(dir)
	for _, prefix := range []string{".before-", ".after-", ".land-", ".revert-"} {
		id = strings.TrimPrefix(id, prefix)
	}
	p, ok, err := s.Load(id)
	if err != nil || !ok {
		return ctx, refused("no host-owned package for the work tree")
	}
	reg := p.GitDir
	if dir != p.Work {
		// Export registrations are created by Git outside the build's writable mount.
		reg, err = recordedRegistration(p.Dir, dir)
		if err != nil {
			return ctx, err
		}
	}
	if err := VerifyGitDirectory(dir, reg); err != nil {
		return ctx, err
	}
	return WithGitDirectory(ctx, dir, reg), nil
}
