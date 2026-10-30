package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/config"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/claude"
	"github.com/agent-sh/momentum/sessions/codex"
	"github.com/agent-sh/momentum/sessions/hermes"
	"github.com/agent-sh/momentum/sessions/opencode"
	"github.com/agent-sh/momentum/sessions/pi"
)

// sessionsCmd prints the live sessions and their phases. It reads only processes, terminals, transcripts and
// the harnesses' own state; no model takes part. Phase-entry times persist in the store between runs.
func sessionsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "one JSON object per session")
	all := fs.Bool("all", false, "also list automated runs")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Sprintf("sessions takes no arguments, got %q", fs.Arg(0))}
	}
	env := sessions.EnvFromOS()
	switch c, err := config.Load(); {
	case err == nil:
		env.Projects = c.Sessions.Projects
	case !errors.Is(err, config.ErrNoSources): // the board needs no configuration file
		return err
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	now := time.Now()
	w, snapErr := sessions.Snapshot(ctx, env, now)
	if w == nil {
		return snapErr
	}
	srcs := []sessions.Source{claude.New(env), codex.New(env), pi.New(env), opencode.New(env), hermes.New(env)}
	defer func() {
		for _, s := range srcs {
			s.Close()
		}
	}()
	ss, scanErr, err := board(ctx, w, st, srcs)
	if err != nil {
		return err
	}
	if err := printBoard(stdout, ss, now, *asJSON, *all); err != nil {
		return err
	}
	return errors.Join(snapErr, scanErr)
}

// board scans every source over one snapshot and stamps each session with when it entered its phase, keeping
// the stamps in the store for the next run. scanErr names the sources that failed; the others still count. err is
// a store failure.
func board(ctx context.Context, w *sessions.World, st *store.Store, srcs []sessions.Source) (ss []*sessions.Session, scanErr, err error) {
	ss, scanErr = sessions.Scan(ctx, w, srcs...)
	stamps, err := st.SessionStamps(ctx)
	if err != nil {
		return nil, nil, err
	}
	stamps.Apply(ss, w.Now)
	if err := st.SaveSessionStamps(ctx, stamps); err != nil {
		return nil, nil, err
	}
	sort.SliceStable(ss, func(i, j int) bool {
		if a, b := sessions.Order[ss[i].Phase], sessions.Order[ss[j].Phase]; a != b {
			return a < b
		}
		return ss[i].Since.Before(ss[j].Since)
	})
	return ss, scanErr, nil
}

func printBoard(out io.Writer, ss []*sessions.Session, now time.Time, asJSON, all bool) error {
	auto := 0
	enc := json.NewEncoder(out)
	for _, s := range ss {
		if s.Auto && !all {
			auto++
			continue
		}
		if asJSON {
			if err := enc.Encode(s); err != nil {
				return err
			}
			continue
		}
		fmt.Fprintf(out, "%-10s %5s  %-8s %-38s %s\n", s.Phase, ago(s.Since, now), s.Tool, oneLine(s.Name, 38), oneLine(s.Project, 40))
		if s.Ask != "" {
			fmt.Fprintf(out, "%18s ask (%s): %s\n", "", s.AskKind, oneLine(s.Ask, 120))
		}
		if s.Now != "" {
			fmt.Fprintf(out, "%18s now: %s (%s)\n", "", oneLine(s.Now, 100), ago(s.NowSince, now))
		}
		for _, t := range s.Tasks {
			line := strings.TrimSpace(fmt.Sprintf("%s %s (%s) %s", t.Kind, oneLine(t.Label, 60), ago(t.Started, now), oneLine(t.Now, 50)))
			fmt.Fprintf(out, "%18s · %s\n", "", line)
		}
		if !s.WakeAt.IsZero() {
			fmt.Fprintf(out, "%18s wakes %s\n", "", s.WakeAt.Local().Format("15:04"))
		}
		if s.Loop != "" {
			fmt.Fprintf(out, "%18s loop: %s\n", "", oneLine(s.Loop, 80))
		}
	}
	if asJSON {
		return nil
	}
	foot := fmt.Sprintf("%d sessions", len(ss)-auto)
	if auto > 0 {
		foot += fmt.Sprintf(", %d automated runs not listed (--all lists them)", auto)
	}
	fmt.Fprintln(out, foot)
	return nil
}

// oneLine folds whitespace and clips to n runes.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// ago is a short age: 45s, 12m, 3h05m, 4d; "?" for an unknown time.
func ago(t, now time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
