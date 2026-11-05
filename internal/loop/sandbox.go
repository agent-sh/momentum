package loop

import (
	"context"
	"errors"
	"fmt"
	"github.com/agent-sh/momentum/internal/sandbox"
	"os"
	"path/filepath"
	"strings"
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
	return commandWords(cmd, true)
}

// Display words retain quoting and argument boundaries after cleaning. They
// are never executable input, so a cleaner's mask need not be a shell token.
func commandWords(cmd string, execute bool) ([]string, error) {
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
		case execute && strings.ContainsRune("|&;<>()$`\\*?[]{}~!#", r):
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
	if execute && !testPrograms[out[0]] {
		return nil, refused("%q is not a test runner a check may run (%s)", out[0], testProgramList)
	}
	return out, nil
}

// testProgramList names the test runners for a refusal's text.
const testProgramList = "go, cargo, npm, pnpm, bun, npx, node, python3, pytest, uv, make, bats, claude"

// Argv checks a test command given as its words, as a build report gives it: a program and its arguments, run as they
// are, without a shell. The program is one of the test runners by its bare name; no word is empty or holds a NUL.
func Argv(args []string) ([]string, error) {
	if len(args) == 0 {
		return nil, refused("an empty test command: give the program and its arguments, such as [\"go\", \"test\", \"./...\"]")
	}
	for _, a := range args {
		if a == "" || strings.ContainsRune(a, 0) {
			return nil, refused("the test command %s has an empty word or a NUL: give the program and its arguments", plainClip(fmt.Sprintf("%q", args), 200))
		}
	}
	if !testPrograms[args[0]] {
		return nil, refused("the test command starts with %q, which is no test runner a check may run (%s): its first word is the program, such as [\"go\", \"test\", \"./...\"], and the host runs it, never a description of the test", plainClip(args[0], 120), testProgramList)
	}
	return append([]string(nil), args...), nil
}

// systemPath are the directories of the sandbox's PATH after the toolchains' (Toolchains.inputs).
var systemPath = []string{"/usr/bin", "/bin"}

// resolve finds a check's program as the sandbox's PATH would: in a toolchain directory the sandbox mounts (under one
// of its read-only roots, or /usr), then in /usr/bin and /bin. A program on none of them would not start there, so the
// check refuses it before it runs.
func (s Sandbox) resolve(program string) error {
	roots, err := (Toolchains{ReadOnly: s.ReadOnly}).roots()
	if err != nil {
		return err
	}
	mounted := append([]string{"/usr", "/etc/alternatives"}, roots...) // both bound at their host paths
	for _, dir := range systemPath {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			mounted = append(mounted, real)
		}
	}
	inMount := func(p string) bool {
		for _, root := range mounted {
			if under(p, root) {
				return true
			}
		}
		return false
	}
	for _, dir := range append(append([]string(nil), s.Path...), systemPath...) {
		// the program as the sandbox would reach it: its directory and, through any link, the file itself in a mount; an
		// absolute link outside /usr names a host path, which the sandbox mounts elsewhere or not at all
		if !relativeLinks(filepath.Join(dir, program), inMount) {
			continue
		}
		real, err := filepath.EvalSymlinks(filepath.Join(dir, program))
		if err != nil || !inMount(real) {
			continue
		}
		if d, err := filepath.EvalSymlinks(dir); err != nil || !inMount(d) {
			continue
		}
		st, err := os.Stat(real)
		if err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return nil
		}
	}
	return refused("the test program %q is not on the sandbox's PATH (its toolchains, /usr/bin and /bin): use a test runner the sandbox has", program)
}

// relativeLinks follows a program's chain of links and says whether every hop stays in a mount the sandbox has and
// every absolute link names a path under /usr or /etc/alternatives: an absolute link elsewhere names a host path, which
// the sandbox mounts at another place or not at all.
func relativeLinks(path string, inMount func(string) bool) bool {
	for range 40 {
		if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err != nil || !inMount(dir) {
			return false
		}
		st, err := os.Lstat(path)
		if err != nil {
			return false
		}
		if st.Mode()&os.ModeSymlink == 0 {
			return true
		}
		target, err := os.Readlink(path)
		if err != nil {
			return false
		}
		if filepath.IsAbs(target) {
			if !under(target, "/usr") && !under(target, "/etc/alternatives") { // both bound at their host paths
				return false
			}
			path = target
			continue
		}
		path = filepath.Join(filepath.Dir(path), target)
	}
	return false
}

// Outcome is one run of a check's test.
type Outcome struct {
	Exit    int     `json:"exit"`
	Seconds float64 `json:"seconds"`
	Tail    string  `json:"tail"` // the end of what it printed
}

// Sandbox runs a host export with the fixed launcher and no model relay.
// Only /work, /out and tmpfs caches are writable.
type Sandbox struct {
	ReadOnly          []string
	Cache             string
	Scratch           string
	Path              []string
	GoRoot, GoModules string
	Timeout           time.Duration
	Gate              func(context.Context) error
}

const DefaultTestTimeout = 20 * time.Minute

func (s Sandbox) spec(dir, scratch string, words, extra []string) (sandbox.Spec, error) {
	t := Toolchains{ReadOnly: s.ReadOnly, Path: s.Path, GoRoot: s.GoRoot, GoModules: s.GoModules}
	binds, env, err := t.inputs()
	if err != nil {
		return sandbox.Spec{}, err
	}
	for _, kv := range extra {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return sandbox.Spec{}, errors.New("test environment entry has no name")
		}
		env[k] = t.rewrite(v)
	}
	binds = append(binds, sandbox.Bind{Src: dir, Dst: "/work", Write: true}, sandbox.Bind{Src: filepath.Join(scratch, "out"), Dst: "/out", Write: true})
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultTestTimeout
	}
	return sandbox.Spec{Unit: runUnit(filepath.Base(scratch), scratch, "check"), Host: scratch, Binds: binds, Dir: "/work", Env: env,
		Limits: sandbox.Limits{RuntimeSec: int(timeout.Seconds()) + 60}, Argv: words}, nil
}

func (s Sandbox) Run(ctx context.Context, dir string, words []string, extra []string) (outcome Outcome, runErr error) {
	if s.Gate != nil {
		if err := s.Gate(ctx); err != nil {
			return Outcome{}, err
		}
	}
	base := firstOf(s.Scratch, filepath.Dir(dir))
	if err := os.MkdirAll(base, 0700); err != nil {
		return Outcome{}, err
	}
	scratch, err := os.MkdirTemp(base, ".check-")
	if err != nil {
		return Outcome{}, err
	}
	defer func() {
		if err := removeAll(scratch); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("test scratch tree cannot be removed: %w", err))
		}
	}()
	if err := os.Mkdir(filepath.Join(scratch, "out"), 0700); err != nil {
		return Outcome{}, err
	}
	spec, err := s.spec(dir, scratch, words, extra)
	if err != nil {
		return Outcome{}, err
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultTestTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := Capture{Limit: MaxStream}
	start := time.Now()
	result, err := spec.Run(cctx, &out, &out)
	if err != nil {
		return Outcome{}, err
	}
	if result.Failed {
		return Outcome{}, errors.New("test launcher failed")
	}
	if out.Over() {
		return Outcome{}, errors.New("test output exceeds the whole-stream limit")
	}
	clean, err := Redact(ctx, out.String())
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Exit: result.Code, Seconds: time.Since(start).Seconds(), Tail: tailOf([]byte(clean), 1500)}, nil
}

func tailOf(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return strings.ToValidUTF8(string(b), "?")
}
