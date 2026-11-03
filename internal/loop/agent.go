// Package loop runs investigations with host-prepared inputs and relay-enforced accounting.
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
	"os"
	"os/exec"
	"path/filepath"
	"slices"

	"strconv"
	"strings"

	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/sandbox"
	"github.com/agent-sh/momentum/internal/store"
)

// Agent starts the harness in the fixed sandbox. Its relay is the only network route.
type Agent struct {
	Runner          []string
	Model, Fallback string
	Binds           []sandbox.Bind
	Env             map[string]string
	Bedrock         *Bedrock
}

// DefaultTools are the harness's built-in tools a loop run has: the file tools, the shell and subagents, all inside
// its sandbox.
const DefaultTools = "Read,Grep,Glob,Write,Edit,Bash,Agent"

// Invocation is one run of the agent.
type Invocation struct {
	ID, Dir                string
	Prompt, System, Schema string
	MaxTurns               int
	BudgetUSD              float64
	Timeout                time.Duration
	Binds                  []sandbox.Bind
	Env                    map[string]string
	Tools                  string
	fallback               bool
	requestLimit           int
}

// Result is what a run came to.
type Result struct {
	Started        bool
	InputSummary   string
	Status         string // store.RunOK, RunFailed, RunBudget, RunTurns, RunTimeout
	Failure        string // how it failed, when it did: a short key the stop rule compares
	CostKnown      bool
	TurnsKnown     bool
	USD            float64
	ReservationUSD float64 // host reservation for an attempt with no observed route request
	Tokens         int64
	Turns          int
	Duration       time.Duration
	Report         json.RawMessage // the final report, by the schema
	Text           string          // the final text
	Transcript     string          // the run's host-only stream archive, outside every run mount
	Model          string          // the model that ran it
	Usage          Usage
	Harness        HarnessDiagnostics // untrusted stdout claims, shown only as diagnostics
	PriorHarness   []HarnessDiagnostics
	// Refused is what the run's relay refused to forward, one entry for each kind of request: shown with the run, never
	// part of Failure, which the fallback and the stop rule compare.
	Refused                                   []string
	RelayUSD                                  float64
	RelayAdmitted, RelayRefused, RelayCharged int
}

// HarnessDiagnostics never determines a charge, limit, fallback or recorded outcome.
type HarnessDiagnostics struct {
	USD                    *float64
	Turns                  *int
	Usage                  Usage
	Session, Subtype, Text string
	Denials                int
	IsError                bool
}

func diagnosticNumber[T int | float64](p *T) any {
	if p == nil {
		return "absent"
	}
	return *p
}

// --max-turns bounds the main thread only. Give its subagents two further
// main-thread-sized allowances, for three times MaxTurns across the whole route.
func requestAllowance(inv Invocation) int {
	if inv.requestLimit > 0 {
		return inv.requestLimit
	}
	return 3 * inv.MaxTurns
}

const MaxStream = 8 << 20
const runFileMax = 1 << 20

