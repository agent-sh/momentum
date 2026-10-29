package source

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// Mistakes reads a Claude Code mistakes log: one JSON row per tool error, deny, hold, interrupt, rejection
// or correction. The cursor is the file's inode, the byte offset of the last complete line and a fingerprint
// of the first line. A rotated, truncated or recreated log (even one that reuses the inode and grows past
// the old offset) fails the fingerprint and is read again from the start; the content key makes that safe.
type Mistakes struct {
	Path string
	Loc  *time.Location // zone of timestamps written without an offset; nil means time.Local
}

func (m Mistakes) Name() string { return "mistakes" }

type mistakeRow struct {
	TS        string `json:"ts"`
	Session   string `json:"session"`
	Cwd       string `json:"cwd"`
	Model     string `json:"model"`
	Kind      string `json:"kind"`
	Tool      string `json:"tool"`
	Input     string `json:"input"`
	Detail    string `json:"detail"`
	AgentID   string `json:"agentId"`
	Cause     string `json:"cause"`
	Probe     string `json:"probe"`
	Repeat    int    `json:"repeat"`
	Heuristic bool   `json:"heuristic"`
}

// fingerprintLen bounds the bytes read for the first-line fingerprint.
const fingerprintLen = 1024

func (m Mistakes) Ingest(ctx context.Context, st *store.Store) (Result, error) {
	var res Result
	f, err := os.Open(m.Path)
	if errors.Is(err, os.ErrNotExist) {
		return res, nil
	}
	if err != nil {
		return res, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return res, err
	}
	inode := uint64(0)
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		inode = sys.Ino
	}
	fp, err := fingerprint(f)
	if err != nil {
		return res, err
	}
	cur, err := st.Cursor(ctx, m.Name())
	if err != nil {
		return res, err
	}
	offset := int64(0)
	if c, ok := parseLogCursor(cur); ok && c.inode == inode && c.fp == fp && c.offset <= info.Size() {
		offset = c.offset
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return res, err
	}

	r := bufio.NewReaderSize(f, 1<<20)
	var evs []store.Event
	unknown := map[string]int{}
	pos := offset
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			res.Read++
			ref := fmt.Sprintf("%s@%d", m.Path, pos)
			ev, reason := m.event(line, ref)
			if reason != "" {
				res.Skipped++
				if err := st.Skip(ctx, m.Name(), ref, reason); err != nil {
					return res, err
				}
			} else {
				if ev.Actor == store.ActorUnknown {
					unknown[ev.Kind]++
				}
				evs = append(evs, ev)
			}
			pos += int64(len(line))
		}
		if err != nil { // io.EOF: a partial last line is left for the next run
			break
		}
	}
	if res.New, err = st.Put(ctx, evs); err != nil {
		return res, err
	}
	res.Notes = countNote(unknown, "unknown kind")
	sort.Strings(res.Notes)
	return res, st.SetCursor(ctx, m.Name(), logCursor{inode, pos, fp}.String())
}

// event maps one row. A non-empty reason means the row could not be parsed.
func (m Mistakes) event(line []byte, ref string) (store.Event, string) {
	var row mistakeRow
	if err := json.Unmarshal(line, &row); err != nil {
		return store.Event{}, "not json: " + err.Error()
	}
	if row.Kind == "" {
		return store.Event{}, "no kind"
	}
	ts, err := m.parseTS(row.TS)
	if err != nil {
		return store.Event{}, "bad ts: " + row.TS
	}
	h := sha256.Sum256(line)
	actor, summary, extra := m.attribute(row)
	extra["cause"], extra["probe"], extra["repeat"] = row.Cause, row.Probe, row.Repeat
	return store.Event{
		Source:    m.Name(),
		SourceKey: hex.EncodeToString(h[:16]),
		TS:        ts,
		Session:   row.Session,
		Agent:     row.AgentID,
		Harness:   "claude-code",
		Model:     row.Model,
		Cwd:       row.Cwd,
		Actor:     actor,
		Kind:      row.Kind,
		Tool:      row.Tool,
		Summary:   clip(summary, 240),
		Meta:      meta(extra),
		Ref:       ref,
		Heuristic: row.Heuristic,
	}, ""
}

// attribute decides who produced a row, from its kind, its subagent id and its cause, and keeps owner
// summaries to the owner's own words. What the agent said before a correction stays in the source (Ref).
func (m Mistakes) attribute(row mistakeRow) (store.Actor, string, map[string]any) {
	extra := map[string]any{}
	detail := firstLine(row.Detail)
	toolText := func() string {
		s := row.Tool
		if detail != "" {
			if s != "" {
				s += ": "
			}
			s += detail
		}
		return s
	}
	switch row.Kind {
	case "correction":
		// input is what the owner typed; detail is the agent's previous answer, which is not his
		return store.ActorOwner, row.Input, extra
	case "interrupt":
		extra["after"] = detail
		if row.AgentID != "" {
			// a subagent's abort can come from its parent, the harness or the owner: credit nobody
			return store.ActorHarness, "", extra
		}
		return store.ActorOwner, "", extra
	case "user-rejected":
		// the detail is harness boilerplate; the owner typed nothing that the log keeps
		return store.ActorOwner, "", extra
	case "deny", "held":
		return store.ActorHarness, toolText(), extra
	case "turn-error":
		return store.ActorHarness, toolText(), extra
	case "refusal":
		return store.ActorAgent, toolText(), extra
	case "tool-error":
		if row.Cause == "classifier" { // the auto-mode classifier refused the call: a deny, not a tool outcome
			return store.ActorHarness, toolText(), extra
		}
		return store.ActorTool, toolText(), extra
	case "wait-timeout", "probe", "verdict":
		return store.ActorTool, toolText(), extra
	default:
		return store.ActorUnknown, toolText(), extra
	}
}

// parseTS accepts RFC 3339 with an offset, and the older naive local layout in Loc. A naive time in the
// repeated hour of a DST fall-back is ambiguous; Go picks one of the two instants.
func (m Mistakes) parseTS(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	loc := m.Loc
	if loc == nil {
		loc = time.Local
	}
	return time.ParseInLocation("2006-01-02T15:04:05", s, loc)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// fingerprint hashes the file's first line (at most fingerprintLen bytes). An empty file has fingerprint "".
func fingerprint(f *os.File) (string, error) {
	buf := make([]byte, fingerprintLen)
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	buf = buf[:n]
	if i := strings.IndexByte(string(buf), '\n'); i >= 0 {
		buf = buf[:i+1]
	}
	if len(buf) == 0 {
		return "", nil
	}
	h := sha256.Sum256(buf)
	return hex.EncodeToString(h[:8]), nil
}

type logCursor struct {
	inode  uint64
	offset int64
	fp     string
}

func (c logCursor) String() string { return fmt.Sprintf("%d:%d:%s", c.inode, c.offset, c.fp) }

func parseLogCursor(s string) (logCursor, bool) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) != 3 {
		return logCursor{}, false
	}
	ino, err1 := strconv.ParseUint(parts[0], 10, 64)
	off, err2 := strconv.ParseInt(parts[1], 10, 64)
	return logCursor{ino, off, parts[2]}, err1 == nil && err2 == nil
}
