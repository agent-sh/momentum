package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

// A record that is a SQLite file is withheld whole, and a mistakes line rewritten since it was read is withheld:
// neither is the bytes the event was read from.
func TestEvidenceShowWithholdsDatabasesAndRewrittenMistakes(t *testing.T) {
	fakeScrubber(t)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dir := t.TempDir()
	database := filepath.Join(dir, "rows.db")
	other, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	mistakes := filepath.Join(dir, "mistakes.jsonl")
	if err := os.WriteFile(mistakes, []byte(`{"text":"marker-7f3a rewritten line"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	read := sha256.Sum256([]byte(`{"text":"the line as it was read"}` + "\n"))
	for _, c := range []struct {
		ev   store.Event
		want string
	}{
		{store.Event{Source: store.Transcripts, SourceKey: "database", Ref: database + "@0"}, "database rows cannot be read"},
		{store.Event{Source: "mistakes", SourceKey: hex.EncodeToString(read[:16]), Ref: mistakes + "@0"}, "rewritten since it was read"},
	} {
		c.ev.Kind, c.ev.Actor, c.ev.TS, c.ev.Summary = "tool_result", store.ActorTool, time.Now().Add(-time.Minute), "fixture summary"
		if _, err := st.Put(context.Background(), []store.Event{c.ev}); err != nil {
			t.Fatal(err)
		}
		code, out, diag := momentum("evidence", "show", c.ev.ID())
		if code != 0 || !strings.Contains(out, c.want) || strings.Contains(out, "marker-7f3a rewritten") {
			t.Errorf("%s: %d %q %s", c.ev.SourceKey, code, out, diag)
		}
	}
}
