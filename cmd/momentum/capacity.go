package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/agent-sh/momentum/internal/capacity"
	"github.com/agent-sh/momentum/internal/config"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
)

// capacityJobs and capacityProc stand in for the machine's scheduled jobs and its /proc; nil and "" are the
// machine's own. Tests set them.
var (
	capacityJobs capacity.Jobs
	capacityProc string
)

// capacityCmd runs the capacity loop: the process sampler, each disk's use, and the rules that are due. A dry run
// changes and records nothing and lists what every rule would do.
func capacityCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("capacity", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dry := fs.Bool("dry-run", false, "delete and record nothing; list what every rule would do")
	now := fs.Bool("now", false, "every rule scans now, whatever its cadence")
	only := fs.String("rule", "", "only these rules, comma-separated: "+strings.Join(capacity.Rules, ", "))
	asJSON := fs.Bool("json", false, "the run as one JSON object")
	all := fs.Bool("all", false, "with the text output, also list what is kept and pending")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Sprintf("capacity takes no arguments, got %q", fs.Arg(0))}
	}
	cfg, err := capacity.Load()
	if errors.Is(err, capacity.ErrNoCapacity) || errors.Is(err, capacity.ErrBadConfig) {
		return usageError{err.Error()}
	}
	if err != nil {
		return err
	}
	opt := capacity.Options{DryRun: *dry, Now: *now, Jobs: capacityJobs, ProcRoot: capacityProc}
	for _, r := range strings.Split(*only, ",") {
		if r = strings.TrimSpace(r); r == "" {
			continue
		}
		if !contains(capacity.Rules, r) {
			return usageError{fmt.Sprintf("--rule names %q, which is not a rule (%s)", r, strings.Join(capacity.Rules, ", "))}
		}
		opt.Rules = append(opt.Rules, r)
	}
	var st *store.Store
	if *dry {
		// A dry run reads the store as it is. Without one this build can read, it reads as one with no history.
		if st, err = store.OpenReadOnly(paths.DB()); err != nil {
			fmt.Fprintf(stderr, "momentum: %v; the dry run has no history\n", err)
			st = nil
		}
	} else {
		unlock, err := capacityLock(5 * time.Minute)
		if err != nil {
			return err
		}
		defer unlock()
		if st, err = store.Open(paths.DB()); err != nil {
			return err
		}
	}
	if st != nil {
		defer st.Close()
		opt.Sessions = func(ctx context.Context) map[int]capacity.Session { return sessionsByPID(ctx, st) }
	}
	res, err := capacity.Run(ctx, cfg, st, opt)
	if err != nil {
		return err
	}
	if *asJSON {
		b, err := json.Marshal(res)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, string(b))
	} else {
		printCapacity(stdout, res, *all)
	}
	var failed []string
	for _, r := range res.Rules {
		if r.Err != "" {
			failed = append(failed, r.Rule+": "+r.Err)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("capacity: %s", strings.Join(failed, "; "))
	}
	return nil
}

// capacityLock makes capacity runs that record take turns: two at once could judge one item twice. It waits up to
// wait for the run before.
func capacityLock(wait time.Duration) (func(), error) {
	if err := os.MkdirAll(paths.Data(), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(paths.Data(), "momentum.db.capacity-lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for deadline := time.Now().Add(wait); ; time.Sleep(200 * time.Millisecond) {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("another capacity run has held the lock for %s", wait)
		}
	}
}

