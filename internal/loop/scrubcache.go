package loop

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// ScrubCache keeps scrubbed text by the hash of the text that was scrubbed, so a pass scrubs only what it has not
// scrubbed before: a month of events is a million rows and the scrubber takes ten minutes over them, four times a day.
//
// It is host state, a 0600 file under the data home that no run ever sees. Its key holds the scrubber's identity
// (its program and arguments), so a changed scrubber starts from nothing. A cached value is what the scrubber
// returned for that exact text, never the text itself. A cache that fails is dropped: the pass scrubs without it,
// so the cache can slow a pass down but never change what it publishes or make it fail.
type ScrubCache struct {
	path string
	fp   [sha256.Size]byte
	db   *sql.DB

	mu           sync.Mutex
	hits, misses int
	broken       bool
	failure      string
}

// Failure is why the cache was dropped during the pass ("" while it works). Never a path or a text.
func (c *ScrubCache) Failure() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failure
}

func (c *ScrubCache) drop(why string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.broken {
		c.broken, c.failure = true, why
	}
}

// cacheRetention is how long an entry no pass used stays: rows leave the window and files leave the repository.
const cacheRetention = 7 * 24 * time.Hour

// scrubberIdentity hashes the configured scrubber, its arguments and its program's bytes: a new scrubber build
// with other rules must not serve what the old one returned.
func scrubberIdentity() ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	if len(Scrubber) == 0 || Scrubber[0] == "" {
		return zero, ErrNoScrubber
	}
	exe, err := exec.LookPath(Scrubber[0])
	if err != nil {
		return zero, fmt.Errorf("%w: executable unavailable", ErrNoScrubber)
	}
	f, err := os.Open(exe)
	if err != nil {
		return zero, fmt.Errorf("%w: executable unavailable", ErrNoScrubber)
	}
	defer f.Close()
	h := sha256.New()
	for _, a := range Scrubber {
		fmt.Fprintf(h, "%d:%s\x00", len(a), a)
	}
	if _, err = io.Copy(h, f); err != nil {
		return zero, err
	}
	copy(zero[:], h.Sum(nil))
	return zero, nil
}

// OpenScrubCache opens (creating it, mode 0600) the cache at path. It returns nil and the reason when the cache
// cannot be used; a nil *ScrubCache scrubs without caching.
func OpenScrubCache(path string) (*ScrubCache, error) {
	if path == "" {
		return nil, nil
	}
	fp, err := scrubberIdentity()
	if err != nil {
		return nil, err
	}
	c := &ScrubCache{path: path, fp: fp}
	if err = c.open(); err != nil {
		// a damaged file is only a cache: start it again once
		os.Remove(path)
		os.Remove(path + "-wal")
		os.Remove(path + "-shm")
		if err = c.open(); err != nil {
			return nil, fmt.Errorf("scrub cache unavailable: %w", err)
		}
	}
	return c, nil
}

func (c *ScrubCache) open() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(c.path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	if err = os.Chmod(c.path, 0o600); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", sqliteInputURI(c.path, false))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL",
		"CREATE TABLE IF NOT EXISTS scrubbed(key BLOB PRIMARY KEY, val BLOB NOT NULL) WITHOUT ROWID",
		// the day an entry was last used lives apart from its value: a touch rewrites a small row, a prune reads an index
		"CREATE TABLE IF NOT EXISTS used(key BLOB PRIMARY KEY, day INTEGER NOT NULL) WITHOUT ROWID",
		"CREATE INDEX IF NOT EXISTS used_day ON used(day)",
		"SELECT count(*) FROM (SELECT key FROM scrubbed LIMIT 1)"} {
		if _, err = db.Exec(q); err != nil {
			db.Close()
			return err
		}
	}
	c.db = db
	return nil
}

// Stats is how many groups of texts were served from the cache and how many the scrubber had to see.
func (c *ScrubCache) Stats() (hits, misses int) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}

// Close prunes the entries nothing used for a week and closes the file; a broken cache is deleted.
func (c *ScrubCache) Close(ctx context.Context, now time.Time) {
	if c == nil || c.db == nil {
		return
	}
	if !c.broken {
		cutoff := day(now.Add(-cacheRetention))
		for _, q := range []string{"DELETE FROM scrubbed WHERE key IN (SELECT key FROM used WHERE day < ?)", "DELETE FROM used WHERE day < ?"} {
			if _, err := c.db.ExecContext(ctx, q, cutoff); err != nil {
				c.broken, c.failure = true, "the cache could not be pruned"
				break
			}
		}
	}
	c.db.Close()
	c.db = nil
	if c.broken {
		os.Remove(c.path)
		os.Remove(c.path + "-wal")
		os.Remove(c.path + "-shm")
	}
}

func day(t time.Time) int64 { return t.Unix() / 86400 * 86400 }

