package loop

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
)

// An investigation is the native path's: the harness's agent with the native brief, the owner's standing
// constraints and momentum's evidence commands, asked for the root cause and the fix, with the case's evidence in
// its prompt. It has no procedure and no family skill, and it keeps no case: the runner writes its report into the
// case when it ends. The golden-set evaluation found the procedure, the skill and the agent's own case bookkeeping
// cost more per root cause reached than the native path and reached no more (SPEC, One loop with skills).

// nativeBrief is the system prompt's brief of an investigation, the same in a live run and in both arms of the
// evaluation. What the file tools reach besides the working directory (an evaluation's repository checkouts) is
// named in the prompt, with the commands.
const nativeBrief = `You find the root cause of a problem in an agent system on one machine, from what the machine recorded.
Your file tools reach your working directory, and any repository checkouts the prompt names. Read the records through momentum's evidence commands.
The owner's words are only what his own turns say (actor owner); never credit him with text an agent, a tool or the harness wrote.`

// investigationSchema is an investigation's report.
const investigationSchema = `{"type":"object","additionalProperties":false,"required":["mechanism","fix","summary"],
"properties":{"mechanism":{"type":"string"},"alternatives":{"type":"array","items":{"type":"string"}},
"fix":{"type":"string"},"summary":{"type":"string"}}}`

// findingsInstruction keeps what a run cut short found: the runner reads the file when the run has no report.
const findingsInstruction = "Keep findings as you go in findings.md in your directory. Use the headings ## Mechanism, ## Alternatives ruled out, ## Fix, and ## Summary. End with your report of the same fields."

// liveQuestion is what a live investigation is asked.
const liveQuestion = "Find the root cause of this problem and the fix. State the mechanism, the alternatives ruled out, the fix, and a summary. Build no change."

// liveCommands is a live investigation's command reference: what it reads the machine with, through its door.
func liveCommands(readDirs []string) string {
	var b strings.Builder
	b.WriteString(`

## Commands available in this run
Your working directory is your run's directory. Your sandbox holds its files and nothing of the machine: you read the machine's records and repositories with these commands, which run outside for you and print what the secret scrubber has passed.
- momentum evidence show ID [ID...] [--bytes N]: reads up to ten stored events, each with the full transcript or log record it names; --bytes N prints each record's first N bytes.
- momentum evidence search TEXT [--source SOURCE] [--since WHEN] [--until WHEN] [--kind K,K] [--actor ACTOR] [--session ID] [--limit N]: matches stored summaries of every source, newest first; --source narrows it and must name one the store holds. Actors: owner, agent, tool, harness, detector, unknown. A phrase that starts with a dash goes after --.
- momentum evidence events [--source SOURCE] [--kind K,K] [--since WHEN] [--until WHEN] [--harness NAME] [--actor ACTOR] [--session ID] [--limit N] [--json]: reads stored events, optionally one session; each line names its event's ev:ID.
- momentum evidence status [--since WHEN]: counts stored events by source and kind.
`)
	b.WriteString("- momentum evidence git DIR CMD ...: reads a repository's history and code with log, show, diff, blame, grep, ls-files, ls-tree, rev-parse, rev-list, merge-base, describe, status, shortlog, or listing forms of branch and tag; `show HEAD:PATH` prints a file as committed. Use read options only (git's -N for a count; grep's first word is its pattern); revisions are checked and paths follow --. A grep with no match exits 1.")
	if len(readDirs) > 0 {
		b.WriteString(" DIR lies under one of the directories the loop reads: " + strings.Join(readDirs, ", ") + ".")
	}
	b.WriteString(`
- momentum evidence unit NAME [--lines N]: reads a user service's state and the end of its log.
- momentum supervise replay [--since WHEN] [--until WHEN] [--anchor]: runs supervision's rules over the store's history.
- momentum report: what needs the owner, what was decided and shipped, and what runs now.
- tacit search|fact|tree ...: the owner's rules, decisions and past mistakes.
No other momentum or tacit command runs: the sandbox's door refuses it.
`)
	return b.String()
}

