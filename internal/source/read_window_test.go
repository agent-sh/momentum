package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
	"github.com/agent-sh/tacit/client/pkg/evidence"
)

// A failed file predating a window does not hold that window; a recent failed file still does.
func TestOldFailedFilesDoNotHoldLaterWindows(t *testing.T) {
	home := testHome(t)
	writeLines(t, claudePath(home), claudeSession()...)
	writeLines(t, codexPath(home), codexRollout()...)
	st := openStore(t)
	ctx := context.Background()
	failCodex := func(ctx context.Context, evs []evidence.Event) ([]evidence.Event, error) {
		for _, e := range evs {
			if strings.Contains(e.Text, "go test ./cache") {
				return nil, errors.New("scrubber: exit status 1")
			}
		}
		return masked(ctx, evs)
	}
	src := Transcripts{Homes: []string{home}, Workers: 1, records: recordsVia(failCodex), redact: failCodex, ready: ready}
	for i := 0; i < 3; i++ {
		out := RunAll(ctx, st, []Source{src})
		read, err := st.ReadThrough(ctx, store.Transcripts)
		t.Logf("run %d: err=%v, stored %d events, read through %v %v", i, out[0].Err, len(all(t, st, "transcripts")), read, err)
		if out[0].Err == nil || !read.IsZero() {
			t.Fatalf("run %d: %+v %v", i, out, read)
		}
	}

	start := time.Now().Add(-2 * time.Hour)
	old := start.Add(-time.Hour)
	if err := os.Chtimes(codexPath(home), old, old); err != nil {
		t.Fatal(err)
	}
	RunAll(ctx, st, []Source{src})
	q := store.ActivityQuery{Since: start, Until: start.Add(time.Hour), BySession: true}
	unread, err := st.UnreadSources(ctx, store.Transcripts, q)
	if err != nil || len(unread) != 0 {
		t.Fatalf("old failed file: %v %v", unread, err)
	}
	// The same partial read cannot certify a window that contains the failed file's modification.
	q.Since = old.Add(-time.Hour)
	unread, err = st.UnreadSources(ctx, store.Transcripts, q)
	if err != nil || len(unread) != 1 {
		t.Fatalf("recent failed file: %v %v", unread, err)
	}
}

// The newest failed file bounds a partial read, even when another failed file is much older.
func TestEveryFailedFileMustPredateTheWindow(t *testing.T) {
	home := testHome(t)
	writeLines(t, claudePath(home), claudeSession()...)
	writeLines(t, codexPath(home), codexRollout()...)
	now := time.Now()
	for path, at := range map[string]time.Time{claudePath(home): now.Add(-3 * time.Hour), codexPath(home): now.Add(-90 * time.Minute)} {
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	fail := func(context.Context, []evidence.Event) ([]evidence.Event, error) {
		return nil, errors.New("redaction failed")
	}
	src := Transcripts{Homes: []string{home}, Workers: 1, records: recordsVia(fail), redact: fail, ready: ready}
	st := openStore(t)
	ctx := context.Background()
	out := RunAll(ctx, st, []Source{src})
	if out[0].Err == nil {
		t.Fatal("redaction unexpectedly passed")
	}
	q := store.ActivityQuery{Since: now.Add(-2 * time.Hour), Until: now.Add(-time.Hour)}
	unread, err := st.UnreadSources(ctx, store.Transcripts, q)
	if err != nil || len(unread) != 1 {
		t.Fatalf("newer failed file ignored: %v %v", unread, err)
	}
	q.Since, q.Until = now.Add(-time.Hour), now.Add(-30*time.Minute)
	unread, err = st.UnreadSources(ctx, store.Transcripts, q)
	if err != nil || len(unread) != 0 {
		t.Fatalf("both files predate the window: %v %v", unread, err)
	}
}

// A run that could not list every source certifies nothing for the windows after its failed files: a file it did not
// see may be newer than they are. The same run over the sources it could list does.
func TestAPartialRunThatCouldNotListEverythingCertifiesNothing(t *testing.T) {
	for _, listsAll := range []bool{true, false} {
		home := testHome(t)
		writeLines(t, claudePath(home), claudeSession()...)
		writeLines(t, codexPath(home), codexRollout()...)
		failCodex := func(ctx context.Context, evs []evidence.Event) ([]evidence.Event, error) {
			for _, e := range evs {
				if strings.Contains(e.Text, "go test ./cache") {
					return nil, errors.New("scrubber: exit status 1")
				}
			}
			return masked(ctx, evs)
		}
		homes := []string{home}
		if !listsAll {
			homes = append(homes, filepath.Join(t.TempDir(), "x[")) // a home whose files cannot be listed: its pattern is malformed
		}
		src := Transcripts{Homes: homes, Workers: 1, records: recordsVia(failCodex), redact: failCodex, ready: ready}
		st := openStore(t)
		ctx := context.Background()
		start := time.Now().Add(-2 * time.Hour)
		old := start.Add(-time.Hour)
		if err := os.Chtimes(codexPath(home), old, old); err != nil {
			t.Fatal(err)
		}
		if out := RunAll(ctx, st, []Source{src}); out[0].Err == nil {
			t.Fatal("the run did not fail")
		}
		unread, err := st.UnreadSources(ctx, store.Transcripts, store.ActivityQuery{Since: start, Until: start.Add(time.Hour), BySession: true})
		if err != nil || (len(unread) == 0) != listsAll {
			t.Errorf("a run that lists everything: %v, unread %v %v", listsAll, unread, err)
		}
	}
}

// A failed file the run cannot date (it is gone by the end of the run) bounds nothing, so the run certifies nothing.
func TestAFailedFileThatCannotBeDatedCertifiesNothing(t *testing.T) {
	home := testHome(t)
	writeLines(t, claudePath(home), claudeSession()...)
	writeLines(t, codexPath(home), codexRollout()...)
	failAndVanish := func(ctx context.Context, evs []evidence.Event) ([]evidence.Event, error) {
		for _, e := range evs {
			if strings.Contains(e.Text, "go test ./cache") {
				os.Remove(codexPath(home))
				return nil, errors.New("scrubber: exit status 1")
			}
		}
		return masked(ctx, evs)
	}
	src := Transcripts{Homes: []string{home}, Workers: 1, records: recordsVia(failAndVanish), redact: failAndVanish, ready: ready}
	st := openStore(t)
	ctx := context.Background()
	if out := RunAll(ctx, st, []Source{src}); out[0].Err == nil {
		t.Fatal("the run did not fail")
	}
	if _, err := os.Stat(codexPath(home)); err == nil {
		t.Fatal("the failed file is still there")
	}
	at, err := st.ReadWindowThrough(ctx, store.Transcripts, time.Now())
	if err != nil || !at.IsZero() {
		t.Fatalf("a failed file with no date certified a window: %v %v", at, err)
	}
}
