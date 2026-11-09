package source

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/pkg/evidence"
	_ "modernc.org/sqlite"

	"github.com/agent-sh/momentum/internal/store"
)

type openCodeDB struct {
	*sql.DB
	path   string
	v2     bool
	legacy bool
}

type openCodeSession struct {
	id, parent, cwd, fingerprint string
	v2                           bool
}

func openOpenCode(path string) (*openCodeDB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", readOnlyDSN(abs))
	if err != nil {
		return nil, errors.New("cannot open opencode store read-only")
	}
	out := &openCodeDB{DB: db, path: abs}
	var tables []string
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name IN ('session','message','part','session_v2','session_message')`)
	if err != nil {
		db.Close()
		return nil, errors.New("cannot inspect opencode schema")
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			db.Close()
			return nil, err
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		db.Close()
		return nil, err
	}
	has := func(name string) bool {
		for _, n := range tables {
			if n == name {
				return true
			}
		}
		return false
	}
	out.v2, out.legacy = has("session_v2") && has("session_message"), has("session") && has("message") && has("part")
	if !out.v2 && !out.legacy {
		db.Close()
		return nil, errors.New("unsupported opencode schema")
	}
	return out, nil
}

// Updated rows are mutable, including tool states. A changed session is read
// again by stable message ids. Counts and update times tell a changed session
// from the last read. V2 wins over the legacy copy for the same session.
func (db *openCodeDB) sessions(ctx context.Context) ([]openCodeSession, error) {
	var out []openCodeSession
	held := map[string]bool{}
	read := func(table, messages string, v2 bool) error {
		query := `SELECT s.id,COALESCE(s.parent_id,''),COALESCE(s.directory,''),COALESCE(s.time_updated,0),
COUNT(m.id),COALESCE(SUM(m.time_updated),0)
FROM ` + table + ` s JOIN ` + messages + ` m ON m.session_id=s.id GROUP BY s.id`
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return errors.New("cannot list opencode sessions")
		}
		defer rows.Close()
		for rows.Next() {
			var s openCodeSession
			var updated, count, sum int64
			if err := rows.Scan(&s.id, &s.parent, &s.cwd, &updated, &count, &sum); err != nil {
				return err
			}
			if held[s.id] {
				continue
			}
			held[s.id], s.v2 = true, v2
			// Legacy tool states live in part, not message.
			var partCount, partSum int64
			if !v2 {
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(time_updated),0)
FROM part WHERE session_id=?`, s.id).Scan(&partCount, &partSum); err != nil {
					return err
				}
			}
			hash := sha256.Sum256([]byte(fmt.Sprintf("%t:%s:%s:%d:%d:%d:%d:%d",
				v2, s.parent, s.cwd, updated, count, sum, partCount, partSum)))
			s.fingerprint = hex.EncodeToString(hash[:])
			out = append(out, s)
		}
		return rows.Err()
	}
	if db.v2 {
		if err := read("session_v2", "session_message", true); err != nil {
			return nil, err
		}
	}
	if db.legacy {
		if err := read("session", "message", false); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s Transcripts) readOpenCodeJob(ctx context.Context, j *job, t *tally, add func(part) bool) {
	db, session := j.native.db, j.native.session
	state := nativeState{Session: session.id, Parent: session.parent, Cwd: session.cwd, Calls: map[string]string{}}
	if session.parent != "" {
		state.Subagent = session.id
	}
	p := part{j: j}
	size := 0
	flush := func(final bool) bool {
		p.cur = store.FileCursor{Source: s.Name(), Path: j.path}
		if final {
			p.cur.FP = session.fingerprint
		}
		ok := add(p)
		p, size = part{j: j}, 0
		return ok
	}
	query := `SELECT id,json_extract(data,'$.role'),time_created,data FROM message WHERE session_id=? ORDER BY time_created,id`
	if session.v2 {
		query = `SELECT id,type,time_created,data FROM session_message WHERE session_id=? ORDER BY seq,id`
	}
	rows, err := db.QueryContext(ctx, query, session.id)
	if err != nil {
		t.fail(j.path, errors.New("cannot read opencode messages"))
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, typ, data string
		var created int64
		if err := rows.Scan(&id, &typ, &created, &data); err != nil {
			t.fail(j.path, err)
			return
		}
		var msg map[string]any
		if json.Unmarshal([]byte(data), &msg) != nil {
			// one bad record is skipped and counted, as in the line formats; the rest of the session is stored
			p.skips = append(p.skips, fmt.Sprintf("%s#message:%s", j.path, id))
			continue
		}
		if !session.v2 {
			parts, err := db.QueryContext(ctx, `SELECT data FROM part WHERE message_id=? ORDER BY time_created,id`, id)
			if err != nil {
				t.fail(j.path, errors.New("cannot read opencode parts"))
				return
			}
			var content []any
			for parts.Next() {
				var raw string
				if err := parts.Scan(&raw); err != nil {
					parts.Close()
					t.fail(j.path, err)
					return
				}
				var part map[string]any
				if json.Unmarshal([]byte(raw), &part) != nil {
					p.skips = append(p.skips, fmt.Sprintf("%s#part:%s", j.path, id))
					continue
				}
				content = append(content, part)
			}
			err = parts.Err()
			parts.Close()
			if err != nil {
				t.fail(j.path, err)
				return
			}
			msg["content"] = content
		}
		evs := state.openCode(typ, msg, time.UnixMilli(created), session.v2)
		for index, e := range evs {
			// A message id remains the same when its completed tool state changes.
			e.Path, e.Index = db.path+"#"+id, index
			p.places = append(p.places, place(e))
			p.read++
			if it, ok := toPending(e, s.GitFlag); ok {
				p.items = append(p.items, it)
				for _, text := range it.texts {
					size += len(text)
				}
			}
		}
		if int64(size) >= chunkBytes && !flush(false) {
			return
		}
	}
	if err := rows.Err(); err != nil {
		t.fail(j.path, err)
		return
	}
	flush(true)
}

