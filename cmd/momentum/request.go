package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/agent-sh/momentum/internal/config"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/request"
	"github.com/agent-sh/momentum/internal/source"
	"github.com/agent-sh/momentum/internal/store"
)

// requestCmd shows the owner's requests across the sessions they ran in, prints a request's handoff, and keeps his
// checks of their results on his own words, which an agent relays (SPEC, Requests). list and handoff read the store
// read-only; check and uncheck write.
func requestCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageError{"request needs list, handoff, check or uncheck"}
	}
	switch args[0] {
	case "list":
		return requestList(ctx, args[1:], stdout, stderr)
	case "handoff":
		return requestHandoff(ctx, args[1:], stdout, stderr)
	case "check", "uncheck":
		return requestCheck(ctx, args[0], args[1:], stdout, stderr)
	}
	if _, ok := request.ParseID(args[0]); ok {
		// a handoff's own id lines, run as printed: momentum request ID is its handoff, and so is its opening line,
		// whose words after " - " say what it is
		if len(args) > 1 && args[1] == "-" {
			args = args[:1]
		}
		return requestHandoff(ctx, args, stdout, stderr)
	}
	return usageError{fmt.Sprintf("unknown request command %q", args[0])}
}

// requestOptions are the rules' configuration: the prefixes of the turns the machine's programs type.
func requestOptions(stderr io.Writer) request.Options {
	switch c, err := config.Load(); {
	case err == nil:
		return request.Options{Prefixes: c.Detect.ProgramPrefixes}
	case !errors.Is(err, config.ErrNoSources):
		fmt.Fprintf(stderr, "momentum: %v; no program prefixes are read, so their turns count as the owner's\n", err)
	}
	return request.Options{}
}

// requestJSON is one request in `request list --json`.
type requestJSON struct {
	ID      string   `json:"id"`
	State   string   `json:"state"`
	Opened  string   `json:"opened,omitempty"`
	Last    string   `json:"last"`
	Asking  string   `json:"asking"`
	Asks    int      `json:"asks"`
	Threads []string `json:"threads"`
	// ResultsInWindow counts its results since the list's window start (since), by each result's time; the list also
	// reads back to the request's last turn when its state needs it (SPEC, Requests, Reads)
	ResultsInWindow int      `json:"results_in_window"`
	Newest          string   `json:"newest,omitempty"`
	Open            string   `json:"open,omitempty"`
	Aliases         []string `json:"aliases,omitempty"`
	// Checked is the owner's check that holds, while the state is checked
	Checked *checkedJSON `json:"checked,omitempty"`
}

// checkedJSON is the owner's check of a request: the result it binds, and his turn it rests on, by its time, its
// event (momentum evidence show takes it) and the words relayed, so a wrong relay shows.
type checkedJSON struct {
	Result string `json:"result"`
	At     string `json:"at"`
	Proof  string `json:"proof"`
	Words  string `json:"words"`
}

func requestList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("request list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	since := fs.String("since", "7d", "the requests with events since WHEN")
	asJSON := fs.Bool("json", false, "one JSON object on one line")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usageError{strayArgs("request list", fs.Arg(0))}
	}
	from, err := parseWhen(*since)
	if err != nil {
		return usageError{err.Error()}
	}
	opt := requestOptions(stderr)
	st, err := store.OpenReadOnly(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	reqs, err := request.List(ctx, st, from, opt)
	if err != nil {
		return err
	}
	if *asJSON {
		out := struct {
			Version     int           `json:"version"`
			GeneratedAt string        `json:"generated_at"`
			Since       string        `json:"since"` // the window: a request's results_in_window counts those since
			Requests    []requestJSON `json:"requests"`
		}{Version: 1, GeneratedAt: time.Now().Format(time.RFC3339), Since: from.Local().Format(time.RFC3339), Requests: []requestJSON{}}
		for _, rq := range reqs {
			n := 0
			for _, x := range rq.Results {
				if !x.At.Before(from) {
					n++
				}
			}
			j := requestJSON{ID: rq.ID, State: rq.State, Last: rq.Last.Local().Format(time.RFC3339), Asking: asking(rq), Asks: len(rq.Asks),
				Threads: []string{}, ResultsInWindow: n, Open: plain.Line(rq.Open), Aliases: rq.Aliases}
			if !rq.Opened.IsZero() {
				j.Opened = rq.Opened.Local().Format(time.RFC3339)
			}
			if n := rq.Newest(); n != nil {
				j.Newest = n.Key
			}
			for _, th := range rq.Threads {
				j.Threads = append(j.Threads, th.ID)
			}
			if c, _, holds := rq.Standing(); holds && rq.State == request.Checked {
				j.Checked = &checkedJSON{Result: c.Result, At: c.ProofAt.Local().Format(time.RFC3339), Proof: "ev:" + c.Proof, Words: c.Words}
			}
			out.Requests = append(out.Requests, j)
		}
		b, err := json.Marshal(out)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, string(b))
		return err
	}
	if len(reqs) == 0 {
		fmt.Fprintln(stdout, "no requests since", from.Local().Format("2006-01-02 15:04"))
	}
	for _, rq := range reqs {
		sessions := fmt.Sprintf("%d sessions", len(rq.Threads))
		if len(rq.Threads) == 1 {
			sessions = "1 session"
		}
		checked := ""
		if c, _, holds := rq.Standing(); holds && rq.State == request.Checked {
			checked = "  (checked in his turn of " + c.ProofAt.Local().Format("2006-01-02 15:04") + ": \"" + plain.Clip(c.Words, 80) + "\")"
		}
		fmt.Fprintf(stdout, "%-14s %-9s %s  %-11s %s%s\n", rq.ID, rq.State, rq.Last.Local().Format("2006-01-02 15:04"), sessions,
			plain.Clip(asking(rq), 80), checked)
	}
	return nil
}

