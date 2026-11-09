package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/momentum/sessions"
)

// putEvents stores events in the store the commands use.
func putEvents(t *testing.T, evs ...store.Event) {
	t.Helper()
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Put(context.Background(), evs); err != nil {
		t.Fatal(err)
	}
}

func countEvents(t *testing.T, source string) int {
	t.Helper()
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	evs, err := st.Events(context.Background(), store.Query{Since: time.Unix(0, 0), Source: source, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return len(evs)
}

// The transcripts' events are kept: a rebuild of them without --force keeps every one and drops only where the
// ingest stopped, so it reads every transcript still there again; with it, it says how many of them have no source
// left, as the ingest of the configured homes finds them. A transcript whose compressed copy is still there is not
// among them; one deleted, or kept outside every home, is.
func TestARebuildOfTheTranscriptsKeepsThemWithoutForceAndCountsWhatIsGone(t *testing.T) {
	ctx := context.Background()
	cfg, user := t.TempDir(), t.TempDir()
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", cfg)
	t.Setenv("TACIT_HERMES_DB", "off")
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	if err := os.WriteFile(filepath.Join(cfg, "sources.json"), []byte(`{"transcripts": {"homes": ["`+user+`"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	files := filepath.Join(user, ".claude", "projects", "-home-u-shop")
	if err := os.MkdirAll(files, 0o700); err != nil {
		t.Fatal(err)
	}
	there, packed, gone := filepath.Join(files, "there.jsonl"), filepath.Join(files, "packed.jsonl"), filepath.Join(files, "gone.jsonl")
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	for _, p := range []string{there, packed + ".zst", outside} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var evs []store.Event
	for i, p := range []string{there, packed, gone, outside} {
		evs = append(evs, store.Event{Source: "transcripts", SourceKey: fmt.Sprintf("%s@%012d#000", filepath.Base(p), i),
			TS: time.UnixMilli(1_700_000_000_000), Harness: "claude", Actor: store.ActorAgent, Kind: "agent_text", Ref: fmt.Sprintf("%s@%d#0", p, i)})
	}
	putEvents(t, evs...)
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = st.PutFiles(ctx, []store.FileWrite{{Cursor: store.FileCursor{Source: store.Transcripts, Path: there, Offset: 3}}})
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run(ctx, []string{"evidence", "rebuild", "transcripts"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "kept every event") {
		t.Fatalf("exit %d, without --force:\n%s%s", code, out.String(), errOut.String())
	}
	if n := countEvents(t, "transcripts"); n != 4 {
		t.Fatalf("a rebuild without --force left %d events", n)
	}
	if st, err = store.OpenReadOnly(paths.DB()); err != nil {
		t.Fatal(err)
	}
	cursors, err := st.FileCursors(ctx, store.Transcripts)
	st.Close()
	if err != nil || len(cursors) != 0 {
		t.Fatalf("where the ingest stopped is kept: %v %v", cursors, err)
	}
	out.Reset()
	errOut.Reset()
	if code := run(ctx, []string{"evidence", "rebuild", "--force=true", "transcripts"}, &out, &errOut); code != 0 ||
		!strings.Contains(out.String(), "dropped 4 events; 2 of them have no source left") {
		t.Fatalf("exit %d, with --force:\n%s%s", code, out.String(), errOut.String())
	}
	if n := countEvents(t, "transcripts"); n != 0 {
		t.Fatalf("a forced rebuild left %d events", n)
	}
}

// A rebuild of the transcripts, forced or not, is refused while a case's change is deployed or watched, and names the
// case: a read again rewrites the events its measure step judges the change on. It keeps every event and every cursor.
// Once the watch closes, the rebuild runs.
func TestARebuildOfTheTranscriptsIsRefusedWhileACaseIsWatched(t *testing.T) {
	ctx := context.Background()
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	at := time.Now().Add(-time.Hour)
	e := store.Event{Source: store.Transcripts, SourceKey: "s.jsonl@000000000000#000", TS: at, Harness: "claude", Actor: store.ActorTool,
		Kind: "tool_result", Tool: "Bash", Summary: "Exit code 1 it broke"}
	putEvents(t, e)
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, _, err := st.PutFiles(ctx, []store.FileWrite{{Cursor: store.FileCursor{Source: store.Transcripts, Path: "/s.jsonl", Offset: 3}}}); err != nil {
		t.Fatal(err)
	}
	c, _, err := st.ObserveCase(ctx, store.Finding{Family: "failures", Signature: "failures/v2:zz", Title: "it breaks",
		Evidence: []store.Evidence{{Ref: "ev:" + e.ID()}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	mech, next := "m", "n"
	if _, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeployCase(ctx, c.ID, t.TempDir(), at, "landed", "test"); err != nil {
		t.Fatal(err)
	}
	refused := func(stage string) {
		t.Helper()
		for _, args := range [][]string{{"evidence", "rebuild", "transcripts"}, {"evidence", "rebuild", "transcripts", "--force"}} {
			var out, errOut bytes.Buffer
			if code := run(ctx, args, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), c.ID+" ("+stage+")") {
				t.Fatalf("%v while %s: exit %d\n%s%s", args, stage, code, out.String(), errOut.String())
			}
		}
		if n := countEvents(t, store.Transcripts); n != 1 {
			t.Fatalf("a refused rebuild left %d events", n)
		}
		if cursors, err := st.FileCursors(ctx, store.Transcripts); err != nil || len(cursors) != 1 {
			t.Fatalf("a refused rebuild dropped where the ingest stopped: %v %v", cursors, err)
		}
	}
	refused("deployed")
	if _, err := st.StartGuarding(ctx, c.ID, 30*24*time.Hour, 4, 684, at, "a guard", "test"); err != nil {
		t.Fatal(err)
	}
	refused("watched by its guard")
	if _, err := st.CloseGuarded(ctx, c.ID, 4, 684, 100, 0.5, "no recurrence", "", "test"); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run(ctx, []string{"evidence", "rebuild", "transcripts"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "kept every event") {
		t.Fatalf("after the guarded close: exit %d\n%s%s", code, out.String(), errOut.String())
	}
}

// The incidents' sightings are kept too: what each incident did over time is not in its file. A rebuild of them is
// refused without --force, and with it says the sightings are gone.
func TestARebuildOfTheIncidentsNeedsForce(t *testing.T) {
	ctx := context.Background()
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	at := time.Now().Add(-time.Hour)
	putEvents(t, store.Event{Source: store.Incidents, SourceKey: "inc-1", TS: at, Actor: store.ActorDetector, Kind: "incident"},
		store.Event{Source: store.Incidents, SourceKey: "inc-1@x#3", TS: at, Actor: store.ActorDetector, Kind: "incident-seen"})
	var out, errOut bytes.Buffer
	if code := run(ctx, []string{"evidence", "rebuild", "incidents"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "--force") ||
		!strings.Contains(errOut.String(), "sightings") {
		t.Fatalf("exit %d, without --force:\n%s%s", code, out.String(), errOut.String())
	}
	if n := countEvents(t, store.Incidents); n != 2 {
		t.Fatalf("a refused rebuild left %d events", n)
	}
	out.Reset()
	if code := run(ctx, []string{"evidence", "rebuild", "incidents", "--force"}, &out, &errOut); code != 0 ||
		!strings.Contains(out.String(), "dropped 2 events; their sightings are gone for good") {
		t.Fatalf("exit %d, with --force:\n%s%s", code, out.String(), errOut.String())
	}
}

// A query by harness takes its one name, and finds its events from every source; another name is refused.
func TestEventsOfAHarnessFromEverySource(t *testing.T) {
	ctx := context.Background()
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	at := time.Now().Add(-time.Hour)
	putEvents(t,
		store.Event{Source: "mistakes", SourceKey: "m", TS: at, Harness: "claude", Actor: store.ActorTool, Kind: "tool-error"},
		store.Event{Source: "sessions", SourceKey: "s@1", TS: at, Harness: "claude", Actor: store.ActorHarness, Kind: "phase"},
		store.Event{Source: "transcripts", SourceKey: "a@000000000000#000", TS: at, Harness: "claude", Actor: store.ActorOwner, Kind: "owner_text"},
		store.Event{Source: "transcripts", SourceKey: "r@000000000000#000", TS: at, Harness: "codex", Actor: store.ActorOwner, Kind: "owner_text"})
	var out, errOut bytes.Buffer
	if code := run(ctx, []string{"evidence", "events", "--harness", "claude", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var sources []string
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		for _, s := range []string{"mistakes", "sessions", "transcripts"} {
			if strings.Contains(l, `"Source":"`+s+`"`) {
				sources = append(sources, s)
			}
		}
	}
	slices.Sort(sources)
	if strings.Join(sources, " ") != "mistakes sessions transcripts" {
		t.Fatalf("Claude Code's events came from %v:\n%s", sources, out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run(ctx, []string{"evidence", "events", "--harness", "claude-code"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "unknown harness") {
		t.Fatalf("exit %d, a harness by another name:\n%s%s", code, out.String(), errOut.String())
	}
}

// The sessions layer names each harness by its tool, as the sources name it.
func TestTheSessionsLayerNamesEachHarnessAsTheSourcesDo(t *testing.T) {
	for _, s := range sessionSources(sessions.Env{}) {
		if !slices.Contains(store.Harnesses, s.Tool()) {
			t.Errorf("the sessions layer names a harness %q, not one of %v", s.Tool(), store.Harnesses)
		}
		s.Close()
	}
}
