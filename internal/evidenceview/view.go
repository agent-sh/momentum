// Package evidenceview shares argument parsing and output for host and run evidence.
package evidenceview

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	field("next test", c.NextTest)
	field("benefit", c.Benefit)
	field("cost", fmt.Sprintf("$%.2f, %d tokens, %s over %d runs", c.Cost.USD, c.Cost.Tokens, c.Cost.Duration.Round(time.Second), c.Cost.Runs))
	field("signatures", strings.Join(c.Signatures, " | "))
	if !c.Back.IsZero() {
		field("came back", c.Back.Local().Format("2006-01-02 15:04"))
	}
	if !c.Deployed.IsZero() {
		field("deployed", c.Deployed.Local().Format("2006-01-02 15:04")+" "+c.Package)
	}
	if m := c.Measurement; m.Window > 0 || m.Effect != "" {
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
	case c.Status != store.CaseClosed:
		return string(c.Status)
	case c.Reason == store.Duplicate:
		return "closed: duplicate of " + c.Into
	}
	return "closed: " + string(c.Reason)
}
