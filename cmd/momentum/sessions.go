package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/config"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/claude"
	"github.com/agent-sh/momentum/sessions/codex"
	"github.com/agent-sh/momentum/sessions/hermes"
	"github.com/agent-sh/momentum/sessions/opencode"
	"github.com/agent-sh/momentum/sessions/pi"
	"github.com/agent-sh/momentum/sessions/state"
)

// sessionsCmd prints the live sessions and their phases. It reads only processes, terminals, transcripts and
// the harnesses' own state; no model takes part. Phase-entry times, the trackers' places in their transcripts and
// one event per phase change persist in the store between runs.
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
	srcs := sessionSources(env)
	defer func() {
		for _, s := range srcs {
			s.Close()
		}
	}()
	snapshot := func(now time.Time) (*sessions.World, error) { return sessions.Snapshot(ctx, env, now) }
	ss, now, scanErr, err := observe(ctx, st, store.BusyTimeout, snapshot, buildID(), srcs)
	if err != nil {
		return err
	}
	if err := printBoard(stdout, ss, now, *asJSON, *all); err != nil {
		return err
	}
	return scanErr
}

// sessionSources are the harnesses the board reads, each named by its tool as in store.Harnesses.
func sessionSources(env sessions.Env) []sessions.Source {
	return []sessions.Source{claude.New(env), codex.New(env), pi.New(env), opencode.New(env), hermes.New(env)}
}

// observe is one run's look at the sessions, from the lock to the save: runs on one store take turns (waiting at
// most wait for the one before), so each reads the time and the machine after the previous one saved. The lock
// is released before anything is printed. scanErr names what failed; err is a store or snapshot failure.
func observe(ctx context.Context, st *store.Store, wait time.Duration, snapshot func(time.Time) (*sessions.World, error), build string,
	srcs []sessions.Source) (ss []*sessions.Session, now time.Time, scanErr, err error) {
	unlock, err := st.LockSessions(wait)
	if err != nil {
		return nil, now, nil, err
	}
	defer unlock()
	now = time.Now()
	w, snapErr := snapshot(now)
	if w == nil {
		return nil, now, nil, snapErr
	}
	ss, scanErr, err = board(ctx, w, snapErr, st, build, srcs)
	return ss, now, scanErr, err
}

// board scans every source over one snapshot, resuming each tracker from the checkpoint the last run of this
// build saved, and stamps each session with when it entered its phase. It saves the run in one transaction: the
// stamps, one event per change of stretch, and the checkpoints of the trackers it used; it drops checkpoints no
// run used for state.Keep. A source that failed, or every source when the snapshot is incomplete (snapErr), keeps
// its stamps: its sessions are unknown on this run, not gone. scanErr names what failed; the rest still counts.
// err is a store failure.
func board(ctx context.Context, w *sessions.World, snapErr error, st *store.Store, build string, srcs []sessions.Source) (ss []*sessions.Session, scanErr, err error) {
	trackers := st.Trackers(build)
	ss, failed, scanErr, _ := scanAll(ctx, w, snapErr, trackers, srcs, time.Time{})
	var cps []sessions.Checkpoint
	for _, src := range srcs {
		if r, ok := src.(sessions.Resumable); ok {
			cps = append(cps, r.Checkpoints()...)
		}
	}
	err = st.SessionRun(ctx, func(r *store.Run) error {
		stamps, err := r.Stamps()
		if err != nil {
			return err
		}
		if _, err := r.Put(phaseEvents(stamps.Apply(ss, w.Now, failed...), ss, w.Now)); err != nil {
			return err
		}
		if err := r.SaveStamps(stamps); err != nil {
			return err
		}
		if err := r.SaveCheckpoints(trackers, cps, w.Now); err != nil {
			return err
		}
		return r.DropCheckpoints(w.Now.Add(-state.Keep))
	})
	if err != nil {
		return nil, nil, err
	}
	if scanErr == nil && snapErr == nil {
		// a session gone from this scan is written vanished up to state.Keep later, dated when it was last seen: the
		// scan has read the sessions only up to that much before its own time
		if err := st.MarkRead(ctx, store.Sessions, w.Now.Add(-state.Keep)); err != nil {
			return nil, nil, err
		}
	}
	sort.SliceStable(ss, func(i, j int) bool {
		if a, b := sessions.Order[ss[i].Phase], sessions.Order[ss[j].Phase]; a != b {
			return a < b
		}
		return ss[i].Since.Before(ss[j].Since)
	})
	return ss, scanErr, nil
}

