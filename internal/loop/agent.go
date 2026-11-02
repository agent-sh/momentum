// Package loop is momentum's improvement loop: one agent loop with skills and tools. Each run takes one case of
// one family through its next step (investigate, build, review, publish) with the harness's own main agent run
// headless in a sandbox (internal/jail), the family's skill appended to its system prompt, and tools whose hard
// limits the sandbox and the ship tool enforce: nothing of the machine's in the sandbox but its own directory and
// what its step needs, momentum's commands outside it through its door, a budget and a time for the run, and gates
// on every write (ship.go, gate.go). The runner (runner.go) picks the cases, keeps the budgets, and stops a case
// that fails the same way twice.
package loop

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agent-sh/momentum/internal/jail"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

// Agent starts the harness's main agent headless: Claude Code's print mode, by the runner command the machine
// configures (its first word the harness's binary, the rest its first arguments), in a sandbox of its own
// (internal/jail) whose one route out is its Bedrock relay.
type Agent struct {
	Runner   []string
	Model    string
	Fallback string
	// Bwrap, Self and Home make the sandbox: bubblewrap, momentum's binary (the sandbox's init and its momentum and
	// tacit commands, the door's client) and the home's path, empty inside.
	Bwrap, Self, Home string
	// Mounts are paths every run reads besides its own (a test's fake harness keeps its records in one).
	Mounts []jail.Mount
	// Env is what a run's environment adds to the sandbox's own: the loop's variables.
	Env map[string]string
	// Bedrock is the run's route: every run is on Bedrock, through the machine's broker, and one with no route does
	// not start.
	Bedrock *Bedrock
}

// DefaultTools are the harness's built-in tools a loop run has: the file tools, the shell and subagents, all inside
// its sandbox.
const DefaultTools = "Read,Grep,Glob,Write,Edit,Bash,Agent"

// Invocation is one run of the agent.
type Invocation struct {
	Dir       string // the run's own directory: its working directory, where its files and its transcript go
	Prompt    string
	System    string // appended to the harness's system prompt: the loop's procedure, the family's skill, the rules
	Schema    string // the JSON schema of the run's final report
	MaxTurns  int
	BudgetUSD float64
	Timeout   time.Duration
	Persist   bool         // keep the session's transcript (live runs), in the run's own directory
	Mounts    []jail.Mount // what the run reaches besides its own directory: a build's work tree, a review's
	Env       map[string]string
	Tools     string // the built-in tools it has: DefaultTools when empty, "none" none (a judge), or a list
	// Door runs the momentum and tacit commands the run types, outside its sandbox: what its step allows, as the
	// loop runs them. Nil: the run has none.
	Door func(context.Context, jail.Request) jail.Reply
}

// Result is what a run came to.
type Result struct {
	Status     string // store.RunOK, RunFailed, RunBudget, RunTurns, RunTimeout
	Failure    string // how it failed, when it did: a short key the stop rule compares
	CostKnown  bool
	TurnsKnown bool
	USD        float64
	Tokens     int64
	Turns      int
	Duration   time.Duration
	Report     json.RawMessage // the final report, by the schema
	Text       string          // the final text
	Denials    int             // calls the harness refused
	Session    string
	Transcript string // the run's stream of events, in Dir
	Model      string // the model that ran it
}

// stopGrace is how long a run that ran out of its time has to stop after SIGTERM before it is killed.
var stopGrace = 10 * time.Second

