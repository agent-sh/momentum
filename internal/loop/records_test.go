package loop

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func recordFixture(t *testing.T, events []store.Event, body string) RecordsOptions {
	t.Helper()
	dir := t.TempDir()
	return RecordsOptions{Store: inputStore(t, events), Destination: filepath.Join(dir, "records"),
		Home: dir, Binary: script(t, dir, "momentum-fixture", body), Env: []string{"PATH=/usr/bin:/bin"}}
}

func TestInputsRecordsPriorityNeighboursIndexAndSecondPass(t *testing.T) {
	goodInputScrubber(t)
	at := time.Date(2025, 4, 10, 12, 0, 0, 0, time.UTC)
	var events []store.Event
	for i := -7; i <= 7; i++ {
		events = append(events, store.Event{Source: "fixture", SourceKey: strconv.Itoa(i), Session: "same",
			TS: at.Add(time.Duration(i) * time.Minute), Kind: "record"})
	}
	events = append(events,
		store.Event{Source: "fixture", SourceKey: "other", Session: "same-child", TS: at, Kind: "record"},
		store.Event{Source: "fixture", SourceKey: "outside", Session: "same", TS: at.Add(11 * time.Minute), Kind: "record"})
	o := recordFixture(t, events, `test "${MOMENTUM_HOME+x}" = x && test "${MOMENTUM_NOW+x}" = x || exit 7
test "$1" = evidence && test "$2" = show && test "$3" = --bytes && test "$4" = 16384 || exit 8
printf 'record %s marker-7f3a\n' "$5"`)
	o.AsOf = at.Add(time.Hour)
	o.IDs = []string{events[7].ID(), events[0].ID(), events[7].ID(), "absent"}
	m, err := Records(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if m.Written != 13 || m.Withheld != 0 || m.Skipped != 1 {
		t.Fatalf("manifest %+v", m)
	}
	if m.Files[0].Event != events[7].ID() || m.Files[1].Event != events[0].ID() {
		t.Fatal("neighbours displaced evidence priority")
	}
	index, err := os.ReadFile(filepath.Join(o.Destination, "index.txt"))
	if err != nil || string(index) != recordIndex(m.Files) || len(strings.Split(strings.TrimSpace(string(index)), "\n")) != m.Written {
		t.Fatalf("index mismatch: %v", err)
	}
	wantFirst := "ev-" + events[7].ID() + ".txt\t" + events[7].ID() + "\t2025-04-10T12:00:00Z\n"
	if !strings.HasPrefix(string(index), wantFirst) {
		t.Fatal("index lost the event id or timestamp")
	}
	total := len(index)
	for _, file := range m.Files {
		text, err := os.ReadFile(filepath.Join(o.Destination, file.Name))
		if err != nil || !strings.Contains(string(text), "[masked]") || strings.Contains(string(text), "marker-7f3a") {
			t.Fatalf("record did not receive second pass: %v", err)
		}
		if file.Event == events[15].ID() || file.Event == events[16].ID() {
			t.Fatal("neighbour came from another session or outside window")
		}
		total += len(text)
	}
	if total != m.Bytes {
		t.Fatal("record byte accounting differs")
	}
}

func TestInputsNeighbourWindowAndExactSession(t *testing.T) {
	goodInputScrubber(t)
	at := time.Date(2025, 4, 10, 12, 0, 0, 0, time.UTC)
	events := []store.Event{
		{Source: "fixture", SourceKey: "anchor", Session: "same", TS: at, Kind: "record"},
		{Source: "fixture", SourceKey: "far", Session: "same", TS: at.Add(11 * time.Minute), Kind: "record"},
		{Source: "fixture", SourceKey: "child", Session: "same-child", TS: at.Add(time.Minute), Kind: "record"},
	}
	o := recordFixture(t, events, `printf 'record\n'`)
	o.IDs = []string{events[0].ID()}
	m, err := Records(context.Background(), o)
	if err != nil || m.Written != 1 || m.Files[0].Event != events[0].ID() {
		t.Fatalf("neighbour isolation: %+v %v", m, err)
	}
}

func TestInputsRecordBoundsAndWithheld(t *testing.T) {
	goodInputScrubber(t)
	at := time.Date(2025, 4, 10, 12, 0, 0, 0, time.UTC)
	var events []store.Event
	for i := range MaxRecords + 1 {
		events = append(events, store.Event{Source: "fixture", SourceKey: strconv.Itoa(i), TS: at, Kind: "record"})
	}
	t.Run("count", func(t *testing.T) {
		o := recordFixture(t, events, `printf 'record\n'`)
		o.Neighbours = -1
		for _, event := range events {
			o.IDs = append(o.IDs, event.ID())
		}
		m, err := Records(context.Background(), o)
		if err != nil || m.Written != MaxRecords || m.Skipped != 1 || m.Stopped != "record count" {
			t.Fatalf("count bound: %+v %v", m, err)
		}
	})
	for _, size := range []int{MaxRecordBytes, MaxRecordBytes + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			o := recordFixture(t, events[:2], `head -c `+strconv.Itoa(size)+` /dev/zero | tr '\000' x`)
			o.Neighbours = -1
			o.IDs = []string{events[0].ID(), events[1].ID()}
			m, err := Records(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if size == MaxRecordBytes && (m.Written != 2 || m.Withheld != 0) {
				t.Fatalf("exact boundary refused: %+v", m)
			}
			if size > MaxRecordBytes && (m.Written != 0 || m.Withheld != 1 || m.Skipped != 1 || m.Stopped != "record bytes") {
				t.Fatalf("oversized record accepted: %+v", m)
			}
		})
	}
	t.Run("withheld", func(t *testing.T) {
		o := recordFixture(t, events[:1], `printf '(record withheld: fixture)\n'`)
		o.IDs = []string{events[0].ID()}
		m, err := Records(context.Background(), o)
		if err != nil || m.Written != 0 || m.Withheld != 1 {
			t.Fatalf("withheld count: %+v %v", m, err)
		}
	})
	// The aggregate limit is larger than 200 * 16 KiB. Pin its independent
	// admission check without weakening the production limits for a fixture.
	if !recordFits(MaxRecordsBytes-10, 0, 5, 5) || recordFits(MaxRecordsBytes-10, 0, 6, 5) {
		t.Fatal("aggregate byte boundary is not exact")
	}
}

