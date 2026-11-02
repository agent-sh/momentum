package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/store"
)

// scrubbing stands a sed program in for the secret scrubber.
func scrubbing(t *testing.T, program string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "scrub")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nsed '"+program+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	was := loop.Scrubber
	loop.Scrubber = []string{p}
	t.Cleanup(func() { loop.Scrubber = was })
}

// evidence git takes the forms agents write as git takes them: -N for the most commits, grep's first word as its
// pattern, and git's own no (a grep that matched nothing, an ancestry that does not hold) as an answer, exit 1. A
// refused option names the options the command runs with.
func TestEvidenceGitTakesGitsOwnForms(t *testing.T) {
	fakeScrubber(t)
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	for i, text := range []string{"alpha\n", "alpha\nneedle here\n", "alpha\nneedle here\nomega\n"} {
		os.WriteFile(filepath.Join(dir, "a.txt"), []byte(text), 0o600)
		gitIn(t, dir, "add", "a.txt")
		gitIn(t, dir, "commit", "-q", "-m", []string{"first", "second", "third"}[i])
	}
	if code, out, errOut := momentum("evidence", "git", dir, "log", "--oneline", "-2"); code != 0 || strings.Count(strings.TrimSpace(out), "\n") != 1 || strings.Contains(out, "first") {
		t.Errorf("log -2: exit %d %q %s", code, out, errOut)
	}
	if code, out, errOut := momentum("evidence", "git", dir, "rev-list", "-1", "HEAD"); code != 0 || strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Errorf("rev-list -1: exit %d %q %s", code, out, errOut)
	}
	if code, out, errOut := momentum("evidence", "git", dir, "grep", "-n", "needle"); code != 0 || !strings.Contains(out, "a.txt:2:needle here") {
		t.Errorf("grep's first word is its pattern: exit %d %q %s", code, out, errOut)
	}
	if code, out, errOut := momentum("evidence", "git", dir, "grep", "-n", "needle", "HEAD~1", "--", "a.txt"); code != 0 || !strings.Contains(out, "needle here") {
		t.Errorf("a pattern, then a revision and a path: exit %d %q %s", code, out, errOut)
	}
	if code, out, errOut := momentum("evidence", "git", dir, "grep", "-i", "absent-word"); code != 1 || out != "" || !strings.Contains(errOut, "no match") || strings.Contains(errOut, "[ERROR]") {
		t.Errorf("a grep with no match: exit %d %q %s", code, out, errOut)
	}
	if code, _, errOut := momentum("evidence", "git", dir, "merge-base", "--is-ancestor", "HEAD", "HEAD~1"); code != 1 || !strings.Contains(errOut, "not an ancestor") {
		t.Errorf("an ancestry that does not hold: exit %d %s", code, errOut)
	}
	if code, _, errOut := momentum("evidence", "git", dir, "log", "--frobnicate"); code != 2 || !strings.Contains(errOut, "--oneline") || !strings.Contains(errOut, "-N") {
		t.Errorf("a refused option names the options: exit %d %s", code, errOut)
	}
	for _, args := range [][]string{{"log", "-2x"}, {"show", "-2"}, {"grep", "-n", "needle", "not-a-revision"}} {
		if code, _, _ := momentum(append([]string{"evidence", "git", dir}, args...)...); code != 2 {
			t.Errorf("git %v: exit %d, want 2", args, code)
		}
	}
}

// grep with -e anywhere takes every other word as a revision, as git does.
func TestEvidenceGitGrepWithDashEReadsTheOtherWordsAsRevisions(t *testing.T) {
	fakeScrubber(t)
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("gamma\n"), 0o600)
	gitIn(t, dir, "add", "a.txt")
	gitIn(t, dir, "commit", "-q", "-m", "first")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("delta\n"), 0o600) // the work tree no longer holds it
	if code, out, diag := momentum("evidence", "git", dir, "grep", "-n", "HEAD", "-e", "gamma"); code != 0 || !strings.Contains(out, "HEAD:a.txt:1:gamma") {
		t.Errorf("grep HEAD -e: exit %d %q %s", code, out, diag)
	}
}

