package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions/state"
)

// Activity and events exclude the loop's directory and every descendant, not a sibling with the same prefix.
// Prefix characters are literal. Both windows include their start and exclude their end.
func TestActivityAndEventsRespectDirectoryAndTimeBounds(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	evs := []Event{
		{Source: Transcripts, Kind: "tool_result", Actor: ActorTool, SourceKey: "start", Session: "start", TS: start, Cwd: "/work/project"},
		{Source: Transcripts, Kind: "tool_result", Actor: ActorTool, SourceKey: "end", Session: "end", TS: end, Cwd: "/work/project"},
		{Source: Transcripts, Kind: "tool_result", Actor: ActorTool, SourceKey: "loop", Session: "loop", TS: start, Cwd: "/work/loop_%"},
		{Source: Transcripts, Kind: "tool_result", Actor: ActorTool, SourceKey: "child", Session: "child", TS: start, Cwd: "/work/loop_%/runs/a"},
		{Source: Transcripts, Kind: "tool_result", Actor: ActorTool, SourceKey: "sibling", Session: "sibling", TS: start, Cwd: "/work/loop_%2"},
		{Source: Transcripts, Kind: "tool_result", Actor: ActorTool, SourceKey: "unknown", Session: "unknown", TS: start},
	}
	if _, err := st.Put(ctx, evs); err != nil {
		t.Fatal(err)
	}
	base, err := st.Activity(ctx, ActivityQuery{Since: start, Until: end, BySession: true, ExcludeCwd: "/work/loop_%/"})
	if err != nil || len(base) != 3 {
		t.Fatalf("activity: %+v %v", base, err)
	}
	for _, e := range evs {
		want := e.SourceKey == "start" || e.SourceKey == "sibling" || e.SourceKey == "unknown"
		if base[UnitOf(e, true)] != want {
			t.Errorf("activity for %s: %v", e.SourceKey, base)
		}
	}
	got, err := st.Events(ctx, Query{Since: start, Until: end, ExcludeCwd: "/work/loop_%"})
	if err != nil || len(got) != 3 {
		t.Fatalf("events: %+v %v", got, err)
	}
	for _, e := range got {
		if e.SourceKey != "start" && e.SourceKey != "sibling" && e.SourceKey != "unknown" {
			t.Errorf("excluded event: %+v", e)
		}
	}
}

// A loop entered through an alias records its resolved working directory. Both forms and their descendants
// are excluded from hits, activity and source checks, while a sibling is still counted.
func TestTheLoopDirectoryIsExcludedThroughItsSymlink(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	real := filepath.Join(t.TempDir(), "loop_%")
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	var evs []Event
	for i, cwd := range []string{real, filepath.Join(real, "runs", "one"), alias, filepath.Join(alias, "runs", "two"), real + "-sibling"} {
		evs = append(evs, Event{Source: Transcripts, SourceKey: fmt.Sprint(i), Session: fmt.Sprint(i), Actor: ActorTool, Kind: "tool_result", TS: start, Cwd: cwd})
	}
	if _, err := st.Put(ctx, evs); err != nil {
		t.Fatal(err)
	}
	q := ActivityQuery{Since: start, Until: end, BySession: true, ExcludeCwd: alias}
	base, err := st.Activity(ctx, q)
	if err != nil || len(base) != 1 || !base[UnitOf(evs[4], true)] {
		t.Fatalf("activity: %+v %v", base, err)
	}
	got, err := st.Events(ctx, Query{Since: start, Until: end, ExcludeCwd: alias})
	if err != nil || len(got) != 1 || got[0].Cwd != real+"-sibling" {
		t.Fatalf("events: %+v %v", got, err)
	}
}

