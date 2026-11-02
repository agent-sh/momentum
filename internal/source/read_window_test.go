package source

import (
	"context"
	"errors"
	"os"
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
