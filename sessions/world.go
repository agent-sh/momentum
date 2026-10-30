package sessions

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agent-sh/momentum/sessions/proc"
	"github.com/agent-sh/momentum/sessions/term"
)

// World is one snapshot of the machine that every source reads: the time, the processes and the tmux panes.
// Where each harness keeps its files is the Source's own Env. A World is not changed after Snapshot, so
// goroutines can share it.
type World struct {
	Now   time.Time
	Procs *proc.Table
	Panes []term.Pane

	once   sync.Once
	agents []PaneAgent
	inPane map[int]bool
}

// Snapshot reads the process table and the tmux panes once. Without a process table there is no World. When
// tmux fails, the World has no panes and the error says why, so the sessions outside tmux still show.
func Snapshot(ctx context.Context, env Env, now time.Time) (*World, error) {
	t, err := proc.Read(env.Proc)
	if err != nil {
		return nil, fmt.Errorf("process table: %w", err)
	}
	w := &World{Now: now, Procs: t}
	if w.Panes, err = term.ListPanes(ctx); err != nil {
		return w, fmt.Errorf("tmux panes: %w", err)
	}
	return w, nil
}

// Scan asks every source for its sessions over one World and applies the phase rule to each. A failing source
// does not stop the others; their errors come back joined, each named by its tool.
func Scan(ctx context.Context, w *World, sources ...Source) ([]*Session, error) {
	var out []*Session
	var errs []error
	for _, src := range sources {
		ss, err := src.Scan(ctx, w)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", src.Tool(), err))
		}
		for _, s := range ss {
			s.Seq = s.Log.Seq()
			Decide(s, w.Now)
			out = append(out, s)
		}
	}
	return out, errors.Join(errs...)
}

// KindOf says whether a command line runs Claude Code ("claude") or the native Codex binary ("codex"). A
// launcher script or a node shim around either is neither.
func KindOf(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	exe := filepath.Base(argv[0])
	switch {
	case exe == "claude" || strings.Contains(argv[0], "/claude/versions/"):
		return "claude"
	case exe == "codex":
		return "codex"
	}
	return ""
}

// PaneAgent is the nearest Claude Code or Codex process under a tmux pane's shell.
type PaneAgent struct {
	Pane term.Pane
	Kind string
	PID  int
}

// PaneAgents returns, for each pane that runs one, the agent process nearest its shell (breadth first). A pane
// whose shell runs Codex that started Claude Code belongs to Codex.
func (w *World) PaneAgents() []PaneAgent {
	w.once.Do(func() {
		w.inPane = map[int]bool{}
		if w.Procs == nil {
			return
		}
		for _, p := range w.Panes {
			queue := []int{p.PID}
			for len(queue) > 0 {
				pid := queue[0]
				queue = queue[1:]
				if pr := w.Procs.ByPID[pid]; pr != nil {
					if kind := KindOf(pr.Argv); kind != "" {
						w.agents = append(w.agents, PaneAgent{Pane: p, Kind: kind, PID: pid})
						w.inPane[pid] = true
						break
					}
				}
				queue = append(queue, w.Procs.Children(pid)...)
			}
		}
	})
	return w.agents
}

// InPane reports whether pid is the agent of some tmux pane.
func (w *World) InPane(pid int) bool {
	w.PaneAgents()
	return w.inPane[pid]
}

// Proc returns the process with pid in the snapshot, or nil.
func (w *World) Proc(pid int) *proc.Proc {
	if w.Procs == nil {
		return nil
	}
	return w.Procs.ByPID[pid]
}
