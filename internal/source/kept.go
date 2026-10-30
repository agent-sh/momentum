package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/agent-sh/tacit/client/pkg/evidence"

	"github.com/agent-sh/momentum/internal/store"
)

// Gone counts the transcripts' events whose source is gone, which a rebuild drops for good: those the next
// ingest of the homes cannot read again. That is a transcript it does not find by its name (an event's key holds
// the name, so a rollout Codex archived since, or a compressed copy, is read again), and a Hermes session that no
// store under the homes holds. With no homes, nothing reads the transcripts again, and every event is gone. A
// rebuild also loses what a source took back from a file or session still there (lines past a cut, rows Hermes
// took back), which Gone does not count.
func (s Transcripts) Gone(ctx context.Context, st *store.Store) (int, error) {
	origins, err := st.Origins(ctx, s.Name())
	if err != nil {
		return 0, err
	}
	found := map[string]bool{}  // the names of the transcripts under the homes
	stores := map[string]bool{} // the Hermes stores the homes name
	for _, home := range s.Homes {
		for _, src := range evidence.Sources(home) {
			if src.Format == evidence.Hermes {
				if src.DB != "" {
					abs, err := filepath.Abs(src.DB)
					if err != nil {
						return 0, err
					}
					stores[abs] = true
				}
				continue
			}
			files, err := evidence.Files(src)
			if err != nil {
				return 0, fmt.Errorf("%s: %w", src.Glob, err)
			}
			for _, f := range files {
				found[evidence.Name(f)] = true
			}
		}
	}
	held := map[string]map[string]bool{} // a Hermes store's sessions, by the store
	gone := 0
	for o, n := range origins {
		if o.Harness != "hermes" {
			if !found[evidence.Name(o.Path)] {
				gone += n
			}
			continue
		}
		if !stores[o.Path] {
			gone += n
			continue
		}
		ids, ok := held[o.Path]
		if !ok {
			if ids, err = hermesSessions(ctx, o.Path); err != nil {
				return 0, err
			}
			held[o.Path] = ids
		}
		// A delegation's events are in its parent's session, the delegation their subagent.
		id := o.Session
		if o.Agent != "" {
			id = o.Agent
		}
		if !ids[id] {
			gone += n
		}
	}
	return gone, nil
}

// hermesSessions are the sessions a Hermes store holds, none when the store is gone.
func hermesSessions(ctx context.Context, path string) (map[string]bool, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	db, err := evidence.OpenHermes(path)
	if err != nil {
		return nil, fmt.Errorf("hermes store %s: %w", path, err)
	}
	defer db.Close()
	ss, err := db.Sessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("hermes store %s: %w", path, err)
	}
	ids := map[string]bool{}
	for _, s := range ss {
		ids[s.ID] = true
	}
	return ids, nil
}
