package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/config"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
)

// The report is how momentum shows its state: one JSON object that views in surfaces the owner already looks at
// read, with what needs him, what was decided and shipped this week, what runs now and what momentum could not
// fix. momentum keeps no app or dashboard of its own.
const (
	reportVersion = 1
	reportCap     = 20 // items in a section's array; its count covers them all
	// how far back the decided and shipped notes reach (owner 2026-10-31: "7 days might be a too much information. I
	// would want to know probably12 hours")
	reportWindow = 12 * time.Hour
)

// reportSources and reportSnapshot are what a report scans: the harnesses' sources and the machine. Tests put
// synthetic ones in their place.
var (
	reportSources  = sessionSources
	reportSnapshot = sessions.Snapshot
)

// reportScanBudget bounds the report's look at the machine, the snapshot and every harness's scan: a harness not
// done by then counts as failed, and the report prints without it. The views that call the report give it 5 s (the
// phone's) and 10 s (the desktop's); a warm scan takes a small part of that, and a cold one (the first run of a new
// build, before a sessions run saved its checkpoints) may not finish.
var reportScanBudget = 3 * time.Second

// reportItem is one item of a section. Its text is plain, its title one line of at most store.TitleMax characters.
type reportItem struct {
	ID     string       `json:"id"`
	Title  string       `json:"title"`
	Detail string       `json:"detail,omitempty"`
	Effect string       `json:"effect,omitempty"`
	At     string       `json:"at"` // when it happened, was decided, or started
	Links  []store.Link `json:"links"`
	Source string       `json:"source"`

	rank int // its importance in its section: a lower rank comes first, then the newest
	at   time.Time
}

type reportCounts struct {
	NeedsYou int `json:"needs_you"`
	Decided  int `json:"decided"`
	Shipped  int `json:"shipped"`
	Running  int `json:"running"`
	Problems int `json:"problems"`
}

type report struct {
	Version     int          `json:"version"`
	GeneratedAt string       `json:"generated_at"`
	NeedsYou    []reportItem `json:"needs_you"`
	Decided     []reportItem `json:"decided"`
	Shipped     []reportItem `json:"shipped"`
	Running     []reportItem `json:"running"`
	Problems    []reportItem `json:"problems"`
	Counts      reportCounts `json:"counts"`
}

// reportCmd prints the report. It changes nothing: it opens the store read-only, reads the notes, and makes the scan
// a sessions run makes, from that run's checkpoints, without saving any of it. A harness whose scan fails is named on
// stderr and running holds what it read; one not done within reportScanBudget is named and left out. The report
// still prints. A report that cannot be made (no store, a store not this build's schema, no process table) prints
// nothing.
func reportCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "one JSON object on one line, the contract the views read")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Sprintf("report takes no arguments, got %q", fs.Arg(0))}
	}
	env := sessions.EnvFromOS()
	switch c, err := config.Load(); {
	case err == nil:
		env.Projects = c.Sessions.Projects
	case !errors.Is(err, config.ErrNoSources): // the report reads only the project names from it
		fmt.Fprintf(stderr, "momentum: %v; projects go unnamed\n", err)
	}
	st, err := store.OpenReadOnly(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	srcs := reportSources(env)
	var cut []int
	defer func() { // all but the sources the deadline left reading
		for i, s := range srcs {
			if !slices.Contains(cut, i) {
				s.Close()
			}
		}
	}()
	deadline := time.Now().Add(reportScanBudget)
	sctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	w, snapErr := reportSnapshot(sctx, env, time.Now())
	if w == nil {
		return snapErr
	}
	r, scanErr, cut, err := makeReport(ctx, st, w, snapErr, buildID(), srcs, deadline)
	if err != nil {
		return err
	}
	if scanErr != nil {
		fmt.Fprintf(stderr, "momentum: running lacks what this scan could not read: %v\n", scanErr)
	}
	if *asJSON {
		return json.NewEncoder(stdout).Encode(r)
	}
	printReport(stdout, r)
	return nil
}

// makeReport makes the report of one moment, w.Now: the notes that stand, and what runs in the sessions one scan
// over w finds by the deadline. scanErr names what the scan could not read, and cut the sources the deadline left
// reading (scanAll); err is a store failure.
func makeReport(ctx context.Context, st *store.Store, w *sessions.World, snapErr error, build string, srcs []sessions.Source,
	deadline time.Time) (r report, scanErr error, cut []int, err error) {
	ss, scanErr, cut, err := look(ctx, w, snapErr, st, build, srcs, deadline)
	if err != nil {
		return report{}, nil, cut, err
	}
	ns, err := st.Notes(ctx)
	if err != nil {
		return report{}, nil, cut, err
	}
	bySection := noteSections(ns, w.Now)
	r = report{Version: reportVersion, GeneratedAt: rfc3339(w.Now)}
	r.NeedsYou, r.Counts.NeedsYou = section(bySection[store.NoteNeeds])
	r.Decided, r.Counts.Decided = section(bySection[store.NoteDecided])
	r.Shipped, r.Counts.Shipped = section(bySection[store.NoteShipped])
	r.Running, r.Counts.Running = section(running(ss, w.Now))
	r.Problems, r.Counts.Problems = section(bySection[store.NoteProblem])
	return r, scanErr, cut, nil
}

