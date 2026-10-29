// Package source turns each existing signal store into momentum events. Every adapter is read-only on its
// source and incremental: it keeps a cursor, and stable source keys make re-reading harmless.
package source

import (
	"context"
	"strings"

	"github.com/agent-sh/momentum/internal/store"
)

// Source is one signal store.
type Source interface {
	Name() string
	// Ingest reads what is new since the saved cursor, stores it, saves the new cursor, and returns
	// how many events were new. A source that does not exist on this machine returns 0 and no error.
	Ingest(ctx context.Context, st *store.Store) (int, error)
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
