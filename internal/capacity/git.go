package capacity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// errTimeout means a command did not finish in its time.
var errTimeout = errors.New("timed out")

// gitRun runs git in dir and returns its standard output. It takes no optional lock (a status never refreshes the
// index another session is using), never prompts, and an ssh remote fails instead of asking.
func gitRun(ctx context.Context, timeout time.Duration, dir string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes -o ConnectTimeout=15")
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if cctx.Err() != nil {
		return out.String(), fmt.Errorf("git %s: %w after %s", strings.Join(args[:min(len(args), 2)], " "), errTimeout, timeout)
	}
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args[:min(len(args), 2)], " "), err, msg)
	}
	return out.String(), nil
}

// gitIn runs git in dir with the given standard input.
func gitIn(ctx context.Context, dir, in string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Stdin = strings.NewReader(in)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if cctx.Err() != nil {
			return "", fmt.Errorf("git %s: %w", strings.Join(args[:min(len(args), 2)], " "), errTimeout)
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args[:min(len(args), 2)], " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// gitOK runs git in dir and reports whether it exited 0. An error is a git that could not run or did not finish.
func gitOK(ctx context.Context, dir string, args ...string) (bool, error) {
	_, err := gitRun(ctx, 30*time.Second, dir, args...)
	var ee *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, errTimeout):
		return false, err
	case errors.As(err, &ee):
		return false, nil
	}
	return false, err
}