// noteSections are the notes that stand at now, by section, as report items. A note is plain as it is stored; it is
// made plain again here, for one an older build or another writer stored.
func noteSections(ns []store.Note, now time.Time) map[store.NoteKind][]reportItem {
	out := map[store.NoteKind][]reportItem{}
	for _, n := range ns {
		if !n.Standing(now.Add(-reportWindow)) {
			continue
		}
		links := []store.Link{}
		for _, l := range n.Links {
			if u, err := url.Parse(l.URL); err == nil && (u.Scheme == "http" || u.Scheme == "https") && plain.Line(l.URL) == l.URL && plain.Line(l.Label) != "" {
				links = append(links, store.Link{Label: plain.Line(l.Label), URL: l.URL})
			}
		}
		out[n.Kind] = append(out[n.Kind], reportItem{ID: plain.Line(n.ID), Title: oneLine(n.Title, store.TitleMax), Detail: plain.Line(n.Detail),
			Effect: plain.Line(n.Effect), At: rfc3339(n.At), Links: links, Source: firstText(plain.Line(n.Source), store.DefaultNoteSource),
			rank: -n.Importance, at: n.At})
	}
	return out
}

// section orders a section's items by importance, then newest first, and keeps the first reportCap of them. The
// count is of them all.
func section(items []reportItem) ([]reportItem, int) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		switch {
		case a.rank != b.rank:
			return a.rank < b.rank
		case !a.at.Equal(b.at):
			return a.at.After(b.at)
		}
		return a.ID < b.ID
	})
	n := len(items)
	if n > reportCap {
		items = items[:reportCap]
	}
	for i := range items {
		if items[i].Links == nil {
			items[i].Links = []store.Link{}
		}
	}
	if items == nil {
		items = []reportItem{}
	}
	return items, n
}

// look is one look at the live sessions by the scan a sessions run makes (scanAll), by the deadline: each tracker
// resumes from the checkpoint the last sessions run of this build saved and reads only what its transcript gained
// since, and each session takes when it entered its phase from the stamps that run saved. It saves nothing, so it
// takes no lock, and the next sessions run reads again what this one read. A session that began working since that
// run is dated now, by the report's clock, until a sessions run saves its stamp. scanErr names what the scan could
// not read, cut the sources the deadline left reading; err is a store failure.
func look(ctx context.Context, w *sessions.World, snapErr error, st *store.Store, build string, srcs []sessions.Source,
	deadline time.Time) (ss []*sessions.Session, scanErr error, cut []int, err error) {
	ss, failed, scanErr, cut := scanAll(ctx, w, snapErr, st.Trackers(build), srcs, deadline)
	stamps, err := st.SessionStamps(ctx)
	if err != nil {
		return nil, nil, cut, err
	}
	stamps.Apply(ss, w.Now, failed...)
	return ss, scanErr, cut, nil
}

// runRanks order running work: the sessions working first, then workflows, agents, and background jobs and
// monitors. An automated run's work (a cron job, a one-shot) comes after all of it, autoRank lower.
var runRanks = map[string]int{"session": 0, "workflow": 1, "agent": 2, "bash": 3, "monitor": 3}

const autoRank = 4

// running is the work in flight: every session that is working, and every task a session has in flight (its
// workflows, agents, background jobs and monitors), whatever the session's phase.
func running(ss []*sessions.Session, now time.Time) []reportItem {
	var out []reportItem
	used := map[string]bool{}
	for _, s := range ss {
		base := 0
		if s.Auto {
			base = autoRank
		}
		if s.Phase == sessions.Working {
			out = append(out, reportItem{ID: "session:" + plain.Line(s.Key), Title: oneLine(sessionTitle(s), store.TitleMax), Detail: workingDetail(s),
				Source: store.Sessions, rank: base + runRanks["session"], at: firstTime(now, s.Since, s.Started, s.LastActive)})
		}
		for _, t := range s.Tasks {
			id := t.ID
			if id == "" { // a task's id names it from call to call; without one, its kind and start do
				id = t.Kind + "@" + strconv.FormatInt(t.Started.UnixMilli(), 10)
			}
			key := "task:" + plain.Line(s.Key) + "/" + plain.Line(id)
			for n := 2; used[key]; n++ {
				key = fmt.Sprintf("task:%s/%s#%d", plain.Line(s.Key), plain.Line(id), n)
			}
			used[key] = true
			rank, ok := runRanks[t.Kind]
			if !ok {
				rank = runRanks["bash"]
			}
			out = append(out, reportItem{ID: key, Title: oneLine(firstText(t.Label, taskNoun(t)+" "+id), store.TitleMax),
				Detail: taskDetail(s, t), Source: store.Sessions, rank: base + rank, at: firstTime(now, t.Started, s.Since, s.Started)})
		}
	}
	for i := range out {
		out[i].At = rfc3339(out[i].at)
	}
	return out
}

