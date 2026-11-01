// momentum keeps improving an agent system. It ingests the signal stores that already exist into one local event
// store, keeps the live sessions moving, keeps the disks from filling, and keeps the cases its improvement loop works.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/config"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/source"
	"github.com/agent-sh/momentum/internal/store"
)

var version = "0.0.0-dev"

const usage = `usage:
  momentum evidence ingest              read what is new in every configured source into the store
  momentum evidence events [--since WHEN] [--until WHEN] [--kind K,K] [--source S] [--harness H] [--session ID]
                           [--limit N] [--json]
                                        events, newest first; one session's (an id or its start) oldest first;
                                        H is claude, codex, pi, opencode or hermes
  momentum evidence status [--since DUR]   events per source and kind, and unparsed records
  momentum evidence show ID             an event, and the record of the transcript or log it names (scrubbed)
  momentum evidence git DIR GITCOMMAND [ARG]... [-- PATH...]
                                        a git command that reads (log, show, diff, blame, grep, ...) in DIR's
                                        work tree, with only the options that read; revisions are checked,
                                        paths come after --
  momentum evidence file PATH [--tail N]   a file of the machine, scrubbed of secrets; not where credentials are
  momentum evidence unit NAME [--lines N]  a user service's state and the end of its log, scrubbed
  momentum evidence rebuild SOURCE [--force]
                                        drop what SOURCE contributed; the next ingest reads it from the start
                                        (not sessions; transcripts and incidents only with --force: it says how
                                        many transcript events have no source left, and incidents' sightings
                                        are history their files do not keep)
  momentum sessions [--json] [--all]    the live agent sessions and their phases, read without a model;
                                        each phase change is kept as an event of source sessions
  momentum report [--json]              what needs the owner, what was decided and shipped in the last 12 hours,
                                        what runs now and what momentum could not fix; changes nothing.
                                        --json prints the one object the owner's views read
  momentum note add --kind needs|decided|shipped|problem --title T [--detail D] [--effect E]
                    [--link LABEL=URL]... [--id ID] [--source S] [--importance high|normal|low]
                                        a note for the report; prints its id, and an id stored already is replaced
  momentum note done ID                 take a note out of the report: resolve a needs or problem note,
                                        withdraw a decided or shipped one
  momentum note list [--json]           every note, newest first, and whether the report shows it
  momentum supervise [--dry] [--json]   scan the sessions, then continue the ones that stopped where a rule says
                                        it may: every decision is an event of source supervise
  momentum supervise replay [--since WHEN] [--until WHEN] [--labels FILE] [--anchor] [--points FILE]
                                        run the rules over the store's history and measure them against the
                                        owner's own steps
  momentum capacity [--dry-run] [--now] [--rule R,R] [--json] [--all]
                                        keep the disks from filling: sample which paths processes use, record
                                        each disk's use, and run the rules that are due (models, targets,
                                        worktrees, scratch, uv, docker); a rule not live lists what it would
                                        delete or offload.
                                        --dry-run deletes and records nothing and runs every rule
  momentum case detect [--since WHEN] [--family F,F] [--dry] [--json]
                                        run the detectors (failures, owner-burden, sessions) over the store and
                                        file what recurs: a case per signature; then, on a week's window or more,
                                        the cases of the families it read that are unseen for 30 days close
  momentum case list [--family F] [--status S,S] [--all] [--json]
                                        the cases, the most recently changed first; closed ones with --all
  momentum case show ID [--evidence N] [--log N] [--json]
                                        one case: its investigation, cost, signatures, newest evidence and its
                                        log's last lines
  momentum case open --family F --signature S --title T [--evidence REF]...
                                        open a case, or give the case that holds S the evidence
  momentum case set ID [--status open|investigating|testing] [--title T] [--mechanism M] [--alternative A]...
                       [--next-test T] [--benefit B]
                                        record the investigation; testing needs a mechanism and a next test
  momentum case evidence ID [--at WHEN] [--kind K] [--summary S] REF...
                                        more evidence: ev:EVENT, note:ID, git:OWNER/REPO@SHA, file:PATH[@OFFSET],
                                        an address; an event or a note takes its own time, kind and summary
  momentum case signature ID SIG        another signature (the root cause's: root:KEY); one another open case
                                        holds merges this case into that one (a closed one comes back first)
  momentum case close ID --reason R [--into ID] --why W
                                        R is not-worth-it, insufficient-evidence, already-solved or duplicate
  momentum case reopen ID --evidence REF... [--at WHEN] --why W
                                        a closed case comes back only on evidence it does not hold
  momentum loop [--dry] [--family F,F] [--case ID] [--json]
                                        the improvement loop, once: detect, take each family's case a step with the
                                        harness's agent, review and land what was proposed
  momentum loop runs [--since WHEN] [--case ID] [--json]
                                        the loop's runs, and what the live ones spent in the last day and week
  momentum loop lesson --case ID --text T
                                        a case closed as improved: its lesson to tacit, as the loop's observation
  momentum ship prepare --repo DIR [--case ID]
  momentum ship check --test CMD [--test-files F,F] [--case ID]
  momentum ship propose --title T --body-file F --rollback R --signal JSON [--window D] [--lesson L] [--case ID]
  momentum ship status [--case ID] [--json]
  momentum ship land ID
  momentum ship rollback ID --why W
                                        a case's change package: built in the loop's own work tree, checked by a
                                        test that fails without it, proposed as a pull request with its
                                        measurement and rollback, revised for its review, landed only live and
                                        reviewed clean
  momentum version
Sources are configured in $MOMENTUM_CONFIG/sources.json (default ~/.config/momentum/sources.json), and the
capacity rules in its capacity section.
The store is $MOMENTUM_HOME/momentum.db (default ~/.local/share/momentum).
DUR is a Go duration or a number of days, e.g. 36h or 7d; WHEN is a DUR ago or a local time, e.g. 2026-01-02T15:04.
Exit: 0 done, 2 usage or configuration error,
3 error (for ingest and sessions: at least one source failed, for capacity at least one rule; the others still
ran; a report whose scan of a harness failed or was not done within 3 s still prints, exits 0 and names it on
stderr).`

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
	case "sessions":
		err = sessionsCmd(ctx, args[1:], stdout, stderr)
	case "report":
		err = reportCmd(ctx, args[1:], stdout, stderr)
	case "note":
		err = noteCmd(ctx, args[1:], stdout, stderr)
	case "supervise":
		err = superviseCmd(ctx, args[1:], stdout, stderr)
	case "capacity":
		err = capacityCmd(ctx, args[1:], stdout, stderr)
	case "case":
		err = caseCmd(ctx, args[1:], stdout, stderr)
	case "loop":
		err = loopCmd(ctx, args[1:], stdout, stderr)
	case "ship":
		err = shipCmd(ctx, args[1:], stdout, stderr)
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
		return events(ctx, args[1:], stdout, stderr)
	case "status":
		return status(ctx, args[1:], stdout)
	case "rebuild":
		return rebuild(ctx, args[1:], stdout, stderr)
	case "git":
		return evidenceGit(ctx, args[1:], stdout, stderr)
	case "show":
		return evidenceShow(ctx, args[1:], stdout, stderr)
	case "file":
		return evidenceFile(ctx, args[1:], stdout, stderr)
	case "unit":
		return evidenceUnit(ctx, args[1:], stdout, stderr)
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
	if len(c.Transcripts.Homes) > 0 {
		out = append(out, source.Transcripts{Homes: c.Transcripts.Homes, Workers: c.Transcripts.Workers})
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
		}
		// What a failing source stored, and what failed, by name.
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

