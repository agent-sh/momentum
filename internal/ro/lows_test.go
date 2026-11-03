package ro

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/evidenceview"
)

// Each command that reads a relative time anchors it itself: with the clock reset before each one, a window of one
// second before the copy's end still holds the newest event, for events and for status alike.
func TestEachRelativeTimeCommandAnchorsItself(t *testing.T) {
	f := inputs(t)
	defer func() { evidenceview.Now = time.Now }()
	evidenceview.Now = time.Now
	code, out, diag := invoke(f, "evidence", "events", "--since", "1s", "--limit", "500")
	if code != 0 || strings.Count(out, "\n") < 1 {
		t.Errorf("events: %d %q %s", code, out, diag)
	}
	evidenceview.Now = time.Now
	code, out, diag = invoke(f, "evidence", "status", "--since", "1s")
	if code != 0 || !strings.Contains(out, "fixture") {
		t.Errorf("status: %d %q %s", code, out, diag)
	}
}

// An id or a unit name is checked whole before it names a file: a valid id or name inside a longer argument is a
// usage error, not a path the records root happens to refuse.
func TestReadOnlyNamesAreCheckedWhole(t *testing.T) {
	f := inputs(t)
	id := f.events[0].ID()
	for _, c := range []struct {
		args []string
		why  string
	}{
		{[]string{"evidence", "show", "../" + id}, "event id must be 24 hex characters"},
		{[]string{"evidence", "show", id + "0"}, "event id must be 24 hex characters"},
		{[]string{"evidence", "unit", "../fixture.service"}, "evidence unit takes one captured service or timer name"},
		{[]string{"evidence", "unit", "fixture.service.txt"}, "evidence unit takes one captured service or timer name"},
	} {
		code, out, diag := invoke(f, c.args...)
		if code != 2 || out != "" || !strings.Contains(diag, c.why) {
			t.Errorf("%v: %d %q %q", c.args, code, out, diag)
		}
	}
}

// A record that is a link inside the records directory is not followed: the root admits a link that stays inside it
// whatever the open's flags say, so the descriptor is checked against the name's own lstat.
func TestReadOnlyUnitRecordLinkInsideTheRootIsNotFollowed(t *testing.T) {
	f := inputs(t)
	if err := os.Chmod(f.loc.records, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.loc.records, "elsewhere.txt"), "a linked unit\n")
	if err := os.Symlink("elsewhere.txt", filepath.Join(f.loc.records, "unit-linked.service.txt")); err != nil {
		t.Skip("no symlinks here")
	}
	if code, out, _ := invoke(f, "evidence", "unit", "linked.service"); code == 0 || strings.Contains(out, "a linked unit") {
		t.Fatalf("a linked unit record was followed: %d %q", code, out)
	}
}
