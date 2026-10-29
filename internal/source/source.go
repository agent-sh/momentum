// Package source turns each existing signal store into momentum events. Every adapter is read-only on its
// source and incremental. Stable source keys make re-reading harmless, so an adapter re-reads a margin
// rather than risk a gap, and anything it cannot parse is recorded, never silently dropped.
//
// Prior art for the log cursor: the OpenTelemetry filelog receiver identifies a file by a fingerprint of
// its first bytes plus the inode, and treats an mtime-only skip as safe only where an unchanged mtime
// reliably means unchanged content. The adapters here follow both rules.
package source

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agent-sh/momentum/internal/store"
)

// Result is what one ingest run did.
type Result struct {
	New     int      // events stored for the first time
	Read    int      // records read from the source this run (a re-read margin counts)
	Skipped int      // records that could not be parsed; each is kept in the store's skipped table
	Notes   []string // anything worth reporting: unknown record kinds, unreadable files
}

// Source is one signal store.
type Source interface {
	Name() string
	// Ingest reads what is new since the saved state, stores it, saves the new state and reports.
	// A source that does not exist on this machine reads nothing and returns no error.
	Ingest(ctx context.Context, st *store.Store) (Result, error)
}

// Outcome is one source's part of a run.
type Outcome struct {
	Name string
	Result
	Err error
}

// RunAll ingests every source. A failing source is reported and never stops the others.
func RunAll(ctx context.Context, st *store.Store, sources []Source) []Outcome {
	out := make([]Outcome, 0, len(sources))
	for _, s := range sources {
		r, err := s.Ingest(ctx, st)
		out = append(out, Outcome{Name: s.Name(), Result: r, Err: err})
	}
	return out
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// meta encodes structured extras; empty values are left out, and an empty map is "".
func meta(kv map[string]any) string {
	for k, v := range kv {
		if v == nil || v == "" || v == false || v == 0 {
			delete(kv, k)
		}
	}
	if len(kv) == 0 {
		return ""
	}
	b, err := json.Marshal(kv)
	if err != nil {
		return ""
	}
	return string(b)
}

func countNote(counts map[string]int, what string) []string {
	var out []string
	for k, n := range counts {
		out = append(out, fmt.Sprintf("%s %q: %d", what, k, n))
	}
	return out
}
