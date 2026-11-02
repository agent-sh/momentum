// Package loop is momentum's improvement loop: one agent loop with skills and tools. Each run takes one case of
// one family through its next step (investigate, build, review, publish) with the harness's own main agent run
// headless, the family's skill appended to its system prompt, and tools whose hard limits the harness and the
// ship tool enforce: permissions that let it read and run only momentum's own tools, a budget and a time for the
// run, and gates on every write (ship.go, gate.go). The runner (runner.go) picks the cases, keeps the budgets, and
// stops a case that fails the same way twice.
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// Agent starts the harness's main agent headless: Claude Code's print mode, by the runner command the machine
// configures (its first word a program, the rest its first arguments).
type Agent struct {
	Runner   []string
	Model    string
	Fallback string
	// Env is the environment the agent runs with, whole: the runner builds it (cleanEnv), so nothing of the
	// calling shell's (a harness's plugin list, a session's id) reaches the run.
	Env []string
	// Scope runs each run in a systemd user scope of its own (systemd-run), so its stop reaches every process it
	// started, those that left its process group too. Without it, the stop reaches its process group.
	Scope bool
	// Bedrock is the run's route: every run is on Bedrock, through the loopback broker, and one with no route does
	// not start.
	Bedrock *Bedrock
}

// DefaultTools are the harness's built-in tools a loop run has: the file tools (confined to its directories),
// the shell (only the commands its permissions allow) and subagents.
const DefaultTools = "Read,Grep,Glob,Write,Edit,Bash,Agent"

// Invocation is one run of the agent.
type Invocation struct {
	Dir       string // the run's own directory: its working directory, where its files and its transcript go
	Prompt    string
	System    string   // appended to the harness's system prompt: the loop's procedure, the family's skill, the rules
	Settings  Settings // the harness's permissions for the run
	Schema    string   // the JSON schema of the run's final report
	MaxTurns  int
	BudgetUSD float64
	Timeout   time.Duration
	MCP       string   // an MCP configuration file, or none
	Persist   bool     // keep the session's transcript (live runs); an evaluation's runs keep none
	AddDirs   []string // the directories the file tools reach besides Dir
	Tools     string   // the built-in tools it has: DefaultTools when empty, "none" none (a judge), or a list
}

// Settings are the permissions the harness enforces: what the agent may call, by the harness's rules. The mode is
// dontAsk, so a call no allow rule matches is refused, never asked.
type Settings struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

// Result is what a run came to.
type Result struct {
	promptHash string // the evaluation prompt as sent, before the run can change its files
	Status     string // store.RunOK, RunFailed, RunBudget, RunTurns, RunTimeout
	Failure    string // how it failed, when it did: a short key the stop rule compares
	CostKnown  bool
	TurnsKnown bool
	USD        float64 // at Bedrock's rates for the models with one (bedrockRates), at the harness's own for the rest
	listUSD    float64 // the harness's own price for the run, at the provider's list rates
	Tokens     int64
	Usage      Usage // the tokens, by kind
	Turns      int
	Duration   time.Duration
	Report     json.RawMessage // the final report, by the schema
	Text       string          // the final text
	Denials    int             // calls the permissions refused
	Session    string
	Transcript string // the run's stream of events, in Dir
	Model      string // the model that ran it
	answered   bool   // the stream holds a message of the model's, whether or not a result followed
}

// stopGrace is how long a run that ran out of its time has to stop after SIGTERM before it is killed.
var stopGrace = 10 * time.Second

