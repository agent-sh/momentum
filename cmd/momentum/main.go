// momentum keeps improving an agent system. This first slice is the evidence layer: it ingests the signal
// stores that already exist into one local event store and lets an agent query them.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/config"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/source"
	"github.com/agent-sh/momentum/internal/store"
)

var version = "0.0.0-dev"

const usage = `usage:
  momentum evidence ingest              read what is new in every configured source into the store
  momentum evidence events [--since DUR] [--kind K,K] [--source S] [--limit N] [--json]   events, newest first
  momentum evidence status [--since DUR]   events per source and kind, and unparsed records
  momentum evidence rebuild SOURCE      drop what SOURCE contributed; the next ingest reads it from the start
  momentum version
Sources are configured in $MOMENTUM_CONFIG/sources.json (default ~/.config/momentum/sources.json).
The store is $MOMENTUM_HOME/momentum.db (default ~/.local/share/momentum).
DUR is a Go duration or a number of days, e.g. 36h or 7d. Exit: 0 done, 2 usage or configuration error,
3 error (for ingest: at least one source failed; the others still ran).`

type usageError struct{ msg string }

func (u usageError) Error() string { return u.msg }

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "evidence":
		err = evidence(ctx, args[1:], stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, "momentum", version)
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, usage)
	default:
		err = usageError{fmt.Sprintf("unknown command %q", args[0])}
	}
	var ue usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ue), errors.Is(err, config.ErrNoSources):
		fmt.Fprintln(stderr, "momentum:", err)
		return 2
	default:
		fmt.Fprintln(stderr, "[ERROR] momentum:", err)
		return 3
	}
}

func evidence(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageError{"evidence needs ingest, events, status or rebuild"}
	}
	switch args[0] {
	case "ingest":
		return ingest(ctx, stdout, stderr)
	case "events":
		return events(ctx, args[1:], stdout)
	case "status":
		return status(ctx, args[1:], stdout)
	case "rebuild":
		if len(args) != 2 {
			return usageError{"rebuild needs one source name"}
		}
		return rebuild(ctx, args[1], stdout)
	}
	return usageError{fmt.Sprintf("unknown evidence command %q", args[0])}
}

// configured builds the sources this machine has. An empty location is left out.
func configured() ([]source.Source, error) {
	c, err := config.Load()
	if err != nil {
		return nil, err
	}
	var out []source.Source
	if c.Mistakes.Path != "" {
		out = append(out, source.Mistakes{Path: c.Mistakes.Path})
	}
	if c.Radar.Path != "" {
		out = append(out, source.Radar{Path: c.Radar.Path, TestDevice: c.Radar.TestDevice})
	}
	if c.Incidents.Dir != "" {
		out = append(out, source.Incidents{Dir: c.Incidents.Dir})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s lists none", config.ErrNoSources, config.SourcesPath())
	}
	return out, nil
}

func ingest(ctx context.Context, stdout, stderr io.Writer) error {
	sources, err := configured()
	if err != nil {
		return err
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	failed := 0
	for _, o := range source.RunAll(ctx, st, sources) {
		if o.Err != nil {
			failed++
			fmt.Fprintf(stderr, "[ERROR] %s: %v\n", o.Name, o.Err)
			continue
		}
		fmt.Fprintf(stdout, "%s: %d new, %d read, %d unparsed\n", o.Name, o.New, o.Read, o.Skipped)
		for _, n := range o.Notes {
			fmt.Fprintf(stdout, "  %s\n", n)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d sources failed", failed, len(sources))
	}
	return nil
}

func events(ctx context.Context, args []string, stdout io.Writer) error {
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
	enc := json.NewEncoder(stdout)
	for _, e := range evs {
		if *asJSON {
			if err := enc.Encode(e); err != nil {
				return err
			}
			continue
		}
		tag := ""
		if e.Experiment != "" {
			tag = " [experiment " + e.Experiment + "]"
		}
		fmt.Fprintf(stdout, "%s  %-15s %-8s %-16s %s%s\n", e.TS.Format("01-02 15:04"), e.Source, e.Actor, e.Kind, e.Summary, tag)
	}
	return nil
}

func status(ctx context.Context, args []string, stdout io.Writer) error {
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
		skipped, err := st.Skipped(ctx, s)
		if err != nil {
			return err
		}
		line := fmt.Sprintf("%-15s %s", s, strings.Join(parts, ", "))
		if skipped > 0 {
			line += fmt.Sprintf("  (%d unparsed)", skipped)
		}
		fmt.Fprintln(stdout, line)
	}
	return nil
}

func rebuild(ctx context.Context, name string, stdout io.Writer) error {
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Rebuild(ctx, name); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s: dropped; the next ingest reads it from the start\n", name)
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