// sessionsByPID is each live agent session but the done ones, by its process (a session without one by a negative
// key), from the scan the report makes: it saves nothing.
func sessionsByPID(ctx context.Context, st *store.Store) map[int]capacity.Session {
	env := sessions.EnvFromOS()
	if c, err := config.Load(); err == nil {
		env.Projects = c.Sessions.Projects
	}
	srcs := sessionSources(env)
	defer func() {
		for _, s := range srcs {
			s.Close()
		}
	}()
	w, snapErr := sessions.Snapshot(ctx, env, time.Now())
	if w == nil {
		return nil
	}
	ss, _, _, err := look(ctx, w, snapErr, st, buildID(), srcs, time.Now().Add(3*time.Second))
	if err != nil {
		return nil
	}
	out := map[int]capacity.Session{}
	for i, s := range ss {
		if s.Phase == sessions.Done {
			continue
		}
		key := s.PID
		if key <= 0 { // a session without a process of its own still works somewhere
			key = -(i + 1)
		}
		out[key] = capacity.Session{Key: s.Key, Name: s.Name, Tool: s.Tool, Cwd: s.Cwd}
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// printCapacity prints a run for a terminal: the sampler, each disk, then each rule's findings. Kept and pending
// items show with all.
func printCapacity(w io.Writer, res *capacity.Result, all bool) {
	mode := ""
	if res.DryRun {
		mode = " (dry run)"
	}
	fmt.Fprintf(w, "momentum capacity %s%s\n", res.At.Format("2006-01-02 15:04"), mode)
	fmt.Fprintf(w, "sampler: %d processes in %s (%d could not be read); record since %s\n", res.Procs,
		res.SampleTook.Round(time.Millisecond), res.Unreadable, res.Since.Format("2006-01-02 15:04"))
	for _, d := range res.Disks {
		if d.Err != "" {
			fmt.Fprintf(w, "disk %s: %s\n", d.Disk, d.Err)
			continue
		}
		pct := 0.0
		if d.Used+d.Avail > 0 {
			pct = 100 * float64(d.Used) / float64(d.Used+d.Avail)
		}
		line := fmt.Sprintf("disk %s: %.0f%% used, %s free", d.Disk, pct, gibText(d.Avail))
		if d.Window > 0 {
			line += fmt.Sprintf(", %+.1f GiB/h over %s", d.Rate, d.Window.Round(time.Minute))
		}
		if d.Fast {
			line += " FILLING FAST"
		}
		fmt.Fprintln(w, line)
		for _, wr := range d.Writers {
			fmt.Fprintf(w, "  writer %s (pid %d) %.0f MiB/s in %s %s\n", wr.Comm, wr.PID, wr.Rate, wr.Cwd, wr.Session)
		}
	}
	for _, p := range res.Problems {
		fmt.Fprintln(w, "problem:", p)
	}
	for _, p := range res.Warnings {
		fmt.Fprintln(w, "warning:", p)
	}
	for _, r := range res.Rules {
		live := "dry"
		if r.Live {
			live = "live"
		}
		if !r.Ran {
			fmt.Fprintf(w, "%s (%s): %s\n", r.Rule, live, r.Why)
			continue
		}
		counts := map[string]int{}
		sizes := map[string]int64{}
		for _, f := range r.Findings {
			counts[f.Action]++
			sizes[f.Action] += f.Bytes
		}
		fmt.Fprintf(w, "%s (%s, %s): delete %d (%s), report %d (%s), keep %d, pending %d (%s)\n", r.Rule, live,
			r.Took.Round(time.Millisecond), counts[capacity.ActDelete], gibText(sizes[capacity.ActDelete]), counts[capacity.ActReport],
			gibText(sizes[capacity.ActReport]), counts[capacity.ActKeep], counts[capacity.ActPending], gibText(sizes[capacity.ActPending]))
		for _, n := range r.Notes {
			fmt.Fprintln(w, "  note:", n)
		}
		if r.Err != "" {
			fmt.Fprintln(w, "  error:", r.Err)
		}
		for _, f := range r.Findings {
			if !all && (f.Action == capacity.ActKeep || f.Action == capacity.ActPending) {
				continue
			}
			act := f.Action
			if f.Done {
				act = "deleted"
			} else if f.Action == capacity.ActDelete && !r.Live {
				act = "would delete"
			}
			line := fmt.Sprintf("  %-12s %10s  %s", act, gibText(f.Bytes), f.Path)
			if !f.Newest.IsZero() {
				line += "  no write since " + f.Newest.Format("2006-01-02")
			}
			if !f.Idle.IsZero() && !f.Idle.Equal(f.Newest) {
				line += ", idle since " + f.Idle.Format("2006-01-02")
			}
			if f.Action == capacity.ActPending && !f.Eligible.IsZero() {
				line += ", eligible " + f.Eligible.Format("2006-01-02")
			}
			for _, s := range []string{f.Detail, f.Why, f.Err} {
				if s != "" {
					line += "; " + s
				}
			}
			if f.Freed > 0 {
				line += "; freed " + gibText(f.Freed)
			}
			fmt.Fprintln(w, line)
		}
	}
}

func gibText(n int64) string {
	if n < 1<<30 {
		return fmt.Sprintf("%.0f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
}
