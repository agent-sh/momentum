package loop

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

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
		{"momentum", "evidence", "git"}, {"momentum", "evidence", "unit"}, {"momentum", "supervise", "replay"},
		{"momentum", "report"}, {"momentum", "ship", "status"}, {"momentum", "case", "list"}, {"momentum", "case", "show"},
		{"tacit", "search"}, {"tacit", "fact"}, {"tacit", "tree"}}
	// doorCase are a step's changes to its own case.
	doorCase = [][]string{{"momentum", "case", "set"}, {"momentum", "case", "evidence"}, {"momentum", "case", "signature"},
		{"momentum", "case", "close"}, {"momentum", "case", "reopen"}}
	// doorSteps are what each step adds: a review reads only.
	doorSteps = map[string][][]string{
		StepInvestigate: doorCase,
		StepBuild:       append(append([][]string{}, doorCase...), []string{"momentum", "ship", "prepare"}, []string{"momentum", "ship", "check"}, []string{"momentum", "ship", "propose"}),
		StepPublish:     append(append([][]string{}, doorCase...), []string{"momentum", "loop", "lesson"}),
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

// door is a run's door: what its step allows, run as the loop runs it, in the run's directory or the directory of
// its own it was typed in (its work tree), with the loop's variables and none of the sandbox's. What tacit prints is
// scrubbed here; what momentum prints, by momentum.
func (r *Runner) door(step string, c store.Case, run store.LoopRun, roots []string) func(context.Context, jail.Request) jail.Reply {
	reads := append(append([]string{}, r.ReadDirs...), roots[1:]...)
	env := CleanEnv(map[string]string{"MOMENTUM_ACTOR": "loop:" + run.ID, "MOMENTUM_LOOP_RUN": run.ID, "MOMENTUM_LOOP_CASE": c.ID,
		"MOMENTUM_LOOP_DIR": run.Dir, "MOMENTUM_LOOP_READ": strings.Join(reads, string(filepath.ListSeparator))}, r.Bins...)
	return func(ctx context.Context, req jail.Request) jail.Reply {
		if !doorAllows(step, req.Argv) {
			return jail.Refuse("%s: not among the commands of a run's %s step", strings.Join(firstWords(req.Argv, 3), " "), step)
		}
		prog := r.Momentum
		if req.Argv[0] == "tacit" {
			prog = r.Tacit
		}
		if prog == "" {
			return jail.Refuse("%s: not on this machine", req.Argv[0])
		}
		cctx, cancel := context.WithTimeout(ctx, r.Budgets.RunTime)
		defer cancel()
		cmd := exec.CommandContext(cctx, prog, req.Argv[1:]...)
		cmd.Env, cmd.Dir = env, run.Dir
		for _, root := range roots {
			if within(req.Dir, root) {
				cmd.Dir = req.Dir
			}
		}
		var out, diag bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &diag
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		err := cmd.Run()
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
