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
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	// Bwrap, Self and Home make the sandbox: bubblewrap, momentum-door (the sandbox's init and its momentum and
	// tacit command client) and the home's path, empty inside.
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
	Dir       string // the run's writable directory: temporary files, configuration and sessions
	Prompt    string
	System    string // appended to the harness's system prompt: the loop's procedure, the family's skill, the rules
	Schema    string // the JSON schema of the run's final report
	MaxTurns  int
	BudgetUSD float64
	Timeout   time.Duration
	Persist   bool         // keep the session's transcript (live runs), in the run's own directory
	Mounts    []jail.Mount // what the run reaches besides its own directory: a build's work tree, a review's
	Env       map[string]string
	Tools     string            // the built-in tools it has: DefaultTools when empty, "none" none (a judge), or a list
	Files     map[string][]byte // host-owned review inputs, mounted read-only beside the system and schema
	Private   []string          // host clone and package directories, absent from every mount
	fallback  bool
	// Door runs the momentum and tacit commands the run types, outside its sandbox: what its step allows, as the
	// loop runs them. Nil: the run has none.
	Door func(context.Context, jail.Request) jail.Reply
}

// Result is what a run came to.
type Result struct {
	Started    bool   // bubblewrap reported that its sandboxed program started
	Status     string // store.RunOK, RunFailed, RunBudget, RunTurns, RunTimeout
	Failure    string // how it failed, when it did: a short key the stop rule compares
	CostKnown  bool
	TurnsKnown bool
	USD        float64 // at Bedrock's rates for the models with one (bedrockRates), at the harness's own for the rest
	Tokens     int64
	Usage      Usage // the tokens, by kind
	Turns      int
	Duration   time.Duration
	Report     json.RawMessage // the final report, by the schema
	Text       string          // the final text
	Denials    int             // calls the harness refused
	Session    string
	Transcript string // the run's host-only stream archive, outside every run mount
	Model      string // the model that ran it
	// Refused is what the run's relay refused to forward, one entry for each kind of request: shown with the run, never
	// part of Failure, which the fallback and the stop rule compare.
	Refused                                   []string
	RelayUSD                                  float64
	RelayAdmitted, RelayRefused, RelayCharged int
}

// stopGrace is how long a run that ran out of its time has to stop after SIGTERM before it is killed.
var stopGrace = 10 * time.Second