// scanAll is the scan every look at the sessions makes, a sessions run's and a report's: each source over one
// snapshot, its trackers resuming from the checkpoints saved hands out. A source that fails, or every source when the
// snapshot is incomplete (snapErr), is named in failed, and what it read still counts. Without a deadline the sources
// run in turn. With one they run at once, each in its own goroutine, so one that hangs holds up no other; a source not
// done by the deadline counts as failed, its sessions are left out, and the scan's context is cancelled, which stops
// the harnesses' commands. cut holds the indexes of the sources the deadline left reading, which the caller must not
// close while the process lives. scanErr names what failed.
func scanAll(ctx context.Context, w *sessions.World, snapErr error, saved sessions.Saved, srcs []sessions.Source, deadline time.Time) (ss []*sessions.Session, failed []string, scanErr error, cut []int) {
	type result struct {
		found []*sessions.Session
		err   error
	}
	scan := func(ctx context.Context, src sessions.Source) result {
		if r, ok := src.(sessions.Resumable); ok {
			r.Resume(saved)
		}
		found, err := sessions.Scan(ctx, w, src)
		return result{found, err}
	}
	errs := []error{snapErr}
	take := func(src sessions.Source, r result) {
		ss = append(ss, r.found...)
		if r.err != nil || snapErr != nil {
			failed = append(failed, src.Tool())
		}
		errs = append(errs, r.err)
	}
	late := func(src sessions.Source, why string) {
		failed = append(failed, src.Tool())
		errs = append(errs, fmt.Errorf("%s: %s", src.Tool(), why))
	}
	if deadline.IsZero() {
		for _, src := range srcs {
			take(src, scan(ctx, src))
		}
		return ss, failed, errors.Join(errs...), nil
	}
	if !time.Now().Before(deadline) { // the snapshot took the scan's time
		for _, src := range srcs {
			late(src, "not scanned: the snapshot ran past the scan's deadline")
		}
		return ss, failed, errors.Join(errs...), nil
	}
	sctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	results := make([]chan result, len(srcs))
	for i, src := range srcs {
		results[i] = make(chan result, 1)
		go func() { results[i] <- scan(sctx, src) }()
	}
	for i, src := range srcs { // in the sources' order, whichever finishes first
		select {
		case r := <-results[i]:
			take(src, r)
			continue
		case <-sctx.Done():
		}
		select {
		case r := <-results[i]: // done by the deadline all the same
			take(src, r)
		default:
			late(src, "not done by the scan's deadline")
			cut = append(cut, i)
		}
	}
	return ss, failed, errors.Join(errs...), cut
}

// buildID names this build of momentum for its trackers' checkpoints, which are only valid for the code that
// wrote them: the module version and revision Go stamped, and the executable's size and time, so any rebuild is
// another build and starts with one full read.
func buildID() string {
	var parts []string
	if bi, ok := debug.ReadBuildInfo(); ok {
		parts = append(parts, bi.Main.Version)
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" || s.Key == "vcs.modified" {
				parts = append(parts, s.Value)
			}
		}
	}
	if exe, err := os.Executable(); err == nil {
		if fi, err := os.Stat(exe); err == nil {
			parts = append(parts, strconv.FormatInt(fi.Size(), 10), strconv.FormatInt(fi.ModTime().UnixNano(), 10))
		}
	}
	return strings.Join(parts, " ")
}

// Kinds of the events a sessions run writes.
const (
	kindAppeared = "appeared" // seen for the first time
	kindPhase    = "phase"    // a new stretch: another phase, or the same phase after the session moved between runs
	kindVanished = "vanished" // not seen for longer than state.Keep
)

