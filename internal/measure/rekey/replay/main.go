// Command replay keys a store's events with the detectors of the tree it is built from and writes each keyed event's
// id and signature (rekey.Write). The loop runs it, in the sandbox, on the base of a change to its own detectors and
// on the change, over the same copy of the store, and compares the two (rekey.Compare). It reads the store read-only
// and writes only its output file.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/agent-sh/momentum/internal/detect"
	"github.com/agent-sh/momentum/internal/measure/rekey"
	"github.com/agent-sh/momentum/internal/store"
)

func main() {
	storePath := flag.String("store", "", "the store to read")
	since := flag.String("since", "", "the window's start (RFC 3339)")
	until := flag.String("until", "", "the window's end (RFC 3339)")
	out := flag.String("out", "", "the file to write")
	flag.Parse()
	if err := run(*storePath, *since, *until, *out); err != nil {
		fmt.Fprintln(os.Stderr, "replay:", err)
		os.Exit(1)
	}
}

func run(storePath, since, until, out string) error {
	from, err := time.Parse(time.RFC3339Nano, since)
	if err != nil {
		return fmt.Errorf("-since: %w", err)
	}
	to, err := time.Parse(time.RFC3339Nano, until)
	if err != nil {
		return fmt.Errorf("-until: %w", err)
	}
	st, err := store.OpenReadOnly(storePath)
	if err != nil {
		return err
	}
	defer st.Close()
	keyed, err := detect.KeyedEvents(context.Background(), st, detect.Window{Since: from, Until: to}, detect.Programs{})
	if err != nil {
		return err // a replay that keyed part of the window would read as a change that dropped the rest
	}
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	lines := make([]rekey.Line, 0, len(keyed))
	for _, k := range keyed {
		lines = append(lines, rekey.Line{ID: k.Event.ID(), Signature: k.Signature})
	}
	if err := rekey.Write(w, rekey.Header{Since: from, Until: to}, lines); err != nil {
		f.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
