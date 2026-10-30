package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
)

// noteCmd writes and lists the notes the report shows: the decisions only the owner can make, the ones made on his
// behalf, what shipped and the problems momentum could not fix. Notes are kept state: nothing derives them.
func noteCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageError{"note needs add, done or list"}
	}
	switch args[0] {
	case "add":
		return noteAdd(ctx, args[1:], stdout, stderr)
	case "done":
		return noteDone(ctx, args[1:], stdout, stderr)
	case "list":
		return noteList(ctx, args[1:], stdout, stderr)
	}
	return usageError{fmt.Sprintf("unknown note command %q", args[0])}
}

// links collects --link LABEL=URL, once per flag.
type links []store.Link

func (l *links) String() string { return fmt.Sprint(*l) }
func (l *links) Set(v string) error {
	label, u, ok := strings.Cut(v, "=")
	if !ok || strings.TrimSpace(label) == "" || strings.TrimSpace(u) == "" {
		return fmt.Errorf("a link is LABEL=URL, got %q", v)
	}
	*l = append(*l, store.Link{Label: label, URL: u})
	return nil
}

// importances are the names of a note's importance.
var importances = map[string]int{"high": store.ImportanceHigh, "normal": store.ImportanceNormal, "low": store.ImportanceLow}

func importanceName(i int) string {
	for name, v := range importances {
		if v == i {
			return name
		}
	}
	return fmt.Sprint(i)
}

// noteAdd stores a note and prints its id. An id already stored is replaced, and stands again.
func noteAdd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("note add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	kind := fs.String("kind", "", "needs, decided, shipped or problem")
	title := fs.String("title", "", fmt.Sprintf("one plain line, at most %d characters; for needs, the question itself", store.TitleMax))
	detail := fs.String("detail", "", "one or two short plain sentences; for decided, how to override it")
	effect := fs.String("effect", "", "for shipped, the measured effect")
	var ls links
	fs.Var(&ls, "link", "LABEL=URL, an http or https address; repeat for more")
	id := fs.String("id", "", "the note's id; one already stored is replaced (default: a new id)")
	source := fs.String("source", store.DefaultNoteSource, "who writes it")
	importance := fs.String("importance", "normal", "high, normal or low: the order in its section, before time")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Sprintf("note add takes no arguments, got %q", fs.Arg(0))}
	}
	imp, ok := importances[*importance]
	if !ok {
		return usageError{fmt.Sprintf("--importance is high, normal or low, got %q", *importance)}
	}
	if n := len([]rune(plain.Line(*title))); n > store.TitleMax {
		return usageError{fmt.Sprintf("--title has %d characters, at most %d: put the rest in --detail", n, store.TitleMax)}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	n, err := st.PutNote(ctx, store.Note{ID: *id, Kind: store.NoteKind(*kind), Title: *title, Detail: *detail, Effect: *effect,
		Links: ls, Source: *source, Importance: imp})
	if errors.Is(err, store.ErrBadNote) {
		return usageError{err.Error()}
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, n.ID)
	return nil
}

// noteDone takes a note out of the report: it resolves a needs or problem note, and withdraws a decided or shipped one.
func noteDone(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return usageError{"note done takes one note's id"}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	now := time.Now()
	n, stood, err := st.DoneNote(ctx, args[0], now)
	if errors.Is(err, store.ErrNoNote) {
		return usageError{err.Error() + "; note list shows them"}
	}
	if err != nil {
		return err
	}
	if !stood {
		fmt.Fprintf(stdout, "%s: done already, %s\n", n.ID, n.Done.Local().Format("01-02 15:04"))
		return nil
	}
	was := n
	was.Done = time.Time{}
	if was.Standing(now.Add(-reportWindow)) {
		fmt.Fprintf(stdout, "%s: done, out of the report: %s\n", n.ID, n.Title)
	} else {
		fmt.Fprintf(stdout, "%s: done (older than the report's week, it was not in the report): %s\n", n.ID, n.Title)
	}
	return nil
}

// listedNote is a note as note list --json prints it.
type listedNote struct {
	ID         string       `json:"id"`
	Kind       string       `json:"kind"`
	Title      string       `json:"title"`
	Detail     string       `json:"detail,omitempty"`
	Effect     string       `json:"effect,omitempty"`
	At         string       `json:"at"`
	Links      []store.Link `json:"links"`
	Source     string       `json:"source"`
	Importance string       `json:"importance"`
	Done       string       `json:"done,omitempty"`
	InReport   bool         `json:"in_report"` // among the items of its section the report shows (at most 20)
}

// noteList prints every note, newest first, and whether the report shows it: a note that stands but is past the
// 20 items of its section is counted in the report, not shown.
func noteList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("note list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "one JSON object per note")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Sprintf("note list takes no arguments, got %q", fs.Arg(0))}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	ns, err := st.Notes(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	since := now.Add(-reportWindow)
	shown := map[string]bool{}
	for _, items := range noteSections(ns, now) {
		items, _ := section(items)
		for _, it := range items {
			shown[it.ID] = true
		}
	}
	enc := json.NewEncoder(stdout)
	for _, n := range ns {
		if *asJSON {
			l := listedNote{ID: n.ID, Kind: string(n.Kind), Title: n.Title, Detail: n.Detail, Effect: n.Effect, At: rfc3339(n.At),
				Links: n.Links, Source: n.Source, Importance: importanceName(n.Importance), InReport: shown[n.ID]}
			if !n.Done.IsZero() {
				l.Done = rfc3339(n.Done)
			}
			if err := enc.Encode(l); err != nil {
				return err
			}
			continue
		}
		var tags []string
		if n.Importance != store.ImportanceNormal {
			tags = append(tags, importanceName(n.Importance))
		}
		switch {
		case !n.Done.IsZero():
			tags = append(tags, "done "+n.Done.Local().Format("01-02 15:04"))
		case !n.Standing(since):
			tags = append(tags, "older than the report's week")
		case !shown[n.ID]:
			tags = append(tags, "counted, past the 20 the report shows")
		}
		tag := ""
		if len(tags) > 0 {
			tag = "  [" + strings.Join(tags, ", ") + "]"
		}
		fmt.Fprintf(stdout, "%-10s %-8s %s  %s%s\n", n.ID, n.Kind, n.At.Local().Format("01-02 15:04"), n.Title, tag)
	}
	return nil
}
