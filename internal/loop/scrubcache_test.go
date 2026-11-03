package loop

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// countingScrubber masks the marker and appends a line to a file for every start, so a test counts scrubber runs.
func countingScrubber(t *testing.T, mask string) string {
	t.Helper()
	count := filepath.Join(t.TempDir(), "starts")
	inputScrubber(t, `echo run >> '`+count+`'; sed -e 's/AKIA[A-Z0-9]*EXAMPLE/[masked]/g' -e 's/marker-7f3a/`+mask+`/g'`)
	return count
}

func starts(t *testing.T, count string) int {
	t.Helper()
	b, err := os.ReadFile(count)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "run")
}

func cacheEvents(n int) []store.Event {
	end := time.Date(2025, 4, 10, 12, 0, 0, 0, time.UTC)
	var evs []store.Event
	for i := 0; i < n; i++ {
		evs = append(evs, store.Event{Source: "fixture", SourceKey: "k" + string(rune('a'+i)), TS: end.Add(-time.Hour * time.Duration(i+1)), Kind: "record",
			Summary: "row marker-7f3a " + string(rune('a'+i))})
	}
	return evs
}

func copyOnce(t *testing.T, src string, cache *ScrubCache) (StoreManifest, []string) {
	t.Helper()
	end := time.Date(2025, 4, 10, 12, 0, 0, 0, time.UTC)
	dst := filepath.Join(t.TempDir(), "store.sqlite")
	m, err := StoreCopy(context.Background(), StoreCopyOptions{Source: src, Destination: dst, Until: end, Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := inputPlainDB(t, dst).Query("SELECT id || '|' || summary FROM events ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return m, out
}

func TestScrubCacheScrubsOnlyNewOrChangedRows(t *testing.T) {
	count := countingScrubber(t, "[masked]")
	ctx := context.Background()
	src := inputStore(t, cacheEvents(6))
	path := filepath.Join(t.TempDir(), "data", "scrub-cache.sqlite")
	cache, err := OpenScrubCache(path)
	if err != nil || cache == nil {
		t.Fatal(err)
	}
	first, rowsFirst := copyOnce(t, src, cache)
	if first.Scrubbed != 6 || first.Cached != 0 || first.Masked != 6 {
		t.Fatalf("cold copy %+v", first)
	}
	before := starts(t, count)
	second, rowsSecond := copyOnce(t, src, cache)
	// a warm copy serves every row from the cache: the scrubber starts once, for the canary
	if second.Scrubbed != 0 || second.Cached != 6 || starts(t, count)-before != 1 {
		t.Fatalf("warm copy %+v, scrubber starts %d", second, starts(t, count)-before)
	}
	if strings.Join(rowsFirst, "\n") != strings.Join(rowsSecond, "\n") || strings.Contains(strings.Join(rowsSecond, ""), "marker-7f3a") {
		t.Fatalf("cached copy differs or leaks:\n%v\n%v", rowsFirst, rowsSecond)
	}
	// one changed row and one new row are all the scrubber sees
	db, err := sql.Open("sqlite", src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE events SET summary='changed marker-7f3a' WHERE source_key='ka'"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err := store.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.Put(ctx, []store.Event{{Source: "fixture", SourceKey: "new", TS: time.Date(2025, 4, 10, 11, 30, 0, 0, time.UTC), Kind: "record", Actor: store.ActorUnknown, Summary: "new marker-7f3a"}}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	third, rowsThird := copyOnce(t, src, cache)
	if third.Scrubbed != 2 || third.Cached != 5 || len(rowsThird) != 7 || strings.Contains(strings.Join(rowsThird, ""), "marker-7f3a") {
		t.Fatalf("incremental copy %+v rows %d", third, len(rowsThird))
	}
	cache.Close(ctx, time.Now())
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cache file mode: %v %v", info, err)
	}
	// the cache holds scrubbed text and hashes, never the marker
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "marker-7f3a") {
		t.Fatal("the cache file holds unscrubbed text")
	}
}

func TestScrubCacheKeysOnTheScrubber(t *testing.T) {
	countingScrubber(t, "[masked]")
	src := inputStore(t, cacheEvents(3))
	path := filepath.Join(t.TempDir(), "scrub-cache.sqlite")
	cache, err := OpenScrubCache(path)
	if err != nil {
		t.Fatal(err)
	}
	copyOnce(t, src, cache)
	cache.Close(context.Background(), time.Now())
	// another scrubber (other rules) must not be served what the first returned
	countingScrubber(t, "[other]")
	cache, err = OpenScrubCache(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close(context.Background(), time.Now())
	m, rows := copyOnce(t, src, cache)
	if m.Cached != 0 || m.Scrubbed != 3 || !strings.Contains(strings.Join(rows, ""), "[other]") {
		t.Fatalf("a changed scrubber was served from the cache: %+v %v", m, rows)
	}
}

func TestScrubCacheDamageNeverStopsAPass(t *testing.T) {
	countingScrubber(t, "[masked]")
	src := inputStore(t, cacheEvents(3))
	path := filepath.Join(t.TempDir(), "scrub-cache.sqlite")
	write(t, path, "this is not a database, and it is long enough to be taken for one by nothing at all")
	cache, err := OpenScrubCache(path)
	if err != nil || cache == nil {
		t.Fatalf("a damaged cache file was not started again: %v", err)
	}
	defer cache.Close(context.Background(), time.Now())
	m, rows := copyOnce(t, src, cache)
	if m.Scrubbed != 3 || len(rows) != 3 || strings.Contains(strings.Join(rows, ""), "marker-7f3a") {
		t.Fatalf("copy through a restarted cache: %+v %v", m, rows)
	}
	// no cache at all is the plain path
	if m, rows = copyOnce(t, src, nil); m.Scrubbed != 0 || m.Cached != 0 || len(rows) != 3 {
		t.Fatalf("copy without a cache: %+v", m)
	}
}

func TestScrubCachePrunesWhatNothingUsed(t *testing.T) {
	countingScrubber(t, "[masked]")
	path := filepath.Join(t.TempDir(), "scrub-cache.sqlite")
	cache, err := OpenScrubCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cache.RedactGroups(context.Background(), "x", []string{"a marker-7f3a", "b"}, 1); err != nil {
		t.Fatal(err)
	}
	// a pass long after: nothing used the entries for more than the retention
	cache.Close(context.Background(), time.Now().Add(2*cacheRetention))
	db, err := sql.Open("sqlite", sqliteInputURI(path, false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err = db.QueryRow("SELECT count(*) FROM scrubbed").Scan(&n); err != nil || n != 0 {
		t.Fatalf("entries left after the retention: %d %v", n, err)
	}
}
