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

	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

func evidenceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "ordinary.txt"), []byte("password=secret-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("password=secret-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-qm", "synthetic")
	return dir
}

// evidence git joins each option's value to its option and refuses a value that reads as an option: a value is
// never another option of git's, and nothing is written outside the repository, even on a refusal.
func TestAGitOptionsValueIsNoOption(t *testing.T) {
	fakeScrubber(t)
	dir := evidenceRepo(t)
	outside := filepath.Join(t.TempDir(), "outside")
	for sub, spec := range gitSpecs {
		for _, option := range spec.values {
			for _, value := range []string{"--output=" + outside, "-o" + outside} {
				for _, args := range [][]string{{option, value}, {option + "=" + value}} {
					code, _, _ := momentum(append([]string{"evidence", "git", dir, sub}, args...)...)
					if code == 0 {
						t.Errorf("accepted %s %v", sub, args)
					}
					if _, err := os.Stat(outside); !os.IsNotExist(err) {
						t.Fatalf("%s %v wrote outside even on refusal", sub, args)
					}
				}
			}
		}
	}
	code, _, errOut := momentum("evidence", "git", dir, "log", "--pretty", "--output="+outside, "--format=format:REVIEW_MARKER")
	if code == 0 {
		t.Errorf("optional option bypass: %s", errOut)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("optional value wrote outside")
	}
	args, err := gitArgs(context.Background(), dir, "log", gitSpecs["log"], []string{"--pretty", "format:%s", "-n", "1"})
	if err != nil || !strings.Contains(strings.Join(args, " "), "--pretty=format:%s -n1") {
		t.Fatalf("ambiguous argv %v: %v", args, err)
	}
}

// What the evidence commands print passes the scrubber first, all of it: git's output and its diagnostics, an
// event's fields and its record. A credential file is in no history query, a blob selector of one is refused, and a
// scrubber that cannot run prints nothing.
func TestEveryEvidenceOutputIsScrubbed(t *testing.T) {
	fakeScrubber(t)
	dir := evidenceRepo(t)
	for _, args := range [][]string{{"show", "HEAD:ordinary.txt"}, {"log", "--format", "secret-value"}, {"grep", "--max-depth", "secret-value", "-e", "x"}} {
		_, out, diagnostic := momentum(append([]string{"evidence", "git", dir}, args...)...)
		if strings.Contains(out+diagnostic, "secret-value") {
			t.Errorf("raw Git output: %v %q %q", args, out, diagnostic)
		}
	}
	// Credential content must be absent even from broad or wildcard history queries.
	os.WriteFile(filepath.Join(dir, ".env"), []byte("credential-only-marker\n"), 0600)
	gitIn(t, dir, "commit", "-qam", "credential fixture")
	for _, args := range [][]string{{"show", "HEAD"}, {"show", "HEAD", "--", ".en*"}, {"grep", "-e", "credential-only-marker"}} {
		_, out, _ := momentum(append([]string{"evidence", "git", dir}, args...)...)
		if strings.Contains(out, "credential-only-marker") {
			t.Errorf("implicit credential content: %v %q", args, out)
		}
	}
	blob := strings.TrimSpace(gitIn(t, dir, "rev-parse", "HEAD:.env"))
	for _, args := range [][]string{{"show", blob}, {"show", ":0:.env"}, {"show", "HEAD:.env"}, {"diff", "HEAD", "--", ".env"}, {"show", "HEAD:.ssh/config"}} {
		if code, out, _ := momentum(append([]string{"evidence", "git", dir}, args...)...); code == 0 || strings.Contains(out, "secret-value") {
			t.Errorf("credential selector %v: %d %q", args, code, out)
		}
	}
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	record := filepath.Join(t.TempDir(), "record.log")
	os.WriteFile(record, []byte("secret-value\n"), 0600)
	e := store.Event{Source: store.Transcripts, SourceKey: "synthetic", TS: time.Now(), Kind: "result", Actor: store.ActorTool, Summary: "secret-value", Ref: record + "@0"}
	if _, err := st.Put(context.Background(), []store.Event{e}); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := momentum("evidence", "show", e.ID()); code != 0 || strings.Contains(out+errOut, "secret-value") {
		t.Errorf("raw event %d %q %s", code, out, errOut)
	}
	loop.Scrubber = []string{filepath.Join(t.TempDir(), "absent")}
	for _, args := range [][]string{{"evidence", "git", dir, "show", "HEAD:ordinary.txt"}, {"evidence", "show", e.ID()}} {
		if code, out, _ := momentum(args...); code == 0 || out != "" {
			t.Errorf("failed scrub emitted bytes: %d %q", code, out)
		}
	}
}