// shortlog with no revision counts from HEAD, as log does, and so does its -N form: git reads the standard input of
// a shortlog given no revision, which is nothing here, and prints nothing.
func TestEvidenceGitShortlogCountsFromHeadWithNoRevision(t *testing.T) {
	fakeScrubber(t)
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	for i := 0; i < 4; i++ {
		os.WriteFile(filepath.Join(dir, "a.txt"), []byte(strconv.Itoa(i)), 0o600)
		gitIn(t, dir, "add", "a.txt")
		gitIn(t, dir, "commit", "-q", "-m", "change "+strconv.Itoa(i))
	}
	for _, args := range [][]string{{"shortlog", "-s", "-n"}, {"shortlog", "-2"}, {"shortlog", "-s", "-n", "HEAD"}, {"shortlog", "-s", "--", "a.txt"}} {
		code, out, diag := momentum(append([]string{"evidence", "git", dir}, args...)...)
		if code != 0 || strings.TrimSpace(out) == "" {
			t.Errorf("git %v: exit %d %q %s", args, code, out, diag)
		}
	}
	if _, out, _ := momentum("evidence", "git", dir, "shortlog", "-s", "-n"); !strings.Contains(out, "4") {
		t.Errorf("the count of the four commits: %q", out)
	}
	if _, out, _ := momentum("evidence", "git", dir, "shortlog", "-s", "-n", "-2"); !strings.Contains(out, "2") {
		t.Errorf("the count of the last two: %q", out)
	}
}

// evidence events takes an actor, as search does, and prints each event's id, which evidence show takes; a kind no
// event has is refused with the kinds the store holds, by events and search alike; a list of kinds with spaces is a
// list.
func TestEvidenceEventsTakesAnActorAndNamesEachEvent(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	at := time.Now().Add(-time.Hour)
	owner := store.Event{Source: store.Transcripts, SourceKey: "o", TS: at, Session: "s1", Actor: store.ActorOwner, Kind: "owner_text", Summary: "why did the export stall"}
	agent := store.Event{Source: store.Transcripts, SourceKey: "a", TS: at.Add(time.Minute), Session: "s1", Actor: store.ActorAgent, Kind: "agent_text", Summary: "the export stalls on its lock"}
	putEvents(t, owner, agent)
	code, out, errOut := momentum("evidence", "events", "--since", "1d", "--actor", "owner")
	if code != 0 || !strings.Contains(out, "ev:"+owner.ID()) || strings.Contains(out, agent.ID()) {
		t.Fatalf("one actor's events, with their ids: exit %d %q %s", code, out, errOut)
	}
	if code, _, errOut := momentum("evidence", "events", "--actor", "person"); code != 2 || !strings.Contains(errOut, "owner, agent") {
		t.Errorf("an unknown actor: exit %d %s", code, errOut)
	}
	for _, args := range [][]string{{"evidence", "events", "--kind", "owner_prompt"}, {"evidence", "search", "export", "--kind", "owner_prompt"}} {
		if code, _, errOut := momentum(args...); code != 2 || !strings.Contains(errOut, "agent_text, owner_text") {
			t.Errorf("%v: exit %d %s", args, code, errOut)
		}
	}
	if code, _, errOut := momentum("evidence", "events", "export", "--since", "1d"); code != 2 || !strings.Contains(errOut, "evidence search") {
		t.Errorf("a phrase given to events: exit %d %s", code, errOut)
	}
	if _, out, _ := momentum("evidence", "events", "--since", "1d", "--kind", "owner_text, agent_text"); !strings.Contains(out, "why did") || !strings.Contains(out, "export stalls") {
		t.Errorf("two spaced kinds: %q", out)
	}
	if _, out, _ := momentum("evidence", "events", "--since", "1d", "--json"); !strings.Contains(out, `"ID":"`+owner.ID()+`"`) {
		t.Errorf("a JSON line's id: %q", out)
	}
}

// A store with no event refuses no kind: it has none to name, and a fresh store prints nothing, not an error.
func TestEvidenceEventsOfAnEmptyStoreRefuseNoKind(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	for _, args := range [][]string{{"evidence", "events", "--since", "1d", "--kind", "owner_text"}, {"evidence", "search", "export", "--kind", "owner_text"}} {
		if code, out, errOut := momentum(args...); code != 0 {
			t.Errorf("%v: exit %d %q %s", args, code, out, errOut)
		}
	}
}