// phaseMeta is the structure of a sessions event, its Meta.
type phaseMeta struct {
	From        sessions.Phase   `json:"from,omitempty"`    // the phase it left; none when it appeared
	To          sessions.Phase   `json:"to,omitempty"`      // the phase it entered; none when it vanished
	ForMS       int64            `json:"for_ms,omitempty"`  // time in the old phase, until the event's time
	SeenMS      int64            `json:"seen_ms,omitempty"` // time in the old phase until the last run that saw it: a lower bound
	Name        string           `json:"name,omitempty"`
	Auto        bool             `json:"auto,omitempty"` // an automated run (cron, one-shot)
	FromAskKind sessions.AskKind `json:"from_ask_kind,omitempty"`
	FromAsk     string           `json:"from_ask,omitempty"` // what it waited on, when it left needs_you
	AskKind     sessions.AskKind `json:"ask_kind,omitempty"`
	Ask         string           `json:"ask,omitempty"` // what it asks, when it entered needs_you
}

// phaseEvents turns a run's changes into events of source sessions: one per change, at the time the new stretch
// began (for a session gone, when a run last saw it), keyed by the session and the run.
func phaseEvents(changes []state.Change, ss []*sessions.Session, now time.Time) []store.Event {
	byKey := map[string]*sessions.Session{}
	for _, s := range ss {
		byKey[s.Key] = s
	}
	var out []store.Event
	for _, c := range changes {
		kind := kindPhase
		switch {
		case c.From == "":
			kind = kindAppeared
		case c.To == "":
			kind = kindVanished
		}
		m := phaseMeta{From: c.From, To: c.To, Name: c.Name, Auto: c.Auto, AskKind: c.ToAskKind, Ask: c.ToAsk}
		if c.From != "" {
			m.ForMS, m.SeenMS, m.FromAskKind, m.FromAsk = c.For.Milliseconds(), max(0, c.Seen.Sub(c.Since).Milliseconds()), c.FromAskKind, c.FromAsk
		}
		meta, _ := json.Marshal(m)
		// A session's tool is its harness, by the name every source gives it (store.Harnesses).
		e := store.Event{Source: store.Sessions, SourceKey: c.Key + "@" + strconv.FormatInt(now.UnixMilli(), 10), TS: c.At,
			Cwd: c.Cwd, Session: c.Key, Harness: c.Tool, Actor: store.ActorHarness, Kind: kind, Summary: oneLine(changeSummary(c), 240), Meta: string(meta)}
		if s := byKey[c.Key]; s != nil {
			e.Model, e.Cwd = s.Model, s.Cwd
			if s.Transcript != "" && s.Size >= 0 {
				e.Ref = s.Transcript + "@" + strconv.FormatInt(s.Size, 10)
			}
		}
		out = append(out, e)
	}
	return out
}

// changeSummary is one line for the event list: the session, its move and how long it was in the phase it left,
// and for needs_you what it asks or what it waited on.
func changeSummary(c state.Change) string {
	name := c.Name
	if name == "" {
		name = c.Key
	}
	var s string
	switch {
	case c.From == "":
		s = fmt.Sprintf("%s: appeared, %s", name, c.To)
	case c.To == "":
		s = fmt.Sprintf("%s: vanished after %s %s", name, span(c.For), c.From)
	default:
		s = fmt.Sprintf("%s: %s -> %s after %s", name, c.From, c.To, span(c.For))
	}
	switch {
	case c.To == sessions.NeedsYou:
		s += fmt.Sprintf("; asks (%s) %s", c.ToAskKind, c.ToAsk)
	case c.From == sessions.NeedsYou:
		s += fmt.Sprintf("; waited on (%s) %s", c.FromAskKind, c.FromAsk)
	}
	return s
}

// span is a short duration: 45s, 12m, 3h05m, 4d.
func span(d time.Duration) string {
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

// oneLine is text as momentum shows it: plain (package plain), so what agents write cannot act on a terminal or
// reorder what a view shows, and clipped to n characters.
func oneLine(s string, n int) string { return plain.Clip(plain.Line(s), n) }

// ago is a short age: 45s, 12m, 3h05m, 4d; "?" for an unknown time.
func ago(t, now time.Time) string {
	if t.IsZero() {
		return "?"
	}
	return span(now.Sub(t))
}
