package source

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/agent-sh/tacit/client/pkg/evidence"
)

// hermesRead keeps the reader and the digests on the rows of one source read transaction. The shared reader
// exposes transformed events, not the raw row; an immutable SQLite snapshot gives both the same raw version.
type hermesRead struct {
	db       *evidence.HermesDB
	snapshot string
	dir      string
}

func (r *hermesRead) Close() {
	r.db.Close()
	os.RemoveAll(r.dir)
}

func snapshotHermes(ctx context.Context, path, scratch string) (*hermesRead, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(scratch, "momentum-hermes-")
	if err != nil {
		return nil, err
	}
	snapshot := filepath.Join(dir, "rows.db")
	complete := false
	defer func() {
		if !complete {
			os.RemoveAll(dir)
		}
	}()
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: "mode=ro&_pragma=busy_timeout(2000)"}
	source, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	defer source.Close()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// VACUUM INTO reads the source in one transaction, including its WAL, and writes only the throwaway copy.
	if _, err := source.ExecContext(cctx, "VACUUM INTO ?", snapshot); err != nil {
		return nil, err
	}
	db, err := evidence.OpenHermes(snapshot)
	if err != nil {
		return nil, err
	}
	// References and the reader's saved state keep the live database's identity, while its handle reads the copy.
	db.Path = abs
	complete = true
	return &hermesRead{db: db, snapshot: snapshot, dir: dir}, nil
}

// Hermes rewrites a message row in place (a turn's prologue rewrites the prompt it stored, a prune rewrites old
// rows), so the row a Hermes event names by its id may no longer be the one the ingest read. The ingest keeps, with
// each Hermes event, a digest of its row's content and tool calls as they were when it read the row (the event's
// meta, under HermesDigestKey), and evidence show reads the row only in that version (it withholds the rest in an
// evaluation).

// HermesDigestKey is the key of an event's meta that holds its Hermes row's digest.
const HermesDigestKey = "row_digest"

// HermesRowDigest is the digest an ingest keeps of a Hermes message row: its content and its tool calls, as text.
func HermesRowDigest(content, toolCalls string) string {
	h := sha256.New()
	h.Write([]byte(content))
	h.Write([]byte{0})
	h.Write([]byte(toolCalls))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// hermesDigests reads the digests of the rows an ingest stores events of, once each, through a read-only handle of
// each store it meets.
type hermesDigests struct {
	mu   sync.Mutex
	dbs  map[string]*sql.DB
	seen map[string]string // "path\x00id" -> digest ("" when the row could not be read)
}

// of is the digest of row id in the immutable snapshot, or "" when it cannot be read: the event then keeps none, and
// an evaluation withholds its row, since a rewrite since cannot be told.
func (d *hermesDigests) of(ctx context.Context, path string, id int64) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := path + "\x00" + strconv.FormatInt(id, 10)
	if v, ok := d.seen[key]; ok {
		return v
	}
	db, ok := d.dbs[path]
	if !ok {
		if abs, err := filepath.Abs(path); err == nil {
			u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: "mode=ro&_pragma=busy_timeout(2000)&_pragma=query_only(1)"}
			db, _ = sql.Open("sqlite", u.String())
		}
		if d.dbs == nil {
			d.dbs = map[string]*sql.DB{}
		}
		d.dbs[path] = db
	}
	digest := ""
	if db != nil {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		has := false
		if rows, err := db.QueryContext(cctx, `SELECT 1 FROM pragma_table_info('messages') WHERE name = 'tool_calls'`); err == nil {
			has = rows.Next()
			rows.Close()
		}
		calls := "''"
		if has {
			calls = "coalesce(tool_calls, '')"
		}
		var content, toolCalls string
		if err := db.QueryRowContext(cctx, `SELECT coalesce(content, ''), `+calls+` FROM messages WHERE id = ?`, id).Scan(&content, &toolCalls); err == nil {
			digest = HermesRowDigest(content, toolCalls)
		}
	}
	if d.seen == nil {
		d.seen = map[string]string{}
	}
	d.seen[key] = digest
	return digest
}

// close ends the handles.
func (d *hermesDigests) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, db := range d.dbs {
		if db != nil {
			db.Close()
		}
	}
}