func events(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	since := fs.String("since", "1d", "how far back, or from when (one session: its start)")
	until := fs.String("until", "", "until when")
	kinds := fs.String("kind", "", "comma-separated kinds")
	src := fs.String("source", "", "one source")
	harness := fs.String("harness", "", "one harness's events, from every source: "+strings.Join(store.Harnesses, ", "))
	session := fs.String("session", "", "one session, by its id or its start; oldest first")
	limit := fs.Int("limit", 50, "most events to print (0: 200)")
	asJSON := fs.Bool("json", false, "one JSON object per line")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if *limit <= 0 {
		*limit = 200
	}
	sinceSet := false
	fs.Visit(func(f *flag.Flag) { sinceSet = sinceSet || f.Name == "since" })
	var from time.Time // one session reads from its start, unless --since says otherwise
	if *session == "" || sinceSet {
		var err error
		if from, err = parseWhen(*since); err != nil {
			return usageError{err.Error()}
		}
	}
	if *harness != "" && !slices.Contains(store.Harnesses, *harness) {
		return usageError{fmt.Sprintf("unknown harness %q: one of %s", *harness, strings.Join(store.Harnesses, ", "))}
	}
	var err error
	q := store.Query{Since: from, Source: *src, Harness: *harness, Session: *session, Limit: *limit + 1}
	if *until != "" {
		if q.Until, err = parseWhen(*until); err != nil {
			return usageError{err.Error()}
		}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	if *kinds != "" {
		q.Kinds = strings.Split(*kinds, ",")
	}
	evs, err := st.Events(ctx, q)
	if err != nil {
		return err
	}
	if len(evs) > *limit {
		evs = evs[:*limit]
		defer fmt.Fprintf(stderr, "momentum: more events than --limit %d; raise it, or narrow with --since and --until\n", *limit)
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
		what := e.Kind
		if e.Tool != "" {
			what += " " + e.Tool
		}
		if strings.Contains(e.Meta, `"is_error":true`) {
			what += " (error)"
		}
		if e.Agent != "" {
			tag += " [subagent " + e.Agent + "]"
		}
		// Plain text: a summary holds what agents and tools wrote, which must not act on the terminal.
		fmt.Fprintf(stdout, "%s  %-15s %-8s %-16s %s%s\n", e.TS.Local().Format("01-02 15:04:05"), e.Source, e.Actor, plain.Line(what),
			plain.Line(e.Summary), plain.Line(tag))
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

// rebuild drops what a source contributed. The transcripts' events can be all that is left of transcripts their
// harness deleted, and the incidents' sightings are all there is of what each incident did over time, so they go
// only with --force, which counts first how many transcript events have no source left. The flag may come before
// or after the source's name.
func rebuild(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("rebuild", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "drop kept events too (the transcripts', the incidents' sightings), which cannot be read again")
	var flags, names []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
		} else {
			names = append(names, a)
		}
	}
	if err := fs.Parse(flags); err != nil {
		return usageError{err.Error()}
	}
	if len(names) != 1 {
		return usageError{"rebuild needs one source name"}
	}
	name := names[0]
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	gone := 0
	if name == store.Transcripts && *force {
		// The homes the ingest reads, where a transcript moved since is found again by its name.
		var homes []string
		switch c, err := config.Load(); {
		case err == nil:
			homes = c.Transcripts.Homes
		case !errors.Is(err, config.ErrNoSources):
			return err
		}
		if gone, err = (source.Transcripts{Homes: homes}).Gone(ctx, st); err != nil {
			return err
		}
	}
	n, err := st.Rebuild(ctx, name, *force)
	switch {
	case errors.Is(err, store.ErrObserved):
		return usageError{err.Error()}
	case errors.Is(err, store.ErrKept):
		return usageError{err.Error() + "; --force drops them all the same"}
	case err != nil:
		return err
	}
	switch name {
	case store.Incidents:
		fmt.Fprintf(stdout, "%s: dropped %d events; their sightings are gone for good, and the next ingest reads the files from the start\n", name, n)
		return nil
	case store.Transcripts:
	default:
		fmt.Fprintf(stdout, "%s: dropped %d events; the next ingest reads it from the start\n", name, n)
		return nil
	}
	msg := fmt.Sprintf("%s: dropped %d events; %d of them have no source left and are gone for good", name, n, gone)
	if gone < n {
		msg += "; the next ingest reads the rest from the start"
	}
	fmt.Fprintln(stdout, msg)
	return nil
}

// parseWhen is a time: a duration ago (parseSince), or a local time ("2006-01-02T15:04", seconds optional)
// or an RFC 3339 one.
func parseWhen(s string) (time.Time, error) {
	if d, err := parseSince(s); err == nil {
		return time.Now().Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("bad time %q: a duration (36h, 7d) or a time (2006-01-02T15:04)", s)
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
