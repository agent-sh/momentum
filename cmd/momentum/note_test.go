package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"

	"github.com/agent-sh/momentum/sessions"
)

// momentum runs one command and returns its exit code, stdout and stderr.
func momentum(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// note add prints the id of the note it stored; an id given again replaces that note. note done takes a note out
// of the report, once. note list prints every note, newest first, and whether the report shows it.
func TestNotesAreAddedDoneAndListed(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	onMachine(t, func() []sessions.Source { return nil })
	code, id, errOut := momentum("note", "add", "--kind", "needs", "--title", "Rent a GPU for the eval?", "--detail", "The eval\nwaits on it.",
		"--link", "eval plan=https://example.com/plan?x=1&y=2", "--link", "PR #9=https://example.com/pr/9", "--importance", "high")
	id = strings.TrimSpace(id)
	if code != 0 || !regexp.MustCompile(`^n[0-9a-f]{8}$`).MatchString(id) {
		t.Fatalf("add: exit %d, id %q: %s", code, id, errOut)
	}
	if code, out, errOut := momentum("note", "add", "--kind", "shipped", "--title", "Old title", "--id", "ship-1"); code != 0 || out != "ship-1\n" {
		t.Fatalf("add with an id: exit %d %q %s", code, out, errOut)
	}
	if code, _, errOut := momentum("note", "add", "--kind", "shipped", "--title", "Scans resume", "--effect", "2.1 s -> 0.2 s", "--id", "ship-1"); code != 0 {
		t.Fatalf("add again: exit %d %s", code, errOut)
	}
	r, _ := reportJSON(t)
	if ids(r.NeedsYou) != id || ids(r.Shipped) != "ship-1" || r.Shipped[0].Title != "Scans resume" || r.Shipped[0].Effect != "2.1 s -> 0.2 s" {
		t.Fatalf("report: %+v", r)
	}
	n := r.NeedsYou[0]
	if n.Detail != "The eval waits on it." || len(n.Links) != 2 || n.Links[0] != (contractLink{"eval plan", "https://example.com/plan?x=1&y=2"}) || n.Source != "momentum" {
		t.Fatalf("the needs note: %+v", n)
	}

	if code, out, _ := momentum("note", "done", id); code != 0 || !strings.Contains(out, "done, out of the report: Rent a GPU for the eval?") {
		t.Fatalf("done: exit %d %q", code, out)
	}
	if code, out, _ := momentum("note", "done", id); code != 0 || !strings.Contains(out, "done already") {
		t.Fatalf("done again: exit %d %q", code, out)
	}
	if code, _, errOut := momentum("note", "done", "nope"); code != 2 || !strings.Contains(errOut, "no note") {
		t.Fatalf("done of an unknown id: exit %d %s", code, errOut)
	}
	if r, _ := reportJSON(t); len(r.NeedsYou) != 0 || r.Counts["needs_you"] != 0 || len(r.Shipped) != 1 {
		t.Fatalf("after done: %+v", r)
	}

	// Two notes added in the same millisecond list in either order.
	code, out, _ := momentum("note", "list")
	byID := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		byID[strings.Fields(l)[0]] = l
	}
	if code != 0 || len(byID) != 2 || !strings.HasPrefix(byID["ship-1"], "ship-1     shipped ") || !strings.HasSuffix(byID["ship-1"], "  Scans resume") ||
		!strings.Contains(byID[id], "needs    ") || !strings.HasSuffix(strings.Split(byID[id], "  [")[0], "  Rent a GPU for the eval?") ||
		!strings.Contains(byID[id], "  [high, done ") {
		t.Fatalf("list: exit %d\n%s", code, out)
	}
	code, out, _ = momentum("note", "list", "--json")
	listed := map[string]map[string]any{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("list --json: %v: %s", err, l)
		}
		listed[m["id"].(string)] = m
	}
	shipped, needs := listed["ship-1"], listed[id]
	if code != 0 || len(listed) != 2 || shipped["in_report"] != true || shipped["done"] != nil || shipped["effect"] != "2.1 s -> 0.2 s" ||
		needs["in_report"] != false || needs["importance"] != "high" || needs["done"] == nil || needs["kind"] != "needs" {
		t.Fatalf("list --json: exit %d\n%s", code, out)
	}
}

