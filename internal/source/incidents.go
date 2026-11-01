package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// Incidents reads an incident detector's case files: one JSON file per incident under open/ and closed/.
// Each file is recorded with the modification time and size it had when parsed; a file is parsed again when
// either changes, and a file never seen is parsed whatever its time says (a move, cp -p, rsync -a or a
// restore keeps an old mtime). A file that cannot be read is reported and retried on the next run.
//
// An incident is one event, rewritten as its file changes: its last sighting and its running count of repeats.
// Each change of those is also an event of its own, a sighting (kind incident-seen, keyed by the incident and
// its last sighting), with the repeats since the sighting before it: what the incident did over time, which the
// one event no longer says. A rebuild reads the files again and keeps one sighting per incident, the one its
// file shows now.
type Incidents struct{ Dir string }

func (i Incidents) Name() string { return "incidents" }

type incidentFile struct {
	ID        string `json:"incident_id"`
	Status    string `json:"status"`
	Summary   string `json:"summary"`
	Signature string `json:"normalized_signature"`
	LastSeen  string `json:"last_seen"`
	CreatedAt string `json:"created_at"`
	Repeat    int    `json:"repeat_count"`
	Severity  string `json:"severity"`
}

func (i Incidents) Ingest(ctx context.Context, st *store.Store) (Result, error) {
	var res Result
	if _, err := os.Stat(i.Dir); errors.Is(err, os.ErrNotExist) {
		return res, nil
	}
	type parsed struct {
		path        string
		mtime, size int64
	}
	var evs []store.Event
	var marks []parsed
	for _, sub := range []string{"open", "closed"} {
		paths, err := filepath.Glob(filepath.Join(i.Dir, sub, "*.json"))
		if err != nil {
			return res, err
		}
		for _, p := range paths {
			info, err := os.Stat(p)
			if err != nil {
				continue // moved between glob and stat; the next run sees it where it went
			}
			mtime, size := info.ModTime().UnixNano(), info.Size()
			if m, s, ok, err := st.FileSeen(ctx, i.Name(), p); err != nil {
				return res, err
			} else if ok && m == mtime && s == size {
				continue
			}
			raw, err := os.ReadFile(p)
			if err != nil {
				res.Notes = append(res.Notes, fmt.Sprintf("unreadable, retried next run: %s: %v", p, err))
				continue
			}
			res.Read++
			var inc incidentFile
			if err := json.Unmarshal(raw, &inc); err != nil || inc.ID == "" {
				res.Skipped++
				reason := "no incident_id"
				if err != nil {
					reason = "not json: " + err.Error()
				}
				if err := st.Skip(ctx, i.Name(), p, reason); err != nil {
					return res, err
				}
				marks = append(marks, parsed{p, mtime, size})
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
			e := store.Event{
				Source:    i.Name(),
				SourceKey: inc.ID,
				TS:        ts,
				Actor:     store.ActorDetector,
				Kind:      "incident",
				Summary:   clip(text, 240),
				Meta:      meta(map[string]any{"status": inc.Status, "repeat": inc.Repeat, "severity": inc.Severity}),
				Ref:       p,
			}
			prev, had, err := st.EventByKey(ctx, i.Name(), inc.ID)
			if err != nil {
				return res, err
			}
			var was struct {
				Repeat int `json:"repeat"`
			}
			if had {
				json.Unmarshal([]byte(prev.Meta), &was)
			}
			if !had || !prev.TS.Equal(ts) || was.Repeat != inc.Repeat {
				fresh := inc.Repeat - was.Repeat
				if !had || fresh < 0 { // a first sighting, or a count that started again, holds all it counts
					fresh = inc.Repeat
				}
				evs = append(evs, store.Event{Source: i.Name(), SourceKey: inc.ID + "@" + ts.UTC().Format(time.RFC3339Nano), TS: ts,
					Actor: store.ActorDetector, Kind: "incident-seen", Summary: e.Summary, Ref: p,
					Meta: meta(map[string]any{"status": inc.Status, "repeat": inc.Repeat, "new": max(fresh, 1), "severity": inc.Severity})})
			}
			evs = append(evs, e)
			marks = append(marks, parsed{p, mtime, size})
		}
	}
	n, err := st.Put(ctx, evs)
	if err != nil {
		return res, err
	}
	res.New = n
	for _, m := range marks { // only after the events are stored
		if err := st.MarkFile(ctx, i.Name(), m.path, m.mtime, m.size); err != nil {
			return res, err
		}
	}
	return res, nil
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", s, time.Local); err == nil {
		return t
	}
	return time.Time{}
}
