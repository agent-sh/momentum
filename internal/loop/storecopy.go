package loop

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

const StoreWindow = 30 * 24 * time.Hour

type StoreCopyOptions struct {
	Source      string
	Destination string
	ScratchDir  string
	Since       time.Time
	Until       time.Time
	OmitInPlace bool
	// Master says Source is a copy this function made for a wider window (scrubbed already): the rows of this
	// window are taken from it and are not scrubbed again. A scrubber start costs most of a second and a month of
	// events is a gigabyte of text, so an evaluation scrubs it once for all its tasks.
	Master bool
	// Cache, when set, serves the rows it scrubbed before (by the hash of the row's text) so a pass scrubs only new or
	// changed rows. The caller owns it and keeps it out of every run.
	Cache *ScrubCache
}

type StoreManifest struct {
	Since  time.Time `json:"since"`
	Until  time.Time `json:"until"`
	Events int       `json:"events"`
	Notes  int       `json:"notes"`
	Bytes  int64     `json:"bytes"`
	Canary bool      `json:"canary"`
	// Rows the scrubber saw in this copy and rows served from the persistent scrub cache (both zero without one).
	Scrubbed int `json:"scrubbed,omitempty"`
	Cached   int `json:"cached,omitempty"`
	// Masked counts the rows whose text the scrubber changed (counts only, never what).
	Masked int `json:"masked,omitempty"`
	// CacheError says why the persistent scrub cache was not used (the pass scrubbed every row).
	CacheError string `json:"cache_error,omitempty"`
}

// StoreCopy makes a scrubbed plain SQLite file for /ro/store.sqlite. Nothing in
// the copy comes from a case, a run or a package. Only events and notes remain;
// the scratch store, including its WAL, is removed before return.
func StoreCopy(ctx context.Context, o StoreCopyOptions) (m StoreManifest, err error) {
	if err = CheckScrubber(ctx); err != nil {
		return m, err
	}
	if o.Until.IsZero() {
		return m, fmt.Errorf("store copy needs an end time")
	}
	if o.Since.IsZero() {
		o.Since = o.Until.Add(-StoreWindow)
	}
	if err = absentInput(o.Destination); err != nil {
		return m, err
	}
	scratchRoot := o.ScratchDir
	if scratchRoot == "" {
		scratchRoot = filepath.Dir(o.Destination)
	}
	tmp, err := os.MkdirTemp(scratchRoot, ".store-input-")
	if err != nil {
		return m, err
	}
	defer os.RemoveAll(tmp)
	var hits0, misses0, masked int
	snapshot := filepath.Join(tmp, "scratch.sqlite")
	if o.Master {
		if err = copyPlainFile(o.Source, snapshot); err != nil {
			return m, err
		}
	} else if err = store.AsOf(ctx, o.Source, snapshot, o.Since, o.Until, o.OmitInPlace); err != nil {
		return m, err
	}
	db, err := sql.Open("sqlite", sqliteInputURI(snapshot, false))
	if err != nil {
		return m, err
	}
	defer db.Close()
	if o.Master {
		for _, q := range []string{"DELETE FROM events WHERE ts < ? OR ts >= ?", "DELETE FROM notes WHERE at >= ?"} {
			args := []any{o.Until.UnixMilli()}
			if strings.Contains(q, "events") {
				args = []any{o.Since.UnixMilli(), o.Until.UnixMilli()}
			}
			if _, err = db.ExecContext(ctx, q, args...); err != nil {
				return m, err
			}
		}
		if _, err = db.ExecContext(ctx, "UPDATE notes SET done = 0 WHERE done >= ?", o.Until.UnixMilli()); err != nil {
			return m, err
		}
	}
	if _, err = db.ExecContext(ctx, "DELETE FROM notes WHERE at < ?", o.Since.UnixMilli()); err != nil {
		return m, err
	}
	if o.Master {
		goto scrubbed
	}
	hits0, misses0 = o.Cache.Stats()
	if masked, err = scrubTable(ctx, db, o.Cache, "events", []string{"source", "source_key", "session", "agent", "harness", "model", "cwd",
		"actor", "kind", "tool", "summary", "meta", "ref", "experiment"}); err != nil {
		return m, err
	}
	if n, err := scrubTable(ctx, db, o.Cache, "notes", []string{"kind", "title", "detail", "effect", "links", "source"}); err != nil {
		return m, err
	} else {
		masked += n
	}
scrubbed:
	m = StoreManifest{Since: o.Since, Until: o.Until, Canary: true, Masked: masked}
	if hits1, misses1 := o.Cache.Stats(); o.Cache != nil {
		m.Cached, m.Scrubbed = hits1-hits0, misses1-misses0
	}
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&m.Events); err != nil {
		return m, err
	}
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM notes").Scan(&m.Notes); err != nil {
		return m, err
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT IN ('events','notes') AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return m, err
	}
	var tables []string
	for rows.Next() {
		var table string
		if err = rows.Scan(&table); err != nil {
			rows.Close()
			return m, err
		}
		tables = append(tables, table)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return m, err
	}
	for _, table := range tables {
		if _, err = db.ExecContext(ctx, `DROP TABLE "`+strings.ReplaceAll(table, `"`, `""`)+`"`); err != nil {
			return m, err
		}
	}
	plain := filepath.Join(tmp, "plain.sqlite")
	if _, err = db.ExecContext(ctx, `VACUUM INTO ?`, plain); err != nil {
		return m, err
	}
	// VACUUM INTO writes a file in rollback-journal mode already (measured: the header says so), so this pragma is a
	// guard that changes nothing today: whatever mode the file has, it ends in DELETE before it is made read-only, so
	// a read-only connection needs no sidecar.
	final, err := sql.Open("sqlite", sqliteInputURI(plain, false))
	if err != nil {
		return m, err
	}
	_, err = final.ExecContext(ctx, "PRAGMA journal_mode=DELETE")
	closeErr := final.Close()
	if err != nil {
		return m, err
	}
	if closeErr != nil {
		return m, closeErr
	}
	info, err := os.Stat(plain)
	if err != nil {
		return m, err
	}
	m.Bytes = info.Size()
	if err = os.Chmod(plain, 0o444); err != nil {
		return m, err
	}
	// Link publishes exclusively, so a concurrent caller cannot be overwritten.
	if err = os.Link(plain, o.Destination); err != nil {
		return m, err
	}
	return m, nil
}