// Readiness uses successful run starts, not event times or write times. An equal boundary is not past it,
// a quiet successful read is sufficient, and a different source's activity has its own requirement.
func TestWindowReadinessUsesEverySourcesSuccessfulRead(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	ev := Event{Source: Sessions, SourceKey: "phase", Session: "one", Actor: ActorHarness, Kind: "phase", TS: start}
	if _, err := st.Put(ctx, []Event{ev}); err != nil {
		t.Fatal(err)
	}
	q := ActivityQuery{Since: start, Until: end, BySession: true}
	check := func(want string) {
		t.Helper()
		got, err := st.UnreadSources(ctx, Transcripts, q)
		if err != nil || strings.Join(got, ",") != want {
			t.Fatalf("unread: %v %v, want %s", got, err, want)
		}
	}
	check("sessions,transcripts")
	if err := st.MarkRead(ctx, Transcripts, end); err != nil {
		t.Fatal(err)
	}
	check("sessions,transcripts")
	if err := st.MarkRead(ctx, Transcripts, end.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	check("sessions")
	if err := st.SetCursor(ctx, Sessions, "complete"); err != nil {
		t.Fatal(err)
	}
	check("sessions")
	if err := st.MarkRead(ctx, Sessions, end.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	check("")
	if err := st.MarkRead(ctx, Transcripts, start); err != nil {
		t.Fatal(err)
	}
	check("")
	q.Actor = ActorOwner
	if _, err := st.db.ExecContext(ctx, `DELETE FROM cursors WHERE source=?`, Sessions); err != nil {
		t.Fatal(err)
	}
	check("") // no sessions-source event is in the owner's activity
}

// Rebuilding drops complete and per-run read records atomically with the source's events and cursors.
func TestRebuildDropsAllSourceReadRecords(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	now := time.Now()
	if err := st.MarkRead(ctx, "mistakes", now); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkPartialRead(ctx, "mistakes", now, now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRead(ctx, "mistakes-file", now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Rebuild(ctx, "mistakes", false); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"mistakes", "mistakes-file"} {
		at, err := st.ReadWindowThrough(ctx, source, now.Add(-24*time.Hour))
		if err != nil || !at.IsZero() {
			t.Fatalf("read record survived for %s: %v %v", source, at, err)
		}
	}
}

// A command's run that read less than it should have (a scan that failed, a run that stopped) loses what it should
// have seen, and no later run puts it in the store. A window reads only through the first such run in it, though the
// runs after it are complete; a window that begins after it is read. A failure of one harness's scan leaves another
// harness's windows read, and the whole source's not.
func TestAWindowReadsOnlyThroughTheFirstIncompleteRunInIt(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	t0 := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if err := st.MarkRead(ctx, Sessions, t0.Add(10*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"claude", "codex"} {
		if err := st.MarkHarnessRead(ctx, Sessions, h, t0.Add(10*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.MarkIncomplete(ctx, Sessions, "claude", t0.Add(2*time.Hour+10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	read := func(harness string, since time.Time) time.Time {
		t.Helper()
		at, err := st.HarnessReadWindowThrough(ctx, Sessions, harness, since)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	for _, c := range []struct {
		harness string
		since   time.Time
		want    time.Time
	}{
		{"claude", t0, t0.Add(2*time.Hour + 10*time.Minute)},                    // the window holds the failed run
		{"claude", t0.Add(2 * time.Hour), t0.Add(2*time.Hour + 10*time.Minute)}, // and so does one that starts just before it
		{"claude", t0.Add(3 * time.Hour), t0.Add(10 * time.Hour)},               // one that starts after it is read through the scans
		{"codex", t0, t0.Add(10 * time.Hour)},                                   // another harness's scan did not fail
		{"", t0, t0.Add(2*time.Hour + 10*time.Minute)},                          // the source as a whole did
		{"", t0.Add(3 * time.Hour), t0.Add(10 * time.Hour)},
	} {
		if got := read(c.harness, c.since); !got.Equal(c.want) {
			t.Errorf("harness %q since %v: read through %v, want %v", c.harness, c.since, got, c.want)
		}
	}
	// a run that read nothing of the source as a whole holds every harness's windows
	if err := st.MarkIncomplete(ctx, Sessions, "", t0.Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := read("codex", t0.Add(5*time.Hour)); !got.Equal(t0.Add(6 * time.Hour)) {
		t.Errorf("a source-wide incomplete run: codex read through %v", got)
	}
	// with no complete read at all, a failed run reads nothing through
	if got, err := st.HarnessReadWindowThrough(ctx, Supervise, "claude", t0); err != nil || !got.IsZero() {
		t.Errorf("a source never read: %v %v", got, err)
	}
	if err := st.MarkIncomplete(ctx, Supervise, "claude", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, err := st.HarnessReadWindowThrough(ctx, Supervise, "claude", t0); err != nil || !got.IsZero() {
		t.Errorf("a failed run is no read: %v %v", got, err)
	}
}

// The unread check of a window asks for the harness a signal counts, and counts a failed run in it as unread.
func TestUnreadSourcesAsksForTheHarnessItCounts(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	start := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(8 * time.Hour)
	for _, h := range []string{"claude", "codex"} {
		if err := st.MarkHarnessRead(ctx, Sessions, h, end.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.MarkRead(ctx, Sessions, start.Add(-time.Hour)); err != nil { // no run read both harnesses since
		t.Fatal(err)
	}
	if err := st.MarkIncomplete(ctx, Sessions, "claude", start.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	q := ActivityQuery{Since: start, Until: end, BySession: true, Sources: []string{Sessions}}
	for harness, want := range map[string]string{"claude": "sessions", "codex": "", "": "sessions"} {
		q.Harness = harness
		got, err := st.UnreadSources(ctx, Transcripts, q)
		if err != nil || strings.Join(got, ",") != want {
			t.Errorf("harness %q: unread %v %v, want %q", harness, got, err, want)
		}
	}
}

// A failure that goes on is one record an hour, which keeps its first and its last run; the newest incomplete run in
// a span is found whether the record began before it or ends after it.
func TestIncompleteRunsOfAnHourAreOneRecord(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	t0 := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	for m := 0; m < 60; m += 5 {
		if err := st.MarkIncomplete(ctx, Sessions, "claude", t0.Add(time.Duration(m)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cursors WHERE source LIKE 'sessions:read:incomplete:%'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("records for an hour of failed runs: %d %v", n, err)
	}
	for _, c := range []struct {
		name         string
		since, until time.Time
		want         time.Time
	}{
		{"inside", t0.Add(20 * time.Minute), t0.Add(30 * time.Minute), t0.Add(55 * time.Minute)},
		{"begun before", t0.Add(-time.Hour), t0.Add(time.Minute), t0.Add(55 * time.Minute)},
		{"ending after", t0.Add(50 * time.Minute), t0.Add(3 * time.Hour), t0.Add(55 * time.Minute)},
		{"before", t0.Add(-time.Hour), t0.Add(-time.Minute), time.Time{}},
		{"after", t0.Add(56 * time.Minute), t0.Add(3 * time.Hour), time.Time{}},
	} {
		got, err := st.Incomplete(ctx, Sessions, "claude", c.since, c.until)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("%s: %v %v, want %v", c.name, got, err, c.want)
		}
	}
	if got, err := st.Incomplete(ctx, Sessions, "codex", t0.Add(-time.Hour), t0.Add(3*time.Hour)); err != nil || !got.IsZero() {
		t.Errorf("another harness: %v %v", got, err)
	}
}

// An incomplete run belongs to a window that starts or ends at it: a failed scan loses what happened after its time, so a
// window that starts at it holds it, and one that ends at it does too (the span is closed at both ends).
func TestAnIncompleteRunAtAWindowsEdgeIsInIt(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	at := time.Date(2026, 2, 1, 10, 30, 0, 0, time.UTC)
	if err := st.MarkIncomplete(ctx, Sessions, "claude", at); err != nil {
		t.Fatal(err)
	}
	ms := time.Millisecond
	for _, c := range []struct {
		name         string
		since, until time.Time
		want         time.Time
	}{
		{"a window that starts at it", at, at.Add(time.Hour), at},
		{"a window that ends at it", at.Add(-time.Hour), at, at},
		{"a window that starts just after it", at.Add(ms), at.Add(time.Hour), time.Time{}},
		{"a window that ends just before it", at.Add(-time.Hour), at.Add(-ms), time.Time{}},
	} {
		got, err := st.Incomplete(ctx, Sessions, "claude", c.since, c.until)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("%s: %v %v, want %v", c.name, got, err, c.want)
		}
	}
	// and the read of a window that starts at the failed run stops at it, as one that starts just after does not
	if err := st.MarkHarnessRead(ctx, Sessions, "claude", at.Add(10*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for since, want := range map[time.Time]time.Time{at: at, at.Add(ms): at.Add(10 * time.Hour)} {
		if got, err := st.HarnessReadWindowThrough(ctx, Sessions, "claude", since); err != nil || !got.Equal(want) {
			t.Errorf("read through for a window from %v: %v %v, want %v", since, got, err, want)
		}
	}
}

// A complete run of a command that reads each harness on its own, further from the run before it than RunGap, leaves
// the stretch between them as an incomplete run: no run started in it, so what the sessions did meanwhile is not in the
// store, and a window that holds the stretch is never read, however complete the runs after it are. A run within RunGap,
// another harness's runs, a run recorded late and a source with no harnesses leave none.
func TestARunFarFromTheOneBeforeLeavesItsStretchUnread(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	t0 := time.Date(2026, 2, 1, 8, 0, 0, 0, time.UTC)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	stretch := func(harness string) time.Time {
		t.Helper()
		at, err := st.Incomplete(ctx, Sessions, harness, t0.Add(-24*time.Hour), t0.Add(24*time.Hour))
		must(err)
		return at
	}
	lag := state.Keep // the sessions scan dates its read this much before its start
	// the timer's runs: the first has none before it, and runs RunGap apart or less leave no stretch
	must(st.MarkRunRead(ctx, Sessions, "claude", t0, lag))
	must(st.MarkRunRead(ctx, Sessions, "claude", t0.Add(5*time.Minute), lag))
	last := t0.Add(5*time.Minute + RunGap)
	must(st.MarkRunRead(ctx, Sessions, "claude", last, lag))
	if at := stretch("claude"); !at.IsZero() {
		t.Fatalf("runs no further apart than %v left a stretch: %v", RunGap, at)
	}
	if read, err := st.HarnessReadThrough(ctx, Sessions, "claude"); err != nil || !read.Equal(last.Add(-lag)) {
		t.Fatalf("a run is read through its start less its lag: %v %v", read, err)
	}
	// the other harness ran all along; the machine was off for claude's scan alone
	for at := t0; !at.After(last.Add(3 * time.Hour)); at = at.Add(5 * time.Minute) {
		must(st.MarkRunRead(ctx, Sessions, "codex", at, lag))
	}
	next := last.Add(3 * time.Hour)
	must(st.MarkRunRead(ctx, Sessions, "claude", next, lag))
	ms := time.Millisecond
	if at := stretch("claude"); !at.Equal(next.Add(-ms)) {
		t.Fatalf("the stretch between two runs 3 hours apart ends just before the second: %v, want %v", at, next.Add(-ms))
	}
	if at := stretch("codex"); !at.IsZero() {
		t.Fatalf("another harness's runs left a stretch: %v", at)
	}
	if at := stretch(""); !at.Equal(next.Add(-ms)) { // a signal of every harness is held by the one
		t.Fatalf("the source as a whole: %v", at)
	}
	// further runs of claude, as the timer goes on: every window is read through them but for the stretch's
	for at := next.Add(5 * time.Minute); !at.After(next.Add(time.Hour)); at = at.Add(5 * time.Minute) {
		must(st.MarkRunRead(ctx, Sessions, "claude", at, lag))
	}
	if at := stretch("claude"); !at.Equal(next.Add(-ms)) {
		t.Fatalf("runs within the gap added a stretch: %v", at)
	}
	unread := func(harness string, since, until time.Time) bool {
		t.Helper()
		got, err := st.UnreadSources(ctx, Transcripts, ActivityQuery{Since: since, Until: until, BySession: true, Harness: harness, Sources: []string{Sessions}})
		must(err)
		return len(got) > 0
	}
	for _, c := range []struct {
		name         string
		harness      string
		since, until time.Time
		unread       bool
	}{
		{"a window that ended at the last run before the stretch", "claude", t0, last, false},
		{"a window that holds the whole stretch", "claude", last.Add(-time.Hour), next.Add(time.Hour), true},
		{"a window inside it", "claude", last.Add(time.Hour), last.Add(2 * time.Hour), true},
		{"a window that ends in it", "claude", t0, last.Add(time.Hour), true},
		{"a window that starts at the run that ended it", "claude", next, next.Add(10 * time.Minute), false},
		{"another harness's window over it", "codex", last.Add(time.Hour), last.Add(2 * time.Hour), false},
		{"the same window of every harness", "", last.Add(time.Hour), last.Add(2 * time.Hour), true},
	} {
		if got := unread(c.harness, c.since, c.until); got != c.unread {
			t.Errorf("%s: unread %v, want %v", c.name, got, c.unread)
		}
	}
	// a run recorded after a later one began (two runs that overlapped) is no stretch
	must(st.MarkRunRead(ctx, Sessions, "hermes", t0.Add(time.Hour), lag))
	must(st.MarkRunRead(ctx, Sessions, "hermes", t0, lag))
	if at := stretch("hermes"); !at.IsZero() {
		t.Fatalf("a run recorded late left a stretch: %v", at)
	}
	// supervision's runs are read at their start, with no lag; a source that keeps no read for each harness has no stretch
	must(st.MarkRunRead(ctx, Supervise, "claude", t0, 0))
	must(st.MarkRunRead(ctx, Supervise, "claude", t0.Add(20*time.Minute), 0))
	if at, err := st.Incomplete(ctx, Supervise, "claude", t0, t0.Add(time.Hour)); err != nil || !at.Equal(t0.Add(20*time.Minute-ms)) {
		t.Fatalf("supervision's stretch: %v %v", at, err)
	}
	if read, err := st.HarnessReadThrough(ctx, Supervise, "claude"); err != nil || !read.Equal(t0.Add(20*time.Minute)) {
		t.Fatalf("supervision's read: %v %v", read, err)
	}
	must(st.MarkRunRead(ctx, Capacity, "", t0, 0))
	must(st.MarkRunRead(ctx, Capacity, "", t0.Add(3*time.Hour), 0))
	if at, err := st.Incomplete(ctx, Capacity, "", t0, t0.Add(4*time.Hour)); err != nil || !at.IsZero() {
		t.Fatalf("a source with no harnesses: %v %v", at, err)
	}
}

// A harness's complete read is its own where the source keeps one; any other source, or a harness it never recorded,
// has the source's.
func TestAHarnessReadIsItsOwnWhereTheSourceKeepsOne(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	t0 := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.MarkRead(ctx, Sessions, t0))
	must(st.MarkHarnessRead(ctx, Sessions, "claude", t0.Add(time.Hour)))
	must(st.MarkHarnessRead(ctx, Capacity, "claude", t0.Add(time.Hour))) // a source with no harnesses: the whole source's
	for _, c := range []struct {
		source, harness string
		want            time.Time
	}{
		{Sessions, "claude", t0.Add(time.Hour)},
		{Sessions, "codex", t0},
		{Sessions, "", t0},
		{Capacity, "claude", t0.Add(time.Hour)},
		{Capacity, "", t0.Add(time.Hour)},
	} {
		got, err := st.HarnessReadThrough(ctx, c.source, c.harness)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("%s %q: %v %v, want %v", c.source, c.harness, got, err, c.want)
		}
	}
	first := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	if _, err := st.Put(ctx, []Event{{Source: Sessions, SourceKey: "a", TS: first.Add(time.Hour), Session: "a", Harness: "codex", Actor: ActorHarness, Kind: "phase"},
		{Source: Sessions, SourceKey: "b", TS: first, Session: "b", Harness: "claude", Actor: ActorHarness, Kind: "phase"}}); err != nil {
		t.Fatal(err)
	}
	for harness, want := range map[string]time.Time{"codex": first.Add(time.Hour), "claude": first, "": first} {
		got, err := st.FirstHarnessEvent(ctx, Sessions, harness)
		if err != nil || !got.Equal(want) {
			t.Errorf("first event of %q: %v %v, want %v", harness, got, err, want)
		}
	}
}

// A complete read makes the partial runs that began before it of no use: they could only raise a window's read time to
// their own start. A partial run after it stays.
func TestACompleteReadDropsThePartialRunsBeforeIt(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()
	t0 := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if err := st.MarkRead(ctx, "mistakes", t0); err != nil {
		t.Fatal(err)
	}
	for _, h := range []int{1, 2, 3} {
		if err := st.MarkPartialRead(ctx, "mistakes", t0.Add(time.Duration(h)*time.Hour), t0.Add(-48*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		t.Helper()
		var n int
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cursors WHERE source LIKE 'mistakes:read:run:%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := st.MarkRead(ctx, "mistakes", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("partial runs left after a read through hour 2: %d", n)
	}
	// the run left still raises a window's read time: the complete read is older than its start no more
	if at, err := st.ReadWindowThrough(ctx, "mistakes", t0); err != nil || !at.Equal(t0.Add(3*time.Hour)) {
		t.Fatalf("read through: %v %v", at, err)
	}
}

// An ingest and a rebuild take turns: one that finds the store taken fails at once and names the process.
func TestAnIngestAndARebuildTakeTurns(t *testing.T) {
	st, path := open(t)
	unlock, err := st.LockIngest()
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.LockIngest(); err == nil || !strings.Contains(err.Error(), fmt.Sprint("pid ", os.Getpid())) {
		t.Fatalf("a second holder: %v", err)
	}
	unlock()
	again, err := other.LockIngest()
	if err != nil {
		t.Fatalf("after the first let go: %v", err)
	}
	again()
}
