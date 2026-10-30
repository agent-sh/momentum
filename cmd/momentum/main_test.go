package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestIngestRunsEverySourceAndExits3WhenOneFails(t *testing.T) {
	home, cfg, src := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("MOMENTUM_HOME", home)
	t.Setenv("MOMENTUM_CONFIG", cfg)
	log := filepath.Join(src, "events.jsonl")
	if err := os.WriteFile(log, []byte(`{"ts":"2026-01-02T10:00:00","kind":"deny","tool":"Bash","detail":"no"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(src, "radar.db") // exists, but has no findings table
	db, err := sql.Open("sqlite", "file:"+broken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE other (x)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := os.MkdirAll(filepath.Join(src, "incidents", "open"), 0o700); err != nil {
		t.Fatal(err)
	}
	conf := `{"mistakes": {"path": "` + log + `"}, "radar": {"path": "` + broken + `"}, "incidents": {"dir": "` + filepath.Join(src, "incidents") + `"}}`
	if err := os.WriteFile(filepath.Join(cfg, "sources.json"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"evidence", "ingest"}, &out, &errOut)
	if code != 3 {
		t.Fatalf("exit %d, want 3; stderr %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "mistakes: 1 new") || !strings.Contains(out.String(), "incidents: 0 new") {
		t.Fatalf("the working sources must still run:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "radar") {
		t.Fatalf("the failing source must be named: %s", errOut.String())
	}
}

func TestIngestWithoutConfigurationIsAUsageError(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"evidence", "ingest"}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2: %s", code, errOut.String())
	}
}

// The transcripts source is registered in sources.json by its homes; a session's events then read oldest
// first, from a local time.
func TestTranscriptsFromTheConfigAndASessionInOrder(t *testing.T) {
	if _, err := exec.LookPath("scrump"); err != nil {
		t.Skip("needs scrump with its text profile")
	}
	home, cfg, user := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("MOMENTUM_HOME", home)
	t.Setenv("MOMENTUM_CONFIG", cfg)
	t.Setenv("HOME", user) // the reader reads the owner's prompt history here
	t.Setenv("TACIT_HERMES_DB", "off")
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	line := func(ts, fields string) string {
		return `{` + fields + `,"timestamp":"` + ts + `","entrypoint":"cli","cwd":"/home/u/shop","sessionId":"5d1c2b3a-0000-4000-8000-00000000c1a1"}`
	}
	path := filepath.Join(user, ".claude", "projects", "-home-u-shop", "5d1c2b3a-0000-4000-8000-00000000c1a1.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := strings.Join([]string{
		line("2026-10-20T10:00:00.000Z", `"type":"user","uuid":"u0","promptSource":"typed","origin":{"kind":"human"},"message":{"role":"user","content":"keep the cache warm"}`),
		line("2026-10-20T10:00:05.000Z", `"type":"assistant","uuid":"a1","message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"command":"make warm"}}]}`),
		line("2026-10-20T10:00:06.000Z", `"type":"user","uuid":"u2","message":{"role":"user","content":[{"tool_use_id":"toolu_01","type":"tool_result","content":"no target warm","is_error":true}]}`),
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "sources.json"), []byte(`{"transcripts": {"homes": ["`+user+`"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"evidence", "ingest"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "transcripts: 3 new") {
		t.Fatalf("exit %d:\n%s%s", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := run(context.Background(), []string{"evidence", "events", "--session", "5d1c2b3a", "--since", "2026-10-20T00:00"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "owner") || !strings.Contains(lines[0], "keep the cache warm") ||
		!strings.Contains(lines[1], "tool_call Bash") || !strings.Contains(lines[2], "tool_result Bash (error)") {
		t.Fatalf("the session, oldest first:\n%s", out.String())
	}
}

func TestWhenIsADurationOrALocalTime(t *testing.T) {
	for _, s := range []string{"36h", "7d", "2026-10-20T10:00", "2026-10-20T10:00:05", "2026-10-20", "2026-10-20T10:00:05+03:00"} {
		if _, err := parseWhen(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	if got, _ := parseWhen("2026-10-20T10:00"); !got.Equal(time.Date(2026, 10, 20, 10, 0, 0, 0, time.Local)) {
		t.Errorf("a local time: %v", got)
	}
	if _, err := parseWhen("yesterday"); err == nil {
		t.Error("yesterday parsed")
	}
}