func TestInputsUnitFinderAndSharedUnitBudget(t *testing.T) {
	goodInputScrubber(t)
	got := FindUnits("first.service first.service /not.service bad.service.more `job@a.timer`")
	if !reflect.DeepEqual(got, []string{"first.service", "job@a.timer"}) {
		t.Fatalf("unit finder %v", got)
	}
	var text strings.Builder
	for i := range MaxUnits + 1 {
		text.WriteString("unit" + strconv.Itoa(i) + ".service ")
	}
	if len(FindUnits(text.String())) != MaxUnits {
		t.Fatal("finder exceeded the unit limit")
	}
	at := time.Date(2025, 4, 10, 12, 0, 0, 0, time.UTC)
	event := store.Event{Source: "fixture", SourceKey: "one", TS: at, Kind: "record"}
	o := recordFixture(t, []store.Event{event}, `printf 'record marker-7f3a\n'`)
	o.IDs = []string{event.ID()}
	if _, err := Records(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	o.Units = append(FindUnits(text.String()), "extra.timer", "../bad.service", "first.service;")
	m, err := UnitLogs(context.Background(), o)
	if err != nil || m.Written != MaxUnits+1 || m.Skipped != 3 {
		t.Fatalf("unit manifest %+v: %v", m, err)
	}
	last := m.Files[len(m.Files)-1]
	lastUnit := "unit" + strconv.Itoa(MaxUnits-1) + ".service"
	if last.Unit != lastUnit || last.Name != "unit-"+lastUnit+".txt" {
		t.Fatal("unit filenames or order changed")
	}
	index, err := os.ReadFile(filepath.Join(o.Destination, "index.txt"))
	if err != nil || string(index) != recordIndex(m.Files) {
		t.Fatal("unit index omitted existing records")
	}
	o.Units = []string{"unit0.service"}
	again, err := UnitLogs(context.Background(), o)
	if err != nil || again.Written != m.Written || again.Bytes != m.Bytes {
		t.Fatal("unit appended twice")
	}
	// Build a nearly full index, then prove UnitLogs shares the count cap.
	full := recordFixture(t, nil, `printf 'unit\n'`)
	if err := os.Mkdir(full.Destination, 0o700); err != nil {
		t.Fatal(err)
	}
	var files []RecordFile
	for i := range MaxRecords {
		id := "id" + strconv.Itoa(i)
		name := "ev-" + id + ".txt"
		write(t, filepath.Join(full.Destination, name), "x")
		files = append(files, RecordFile{Name: name, Event: id, At: at, Bytes: 1})
	}
	write(t, filepath.Join(full.Destination, "index.txt"), recordIndex(files))
	full.Units = []string{"extra.service"}
	capped, err := UnitLogs(context.Background(), full)
	if err != nil || capped.Written != MaxRecords || capped.Skipped != 1 || capped.Stopped != "record count" {
		t.Fatalf("unit bypassed record bound: %+v %v", capped, err)
	}
}

// A unit the machine runs of its own whose journal has entries in the window is rendered without being named in the
// case: the run has no command to ask for a log, so the logs that might matter are there.
func TestActiveUnitsAreTheMachinesOwnWithJournalEntries(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{"quiet.service", "busy.service", "other.timer", "template@.service"} {
		write(t, filepath.Join(home, ".config/systemd/user", name), "[Unit]\n")
	}
	bin := t.TempDir()
	script(t, bin, "journalctl", `printf 'busy.service\nbusy.service\nother.timer\nforeign.service\ninit.scope\n\nbusy.service\n'`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	got := ActiveUnits(context.Background(), home, time.Now().Add(-ActiveUnitWindow), time.Now())
	if !reflect.DeepEqual(got, []string{"busy.service", "other.timer"}) {
		t.Fatalf("active units %v", got)
	}
	if u := UnionUnits([]string{"named.service", "busy.service"}, got); !reflect.DeepEqual(u, []string{"named.service", "busy.service", "other.timer"}) {
		t.Fatalf("union %v", u)
	}
	if none := ActiveUnits(context.Background(), t.TempDir(), time.Now().Add(-time.Hour), time.Now()); none != nil {
		t.Fatalf("a home with no units gave %v", none)
	}
}