// Run runs the agent once in its sandbox and returns what came of it. It writes trusted files into HostDir(inv.Dir)
// and gives the harness only its read-only inputs. It waits at most inv.Timeout; then it stops the
// sandbox, and every process the run started goes with it.
//
// Inside, the harness has its own directory and inv.Mounts, an empty home, and no network but its relay; it runs
// with no permission prompt, since what it must not reach is not there (--dangerously-skip-permissions, which
// Claude Code recommends only for a sandbox with no internet access), bare (no hooks, plugins, memory or
// CLAUDE.md), and with only the tools named.
func (a Agent) Run(ctx context.Context, inv Invocation) (Result, error) {
	if err := CheckDoorClient(a.Self); err != nil {
		return Result{}, err
	}
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
	for _, d := range []string{inv.Dir, HostDir(inv.Dir)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return Result{}, err
		}
	}
	if err := runDirectories(inv.Dir); err != nil {
		return Result{}, err
	}
	if err := hostDir(HostDir(inv.Dir)); err != nil {
		return Result{}, err
	}
	mounts := append(append([]jail.Mount{{Path: inv.Dir, Write: true}, {Path: harness}}, a.Mounts...), inv.Mounts...)
	for _, m := range mounts {
		for _, dir := range inv.Private {
			if within(dir, m.Path) || within(m.Path, dir) {
				return Result{}, fmt.Errorf("a run mount reaches a host-only repository or package directory")
			}
		}
		if within(HostDir(inv.Dir), m.Path) || within(m.Path, HostDir(inv.Dir)) {
			return Result{}, fmt.Errorf("a run mount reaches its host-only directory")
		}
		if _, err := os.Stat(m.Path); errors.Is(err, os.ErrNotExist) {
			return Result{}, fmt.Errorf("the sandbox cannot mount %s: it does not exist", m.Path)
		} else if err != nil {
			return Result{}, fmt.Errorf("the sandbox cannot mount %s: %w", m.Path, err)
		}
	}
	attempt := "primary"
	streamName, errName := "transcript.jsonl", "stderr.txt"
	if inv.fallback {
		attempt, streamName, errName = "fallback", "transcript.fallback.jsonl", "stderr.fallback.txt"
	}
	inputDir := filepath.Join(HostDir(inv.Dir), attempt)
	if err := hostDir(inputDir); err != nil {
		return Result{}, err
	}
	for name, data := range inv.Files {
		if filepath.Base(name) != name || name == "system.md" || name == "schema.json" {
			return Result{}, fmt.Errorf("invalid run input name")
		}
		if err := writeExclusive(inputDir, name, data); err != nil {
			return Result{}, err
		}
	}
	if err := writeExclusive(inputDir, "system.md", []byte(inv.System)); err != nil {
		return Result{}, err
	}
	if err := writeExclusive(inputDir, "schema.json", []byte(inv.Schema)); err != nil {
		return Result{}, err
	}
	inputs, err := os.Open(inputDir)
	if err != nil {
		return Result{}, err
	}
	defer inputs.Close()
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
		"--append-system-prompt-file", jail.InputDir+"/system.md",
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
	route, routeEnv, err := a.Bedrock.open(filepath.Join(sockets, "route.sock"), inv.BudgetUSD)
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
	var status *os.File
	spec := jail.Spec{Bwrap: a.Bwrap, Self: a.Self, Home: a.Home, Mounts: jail.Tidy(mounts), Dir: inv.Dir,
		Status: func(r *os.File) { status = r },
		Env:    sandboxEnv(a.Home, inv.Dir, routeEnv, a.Env, inv.Env), Route: filepath.Join(sockets, "route.sock"), RoutePort: RoutePort, Door: door, Inputs: inputs}
	cmd, err := spec.Command(context.Background(), append([]string{harness}, args...)...)
	if err != nil {
		return Result{}, err
	}
	defer jail.Close(cmd)
	defer status.Close()
	transcript := filepath.Join(HostDir(inv.Dir), streamName)
	out, err := exclusiveOutput(HostDir(inv.Dir), streamName)
	if err != nil {
		return Result{}, err
	}
	defer out.Close()
	errLog, err := exclusiveOutput(HostDir(inv.Dir), errName)
	if err != nil {
		return Result{}, err
	}
	defer errLog.Close()
	stream, diag := &boundedFile{w: out, left: 64 << 20}, &boundedFile{w: errLog, left: jail.MaxStream}
	cmd.Stdout, cmd.Stderr = stream, diag
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	runCtx, cancel := context.WithTimeout(ctx, inv.Timeout)
	defer cancel()
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{Status: store.RunFailed, Failure: "sandbox", CostKnown: true, Text: fmt.Sprintf("start the sandbox: %v", err)}, nil
	}
	jail.Close(cmd)
	started := make(chan bool, 1)
	go func() { started <- sandboxStarted(status) }()
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
	select {
	case res.Started = <-started:
	case <-runCtx.Done():
		status.Close() // a launcher holding the status pipe cannot outlive the invocation's deadline
		res.Started = <-started
	}
	if !res.Started {
		return Result{Status: store.RunFailed, Failure: "sandbox", Transcript: transcript, Model: a.Model, CostKnown: true,
			Text: lastLine(filepath.Join(HostDir(inv.Dir), errName))}, nil
	}
	route.close()
	res.Refused = route.refusals()
	res.RelayUSD, res.RelayAdmitted, res.RelayRefused, res.RelayCharged = route.accounting()
	reports, refusal := route.reportsObserved()
	hadReport := len(res.Report) > 0
	authenticateReport(&res, reports, refusal)
	res.Turns, res.TurnsKnown = res.RelayAdmitted, true
	// Timeout, cancellation, archive limits and process exit are host observations.
	// No stream record can overrule them, even an authenticated report.
	switch {
	case timedOut:
		res.Status, res.Failure = store.RunTimeout, "timeout"
	case ctx.Err() != nil:
		res.Status, res.Failure = store.RunFailed, "stopped"
	case stream.over || diag.over:
		res.Status, res.Failure = store.RunFailed, "stream limit"
	case waitErr != nil && !refusal:
		res.Status = store.RunOK // reconcileExit installs the host's exit key
		reconcileExit(&res, waitErr)
	case perr != nil && res.Status == store.RunOK:
		res.Status, res.Failure = store.RunFailed, "stream"
	case !hadReport && res.Failure == "report" && inv.MaxTurns > 0 && res.RelayAdmitted >= inv.MaxTurns:
		res.Status, res.Failure = store.RunTurns, "turns"
	}
	// Stale recovery must use the host's authenticated result, never the harness's stdout as proof of a report.
	resultName := "result.json"
	if inv.fallback {
		resultName = "result.fallback.json"
	}
	if err := keepHostResult(ctx, inv.Dir, resultName, &res); err != nil {
		return res, err
	}
	return res, nil
}

