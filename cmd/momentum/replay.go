package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/internal/supervise"
	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/hermes"
)

// replay runs the supervision rules over the store's history and prints how they would have done against the
// owner's labelled steps: what each would have covered, how much earlier, and what it would have done unasked.
func replay(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("supervise replay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	since := fs.String("since", "30d", "the window's start: a duration ago or a time")
	until := fs.String("until", "", "the window's end (default now)")
	labelsPath := fs.String("labels", "", "the owner's steps to measure against, one JSON object per line")
	pointsPath := fs.String("points", "", "write every replayed stop to this file, one JSON object per line")
	anchor := fs.Bool("anchor", false, "print, for each rule, how long its stops sat before what came next, instead of the replay")
	window := fs.Duration("owner-window", supervise.DefaultParams.OwnerWindow, "hold while the owner typed into the session within it")
	promiseWait := fs.Duration("promise-wait", supervise.DefaultParams.PromiseWait, "the promise rule's wait")
	reopenWait := fs.Duration("reopen-wait", supervise.DefaultParams.ReopenWait, "the reopened rule's wait")
	stalledWait := fs.Duration("stalled-wait", supervise.DefaultParams.StalledWait, "the stalled rule's wait")
	overdueSlack := fs.Duration("overdue-slack", supervise.DefaultParams.OverdueSlack, "the overdue rule's slack")
	cutoffWait := fs.Duration("cutoff-wait", supervise.DefaultParams.CutoffWait, "the cutoff rule's wait")
	firstTry := fs.Duration("first-try", supervise.DefaultParams.Backoff[0], "the transient rule's first wait")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	from, err := parseWhen(*since)
	if err != nil {
		return usageError{err.Error()}
	}
	if os.Getenv("MOMENTUM_LOOP_RUN") != "" {
		var refused string
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "points" || f.Name == "labels" {
				refused = f.Name
			}
		})
		if refused != "" {
			return usageError{"in a loop run, --" + refused + " is refused; read replay output"}
		}
	}
	to := time.Now()
	if *until != "" {
		if to, err = parseWhen(*until); err != nil {
			return usageError{err.Error()}
		}
	}
	p := supervise.DefaultParams
	p.OwnerWindow, p.PromiseWait, p.ReopenWait, p.CutoffWait, p.StalledWait = *window, *promiseWait, *reopenWait, *cutoffWait, *stalledWait
	p.OverdueSlack = *overdueSlack
	p.Backoff = append([]time.Duration{*firstTry}, p.Backoff[1:]...)
	if *anchor {
		p.OwnerWindow, p.PromiseWait, p.ReopenWait, p.CutoffWait, p.StalledWait, p.OverdueSlack, p.Backoff = 0, 0, 0, 0, 0, 0, []time.Duration{0}
	}
	var labels []supervise.Label
	if *labelsPath != "" {
		f, err := os.Open(*labelsPath)
		if err != nil {
			return err
		}
		labels, err = supervise.ReadLabels(f)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", *labelsPath, err)
		}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	points, err := supervise.History(ctx, st, from, to)
	if err != nil {
		return err
	}
	// The store does not keep the platform a Hermes conversation runs on; Hermes's own database does.
	platforms, err := hermes.Platforms(ctx, sessions.EnvFromOS().HermesHome)
	if err != nil {
		return err
	}
	for i := range points {
		if points[i].Stop.Tool == "hermes" {
			points[i].Stop.Where = platforms[points[i].Session]
		}
	}
	probe, err := supervise.StoreProbe(ctx, st, from, to)
	if err != nil {
		return err
	}
	rs := supervise.Replay(points, labels, p, probe, to)
	if *pointsPath != "" {
		if err := writePoints(*pointsPath, rs); err != nil {
			return err
		}
	}
	if *anchor {
		printAnchors(stdout, rs)
		return nil
	}
	printReplay(stdout, rs, labels, from, to)
	return nil
}