// investigationSystem is an investigation's system prompt: the native brief, the owner's standing constraints and,
// in a live run, the machine's rules files, read at each run.
func investigationSystem(rules, constraints []string) (string, error) {
	var b strings.Builder
	b.WriteString(nativeBrief + "\n")
	for _, c := range constraints {
		fmt.Fprintf(&b, "- %s\n", c)
	}
	for _, r := range rules {
		raw, err := os.ReadFile(r)
		if err != nil {
			return "", fmt.Errorf("a rules file the loop reads: %w", err)
		}
		fmt.Fprintf(&b, "\n# The machine's rules: %s\n\n%s\n", r, strings.TrimSpace(string(raw)))
	}
	return b.String(), nil
}

// investigate runs an investigation in its sandbox: the native path's brief, commands and report, and the case's
// title, evidence and record in its prompt. Its door runs no case command; the runner writes its report into the
// case when it ends (finish).
func (r *Runner) investigate(ctx context.Context, c store.Case, run store.LoopRun) (Result, error) {
	system, err := investigationSystem(r.Rules, r.Constraints)
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(run.Dir, 0o700); err != nil {
		return Result{}, err
	}
	evidence, err := caseEvidence(ctx, r.Store, c)
	if err != nil {
		return Result{}, err
	}
	inv := Invocation{Dir: run.Dir, System: system, Schema: investigationSchema, MaxTurns: r.Budgets.MaxTurns, BudgetUSD: r.Budgets.Run,
		Timeout: r.Budgets.RunTime, Persist: true}
	inv.Prompt = fmt.Sprintf("%s\nProblem: %s\n%s%s%s%s", liveQuestion, c.Title, evidence, caseHolds(c), findingsInstruction, liveCommands(r.ReadDirs))
	inv.Door = r.door(StepInvestigate, c, run, []string{run.Dir})
	// the prompt holds the case's title and evidence, which a detector made of what it read: it reaches the run scrubbed
	if inv.Prompt, err = Redact(ctx, inv.Prompt); err != nil {
		return Result{}, err
	}
	agent := r.Agent
	agent.Env = map[string]string{"MOMENTUM_ACTOR": "loop:" + run.ID, "MOMENTUM_LOOP_RUN": run.ID, "MOMENTUM_LOOP_CASE": c.ID, "MOMENTUM_LOOP_DIR": run.Dir}
	return agent.RunOrFallback(ctx, inv)
}

// evidenceShown is how much of a case's evidence an investigation's prompt lists, newest first.
const evidenceShown = 20

// caseEvidence lists a case's evidence for an investigation's prompt, newest first: each event as evidence search
// prints it (its time, id, session, actor, kind, tool and stored summary), any other reference with its own time,
// kind and summary.
func caseEvidence(ctx context.Context, st *store.Store, c store.Case) (string, error) {
	evs, err := st.CaseEvidence(ctx, c.ID, evidenceShown)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The case's evidence, newest first (%d of %d):\n", len(evs), c.Evidence)
	for _, ev := range evs {
		if id, ok := strings.CutPrefix(ev.Ref, "ev:"); ok {
			e, found, err := st.EventByID(ctx, id)
			if err != nil {
				return "", err
			}
			if found {
				fmt.Fprintf(&b, "- %s ev:%s %s %s %s %s\n  %s\n", e.TS.Local().Format("2006-01-02 15:04:05"), e.ID(), e.Session, e.Actor, e.Kind,
					plain.Line(e.Tool), plain.Line(e.Summary))
				continue
			}
		}
		fmt.Fprintf(&b, "- %s %s %s\n  %s\n", ev.At.Local().Format("2006-01-02 15:04:05"), plain.Line(ev.Ref), plain.Line(ev.Kind), plain.Line(ev.Summary))
	}
	return b.String(), nil
}

// investigationFindings reads what an investigation found: its report, or, when the report names neither a mechanism
// nor a fix (a run cut short has none), the findings file it kept as it went. reported says which: only a report is
// the investigation's conclusion.
func investigationFindings(res Result, dir string) (f finding, reported, ok bool) {
	if f := nativeFinding(res); f.Mechanism != "" || f.Fix != "" {
		return f, true, true
	}
	raw, err := readRunFile(dir, "findings.md")
	if err != nil {
		return finding{}, false, false
	}
	f = findingsFile(string(raw))
	if f.Mechanism == "" && f.Fix == "" {
		return finding{}, false, false // alternatives alone are no finding, for the loop's case and the native arm alike
	}
	return f, false, true
}

