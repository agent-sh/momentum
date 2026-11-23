package scorecard

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"
)

// Recommendation is one concrete change to tacit the model pass ranked, tied to the week's evidence.
type Recommendation struct {
	Key      string `json:"key"` // a short stable slug: the same change next week keeps it
	Title    string `json:"title"`
	Change   string `json:"change"`
	Evidence string `json:"evidence"`
	Expected string `json:"expected"` // the number it should move
	Case     string `json:"-"`        // the case it is tracked by
}

// Handled is an open recommendation's case the week's numbers show handled.
type Handled struct {
	Case string `json:"case"`
	Why  string `json:"why"`
}

// Advice is the model pass's answer, or why there is none.
type Advice struct {
	Recommendations []Recommendation `json:"recommendations"`
	Handled         []Handled        `json:"handled"`
	Dropped         []string         `json:"-"` // recommendations refused by the owner's constraints
	None            string           `json:"-"` // why no model pass ran or answered
	Model           string           `json:"-"`
	USD             float64          `json:"-"`
}

// Render is the week's report in Markdown.
func Render(o *Observation, sc Scored, a Advice) string {
	var b strings.Builder
	w := o.Week
	fmt.Fprintf(&b, "# momentum weekly %s\n\n", w.Name)
	fmt.Fprintf(&b, "%s to %s, local time. Computed from tacit's delivery log and store and momentum's store; the rules are fixed in internal/scorecard.\n\n",
		w.Start.Format("Mon 2006-01-02 15:04"), w.End.Format("Mon 2006-01-02 15:04"))

	b.WriteString("## Scorecard\n\n")
	for i, v := range sc.Verdicts {
		state := "does not deliver"
		if v.Delivers {
			state = "delivers"
		}
		fmt.Fprintf(&b, "- %s %s: %s.", v.System, state, v.Why)
		if i < len(sc.Standings) {
			s := sc.Standings[i]
			switch {
			case !s.On:
				fmt.Fprintf(&b, " It is off")
				if s.Shut != "" {
					fmt.Fprintf(&b, " (shut down in %s)", s.Shut)
				}
				b.WriteString(".")
			default:
				fmt.Fprintf(&b, " Failing scored weeks in a row: %d; weeks to shutdown: %d.", s.Failing, s.ToShutdown)
			}
		}
		b.WriteString("\n")
	}
	for _, s := range sc.Reenabled {
		fmt.Fprintf(&b, "- %s was turned back on after a shutdown: its scoring starts over with the next full week.\n", s)
	}
	for _, sd := range sc.Shutdowns {
		fmt.Fprintf(&b, "- SHUT DOWN %s: off-file %s; timers stopped: %s. Turn it back on: `%s`.", sd.System, sd.OffFile, list(sd.Units), sd.Restore)
		if len(sd.Failed) > 0 {
			fmt.Fprintf(&b, " Not done: %s.", strings.Join(sd.Failed, "; "))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nScoring counts from %s. tacit delivers when, among delivered lines agents marked in interactive sessions, used >= not relevant with at least one used, and the owner's restatements of facts tacit holds are not above the median of the %d weeks before. momentum delivers when at least one case closed as improved. %d failing scored weeks in a row shut a system down.\n\n",
		sc.Deployed.Format("2006-01-02 15:04"), BaselineWeeks, FailingWeeks)

	weeks := append([]TacitWeek{o.Tacit}, o.TacitBefore...)
	b.WriteString("## tacit\n\n")
	b.WriteString("| | " + strings.Join(mapWeeks(weeks, func(t TacitWeek) string {
		if t.Partial {
			return t.Week + " (log partial)"
		}
		return t.Week
	}), " | ") + " |\n|---|" + strings.Repeat("---|", len(weeks)) + "\n")
	row := func(name string, f func(TacitWeek) string) {
		fmt.Fprintf(&b, "| %s | %s |\n", name, strings.Join(mapWeeks(weeks, f), " | "))
	}
	for _, h := range DeliveryHooks {
		row("hook "+h+": calls, delivered", func(t TacitWeek) string { return count(t.ByHook[h]) })
	}
	for _, k := range Kinds {
		row(k+" sessions: calls, delivered", func(t TacitWeek) string { return count(t.ByKind[k]) })
	}
	row("lines delivered", func(t TacitWeek) string { return fmt.Sprint(t.Lines) })
	row("distinct facts delivered", func(t TacitWeek) string { return fmt.Sprint(t.Facts) })
	row("mark asks, answered", func(t TacitWeek) string { return fmt.Sprintf("%d, %d", t.Marks.Asked, t.Marks.Answered) })
	row("marked used, not relevant", func(t TacitWeek) string { return fmt.Sprintf("%d, %d", t.Marks.Used, t.Marks.NotRelevant) })
	row("interactive: asks, answered, used, not relevant", func(t TacitWeek) string {
		m := t.InteractiveMarks
		return fmt.Sprintf("%d, %d, %d, %d", m.Asked, m.Answered, m.Used, m.NotRelevant)
	})
	row("judge labels", func(t TacitWeek) string { return fmt.Sprint(t.Judge.Labeled) })
	row("judge relevant yes, no, unsure", func(t TacitWeek) string {
		return fmt.Sprintf("%d, %d, %d", t.Judge.Relevant["yes"], t.Judge.Relevant["no"], t.Judge.Relevant["unsure"])
	})
	row("judge relevant and new", func(t TacitWeek) string { return pct(t.Judge.RelevantNew, t.Judge.Labeled) })
	row("MCP calls (search, fact, tree, add, mark)", func(t TacitWeek) string {
		return fmt.Sprintf("%d, %d, %d, %d, %d", t.MCP["tacit_search"], t.MCP["tacit_fact"], t.MCP["tacit_tree"], t.MCP["tacit_add"], t.MCP["tacit_mark"])
	})
	row("MCP calls by kind (interactive, headless, subagent, unknown)", func(t TacitWeek) string {
		return fmt.Sprintf("%d, %d, %d, %d", t.MCPByKind[Interactive], t.MCPByKind[Headless], t.MCPByKind[Subagent], t.MCPByKind[Unknown])
	})
	row("nav steps logged (tree, search, fact)", func(t TacitWeek) string {
		return fmt.Sprintf("%d, %d, %d", t.Nav["tree"], t.Nav["search"], t.Nav["fact"])
	})
	row("owner restatements", func(t TacitWeek) string {
		if !t.OwnerCovered {
			return fmt.Sprintf("%d (store partial)", t.Restatements)
		}
		return fmt.Sprint(t.Restatements)
	})
	row("of them, fact delivered first", func(t TacitWeek) string { return fmt.Sprint(t.RestatedSeen) })
	b.WriteString("\n")
	if t := o.Tacit; len(t.Latency) > 0 || t.Subagent != nil {
		b.WriteString("Hook run times this week, as Claude Code's transcripts record them:\n\n")
		for _, h := range slices.Sorted(maps.Keys(t.Latency)) {
			l := t.Latency[h]
			fmt.Fprintf(&b, "- %s: %d runs, p50 %d ms, p90 %d ms\n", h, l.N, l.P50, l.P90)
		}
		if t.Subagent != nil {
			fmt.Fprintf(&b, "- inside subagents: %s\n", count(*t.Subagent))
		}
		b.WriteString("\n")
	}
	if len(o.Tacit.Restated) > 0 {
		b.WriteString("Restatements this week (the message, cut, and the fact it restates):\n\n")
		for _, r := range o.Tacit.Restated {
			seen := "not delivered in the session before"
			if r.Delivered {
				seen = "delivered in the session before"
			}
			fmt.Fprintf(&b, "- %s `%s` (cover %.2f, %s): %s\n", r.At.Local().Format("01-02 15:04"), r.Fact, r.Score, seen, clip(r.Text, 160))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Curated facts matched: %d. Delivery log lines unread: %d. Session kinds: interactive when the owner wrote in the session, headless when momentum's store holds it otherwise, unknown when it holds nothing of it.\n\n", o.Facts, o.Skipped)

	b.WriteString("## momentum\n\n")
	mweeks := append([]MomentumWeek{o.Momentum}, o.MomentumBefore...)
	b.WriteString("| | " + strings.Join(mapM(mweeks, func(m MomentumWeek) string { return m.Week }), " | ") + " |\n|---|" + strings.Repeat("---|", len(mweeks)) + "\n")
	mrow := func(name string, f func(MomentumWeek) string) {
		fmt.Fprintf(&b, "| %s | %s |\n", name, strings.Join(mapM(mweeks, f), " | "))
	}
	mrow("cases closed improved", func(m MomentumWeek) string { return fmt.Sprint(m.Improved) })
	mrow("changes landed", func(m MomentumWeek) string { return fmt.Sprint(m.Landed) })
	mrow("cases opened", func(m MomentumWeek) string { return fmt.Sprint(m.Opened) })
	mrow("cases closed", func(m MomentumWeek) string { return counts(m.Closed) })
	mrow("loop spend", func(m MomentumWeek) string { return fmt.Sprintf("$%.2f", m.SpendUSD) })
	mrow("loop runs", func(m MomentumWeek) string { return counts(m.Runs) })
	mrow("supervise messages sent", func(m MomentumWeek) string { return fmt.Sprint(m.SuperviseActs) })
	mrow("supervise outcomes", func(m MomentumWeek) string { return counts(m.Supervise) })
	b.WriteString("\n## What to change in tacit\n\n")
	switch {
	case a.None != "":
		fmt.Fprintf(&b, "No model pass: %s.\n", a.None)
	case len(a.Recommendations) == 0:
		b.WriteString("The model pass named no change.\n")
	}
	for i, r := range a.Recommendations {
		fmt.Fprintf(&b, "%d. **%s**", i+1, r.Title)
		if r.Case != "" {
			fmt.Fprintf(&b, " (case %s)", r.Case)
		}
		fmt.Fprintf(&b, "\n   - change: %s\n   - evidence: %s\n   - expected: %s\n", r.Change, r.Evidence, r.Expected)
	}
	for _, h := range a.Handled {
		fmt.Fprintf(&b, "- closed %s as handled: %s\n", h.Case, h.Why)
	}
	for _, d := range a.Dropped {
		fmt.Fprintf(&b, "- dropped (it would promote facts into an agent rules file): %s\n", d)
	}
	if a.Model != "" {
		fmt.Fprintf(&b, "\nModel: %s, $%.2f.\n", a.Model, a.USD)
	}
	return b.String()
}

func mapWeeks(ws []TacitWeek, f func(TacitWeek) string) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = f(w)
	}
	return out
}

func mapM(ws []MomentumWeek, f func(MomentumWeek) string) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = f(w)
	}
	return out
}

func count(c Count) string { return fmt.Sprintf("%d, %s", c.Calls, pct(c.Delivered, c.Calls)) }

func pct(n, of int) string {
	if of == 0 {
		return "-"
	}
	return fmt.Sprintf("%d (%.0f%%)", n, 100*float64(n)/float64(of))
}

func counts(m map[string]int) string {
	if len(m) == 0 {
		return "-"
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func list(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, ", ")
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.ReplaceAll(s, "|", "/")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "..."
}
