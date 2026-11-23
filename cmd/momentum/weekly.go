package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/agent-sh/momentum/internal/config"
	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/scorecard"
	"github.com/agent-sh/momentum/internal/store"
)

// weeklyDir is where the weekly reports are kept: weekly/YYYY-Www.md beside the store.
func weeklyDir() string { return filepath.Join(paths.Data(), "weekly") }

// advisor runs the model pass: a system prompt, a prompt and a schema in, the report and what it cost out. A test
// stands in for it; the command's own runs the loop's agent (loop.Advice).
type advisor func(ctx context.Context, system, prompt, schema string) (report json.RawMessage, model string, usd float64, err error)

// weeklyDeps are what a weekly run reaches outside the store: the model pass, the switch that shuts a system down,
// the clock, and tacit's records.
type weeklyDeps struct {
	advise      advisor
	sw          scorecard.Switch
	now         func() time.Time
	deliveryDir string
	tacitStore  string
	constraints []string
}

func weeklyCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "show":
			return weeklyShow(args[1:], stdout)
		case "install":
			return weeklyInstall(args[1:], stdout, stderr)
		}
	}
	fs := flag.NewFlagSet("weekly", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dry := fs.Bool("dry", false, "compute and print the week's report: no model pass, and no verdict, case, note, report file or shutdown is written")
	week := fs.String("week", "", "the week, YYYY-Www (default: the last complete week, Sunday to Sunday)")
	asJSON := fs.Bool("json", false, "print the week's numbers and verdicts as one JSON object")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Sprintf("weekly takes show, install or flags, got %q", fs.Arg(0))}
	}
	var c config.Sources
	switch got, err := config.Load(); {
	case err == nil:
		c = got
	case !errors.Is(err, config.ErrNoSources):
		return err
	}
	deps := weeklyDeps{sw: scorecard.DefaultSwitch(), now: time.Now, deliveryDir: scorecard.DefaultDeliveryDir(),
		tacitStore: paths.Expand(c.Loop.TacitStore), constraints: c.Loop.Constraints}
	if c.Loop.TacitStore == "" {
		deps.tacitStore = filepath.Join(paths.Home(), ".local", "share", "tacit", "store")
	}
	open := store.OpenReadOnly // a dry run writes nothing, not even a migration
	if !*dry {
		unlock, err := weeklyLock(paths.DB())
		if err != nil {
			return err
		}
		defer unlock()
		open = store.Open
	}
	st, err := open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	deps.advise = loopAdvisor(st, c.Loop)
	return weekly(ctx, st, deps, *week, *dry, *asJSON, stdout, stderr)
}

// weeklyLock keeps one weekly run at a time: a second would stop the first's model pass and close its cases twice.
func weeklyLock(db string) (func(), error) {
	f, err := os.OpenFile(db+".weekly-lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another weekly run runs now")
		}
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// loopAdvisor is the model pass on the loop's agent: its runner, model and fallback, its Bedrock route, within the
// loop's weekly budget.
func loopAdvisor(st *store.Store, c config.Loop) advisor {
	return func(ctx context.Context, system, prompt, schema string) (json.RawMessage, string, float64, error) {
		model, fallback := c.Model, c.FallbackModel
		if model == "" {
			model = defaultModel
		}
		if fallback == "" {
			fallback = defaultFallback
		}
		agent := loop.Agent{Runner: c.Runner, Model: model, Fallback: fallback, Bedrock: &loop.Bedrock{File: c.BedrockEnv}}
		if err := onBedrock(agent); err != nil {
			return nil, "", 0, err
		}
		b := selectionRunner(st, c).Budgets
		for _, w := range []struct {
			name   string
			span   time.Duration
			budget float64
		}{{"day", 24 * time.Hour, b.Day}, {"week", 7 * 24 * time.Hour, b.Week}} {
			spent, err := st.LoopSpend(ctx, time.Now().Add(-w.span), 0)
			if err != nil {
				return nil, "", 0, err
			}
			if spent+loop.AdviceBudgetUSD > w.budget {
				return nil, "", 0, fmt.Errorf("the loop's %s has $%.2f of its $%.2f left, under the pass's $%.2f", w.name, max(0, w.budget-spent), w.budget, loop.AdviceBudgetUSD)
			}
		}
		clean, err := loop.RedactAll(ctx, []string{system, prompt})
		if err != nil {
			return nil, "", 0, fmt.Errorf("the scrubber: %w", err)
		}
		a := loop.Advice{Agent: agent, Store: st, Dir: filepath.Join(loopDir(), "weekly"), System: clean[0], Prompt: clean[1],
			Schema: adviceSchema, BudgetUSD: loop.AdviceBudgetUSD, MaxTurns: 3, Timeout: 10 * time.Minute,
			Family: store.TacitFamily, Step: "weekly"}
		report, run, err := a.Run(ctx)
		return report, run.Model, run.USD, err
	}
}

