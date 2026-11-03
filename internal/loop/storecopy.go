package loop

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
}

type StoreManifest struct {
	Since  time.Time `json:"since"`
	Until  time.Time `json:"until"`
	Events int       `json:"events"`
	Notes  int       `json:"notes"`
	Bytes  int64     `json:"bytes"`
	Canary bool      `json:"canary"`
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
	snapshot := filepath.Join(tmp, "scratch.sqlite")
	if err = store.AsOf(ctx, o.Source, snapshot, o.Since, o.Until, o.OmitInPlace); err != nil {
		return m, err
	}
	db, err := sql.Open("sqlite", sqliteInputURI(snapshot, false))
	if err != nil {
		return m, err
	}
	defer db.Close()
	if _, err = db.ExecContext(ctx, "DELETE FROM notes WHERE at < ?", o.Since.UnixMilli()); err != nil {
		return m, err
	}
	if err = scrubTable(ctx, db, "events", []string{"source", "source_key", "session", "agent", "harness", "model", "cwd",
		"actor", "kind", "tool", "summary", "meta", "ref", "experiment"}); err != nil {
		return m, err
	}
	if err = scrubTable(ctx, db, "notes", []string{"kind", "title", "detail", "effect", "links", "source"}); err != nil {
		return m, err
	}
	m = StoreManifest{Since: o.Since, Until: o.Until, Canary: true}
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

// scrubTable keeps only one batch of rows in memory. Stable ids and numeric
// columns stay intact; every other column is passed through the scrubber.
func scrubTable(ctx context.Context, db *sql.DB, table string, cols []string) error {
	last := ""
	for {
		rows, err := db.QueryContext(ctx, `SELECT id, `+strings.Join(cols, ",")+` FROM `+table+` WHERE id > ? ORDER BY id LIMIT 128`, last)
		if err != nil {
			return err
		}
		var ids, texts []string
		for rows.Next() {
			values := make([]string, len(cols)+1)
			ptrs := make([]any, len(values))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err = rows.Scan(ptrs...); err != nil {
				rows.Close()
				return err
			}
			ids, texts = append(ids, values[0]), append(texts, values[1:]...)
		}
		err = rows.Err()
		rows.Close()
		if err != nil || len(ids) == 0 {
			return err
		}
		clean, err := RedactAll(ctx, texts)
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		assign := make([]string, len(cols))
		for i, col := range cols {
			assign[i] = col + "=?"
		}
		for i, id := range ids {
			args := make([]any, len(cols)+1)
			for j, col := range cols {
				text := clean[i*len(cols)+j]
				if col == "meta" && !json.Valid([]byte(text)) {
					text = "{}"
				}
				if col == "links" && !json.Valid([]byte(text)) {
					text = "[]"
				}
				args[j] = text
			}
			args[len(cols)] = id
			if _, err = tx.ExecContext(ctx, "UPDATE "+table+" SET "+strings.Join(assign, ",")+" WHERE id=?", args...); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		last = ids[len(ids)-1]
	}
}
