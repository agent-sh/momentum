package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/detect"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

// The case commands open a case by its signature (the same signature again finds it), record the investigation,
// refuse what only the ship and measure tools may do, close with a reason, and merge two cases that name the same
// root cause. Every refusal exits 2 and says why.
func TestTheCaseCommands(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	t.Setenv("MOMENTUM_ACTOR", "loop:test")
	code, out, errOut := momentum("case", "open", "--family", "failures", "--signature", "failures:Bash:exit", "--title", "Bash exits",
		"--evidence", "file:/a@1", "--evidence", "file:/a@2")
	m := regexp.MustCompile(`^(c[0-9a-f]{8}): opened \[open\] Bash exits\n$`).FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("open: exit %d %q %s", code, out, errOut)
	}
	id := m[1]
	if code, out, _ := momentum("case", "open", "--family", "failures", "--signature", "failures:Bash:exit", "--title", "x",
		"--evidence", "file:/a@3"); code != 0 || out != id+": updated [open] Bash exits\n" {
		t.Fatalf("open again: exit %d %q", code, out)
	}
	if code, _, errOut := momentum("case", "set", id, "--status", "testing", "--mechanism", "the PATH lacks go"); code != 2 ||
		!strings.Contains(errOut, "next discriminating test") {
		t.Fatalf("testing without a next test: exit %d %s", code, errOut)
	}
	if code, _, errOut := momentum("case", "set", id, "--status", "investigating", "--mechanism", "the PATH lacks go",
		"--alternative", "a stale shell", "--alternative", "a missing module", "--next-test", "run with PATH set", "--benefit", "no retries"); code != 0 {
		t.Fatalf("set: exit %d %s", code, errOut)
	}
	if code, _, errOut := momentum("case", "set", id, "--status", "deployed"); code != 2 || !strings.Contains(errOut, "ship tool") {
		t.Fatalf("deployed by hand: exit %d %s", code, errOut)
	}
	if code, _, errOut := momentum("case", "close", id, "--reason", "improved", "--why", "it is better"); code != 2 ||
		!strings.Contains(errOut, "measure tool") {
		t.Fatalf("improved by hand: exit %d %s", code, errOut)
	}
	code, out, errOut = momentum("case", "show", id, "--json")
	var shown struct {
		ID           string   `json:"id"`
		Status       string   `json:"status"`
		Mechanism    string   `json:"mechanism"`
		Alternatives []string `json:"alternatives"`
		Evidence     int      `json:"evidence"`
		Signatures   []string `json:"signatures"`
		Log          []struct {
			Actor  string `json:"actor"`
			Action string `json:"action"`
		} `json:"log"`
	}
	if code != 0 {
		t.Fatalf("show: exit %d %s", code, errOut)
	}
	if err := json.Unmarshal([]byte(out), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.Status != "investigating" || shown.Mechanism != "the PATH lacks go" || len(shown.Alternatives) != 2 || shown.Evidence != 3 ||
		len(shown.Log) != 3 || shown.Log[0].Actor != "loop:test" || shown.Log[2].Action != "set" {
		t.Fatalf("show: %+v", shown)
	}

	_, out, _ = momentum("case", "open", "--family", "failures", "--signature", "failures:go:missing", "--title", "go is missing",
		"--evidence", "file:/b@1")
	other := strings.SplitN(out, ":", 2)[0]
	if code, out, _ := momentum("case", "signature", id, "root:path-without-go"); code != 0 || !strings.Contains(out, "signature added") {
		t.Fatalf("signature: exit %d %q", code, out)
	}
	if code, out, _ := momentum("case", "signature", other, "root:path-without-go"); code != 0 || !strings.HasPrefix(out, id+": "+other+" went into") {
		t.Fatalf("a second case with the root: exit %d %q", code, out)
	}
	if code, out, _ := momentum("case", "list"); code != 0 || strings.Count(out, "\n") != 1 || !strings.Contains(out, id) {
		t.Fatalf("list: exit %d %q", code, out)
	}
	if code, out, _ := momentum("case", "list", "--all"); code != 0 || !strings.Contains(out, "closed: duplicate of "+id) {
		t.Fatalf("list --all: exit %d %q", code, out)
	}
	if code, out, _ := momentum("case", "list", "--status", "closed"); code != 0 || !strings.HasPrefix(out, other) || strings.Contains(out, "\n"+id) {
		t.Fatalf("list --status closed: exit %d %q", code, out)
	}
	if code, out, _ := momentum("case", "list", "--status", "investigating,open"); code != 0 || strings.Contains(out, other) || !strings.HasPrefix(out, id) {
		t.Fatalf("list --status investigating,open: exit %d %q", code, out)
	}
	if code, _, errOut := momentum("case", "show", id, other, "--json"); code != 2 || !strings.Contains(errOut, "one case's id") {
		t.Fatalf("show with two ids: exit %d %s", code, errOut)
	}
	_, out, _ = momentum("case", "open", "--family", "failures", "--signature", "failures:third", "--title", "a third", "--evidence", "file:/c@1")
	third := strings.SplitN(out, ":", 2)[0]
	if code, out, _ := momentum("case", "close", third, "--reason", "duplicate", "--into", id, "--why", "same"); code != 0 ||
		!strings.HasPrefix(out, id+": "+third+" went into this case") {
		t.Fatalf("close --into: exit %d %q", code, out)
	}
	if code, _, errOut := momentum("case", "close", id, "--reason", "not-worth-it"); code != 2 || !strings.Contains(errOut, "--why") {
		t.Fatalf("close without why: exit %d %s", code, errOut)
	}
	if code, out, _ := momentum("case", "close", id, "--reason", "not-worth-it", "--why", "rare"); code != 0 ||
		!strings.Contains(out, "[closed: not-worth-it]") {
		t.Fatalf("close: exit %d %q", code, out)
	}
	if code, _, errOut := momentum("case", "reopen", id, "--evidence", "file:/a@1", "--why", "again"); code != 2 || !strings.Contains(errOut, "does not hold") {
		t.Fatalf("reopen on old evidence: exit %d %s", code, errOut)
	}
	if code, out, _ := momentum("case", "reopen", id, "--evidence", "file:/a@9", "--at", "2026-01-02T03:04", "--why", "it came back"); code != 0 ||
		!strings.Contains(out, "reopened [open]") {
		t.Fatalf("reopen: exit %d %q", code, out)
	}
	if code, out, _ := momentum("case", "show", id, "--json"); code != 0 || !strings.Contains(out, `"back":`) {
		t.Fatalf("a reopened case shows when it came back: %q", out)
	}
	if code, out, _ := momentum("case", "show", id, "--json", "--evidence", "0"); code != 0 || !strings.Contains(out, `"ref":"file:/a@9","at":"2026-01-02T03:04`) {
		t.Fatalf("reopen --at dates the evidence: %q", out)
	}
	if code, _, errOut := momentum("case", "show", "c00000000"); code != 2 || !strings.Contains(errOut, "no case") {
		t.Fatalf("show of an unknown case: exit %d %s", code, errOut)
	}
	if code, _, errOut := momentum("case", "reopen", id, "--evidence", "file:/a@10"); code != 2 || !strings.Contains(errOut, "--why") {
		t.Fatalf("reopen without why: exit %d %s", code, errOut)
	}
	for _, args := range [][]string{{"--status", "bogus"}, {"--family", "nonsense"}} {
		if code, _, errOut := momentum(append([]string{"case", "list"}, args...)...); code != 2 || !strings.Contains(errOut, "is not") {
			t.Fatalf("list %v: exit %d %s", args, code, errOut)
		}
	}
	if code, out, _ := momentum("case", "list", "--status", "open", "--family", "failures"); code != 0 || !strings.Contains(out, id) {
		t.Fatalf("list by status and family: exit %d %q", code, out)
	}
	if code, out, _ := momentum("case", "list", "--family", "sessions"); code != 0 || out != "" {
		t.Fatalf("list of another family: exit %d %q", code, out)
	}
	if code, out, errOut := momentum("case", "evidence", id, "--at", "2026-01-02T03:04", "--kind", "log", "--summary", "the run's log",
		"file:/var/log/run.log@42"); code != 0 || !strings.Contains(out, "updated [open]") {
		t.Fatalf("evidence: exit %d %q %s", code, out, errOut)
	}
	code, out, _ = momentum("case", "show", id, "--log", "2", "--json")
	var more struct {
		Recent []struct {
			Ref, At, Kind, Summary string
		} `json:"recent_evidence"`
		Log      []struct{ Action string } `json:"log"`
		LogLines int                       `json:"log_lines"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &more) != nil {
		t.Fatalf("show: exit %d %q", code, out)
	}
	found := false
	for _, e := range more.Recent {
		found = found || e.Ref == "file:/var/log/run.log@42" && e.Kind == "log" && e.Summary == "the run's log" && strings.HasPrefix(e.At, "2026-01-02T03:04")
	}
	if !found || len(more.Log) != 2 || more.LogLines < 5 || more.Log[1].Action != "evidence" {
		t.Fatalf("show --log 2: %+v", more)
	}
}

// case detect runs the detectors over the window and files what they find; --dry files nothing, --family runs only
// the families it names, and a family with no detector is a usage error.
func TestCaseDetectFilesWhatTheDetectorsFind(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MOMENTUM_HOME", home)
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Hour)
	deny := "<tool_use_error>fence-guard: a process match on 'x' takes its own shell too"
	var evs []store.Event
	for i, s := range []string{"s1", "s1", "s2"} {
		evs = append(evs, store.Event{Source: store.Transcripts, SourceKey: fmt.Sprintf("%s@%d", s, i), TS: now.Add(time.Duration(i) * time.Minute),
			Session: s, Harness: "claude", Actor: store.ActorTool, Kind: "tool_result", Tool: "Bash", Summary: deny, Meta: `{"is_error":true}`})
	}
	if _, err := st.Put(context.Background(), evs); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if code, out, errOut := momentum("case", "detect", "--dry", "--family", "owner-burden"); code != 0 || out != "" {
		t.Fatalf("one family with nothing: exit %d %q %s", code, out, errOut)
	}
	if code, out, _ := momentum("case", "detect", "--dry", "--family", "failures"); code != 0 || !strings.Contains(out, "3 ev  found  fence-guard keeps refusing") {
		t.Fatalf("dry: exit %d %q", code, out)
	}
	if code, out, _ := momentum("case", "list", "--all"); code != 0 || out != "" {
		t.Fatalf("a dry run filed: %q", out)
	}
	code, out, errOut := momentum("case", "detect", "--family", "failures,owner-burden")
	if code != 0 || !regexp.MustCompile(`c[0-9a-f]{8} opened \[open\]  fence-guard keeps refusing`).MatchString(out) {
		t.Fatalf("detect: exit %d %q %s", code, out, errOut)
	}
	if code, _, errOut := momentum("case", "detect", "--family", "weather"); code != 2 || !strings.Contains(errOut, "no detector") {
		t.Fatalf("an unknown family: exit %d %s", code, errOut)
	}
}

// partialDetector finds a finding the case store refuses, and one it files.
type partialDetector struct{}

func (partialDetector) Family() string { return "failures" }
func (partialDetector) Detect(context.Context, *store.Store, detect.Window) ([]store.Finding, error) {
	return []store.Finding{{Family: "failures", Signature: "bad\nsignature", Title: "x"},
		{Family: "failures", Signature: "failures/v1:good", Title: "good", Evidence: []store.Evidence{{Ref: "file:/t/g", At: time.Now().Add(-time.Hour)}}}}, nil
}

// brokenDetector fails without a finding.
type brokenDetector struct{}

func (brokenDetector) Family() string { return "sessions" }
func (brokenDetector) Detect(context.Context, *store.Store, detect.Window) ([]store.Finding, error) {
	return nil, errors.New("no source")
}

// case detect says what the case store refused apart from a detector that failed: the refused finding's line, its
// error in --json, and a run's error that counts it; a case swept closed has its line too. A configuration that
// cannot be read stops the run, and a dry run reads a store from before this build as it is: it migrates nothing.
func TestCaseDetectNamesWhatItRefusedAndSwept(t *testing.T) {
	home, conf := t.TempDir(), t.TempDir()
	t.Setenv("MOMENTUM_HOME", home)
	t.Setenv("MOMENTUM_CONFIG", conf)
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	old, _, err := st.ObserveCase(context.Background(), store.Finding{Family: "failures", Signature: "failures/v1:old", Title: "an old one",
		Evidence: []store.Evidence{{Ref: "file:/t/old", At: time.Now().Add(-40 * 24 * time.Hour)}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	db, err := sql.Open("sqlite", "file:"+paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(-40 * 24 * time.Hour).UnixMilli()
	if _, err := db.Exec(`UPDATE cases SET opened = ?, seen = ?`, long, long); err != nil {
		t.Fatal(err)
	}
	db.Close()
	was := listDetectors
	t.Cleanup(func() { listDetectors = was })
	listDetectors = func() ([]detect.Detector, error) { return []detect.Detector{partialDetector{}}, nil }
	code, out, errOut := momentum("case", "detect", "--json")
	var lines []filedJSON
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var j filedJSON
		if err := json.Unmarshal([]byte(l), &j); err != nil {
			t.Fatalf("%q: %v", l, err)
		}
		lines = append(lines, j)
	}
	if code != 3 || !strings.Contains(errOut, "the case store refused a finding") || strings.Contains(errOut, "detectors failed") || len(lines) != 3 ||
		!strings.Contains(lines[0].Error, "bad case") || lines[1].Error != "" || lines[1].Observed != "opened" ||
		lines[2].Case != old.ID || lines[2].Observed != "swept" {
		t.Fatalf("exit %d %s\n%+v", code, errOut, lines)
	}
	// the text lines: a refused finding's, and a failed detector's count
	listDetectors = func() ([]detect.Detector, error) { return []detect.Detector{partialDetector{}, brokenDetector{}}, nil }
	code, out, errOut = momentum("case", "detect")
	if code != 3 || !strings.Contains(out, "refused (") || !strings.Contains(errOut, "1 of 2 detectors failed") {
		t.Fatalf("text: exit %d %q %s", code, out, errOut)
	}
	listDetectors = func() ([]detect.Detector, error) { return []detect.Detector{brokenDetector{}}, nil }
	if code, _, errOut := momentum("case", "detect"); code != 3 || !strings.Contains(errOut, "1 of 1 detectors failed") {
		t.Fatalf("a failed detector alone: exit %d %s", code, errOut)
	}
	listDetectors = detectors
	if err := os.WriteFile(filepath.Join(conf, "sources.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := momentum("case", "detect", "--dry"); code == 0 || !strings.Contains(errOut, "sources.json") {
		t.Fatalf("an unreadable configuration: exit %d %s", code, errOut)
	}
	os.Remove(filepath.Join(conf, "sources.json"))
	// a store from before the cases: the dry run refuses it and leaves it so
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	db, err = sql.Open("sqlite", "file:"+paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE events (id TEXT PRIMARY KEY, source TEXT NOT NULL, source_key TEXT NOT NULL, ts INTEGER NOT NULL,
	  actor TEXT NOT NULL, kind TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if code, _, _ := momentum("case", "detect", "--dry"); code == 0 {
		t.Fatal("a dry run read a store from before this build")
	}
	db, _ = sql.Open("sqlite", "file:"+paths.DB())
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'cases'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a dry run migrated the store: %d %v", n, err)
	}
}
