package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/agent-sh/momentum/internal/jail"
	"github.com/agent-sh/momentum/internal/store"
)

// A run's door runs, outside its sandbox, the momentum and tacit commands the run types: those its step allows,
// each named by its first words. The commands' own checks hold beyond the door: a run changes only its own case,
// lands and rolls back nothing, reads replay only from its output, reads git only where the loop reads, and all
// they print is scrubbed whole (momentum's own, in a loop run) before it reaches the run.
var (
	// doorReads are what every step reads the machine with.
	doorReads = [][]string{{"momentum", "evidence", "events"}, {"momentum", "evidence", "status"}, {"momentum", "evidence", "show"},
		{"momentum", "evidence", "search"}, {"momentum", "evidence", "git"}, {"momentum", "evidence", "unit"}, {"momentum", "supervise", "replay"},
		{"momentum", "report"}, {"momentum", "ship", "status"}, {"tacit", "search"}, {"tacit", "fact"}, {"tacit", "tree"}}
	// doorCaseReads are the case commands that read.
	doorCaseReads = [][]string{{"momentum", "case", "list"}, {"momentum", "case", "show"}}
	// doorCase are a step's changes to its own case.
	doorCase = [][]string{{"momentum", "case", "set"}, {"momentum", "case", "evidence"}, {"momentum", "case", "signature"},
		{"momentum", "case", "close"}, {"momentum", "case", "reopen"}}
	// doorSteps are what each step adds: an investigation runs no case command (the runner writes its report into
	// its case), a review reads cases only.
	doorSteps = map[string][][]string{
		StepInvestigate: nil,
		StepBuild: append(append(append([][]string{}, doorCaseReads...), doorCase...),
			[]string{"momentum", "ship", "prepare"}, []string{"momentum", "ship", "check"}, []string{"momentum", "ship", "propose"}),
		StepReview:  doorCaseReads,
		StepPublish: append(append(append([][]string{}, doorCaseReads...), doorCase...), []string{"momentum", "loop", "lesson"}),
	}
)

// doorAllows says whether a step's door runs a command: its first words are one of the step's commands.
func doorAllows(step string, argv []string) bool {
	for _, cmd := range append(append([][]string{}, doorReads...), doorSteps[step]...) {
		if len(argv) >= len(cmd) && slices.Equal(argv[:len(cmd)], cmd) {
			return true
		}
	}
	return false
}

// door runs only a step's commands, in a host-only directory, with the loop's variables and none of the
// sandbox's. Requested directories are data and never become a host process's working directory.
func (r *Runner) door(step string, c store.Case, run store.LoopRun, _ []string) func(context.Context, jail.Request) jail.Reply {
	reads := append([]string{}, r.ReadDirs...)
	env := CleanEnv(map[string]string{"MOMENTUM_ACTOR": "loop:" + run.ID, "MOMENTUM_LOOP_RUN": run.ID, "MOMENTUM_LOOP_CASE": c.ID,
		"MOMENTUM_LOOP_DIR": run.Dir, "MOMENTUM_LOOP_READ": strings.Join(reads, string(filepath.ListSeparator))}, r.Bins...)
	d := doorServer{momentum: r.Momentum, tacit: r.Tacit, env: env, dir: run.Dir, timeout: r.Budgets.RunTime,
		allow: func(argv []string) string {
			if doorAllows(step, argv) {
				return ""
			}
			return fmt.Sprintf("%s: not among the commands of a run's %s step", strings.Join(firstWords(argv, 3), " "), step)
		}}
	return func(ctx context.Context, req jail.Request) jail.Reply {
		return d.serve(ctx, req)
	}
}

// A doorServer runs, outside a sandbox, the momentum and tacit commands its program types: those allow lets
// through, with the programs and environment its caller gives, in a host-only sibling of dir.
// A request's directory never selects a host command directory. Whatever allow does not name runs nothing.
type doorServer struct {
	// allow says why a command may not run, or "" when it may.
	allow           func(argv []string) string
	momentum, tacit string // the programs
	env             []string
	tacitEnv        []string // tacit's environment; env when nil
	dir             string
	timeout         time.Duration
	// ran is told of each command that is about to run.
	ran func(argv []string)
}