// Run archives the untrusted stream outside the writable mounts. Only relay-observed
// reports and usage can determine the model outcome and charge.
func (a Agent) Run(ctx context.Context, inv Invocation) (Result, error) {
	if inv.MaxTurns <= 0 || inv.MaxTurns > int(^uint(0)>>1)/3 {
		return Result{}, errors.New("invalid model turn allowance")
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
	attempt := "primary"
	streamName, errName := "transcript.jsonl", "stderr.txt"
	if inv.fallback {
		attempt, streamName, errName = "fallback", "transcript.fallback.jsonl", "stderr.fallback.txt"
	}
	inputDir := filepath.Join(HostDir(inv.Dir), "input")
	if err := hostDir(inputDir); err != nil {
		return Result{}, err
	}
	if !inv.fallback {
		if err := writeRunInput(inputDir, "system.md", []byte(inv.System)); err != nil {
			return Result{}, err
		}
		if err := writeRunInput(inputDir, "schema.json", []byte(inv.Schema)); err != nil {
			return Result{}, err
		}
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
		"--append-system-prompt-file", "/run/input/system.md",
		"--model", a.Model,
		"--max-turns", fmt.Sprint(inv.MaxTurns),
		"--max-budget-usd", strconv.FormatFloat(inv.BudgetUSD, 'f', -1, 64),
		"--json-schema", inv.Schema, "--strict-mcp-config")
	if a.Fallback != "" {
		args = append(args, "--fallback-model", a.Fallback)
	}
	args = append(args, "--no-session-persistence", "--add-dir", "/ro")
	socket := filepath.Join(HostDir(inv.Dir), "relay.sock")
	route, routeEnv, err := a.Bedrock.open(socket, inv.BudgetUSD, requestAllowance(inv))
	if err != nil {
		return Result{}, err
	}
	defer route.close()
	binds := []sandbox.Bind{
		{Src: filepath.Join(inv.Dir, "work"), Dst: "/work", Write: true},
		{Src: filepath.Join(inv.Dir, "out"), Dst: "/out", Write: true},
		{Src: inputDir, Dst: "/run/input"}, {Src: harness, Dst: "/opt/claude/claude"},
	}
	binds = append(append(binds, a.Binds...), inv.Binds...)
	for _, b := range binds {
		if !((b.Src == inputDir && b.Dst == "/run/input" || b.Src == filepath.Join(HostDir(inv.Dir), "records") && b.Dst == "/ro/records") && !b.Write) && (within(HostDir(inv.Dir), b.Src) || within(b.Src, HostDir(inv.Dir))) {
			return Result{}, errors.New("a run bind reaches its host-only directory")
		}
	}
	spec := sandbox.Spec{Unit: runUnit(inv.ID, inv.Dir, attempt), Host: HostDir(inv.Dir), Binds: binds,
		Relay: socket, Dir: "/work", Env: sandboxEnv(routeEnv, a.Env, inv.Env),
		// The unit's own limit is a last bound a minute past the run's timeout, so the host's timeout is the one that fires
		// and classifies the run (an expiry of the unit reads as an ordinary exit 1).
		Limits: sandbox.Limits{RuntimeSec: max(1, int(inv.Timeout.Seconds())) + 60},
		Argv:   append([]string{"/opt/claude/claude"}, args...)}
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
	stream, diag := &boundedFile{w: out, left: 64 << 20}, &boundedFile{w: errLog, left: MaxStream}
	runCtx, cancel := context.WithTimeout(ctx, inv.Timeout)
	defer cancel()
	start := time.Now()
	exit, waitErr := spec.Run(runCtx, stream, diag)
	if exit.Code == -1 && waitErr != nil && runCtx.Err() == nil {
		return Result{}, waitErr
	}
	// Failed is a diagnostic only. The authenticated report and host observations decide.
	if waitErr == nil && exit.Code != 0 {
		waitErr = fmt.Errorf("sandbox program exit %d", exit.Code)
	}
	timedOut := runCtx.Err() == context.DeadlineExceeded
	res, perr := readStream(transcript)
	res.Started = true
	res.Duration, res.Transcript, res.Model = time.Since(start), transcript, a.Model
	route.close()
	res.Refused = route.refusals()
	res.RelayUSD, res.RelayAdmitted, res.RelayRefused, res.RelayCharged = route.accounting()
	res.Usage, _ = route.observedUsage()
	res.Tokens = res.Usage.tokens()
	res.USD, res.CostKnown = res.RelayUSD, route.accountingKnown()
	reports, refusal := route.reportsObserved()
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
		if exit.Code != 0 && runCtx.Err() == nil {
			res.Status, res.Failure = store.RunFailed, fmt.Sprintf("exit %d", exit.Code)
		} else {
			reconcileExit(&res, waitErr)
		}
	case perr != nil && res.Status == store.RunOK:
		res.Status, res.Failure = store.RunFailed, "stream"
	}
	_, overTurns := route.observedUsage()
	if overTurns {
		res.Report = nil
		res.Status, res.Failure = store.RunTurns, "turns"
	}
	data, saveErr := json.Marshal(res)
	name := "result.json"
	if inv.fallback {
		name = "result.fallback.json"
	}
	if saveErr == nil {
		saveErr = writeExclusive(HostDir(inv.Dir), name, data)
	}
	return res, saveErr
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

func readRunFile(dir, name string) ([]byte, error) {
	f, err := OpenFile(dir, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadFile(f, runFileMax)
}

func runUnit(id, dir, attempt string) string {
	if id == "" {
		sum := sha256.Sum256([]byte(dir))
		id = hex.EncodeToString(sum[:8])
	}
	var b strings.Builder
	for _, c := range strings.ToLower(id) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	unit := "momentum-run-loop-" + b.String()
	if attempt == "fallback" {
		unit += "-fallback"
	}
	return unit
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
		if len(reports) > 0 {
			terminal := reports[len(reports)-1]
			if len(terminal) > 0 && bytes.Equal(candidate, terminal) {
				res.Report = append(json.RawMessage(nil), terminal...)
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

// sandboxEnv adds only the relay's provider variables and the harness controls.
func sandboxEnv(route []string, extra ...map[string]string) map[string]string {
	env := map[string]string{"CLAUDE_CONFIG_DIR": "/home/agent/.claude", "DISABLE_AUTOUPDATER": "1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"}
	for _, entry := range route {
		k, v, ok := strings.Cut(entry, "=")
		if ok {
			env[k] = v
		}
	}
	for _, m := range extra {
		for k, v := range m {
			env[k] = v
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
		if !res.CostKnown && res.Transcript != "" {
			res.ReservationUSD = inv.BudgetUSD
			res.USD = res.RelayUSD + res.ReservationUSD
		}
		return res, err
	}
	if !res.CostKnown || !res.TurnsKnown {
		res.ReservationUSD = inv.BudgetUSD
		res.USD = res.RelayUSD + res.ReservationUSD
		return res, nil
	}
	first := res
	firstCharge := first.USD
	inv = fallbackInvocation(inv, first, time.Since(started))
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

func fallbackInvocation(inv Invocation, first Result, elapsed time.Duration) Invocation {
	inv.BudgetUSD -= first.USD
	inv.requestLimit = requestAllowance(inv) - first.Turns
	inv.MaxTurns = min(inv.MaxTurns, inv.requestLimit)
	inv.Timeout -= elapsed
	return inv
}

// Each attempt's charge is observed by its relay. An unknown fallback holds its reservation.
func finishFallback(first, res Result, remaining float64) Result {
	if !res.CostKnown {
		res.ReservationUSD += remaining
	}
	res.ReservationUSD += first.ReservationUSD
	res.CostKnown = true
	res.Tokens += first.Tokens
	res.Usage.add(first.Usage)
	res.Turns += first.Turns
	res.Duration += first.Duration
	res.PriorHarness = append(append(first.PriorHarness, first.Harness), res.PriorHarness...)
	res.RelayUSD += first.RelayUSD
	res.USD = res.RelayUSD + res.ReservationUSD
	res.RelayAdmitted += first.RelayAdmitted
	res.RelayRefused += first.RelayRefused
	res.RelayCharged += first.RelayCharged
	res.Refused = slices.Concat(first.Refused, slices.DeleteFunc(res.Refused, func(r string) bool { return slices.Contains(first.Refused, r) }))
	return res
}

// readStream reads the untrusted candidate report and diagnostic claims. Only
// the host observer installs outcomes and accounting.
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
		res = Result{Status: store.RunFailed, Failure: "report", Report: r.StructuredOutput,
			Harness: HarnessDiagnostics{USD: r.TotalCostUSD, Turns: r.NumTurns,
				Usage:   Usage{Input: r.Usage.Input, Output: r.Usage.Output, CacheRead: r.Usage.CacheRead, CacheWrite5: r.Usage.CacheCreate},
				Session: r.SessionID, Denials: len(r.PermissionDenials), Subtype: r.Subtype, Text: r.Result, IsError: r.IsError}}
	}
	if err := sc.Err(); err != nil {
		return res, err
	}
	if !found {
		return res, errors.New("the run wrote no result")
	}
	return res, nil
}

// CleanEnv is the small environment of host-side evidence child processes.
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

// writeRunInput accepts an already-prepared host input only when its bytes match.
func writeRunInput(dir, name string, data []byte) error {
	raw, err := readHostInput(dir, name)
	if errors.Is(err, os.ErrNotExist) {
		return writeExclusive(dir, name, data)
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, data) {
		return errors.New("prepared run input differs")
	}
	return nil
}
func readHostInput(dir, name string) ([]byte, error) {
	f, err := OpenFile(dir, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadFile(f, MaxStream)
}