// A record is scrubbed whole before any of it is selected: a tail never starts inside a secret's line, an event's
// long record keeps its end, and a record or a unit's output too long to scrub whole is withheld.
func TestOnlyWholeRecordsAreScrubbedAndShown(t *testing.T) {
	fakeScrubber(t)
	scrub := filepath.Join(t.TempDir(), "scrub")
	os.WriteFile(scrub, []byte("#!/bin/sh\nsed 's/password=Zq7Vx9Lm2Kp4/[masked]/g'\n"), 0700)
	loop.Scrubber = []string{scrub}
	file := filepath.Join(t.TempDir(), "ordinary.log")
	value := "Zq7Vx9Lm2Kp4"
	os.WriteFile(file, []byte("password="+value+"\n"+strings.Repeat("z", readMax-len(value)-1)), 0600)
	if code, out, errOut := momentum("evidence", "file", file, "--tail", "2"); code != 0 || strings.Contains(out, value) || !strings.Contains(out, "[masked]") {
		t.Errorf("partial tail %d %q %s", code, out[:min(len(out), 200)], errOut)
	}
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	os.WriteFile(file, []byte(strings.Repeat("z", 70<<10)+" password="+value+" END\n"), 0600)
	e := store.Event{Source: store.Transcripts, SourceKey: "long", TS: time.Now(), Kind: "result", Actor: store.ActorTool, Ref: file + "@0"}
	st.Put(context.Background(), []store.Event{e})
	if code, out, errOut := momentum("evidence", "show", e.ID()); code != 0 || !strings.Contains(out, "END") || strings.Contains(out, value) {
		t.Errorf("partial event %d len=%d %s", code, len(out), errOut)
	}
	os.WriteFile(file, []byte(strings.Repeat("z", readMax+1)+"password="+value), 0600)
	if code, out, _ := momentum("evidence", "file", file); code != 0 || strings.Contains(out, "zzz") || strings.Contains(out, value) {
		t.Error("oversized record was not withheld")
	}
	// Unit output may be capped only by withholding the whole result.
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\ncat '"+file+"'\n"), 0700)
	os.WriteFile(filepath.Join(bin, "journalctl"), []byte("#!/bin/sh\nexit 0\n"), 0700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if code, out, _ := momentum("evidence", "unit", "fixture.service"); code == 0 || out != "" {
		t.Error("oversized unit output was emitted")
	}
}

// A run changes only its own case: it merges it into no other, by a close or a signature, and once another run
// merged it, its id changes nothing of the case it went into.
func TestAMergedCaseGivesARunNoAuthority(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	ctx := context.Background()
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	makeCase := func(sig string) store.Case {
		c, _, err := st.ObserveCase(ctx, store.Finding{Family: "failures", Signature: sig, Title: sig, Evidence: []store.Evidence{{Ref: "file:/fixture/" + sig, At: time.Now()}}}, "test")
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a, b := makeCase("a"), makeCase("b")
	t.Setenv("MOMENTUM_LOOP_RUN", "synthetic")
	t.Setenv("MOMENTUM_LOOP_CASE", a.ID)
	if code, _, _ := momentum("case", "close", a.ID, "--reason", "duplicate", "--into", b.ID, "--why", "synthetic"); code == 0 {
		t.Error("a run merged another case")
	}
	if code, _, _ := momentum("case", "signature", a.ID, "b"); code == 0 {
		t.Error("signature merged another case")
	}
	// Simulate an authorized merge outside this run. Its old ID must grant no writes.
	if _, err := st.CloseCase(ctx, a.ID, store.Duplicate, b.ID, "synthetic", "test"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"set", a.ID, "--title", "changed through alias"}, {"evidence", a.ID, "file:/fixture/new"}, {"signature", a.ID, "new"}} {
		if code, _, _ := momentum(append([]string{"case"}, args...)...); code == 0 {
			t.Errorf("alias write: %v", args)
		}
	}
	got, _ := st.CaseByID(ctx, b.ID)
	if got.Title != b.Title {
		t.Fatal("destination changed through alias")
	}
}

// A case's lesson is published once: after its receipt, the lesson command writes nothing to tacit.
func TestALessonIsPublishedOnce(t *testing.T) {
	// The normal lesson fixture sets up a synthetic improved case and a fake tacit.
	TestALessonIsTheLoopsObservation(t)
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs, err := st.Cases(context.Background(), store.CaseFilter{All: true})
	if err != nil || len(cs) != 1 {
		t.Fatalf("cases %v %v", cs, err)
	}
	bin := strings.Split(os.Getenv("PATH"), ":")[0]
	marker := filepath.Join(bin, "again")
	os.WriteFile(filepath.Join(bin, "tacit"), []byte(fmt.Sprintf("#!/bin/sh\necho called > '%s'\n", marker)), 0700)
	if code, _, errOut := momentum("loop", "lesson", "--case", cs[0].ID, "--text", "another attempt"); code != 0 {
		t.Fatal(errOut)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("lesson published twice")
	}
}

// A lesson whose write left no receipt is written again with the text it reserved, under its case's name, which
// tacit takes as the same fact; then its receipt ends it.
func TestALessonWhoseWriteLeftNoReceiptIsWrittenTheSame(t *testing.T) {
	TestALessonIsTheLoopsObservation(t)
	ctx := context.Background()
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c, _, err := st.ObserveCase(ctx, store.Finding{Family: "failures", Signature: "uncertain", Title: "fixture", Evidence: []store.Evidence{{Ref: "file:/fixture/uncertain", At: time.Now()}}}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE cases SET status = ?, reason = ? WHERE id = ?`, store.CaseClosed, store.Improved, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER stop_receipt BEFORE INSERT ON case_log WHEN NEW.action = 'published' BEGIN SELECT RAISE(ABORT, 'synthetic interruption'); END`); err != nil {
		t.Fatal(err)
	}
	bin := strings.Split(os.Getenv("PATH"), ":")[0]
	calls := filepath.Join(bin, "calls")
	os.WriteFile(filepath.Join(bin, "tacit"), []byte("#!/bin/sh\nprintf '%s|' \"$@\" >> '"+calls+"'\necho >> '"+calls+"'\n"), 0700)
	if code, _, _ := momentum("loop", "lesson", "--case", c.ID, "--text", "the first lesson"); code == 0 {
		t.Fatal("fixture failed to interrupt receipt")
	}
	if _, err := db.Exec(`DROP TRIGGER stop_receipt`); err != nil {
		t.Fatal(err)
	}
	// tried again with another text, it writes the text it reserved, under the same name: tacit's same fact
	if code, _, errOut := momentum("loop", "lesson", "--case", c.ID, "--text", "another lesson"); code != 0 {
		t.Fatalf("the lesson tried again: %s", errOut)
	}
	b, err := os.ReadFile(calls)
	writes := strings.Split(strings.TrimSpace(string(b)), "\n")
	if err != nil || len(writes) != 2 || writes[0] != writes[1] || !strings.Contains(writes[0], "the first lesson") ||
		!strings.HasSuffix(writes[0], "|momentum-lesson-"+strings.ToLower(c.ID)+"|") {
		t.Fatalf("tacit's writes %q %v", writes, err)
	}
	if code, out, _ := momentum("loop", "lesson", "--case", c.ID, "--text", "a third"); code != 0 || !strings.Contains(out, "published already") {
		t.Fatalf("a published lesson written again: %q", out)
	}
	if b, _ := os.ReadFile(calls); strings.Count(string(b), "\n") != 2 {
		t.Fatalf("tacit called after the receipt: %q", b)
	}
}
