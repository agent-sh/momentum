package main

import (
	"context"
	"fmt"
	"github.com/agent-sh/momentum/internal/capacity"
	"github.com/agent-sh/momentum/internal/config"
	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

func toolchains(home string) (ro, path []string, goRoot, goModules string) {
	add := func(dir string) {
		if dir != "" && !slices.Contains(ro, dir) {
			if st, err := os.Stat(dir); err == nil && st.IsDir() {
				ro = append(ro, dir)
			}
		}
	}
	if out, err := commandOutput(exec.Command("go", "env", "GOROOT", "GOMODCACHE"), false); err == nil {
		f := strings.Fields(string(out))
		if len(f) == 2 {
			goRoot = f[0]
			if st, err := os.Stat(f[1]); err == nil && st.IsDir() {
				goModules = f[1]
			}
			add(f[0])
			add(f[1])
			path = append(path, filepath.Join(f[0], "bin"))
		}
	}
	for _, d := range []string{".cargo/bin", ".cargo/registry", ".cargo/git", ".rustup", ".nvm"} {
		add(filepath.Join(home, d))
	}
	path = append(path, filepath.Join(home, ".cargo", "bin"))
	if p, err := exec.LookPath("node"); err == nil {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			path = append(path, filepath.Dir(real))
		}
	}
	return ro, path, goRoot, goModules
}

func gitGlobal(key string) string {
	cmd := exec.Command("git", append(loop.GitSafety(), "config", "--global", key)...)
	cmd.Dir = "/" // identity reads must never discover a caller's repository
	cmd.Env = loop.CleanEnv(nil)
	out, _ := commandOutput(cmd, false)
	return strings.TrimSpace(string(out))
}

// repoBusy names what works in a repository now, or "": a live agent session (any phase but done) in one of its
// work trees, or above one whose calls of the last hour name it; a process with its working directory in one; a
// change to its tracked files not committed; or a commit, a checkout or a staging in one in the last hour. The
// loop's own trees and processes are its own.
func repoBusy(ctx context.Context, st *store.Store, dir string) (string, error) {
	ss, err := sessionsByPID(ctx, st)
	if err != nil {
		return "", err
	}
	return busyIn(ctx, st, dir, ss)
}

// busyIn is repoBusy over the live sessions given.
func busyIn(ctx context.Context, st *store.Store, dir string, ss map[int]capacity.Session) (string, error) {
	out, err := loop.HostGitRead(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("git worktree list: %w", err)
	}
	own := loopDir()
	var trees []string
	for _, l := range strings.Split(string(out), "\n") {
		if t, ok := strings.CutPrefix(l, "worktree "); ok && !within(t, own) {
			trees = append(trees, t)
		}
	}
	home := paths.Home()
	for _, t := range trees {
		for _, s := range ss {
			switch {
			case s.Cwd == "" || within(s.Cwd, own):
			case within(s.Cwd, t):
				return fmt.Sprintf("session %s (%s) works in %s", firstText(s.Name, s.Key), s.Tool, t), nil
			case within(t, s.Cwd) && s.ID != "":
				// a session above the tree: its last hour's calls say whether it works in it
				calls, err := st.Events(ctx, store.Query{Since: time.Now().Add(-time.Hour), Session: s.ID, Kinds: []string{"tool_call"},
					Source: store.Transcripts, Limit: 2000})
				if err != nil {
					return "", err
				}
				rel, _ := filepath.Rel(s.Cwd, t)
				names := []string{t, "~/" + strings.TrimPrefix(strings.TrimPrefix(t, home), "/"), rel}
				for _, e := range calls {
					for _, n := range names {
						if n != "" && n != "." && strings.Contains(e.Summary, n) {
							return fmt.Sprintf("session %s (%s) works in %s from %s", firstText(s.Name, s.Key), s.Tool, t, s.Cwd), nil
						}
					}
				}
			}
		}
	}
	self := os.Getpid()
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		var pid int
		if _, err := fmt.Sscanf(e.Name(), "%d", &pid); err != nil || pid == self {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd"))
		if err != nil || within(cwd, own) {
			continue
		}
		for _, t := range trees {
			if within(cwd, t) {
				comm, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
				return fmt.Sprintf("%s (pid %d) in %s", strings.TrimSpace(string(comm)), pid, t), nil
			}
		}
	}
	for _, t := range trees {
		gitdir := filepath.Join(t, ".git")
		if b, err := os.ReadFile(gitdir); err == nil && strings.HasPrefix(string(b), "gitdir: ") {
			gitdir = strings.TrimSpace(strings.TrimPrefix(string(b), "gitdir: "))
		}
		for _, f := range []string{"index", "HEAD", "logs/HEAD"} {
			if st, err := os.Stat(filepath.Join(gitdir, f)); err == nil && time.Since(st.ModTime()) < time.Hour {
				return fmt.Sprintf("a change in %s in the last hour", t), nil
			}
		}
		// his tree's status, read without the lock that would rewrite its index
		status, err := loop.HostGitRead(ctx, t, "status", "--porcelain", "--untracked-files=no")
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(status) != "" {
			return fmt.Sprintf("uncommitted changes in %s", t), nil
		}
	}
	return "", nil
}

func setupShip(st *store.Store, sources config.Sources) *loop.Ship {
	c := sources.Loop
	home := paths.Home()
	ro, toolPath, goRoot, goModules := toolchains(home)
	sb := loop.Sandbox{Home: home, ReadOnly: append(ro, expandAll(c.SandboxRead)...), Cache: filepath.Join(loopDir(), "cache"), Scratch: filepath.Join(loopDir(), "tmp"), Path: toolPath, GoRoot: goRoot, GoModules: goModules}
	if len(c.CapacityGate) > 0 {
		sb.Gate = func(ctx context.Context) error {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(cctx, c.CapacityGate[0], c.CapacityGate[1:]...)
			cmd.Env = loop.CleanEnv(nil)
			if _, err := commandOutput(cmd, true); err != nil {
				return fmt.Errorf("the machine has no room for the test now: %w", err)
			}
			return nil
		}
	}
	return &loop.Ship{Store: st, SourceLive: sources.SourceLive, Dir: loopDir(), InstalledRoots: expandAll(c.ReadDirs), Live: c.Live, Actor: caseActor(), Identity: [2]string{gitGlobal("user.name"), gitGlobal("user.email")}, Sandbox: sb,
		Gate: loop.Gate{Owners: c.Owners, Exclude: c.Exclude, RepoGuard: c.RepoGuard, Home: home, Busy: func(ctx context.Context, dir string) (string, error) { return repoBusy(ctx, st, dir) }}}
}
