// Package evidenceview shares argument parsing and output for host and run evidence.
package evidenceview

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agent-sh/momentum/internal/measure"
	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
)

type EventOptions struct {
	Query store.Query
	Limit int
	JSON  bool
}

func ParseEvents(args []string, stderr io.Writer) (EventOptions, error) {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	fs.SetOutput(stderr)
	since := fs.String("since", "1d", "how far back, or from when (one session: its start)")
	until := fs.String("until", "", "until when")
	kinds := fs.String("kind", "", "comma-separated kinds")
	src := fs.String("source", "", "one source")
	harness := fs.String("harness", "", "one harness's events, from every source: "+strings.Join(store.Harnesses, ", "))
	session := fs.String("session", "", "one session, by its id or its start; oldest first")
	limit := fs.Int("limit", 50, "most events to print (0: 200)")
	asJSON := fs.Bool("json", false, "one JSON object per line")
	if err := fs.Parse(args); err != nil {
		return EventOptions{}, err
	}
	if fs.NArg() > 0 {
		return EventOptions{}, fmt.Errorf("evidence events takes no arguments, got %q (a time with a space goes in quotes)", fs.Arg(0))
	}
	if *limit <= 0 {
		*limit = 200
	}
	sinceSet := false
	fs.Visit(func(f *flag.Flag) { sinceSet = sinceSet || f.Name == "since" })
	var from time.Time // one session reads from its start, unless --since says otherwise
	if *session == "" || sinceSet {
		var err error
		if from, err = ParseWhen(*since); err != nil {
			return EventOptions{}, err
		}
	}
	if *harness != "" && !slices.Contains(store.Harnesses, *harness) {
		return EventOptions{}, fmt.Errorf("unknown harness %q: one of %s", *harness, strings.Join(store.Harnesses, ", "))
	}
	var err error
	q := store.Query{Since: from, Source: *src, Harness: *harness, Session: *session, Limit: *limit + 1}
	if *until != "" {
		if q.Until, err = ParseWhen(*until); err != nil {
			return EventOptions{}, err
		}
	}
	if *kinds != "" {
		q.Kinds = strings.Split(*kinds, ",")
	}
	return EventOptions{Query: q, Limit: *limit, JSON: *asJSON}, nil
}

func Events(stdout io.Writer, evs []store.Event, asJSON bool) error {
	enc := json.NewEncoder(stdout)
	for _, e := range evs {
		if asJSON {
			if err := enc.Encode(e); err != nil {
				return err
			}
			continue
		}
		tag := ""
		if e.Experiment != "" {
			tag = " [experiment " + e.Experiment + "]"
		}
		what := e.Kind
		if e.Tool != "" {
			what += " " + e.Tool
		}
		if strings.Contains(e.Meta, `"is_error":true`) {
			what += " (error)"
		}
		if e.Agent != "" {
			tag += " [subagent " + e.Agent + "]"
		}
		// Plain text: a summary holds what agents and tools wrote, which must not act on the terminal.
		fmt.Fprintf(stdout, "%s  %-15s %-8s %-16s %s%s\n", e.TS.Local().Format("01-02 15:04:05"), e.Source, e.Actor, plain.Line(what),
			plain.Line(e.Summary), plain.Line(tag))
	}
	return nil
}

// Now is the instant a relative time counts back from. The read-only binary of a run sets it to the end of the
// store it reads, which for an as-of copy is the task's end, not the machine's clock.
var Now = time.Now

func ParseWhen(s string) (time.Time, error) {
	if d, err := ParseSince(s); err == nil {
		return Now().Add(-d), nil
	}
	// a time with its offset keeps it: git's iso date puts it after a space
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05 -0700", "2006-01-02 15:04 -0700", "2006-01-02 15:04:05 -07:00", "2006-01-02 15:04:05Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	// agents write a local time with a T or with a space, as momentum and many logs print it
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf(`bad time %q: a duration (36h, 7d), a local time (2006-01-02T15:04 or "2006-01-02 15:04") or one with its offset (2006-01-02T15:04:05+03:00 or "2006-01-02 15:04:05 +0300")`, s)
}

