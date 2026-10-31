package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"
)

// capacityRecord is a run, two disk samples and a sighting, as a string to compare.
func capacityRecord(t *testing.T, st *Store) string {
	t.Helper()
	ctx := context.Background()
	start, err := st.SamplerStart(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ds, err := st.DiskSamples(ctx, "/data", time.UnixMilli(0))
	if err != nil {
		t.Fatal(err)
	}
	seen, err := st.SeenPaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mark, err := st.Mark(ctx, "prune:uv")
	if err != nil {
		t.Fatal(err)
	}
	out := start.UTC().Format(time.RFC3339)
	for _, d := range ds {
		out += " " + d.At.UTC().Format(time.RFC3339)
	}
	for _, p := range []string{"/m/a"} {
		out += " " + p + "@" + seen[p].Last.UTC().Format(time.RFC3339)
	}
	return out + " uv@" + mark.UTC().Format(time.RFC3339)
}

// What the capacity runs observed is kept: no step drops it, not one that reads every source again, nor the visit
// of a build from before version 3.
func TestCapacityRecordsAreKeptAcrossSchemaChanges(t *testing.T) {
	ctx := context.Background()
	st, path := open(t)
	t0 := time.UnixMilli(1_700_000_000_000)
	if err := st.PutCapacityRun(ctx, CapacityRun{At: t0, Sample: 120 * time.Millisecond, Procs: 600},
		[]DiskSample{{Disk: "/data", At: t0, Size: 100, Used: 50, Avail: 50}}, []Seen{{Path: "/m/a", Last: t0, How: "map"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutCapacityRun(ctx, CapacityRun{At: t0.Add(time.Hour)},
		[]DiskSample{{Disk: "/data", At: t0.Add(time.Hour), Size: 100, Used: 60, Avail: 40}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMark(ctx, "prune:uv", t0); err != nil {
		t.Fatal(err)
	}
	want := capacityRecord(t, st)
	st.Close()
	reopen := func(name, visit string) {
		t.Helper()
		if visit != "" {
			db, err := sql.Open("sqlite", "file:"+path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(visit); err != nil {
				t.Fatal(err)
			}
			db.Close()
		}
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if got := capacityRecord(t, st); got != want {
			t.Fatalf("%s changed the capacity record:\n%s\nwant\n%s", name, got, want)
		}
	}
	reopen("a build from before version 3", `DELETE FROM events WHERE source != 'sessions'; PRAGMA user_version = 2`)
	was := steps
	t.Cleanup(func() { steps = was })
	steps = append(slices.Clone(was), step{version: latest() + 1, reread: true})
	reopen("the next step that reads again", "")
}

// A store from before the capacity tables gains them at its next open and takes no step, so a build from before
// them, which never reads them, keeps working on it.
func TestAStoreFromBeforeTheCapacityTablesGainsThem(t *testing.T) {
	ctx := context.Background()
	st, path := open(t)
	t0 := time.UnixMilli(1_700_000_000_000)
	st.Close()
	var stepsBefore int
	rawQuery(t, path, `SELECT COUNT(*) FROM schema_steps`, &stepsBefore)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tb := range capacityTableNames {
		if _, err := db.Exec(`DROP TABLE ` + tb); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.PutCapacityRun(ctx, CapacityRun{At: t0}, []DiskSample{{Disk: "/", At: t0}}, []Seen{{Path: "/m", Last: t0}}); err != nil {
		t.Fatalf("a run on a store from before the capacity tables: %v", err)
	}
	var stepsAfter int
	rawQuery(t, path, `SELECT COUNT(*) FROM schema_steps`, &stepsAfter)
	if stepsAfter != stepsBefore {
		t.Fatalf("%d steps before and %d after: the capacity tables take no step", stepsBefore, stepsAfter)
	}
	if ok, err := current(st.db); err != nil || !ok {
		t.Fatalf("the store is not current: %v", err)
	}
}

// A path seen again keeps its first sighting and takes the latest one's time and process; an older sighting
// changes neither.
func TestASightingKeepsTheFirstAndTheLatest(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	t0 := time.UnixMilli(1_700_000_000_000)
	for _, v := range []Seen{
		{Path: "/m/a", Last: t0.Add(time.Hour), How: "map", Proc: "vllm 1"},
		{Path: "/m/a", Last: t0, How: "fd", Proc: "cp 2"},
		{Path: "/m/a", Last: t0.Add(2 * time.Hour), How: "arg", Proc: "srv 3"},
	} {
		if err := st.PutCapacityRun(ctx, CapacityRun{At: v.Last}, nil, []Seen{v}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.SeenPaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a := got["/m/a"]
	if !a.First.Equal(t0) || !a.Last.Equal(t0.Add(2*time.Hour)) || a.How != "arg" || a.Proc != "srv 3" {
		t.Fatalf("got %+v", a)
	}
	if start, _ := st.SamplerStart(ctx); !start.Equal(t0) {
		t.Fatalf("the record starts at the first run: %v", start)
	}
	if ts, _ := st.RunTimes(ctx); len(ts) != 3 || !ts[0].Equal(t0) {
		t.Fatalf("run times %v", ts)
	}
	if err := st.PutRuns(ctx, []CapacityRun{{At: t0.Add(3 * time.Hour), Procs: 2, Blind: true}}); err != nil {
		t.Fatal(err)
	}
	if ts, _ := st.RunTimes(ctx); len(ts) != 3 {
		t.Fatalf("a blind run watched nothing: %v", ts)
	}
	if ps, _ := st.RecentProcs(ctx, 10); len(ps) != 3 {
		t.Fatalf("a blind run's count is no measure of the machine: %v", ps)
	}
}

func TestRebuildRefusesTheCapacityRecord(t *testing.T) {
	st, _ := open(t)
	if _, err := st.Rebuild(context.Background(), Capacity, true); !errors.Is(err, ErrObserved) {
		t.Fatalf("capacity's events are observations: %v", err)
	}
}

// A capacity_runs table from before it had the blind column gains it and keeps its rows: the column is the table's
// next one in keptColumns, not in the CREATE TABLE.
func TestARunsTableFromBeforeTheBlindColumnGainsIt(t *testing.T) {
	ctx := context.Background()
	st, path := open(t)
	t0 := time.UnixMilli(1_700_000_000_000)
	if err := st.PutCapacityRun(ctx, CapacityRun{At: t0, Procs: 5}, nil, nil); err != nil {
		t.Fatal(err)
	}
	st.Close()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE capacity_runs DROP COLUMN blind`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.PutCapacityRun(ctx, CapacityRun{At: t0.Add(time.Minute), Procs: 5, Blind: true}, nil, nil); err != nil {
		t.Fatalf("a run on the older table: %v", err)
	}
	if ts, err := st.RunTimes(ctx); err != nil || len(ts) != 1 || !ts[0].Equal(t0) {
		t.Fatalf("the old run stays, the blind one is no watched time: %v %v", ts, err)
	}
}

func TestWritesAreReplacedByEachRecord(t *testing.T) {
	ctx := context.Background()
	st, _ := open(t)
	if at, ws, err := st.Writes(ctx); err != nil || !at.IsZero() || ws != nil {
		t.Fatalf("none on record: %v %v %v", at, ws, err)
	}
	t1 := time.UnixMilli(1_800_000_000_000)
	if err := st.PutWrites(ctx, t1, []ProcWrite{{PID: 1, Start: 100, Written: 5}, {PID: 2, Start: 200, Written: 7}}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutWrites(ctx, t1.Add(time.Minute), []ProcWrite{{PID: 2, Start: 200, Written: 9}}); err != nil {
		t.Fatal(err)
	}
	at, ws, err := st.Writes(ctx)
	if err != nil || !at.Equal(t1.Add(time.Minute)) || len(ws) != 1 || ws[0] != (ProcWrite{PID: 2, Start: 200, Written: 9}) {
		t.Errorf("the last record only: %v %v %v", at, ws, err)
	}
}
