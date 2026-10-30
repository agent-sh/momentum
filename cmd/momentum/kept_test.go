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

// The transcripts' events are kept: a rebuild of them is refused without --force, and with it says how many of
// them have no source left, as the ingest of the configured homes finds them. A transcript whose compressed copy
// is still there is not among them; one deleted, or kept outside every home, is.
func TestARebuildOfTheTranscriptsNeedsForceAndCountsWhatIsGone(t *testing.T) {
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
	var out, errOut bytes.Buffer
	if code := run(ctx, []string{"evidence", "rebuild", "transcripts"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "--force") {
		t.Fatalf("exit %d, without --force:\n%s%s", code, out.String(), errOut.String())
	}
	if n := countEvents(t, "transcripts"); n != 4 {
		t.Fatalf("a refused rebuild left %d events", n)
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