func (d doorServer) serve(ctx context.Context, req jail.Request) jail.Reply {
	if len(req.Argv) == 0 {
		return jail.Refuse("empty command")
	}
	if why := d.allow(req.Argv); why != "" {
		return jail.Refuse("%s", why)
	}
	prog, env := d.momentum, d.env
	if req.Argv[0] == "tacit" {
		prog = d.tacit
		if d.tacitEnv != nil {
			env = d.tacitEnv
		}
	}
	if prog == "" {
		return jail.Refuse("%s: not on this machine", req.Argv[0])
	}
	cctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, prog, req.Argv[1:]...)
	cmd.Env = env
	if err := hostDir(HostDir(d.dir)); err != nil {
		return jail.Refuse("host command directory: %v", err)
	}
	cmd.Dir = HostDir(d.dir)
	out, diag := Capture{Limit: jail.MaxStream}, Capture{Limit: jail.MaxStream}
	cmd.Stdout, cmd.Stderr = &out, &diag
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	if d.ran != nil {
		d.ran(req.Argv)
	}
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if out.Over() || diag.Over() {
		return jail.Reply{Stderr: []byte("(output withheld: over the door's limit)\n"), Code: 126}
	}
	code := 0
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		code = ee.ExitCode()
	case err != nil:
		return jail.Refuse("%s: %v", req.Argv[0], err)
	}
	reply := jail.Reply{Stdout: out.Bytes(), Stderr: diag.Bytes(), Code: code}
	if req.Argv[0] == "tacit" {
		clean, e1 := Redact(ctx, out.String())
		cleanDiag, e2 := Redact(ctx, diag.String())
		if e1 != nil || e2 != nil {
			return jail.Refuse("tacit: the secret scrubber did not run, so nothing of this command is shown")
		}
		reply.Stdout, reply.Stderr = []byte(clean), []byte(cleanDiag)
	}
	return reply
}

func firstWords(argv []string, n int) []string {
	if len(argv) > n {
		return argv[:n]
	}
	return argv
}

// within says whether p is dir or lies under it.
func within(p, dir string) bool {
	if p == "" || dir == "" {
		return false
	}
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// Toolchains are what a build's tests need of the machine inside its sandbox: the toolchains and their caches,
// read-only; the loop's own build caches, writable, which nothing of the user's reads; the toolchains' programs,
// first on its path; and their variables, offline (the sandbox has no network).
type Toolchains struct {
	ReadOnly []string
	Cache    string
	Path     []string
}

// makeCaches makes the loop's build caches before a sandbox mounts them: the directory a build mounts writable, and
// Go's build cache in it. A machine's first build finds neither, and a mount whose source is missing does not start.
func (t Toolchains) makeCaches() error {
	if t.Cache == "" {
		return nil
	}
	if err := os.MkdirAll(t.Cache, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(t.Cache)
	if err != nil {
		return err
	}
	defer root.Close()
	st, err := root.Lstat("go-build")
	if errors.Is(err, os.ErrNotExist) {
		return root.Mkdir("go-build", 0o700)
	}
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("build cache is not a directory")
	}
	return nil
}

func (t Toolchains) mounts() []jail.Mount {
	var ms []jail.Mount
	for _, d := range t.ReadOnly {
		ms = append(ms, jail.Mount{Path: d})
	}
	if t.Cache != "" {
		ms = append(ms, jail.Mount{Path: t.Cache, Write: true})
	}
	return ms
}

func (t Toolchains) env() map[string]string {
	path := append(append([]string{jail.BinDir}, t.Path...), "/usr/local/bin", "/usr/bin", "/bin")
	env := map[string]string{"PATH": strings.Join(path, ":"), "GOTOOLCHAIN": "local", "GOTELEMETRY": "off", "GOPROXY": "off",
		"GOFLAGS": "-buildvcs=false", "CARGO_NET_OFFLINE": "true", "GIT_TERMINAL_PROMPT": "0"}
	if t.Cache != "" {
		env["GOCACHE"] = filepath.Join(t.Cache, "go-build")
		env["UV_CACHE_DIR"] = filepath.Join(t.Cache, "uv")
		env["npm_config_cache"] = filepath.Join(t.Cache, "npm")
	}
	return env
}
