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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// Mistakes reads the Claude Code mistakes mod's log: one JSON row per tool error, deny, hold, interrupt,
// rejection or correction. The log is append-only; the cursor is the file's inode and the byte offset of
// the last complete line, so a rotated or truncated log is read again from the start.
type Mistakes struct{ Path string }

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
	Heuristic bool   `json:"heuristic"`
}

// actorOf says who produced each kind of row: a tool's outcome, a guard in the harness, or the owner.
func actorOf(kind string) store.Actor {
	switch kind {
	case "deny", "held":
		return store.ActorHarness
	case "user-rejected", "interrupt", "correction":
		return store.ActorOwner
	case "refusal":
		return store.ActorAgent
	default: // tool-error, wait-timeout, probe, verdict, turn-error
		return store.ActorTool
	}
}

func (m Mistakes) Ingest(ctx context.Context, st *store.Store) (int, error) {
	f, err := os.Open(m.Path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	inode := uint64(0)
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		inode = sys.Ino
	}
	cur, err := st.Cursor(ctx, m.Name())
	if err != nil {
		return 0, err
	}
	offset := int64(0)
	if ino, off, ok := parseCursor(cur); ok && ino == inode && off <= info.Size() {
		offset = off
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}

	r := bufio.NewReaderSize(f, 1<<20)
	var evs []store.Event
	pos := offset
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			if ev, ok := m.event(line, pos); ok {
				evs = append(evs, ev)
			}
			pos += int64(len(line))
		}
		if err != nil { // io.EOF: a partial last line is left for the next run
			break
		}
	}
	added, err := st.Put(ctx, evs)
	if err != nil {
		return 0, err
	}
	return added, st.SetCursor(ctx, m.Name(), fmt.Sprintf("%d:%d", inode, pos))
}

func (m Mistakes) event(line []byte, pos int64) (store.Event, bool) {
	var row mistakeRow
	if json.Unmarshal(line, &row) != nil || row.Kind == "" {
		return store.Event{}, false
	}
	ts, err := time.ParseInLocation("2006-01-02T15:04:05", row.TS, time.Local)
	if err != nil {
		return store.Event{}, false
	}
	h := sha256.Sum256(line)
	// the kind is its own column; the summary says what and why
	summary := row.Tool
	if row.Cause != "" {
		summary += " (" + row.Cause + ")"
	}
	if d := firstLine(row.Detail); d != "" {
		if summary != "" {
			summary += ": "
		}
		summary += d
	} else if summary == "" {
		summary = firstLine(row.Input)
	}
	return store.Event{
		Source:    m.Name(),
		SourceKey: hex.EncodeToString(h[:16]),
		TS:        ts,
		Session:   row.Session,
		Agent:     row.AgentID,
		Harness:   "claude-code",
		Model:     row.Model,
		Cwd:       row.Cwd,
		Actor:     actorOf(row.Kind),
		Kind:      row.Kind,
		Tool:      row.Tool,
		Summary:   clip(summary, 240),
		Ref:       fmt.Sprintf("%s@%d", m.Path, pos),
		Heuristic: row.Heuristic,
	}, true
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func parseCursor(c string) (uint64, int64, bool) {
	a, b, ok := strings.Cut(c, ":")
	if !ok {
		return 0, 0, false
	}
	ino, err1 := strconv.ParseUint(a, 10, 64)
	off, err2 := strconv.ParseInt(b, 10, 64)
	return ino, off, err1 == nil && err2 == nil
}
