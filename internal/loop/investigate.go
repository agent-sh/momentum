package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
)

// An investigation is the native path's: the harness's agent with the native brief, the owner's standing
// constraints and momentum's evidence commands, asked for the root cause and the fix, with the case's evidence in
// its prompt. It has no procedure and no family skill, and it keeps no case: the runner writes its report into the
// case when it ends. The golden-set evaluation found the procedure, the skill and the agent's own case bookkeeping
// cost more per root cause reached than the native path and reached no more (SPEC, One loop with skills).

// nativeBrief is the system prompt's brief of an investigation, the same in both arms of the evaluation.
const nativeBrief = `You find the root cause of a problem in an agent system on one machine, from what the machine recorded.
Your file tools reach your working directory and its repository checkouts. Read the records through momentum's evidence commands.`

// investigationSchema is an investigation's report.
const investigationSchema = `{"type":"object","additionalProperties":false,"required":["mechanism","fix","summary"],
"properties":{"mechanism":{"type":"string"},"alternatives":{"type":"array","items":{"type":"string"}},
"fix":{"type":"string"},"summary":{"type":"string"}}}`

// findingsInstruction keeps what a run cut short found: the runner reads the file when the run has no report.
const findingsInstruction = "Keep findings as you go in findings.md in your directory. Use the headings ## Mechanism, ## Alternatives ruled out, ## Fix, and ## Summary. End with your report of the same fields."

// liveCommands is a live investigation's command reference: what it reads the machine with.
const liveCommands = `

## Commands available in this run
Your working directory is your run's directory. File tools read its files and the directories the loop reads.
- momentum evidence show ID [ID...] [--bytes N]: reads up to ten stored events, each with the full transcript or log record it names; --bytes N prints each record's first N bytes.
- momentum evidence search TEXT [--source SOURCE] [--since WHEN] [--until WHEN] [--kind K,K] [--actor ACTOR] [--session ID] [--limit N]: matches stored summaries of every source, newest first; --source narrows it and must name one the store holds. Actors: owner, agent, tool, harness, detector, unknown. A phrase that starts with a dash goes after --.
- momentum evidence events [--source SOURCE] [--kind K,K] [--since WHEN] [--until WHEN] [--harness NAME] [--actor ACTOR] [--session ID] [--limit N] [--json]: reads stored events, optionally one session; each line names its event's ev:ID.
- momentum evidence status [--since WHEN]: counts stored events by source and kind.
- momentum evidence git DIR CMD ...: reads a repository's history with log, show, diff, blame, grep, ls-files, ls-tree, rev-parse, rev-list, merge-base, describe, status, shortlog, or listing forms of branch and tag. Use read options only (git's -N for a count; grep's first word is its pattern); revisions are checked and paths follow --. A grep with no match exits 1.
- momentum evidence file PATH [--tail N]: reads a file of the machine, scrubbed; not where credentials are kept.
- momentum evidence unit NAME [--lines N]: reads a user service's state and the end of its log.
- momentum supervise replay [--since WHEN] [--until WHEN] [--anchor]: runs supervision's rules over the store's history.
- momentum report: what needs the owner, what was decided and shipped, and what runs now.
- tacit search|fact|tree ...: the owner's rules, decisions and past mistakes.
No other shell commands are available: the harness refuses any other program, and a pipe, a loop, a substitution or a redirection.
`

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

// investigationFindings reads what an investigation found: its report, or, when it has none (a run cut short), the
// findings file it kept as it went.
func investigationFindings(res Result, dir string) (finding, bool) {
	if f := nativeFinding(res); f.Mechanism != "" || f.Fix != "" {
		return f, true
	}
	raw, err := os.ReadFile(filepath.Join(dir, "findings.md"))
	if err != nil {
		return finding{}, false
	}
	f := findingsFile(string(raw))
	return f, f.Mechanism != "" || f.Fix != ""
}

// recordFindings is the runner's bookkeeping of an investigation: its mechanism, alternatives and fix (the case's
// next test) go into the case it investigated, which moves to testing when it names both a mechanism and a fix and
// to investigating otherwise. A case that went into another, or moved on while the run ran, keeps what it has; the
// bool says whether the findings were recorded.
func recordFindings(ctx context.Context, st *store.Store, caseID string, f finding, actor string) (store.Case, bool, error) {
	var alternatives []string
	for _, a := range f.Alternatives {
		if a = strings.TrimSpace(a); a != "" {
			alternatives = append(alternatives, a)
		}
	}
	mechanism, fix := strings.TrimSpace(f.Mechanism), strings.TrimSpace(f.Fix)
	u := store.CaseUpdate{Status: store.CaseInvestigating, Mechanism: &mechanism, NextTest: &fix, Alternatives: alternatives}
	if alternatives == nil {
		u.Alternatives = []string{}
	}
	if mechanism != "" && fix != "" {
		u.Status = store.CaseTesting
	}
	return st.RecordFindings(ctx, caseID, u, nil, actor)
}

// reportSummary is a report's summary, for the run's record.
func reportSummary(raw json.RawMessage) string {
	var rep struct{ Summary string }
	json.Unmarshal(raw, &rep)
	return rep.Summary
}
