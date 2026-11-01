package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// The case commands open a case by its signature (the same signature again finds it), record the investigation,
// refuse what only the ship and measure tools may do, close with a reason, and merge two cases that name the same
// root cause. Every refusal exits 2 and says why.
func TestTheCaseCommands(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	t.Setenv("MOMENTUM_ACTOR", "loop:test")
	code, out, errOut := momentum("case", "open", "--family", "failures", "--signature", "failures:Bash:exit", "--title", "Bash exits",
		"--evidence", "file:/a@1", "--evidence", "file:/a@2")
	m := regexp.MustCompile(`^(c[0-9a-f]{8}): opened \[open\] Bash exits\n$`).FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("open: exit %d %q %s", code, out, errOut)
	}
	id := m[1]
	if code, out, _ := momentum("case", "open", "--family", "failures", "--signature", "failures:Bash:exit", "--title", "x",
		"--evidence", "file:/a@3"); code != 0 || out != id+": updated [open] Bash exits\n" {
		t.Fatalf("open again: exit %d %q", code, out)
	}
	if code, _, errOut := momentum("case", "set", id, "--status", "testing", "--mechanism", "the PATH lacks go"); code != 2 ||
		!strings.Contains(errOut, "next discriminating test") {
		t.Fatalf("testing without a next test: exit %d %s", code, errOut)
	}
	if code, _, errOut := momentum("case", "set", id, "--status", "investigating", "--mechanism", "the PATH lacks go",
		"--alternative", "a stale shell", "--alternative", "a missing module", "--next-test", "run with PATH set", "--benefit", "no retries"); code != 0 {
		t.Fatalf("set: exit %d %s", code, errOut)
	}
	if code, _, errOut := momentum("case", "set", id, "--status", "deployed"); code != 2 || !strings.Contains(errOut, "ship tool") {
		t.Fatalf("deployed by hand: exit %d %s", code, errOut)
	}
	if code, _, errOut := momentum("case", "close", id, "--reason", "improved", "--why", "it is better"); code != 2 ||
		!strings.Contains(errOut, "measure tool") {
		t.Fatalf("improved by hand: exit %d %s", code, errOut)
	}
	code, out, errOut = momentum("case", "show", id, "--json")
	var shown struct {
		ID           string   `json:"id"`
		Status       string   `json:"status"`
		Mechanism    string   `json:"mechanism"`
		Alternatives []string `json:"alternatives"`
		Evidence     int      `json:"evidence"`
		Signatures   []string `json:"signatures"`
		Log          []struct {
			Actor  string `json:"actor"`
			Action string `json:"action"`
		} `json:"log"`
	}
	if code != 0 {
		t.Fatalf("show: exit %d %s", code, errOut)
	}
	if err := json.Unmarshal([]byte(out), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.Status != "investigating" || shown.Mechanism != "the PATH lacks go" || len(shown.Alternatives) != 2 || shown.Evidence != 3 ||
		len(shown.Log) != 3 || shown.Log[0].Actor != "loop:test" || shown.Log[2].Action != "set" {
		t.Fatalf("show: %+v", shown)
	}

	_, out, _ = momentum("case", "open", "--family", "failures", "--signature", "failures:go:missing", "--title", "go is missing",
		"--evidence", "file:/b@1")
	other := strings.SplitN(out, ":", 2)[0]
	if code, out, _ := momentum("case", "signature", id, "root:path-without-go"); code != 0 || !strings.Contains(out, "signature added") {
		t.Fatalf("signature: exit %d %q", code, out)
	}
	if code, out, _ := momentum("case", "signature", other, "root:path-without-go"); code != 0 || !strings.HasPrefix(out, id+": "+other+" went into") {
		t.Fatalf("a second case with the root: exit %d %q", code, out)
	}
	if code, out, _ := momentum("case", "list"); code != 0 || strings.Count(out, "\n") != 1 || !strings.Contains(out, id) {
		t.Fatalf("list: exit %d %q", code, out)
	}
	if code, out, _ := momentum("case", "list", "--all"); code != 0 || !strings.Contains(out, "closed: duplicate of "+id) {
		t.Fatalf("list --all: exit %d %q", code, out)
	}
	if code, _, errOut := momentum("case", "close", id, "--reason", "not-worth-it"); code != 2 || !strings.Contains(errOut, "--why") {
		t.Fatalf("close without why: exit %d %s", code, errOut)
	}
	if code, out, _ := momentum("case", "close", id, "--reason", "not-worth-it", "--why", "rare"); code != 0 ||
		!strings.Contains(out, "[closed: not-worth-it]") {
		t.Fatalf("close: exit %d %q", code, out)
	}
	if code, _, errOut := momentum("case", "reopen", id, "--evidence", "file:/a@1", "--why", "again"); code != 2 || !strings.Contains(errOut, "does not hold") {
		t.Fatalf("reopen on old evidence: exit %d %s", code, errOut)
	}
	if code, out, _ := momentum("case", "reopen", id, "--evidence", "file:/a@9", "--why", "it came back"); code != 0 || !strings.Contains(out, "reopened [open]") {
		t.Fatalf("reopen: exit %d %q", code, out)
	}
	if code, _, errOut := momentum("case", "show", "c00000000"); code != 2 || !strings.Contains(errOut, "no case") {
		t.Fatalf("show of an unknown case: exit %d %s", code, errOut)
	}
}
