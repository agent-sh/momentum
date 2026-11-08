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
	"github.com/agent-sh/momentum/internal/store"
)

// requestCmd shows the owner's requests across the sessions they ran in, and prints a request's handoff (SPEC,
// Requests). Both read the store read-only.
func requestCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageError{"request needs list or handoff"}
	}
	switch args[0] {
	case "list":
		return requestList(ctx, args[1:], stdout, stderr)
	case "handoff":
		return requestHandoff(ctx, args[1:], stdout, stderr)
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
	// ResultsInWindow counts its results whose calls came in the list's window (since): a list reads only those
	ResultsInWindow int      `json:"results_in_window"`
	Newest          string   `json:"newest,omitempty"`
	Open            string   `json:"open,omitempty"`
	Aliases         []string `json:"aliases,omitempty"`
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
		fmt.Fprintf(stdout, "%-14s %-9s %s  %-11s %s\n", rq.ID, rq.State, rq.Last.Local().Format("2006-01-02 15:04"), sessions,
			plain.Clip(asking(rq), 80))
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
