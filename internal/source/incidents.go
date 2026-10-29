package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// Incidents reads an incident detector's case files: one JSON file per incident under open/ and closed/.
// The cursor is the newest file modification time seen, so only changed files are parsed again.
type Incidents struct{ Dir string }

func (i Incidents) Name() string { return "incidents" }

type incidentFile struct {
	ID          string `json:"incident_id"`
	Status      string `json:"status"`
	Summary     string `json:"summary"`
	Signature   string `json:"normalized_signature"`
	Fingerprint string `json:"fingerprint"`
	LastSeen    string `json:"last_seen"`
	CreatedAt   string `json:"created_at"`
	Repeat      int    `json:"repeat_count"`
}

func (i Incidents) Ingest(ctx context.Context, st *store.Store) (int, error) {
	if _, err := os.Stat(i.Dir); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	cur, err := st.Cursor(ctx, i.Name())
	if err != nil {
		return 0, err
	}
	since, _ := strconv.ParseInt(cur, 10, 64)
	newest := since
	var evs []store.Event
	for _, sub := range []string{"open", "closed"} {
		paths, err := filepath.Glob(filepath.Join(i.Dir, sub, "*.json"))
		if err != nil {
			return 0, err
		}
		for _, p := range paths {
			info, err := os.Stat(p)
			if err != nil || info.ModTime().UnixNano() <= since {
				continue
			}
			newest = max(newest, info.ModTime().UnixNano())
			raw, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			var inc incidentFile
			if json.Unmarshal(raw, &inc) != nil || inc.ID == "" {
				continue
			}
			ts := parseTime(inc.LastSeen)
			if ts.IsZero() {
				ts = parseTime(inc.CreatedAt)
			}
			if ts.IsZero() {
				ts = info.ModTime()
			}
			text := inc.Summary
			if text == "" {
				text = inc.Signature
			}
			evs = append(evs, store.Event{
				Source:    i.Name(),
				SourceKey: inc.ID,
				TS:        ts,
				Actor:     store.ActorDetector,
				Kind:      "incident",
				Summary:   clip(fmt.Sprintf("[%s x%d] %s", inc.Status, inc.Repeat, text), 240),
				Ref:       p,
			})
		}
	}
	added, err := st.Put(ctx, evs)
	if err != nil {
		return 0, err
	}
	return added, st.SetCursor(ctx, i.Name(), strconv.FormatInt(newest, 10))
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}