// caseHolds is what a case holds from its earlier runs, for an investigation's prompt: the mechanism, the
// alternatives and the next test, which a build may have sent back with what is missing, and which the run's own
// report replaces. A new case holds none of it.
func caseHolds(c store.Case) string {
	if c.Mechanism == "" && c.NextTest == "" && len(c.Alternatives) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("What the case holds from its earlier runs (its record, to check, not to trust; your report replaces it):\n")
	if c.Mechanism != "" {
		fmt.Fprintf(&b, "- mechanism: %s\n", plain.Line(c.Mechanism))
	}
	for _, a := range c.Alternatives {
		fmt.Fprintf(&b, "- alternative: %s\n", plain.Line(a))
	}
	if c.NextTest != "" {
		fmt.Fprintf(&b, "- fix: %s\n", plain.Line(c.NextTest))
	}
	return b.String()
}

// recordFindings is the runner's bookkeeping of an investigation: its mechanism, alternatives and fix (the case's
// next test) go into the case it investigated. A report replaces all three; a findings file sets only the ones it
// holds, so a run cut short does not clear what the case held from an earlier run. A report that names both a
// mechanism and a fix moves the case to testing; a run cut short, whose findings file is all it left, leaves it
// investigating. A run that ended well with a report short of either did not decide (undecided, its summary): its
// findings go in and the case closes as insufficient evidence, which Cases' rule brings back once as much evidence
// again comes in. A case that went into another, or moved on while the run ran, keeps what it has; the bool says
// whether the findings were recorded.
func recordFindings(ctx context.Context, st *store.Store, caseID string, episode int64, f finding, reported bool, undecided, actor string) (store.Case, bool, error) {
	var err error
	if f, err = cleanFinding(ctx, f); err != nil {
		return store.Case{}, false, err
	}
	if undecided, err = Redact(ctx, undecided); err != nil {
		return store.Case{}, false, err
	}
	var alternatives []string
	for _, a := range f.Alternatives {
		if a = strings.TrimSpace(a); a != "" {
			alternatives = append(alternatives, a)
		}
	}
	mechanism, fix := strings.TrimSpace(f.Mechanism), strings.TrimSpace(f.Fix)
	u := store.CaseUpdate{Status: store.CaseInvestigating}
	if reported {
		u.Mechanism, u.NextTest, u.Alternatives = &mechanism, &fix, alternatives
		if alternatives == nil {
			u.Alternatives = []string{}
		}
		if mechanism != "" && fix != "" {
			u.Status = store.CaseTesting
		}
	} else {
		if mechanism != "" {
			u.Mechanism = &mechanism
		}
		if fix != "" {
			u.NextTest = &fix
		}
		u.Alternatives = alternatives // nil keeps the case's
	}
	if undecided != "" && u.Status != store.CaseTesting {
		return st.RecordUndecided(ctx, caseID, episode, u, undecided, actor)
	}
	return st.RecordFindings(ctx, caseID, episode, u, actor)
}

// cleanFinding cleans whole fields before the case store or judge clips them.
func cleanFinding(ctx context.Context, f finding) (finding, error) {
	f.Alternatives = append([]string(nil), f.Alternatives...)
	texts := []*string{&f.Mechanism, &f.Fix, &f.Report}
	for i := range f.Alternatives {
		texts = append(texts, &f.Alternatives[i])
	}
	for _, text := range texts {
		clean, err := Redact(ctx, *text)
		if err != nil {
			return finding{}, err
		}
		*text = clean
	}
	return f, nil
}

// undecided is why an investigation that ended well decided nothing, from its report: "" when it named both a
// mechanism and a fix, or did not end well (a run cut short, an error), whose case stays investigating.
func undecided(res Result, err error) string {
	if err != nil || res.Status != store.RunOK || len(res.Report) == 0 {
		return ""
	}
	f := nativeFinding(res)
	if strings.TrimSpace(f.Mechanism) != "" && strings.TrimSpace(f.Fix) != "" {
		return ""
	}
	why := "the investigation named no mechanism and fix"
	switch {
	case strings.TrimSpace(f.Mechanism) != "":
		why = "the investigation named a mechanism and no fix"
	case strings.TrimSpace(f.Fix) != "":
		why = "the investigation named a fix and no mechanism"
	}
	if s := strings.TrimSpace(f.Report); s != "" {
		why += ": " + s // clean the whole diagnostic before the store clips it
	}
	return why
}
