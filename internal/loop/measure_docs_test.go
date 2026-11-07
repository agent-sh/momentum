package loop

import (
	"os"
	"strings"
	"testing"
)

// SPEC states the rules the measurement keeps beyond its statistics: what a window reads through, what a run that read
// less leaves, how a Go build from a modified tree and another installed file are judged, where a loop run names an
// installed file, and what the loop may not change.
func TestMeasurementDocsStateTheReadAndInEffectRules(t *testing.T) {
	raw, err := os.ReadFile("../../SPEC.md")
	if err != nil {
		t.Fatal(err)
	}
	spec := string(raw)
	for _, text := range []string{
		"reads only through the first incomplete run inside it",
		"A sessions scan reads each harness on its own",
		"another harness's failing scan neither stops it nor holds it",
		"A run that never started leaves no record",
		"judged once, at the first run that sees it",
		"The check reads only the commit the reflog names at the file's time, so a build from uncommitted edits",
		"neither is a move while a longer build ran",
		"an install followed by a move in the same second is read as if the move came first",
		"the reflog's newest entry, by its place in the reflog, dated at or before the file's second",
		"When HEAD's own reflog is empty or missing and HEAD names a branch, git lists the branch's reflog instead",
		"A clock set back between a move and the file (either way round) dates that move on the wrong side of the file",
		"Git's `gc.reflogExpire` (90 days by default) removes every entry older than that",
		"`gc.reflogExpireUnreachable` (30 days by default) removes older entries that move to or from a commit no ref reaches (in a branch's own reflog, one the branch does not reach), the newest one too",
		"Git 2.50.0 through 2.56.0 swap the two defaults, so on those releases every entry older than 30 days goes unless `gc.reflogExpire` (or a `gc.<pattern>.reflogExpire` that matches the ref) is set",
		"one expired after 90 days without a move, or 30 on git 2.50.0 through 2.56.0",
		"later runs give that reason while the file stands",
		"is not the file's and is not kept",
		"lies in a directory of `loop.read_dirs`, which a run cannot write, and not in the build's work tree",
		"The step opens the file once, without following a last link and without waiting on a pipe",
		"the session scanners that write the sessions events",
		"a proposal refused for it is a wait, recorded with its plan and signal",
		"the problem is too new to measure with that window",
		"A reader is dated by its first event, so one switched off and on again is not caught",
		"A rebuild and an ingest, and two ingests, exclude each other with one lock: a run that finds it held fails at once and names the process that holds it",
		"records the stretch between them as one incomplete run",
		"The stopped rule defers a decision while a source is stopped; it does not cover the stretch",
		"A session the scan read in part (a query of its harness's own failed) leaves its harness's run incomplete, in a sessions scan as in supervision",
		"a reader old enough at the proposal is not too new for the plan the failure moved",
		"The test assumes independent units",
		"every rate below is a floor, not a bound",
		"the rates above are floors for it",
		"it says there was no measured effect, not that the change did not help",
		"Each decision's note is written first, under an id of its own for the change",
		"a note that cannot be written makes no decision",
		"fails when the span holds more rather than count the newest alone",
		"incidents for a file the process cannot read",
	} {
		if !strings.Contains(spec, text) {
			t.Errorf("SPEC omits %q", text)
		}
	}
	raw, err = os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := strings.ToLower(string(raw))
	for _, text := range []string{"a window in which a scan failed is never read", "judged once by the tree it was built from", "a build report names a file only where the loop reads"} {
		if !strings.Contains(readme, text) {
			t.Errorf("README omits %q", text)
		}
	}
}