// note add refuses a note it cannot store as asked, as a usage error, and stores nothing.
func TestNoteAddRefusesWhatIsWrong(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"--title", "No kind?"}, "kind"},
		{[]string{"--kind", "fyi", "--title", "x"}, "kind"},
		{[]string{"--kind", "needs"}, "no title"},
		{[]string{"--kind", "needs", "--title", strings.Repeat("x", 121)}, "--detail"},
		{[]string{"--kind", "needs", "--title", "x", "--importance", "urgent"}, "high, normal or low"},
		{[]string{"--kind", "needs", "--title", "x", "--link", "https://example.com"}, "LABEL=URL"},
		{[]string{"--kind", "needs", "--title", "x", "--link", "run=javascript:alert(1)"}, "http or https"},
		{[]string{"--kind", "needs", "--title", "x", "--id", "two words"}, "space"},
		{[]string{"--kind", "needs", "--title", "x", "--id", "-x"}, "starts with -"},
		{[]string{"--kind", "needs", "--title", "x", "--id", "session:w"}, "running items"},
		{[]string{"--kind", "needs", "--title", "x", "--id", "task:w/bg1"}, "running items"},
		{[]string{"--kind", "needs", "--title", "x", "extra"}, "no arguments"},
	} {
		code, _, errOut := momentum(append([]string{"note", "add"}, c.args...)...)
		if code != 2 || !strings.Contains(errOut, c.says) {
			t.Errorf("%v: exit %d, %q; want 2 and %q", c.args, code, errOut, c.says)
		}
	}
	if code, out, _ := momentum("note", "list"); code != 0 || out != "" {
		t.Fatalf("a refused note was stored: %q", out)
	}
	if code, _, _ := momentum("note"); code != 2 {
		t.Fatalf("note alone: exit %d", code)
	}
	if code, _, _ := momentum("note", "done"); code != 2 {
		t.Fatalf("done without an id: exit %d", code)
	}
}

// in_report says whether the report shows a note: one past the 20 of its section is counted, not shown. note done
// says whether the note it took was in the report.
func TestNoteListSaysWhatTheReportShows(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	now := time.Now()
	var ns []store.Note
	for i := range 25 {
		ns = append(ns, store.Note{ID: fmt.Sprintf("ask-%02d", i), Kind: store.NoteNeeds, Title: "Question?", At: now.Add(time.Duration(i-30) * time.Minute)})
	}
	ns = append(ns, store.Note{ID: "dec-old", Kind: store.NoteDecided, Title: "Kept the cache", At: now.Add(-8 * 24 * time.Hour)})
	putNotes(t, ns...)
	code, out, _ := momentum("note", "list", "--json")
	shown := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		shown[m["id"].(string)] = m["in_report"].(bool)
	}
	count := 0
	for _, v := range shown {
		if v {
			count++
		}
	}
	if code != 0 || len(shown) != 26 || count != 20 || shown["ask-00"] || shown["ask-04"] || !shown["ask-05"] || shown["dec-old"] {
		t.Fatalf("in_report: exit %d, %d of %d shown: %v", code, count, len(shown), shown)
	}
	if _, out, _ := momentum("note", "list"); !strings.Contains(out, "ask-00     needs ") || !strings.Contains(out, "[counted, past the 20 the report shows]") ||
		!strings.Contains(out, "[older than the report's week]") {
		t.Fatalf("list:\n%s", out)
	}
	if code, out, _ := momentum("note", "done", "dec-old"); code != 0 || !strings.Contains(out, "it was not in the report") {
		t.Fatalf("done of a note past the window: exit %d %q", code, out)
	}
	if code, out, _ := momentum("note", "done", "ask-24"); code != 0 || !strings.Contains(out, "done, out of the report") {
		t.Fatalf("done: exit %d %q", code, out)
	}
}
