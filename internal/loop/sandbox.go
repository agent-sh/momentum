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

// Sandbox runs a test in a bubblewrap sandbox, every namespace its own and no capability: the root file system
// read-only, with its own /tmp and /run (no socket of the user's session, his agent or a daemon is there) and his
// home empty but for the toolchains it reads (ReadOnly); the tree under test, its scratch directory and the loop's
// own build caches writable, the tree's .git read-only; no network; its own process tree, which ends with it. A
// test of a change the loop made can then write nothing outside its tree, reach no service, and read none of the
// user's files.
type Sandbox struct {
	Bwrap    string        // the bubblewrap binary
	Home     string        // the user's home, which the test sees empty
	ReadOnly []string      // the directories under the home the test reads: its toolchains and their caches
	Cache    string        // the loop's own build caches, writable, which nothing of the user's reads
	Scratch  string        // where each test's scratch directory goes
	Path     []string      // the test's PATH: its toolchains' directories first
	Timeout  time.Duration // how long a test may run
	// Gate runs before each test and waits until the machine has room for it (capacity), or returns why not.
	Gate func(ctx context.Context) error
}

// DefaultTestTimeout is how long one run of a check's test may take: a package's suite, not the world's.
const DefaultTestTimeout = 20 * time.Minute

// Run runs words in dir and returns how it went. The environment is only what the test needs: its home, its
// path, a scratch directory of its own as TMPDIR, the build caches' variables, and env's own entries (a
// toolchain's settings). Nothing of the user's session reaches it.
func (s Sandbox) Run(ctx context.Context, dir string, words []string, env []string) (Outcome, error) {
	if s.Gate != nil {
		if err := s.Gate(ctx); err != nil {
			return Outcome{}, err
		}
	}
	base := s.Scratch
	if base == "" {
		base = filepath.Dir(dir)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return Outcome{}, err
	}
	scratch, err := os.MkdirTemp(base, ".check-")
	if err != nil {
		return Outcome{}, err
	}
	defer os.RemoveAll(scratch)
	args := []string{"--unshare-all", "--die-with-parent", "--new-session", "--cap-drop", "ALL",
		"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp", "--tmpfs", "/run", "--tmpfs", "/var/tmp"}
	if s.Home != "" {
		args = append(args, "--tmpfs", s.Home)
		for _, d := range s.ReadOnly {
			if st, err := os.Stat(d); err == nil && st.IsDir() {
				args = append(args, "--ro-bind", d, d)
			}
		}
	}
	args = append(args, "--bind", dir, dir, "--bind", scratch, scratch)
	if st, err := os.Lstat(filepath.Join(dir, ".git")); err == nil && !st.IsDir() {
		args = append(args, "--ro-bind", filepath.Join(dir, ".git"), filepath.Join(dir, ".git")) // its link to the repository stays the loop's
	}
	cache := s.Cache
	if cache != "" {
		if err := os.MkdirAll(filepath.Join(cache, "go-build"), 0o700); err != nil {
			return Outcome{}, err
		}
		args = append(args, "--bind", cache, cache)
	}
	args = append(append(args, "--chdir", dir, "--"), words...)
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultTestTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, s.Bwrap, args...)
	cmd.Env = s.env(scratch, cache, env)
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

// env is a test's whole environment.
func (s Sandbox) env(scratch, cache string, extra []string) []string {
	path := append(append([]string{}, s.Path...), "/usr/local/bin", "/usr/bin", "/bin")
	env := []string{"HOME=" + firstOf(s.Home, scratch), "PATH=" + strings.Join(path, ":"), "TMPDIR=" + scratch, "LANG=C.UTF-8",
		"GIT_TERMINAL_PROMPT=0", "GOTOOLCHAIN=local", "GOTELEMETRY=off", "GOPROXY=off", "CARGO_NET_OFFLINE=true"}
	if cache != "" {
		env = append(env, "GOCACHE="+filepath.Join(cache, "go-build"), "UV_CACHE_DIR="+filepath.Join(cache, "uv"),
			"npm_config_cache="+filepath.Join(cache, "npm"))
	}
	return append(env, extra...)
}

func tailOf(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return strings.ToValidUTF8(string(b), "?")
}