// Run runs the agent once in its sandbox and returns what came of it. It writes the run's files into inv.Dir (its
// system prompt, schema and the stream of its events) and waits for it, at most inv.Timeout; then it stops the
// sandbox, and every process the run started goes with it.
//
// Inside, the harness has its own directory and inv.Mounts, an empty home, and no network but its relay; it runs
// with no permission prompt, since what it must not reach is not there (--dangerously-skip-permissions, which
// Claude Code recommends only for a sandbox with no internet access), bare (no hooks, plugins, memory or
// CLAUDE.md), and with only the tools named.
func (a Agent) Run(ctx context.Context, inv Invocation) (Result, error) {
	if len(a.Runner) == 0 {
		return Result{}, errors.New("no runner: set loop.runner in sources.json")
	}
	if a.Bedrock == nil {
		return Result{}, fmt.Errorf("%w: every headless run is on Bedrock, and none falls back to the direct API", ErrNoBedrock)
	}
	harness, err := harnessBinary(a.Runner[0])
	if err != nil {
		return Result{}, err
	}
	for _, d := range []string{inv.Dir, filepath.Join(inv.Dir, "tmp"), filepath.Join(inv.Dir, "claude")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return Result{}, err
		}
	}
	write := func(name string, data []byte) (string, error) {
		p := filepath.Join(inv.Dir, name)
		return p, os.WriteFile(p, data, 0o600)
	}
	system, err := write("system.md", []byte(inv.System))
	if err != nil {
		return Result{}, err
	}
	if _, err := write("schema.json", []byte(inv.Schema)); err != nil {
		return Result{}, err
	}
	tools := inv.Tools
	switch tools {
	case "":
		tools = DefaultTools
	case "none":
		tools = ""
	}
	args := append(append([]string{}, a.Runner[1:]...), "-p", inv.Prompt,
		"--bare", "--dangerously-skip-permissions", "--tools", tools,
		"--output-format", "stream-json", "--verbose",
		"--append-system-prompt-file", system,
		"--model", a.Model,
		"--max-turns", fmt.Sprint(inv.MaxTurns),
		"--max-budget-usd", strconv.FormatFloat(inv.BudgetUSD, 'f', -1, 64),
		"--json-schema", inv.Schema, "--strict-mcp-config")
	if a.Fallback != "" {
		args = append(args, "--fallback-model", a.Fallback)
	}
	if !inv.Persist {
		args = append(args, "--no-session-persistence")
	}
	for _, m := range inv.Mounts {
		args = append(args, "--add-dir", m.Path)
	}
	sockets, err := socketDir(inv.Dir)
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(sockets)
	// the route is read now, at this launch: the broker writes its file again when it restarts
	route, routeEnv, err := a.Bedrock.open(filepath.Join(sockets, "route.sock"))
	if err != nil {
		return Result{}, err
	}
	defer route.close()
	door := ""
	if inv.Door != nil {
		door = filepath.Join(sockets, "door.sock")
		stop, err := jail.ServeDoor(door, inv.Door)
		if err != nil {
			return Result{}, err
		}
		defer stop()
	}
	mounts := append([]jail.Mount{{Path: inv.Dir, Write: true}, {Path: harness}}, a.Mounts...)
	spec := jail.Spec{Bwrap: a.Bwrap, Self: a.Self, Home: a.Home, Mounts: append(mounts, inv.Mounts...), Dir: inv.Dir,
		Env: sandboxEnv(a.Home, inv.Dir, routeEnv, a.Env, inv.Env), Route: filepath.Join(sockets, "route.sock"), RoutePort: RoutePort, Door: door}
	cmd, err := spec.Command(context.Background(), append([]string{harness}, args...)...)
	if err != nil {
		return Result{}, err
	}
	defer jail.Close(cmd)
	transcript := filepath.Join(inv.Dir, "transcript.jsonl")
	out, err := os.OpenFile(transcript, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return Result{}, err
	}
	defer out.Close()
	errLog, err := os.OpenFile(filepath.Join(inv.Dir, "stderr.txt"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return Result{}, err
	}
	defer errLog.Close()
	cmd.Stdout, cmd.Stderr = out, errLog
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	runCtx, cancel := context.WithTimeout(ctx, inv.Timeout)
	defer cancel()
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start the sandbox: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	timedOut := false
	select {
	case waitErr = <-done:
	case <-runCtx.Done():
		timedOut = runCtx.Err() == context.DeadlineExceeded
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case waitErr = <-done:
		case <-time.After(stopGrace):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			waitErr = <-done
		}
	}
	res, perr := readStream(transcript)
	res.Duration, res.Transcript, res.Model = time.Since(start), transcript, firstOf(res.Model, a.Model)
	switch {
	case timedOut:
		res.Status, res.Failure = store.RunTimeout, "timeout"
	case ctx.Err() != nil:
		res.Status, res.Failure = store.RunFailed, "stopped"
	case perr != nil:
		res.Status, res.Failure = store.RunFailed, "no result: "+perr.Error()
		if waitErr != nil {
			res.Failure = fmt.Sprintf("exit %v: %s", waitErr, lastLine(filepath.Join(inv.Dir, "stderr.txt")))
		}
	}
	if refused := route.refusals(); len(refused) > 0 && res.Status != store.RunOK {
		res.Failure += "; the relay refused " + strings.Join(refused, "; ")
	}
	return res, nil
}

