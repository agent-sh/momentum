package loop

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

// A unit withheld once is asked for no more on later appends: the index keeps one withheld line for it, and the count
// stays one.
func TestAWithheldUnitIsNotWithheldAgainOnAppend(t *testing.T) {
	goodInputScrubber(t)
	at := time.Date(2025, 4, 10, 12, 0, 0, 0, time.UTC)
	o := recordFixture(t, []store.Event{{Source: "fixture", SourceKey: "kept", TS: at, Kind: "record"}},
		`case "$2" in unit) printf '(record withheld: fixture)\n';; *) printf 'record\n';; esac`)
	o.Neighbours, o.IDs = -1, []string{store.Event{Source: "fixture", SourceKey: "kept", TS: at, Kind: "record"}.ID()}
	if _, err := Records(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	o.Units = []string{"held.service"}
	var m RecordsManifest
	for i := 0; i < 3; i++ {
		var err error
		if m, err = UnitLogs(context.Background(), o); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	index, err := os.ReadFile(filepath.Join(o.Destination, "index.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(index), "withheld\tunit\theld.service\n"); n != 1 || m.Withheld != 1 {
		t.Fatalf("after three appends: %d withheld lines, count %d\n%s", n, m.Withheld, index)
	}
}

// An event id is never read as another kind of index line or as a flag: `unit`, the unit lines' tag, and an id that
// starts with a dash are refused at selection.
func TestRecordIDsThatReadAsTagsOrFlagsAreRefused(t *testing.T) {
	goodInputScrubber(t)
	for _, id := range []string{"unit", "-x", "--bytes"} {
		t.Run(id, func(t *testing.T) {
			at := time.Date(2025, 4, 10, 12, 0, 0, 0, time.UTC)
			o := recordFixture(t, []store.Event{{Source: "fixture", SourceKey: "odd", TS: at, Kind: "record"}}, `printf 'record\n'`)
			db, err := sql.Open("sqlite", o.Store)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("UPDATE events SET id = ?", id); err != nil {
				t.Fatal(err)
			}
			db.Close()
			o.Neighbours, o.IDs = -1, []string{"ev:" + id}
			if m, err := Records(context.Background(), o); err == nil {
				t.Fatalf("id %q was admitted: %+v", id, m)
			}
		})
	}
}

// A record's own child gets its id after `--`, as a shared child does, so no id can be read as a flag.
func TestARecordsOwnChildEndsItsFlagsBeforeTheID(t *testing.T) {
	goodInputScrubber(t)
	at := time.Date(2025, 4, 10, 12, 0, 0, 0, time.UTC)
	ev := store.Event{Source: "fixture", SourceKey: "one", TS: at, Kind: "record"}
	o := recordFixture(t, []store.Event{ev}, fmt.Sprintf(`test "$2" = show && test "$5" = -- && test "$6" = %s || exit 9
printf 'record\n'`, ev.ID()))
	o.Neighbours, o.IDs = -1, []string{ev.ID()}
	if m, err := Records(context.Background(), o); err != nil || m.Written != 1 {
		t.Fatalf("one record: %+v %v", m, err)
	}
}

// Without XDG_RUNTIME_DIR the evaluation lock falls back to the user's runtime directory, /run/user/UID, where the
// user manager lives, before the cache directory.
func TestTheEvaluationLockFallsBackToTheUsersRuntimeDirectory(t *testing.T) {
	runtime := fmt.Sprintf("/run/user/%d", os.Getuid())
	if st, err := os.Stat(runtime); err != nil || !st.IsDir() {
		t.Skip("no user runtime directory here")
	}
	was := EvalUnitPrefix
	defer func() { EvalUnitPrefix = was }()
	EvalUnitPrefix = was + "fallback-"
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	unlock, err := LockEval()
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(runtime, strings.TrimSuffix(EvalUnitPrefix, "-")+".lock")
	defer os.Remove(lock)
	defer unlock()
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("the lock is not in the user's runtime directory: %v", err)
	}
}

// A number of any kind is text: a roman numeral or a fraction alone names something.
func TestNumbersOfAnyKindHoldText(t *testing.T) {
	for _, s := range []string{"Ⅻ", "½"} {
		if !holdsText(s) {
			t.Errorf("%q holds no text", s)
		}
	}
}