func (s *nativeState) openCode(typ string, msg map[string]any, at time.Time, v2 bool) []evidence.Event {
	harness := "opencode"
	switch typ {
	case "user":
		text := str(msg, "text")
		// Main-thread user role alone cannot distinguish CLI/UI input from
		// headless/API prompts. The stored formats provide no input provenance.
		owner := false
		if s.Subagent != "" || evidence.AgentCwd(s.Cwd) {
			s.Known = true
		}
		if !v2 {
			var out []evidence.Event
			blocks, _ := msg["content"].([]any)
			for _, block := range blocks {
				b := object(block)
				out = append(out, s.user(harness, nativeText(b), owner && b["synthetic"] != true, !s.Known, at, evidence.InputTyped)...)
			}
			return out
		}
		return s.user(harness, text, owner, !s.Known, at, evidence.InputTyped)
	case "system", "synthetic":
		return []evidence.Event{s.event(harness, evidence.KindHarnessText, str(msg, "text"), at)}
	case "compaction":
		return []evidence.Event{s.event(harness, evidence.KindCompaction, str(msg, "summary"), at)}
	case "idle":
		s.Prior = "task_complete"
		e := s.event(harness, evidence.KindHarnessText, "", at)
		e.Why = "turn ended"
		return []evidence.Event{e}
	case "assistant":
		model := object(msg["model"])
		s.Model = firstOf(str(model, "id"), str(model, "modelID"), str(msg, "modelID"), s.Model)
		var out []evidence.Event
		content, _ := msg["content"].([]any)
		finished := nativeAt(object(msg["time"]), "completed", at)
		for _, block := range content {
			b := object(block)
			switch str(b, "type") {
			case "text":
				out = append(out, s.event(harness, evidence.KindAgentText, str(b, "text"), finished))
			case "compaction":
				out = append(out, s.event(harness, evidence.KindCompaction, str(b, "summary"), at))
			case "tool":
				id, name := firstOf(str(b, "id"), str(b, "callID")), firstOf(str(b, "name"), str(b, "tool"))
				st := object(b["state"])
				times := object(b["time"])
				if !v2 {
					times = object(st["time"])
				}
				startKey, endKey := "created", "completed"
				if !v2 {
					startKey, endKey = "start", "end"
				}
				e := s.event(harness, evidence.KindToolCall, "", nativeAt(times, startKey, at))
				e.ToolID, e.ToolName, e.ToolInput = id, name, encoded(st["input"])
				out = append(out, e)
				status := str(st, "status")
				if status == "completed" || status == "error" {
					e = s.event(harness, evidence.KindToolResult, "", nativeAt(times, endKey, finished))
					e.ToolID, e.ToolName = id, name
					e.ToolOutput = firstOf(str(st, "output"), nativeText(st["content"]))
					if status == "error" {
						e.ToolOutput = errorText(st["error"])
					}
					// The state contains input and file content too. Only
					// native status sets its flag; shell output is checked
					// by the shared writer before clipping.
					e.IsError = status == "error"
					// A shell tool's result is "completed" whatever the command's exit: the code is in the metadata.
					if meta := object(st["metadata"]); meta != nil {
						if _, has := meta["exit"]; has && number(meta, "exit") != 0 {
							e.IsError = true
						}
					}
					out = append(out, e)
				}
			}
		}
		s.Prior = firstOf(str(msg, "finish"), "task_complete")
		return out
	}
	return nil
}

func errorText(v any) string {
	if m := object(v); m != nil {
		if s := firstOf(str(m, "message"), str(m, "error")); s != "" {
			return s
		}
	}
	return strings.TrimSpace(encoded(v))
}

func nativeAt(fields map[string]any, key string, fallback time.Time) time.Time {
	if n := number(fields, key); n > 0 {
		return time.UnixMilli(n)
	}
	return fallback
}
