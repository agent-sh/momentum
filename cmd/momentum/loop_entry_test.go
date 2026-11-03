package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

// loopMachine is a machine for `momentum loop`: its own store and configuration, a harness that records that it
// started, a Bedrock env file when route is true, and a loop run killed hours ago with the process that ran it, which
// the next loop run would end. It returns the harness's marker and the stale run's case.
func loopMachine(t *testing.T, loopConfig string, route bool) (started string, c store.Case) {
	t.Helper()
	isolatedHome(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	dir := t.TempDir()
	started = filepath.Join(dir, "started")
	harness := filepath.Join(dir, "harness")
	if err := os.WriteFile(harness, []byte("#!/bin/sh\ntouch '"+started+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(dir, "bedrock.env")
	if route {
		if err := os.WriteFile(env, []byte("CLAUDE_CODE_USE_BEDROCK=\"1\"\nANTHROPIC_BEDROCK_BASE_URL=\"http://127.0.0.1:9/n\"\nCLAUDE_CODE_SKIP_BEDROCK_AUTH=\"1\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	conf := `{"loop": {"runner": ["` + harness + `"], "bedrock_env": "` + env + `", "families": ["failures"]` + loopConfig + `}}`
	if err := os.WriteFile(filepath.Join(os.Getenv("MOMENTUM_CONFIG"), "sources.json"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	c, _, err = st.ObserveCase(ctx, store.Finding{Family: "failures", Signature: "failures/v1:entry", Title: "entry",
		Evidence: []store.Evidence{{Ref: "file:/t/entry", At: time.Now().Add(-time.Hour)}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutLoopRun(ctx, store.LoopRun{ID: "stale", Case: c.ID, Family: "failures", Step: "investigate", Started: time.Now().Add(-5 * time.Hour),
		Status: store.RunRunning, USD: 4}); err != nil {
		t.Fatal(err)
	}
	return started, c
}

// isolatedHome gives the test a home with nothing in it: whatever would read the machine's user data reads this
// instead. The go command writes its telemetry counters under the home as it runs, some of them after the test
// ends, so the home tells it its telemetry is off.
func isolatedHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	telemetry := filepath.Join(home, ".config", "go", "telemetry")
	if err := os.MkdirAll(telemetry, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(telemetry, "mode"), []byte("off 2026-01-01\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
}

// untouched says that no loop run recovered a stale run, detected, reserved or started anything.
func untouched(t *testing.T, started string) {
	t.Helper()
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runs, err := st.LoopRuns(context.Background(), store.RunFilter{})
	if err != nil || len(runs) != 1 || runs[0].ID != "stale" || runs[0].Status != store.RunRunning {
		t.Errorf("the loop touched its runs: %+v %v", runs, err)
	}
	if _, err := os.Stat(started); err == nil {
		t.Error("the harness started")
	}
}

// A loop run with no route starts nothing: its stale recovery, detection and reservations included, and nothing
// falls back to the direct API.
func TestALoopRunWithNoRouteStartsNothing(t *testing.T) {
	started, _ := loopMachine(t, "", false)
	code, _, errOut := momentum("loop")
	if code != 3 || !strings.Contains(errOut, "no Bedrock route") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	untouched(t, started)
	// a dry pick needs no route: it runs no model
	if code, _, errOut := momentum("loop", "--dry"); code != 0 {
		t.Fatalf("a dry pick: exit %d: %s", code, errOut)
	}
}

// The loop's models are Claude models on Bedrock's global inference profiles, one model and one fallback model, and a
// loop run says so before it starts anything.
func TestALoopRunChecksItsModelsBeforeItStartsAnything(t *testing.T) {
	for name, c := range map[string]struct{ conf, want string }{
		"regional model":    {`, "model": "us.anthropic.claude-opus-5-5"`, `loop model "us.anthropic.claude-opus-5-5" is no Bedrock model`},
		"alias model":       {`, "model": "sonnet"`, `is no Bedrock model`},
		"regional fallback": {`, "fallback_model": "us.anthropic.claude-sonnet-5-5"`, `loop fallback_model "us.anthropic.claude-sonnet-5-5" is no Bedrock model`},
		"unpriced fallback": {`, "fallback_model": "global.anthropic.claude-haiku-4-5"`, `has no rate in the relay`},
		"a list":            {`, "fallback_model": "global.anthropic.claude-sonnet-5-5,global.anthropic.claude-haiku-4-5"`, `is a list: the loop takes one model`},
		"a list with space": {`, "fallback_model": "global.anthropic.claude-sonnet-5-5 global.anthropic.claude-haiku-4-5"`, `is a list: the loop takes one model`},
	} {
		t.Run(name, func(t *testing.T) {
			started, _ := loopMachine(t, c.conf, true)
			code, _, errOut := momentum("loop")
			if code != 3 || !strings.Contains(errOut, c.want) {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			untouched(t, started)
		})
	}
	// the defaults, and a model with the harness's long-context suffix, are fine
	for _, a := range []loop.Agent{
		{Model: "global.anthropic.claude-opus-5-5", Fallback: "global.anthropic.claude-sonnet-5-5", Bedrock: &loop.Bedrock{File: bedrockFile(t)}},
		{Model: "global.anthropic.claude-opus-5-5[1m]", Bedrock: &loop.Bedrock{File: bedrockFile(t)}},
	} {
		if err := onBedrock(a); err != nil {
			t.Errorf("%+v: %v", a, err)
		}
	}
}

func bedrockFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bedrock.env")
	if err := os.WriteFile(p, []byte("CLAUDE_CODE_USE_BEDROCK=\"1\"\nANTHROPIC_BEDROCK_BASE_URL=\"http://127.0.0.1:9/n\"\nCLAUDE_CODE_SKIP_BEDROCK_AUTH=\"1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// One loop at a time: a second fails at once and says so, whatever it would do, and runs again once the first ends.
func TestASecondLoopRunFailsAtOnce(t *testing.T) {
	loopMachine(t, "", true)
	unlock, err := lockLoop(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	code, _, errOut := momentum("loop", "--dry")
	if code != 3 || !strings.Contains(errOut, "another loop runs now") || time.Since(start) > 5*time.Second {
		t.Fatalf("a second loop run: exit %d after %v: %s", code, time.Since(start), errOut)
	}
	unlock()
	if code, _, errOut := momentum("loop", "--dry"); code != 0 {
		t.Fatalf("after the first ended: exit %d: %s", code, errOut)
	}
}

// dryFiles records persistent contents, modes and file timestamps without opening a store for writing.
func dryFiles(t *testing.T, root string) map[string]struct {
	Body     string
	Mode     fs.FileMode
	Modified time.Time
} {
	t.Helper()
	files := map[string]struct {
		Body     string
		Mode     fs.FileMode
		Modified time.Time
	}{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var body []byte
		var modified time.Time
		if !entry.IsDir() {
			body, err = os.ReadFile(path)
			modified = info.ModTime()
			if err != nil {
				return err
			}
		}
		files[path] = struct {
			Body     string
			Mode     fs.FileMode
			Modified time.Time
		}{string(body), info.Mode(), modified}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// dryConfig gives the dry CLI tests a configuration with no source outside their own fixtures.
func dryConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MOMENTUM_CONFIG", dir)
	if err := os.WriteFile(filepath.Join(dir, "sources.json"), []byte(`{"loop":{"families":["failures"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Dry selection executes no toolchain discovery, even when the discovered program would write outside the store.
func TestADryLoopRunsNoToolchainDiscovery(t *testing.T) {
	isolatedHome(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	dryConfig(t)
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := st.ObserveCase(context.Background(), store.Finding{Family: "failures", Signature: "failures/v1:marker-7f3a",
		Title: "marker-7f3a", Evidence: []store.Evidence{{Ref: "file:/fixture/marker-7f3a", At: time.Now()}}}, "test")
	if closeErr := st.Close(); err != nil || closeErr != nil {
		t.Fatalf("fixture store: %v %v", err, closeErr)
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	goFixture := filepath.Join(dir, "go")
	if err := os.WriteFile(goFixture, []byte("#!/bin/sh\nprintf 'marker-7f3a' > '"+marker+"'\nprintf '/fixture/toolchain\\n/fixture/modules\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	before := dryFiles(t, paths.Data())
	code, out, errOut := momentum("loop", "--dry", "--json")
	if code != 0 || !strings.Contains(out, c.ID) || !strings.Contains(out, `"step":"investigate"`) {
		t.Errorf("dry selection: exit %d: %s %s", code, out, errOut)
	}
	if body, err := os.ReadFile(marker); !os.IsNotExist(err) {
		t.Errorf("dry selection invoked go: marker %q, error %v", body, err)
	}
	if after := dryFiles(t, paths.Data()); !reflect.DeepEqual(before, after) {
		t.Error("dry selection changed the store")
	}
}

// A dry CLI call creates no store, schema, data directory or lock, and explains that no store exists.
func TestADryLoopWithNoStoreCreatesNothing(t *testing.T) {
	for _, directoryExists := range []bool{false, true} {
		t.Run(fmt.Sprint(directoryExists), func(t *testing.T) {
			isolatedHome(t)
			root := t.TempDir()
			data := filepath.Join(root, "state")
			t.Setenv("MOMENTUM_HOME", data)
			dryConfig(t)
			if directoryExists {
				if err := os.Mkdir(data, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			before := dryFiles(t, root)
			code, _, errOut := momentum("loop", "--dry")
			if code != 3 || !strings.Contains(errOut, "no store to read") {
				t.Errorf("missing store: exit %d: %s", code, errOut)
			}
			if after := dryFiles(t, root); !reflect.DeepEqual(before, after) {
				t.Error("dry CLI created or changed persistent state")
			}
		})
	}
}

// A dry CLI call refuses an older schema without migrating its tables or creating lock files.
func TestADryLoopDoesNotMigrateAnOlderStore(t *testing.T) {
	isolatedHome(t)
	root := t.TempDir()
	t.Setenv("MOMENTUM_HOME", root)
	dryConfig(t)
	db, err := sql.Open("sqlite", paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE marker (value TEXT);
INSERT INTO marker VALUES ('marker-7f3a');
PRAGMA user_version = 2;`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := dryFiles(t, root)
	code, _, errOut := momentum("loop", "--dry")
	if code != 3 || !strings.Contains(errOut, "not this build's schema") {
		t.Errorf("older store: exit %d: %s", code, errOut)
	}
	if after := dryFiles(t, root); !reflect.DeepEqual(before, after) {
		t.Error("dry CLI changed the older store")
	}
	db, err = sql.Open("sqlite", "file:"+paths.DB()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, tables int
	var marker string
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT value FROM marker`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if version != 2 || tables != 1 || marker != "marker-7f3a" {
		t.Errorf("older schema changed: version %d, %d tables, marker %q", version, tables, marker)
	}
}

// A dry CLI call reads a current store and an existing free lock without changing persistent state.
func TestADryLoopDoesNotCreateOrRewriteItsLock(t *testing.T) {
	for _, lockExists := range []bool{false, true} {
		t.Run(fmt.Sprint(lockExists), func(t *testing.T) {
			loopMachine(t, "", false)
			root := paths.Data()
			path := paths.DB() + ".loop-lock"
			if lockExists {
				if err := os.WriteFile(path, []byte("marker-7f3a"), 0o400); err != nil {
					t.Fatal(err)
				}
			}
			before := dryFiles(t, root)
			if code, _, errOut := momentum("loop", "--dry"); code != 0 {
				t.Errorf("current store: exit %d: %s", code, errOut)
			}
			if after := dryFiles(t, root); !reflect.DeepEqual(before, after) {
				t.Error("dry CLI created or changed persistent state")
			}
		})
	}
}

// A live SQLite transaction prevents the dry path from claiming a stable, sidecar-free view.
func TestADryLoopRefusesALiveStoreWithoutChangingIt(t *testing.T) {
	loopMachine(t, "", false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDryStoreHolder$")
	cmd.Env = append(os.Environ(), "MOMENTUM_TEST_STORE_HOLDER="+paths.DB())
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		input.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("store holder: %v", err)
		}
	}()
	scanner := bufio.NewScanner(output)
	if !scanner.Scan() || scanner.Text() != "marker-7f3a" {
		t.Fatalf("store holder did not become ready: %q %v", scanner.Text(), scanner.Err())
	}
	before := dryFiles(t, paths.Data())
	code, _, errOut := momentum("loop", "--dry")
	if code != 3 || !strings.Contains(errOut, "idle store") {
		t.Errorf("live store: exit %d: %s", code, errOut)
	}
	if after := dryFiles(t, paths.Data()); !reflect.DeepEqual(before, after) {
		t.Error("dry CLI changed a live store")
	}
}

// TestDryStoreHolder holds a real SQLite read transaction only for its parent test's synthetic store.
func TestDryStoreHolder(t *testing.T) {
	path := os.Getenv("MOMENTUM_TEST_STORE_HOLDER")
	if path == "" {
		return
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var cases int
	if err := tx.QueryRow(`SELECT count(*) FROM cases`).Scan(&cases); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stdout, "marker-7f3a")
	_, _ = os.Stdin.Read(make([]byte, 1)) // its parent closes stdin after checking the refusal
}

// Leftover journal data must be recovered by a writer, never silently ignored or changed by a dry selection.
func TestADryLoopRefusesJournalDataWithoutChangingIt(t *testing.T) {
	for _, suffix := range []string{"-wal", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			loopMachine(t, "", false)
			if err := os.WriteFile(paths.DB()+suffix, []byte("marker-7f3a"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := dryFiles(t, paths.Data())
			code, _, errOut := momentum("loop", "--dry")
			if code != 3 || !strings.Contains(errOut, "checkpointed store") || !strings.Contains(errOut, suffix) {
				t.Errorf("leftover journal data: exit %d: %s", code, errOut)
			}
			if after := dryFiles(t, paths.Data()); !reflect.DeepEqual(before, after) {
				t.Error("dry CLI changed journal data")
			}
		})
	}
}

// What a run's relay refused is printed after the line of its step, and is a field of the JSON, apart from the
// failure the stop rule compares.
func TestTheLoopPrintsWhatTheRelayRefusedApartFromTheFailure(t *testing.T) {
	fakeScrubber(t)
	d := loop.Done{Family: "failures", Case: "c1", Step: "build", Run: store.LoopRun{ID: "r1", Status: store.RunFailed, Failure: "the model's safeguards", USD: 1.5},
		Refused: []string{`model "x" is no Claude model`}}
	var out bytes.Buffer
	printDone(&out, json.NewEncoder(&out), d, false)
	if line := out.String(); !strings.Contains(line, "failed $1.50 the model's safeguards (the relay refused model \"x\" is no Claude model)\n") {
		t.Fatalf("the line: %q", line)
	}
	out.Reset()
	printDone(&out, json.NewEncoder(&out), d, true)
	var got struct {
		Failure      string   `json:"failure"`
		RelayRefused []string `json:"relay_refused"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Failure != "the model's safeguards" || len(got.RelayRefused) != 1 {
		t.Fatalf("the object: %q %v", out.String(), err)
	}
	out.Reset()
	printDone(&out, json.NewEncoder(&out), loop.Done{Family: "failures", Case: "c1", Skipped: "no case to work"}, false)
	if got := out.String(); !strings.Contains(got, "no case to work") || strings.Contains(got, "relay") {
		t.Fatalf("a step not taken: %q", got)
	}
}