func ParseSince(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || n < 0 {
			return 0, fmt.Errorf("bad --since %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("bad --since %q", s)
	}
	return d, nil
}

func EventHeader(e store.Event) string {
	return EventHeaderID(e, e.ID())
}

// EventHeaderID keeps a copy's stored id when its source key was scrubbed.
func EventHeaderID(e store.Event, id string) string {
	return fmt.Sprintf("%s ev:%s %s %s %s %s %s\n  %s\n", e.TS.Local().Format("2006-01-02 15:04:05"), id, e.Source, e.Actor, e.Kind, e.Session, plain.Line(e.Tool), plain.Line(e.Summary))
}

func RecordPrefix(clean string, n int) string {
	if n <= 0 || len(clean) <= n {
		return clean
	}
	whole := len(clean)
	clean = clean[:n]
	for !utf8.ValidString(clean) && len(clean) > 0 {
		clean = clean[:len(clean)-1]
	}
	return clean + fmt.Sprintf("\n  (its first %d of %d bytes)\n", len(clean), whole)
}

func Status(w io.Writer, counts map[string]map[string]int, skipped map[string]int) error {
	var srcs []string
	for s := range counts {
		srcs = append(srcs, s)
	}
	sort.Strings(srcs)
	for _, s := range srcs {
		var parts []string
		for k, n := range counts[s] {
			parts = append(parts, fmt.Sprintf("%s %d", k, n))
		}
		sort.Strings(parts)
		line := fmt.Sprintf("%-15s %s", s, strings.Join(parts, ", "))
		if skipped[s] > 0 {
			line += fmt.Sprintf("  (%d unparsed)", skipped[s])
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

func Case(stdout io.Writer, c store.Case, evs []store.Evidence, log []store.CaseEntry, logged int) error {
	fmt.Fprintf(stdout, "%s  %s  [%s, %s]\n", c.ID, c.Title, c.Family, StatusLine(c))
	field := func(name, v string) {
		if v != "" {
			fmt.Fprintf(stdout, "  %-13s %s\n", name+":", v)
		}
	}
	field("mechanism", c.Mechanism)
	for i, a := range c.Alternatives {
		field(fmt.Sprintf("alternative %d", i+1), a)
	}
	field("fix", c.NextTest) // the investigation's proposed fix, in prose: no test command
	field("benefit", c.Benefit)
	field("cost", fmt.Sprintf("$%.2f, %d tokens, %s over %d runs", c.Cost.USD, c.Cost.Tokens, c.Cost.Duration.Round(time.Second), c.Cost.Runs))
	field("signatures", strings.Join(c.Signatures, " | "))
	if !c.Back.IsZero() {
		field("came back", c.Back.Local().Format("2006-01-02 15:04"))
	}
	if !c.Deployed.IsZero() {
		field("deployed", c.Deployed.Local().Format("2006-01-02 15:04")+" "+c.Package)
	}
	if m := c.Measurement; Guarded(c) {
		// a guard measures no rate: it watches for a problem that had stopped to come back
		text := fmt.Sprintf("watching %dd for a recurrence, after %d incidents in %d units of exposure before the change; a problem that stopped by itself shows the same",
			int(m.Window.Hours()/24), m.Before, m.Exposure)
		if c.Status == store.CaseDeployed {
			incidents, exposure := planGuard(c)
			text = fmt.Sprintf("its watch starts once its change is in effect: 30 days of watching for a recurrence, after %d incidents in %d units of exposure before it, as proposed; a problem that stopped by itself shows the same",
				incidents, exposure)
		}
		if c.Status == store.CaseMeasuring && m.After > 0 {
			text = fmt.Sprintf("recurred: %d units of its problem came back after the change; its rollback is held; %d incidents in %d units of exposure before it",
				m.After, m.Before, m.Exposure)
		}
		if c.Status == store.CaseClosed && c.Reason == store.Guarded {
			text = fmt.Sprintf("no recurrence in %d units of exposure watched, after %d incidents in %d before the change; %s; a problem that stopped by itself shows the same: %s",
				m.Watched, m.Before, m.Exposure, guardChance(m, "%.1f"), m.Effect)
			if m.Watched == 0 {
				text = fmt.Sprintf("%s, after %d incidents in %d units of exposure before the change: %s", guardChance(m, "%.1f"), m.Before, m.Exposure, m.Effect)
			}
		}
		field("guarded", text)
	} else if m.Window > 0 || m.Effect != "" {
		field("measured", fmt.Sprintf("%d before, %d after over %s each: %s", m.Before, m.After, m.Window, m.Effect))
	}
	fmt.Fprintf(stdout, "  evidence: %d, newest first\n", c.Evidence)
	for _, e := range evs {
		fmt.Fprintf(stdout, "    %s  %-14s %s  %s\n", e.At.Local().Format("01-02 15:04"), e.Kind, e.Ref, plain.Clip(e.Summary, 120))
	}
	fmt.Fprintf(stdout, "  log: %d lines, the last %d\n", logged, len(log))
	for _, e := range log {
		fmt.Fprintf(stdout, "    %s  %-10s %-12s %s\n", e.At.Local().Format("01-02 15:04"), e.Actor, e.Action, e.Detail)
	}
	return nil
}

func StatusLine(c store.Case) string {
	switch {
	case c.Status == store.CaseMeasuring && Guarded(c) && c.Measurement.After > 0:
		return "measuring (guarded: recurred, rollback held)"
	case c.Status == store.CaseMeasuring && Guarded(c):
		return "measuring (guarded)"
	case c.Status != store.CaseClosed:
		return string(c.Status)
	case c.Reason == store.Duplicate:
		return "closed: duplicate of " + c.Into
	case c.Reason == store.Guarded:
		if c.Measurement.Watched == 0 {
			return "closed: guarded, " + guardChance(c.Measurement, "%.0f")
		}
		return fmt.Sprintf("closed: guarded, %d watched, %s; or it stopped by itself", c.Measurement.Watched, guardChance(c.Measurement, "%.0f"))
	}
	return "closed: " + string(c.Reason)
}

// guardChance is a guarded close's P in words, with its precision: the probability that the problem, had it kept
// happening, would have recurred in its watch, or that its history gives no rate.
func guardChance(m store.Measurement, precision string) string {
	if m.Watched == 0 {
		return measure.GuardNotWatched(m.Effect)
	}
	if m.Before == 0 {
		return "no rate to compare: the history holds no incident after its first"
	}
	return fmt.Sprintf("P="+precision+"%% had it kept happening", 100*m.Recur)
}

// Guarded says a case's measurement is a guard's watch: a case deployed, measuring or closed as guarded whose
// package's plan has a guard, when the package is there to read (a column an older build left behind says nothing
// then), else whose guard columns say so.
func Guarded(c store.Case) bool {
	if c.Status != store.CaseDeployed && c.Status != store.CaseMeasuring && c.Reason != store.Guarded {
		return false
	}
	if g, ok := packageGuard(c); ok {
		return g != nil
	}
	return c.Measurement.Guarded()
}

// guardOfPlan is what a view reads of a package's guard.
type guardOfPlan struct {
	Incidents int `json:"incidents"`
	Exposure  int `json:"exposure"`
}

// packageGuard reads the guard of a case's package's plan: nil for a plan with none; ok is false when the package
// cannot be read.
func packageGuard(c store.Case) (*guardOfPlan, bool) {
	if c.Package == "" {
		return nil, false
	}
	raw, err := os.ReadFile(filepath.Join(c.Package, "package.json"))
	if err != nil {
		return nil, false
	}
	var p struct {
		Plan *struct {
			Guard *guardOfPlan `json:"guard"`
		} `json:"plan"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return nil, false
	}
	if p.Plan == nil {
		return nil, true
	}
	return p.Plan.Guard, true
}

// planGuard is the incidents and exposure its plan's guard was proposed with, for a case whose watch has not started.
func planGuard(c store.Case) (incidents, exposure int) {
	if g, ok := packageGuard(c); ok && g != nil {
		return g.Incidents, g.Exposure
	}
	return c.Measurement.Before, c.Measurement.Exposure
}