func sqliteInputURI(path string, readOnly bool) string {
	query := ""
	if readOnly {
		query = "mode=ro&immutable=1"
	}
	return (&url.URL{Scheme: "file", Path: path, RawQuery: query}).String()
}

func absentInput(path string) error {
	if path == "" {
		return fmt.Errorf("input destination is empty")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return fmt.Errorf("input destination must be absent")
	}
	return nil
}

// A scrubber start costs most of a second whatever it is given, so a page is as large as one start can take: many rows,
// and some megabytes of text (RedactAll splits at its own limit).
const (
	scrubPageRows  = 5000
	scrubPageBytes = 4 << 20
)

// scrubTable keeps only one batch of rows in memory. Stable ids and numeric
// columns stay intact; every other column is passed through the scrubber.
func scrubTable(ctx context.Context, db *sql.DB, cache *ScrubCache, table string, cols []string) (masked int, err error) {
	last := ""
	for {
		rows, err := db.QueryContext(ctx, `SELECT id, `+strings.Join(cols, ",")+` FROM `+table+` WHERE id > ? ORDER BY id LIMIT ?`, last, scrubPageRows)
		if err != nil {
			return masked, err
		}
		var ids, texts []string
		size := 0
		for rows.Next() {
			if size >= scrubPageBytes {
				break // the page is full; the next starts after its last row
			}
			values := make([]string, len(cols)+1)
			ptrs := make([]any, len(values))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err = rows.Scan(ptrs...); err != nil {
				rows.Close()
				return masked, err
			}
			// the id goes through the scrubber too: a row whose id it would change is left out of the copy
			ids, texts = append(ids, values[0]), append(append(texts, values[0]), values[1:]...)
			for _, v := range values {
				size += len(v)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil || len(ids) == 0 {
			return masked, err
		}
		stride := len(cols) + 1
		clean, err := redactGroups(ctx, cache, table, texts, stride)
		if err != nil {
			return masked, err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return masked, err
		}
		assign := make([]string, len(cols))
		for i, col := range cols {
			assign[i] = col + "=?"
		}
		for i, id := range ids {
			changed := false
			for j := 0; j < stride; j++ {
				if clean[i*stride+j] != strings.ReplaceAll(texts[i*stride+j], "\x1e", " ") {
					changed = true
				}
			}
			if changed {
				masked++
			}
			if clean[i*stride] != id {
				if _, err = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE id=?", id); err != nil {
					tx.Rollback()
					return masked, err
				}
				continue
			}
			args := make([]any, len(cols)+1)
			write := false
			for j, col := range cols {
				text := clean[i*stride+1+j]
				if col == "meta" && !json.Valid([]byte(text)) {
					text = "{}"
				}
				if col == "links" && !json.Valid([]byte(text)) {
					text = "[]"
				}
				if text != texts[i*stride+1+j] {
					write = true
				}
				args[j] = text
			}
			if !write {
				continue // the row is already its own scrubbed copy
			}
			args[len(cols)] = id
			if _, err = tx.ExecContext(ctx, "UPDATE "+table+" SET "+strings.Join(assign, ",")+" WHERE id=?", args...); err != nil {
				tx.Rollback()
				return masked, err
			}
		}
		if err = tx.Commit(); err != nil {
			return masked, err
		}
		last = ids[len(ids)-1]
	}
}

// copyPlainFile copies a master into a new file of the caller's (the master is read-only).
func copyPlainFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