// Run runs the agent once and returns what came of it. It writes the run's files into inv.Dir (its system prompt,
// settings, schema and the stream of its events) and waits for it, at most inv.Timeout; then it stops the run and
// every process it started.
//
// The harness runs restricted: it reads none of the user's settings files (only the run's own, so no allow rule of
// his reaches a run), its file tools reach only the run's directories (Dir and AddDirs), only a person may
// approve a write to a settings, git or tool-configuration file, and it has only the tools named. --bare keeps
// the user's hooks, plugins, memory and CLAUDE.md out.
func (a Agent) Run(ctx context.Context, inv Invocation) (Result, error) {
	if len(a.Runner) == 0 {
		return Result{}, errors.New("no runner: set loop.runner in sources.json")
	}
	if a.Bedrock == nil {
		return Result{}, fmt.Errorf("%w: every headless run is on Bedrock, and none falls back to the direct API", ErrNoBedrock)
	}
	if err := os.MkdirAll(inv.Dir, 0o700); err != nil {
		return Result{}, err
	}
	write := func(name string, data []byte) (string, error) {
		p := filepath.Join(inv.Dir, name)
		return p, os.WriteFile(p, data, 0o600)
	}
	system, err := write("system.md", []byte(inv.System))
	if err != nil {
		return Result{}, err
	}
	raw, err := json.MarshalIndent(map[string]any{"permissions": inv.Settings}, "", "  ")
	if err != nil {
		return Result{}, err
	}
	settings, err := write("settings.json", raw)
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
		"--bare", "--restricted", "--tools", tools, "--settings", settings, "--permission-mode", "dontAsk",
		"--output-format", "stream-json", "--verbose",
		"--append-system-prompt-file", system,
		"--model", a.Model,
		"--max-turns", fmt.Sprint(inv.MaxTurns),
		"--max-budget-usd", strconv.FormatFloat(inv.BudgetUSD, 'f', -1, 64),
		"--json-schema", inv.Schema)
	if a.Fallback != "" {
		args = append(args, "--fallback-model", a.Fallback)
	}
	if inv.MCP != "" {
		args = append(args, "--mcp-config", inv.MCP)
	}
	args = append(args, "--strict-mcp-config")
	if !inv.Persist {
		args = append(args, "--no-session-persistence")
	}
	for _, d := range inv.AddDirs {
		args = append(args, "--add-dir", d)
	}
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
	runCtx, cancel := context.WithTimeout(ctx, inv.Timeout)
	defer cancel()
	prog, argv := a.Runner[0], args
	unit := ""
	if a.Scope {
		unit = ScopeUnit(inv.Dir)
		prog, argv = "systemd-run", append([]string{"--user", "--scope", "--quiet", "--collect", "--unit", unit, "--", a.Runner[0]}, args...)
	}
	// the route is read now, at this launch: the broker writes its file again when it restarts
	route, routeEnv, err := a.Bedrock.open()
	if err != nil {
		return Result{}, err
	}
	defer route.close()
	cmd := exec.Command(prog, argv...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = inv.Dir, routed(a.Env, routeEnv), out, errLog
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // its own group: a stop reaches every process it started
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start %s: %w", prog, err)
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
	// what it started dies with it: its group, and its scope's every process, those that left the group too
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if unit != "" {
		StopScope(unit)
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
	// the proof a run was on Bedrock: the model ran (a result, or only a message of its own in a run cut short),
	// and the broker answered a model request of it, through the run's relay
	if modelRan(res) && route.models.Load() == 0 {
		res.Status, res.Failure = store.RunFailed, "not on Bedrock"
		if res.CostKnown {
			res.USD = res.listUSD // it went by another route, at that route's prices: what its stream says it spent
		}
		return res, fmt.Errorf("%w: the run's model answered, but no request of it reached the Bedrock broker", ErrNoBedrock)
	}
	return res, nil
}

// ScopeUnit is the systemd scope a run runs in, named by its directory: two runs at once never share one.
func ScopeUnit(dir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(dir)))
	return "momentum-loop-" + hex.EncodeToString(sum[:6]) + ".scope"
}

// StopScope kills every process of a run's scope and stops it.
func StopScope(unit string) {
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exec.CommandContext(cctx, "systemctl", "--user", "kill", "--signal=SIGKILL", unit).Run()
	exec.CommandContext(cctx, "systemctl", "--user", "stop", unit).Run()
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
	res.listUSD += first.listUSD
	res.CostKnown = true
	res.Tokens += first.Tokens
	res.Usage = res.Usage.Add(first.Usage)
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
		var message struct {
			Type    string
			Message struct{ Model string }
		}
		if json.Unmarshal(line, &message) == nil && message.Type == "assistant" {
			res.answered = true
			if message.Message.Model != "" {
				res.Model = message.Message.Model
			}
		}
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
		res = Result{Model: res.Model, answered: res.answered, Text: r.Result, Report: r.StructuredOutput, Session: r.SessionID,
			Denials: len(r.PermissionDenials), Usage: Usage{Input: r.Usage.Input, Output: r.Usage.Output, CacheRead: r.Usage.CacheRead,
				CacheWrite: r.Usage.CacheCreate, CacheWrite1h: r.Usage.CacheCreation.Hour}}
		if r.TotalCostUSD != nil && known(*r.TotalCostUSD) {
			res.USD, res.listUSD, res.CostKnown = *r.TotalCostUSD, *r.TotalCostUSD, true
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

// CleanEnv is the environment a run starts with: the user's home, a path to momentum's tools, the locale and the
// user's session bus (the provider's credentials come from the user's keyring, and the scope from his systemd),
// and what the loop sets. Nothing else of the caller's environment reaches it: a harness's plugin list or session
// id would change what runs. A check's test gets less (Sandbox).
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