// harnessBinary is the harness's binary as it lies on disk, links followed, which the sandbox binds read-only.
func harnessBinary(name string) (string, error) {
	p, err := exec.LookPath(paths.Expand(name))
	if err != nil {
		return "", fmt.Errorf("the runner %q: %w", name, err)
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	return filepath.Abs(p)
}

// socketDir is a directory of the run's own for its relay's and its door's sockets: under the run time directory,
// since a socket's path must fit 108 bytes and the run's directory may be deep. It is named by the run's directory,
// so two runs never share one.
func socketDir(runDir string) (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = filepath.Join(paths.Data(), "loop", "sockets")
	}
	sum := sha256.Sum256([]byte(filepath.Clean(runDir)))
	d := filepath.Join(base, "momentum-"+hex.EncodeToString(sum[:6]))
	if len(d)+len("/route.sock") > 100 {
		return "", fmt.Errorf("the run's socket directory %s is too deep for a socket", d)
	}
	return d, os.MkdirAll(d, 0o700)
}

// sandboxEnv is a run's whole environment inside its sandbox: the home, a path that finds momentum and tacit (the
// door's client) first, the locale, the harness's directories in the run's own (its configuration and sessions,
// its temporary files), the route, and the loop's variables. Nothing of the caller's environment is in it.
func sandboxEnv(home, dir string, route []string, extra ...map[string]string) []string {
	env := []string{"HOME=" + home, "PATH=" + jail.BinDir + ":/usr/local/bin:/usr/bin:/bin", "SHELL=/bin/bash",
		"LANG=" + firstOf(os.Getenv("LANG"), "C.UTF-8"), "TMPDIR=" + filepath.Join(dir, "tmp"), "CLAUDE_CODE_TMPDIR=" + filepath.Join(dir, "tmp"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(dir, "claude"), "DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}
	for _, k := range []string{"LC_ALL", "TZ", "USER", "LOGNAME"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	env = append(env, route...)
	for _, m := range extra {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			env = append(env, k+"="+m[k])
		}
	}
	return env
}

// RunOrFallback runs the agent, and when the model's safeguards refuse the run, runs it once more on the
// fallback model: Opus first, Sonnet when Opus fails. The result says which model ran (Model) and counts both
// runs' cost.
func (a Agent) RunOrFallback(ctx context.Context, inv Invocation) (Result, error) {
	deadline, cancel := context.WithTimeout(ctx, inv.Timeout)
	defer cancel()
	started := time.Now()
	res, err := a.Run(deadline, inv)
	if err != nil || res.Failure != "the model's safeguards" || a.Fallback == "" || a.Fallback == a.Model {
		return res, err
	}
	if !res.CostKnown || !res.TurnsKnown {
		res.USD = inv.BudgetUSD
		return res, nil
	}
	first := res
	inv.BudgetUSD -= first.USD
	inv.MaxTurns -= first.Turns
	inv.Timeout -= time.Since(started)
	if inv.BudgetUSD <= 0 || inv.MaxTurns <= 0 || inv.Timeout <= 0 || deadline.Err() != nil {
		return first, nil
	}
	b := a
	b.Model, b.Fallback = a.Fallback, ""
	if err := os.Rename(filepath.Join(inv.Dir, "transcript.jsonl"), filepath.Join(inv.Dir, "transcript.refused.jsonl")); err != nil {
		return first, err
	}
	res, err = b.Run(deadline, inv)
	if !res.CostKnown {
		res.USD = inv.BudgetUSD
	}
	res.USD += first.USD
	res.CostKnown = true
	res.Tokens += first.Tokens
	res.Turns += first.Turns
	res.Duration += first.Duration
	return res, err
}

// readStream reads the run's stream of events (Claude Code's stream-json): its last result says how the run
// ended, what it cost and what it reported; a call the permissions refused is counted.
func readStream(path string) (Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	var res Result
	found := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"type":"result"`)) && !bytes.Contains(line, []byte(`"type": "result"`)) {
			continue
		}
		var r struct {
			Type              string          `json:"type"`
			Subtype           string          `json:"subtype"`
			IsError           bool            `json:"is_error"`
			NumTurns          *int            `json:"num_turns"`
			TotalCostUSD      *float64        `json:"total_cost_usd"`
			Result            string          `json:"result"`
			StructuredOutput  json.RawMessage `json:"structured_output"`
			SessionID         string          `json:"session_id"`
			PermissionDenials []any           `json:"permission_denials"`
			Usage             struct {
				Input       int64 `json:"input_tokens"`
				Output      int64 `json:"output_tokens"`
				CacheRead   int64 `json:"cache_read_input_tokens"`
				CacheCreate int64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(line, &r) != nil || r.Type != "result" {
			continue
		}
		found = true
		res = Result{Text: r.Result, Report: r.StructuredOutput, Session: r.SessionID,
			Denials: len(r.PermissionDenials), Tokens: r.Usage.Input + r.Usage.Output + r.Usage.CacheRead + r.Usage.CacheCreate}
		if r.TotalCostUSD != nil && *r.TotalCostUSD >= 0 && !math.IsInf(*r.TotalCostUSD, 0) && !math.IsNaN(*r.TotalCostUSD) {
			res.USD, res.CostKnown = *r.TotalCostUSD, true
		}
		if r.NumTurns != nil && *r.NumTurns >= 0 {
			res.Turns, res.TurnsKnown = *r.NumTurns, true
		}
		res.Status, res.Failure = resultStatus(r.Subtype, r.IsError, len(r.StructuredOutput) > 0)
		if res.Status == store.RunFailed && r.IsError && r.Subtype == "success" {
			res.Failure = errorKind(r.Result)
		}
	}
	if err := sc.Err(); err != nil {
		return res, err
	}
	if !found {
		return res, errors.New("the run wrote no result")
	}
	return res, nil
}

// resultStatus reads how a run ended from its result's subtype: a success with its report is ok; a limit is its
// limit; a success that is an error (the provider's, ending the turn) is an error; anything else failed, by its
// subtype.
func resultStatus(subtype string, isError, reported bool) (string, string) {
	switch {
	case subtype == "success" && !isError && reported:
		return store.RunOK, ""
	case subtype == "success" && !isError:
		return store.RunFailed, "no report"
	case strings.Contains(subtype, "budget"):
		return store.RunBudget, "budget"
	case strings.Contains(subtype, "turns"):
		return store.RunTurns, "turns"
	case subtype == "success" || subtype == "":
		return store.RunFailed, "error"
	}
	return store.RunFailed, subtype
}

// errorKind names the error a run's result text ends on, as the stop rule compares it: the model's safeguards, an
// API error, or an error.
func errorKind(text string) string {
	switch low := strings.ToLower(text); {
	case strings.Contains(low, "safeguards"):
		return "the model's safeguards"
	case strings.Contains(low, "api error"):
		return "api error"
	}
	return "error"
}

// lastLine is a file's last non-empty line, clipped: what a failed command said last.
func lastLine(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	l := strings.TrimSpace(lines[len(lines)-1])
	if len(l) > 200 {
		l = l[:200]
	}
	return l
}

// CleanEnv is the environment of what the loop runs outside a sandbox for a run: the commands its door runs
// (momentum's, which read the store and the records, and tacit's). It has the user's home, a path to momentum's
// tools, the locale, the session bus (evidence unit reads a service's state) and what the loop sets. Nothing else
// of the caller's environment reaches them: a harness's plugin list or session id would change what runs. A check's
// test gets less (Sandbox).
func CleanEnv(extra map[string]string, bins ...string) []string {
	keep := []string{"HOME", "LANG", "LC_ALL", "TZ", "USER", "LOGNAME", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR",
		"MOMENTUM_HOME", "MOMENTUM_CONFIG", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"}
	var env []string
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	path := append(append([]string{}, bins...), "/usr/local/bin", "/usr/bin", "/bin")
	env = append(env, "PATH="+strings.Join(path, ":"))
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}
