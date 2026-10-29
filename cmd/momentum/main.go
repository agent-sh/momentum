// momentum keeps improving an agent system. This first slice is the evidence layer: it ingests the signal
// stores that already exist into one local event store and lets an agent query them.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/source"
	"github.com/agent-sh/momentum/internal/store"
)

var version = "0.0.0-dev"

const usage = `usage:
  momentum ingest [--mistakes FILE] [--radar DB] [--incidents DIR]   read what is new in each source into the store
  momentum events [--since DUR] [--kind K,K] [--source S] [--limit N] [--json]   events, newest first
  momentum status [--since DUR]   events per source and kind
  momentum version
DUR is a Go duration or a number of days, e.g. 36h or 7d. The store is ` + "`$MOMENTUM_HOME/momentum.db`" + `,
default ~/.local/share/momentum. Exit: 0 done, 2 usage error, 3 error.`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "ingest":
		err = ingest(ctx, os.Args[2:])
	case "events":
		err = events(ctx, os.Args[2:])
	case "status":
		err = status(ctx, os.Args[2:])
	case "version":
		fmt.Println("momentum", version)
	case "-h", "--help", "help":
		fmt.Println(usage)
	default:
		fmt.Fprintf(os.Stderr, "momentum: unknown command %q\n%s\n", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		if _, ok := err.(usageError); ok {
			fmt.Fprintln(os.Stderr, "momentum:", err)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "[ERROR] momentum:", err)
		os.Exit(3)
	}
}

type usageError struct{ msg string }

func (u usageError) Error() string { return u.msg }

func ingest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	mistakes := fs.String("mistakes", "~/.local/state/claude-mistakes/events.jsonl", "Claude Code mistakes log")
	radar := fs.String("radar", radarDefault(), "findings scout store")
	incidents := fs.String("incidents", "~/.hermes/avi-debugger/incidents", "incident detector directory")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	sources := []source.Source{
		source.Mistakes{Path: paths.Expand(*mistakes)},
		source.Radar{Path: paths.Expand(*radar)},
		source.Incidents{Dir: paths.Expand(*incidents)},
	}
	for _, s := range sources {
		n, err := s.Ingest(ctx, st)
		if err != nil {
			return fmt.Errorf("%s: %w", s.Name(), err)
		}
		fmt.Printf("%s: %d new\n", s.Name(), n)
	}
	return nil
}

func radarDefault() string {
	if h := os.Getenv("RADAR_HOME"); h != "" {
		return h + "/radar.db"
	}
	return "~/.hermes/radar/radar.db"
}

func events(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	since := fs.String("since", "1d", "how far back")
	kinds := fs.String("kind", "", "comma-separated kinds")
	src := fs.String("source", "", "one source")
	limit := fs.Int("limit", 50, "most events to print")
	asJSON := fs.Bool("json", false, "one JSON object per line")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	d, err := parseSince(*since)
	if err != nil {
		return usageError{err.Error()}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	q := store.Query{Since: time.Now().Add(-d), Source: *src, Limit: *limit}
	if *kinds != "" {
		q.Kinds = strings.Split(*kinds, ",")
	}
	evs, err := st.Events(ctx, q)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	for _, e := range evs {
		if *asJSON {
			if err := enc.Encode(e); err != nil {
				return err
			}
			continue
		}
		fmt.Printf("%s  %-9s %-8s %-10s %s\n", e.TS.Format("01-02 15:04"), e.Source, e.Actor, e.Kind, e.Summary)
	}
	return nil
}

func status(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	since := fs.String("since", "7d", "how far back")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	d, err := parseSince(*since)
	if err != nil {
		return usageError{err.Error()}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	counts, err := st.Counts(ctx, time.Now().Add(-d))
	if err != nil {
		return err
	}
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
		fmt.Printf("%-10s %s\n", s, strings.Join(parts, ", "))
	}
	return nil
}

// parseSince accepts a Go duration or a whole number of days ("7d").
func parseSince(s string) (time.Duration, error) {
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