// harnessNames are the harnesses as people name them.
var harnessNames = map[string]string{"claude": "Claude Code", "codex": "Codex", "pi": "pi", "opencode": "opencode", "hermes": "Hermes"}

func harnessName(tool string) string {
	if n, ok := harnessNames[tool]; ok {
		return n
	}
	return tool
}

// sessionTitle names a session: its name, else its project, else its key.
func sessionTitle(s *sessions.Session) string { return firstText(s.Name, s.Project, s.Key) }

// sessionKind is what a session is: a harness's session, or its automated run.
func sessionKind(s *sessions.Session) string {
	if s.Auto {
		return harnessName(s.Tool) + " automated run"
	}
	return harnessName(s.Tool) + " session"
}

// where is " in" the session's project, "" without one. The sessions layer names the home directory home.
func where(s *sessions.Session) string {
	switch s.Project {
	case "":
		return ""
	case "home":
		return " in the home directory"
	}
	return " in " + s.Project
}

// workingDetail says where a working session runs, and what it has in flight.
func workingDetail(s *sessions.Session) string {
	d := sessionKind(s) + where(s)
	d += ", working"
	if len(s.Tasks) > 0 {
		d += ", with " + taskCounts(s.Tasks)
	}
	return oneLine(d+".", 240)
}

// taskDetail says what a task is and which session it is in.
func taskDetail(s *sessions.Session, t sessions.Task) string {
	d := taskNoun(t)
	if t.Kind == "agent" && t.Type != "" {
		d += " (" + t.Type + ")"
	}
	d += " of the " + sessionKind(s) + ` "` + oneLine(sessionTitle(s), 60) + `"` + where(s)
	return oneLine(d+".", 240)
}

// taskNouns name the kinds of task, in the order a count of them lists them.
var taskNouns = []struct{ kind, one, many string }{
	{"workflow", "Workflow", "workflows"}, {"agent", "Agent", "agents"},
	{"bash", "Background job", "background jobs"}, {"monitor", "Monitor", "monitors"},
}

func taskNoun(t sessions.Task) string {
	for _, n := range taskNouns {
		if n.kind == t.Kind {
			return n.one
		}
	}
	return "Task"
}

// taskCounts counts tasks by kind: "2 agents and 1 background job".
func taskCounts(ts []sessions.Task) string {
	n := map[string]int{}
	for _, t := range ts {
		n[t.Kind]++
	}
	var parts []string
	for _, k := range taskNouns {
		switch c := n[k.kind]; {
		case c == 1:
			parts = append(parts, "1 "+strings.ToLower(k.one))
		case c > 1:
			parts = append(parts, fmt.Sprintf("%d %s", c, k.many))
		}
		delete(n, k.kind)
	}
	other := 0
	for _, c := range n {
		other += c
	}
	switch {
	case other == 1:
		parts = append(parts, "1 other task")
	case other > 1:
		parts = append(parts, fmt.Sprintf("%d other tasks", other))
	}
	if len(parts) < 2 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// firstText is the first of texts that has something to show as plain text.
func firstText(texts ...string) string {
	for _, s := range texts {
		if plain.Line(s) != "" {
			return s
		}
	}
	return ""
}

// firstTime is the first of ts that is set, or def.
func firstTime(def time.Time, ts ...time.Time) time.Time {
	for _, t := range ts {
		if !t.IsZero() {
			return t
		}
	}
	return def
}

// rfc3339 is a time as the report gives it: RFC 3339 in local time, to the second.
func rfc3339(t time.Time) string { return t.Local().Format(time.RFC3339) }

// printReport prints the report for a terminal, in the order the views show it: what needs the owner first.
func printReport(out io.Writer, r report) {
	for _, s := range []struct {
		name  string
		items []reportItem
		n     int
	}{
		{"needs you", r.NeedsYou, r.Counts.NeedsYou}, {"problems", r.Problems, r.Counts.Problems},
		{"running", r.Running, r.Counts.Running}, {"shipped", r.Shipped, r.Counts.Shipped}, {"decided", r.Decided, r.Counts.Decided},
	} {
		fmt.Fprintf(out, "%s (%d)\n", s.name, s.n)
		for _, it := range s.items {
			fmt.Fprintf(out, "  %s  %s\n", it.at.Local().Format("01-02 15:04"), it.Title)
			for _, line := range []string{it.Detail, it.Effect} {
				if line != "" {
					fmt.Fprintf(out, "%15s%s\n", "", line)
				}
			}
			for _, l := range it.Links {
				fmt.Fprintf(out, "%15s%s: %s\n", "", l.Label, l.URL)
			}
		}
		if s.n > len(s.items) {
			fmt.Fprintf(out, "  and %d more\n", s.n-len(s.items))
		}
	}
}