// weekly runs one week's observation: the numbers, the verdicts and any shutdown, the model pass and its cases, the
// report file and its note.
func weekly(ctx context.Context, st *store.Store, d weeklyDeps, name string, dry, asJSON bool, stdout, stderr io.Writer) error {
	now := d.now()
	w := scorecard.LastComplete(now)
	if name != "" {
		var err error
		if w, err = scorecard.ParseWeek(name, now.Location()); err != nil {
			return usageError{err.Error()}
		}
		if w.End.After(now) {
			return usageError{fmt.Sprintf("week %s has not ended", w.Name)}
		}
		if w.Name != scorecard.LastComplete(now).Name && !dry {
			// an older week is judged on records that have aged out since: it is a report, never a verdict
			return usageError{fmt.Sprintf("week %s is not the last complete week (%s): only --dry reports it", w.Name, scorecard.LastComplete(now).Name)}
		}
	}
	o, err := scorecard.Observe(ctx, scorecard.Inputs{Store: st, DeliveryDir: d.deliveryDir, TacitStore: d.tacitStore}, w)
	if err != nil {
		return err
	}
	sc, err := scorecard.Score(ctx, st, d.sw, o, dry, now)
	if err != nil {
		return err
	}
	var a scorecard.Advice
	var failed []error // what went wrong after the verdicts: the run still keeps its report and notes, then fails
	report := filepath.Join(weeklyDir(), w.Name+".md")
	if dry {
		a.None = "a dry run asks no model"
	} else {
		a = advise(ctx, st, d, o, sc)
		if a.None != "" {
			failed = append(failed, errors.New("the model pass: "+a.None))
		}
		if err := trackAdvice(ctx, st, &a, report); err != nil {
			failed = append(failed, fmt.Errorf("the recommendations' cases: %w", err))
		}
	}
	text := scorecard.Render(o, sc, a)
	if asJSON {
		out := map[string]any{"week": w.Name, "start": w.Start, "end": w.End, "tacit": o.Tacit, "momentum": o.Momentum, "scorecard": sc}
		if !dry {
			out["report"] = report
		}
		if err := json.NewEncoder(stdout).Encode(out); err != nil {
			return err
		}
	}
	if dry {
		if !asJSON {
			fmt.Fprint(stdout, text)
		}
		return nil
	}
	for _, sd := range sc.Shutdowns { // first: a system turned off is said so before anything else can fail
		if _, err := st.PutNote(ctx, shutdownNote(w, sc, sd, now)); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(weeklyDir(), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(report, []byte(text), 0o600); err != nil {
		return err
	}
	if _, err := st.PutNote(ctx, weeklyNote(w, sc, a, now)); err != nil {
		return err
	}
	if !asJSON {
		fmt.Fprintf(stdout, "%s: %s\n", w.Name, report)
		for _, v := range sc.Verdicts {
			fmt.Fprintf(stdout, "  %s: %s\n", v.System, v.Why)
		}
	}
	return errors.Join(failed...)
}

// weeklyNote is the week's note in the report: decided, high, what each system came to and where the report is.
func weeklyNote(w scorecard.Week, sc scorecard.Scored, a scorecard.Advice, now time.Time) store.Note {
	var parts []string
	for _, v := range sc.Verdicts {
		state := "does not deliver"
		if v.Delivers {
			state = "delivers"
		}
		if !v.Scored {
			state += " (not scored)"
		}
		parts = append(parts, v.System+" "+state)
	}
	title := "Weekly " + w.Name + ": " + strings.Join(parts, ", ")
	detail := fmt.Sprintf("momentum weekly show %s prints the report.", w.Name)
	for _, s := range sc.Standings {
		if s.On {
			detail += fmt.Sprintf(" %s: %d weeks to shutdown.", s.System, s.ToShutdown)
		}
	}
	if n := len(a.Recommendations); n > 0 {
		detail += fmt.Sprintf(" %d tacit changes are open as cases of family tacit.", n)
	}
	return store.Note{ID: "weekly:" + w.Name, Kind: store.NoteDecided, Title: title, Detail: detail, Source: "momentum weekly",
		Importance: store.ImportanceHigh, At: now}
}

// shutdownNote says what a shutdown shut, why, and how to turn it back on.
func shutdownNote(w scorecard.Week, sc scorecard.Scored, sd scorecard.Shutdown, now time.Time) store.Note {
	why := ""
	for _, v := range sc.Verdicts {
		if v.System == sd.System {
			why = v.Why
		}
	}
	detail := fmt.Sprintf("%d scored weeks in a row did not deliver. %s: %s. Turn it back on: %s", scorecard.FailingWeeks, w.Name, why, sd.Restore)
	if len(sd.Failed) > 0 {
		detail += ". Not done: " + strings.Join(sd.Failed, "; ")
	}
	return store.Note{ID: "scorecard-shutdown:" + sd.System + ":" + w.Name, Kind: store.NoteDecided,
		Title: "momentum's scorecard shut " + sd.System + " down: it did not deliver two weeks in a row", Detail: detail,
		Source: "momentum weekly", Importance: store.ImportanceHigh, At: now}
}

// adviceSchema is the model pass's report: the ranked changes, and the open recommendations the numbers show handled.
const adviceSchema = `{"type":"object","additionalProperties":false,"required":["recommendations","handled"],"properties":{
"recommendations":{"type":"array","maxItems":5,"items":{"type":"object","additionalProperties":false,
 "required":["key","title","change","evidence","expected"],"properties":{
 "key":{"type":"string","description":"a short lowercase slug naming the change; reuse an open case's key when it is the same change"},
 "title":{"type":"string"},"change":{"type":"string"},"evidence":{"type":"string"},"expected":{"type":"string"}}}},
"handled":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["case","why"],"properties":{
 "case":{"type":"string"},"why":{"type":"string"}}}}}}`

// rulesFiles are the agent rules files no recommendation may promote facts into (the owner's constraint).
var rulesFiles = regexp.MustCompile(`(?i)\b(agents|claude|shared|gemini|memory|rules|copilot-instructions)\.md\b|\.cursorrules|\brules files?\b|\bsystem prompt\b`)

const adviceSystem = `You review one week of measurements of tacit, a memory system that delivers facts to coding agents through hooks (a block of fact lines at a session's start, at each prompt, before tool calls) and an MCP server (tacit_search, tacit_fact, tacit_tree, tacit_add, tacit_mark). Your answer is a short ranked list of concrete changes to tacit itself, its delivery, ranking, gates, curation, self-mark asks or judge, each tied to the numbers or the samples you were given, with the number it should move. Rank by expected effect on the scorecard: agents in interactive sessions marking delivered lines used at least as often as not relevant, and the owner having to restate what tacit already holds. Never propose promoting facts into AGENTS.md, CLAUDE.md, SHARED.md or any other agent rules file. Propose nothing you cannot tie to the evidence; fewer items are better than weak ones. For each open recommendation case you were given, list it under handled only when this week's numbers show the problem it named is gone, and say which numbers.`

// advise runs the model pass over the week's numbers and samples. It never fails the run: what went wrong is the
// advice's None.
func advise(ctx context.Context, st *store.Store, d weeklyDeps, o *scorecard.Observation, sc scorecard.Scored) scorecard.Advice {
	open, err := st.Cases(ctx, store.CaseFilter{Family: store.TacitFamily})
	if err != nil {
		return scorecard.Advice{None: "the open tacit cases could not be read: " + err.Error()}
	}
	system := adviceSystem
	for _, c := range d.constraints {
		system += "\nThe owner's constraint: " + c
	}
	prompt := advicePrompt(o, sc, open)
	raw, model, usd, err := d.advise(ctx, system, prompt, adviceSchema)
	a := scorecard.Advice{Model: model, USD: usd}
	if err != nil {
		a.None = err.Error()
		return a
	}
	var got scorecard.Advice
	if err := json.Unmarshal(raw, &got); err != nil {
		a.None = "its report is not the schema's: " + err.Error()
		return a
	}
	openIDs := map[string]bool{}
	for _, c := range open {
		openIDs[c.ID] = true
	}
	for _, r := range got.Recommendations {
		r.Key = slug(r.Key)
		if r.Key == "" || strings.TrimSpace(r.Title) == "" {
			continue
		}
		if rulesFiles.MatchString(r.Title + " " + r.Change + " " + r.Evidence + " " + r.Expected) {
			a.Dropped = append(a.Dropped, r.Title)
			continue
		}
		a.Recommendations = append(a.Recommendations, r)
	}
	for _, h := range got.Handled {
		if openIDs[h.Case] && strings.TrimSpace(h.Why) != "" {
			a.Handled = append(a.Handled, h)
		}
	}
	return a
}

var notSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.Trim(notSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 60 {
		s = strings.TrimRight(s[:60], "-")
	}
	return s
}

// advicePrompt is the model pass's input: the week's report without its advice, the samples, and the open cases.
func advicePrompt(o *scorecard.Observation, sc scorecard.Scored, open []store.Case) string {
	var b strings.Builder
	b.WriteString(scorecard.Render(o, sc, scorecard.Advice{None: "this is the pass"}))
	b.WriteString("\n## Samples\n\nMost delivered facts this week:\n")
	for _, f := range o.Top {
		fmt.Fprintf(&b, "- %s: %s\n", f.Name, cut(f.Text, 240))
	}
	b.WriteString("\nFacts marked or labeled not relevant most this week:\n")
	for _, f := range o.Worst {
		fmt.Fprintf(&b, "- %s: %s\n", f.Name, cut(f.Text, 240))
	}
	b.WriteString("\nOwner restatements of facts tacit holds (the owner's message, the fact):\n")
	for i, r := range o.Tacit.Restated {
		if i == 20 {
			break
		}
		fmt.Fprintf(&b, "- delivered in the session before: %t. Message: %s. Fact %s: %s\n", r.Delivered, cut(r.Text, 300), r.Fact, cut(r.FactText, 240))
	}
	b.WriteString("\nOpen recommendation cases (id, key, title, change):\n")
	for _, c := range open {
		if c.Status == store.CaseClosed {
			continue
		}
		fmt.Fprintf(&b, "- %s, %s, %s, %s\n", c.ID, keyOf(c), c.Title, cut(c.NextTest, 240))
	}
	return b.String()
}

func cut(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

// signaturePrefix starts a recommendation case's signature: its key after it.
const signaturePrefix = "tacit:"

func keyOf(c store.Case) string {
	for _, s := range c.Signatures {
		if k, ok := strings.CutPrefix(s, signaturePrefix); ok {
			return k
		}
	}
	return ""
}

// trackAdvice files each recommendation as a case of family tacit, the same change in the same case week after
// week, and closes the ones the numbers show handled as already solved.
func trackAdvice(ctx context.Context, st *store.Store, a *scorecard.Advice, report string) error {
	var errs []error
	for i := range a.Recommendations {
		r := &a.Recommendations[i]
		c, _, err := st.ObserveCase(ctx, store.Finding{Family: store.TacitFamily, Signature: signaturePrefix + r.Key, Title: r.Title,
			Evidence: []store.Evidence{{Ref: "file:" + report, At: time.Now(), Kind: "weekly", Summary: r.Evidence}}}, "weekly")
		if err != nil {
			errs = append(errs, err)
			continue
		}
		r.Case = c.ID
		if c.Status == store.CaseClosed {
			continue // a closed one keeps the evidence and comes back only as its rules say
		}
		change, expected := r.Change, r.Expected
		if _, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{NextTest: &change, Benefit: &expected}, "weekly"); err != nil {
			errs = append(errs, err)
		}
	}
	var kept []scorecard.Handled
	for _, h := range a.Handled {
		if slices.ContainsFunc(a.Recommendations, func(r scorecard.Recommendation) bool { return r.Case == h.Case }) {
			continue // recommended again this week: not handled
		}
		if _, err := st.CloseCase(ctx, h.Case, store.Solved, "", "the weekly numbers show it handled: "+h.Why, "weekly"); err != nil {
			errs = append(errs, err)
			continue
		}
		kept = append(kept, h)
	}
	a.Handled = kept
	return errors.Join(errs...)
}

// weeklyShow prints a week's report: the newest with no week named.
func weeklyShow(args []string, stdout io.Writer) error {
	if len(args) > 1 {
		return usageError{"weekly show takes at most one week"}
	}
	name := ""
	if len(args) == 1 {
		if _, err := scorecard.ParseWeek(args[0], time.Local); err != nil {
			return usageError{err.Error()}
		}
		name = args[0]
	} else {
		des, err := os.ReadDir(weeklyDir())
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for _, de := range des {
			if n, ok := strings.CutSuffix(de.Name(), ".md"); ok {
				if _, err := scorecard.ParseWeek(n, time.Local); err == nil && n > name {
					name = n
				}
			}
		}
		if name == "" {
			return fmt.Errorf("no weekly report in %s yet", weeklyDir())
		}
	}
	b, err := os.ReadFile(filepath.Join(weeklyDir(), name+".md"))
	if err != nil {
		return err
	}
	_, err = stdout.Write(b)
	return err
}

// The weekly unit and its timer: Sunday 08:00 in the user manager's local time, a missed run fired at the next
// start. Its memory bound is four times a full observation's peak on a working machine (about half a gigabyte); the
// model pass runs in a sandbox unit of its own.
const weeklyService = `[Unit]
Description=momentum: the weekly observation of tacit and momentum, and their scorecard
Wants=momentum-ingest.service
After=momentum-ingest.service

[Service]
Type=oneshot
Environment=PATH=%%h/.local/bin:/usr/local/bin:/usr/bin:/bin
%sExecStart=%s weekly
Nice=10
TimeoutStartSec=1h
MemoryHigh=1536M
MemoryMax=2G
MemorySwapMax=1G
StandardOutput=journal
StandardError=journal
`

const weeklyTimer = `[Unit]
Description=momentum: the weekly observation, Sunday morning

[Timer]
OnCalendar=Sun *-*-* 08:00:00
Persistent=true
RandomizedDelaySec=5min

[Install]
WantedBy=timers.target
`

// weeklyInstall writes the weekly unit and timer into the user's systemd directory (or --dir). It enables nothing:
// it prints the command that does.
func weeklyInstall(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("weekly install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "where to write the units (default: $XDG_CONFIG_HOME/systemd/user)")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if *dir == "" {
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" || !filepath.IsAbs(base) {
			base = filepath.Join(paths.Home(), ".config")
		}
		*dir = filepath.Join(base, "systemd", "user")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	// where tacit and momentum keep their records, as this shell has them: the user manager's environment may not
	env := ""
	for _, k := range []string{"TACIT_STATE_DIR", "TACIT_STORE", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "MOMENTUM_HOME", "MOMENTUM_CONFIG"} {
		if v := os.Getenv(k); v != "" && !strings.ContainsAny(v, "\n\"%") {
			env += fmt.Sprintf("Environment=\"%s=%s\"\n", k, v)
		}
	}
	for name, text := range map[string]string{"momentum-weekly.service": fmt.Sprintf(weeklyService, env, exe), "momentum-weekly.timer": weeklyTimer} {
		if err := os.WriteFile(filepath.Join(*dir, name), []byte(text), 0o644); err != nil {
			return err
		}
		fmt.Fprintln(stdout, filepath.Join(*dir, name))
	}
	fmt.Fprintln(stdout, "enable it: systemctl --user daemon-reload && systemctl --user enable --now momentum-weekly.timer")
	return nil
}

// scorecardCmd prints where each system stands: on or off, its scored weeks, how many failed in a row and how many
// more would shut it down.
func scorecardCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("scorecard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "one JSON object")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Sprintf("scorecard takes no arguments, got %q", fs.Arg(0))}
	}
	st, err := store.OpenReadOnly(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	return printScorecard(ctx, st, scorecard.DefaultSwitch(), *asJSON, stdout)
}

func printScorecard(ctx context.Context, st *store.Store, sw scorecard.Switch, asJSON bool, stdout io.Writer) error {
	type week struct {
		Week     string `json:"week"`
		Delivers bool   `json:"delivers"`
		Why      string `json:"why"`
	}
	type standing struct {
		scorecard.Standing
		Scored []week `json:"scored_weeks"`
	}
	var out []standing
	for _, system := range []string{scorecard.Tacit, scorecard.Momentum} {
		s, err := scorecard.Stand(ctx, st, sw, system)
		if err != nil {
			return err
		}
		x := standing{Standing: s, Scored: []week{}}
		for _, w := range s.Weeks {
			x.Scored = append(x.Scored, week{w.Week, w.Delivers, w.Why})
		}
		out = append(out, x)
	}
	if asJSON {
		return json.NewEncoder(stdout).Encode(map[string]any{"systems": out})
	}
	for _, s := range out {
		state := "on"
		if !s.On {
			state = "off"
			if s.Shut != "" {
				state += ", shut down in " + s.Shut
			}
		}
		switch {
		case s.Since.IsZero():
			fmt.Fprintf(stdout, "%s: %s; not deployed: the first momentum weekly run starts its scoring\n", s.System, state)
		default:
			fmt.Fprintf(stdout, "%s: %s; scored from %s; %d scored weeks, %d failing in a row; weeks to shutdown: %d\n",
				s.System, state, s.FirstScored, len(s.Scored), s.Failing, s.ToShutdown)
		}
		for _, w := range s.Scored {
			v := "fails"
			if w.Delivers {
				v = "delivers"
			}
			fmt.Fprintf(stdout, "  %s %s: %s\n", w.Week, v, w.Why)
		}
	}
	return nil
}