// evidence search takes a phrase of two characters (a program's name), and names the form for a phrase that starts
// with a dash; a phrase too short is refused for its length, not for a count of phrases.
func TestEvidenceSearchTakesShortPhrasesAndNamesTheDashForm(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	putEvents(t, store.Event{Source: store.Transcripts, SourceKey: "c", TS: time.Now().Add(-time.Hour), Session: "s1", Actor: store.ActorTool,
		Kind: "tool_result", Summary: "uv: prune --force refused"})
	if code, out, errOut := momentum("evidence", "search", "uv"); code != 0 || !strings.Contains(out, "prune") {
		t.Errorf("a two-letter phrase: exit %d %q %s", code, out, errOut)
	}
	if code, _, errOut := momentum("evidence", "search", "--force"); code != 2 || !strings.Contains(errOut, "-- PHRASE") {
		t.Errorf("a dash phrase: exit %d %s", code, errOut)
	}
	if code, out, errOut := momentum("evidence", "search", "--", "--force"); code != 0 || !strings.Contains(out, "prune") {
		t.Errorf("a dash phrase after --: exit %d %q %s", code, out, errOut)
	}
	if code, _, errOut := momentum("evidence", "search", "u"); code != 2 || !strings.Contains(errOut, "2 characters") || strings.Contains(errOut, "1 phrases") {
		t.Errorf("a phrase of one character: exit %d %s", code, errOut)
	}
	if code, _, errOut := momentum("evidence", "search", "uv", "prune"); code != 2 || !strings.Contains(errOut, "one phrase") {
		t.Errorf("two phrases: exit %d %s", code, errOut)
	}
}

// evidence show prints several events in one call, each record scrubbed whole and then cut to --bytes; an id that
// is a session's or a record's place says what reads it; eleven are more than a call takes.
func TestEvidenceShowTakesSeveralEventsAndNamesWhatReadsAPlace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	at := time.Now().Add(-time.Hour)
	transcript := filepath.Join(home, "t.jsonl")
	first := `{"type":"user","timestamp":"` + at.UTC().Format(time.RFC3339Nano) + `","text":"first record secret-value and a long tail of words"}` + "\n"
	second := `{"type":"assistant","timestamp":"` + at.UTC().Format(time.RFC3339Nano) + `","text":"second record"}` + "\n"
	os.WriteFile(transcript, []byte(first+second), 0o600)
	a := store.Event{Source: store.Transcripts, SourceKey: "a", TS: at, Session: "s1", Actor: store.ActorOwner, Kind: "owner_text", Summary: "first", Ref: transcript + "@0#0"}
	b := store.Event{Source: store.Transcripts, SourceKey: "b", TS: at, Session: "s1", Actor: store.ActorAgent, Kind: "agent_text", Summary: "second", Ref: transcript + "@" + strconv.Itoa(len(first)) + "#0"}
	putEvents(t, a, b)
	code, out, errOut := momentum("evidence", "show", a.ID(), "ev:"+b.ID())
	if code != 0 || !strings.Contains(out, "ev:"+a.ID()) || !strings.Contains(out, "ev:"+b.ID()) || !strings.Contains(out, "second record") || strings.Contains(out, "secret-value") {
		t.Fatalf("two events: exit %d %q %s", code, out, errOut)
	}
	if code, out, errOut := momentum("evidence", "show", a.ID(), "--bytes", "40"); code != 0 || strings.Contains(out, "long tail") || !strings.Contains(out, "its first 40 of") {
		t.Errorf("a cut record: exit %d %q %s", code, out, errOut)
	}
	if code, _, errOut := momentum("evidence", "show", "0f6b2c1e-1a2b-4c3d-8e9f-001122334455"); code != 2 || !strings.Contains(errOut, "--session") {
		t.Errorf("a session's id: exit %d %s", code, errOut)
	}
	if code, out, errOut := momentum("evidence", "show", a.ID(), "0f6b2c1e-1a2b-4c3d-8e9f-001122334455@000012#000"); code != 0 || !strings.Contains(out, "first record") || !strings.Contains(out, "a record's place") {
		t.Errorf("an event and a place: exit %d %q %s", code, out, errOut)
	}
	var ids []string
	for i := 0; i < 11; i++ {
		ids = append(ids, a.ID())
	}
	if code, out, errOut := momentum(append([]string{"evidence", "show"}, ids...)...); code != 2 || out != "" || !strings.Contains(errOut, "1 to 10") {
		t.Errorf("eleven events: exit %d %q %s", code, out, errOut)
	}
	if code, out, errOut := momentum(append([]string{"evidence", "show"}, ids[:10]...)...); code != 0 || strings.Count(out, "ev:"+a.ID()) != 10 {
		t.Errorf("ten events: exit %d %d heads %s", code, strings.Count(out, "ev:"+a.ID()), errOut)
	}
}