func (c *ScrubCache) key(ns string, group []string) []byte {
	h := sha256.New()
	h.Write(c.fp[:])
	fmt.Fprintf(h, "%d:%s", len(ns), ns)
	var n [8]byte
	for _, s := range group {
		binary.LittleEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		io.WriteString(h, s)
	}
	return h.Sum(nil)
}

func packGroup(group []string) []byte {
	var out []byte
	for _, s := range group {
		out = binary.AppendUvarint(out, uint64(len(s)))
		out = append(out, s...)
	}
	return out
}

func unpackGroup(b []byte, n int) ([]string, bool) {
	out := make([]string, 0, n)
	for len(out) < n {
		l, k := binary.Uvarint(b)
		if k <= 0 || uint64(len(b)-k) < l {
			return nil, false
		}
		out = append(out, string(b[k:k+int(l)]))
		b = b[k+int(l):]
	}
	return out, len(b) == 0
}

// RedactGroups scrubs texts like RedactAll, in groups of stride consecutive texts that are cached as one unit
// (a table row: its id and columns). ns names what kind of text it is. Texts a group cached before come from the
// cache after the scrubber's identity matched; the others go to the scrubber, in order, and are cached. The
// result equals RedactAll's for the same texts. A nil cache scrubs everything.
func (c *ScrubCache) RedactGroups(ctx context.Context, ns string, texts []string, stride int) ([]string, error) {
	if stride <= 0 || len(texts)%stride != 0 {
		return nil, fmt.Errorf("scrub groups need a stride that divides the texts")
	}
	if c == nil {
		return RedactAll(ctx, texts)
	}
	c.mu.Lock()
	usable := c.db != nil && !c.broken
	if !usable {
		c.misses += len(texts) / stride
	}
	c.mu.Unlock()
	if !usable {
		return RedactAll(ctx, texts)
	}
	groups := len(texts) / stride
	out := make([]string, len(texts))
	keys := make([][]byte, groups)
	have := make([]bool, groups)
	var miss []int
	today := day(time.Now())
	tx, err := c.db.BeginTx(ctx, nil)
	if err == nil {
		var sel, touch *sql.Stmt
		sel, err = tx.PrepareContext(ctx, "SELECT val FROM scrubbed WHERE key=?")
		if err == nil {
			touch, err = tx.PrepareContext(ctx, "UPDATE used SET day=? WHERE key=? AND day<?")
		}
		for g := 0; err == nil && g < groups; g++ {
			group := texts[g*stride : (g+1)*stride]
			keys[g] = c.key(ns, group)
			var val []byte
			switch e := sel.QueryRowContext(ctx, keys[g]).Scan(&val); {
			case e == sql.ErrNoRows:
				miss = append(miss, g)
			case e != nil:
				err = e
			default:
				if clean, ok := unpackGroup(val, stride); ok {
					copy(out[g*stride:], clean)
					have[g] = true
					_, err = touch.ExecContext(ctx, today, keys[g], today)
				} else {
					miss = append(miss, g)
				}
			}
		}
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err() // a pass told to stop is not a failure of the cache
		}
		// a cache that fails is dropped: scrub without it
		c.drop("the cache failed while reading")
		c.mu.Lock()
		c.misses += groups
		c.mu.Unlock()
		return RedactAll(ctx, texts)
	}
	if len(miss) > 0 {
		send := make([]string, 0, len(miss)*stride)
		for _, g := range miss {
			send = append(send, texts[g*stride:(g+1)*stride]...)
		}
		clean, err := RedactAll(ctx, send)
		if err != nil {
			return nil, err
		}
		for i, g := range miss {
			copy(out[g*stride:], clean[i*stride:(i+1)*stride])
		}
		if err = c.store(ctx, keys, miss, out, stride, today); err != nil && ctx.Err() == nil {
			c.drop("the cache failed while writing")
		}
	}
	c.mu.Lock()
	c.hits += groups - len(miss)
	c.misses += len(miss)
	c.mu.Unlock()
	return out, nil
}

func (c *ScrubCache) store(ctx context.Context, keys [][]byte, miss []int, out []string, stride int, today int64) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, g := range miss {
		if _, err = tx.ExecContext(ctx, "INSERT OR REPLACE INTO scrubbed(key,val) VALUES(?,?)", keys[g], packGroup(out[g*stride:(g+1)*stride])); err != nil {
			tx.Rollback()
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT OR REPLACE INTO used(key,day) VALUES(?,?)", keys[g], today); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// redactGroups is RedactGroups for a possibly nil cache, with the text's kind named for the key.
func redactGroups(ctx context.Context, c *ScrubCache, ns string, texts []string, stride int) ([]string, error) {
	if c == nil {
		return RedactAll(ctx, texts)
	}
	return c.RedactGroups(ctx, ns, texts, stride)
}
