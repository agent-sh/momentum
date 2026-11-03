package loop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func TestBuildReportsNameInstalledFilesInAdmittedHostDirectories(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "tool")
	write(t, file, "marker-7f3a")
	ship := &Ship{Dir: filepath.Join(root, "loop"), InstalledRoots: []string{root}}
	raw := `{"summary":"marker-7f3a","test":"go test ./...","title":"Fix the result","body":"Return the expected result.","rollback":"revert","signal":{"source":"transcripts","kinds":["tool_result"],"match":["marker-7f3a"]},"window":"7d","lesson":""}`
	var fields map[string]any
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatal(err)
	}
	fields["installed"] = file
	body, _ := json.Marshal(fields)
	report, err := buildReport(body)
	if err != nil || report.Installed != file {
		t.Fatalf("installed report: %q %v", report.Installed, err)
	}
	if err := ship.installedReadable(report.Installed); err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(buildSchema), &schema); err != nil || schema.Properties["installed"] == nil {
		t.Fatalf("installed schema field: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	write(t, outside, "marker-7f3a")
	link := filepath.Join(root, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	runFile := filepath.Join(ship.Dir, "runs", "run", "work", "tool")
	write(t, runFile, "marker-7f3a")
	ship.Sandbox.Cache = filepath.Join(root, "cache")
	cacheFile := filepath.Join(ship.Sandbox.Cache, "tool")
	write(t, cacheFile, "marker-7f3a")
	for _, path := range []string{outside, link, root, runFile, cacheFile, "tool"} {
		if err := ship.installedReadable(path); err == nil {
			t.Errorf("accepted installed file %s", filepath.Base(path))
		}
	}
}

func TestAnOutsideInstalledPathRevealsNoFileState(t *testing.T) {
	ship := &Ship{Dir: filepath.Join(t.TempDir(), "loop"), InstalledRoots: []string{t.TempDir()}}
	outside := t.TempDir()
	file := filepath.Join(outside, "tool")
	write(t, file, "marker-7f3a")
	link := filepath.Join(outside, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	reason := ""
	for _, path := range []string{file, link, filepath.Join(outside, "missing"), outside} {
		err := ship.installedReadable(path)
		if err == nil {
			t.Fatal("accepted a path outside the admitted directories")
		}
		if reason == "" {
			reason = err.Error()
		} else if err.Error() != reason {
			t.Fatalf("outside file state changed the refusal: %q, want %q", err.Error(), reason)
		}
	}
}

func TestADeploymentNotePrecedesTheRecoveredCaseReceipt(t *testing.T) {
	ctx := context.Background()
	st, db := testStoreAt(t)
	c := testingCase(t, st)
	r := measurementRunner(t, st)
	p := Package{Case: c.ID, State: Landed, LandedAt: time.Now().Truncate(time.Millisecond),
		Landed: []string{strings.Repeat("a", 40)}, Plan: &Plan{Window: 7 * 24 * time.Hour}, Title: "marker-7f3a"}
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	allow := refuseNotes(t, db)
	if _, step, why, err := r.pick(ctx, c.Family, c.ID, false); err != nil || step != "" || !strings.Contains(why, "deployment:") {
		t.Fatalf("failed deployment was not skipped with its cause: %q %q %v", step, why, err)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Status != store.CaseTesting {
		t.Fatalf("case left testing before its note: %s", got.Status)
	}
	allow()
	if _, _, _, err := r.pick(ctx, c.Family, c.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := r.Ship.deploymentReceipt(ctx, p); err != nil {
		t.Fatal(err)
	}
	notes := notesOf(t, st, store.NoteDecided)
	if len(notes) != 1 || notes[0].ID != noteID("landed", p) || !strings.Contains(notes[0].Detail, "momentum ship rollback "+c.ID) {
		t.Fatalf("deployment note: %+v", notes)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Status != store.CaseDeployed {
		t.Fatalf("case did not deploy: %s", got.Status)
	}
}

func TestARollbackReceiptWaitsForItsNoteWithoutStartingAnotherRevert(t *testing.T) {
	ctx := context.Background()
	m := measuredCase(t)
	p := mustLoad(t, m)
	p.State, p.Undo = RolledBack, &RollbackJournal{Why: "marker-7f3a no measured effect"}
	if err := m.r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	allow := refuseNotes(t, m.db)
	if got := m.measure(t, Options{}); got.Status != "failed" || m.theCase(t).Status != store.CaseDeployed {
		t.Fatalf("receipt without note: %+v", got)
	}
	allow()
	if got := m.measure(t, Options{}); got.Status != "rolled back" || m.theCase(t).Status != store.CaseTesting {
		t.Fatalf("receipt after retry: %+v", got)
	}
	if err := m.r.Ship.rollbackReceipt(ctx, p, "different reason"); err != nil {
		t.Fatal(err)
	}
	notes := notesOf(t, m.st, store.NoteDecided)
	if len(notes) != 1 || notes[0].ID != noteID("rolled-back", p) || !strings.Contains(notes[0].Detail, p.Undo.Why) {
		t.Fatalf("rollback note: %+v", notes)
	}
}
