package loop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// testPrograms are the programs a check may run as its test: the test runners of the languages the owned
// repositories are written in, and Claude Code's runner of a mod's tests. A check runs its command directly, never
// through a shell.
var testPrograms = map[string]bool{"go": true, "cargo": true, "npm": true, "pnpm": true, "bun": true, "npx": true, "node": true,
	"python3": true, "pytest": true, "uv": true, "make": true, "bats": true, "claude": true}

// Words splits a test command into its words as a shell would split plain words and quoted ones, and refuses
// what only a shell gives meaning to (a pipe, a redirection, a substitution, a variable, a glob, a command list):
// the command runs without a shell.
func Words(cmd string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inWord, quote := false, rune(0)
	for _, r := range cmd {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case unicode.IsSpace(r):
			if inWord {
				out, inWord = append(out, cur.String()), false
				cur.Reset()
			}
		case strings.ContainsRune("|&;<>()$`\\*?[]{}~!#", r):
			return nil, refused("the test command %q holds %q, which only a shell reads: give one program and its arguments", cmd, r)
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, refused("the test command %q has an unclosed quote", cmd)
	}
	if inWord {
		out = append(out, cur.String())
	}
	if len(out) == 0 {
		return nil, refused("an empty test command")
	}
	if !testPrograms[out[0]] {
		return nil, refused("%q is not a test runner a check may run (go, cargo, npm, pnpm, bun, npx, node, python3, pytest, uv, make, bats, claude)", out[0])
	}
	return out, nil
}

// Outcome is one run of a check's test.
type Outcome struct {
	Exit    int     `json:"exit"`
	Seconds float64 `json:"seconds"`
	Tail    string  `json:"tail"` // the end of what it printed
}

// Sandbox runs a test in a bubblewrap sandbox: the whole filesystem read-only, but the tree under test, its own
// scratch directory and the build caches the user's toolchains keep; no network; its own process tree, which ends
// with it. A test of a change the loop made can then write nothing outside its tree.
type Sandbox struct {
	Bwrap    string        // the bubblewrap binary
	Writable []string      // the build caches it may write (Go's, Cargo's), when they exist
	Timeout  time.Duration // how long a test may run
	// Gate runs before each test and waits until the machine has room for it (capacity), or returns why not.
	Gate func(ctx context.Context) error
}

// DefaultTestTimeout is how long one run of a check's test may take: a package's suite, not the world's.
const DefaultTestTimeout = 20 * time.Minute

// Run runs words in dir and returns how it went. Its environment is the user's home and path, a scratch
// directory of its own as TMPDIR, and the caches' variables.
func (s Sandbox) Run(ctx context.Context, dir string, words []string, env []string) (Outcome, error) {
	if s.Gate != nil {
		if err := s.Gate(ctx); err != nil {
			return Outcome{}, err
		}
	}
	scratch, err := os.MkdirTemp(filepath.Dir(dir), ".check-")
	if err != nil {
		return Outcome{}, err
	}
	defer os.RemoveAll(scratch)
	args := []string{"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--bind", dir, dir, "--bind", scratch, scratch,
		"--unshare-net", "--unshare-pid", "--die-with-parent", "--new-session", "--chdir", dir}
	for _, w := range s.Writable {
		if st, err := os.Stat(w); err == nil && st.IsDir() {
			args = append(args, "--bind", w, w)
		}
	}
	args = append(append(args, "--"), words...)
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultTestTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, s.Bwrap, args...)
	cmd.Env = append(append([]string{}, env...), "TMPDIR="+scratch, "GIT_TERMINAL_PROMPT=0")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	start := time.Now()
	err = cmd.Run()
	o := Outcome{Seconds: time.Since(start).Seconds(), Tail: tailOf(out.Bytes(), 1500)}
	var ee *exec.ExitError
	switch {
	case cctx.Err() == context.DeadlineExceeded:
		return o, fmt.Errorf("the test ran past %s", timeout)
	case errors.As(err, &ee):
		o.Exit = ee.ExitCode()
	case err != nil:
		return o, fmt.Errorf("the sandbox did not run: %w", err)
	}
	return o, nil
}

func tailOf(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return strings.ToValidUTF8(string(b), "?")
}