func writePoints(path string, rs []supervise.Result) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, r := range rs {
		v := map[string]any{"session": r.Point.Session, "tool": r.Point.Stop.Tool, "stop_at": r.Point.Stop.At, "phase": r.Point.Stop.Phase,
			"error": r.Point.Stop.Error, "words": r.Point.Stop.Words, "tasks": r.Point.Stop.Tasks, "owner_at": r.Point.Stop.OwnerAt,
			"resumed_at": r.Point.Stop.ResumedAt, "request_at": r.Point.Stop.RequestAt, "waiting": r.Point.Waiting,
			"wake_due": r.Point.Stop.WakeDue, "wake_for": r.Point.Stop.WakeFor, "wake_key": r.Point.Stop.WakeKey,
			"next": r.Point.Next, "entry": r.Point.Entry, "rule": r.Decision.Rule,
			"verdict": r.Decision.Verdict, "at": r.Decision.At, "why": r.Decision.Why, "fired": r.Fired, "outcome": r.Outcome}
		if r.Label != nil {
			v["label"] = r.Label.Kind
		}
		if r.Outcome == "" && r.Label == nil && r.Decision.Rule == "" {
			continue
		}
		if err := enc.Encode(v); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

func minutes(d time.Duration) string { return fmt.Sprintf("%.0f", d.Minutes()) }

func printReplay(out io.Writer, rs []supervise.Result, labels []supervise.Label, from, to time.Time) {
	waiting := 0
	for _, r := range rs {
		if r.Point.Waiting {
			waiting++
		}
	}
	fmt.Fprintf(out, "window %s to %s, %d stops and %d long waits for a reply\n\n", from.Local().Format("2006-01-02 15:04"),
		to.Local().Format("2006-01-02 15:04"), len(rs)-waiting, waiting)
	fmt.Fprintf(out, "%-10s %5s %8s %13s %8s %6s %5s %5s %5s %5s %7s %8s\n", "rule", "acts", "covered", "min earlier", "owner1st", "false",
		"self", "other", "left", "bare", "false%", "w/o bare")
	for _, s := range supervise.Summarize(rs) {
		earlier := "-"
		if len(s.Earlier) > 0 {
			var sum time.Duration
			for _, e := range s.Earlier {
				sum += e
			}
			earlier = fmt.Sprintf("%s/%s", minutes(supervise.Quantile(s.Earlier, 0.5)), minutes(sum))
		}
		rate, broad := "-", "-"
		if s.Acts() > 0 {
			rate = fmt.Sprintf("%.0f%%", 100*float64(s.FalseActions())/float64(s.Acts()))
			broad = fmt.Sprintf("%.0f%%", 100*float64(s.FalseActions()-s.Bare)/float64(s.Acts()))
		}
		fmt.Fprintf(out, "%-10s %5d %8d %13s %8d %6d %5d %5d %5d %5d %7s %8s\n", s.Rule, s.Acts(), s.Covered, earlier, s.OwnerFirst,
			s.FalseActions(), s.Self, s.Other, s.Left, s.Bare, rate, broad)
	}
	fmt.Fprintln(out, "(min earlier: median/total minutes before the owner's step; bare: the owner came back with a nudge the labels")
	fmt.Fprintln(out, " do not hold; false% counts it as false, w/o bare does not)")
	printRedundant(out, rs)
	if len(labels) == 0 {
		return
	}
	matched := map[*supervise.Label]supervise.Result{}
	for _, r := range rs {
		if r.Label != nil {
			matched[r.Label] = r
		}
	}
	for _, kind := range []string{"nudge", "restart"} {
		var n, gone int
		by := map[string]int{}
		for i := range labels {
			l := &labels[i]
			if l.Kind != kind || l.At.Before(from) || !l.At.Before(to) {
				continue
			}
			n++
			r, ok := matched[l]
			if !ok {
				gone++
				continue
			}
			switch r.Outcome {
			case supervise.Covered:
				by["covered by "+r.Decision.Rule]++
			case supervise.OwnerFirst:
				by["owner first, "+r.Decision.Rule]++
			default:
				by["missed: "+supervise.Why(r)]++
			}
		}
		fmt.Fprintf(out, "\n%s steps: %d (%d not in the store)\n", kind, n, gone)
		var keys []string
		for k := range by {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			return by[keys[i]] > by[keys[j]] || by[keys[i]] == by[keys[j]] && keys[i] < keys[j]
		})
		for _, k := range keys {
			fmt.Fprintf(out, "  %4d  %s\n", by[k], k)
		}
	}
}

// printRedundant prints how often the owner's own labelled nudges went to a session that would have gone on by
// itself: a tool call was running, background work was in flight, or a wakeup or a loop was due, unless its turn
// waited on a request nothing answered, which none of those moves. It is the false rate of the owner's own nudges,
// the bar a rule's false rate is held to.
func printRedundant(out io.Writer, rs []supervise.Result) {
	var n, by int
	for _, r := range rs {
		if r.Label == nil || r.Label.Kind != "nudge" {
			continue
		}
		n++
		s := r.Point.Stop
		if s.RequestAt.IsZero() && (s.Busy || s.Tasks > 0 || !s.WakeAt.IsZero() || s.Loop != "") {
			by++
		}
	}
	if n > 0 {
		fmt.Fprintf(out, "\nthe owner's nudges to a session that would have gone on by itself: %d of %d (%.0f%%)\n", by, n, 100*float64(by)/float64(n))
	}
}

// printAnchors prints, for each rule, how long its stops sat from the time the rule counts from to what came
// next, by what that was: the data its wait is chosen from.
func printAnchors(out io.Writer, rs []supervise.Result) {
	type key struct{ rule, next string }
	gaps := map[key][]time.Duration{}
	for _, r := range rs {
		if r.Decision.Rule == "" || r.Decision.At.IsZero() {
			continue
		}
		next := r.Point.Next.Kind
		if r.Label != nil {
			next = "owner " + r.Label.Kind
		} else if next == "owner" {
			next = "owner other"
		}
		var g time.Duration
		if next == "none" {
			g = -1
		} else {
			g = r.Point.Next.At.Sub(r.Decision.At)
		}
		gaps[key{r.Decision.Rule, next}] = append(gaps[key{r.Decision.Rule, next}], g)
	}
	fmt.Fprintf(out, "%-10s %-14s %5s %7s %7s %7s %7s %7s\n", "rule", "next", "n", "p10", "p25", "p50", "p75", "p90")
	for _, rule := range supervise.Rules {
		for _, next := range []string{"owner nudge", "owner restart", "owner other", "self", "none"} {
			g := gaps[key{rule, next}]
			if len(g) == 0 {
				continue
			}
			if next == "none" {
				fmt.Fprintf(out, "%-10s %-14s %5d\n", rule, next, len(g))
				continue
			}
			q := func(x float64) string { return fmt.Sprintf("%.1f", supervise.Quantile(g, x).Minutes()) }
			fmt.Fprintf(out, "%-10s %-14s %5d %7s %7s %7s %7s %7s\n", rule, next, len(g), q(.1), q(.25), q(.5), q(.75), q(.9))
		}
	}
	fmt.Fprintln(out, "(minutes from the rule's anchor to what came next)")
}
