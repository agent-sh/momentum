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
	"os"
	"os/exec"
	"path/filepath"
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
	Status     string // store.RunOK, RunFailed, RunBudget, RunTurns, RunTimeout
	Failure    string // how it failed, when it did: a short key the stop rule compares
	USD        float64
	Tokens     int64
	Turns      int
	Duration   time.Duration
	Report     json.RawMessage // the final report, by the schema
	Text       string          // the final text
	Denials    int             // calls the permissions refused
	Session    string
	Transcript string // the run's stream of events, in Dir
	Model      string // the model that ran it
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
		"--max-budget-usd", fmt.Sprintf("%.2f", inv.BudgetUSD),
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
	cmd := exec.Command(prog, argv...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = inv.Dir, a.Env, out, errLog
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
	res.Duration, res.Transcript, res.Model = time.Since(start), transcript, a.Model
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
	res, err := a.Run(ctx, inv)
	if err != nil || res.Failure != "the model's safeguards" || a.Fallback == "" || a.Fallback == a.Model {
		return res, err
	}
	first := res
	b := a
	b.Model, b.Fallback = a.Fallback, ""
	os.Rename(filepath.Join(inv.Dir, "transcript.jsonl"), filepath.Join(inv.Dir, "transcript.refused.jsonl"))
	res, err = b.Run(ctx, inv)
	res.USD += first.USD
	res.Tokens += first.Tokens
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
			NumTurns          int             `json:"num_turns"`
			TotalCostUSD      float64         `json:"total_cost_usd"`
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
		res = Result{USD: r.TotalCostUSD, Turns: r.NumTurns, Text: r.Result, Report: r.StructuredOutput, Session: r.SessionID,
			Denials: len(r.PermissionDenials), Tokens: r.Usage.Input + r.Usage.Output + r.Usage.CacheRead + r.Usage.CacheCreate}
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

// CleanEnv is the environment a run starts with: the user's home, a path to momentum's tools, the locale and the
// user's session bus (the provider's credentials come from the user's keyring, and the scope from his systemd),
// and what the loop sets. Nothing else of the caller's environment reaches it: a harness's plugin list or session
// id would change what runs. A check's test gets less (Sandbox).
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