// keepHostResult keeps the result only after report authentication and the host's exit checks. The raw stream
// remains an archive; the result used by stale recovery has its model text cleaned before storage.
func keepHostResult(ctx context.Context, dir, name string, res *Result) error {
	if len(res.Report) > 0 {
		clean, err := cleanReport(ctx, res.Report)
		if err != nil {
			res.Report = nil
			res.Status, res.Failure, res.Text = store.RunFailed, "scrub", "(report withheld: the cleaner failed)"
			return err
		}
		res.Report = clean
	}
	// Recovery needs only the host's outcome and report, not the harness's session text or accounting.
	raw, err := json.Marshal(struct {
		Started         bool
		Status, Failure string
		Report          json.RawMessage
	}{res.Started, res.Status, res.Failure, res.Report})
	if err != nil {
		return err
	}
	return writeExclusive(HostDir(dir), name, raw)
}

// cleanReport decodes strings before cleaning: JSON escapes must not hide text from the cleaner.
// Numbers stay JSON numbers so cleaning cannot change a report's precision.
func cleanReport(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	var clean func(any) (any, error)
	clean = func(value any) (any, error) {
		switch value := value.(type) {
		case string:
			return Redact(ctx, value)
		case []any:
			for i, item := range value {
				next, err := clean(item)
				if err != nil {
					return nil, err
				}
				value[i] = next
			}
		case map[string]any:
			next := make(map[string]any, len(value))
			for key, item := range value {
				name, err := Redact(ctx, key)
				if err != nil {
					return nil, err
				}
				if _, exists := next[name]; exists {
					return nil, errors.New("cleaned report keys collide")
				}
				next[name], err = clean(item)
				if err != nil {
					return nil, err
				}
			}
			return next, nil
		}
		return value, nil
	}
	value, err := clean(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func readHostResult(dir string) (Result, error) {
	name := "result.json"
	if _, err := os.Lstat(filepath.Join(HostDir(dir), "transcript.fallback.jsonl")); !errors.Is(err, os.ErrNotExist) {
		name = "result.fallback.json"
	}
	f, err := OpenFile(HostDir(dir), name)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	raw, err := ReadFile(f, 64<<20)
	if err != nil {
		return Result{}, err
	}
	var res Result
	if err := json.Unmarshal(raw, &res); err != nil {
		return Result{}, err
	}
	return res, nil
}

func authenticateReport(res *Result, reports []json.RawMessage, refusal bool) {
	res.Text = "" // stdout text has no authenticated producer
	if refusal {
		res.Report = nil
		res.Status, res.Failure = store.RunFailed, "the model's safeguards"
		return
	}
	candidate, err := canonicalReport(res.Report)
	if err == nil {
		// Prefer the last matching model input; any input it emitted is admissible.
		for i := len(reports) - 1; i >= 0; i-- {
			if bytes.Equal(candidate, reports[i]) {
				res.Report = append(json.RawMessage(nil), reports[i]...)
				res.Status, res.Failure = store.RunOK, ""
				return
			}
		}
	}
	res.Report = nil
	res.Status, res.Failure = store.RunFailed, "report"
}

func reconcileExit(res *Result, waitErr error) {
	if waitErr == nil || res.Status != store.RunOK {
		return
	}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		res.Failure = fmt.Sprintf("exit %d", ee.ExitCode())
	} else {
		res.Failure = "exit failure"
	}
	res.Status = store.RunFailed
}

// readRunFile reads a file a run's agent wrote in its directory, for the loop to use: a regular file of at most
// 1 MiB, opened without following a link or waiting on a writer. The agent writes there as it likes inside its
// sandbox, a link to a path of the machine's outside it included, which this process, outside, must not follow.
func readRunFile(dir, name string) ([]byte, error) {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	raw, err := io.ReadAll(io.LimitReader(f, runFileMax+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > runFileMax {
		return nil, fmt.Errorf("%s is over %d bytes", name, runFileMax)
	}
	return raw, nil
}

// runFileMax bounds a file of a run's the loop reads.
const runFileMax = 1 << 20

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
		if res.CostKnown {
			res.USD = max(res.USD, res.RelayUSD)
		}
		return res, err
	}
	if !res.CostKnown || !res.TurnsKnown {
		res.USD = max(inv.BudgetUSD, res.RelayUSD)
		return res, nil
	}
	first := res
	firstCharge := max(first.USD, first.RelayUSD)
	inv.BudgetUSD -= firstCharge
	inv.MaxTurns -= first.Turns
	inv.Timeout -= time.Since(started)
	if inv.BudgetUSD <= 0 || inv.MaxTurns <= 0 || inv.Timeout <= 0 || deadline.Err() != nil {
		first.USD = firstCharge
		return first, nil
	}
	b := a
	b.Model, b.Fallback = a.Fallback, ""
	inv.fallback = true
	res, err = b.Run(deadline, inv)
	if err != nil && res.Transcript == "" {
		// A fallback that never started adds no spend to the completed first attempt.
		first.USD = firstCharge
		return first, err
	}
	return finishFallback(first, res, inv.BudgetUSD), err
}

// The charge compares the totals of both attempts, rather than summing two maxima.
// An unknown fallback still holds all of its remaining reservation.
func finishFallback(first, res Result, remaining float64) Result {
	if !res.CostKnown && res.Started {
		res.USD = max(first.USD, first.RelayUSD) + remaining
	} else {
		res.USD += first.USD
	}
	res.Started = res.Started || first.Started
	res.CostKnown = true
	res.Tokens += first.Tokens
	res.Usage = res.Usage.Add(first.Usage)
	res.Turns += first.Turns
	res.Duration += first.Duration
	res.RelayUSD += first.RelayUSD
	res.RelayAdmitted += first.RelayAdmitted
	res.RelayRefused += first.RelayRefused
	res.RelayCharged += first.RelayCharged
	res.USD = max(res.USD, res.RelayUSD)
	res.Refused = slices.Concat(first.Refused, slices.DeleteFunc(res.Refused, func(r string) bool { return slices.Contains(first.Refused, r) }))
	return res
}

// readStream reads the run's stream of events (Claude Code's stream-json): its last result says how the run
// ended, what it cost and what it reported; a call the permissions refused is counted.
func readStream(path string) (Result, error) {
	f, err := OpenFile(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	if _, err := regularOwned(f); err != nil {
		return Result{}, err
	}
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
				Input         int64 `json:"input_tokens"`
				Output        int64 `json:"output_tokens"`
				CacheRead     int64 `json:"cache_read_input_tokens"`
				CacheCreate   int64 `json:"cache_creation_input_tokens"`
				CacheCreation struct {
					Hour int64 `json:"ephemeral_1h_input_tokens"`
				} `json:"cache_creation"`
			} `json:"usage"`
			// each model's tokens and the harness's price for them, subagents' and a fallback's models among them
			ModelUsage map[string]struct {
				Input       int64    `json:"inputTokens"`
				Output      int64    `json:"outputTokens"`
				CacheRead   int64    `json:"cacheReadInputTokens"`
				CacheCreate int64    `json:"cacheCreationInputTokens"`
				Hour        int64    `json:"cacheCreationEphemeral1hInputTokens"`
				CostUSD     *float64 `json:"costUSD"`
			} `json:"modelUsage"`
		}
		if json.Unmarshal(line, &r) != nil || r.Type != "result" {
			continue
		}
		found = true
		res = Result{Text: r.Result, Report: r.StructuredOutput, Session: r.SessionID, Denials: len(r.PermissionDenials),
			Usage: Usage{Input: r.Usage.Input, Output: r.Usage.Output, CacheRead: r.Usage.CacheRead, CacheWrite: r.Usage.CacheCreate,
				CacheWrite1h: r.Usage.CacheCreation.Hour}}
		res.Started = true // a stored final result proves startup for stale recovery; Run uses bubblewrap's status
		if r.TotalCostUSD != nil && known(*r.TotalCostUSD) {
			res.USD, res.CostKnown = *r.TotalCostUSD, true
		}
		// with each model's tokens, the run is priced at Bedrock's rates, model by model. The harness's own prices
		// check them: a model whose tokens price outside what Bedrock's rates can be against its list price (half of
		// it to all of it) keeps the harness's price, and a run whose models' prices do not add up to its own, or
		// with a model the harness gave no price, keeps the harness's price whole
		if len(r.ModelUsage) > 0 {
			var u Usage
			usd, list, priced := 0.0, 0.0, true
			for model, m := range r.ModelUsage {
				mu := Usage{Input: m.Input, Output: m.Output, CacheRead: m.CacheRead, CacheWrite: m.CacheCreate, CacheWrite1h: m.Hour}
				u = u.Add(mu)
				if m.CostUSD == nil || !known(*m.CostUSD) {
					priced = false
					continue
				}
				list += *m.CostUSD
				if p, ok := bedrockPrice(model, mu); ok && p >= *m.CostUSD/2-priceSlack && p <= *m.CostUSD+priceSlack {
					usd += p
				} else {
					usd += *m.CostUSD
				}
			}
			res.Usage = u
			if priced && res.CostKnown && math.Abs(list-res.USD) <= priceSlack {
				res.USD = usd
			}
		}
		res.Tokens = res.Usage.Total()
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

// priceSlack is how far apart two prices of one run may be and still agree: half a cent, the harness's rounding.
const priceSlack = 0.005

// known is a price that is a number and not below zero.
func known(usd float64) bool { return usd >= 0 && !math.IsInf(usd, 0) && !math.IsNaN(usd) }

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
	f, err := OpenFile(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return ""
	}
	defer f.Close()
	// Diagnostics have the door's same whole-stream limit before selecting the last line.
	raw, err := ReadFile(f, jail.MaxStream)
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
// tools, the locale, the temporary directory the caller chose, the session bus (evidence unit reads a service's state) and what the loop sets. Nothing else
// of the caller's environment reaches them: a harness's plugin list or session id would change what runs. A check's
// test gets less (Sandbox).
func CleanEnv(extra map[string]string, bins ...string) []string {
	keep := []string{"TMPDIR", "HOME", "LANG", "LC_ALL", "TZ", "USER", "LOGNAME", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR",
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
