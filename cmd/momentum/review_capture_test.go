package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func TestPairCaptureBoundsItsSizeAndFreezesTheFullState(t *testing.T) {
	t.Setenv("MOMENTUM_EVIDENCE_CAPTURE", t.TempDir())
	t.Setenv("MOMENTUM_NOW", "")
	dir := t.TempDir()
	first := ""
	var full store.Event
	for i := 0; i < captureMax/readMax+2; i++ {
		path := filepath.Join(dir, fmt.Sprintf("record-%d", i))
		raw := "marker-7f3a" + strings.Repeat("x", readMax-32) + "\n"
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		e := store.Event{Source: store.Transcripts, SourceKey: fmt.Sprint(i), TS: time.Unix(100, 0), Ref: path + "@0"}
		record, note, err := eventRecord(context.Background(), e)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = record
		}
		if note == captureFull {
			full = e
			// Making the record smaller afterwards must not change what the other arm sees.
			if err := os.WriteFile(path, []byte("small marker-7f3a\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if full.Ref == "" || first == "" {
		t.Fatal("capture never filled, or its first record was not shown")
	}
	record, note, err := eventRecord(context.Background(), full)
	if err != nil || record != "" || note != captureFull {
		t.Fatalf("full state changed: %q %s %v", record, note, err)
	}
	st, err := os.Stat(filepath.Join(os.Getenv("MOMENTUM_EVIDENCE_CAPTURE"), "records.json"))
	if err != nil || st.Size() > captureMax {
		t.Fatalf("capture exceeded its bound: %v %v", st, err)
	}
}

func TestPairCaptureKeepsDistinctPathsThatHoldAHash(t *testing.T) {
	t.Setenv("MOMENTUM_EVIDENCE_CAPTURE", t.TempDir())
	t.Setenv("MOMENTUM_NOW", "")
	dir := t.TempDir()
	for _, name := range []string{"records#a", "records#b"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		e := store.Event{Source: store.Transcripts, TS: time.Unix(100, 0), Ref: path + "@0#0"}
		record, note, err := eventRecord(context.Background(), e)
		if err != nil || record != name+"\n" || note != "" {
			t.Fatalf("record identity collided: %q %s %v", record, note, err)
		}
	}
}

func TestPairedEvidenceShowKeepsItsFirstRawRecord(t *testing.T) {
	for _, kind := range []string{"transcript", "hermes", "withheld", "unavailable"} {
		t.Run(kind, func(t *testing.T) {
			home := hermesHome(t)
			fakeScrubber(t)
			at := time.Now().Add(-time.Hour).Truncate(time.Second)
			trialEnds(t, at.Add(time.Minute))
			pair := t.TempDir()
			t.Setenv("MOMENTUM_EVIDENCE_CAPTURE", pair)
			line := `{"timestamp":"` + at.Format(time.RFC3339) + `","text":"capture-row-2c9b"}` + "\n"
			path := filepath.Join(home, "records.jsonl")
			e := store.Event{Source: store.Transcripts, SourceKey: "row", TS: at, Actor: store.ActorAgent, Kind: "agent_text",
				Summary: "symptom", Ref: path + "@0"}
			if kind == "hermes" || kind == "withheld" {
				path = hermesStore(t, home, hermesMessage{"session", "capture-row-2c9b", nil, hermesTS(at)})
				e = hermesEvent(path, 1, "session", at, "capture-row-2c9b", "")
				if kind == "withheld" {
					db, _ := sql.Open("sqlite", path)
					if _, err := db.Exec(`UPDATE messages SET content = 'later version'`); err != nil {
						t.Fatal(err)
					}
					db.Close()
				}
			} else if kind != "unavailable" {
				if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			putEvents(t, e)
			code, first, diagnostic := momentum("evidence", "show", e.ID())
			if code != 0 {
				t.Fatalf("first read: %d %s", code, diagnostic)
			}
			if kind == "transcript" || kind == "hermes" {
				if !strings.Contains(first, "capture-row-2c9b") {
					t.Fatalf("fixture not read: %s", first)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else if kind == "withheld" {
				db, _ := sql.Open("sqlite", path)
				if _, err := db.Exec(`UPDATE messages SET content = 'capture-row-2c9b'`); err != nil {
					t.Fatal(err)
				}
				db.Close()
			} else {
				if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// The other arm has its own copy of the event store, but shares the pair's capture.
			t.Setenv("MOMENTUM_HOME", t.TempDir())
			putEvents(t, e)
			code, second, diagnostic := momentum("evidence", "show", e.ID())
			if code != 0 || second != first {
				t.Fatalf("arms read different raw evidence:\nfirst=%s\nsecond=%s\n%d %s", first, second, code, diagnostic)
			}
		})
	}
}
