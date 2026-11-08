package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/agent-sh/momentum/internal/capacity"
	"github.com/agent-sh/momentum/internal/config"
	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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
// work trees, or above one whose calls of the last hour act in it (capacity.ActsIn); a process with its working directory in one; a
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
				// a session above the tree works in it when a call of its last hour acts there; one that only reads
				// it, or names the repository, does not
				calls, err := st.Events(ctx, store.Query{Since: time.Now().Add(-time.Hour), Session: s.ID, Kinds: []string{"tool_call"},
					Source: store.Transcripts, Limit: math.MaxInt32}) // every call of the hour: the newest matter most
				if err != nil {
					return "", err
				}
				for _, e := range calls {
					if capacity.ActsIn(e.Tool, e.Summary, firstText(e.Cwd, s.Cwd), t, home) {
						return fmt.Sprintf("session %s (%s) works in %s from %s", firstText(s.Name, s.Key), s.Tool, t, s.Cwd), nil
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

// gateTimeout bounds one wait at the capacity gate. The gate runs in a process group of its own, which is killed whole
// when the gate ends, at its deadline or on its own, so its children (a shell's waiter) end with it; gateWaitDelay
// bounds how long its output may stay open after.
var (
	gateTimeout   = 10 * time.Minute
	gateWaitDelay = 5 * time.Second
)

// gateSaid is the end of what the capacity gate printed, on one line, for the loop's log and capacity note
// (loop.GateSaid): never in the error's text, which becomes case lines.
func gateSaid(out []byte) string {
	text := strings.Join(strings.Fields(string(out)), " ")
	if r := []rune(text); len(r) > 300 {
		text = "..." + string(r[len(r)-300:])
	}
	return text
}

// configTargets is the loop's authority over configuration files as configured, with its own files refused.
func configTargets(c config.Loop) loop.ConfigTargets {
	own := append(expandAll(c.Own()), paths.Config(), paths.Data(), config.SourcesPath())
	return loop.ConfigTargets{Enabled: c.ConfigTargets.Enabled, Allow: c.ConfigTargets.Allow, Home: paths.Home(), Own: own}
}

func setupShip(st *store.Store, sources config.Sources) *loop.Ship {
	c := sources.Loop
	home := paths.Home()
	ro, toolPath, goRoot, goModules := toolchains(home)
	sb := loop.Sandbox{ReadOnly: append(ro, expandAll(c.SandboxRead)...), Cache: filepath.Join(loopDir(), "cache"), Scratch: filepath.Join(loopDir(), "tmp"), Path: toolPath, GoRoot: goRoot, GoModules: goModules}
	if len(c.CapacityGate) > 0 {
		sb.HoldExits = c.CapacityHoldExit
		sb.Gate = func(ctx context.Context) error {
			cctx, cancel := context.WithTimeout(ctx, gateTimeout)
			defer cancel()
			cmd := exec.CommandContext(cctx, c.CapacityGate[0], c.CapacityGate[1:]...)
			cmd.Env = loop.CleanEnv(nil)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
			cmd.WaitDelay = gateWaitDelay
			out, err := commandOutput(cmd, true)
			if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
				err = nil // the gate exited 0, room, though a child it left held its output past the wait delay
			}
			// the gate's children end with it when it exits on its own too; a group with no process left is no error
			if cmd.Process != nil {
				if kill := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); kill != nil && !errors.Is(kill, syscall.ESRCH) {
					err = errors.Join(err, fmt.Errorf("its process group cannot be ended: %w", kill))
				}
			}
			if err == nil {
				return nil
			}
			if ctx.Err() == nil && errors.Is(cctx.Err(), context.DeadlineExceeded) {
				// its own deadline ended the wait: the machine had no room all that time, which the loop holds on
				err = fmt.Errorf("its wait passed %s: %w", gateTimeout, context.DeadlineExceeded)
			}
			return &loop.GateSaid{Err: fmt.Errorf("the capacity gate %s: %w", filepath.Base(c.CapacityGate[0]), err), Said: gateSaid(out)}
		}
	}
	gate := configGate(c)
	gate.Busy = func(ctx context.Context, dir string) (string, error) { return repoBusy(ctx, st, dir) }
	for _, why := range configTargets(c).RefusedAllow() {
		fmt.Fprintf(os.Stderr, "momentum: loop.config_targets.allow names a file that is never a target, %s\n", why)
	}
	return &loop.Ship{Store: st, SourceLive: sources.SourceLive, Dir: loopDir(), InstalledRoots: expandAll(c.ReadDirs), Live: c.Live, Actor: caseActor(),
		ConfigTargets: configTargets(c), Identity: [2]string{gitGlobal("user.name"), gitGlobal("user.email")}, Sandbox: sb,
		Gate: gate}
}
