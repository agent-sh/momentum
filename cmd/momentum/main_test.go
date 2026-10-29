package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