// asking is the request's first words: its opening's, else unknown.
func asking(rq *request.Request) string {
	if rq.Opened.IsZero() {
		return "(its opening is not in this store)"
	}
	for _, a := range rq.Asks {
		if a.Words != "" {
			return plain.Line(a.Words)
		}
	}
	return ""
}

// requestHandoff prints a request's handoff.
func requestHandoff(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("request handoff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() != 1 {
		return usageError{"request handoff needs one ID: a request's id, or a session's id or its start"}
	}
	st, err := store.OpenReadOnly(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	rq, err := request.Find(ctx, st, fs.Arg(0), requestOptions(stderr))
	if errors.Is(err, request.ErrNoRequest) {
		return usageError{err.Error()}
	}
	if err != nil {
		return err
	}
	_, err = io.WriteString(stdout, request.Handoff(rq))
	return err
}

// requestCheck records the owner's check of a request's result, or undoes it, on his own turn an agent relays with
// --proof: momentum finds it among his turns in the request's sessions, after reading what is new in the transcripts,
// and refuses, saying why, when it does not.
func requestCheck(ctx context.Context, cmd string, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("request "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	proof := fs.String("proof", "", "the owner's own turn where he says it, whole, as he typed it")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	var ids []string
	for fs.NArg() > 0 { // the flag before or after the id
		ids = append(ids, fs.Arg(0))
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return usageError{err.Error()}
		}
	}
	if len(ids) != 1 {
		return usageError{fmt.Sprintf(`request %s needs one ID (a request's id, or a session's id or its start) and --proof "WORDS", the owner's own`, cmd)}
	}
	if err := request.ProofShape(*proof); err != nil {
		return usageError{fmt.Sprintf("%s %s not recorded: %v", ids[0], cmd, err)}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	caught := catchUp(ctx, st)
	rq, err := request.Find(ctx, st, ids[0], requestOptions(stderr))
	if errors.Is(err, request.ErrNoRequest) {
		return usageError{err.Error()}
	}
	if err != nil {
		return err
	}
	refused := func(err error) error {
		if !errors.Is(err, request.ErrNoProof) && !errors.Is(err, request.ErrNoRequest) && !errors.Is(err, store.ErrNoCheck) {
			return err
		}
		msg := fmt.Sprintf("%s: %s not recorded: %v", rq.ID, cmd, err)
		if errors.Is(err, request.ErrNoTurn) {
			// the store lags his turns: say how far it reaches, never that he did not say it
			through, rerr := st.ReadThrough(ctx, store.Transcripts)
			reach := "has read no transcripts yet"
			if rerr == nil && !through.IsZero() {
				reach = "holds the transcripts as read up to " + through.Local().Format("2006-01-02 15:04")
			}
			msg += "; the store " + reach
			if caught != nil {
				msg += ", and the ingest this command runs first could not read them (" + caught.Error() + ")"
			}
			msg += ", so a turn of his after that is not in it yet: retry after the next ingest"
		}
		return usageError{msg}
	}
	if cmd == "uncheck" {
		c, p, err := request.Uncheck(ctx, st, rq, *proof, time.Now())
		if err != nil {
			return refused(err)
		}
		fmt.Fprintf(stdout, "%s: undid the check of his turn of %s on %s, on his turn of %s, relayed as %q (ev:%s)\n", rq.ID,
			c.ProofAt.Local().Format("2006-01-02 15:04"), c.Result, p.At.Local().Format("2006-01-02 15:04"), p.Words, p.Event)
		return nil
	}
	c, err := request.Check(ctx, st, rq, *proof, time.Now())
	if err != nil {
		return refused(err)
	}
	if !c.Added {
		fmt.Fprintf(stdout, "%s: checked already, on his turn of %s\n", rq.ID, c.Proof.At.Local().Format("2006-01-02 15:04"))
		return nil
	}
	fmt.Fprintf(stdout, "%s: checked %s, on his turn of %s, relayed as %q (ev:%s); a newer artifact or a later ask of his reopens it\n", rq.ID,
		plain.Clip(plain.Line(c.Result.Text), 120), c.Proof.At.Local().Format("2006-01-02 15:04"), c.Proof.Words, c.Proof.Event)
	return nil
}

// catchUpFor bounds the ingest a check runs first: the store lags his turns by up to an ingest timer's period.
const catchUpFor = 2 * time.Minute

// catchUp reads what is new in the transcripts into the store, as the ingest timer does and under its lock, for at
// most catchUpFor, so his newest turn is there when a check looks for it. It waits while another ingest holds the
// lock. It returns why it could not read them.
func catchUp(ctx context.Context, st *store.Store) error {
	sources, err := configured()
	if err != nil {
		return err
	}
	var tr []source.Source
	for _, s := range sources {
		if s.Name() == store.Transcripts {
			tr = append(tr, s)
		}
	}
	if len(tr) == 0 {
		return errors.New("no transcripts source is configured")
	}
	ctx, cancel := context.WithTimeout(ctx, catchUpFor)
	defer cancel()
	for {
		unlock, err := st.LockIngest()
		if err == nil {
			defer unlock()
			break
		}
		if !errors.Is(err, store.ErrIngestHeld) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%v for %s", err, catchUpFor)
		case <-time.After(time.Second):
		}
	}
	for _, o := range source.RunAll(ctx, st, tr) {
		if o.Err != nil {
			return o.Err
		}
	}
	return nil
}
