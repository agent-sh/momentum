package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions"
	"github.com/agent-sh/momentum/sessions/state"
)

func TestSessionStampsPersistAcrossOpens(t *testing.T) {
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "m.db")
	since := time.UnixMilli(1_700_000_000_123)
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSessionStamps(ctx, state.Stamps{"a": {Phase: sessions.Idle, Since: since, Seq: 4, Seen: since.Add(time.Minute)},
		"b": {Phase: sessions.Working, Since: since, Seen: since}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSessionStamps(ctx, state.Stamps{"a": {Phase: sessions.Idle, Since: since, Seq: 4, Seen: since.Add(time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.SessionStamps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["a"].Phase != sessions.Idle || !got["a"].Since.Equal(since) || got["a"].Seq != 4 || !got["a"].Seen.Equal(since.Add(time.Minute)) {
		t.Fatalf("a save replaces the stamps; got %+v", got)
	}
}

func TestSessionStampsSurviveADerivedSchemaChange(t *testing.T) {
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "m.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSessionStamps(ctx, state.Stamps{"a": {Phase: sessions.Idle, Since: time.UnixMilli(5), Seen: time.UnixMilli(6)}}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil { // a store from an older derived schema
		t.Fatal(err)
	}
	db.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.SessionStamps(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("the derived tables are rebuilt, the state stays: %v %v", got, err)
	}
}