// evidence show takes every id after --, as search takes its phrase there, and a lone - as a word that names no
// event: no id is left out without a word.
func TestEvidenceShowReadsTheIdsAfterTheEndOfItsFlags(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	fakeScrubber(t)
	at := time.Now().Add(-time.Hour)
	transcript := filepath.Join(home, "t.jsonl")
	first, second := `{"text":"the first record"}`+"\n", `{"text":"the second record"}`+"\n"
	os.WriteFile(transcript, []byte(first+second), 0o600)
	a := store.Event{Source: store.Transcripts, SourceKey: "a", TS: at, Session: "s1", Actor: store.ActorAgent, Kind: "agent_text", Summary: "one", Ref: transcript + "@0#0"}
	b := store.Event{Source: store.Transcripts, SourceKey: "b", TS: at.Add(time.Minute), Session: "s1", Actor: store.ActorAgent, Kind: "agent_text", Summary: "two",
		Ref: transcript + "@" + strconv.Itoa(len(first)) + "#0"}
	putEvents(t, a, b)
	for _, args := range [][]string{{"--", a.ID(), b.ID()}, {a.ID(), "--", b.ID()}, {a.ID(), "--bytes", "40", "--", b.ID()}} {
		code, out, diag := momentum(append([]string{"evidence", "show"}, args...)...)
		if code != 0 || !strings.Contains(out, "the first record") || !strings.Contains(out, "the second record") {
			t.Errorf("%v: exit %d %q %s", args, code, out, diag)
		}
	}
	code, out, diag := momentum("evidence", "show", a.ID(), "-", b.ID())
	if code != 0 || !strings.Contains(out, "the first record") || !strings.Contains(out, "the second record") || !strings.Contains(out, "-: ") {
		t.Errorf("a lone dash: exit %d %q %s", code, out, diag)
	}
	for _, args := range [][]string{{a.ID(), "--bytes"}, {a.ID(), "--bytes", "x"}, {"--bytes", "5"}, {a.ID(), "-x", b.ID()}, {"-"}, {"--"}} {
		if code, _, _ := momentum(append([]string{"evidence", "show"}, args...)...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// A record is scrubbed whole before --bytes cuts it: a secret the cut would split is masked as the scrubber sees it
// whole, so no first part of it is printed; and a cut backs off only a rune it splits, not to an invalid byte before.
func TestEvidenceShowScrubsARecordWholeBeforeItsCut(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	scrubbing(t, `s/alpha-value-0001/****************/g`) // only the whole word
	transcript := filepath.Join(home, "t.jsonl")
	first := `{"text":"the value is alpha-value-0001 and more words after it"}` + "\n"
	os.WriteFile(transcript, []byte(first+"{\"text\":\"a \xff byte, then a long tail of words\"}\n"), 0o600)
	at := time.Now().Add(-time.Hour)
	e := store.Event{Source: store.Transcripts, SourceKey: "e", TS: at, Session: "s1", Actor: store.ActorAgent, Kind: "agent_text", Summary: "the value", Ref: transcript + "@0#0"}
	f := store.Event{Source: store.Transcripts, SourceKey: "f", TS: at, Session: "s1", Actor: store.ActorAgent, Kind: "agent_text", Summary: "a byte",
		Ref: transcript + "@" + strconv.Itoa(len(first)) + "#0"}
	putEvents(t, e, f)
	code, out, diag := momentum("evidence", "show", e.ID(), "--bytes", "31")
	if code != 0 || strings.Contains(out, "alpha-val") || !strings.Contains(out, "the value is ***") {
		t.Fatalf("exit %d %q %s", code, out, diag)
	}
	if _, out, _ := momentum("evidence", "show", f.ID(), "--bytes", "20"); !strings.Contains(out, "its first 20 of") {
		t.Errorf("a cut past an invalid byte: %q", out)
	}
}
