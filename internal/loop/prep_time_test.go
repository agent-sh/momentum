package loop

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A records child is one scrubber start (about half a second), and a run asks for up to 200 records: consecutive
// events share a child, ten at a time, and each record is kept or withheld exactly as its own child would decide.
func TestRecordsShareChildrenTenEventsAtATime(t *testing.T) {
	goodInputScrubber(t)
	at := time.Date(2025, 4, 10, 12, 0, 0, 0, time.UTC)
	var events []store.Event
	for i := range 25 {
		events = append(events, store.Event{Source: "fixture", SourceKey: strconv.Itoa(i), TS: at, Kind: "record"})
	}
	calls := filepath.Join(t.TempDir(), "calls")
	withheld := events[3].ID()
	o := recordFixture(t, events, `printf 'call\n' >> '`+calls+`'
show() { case "$1" in `+withheld+`) printf '(record withheld: fixture)\n';; *) printf 'record %s\n' "$1";; esac; }
if test "$3" = --separate; then
	test "$4" = --bytes && test "$6" = -- || exit 8
	shift 6
	sep=''
	for id in "$@"; do printf "$sep"; show "$id"; sep='\036'; done
	exit 0
fi
show "$5"`)
	o.Neighbours = -1
	for _, e := range events {
		o.IDs = append(o.IDs, e.ID())
	}
	m, err := Records(context.Background(), o)
	if err != nil || m.Written != 24 || m.Withheld != 1 || m.Skipped != 0 {
		t.Fatalf("manifest %+v %v", m, err)
	}
	for i, file := range m.Files {
		want := events[i].ID()
		if i >= 3 {
			want = events[i+1].ID()
		}
		if file.Event != want || readText(t, filepath.Join(o.Destination, file.Name)) != "record "+want+"\n" {
			t.Fatalf("record %d is not its own child's output: %+v", i, file)
		}
	}
	if n := strings.Count(readText(t, calls), "call\n"); n != 3 {
		t.Fatalf("25 events took %d children, want 3", n)
	}
}

// The pass's store copy, configuration copy and snapshots read different sources, so they run at the same time: some
// scrubber calls of different steps overlap. Each step's manifest counts stay its own.
func TestPassInputStepsOverlap(t *testing.T) {
	r, _ := inputRunner(t)
	r.Inputs.ReadDirs = []string{inputRepo(t, t.TempDir(), "example")}
	write(t, filepath.Join(r.Inputs.Home, ".config", "agents", "rules.md"), "rule")
	log := meetingScrubber(t)
	r.Inputs.ScrubCache = filepath.Join(t.TempDir(), "cache.sqlite")
	r.pass = &passInputs{}
	defer r.pass.close()
	if err := r.prepareInputs(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := r.pass.manifest.Store
	if s.Cached+s.Scrubbed != s.Events+s.Notes {
		t.Fatalf("store counts hold other steps' texts: %+v", s)
	}
	if !callsMet(log) {
		t.Fatal("the steps' scrubber calls ran one after another")
	}
}

// meetingScrubber is the good input scrubber whose calls (all but canaries) wait, up to three seconds, for another
// call to be open at the same time. The first that finds one marks the meeting, and every call after it goes on at
// once. It returns the mark's path.
func meetingScrubber(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	open, met := filepath.Join(dir, "open"), filepath.Join(dir, "met")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	inputScrubber(t, `in=$(cat; printf x); in=${in%x}
case "$in" in *AKIA*) ;; *)
	: > '`+open+`'/$$
	n=0
	while [ $n -lt 60 ] && [ ! -e '`+met+`' ]; do
		if [ $(ls '`+open+`' | wc -l) -ge 2 ]; then : > '`+met+`'; break; fi
		sleep 0.05; n=$((n+1))
	done
	rm -f '`+open+`'/$$;;
esac
printf '%s' "$in" | sed -e 's/AKIA[A-Z0-9]*EXAMPLE/[masked]/g' -e 's/marker-7f3a/[masked]/g'`)
	return met
}

// callsMet reports whether two scrubber calls were open at the same time.
func callsMet(met string) bool {
	_, err := os.Stat(met)
	return err == nil
}

// A cold store copy is hundreds of pages of seconds each: pages are with the scrubber at the same time, and every row
// is still scrubbed and written back once.
func TestStoreCopyScrubsPagesAtTheSameTime(t *testing.T) {
	log := meetingScrubber(t)
	at := time.Now().Add(-time.Hour)
	var events []store.Event
	for i := range 3*scrubPageRows + 7 {
		events = append(events, store.Event{Source: "fixture", SourceKey: strconv.Itoa(i), TS: at, Kind: "record", Summary: "row marker-7f3a " + strconv.Itoa(i)})
	}
	src := inputStore(t, events)
	cache, err := OpenScrubCache(filepath.Join(t.TempDir(), "cache.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close(context.Background(), time.Now())
	dst := filepath.Join(t.TempDir(), "store.sqlite")
	m, err := StoreCopy(context.Background(), StoreCopyOptions{Source: src, Destination: dst, Until: time.Now(), Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if m.Events != len(events) || m.Scrubbed != len(events) || m.Masked != len(events) {
		t.Fatalf("counts %+v", m)
	}
	db := inputPlainDB(t, dst)
	var left int
	if err := db.QueryRow("SELECT count(*) FROM events WHERE summary NOT LIKE 'row [masked] %'").Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d rows were not scrubbed: %v", left, err)
	}
	if !callsMet(log) {
		t.Fatal("the store's pages were scrubbed one after another")
	}
}

// The store's counts are its own rows even while other steps use the cache at the same time.
func TestStoreCopyCountsOnlyItsOwnRows(t *testing.T) {
	goodInputScrubber(t)
	at := time.Now().Add(-time.Hour)
	var events []store.Event
	for i := range 30 {
		events = append(events, store.Event{Source: "fixture", SourceKey: strconv.Itoa(i), TS: at, Kind: "record", Summary: "row " + strconv.Itoa(i)})
	}
	src := inputStore(t, events)
	cache, err := OpenScrubCache(filepath.Join(t.TempDir(), "cache.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close(context.Background(), time.Now())
	ctx, stop := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ctx.Err() == nil; i++ {
			cache.RedactGroups(ctx, "file", []string{"other " + strconv.Itoa(i)}, 1)
		}
	}()
	dir := t.TempDir()
	m, err := StoreCopy(context.Background(), StoreCopyOptions{Source: src, Destination: filepath.Join(dir, "store.sqlite"), Until: time.Now(), Cache: cache})
	stop()
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if m.Cached != 0 || m.Scrubbed != m.Events+m.Notes {
		t.Fatalf("counts %+v", m)
	}
}
