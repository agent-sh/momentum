package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/config"
	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

// measureCmd runs the measure step once (loop.Runner.Measure): the loop runs it on each of its runs too. A dry
// one reads the store read-only and takes no lock.
func measureCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("measure", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dry := fs.Bool("dry", false, "say what each deployed, measuring or improved case's measurement shows, and change nothing")
	families := fs.String("family", "", "comma-separated families (default: every family)")
	only := fs.String("case", "", "this case only")
	asJSON := fs.Bool("json", false, "one JSON object per case")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Sprintf("measure takes no arguments, got %q", fs.Arg(0))}
	}
	if os.Getenv("MOMENTUM_LOOP_RUN") != "" {
		return usageError{"the measure step is the loop's, not one of its runs'"}
	}
	c, err := config.Load()
	if err != nil {
		return err
	}
	open := store.Open
	if *dry {
		open = store.OpenReadOnly
	}
	st, err := open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	if !*dry {
		unlock, err := lockLoop(paths.DB())
		if err != nil {
			return err
		}
		defer unlock()
	}
	r, _, err := setup(st, c.Loop)
	if err != nil {
		return err
	}
	r.Log = func(format string, a ...any) { fmt.Fprintf(stderr, format+"\n", a...) }
	o := loop.Options{Dry: *dry, Case: *only}
	for _, f := range strings.Split(*families, ",") {
		if f = strings.TrimSpace(f); f != "" {
			o.Families = append(o.Families, f)
		}
	}
	ms, err := r.Measure(ctx, o)
	enc := json.NewEncoder(stdout)
	for _, m := range ms {
		if *asJSON {
			if err := enc.Encode(m); err != nil {
				return err
			}
			continue
		}
		due := "-"
		if !m.Due.IsZero() {
			due = m.Due.Local().Format(time.DateTime)
		}
		fmt.Fprintf(stdout, "%s %-11s due %s  %s\n", m.Case, m.Status, due, m.Title)
		if m.Result != nil && m.Result.Effect != "" {
			fmt.Fprintf(stdout, "  %s\n", m.Result.Effect)
		}
		if m.Why != "" {
			fmt.Fprintf(stdout, "  %s\n", m.Why)
		}
	}
	return err
}
