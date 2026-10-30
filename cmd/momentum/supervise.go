package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agent-sh/momentum/internal/config"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/internal/supervise"
	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/act"
)

// superviseCmd scans the sessions as `momentum sessions` does, then runs the supervision rules over them: what the
// live rules decide is typed into the session, what the others decide is recorded as it would have been sent.
// Every decision is an event of source supervise. `supervise replay` measures the rules over the store's history.
func superviseCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "replay" {
		return replay(ctx, args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("supervise", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dry := fs.Bool("dry", false, "send nothing: every rule records what it would have sent")
	asJSON := fs.Bool("json", false, "one JSON object per decision")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Sprintf("supervise takes no arguments but replay, got %q", fs.Arg(0))}
	}
	env := sessions.EnvFromOS()
	var cfg config.Sources
	switch c, err := config.Load(); {
	case err == nil:
		cfg = c
		env.Projects = c.Sessions.Projects
	case !errors.Is(err, config.ErrNoSources):
		return err
	}
	if cfg.Supervise.Off {
		return nil
	}
	live := supervise.Live
	if cfg.Supervise.Live != nil {
		live = map[string]bool{}
		for _, r := range cfg.Supervise.Live {
			live[r] = true
		}
	}
	if *dry {
		live = map[string]bool{}
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
	sv := &supervise.Supervisor{Store: st, Sender: &act.Sender{ProcRoot: env.Proc}, Params: supervise.DefaultParams, Live: live,
		ClaudeDir: env.ClaudeDir, Notice: cfg.Supervise.Notice}
	lines, err := superviseOnce(ctx, st, paths.DB(), func(now time.Time) (*sessions.World, error) { return sessions.Snapshot(ctx, env, now) },
		srcs, sv, stderr)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	for _, l := range lines {
		if *asJSON {
			if err := enc.Encode(l); err != nil {
				return err
			}
			continue
		}
		mark := l.Verdict
		switch {
		case l.Verdict == supervise.Act && l.Sent:
			mark = "sent"
		case l.Verdict == supervise.Act && l.Err != "":
			mark = "refused"
		case l.Verdict == supervise.Act:
			mark = "dry"
		}
		fmt.Fprintf(stdout, "%-7s %-10s %-30s %s\n", mark, l.Rule, oneLine(l.Name, 30), oneLine(strings.TrimSpace(l.Why+" "+l.Err), 120))
	}
	return nil
}

// superviseOnce scans the sessions as a sessions run does and runs the rules over what it found. A scan that came
// back short decides nothing, and records that: a harness it could not read, or a look at the machine that came
// back incomplete, leaves its sessions unknown on this run, not stopped, and one with no pane in the listing would
// seem to have no terminal. A run that finds another one deciding gives up: the next timer run decides again.
func superviseOnce(ctx context.Context, st *store.Store, db string, snapshot func(time.Time) (*sessions.World, error),
	srcs []sessions.Source, sv *supervise.Supervisor, stderr io.Writer) ([]supervise.Line, error) {
	var world *sessions.World
	ss, _, scanErr, err := observe(ctx, st, store.BusyTimeout, func(now time.Time) (*sessions.World, error) {
		w, err := snapshot(now)
		world = w
		return w, err
	}, buildID(), srcs)
	if err != nil {
		return nil, err
	}
	if scanErr != nil {
		// The scan's own run already reports the failure; supervise records that it decided nothing.
		fmt.Fprintln(stderr, "momentum: supervise skipped: the scan was incomplete:", scanErr)
		now := time.Now()
		why := "the scan was incomplete: decided nothing"
		meta, _ := json.Marshal(map[string]any{"why": why, "error": scanErr.Error()})
		_, err := st.Put(ctx, []store.Event{{Source: store.Supervise, SourceKey: fmt.Sprintf("scan@%d", now.UnixMilli()), TS: now,
			Actor: store.ActorDetector, Kind: supervise.KindSkip, Summary: why, Meta: string(meta)}})
		return nil, err
	}
	unlock, err := lockSupervise(db)
	if err != nil {
		fmt.Fprintln(stderr, "momentum: supervise skipped:", err)
		return nil, nil
	}
	defer unlock()
	starts := map[int]uint64{}
	if world != nil && world.Procs != nil {
		for pid, p := range world.Procs.ByPID {
			starts[pid] = p.StartTicks
		}
	}
	return sv.Run(ctx, ss, starts)
}

// lockSupervise makes supervise runs on one store take turns from deciding to recording, so two never send the
// same message. A run that finds another one there gives up at once: the next timer run decides again.
func lockSupervise(db string) (func(), error) {
	f, err := os.OpenFile(db+".supervise-lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		holder, _ := os.ReadFile(db + ".supervise-lock")
		return nil, fmt.Errorf("another supervise run (pid %s) holds the store", strings.TrimSpace(string(holder)))
	}
	if err := f.Truncate(0); err == nil {
		f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return func() { f.Close() }, nil
}
