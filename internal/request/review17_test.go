package request

// Tests from the seventeenth review of the requests change. Synthetic data only.

import (
	"path/filepath"
	"strings"
	"testing"
)

// A patch names the files of its own header lines: a header its content mentions (an added line, a string it
// adds) names no file, in a Hermes patch field and in a Codex apply_patch, read as stored and as the store keeps a
// raw patch (its lines joined, so read whole).
func TestAPatchNamesOnlyItsHeaderLines(t *testing.T) {
	hermes := `{"mode": "patch", "patch": "*** Begin Patch\n*** Update File: /work/a.md\n@@\n+*** Update File: /work/b.go\n*** End Patch"}`
	if got := writeAs("hermes", "patch", hermes, `{"success": true}`); got != "file /work/a.md" {
		t.Errorf("hermes: got %q", got)
	}
	codex := "*** Begin Patch\n*** Add File: /work/t.go\n+const p = \"*** Update File: /work/other.go\"\n*** End Patch\n"
	if got := writeAs("codex", "apply_patch", codex, "Success. Updated the following files:\nA /work/t.go\n"); got != "file /work/t.go" {
		t.Errorf("codex: got %q", got)
	}
	f := newFixture(t)
	s := "019a0000-0000-7000-8000-00000000f201"
	f.owner(s, at(0), "add the test")
	f.pairWithID(s, "", "codex", "apply_patch", "call_p1", at(1), codex, "Success. Updated the following files:\nA /work/t.go\n")
	if got := kinds(f.groupWith(Options{Redact: fakeRedact})[0].Results); got != "file /work/t.go" {
		t.Errorf("codex, stored with its lines joined: got %q", got)
	}
	if !strings.Contains(storedCut(codex), "*** Add File: /work/t.go +const") {
		t.Errorf("the fixture keeps the patch's lines: %q", storedCut(codex))
	}
	// with its transcript gone, the joined patch names no file, and its details count as cut
	f = newFixture(t)
	f.owner(s, at(0), "add the test")
	gone := filepath.Join(t.TempDir(), "rollout-gone.jsonl")
	f.cutPair(s, "codex", "apply_patch", "call_p2", at(1), codex, "Success. Updated the following files:\nA /work/t.go\n", gone+"@0#0", gone+"@100#0")
	if rq := f.groupWith(Options{Redact: fakeRedact})[0]; len(rq.Results) != 0 || rq.CutGone != 1 {
		t.Errorf("transcript gone: %q, cut and gone %d", kinds(rq.Results), rq.CutGone)
	}
}
