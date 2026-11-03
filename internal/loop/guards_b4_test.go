package loop

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func TestBasePathGuardRejectsLinksBeforeGit(t *testing.T) {
	for _, mode := range []string{"120000", "160000"} {
		for _, name := range []string{"link", "link/child"} {
			err := checkBasePaths(map[string]treeEntry{"link": {mode: mode}}, []string{name})
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "has a symlink or gitlink in the base") {
				t.Fatalf("base mode %s at %s: %v", mode, name, err)
			}
		}
	}
	if err := checkBasePaths(map[string]treeEntry{"file": {mode: "100644"}}, []string{"file"}); err != nil {
		t.Fatal(err)
	}
}

func TestResultTreeGuardsRejectNamesModesAndSizes(t *testing.T) {
	for _, mode := range []string{"120000", "160000", "100600", "040000"} {
		err := checkChangedFiles(nil, map[string]treeEntry{"file": {mode: mode}}, []string{"file"})
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "refused mode") {
			t.Fatalf("result mode %s: %v", mode, err)
		}
		err = checkChangedFiles(map[string]treeEntry{"file": {mode: mode}}, map[string]treeEntry{"file": {mode: "100644"}}, []string{"file"})
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "nonregular base file") {
			t.Fatalf("base mode %s: %v", mode, err)
		}
	}
	for _, name := range []string{"../file", "x/.git/config", "x\\file", "/file", "x\nfile"} {
		if err := checkChangedFiles(nil, nil, []string{name}); !errors.Is(err, ErrRefused) {
			t.Fatalf("deleted changed name %q: %v", name, err)
		}
	}
	for _, size := range []int64{FileMax, FileMax + 1} {
		err := checkChangedFiles(nil, map[string]treeEntry{"file": {mode: "100644", size: size}}, []string{"file"})
		if (err == nil) != (size == FileMax) {
			t.Fatalf("per-file boundary %d: %v", size, err)
		}
	}
	result := map[string]treeEntry{}
	var names []string
	for _, name := range []string{"a", "b", "c", "d"} {
		result[name] = treeEntry{mode: "100644", size: FileMax}
		names = append(names, name)
	}
	if err := checkChangedFiles(nil, result, names); err != nil {
		t.Fatal("exact total limit refused", err)
	}
	result["e"] = treeEntry{mode: "100755", size: 1}
	if err := checkChangedFiles(nil, result, append(names, "e")); !errors.Is(err, ErrRefused) {
		t.Fatal("total result limit ignored", err)
	}
}

func TestTextPatchResultCannotExceedPerFileLimit(t *testing.T) {
	s, _, _, p, _ := b4Checked(t)
	write(t, filepath.Join(p.Work, "large.txt"), strings.Repeat("x", FileMax)+"\n")
	_, _, err := s.applyPatch(context.Background(), p, []Patch{fixturePatch(t, s, p.Case)})
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "patch result exceeds") {
		t.Fatal("oversize text result reached a later guard", err)
	}
}

func TestExportRejectsAnInTreeSymlinkParent(t *testing.T) {
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, h := range []*tar.Header{
		{Name: "target/", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "target"},
		{Name: "link/file", Typeflag: tar.TypeReg, Size: 1, Mode: 0644},
	} {
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := w.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	err := extractTree(t.TempDir(), tar.NewReader(&buf))
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "non-directory parent") {
		t.Fatal("archive followed an in-tree parent link", err)
	}
}

func TestWriteTestRejectsInTreeLinks(t *testing.T) {
	for _, parent := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "parent"}[parent], func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "target", "file_test.go"), "marker-7f3a")
			link, target, name := "file_test.go", "target/file_test.go", "file_test.go"
			if parent {
				link, target, name = "link", "target", "link/file_test.go"
			}
			if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
				t.Fatal(err)
			}
			err := writeTest(dir, name, []byte("changed"), 0644)
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "base test path contains a symlink") {
				t.Fatal("test write followed an in-tree link", err)
			}
			b, err := os.ReadFile(filepath.Join(dir, "target", "file_test.go"))
			if err != nil || string(b) != "marker-7f3a" {
				t.Fatal("test write changed the target")
			}
		})
	}
}

func TestReviewerPackageHasOnlyFreshClaims(t *testing.T) {
	p := Package{Title: "One change", Body: "One cause", Rollback: "Revert", Plan: &Plan{Window: time.Hour},
		Dir: "/home/alice/source", Host: "/home/alice/host", Work: "/home/alice/work",
		Reviews: []Review{{Text: "marker-7f3a earlier finding"}},
		Check: &Check{Tree: strings.Repeat("a", 40), Test: []string{"/home/alice/run"},
			CommandInput: "host-only", Before: Outcome{Exit: 1, Tail: "marker-7f3a"}, After: Outcome{Tail: "marker-7f3a"}}}
	raw, err := json.Marshal(freshReviewPackage(p))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("marker-7f3a")) || bytes.Contains(raw, []byte("/home/")) || bytes.Contains(raw, []byte("host-only")) {
		t.Fatal("review package carries earlier findings or host execution data")
	}
	if !bytes.Contains(raw, []byte("before_exit")) || !bytes.Contains(raw, []byte("One change")) {
		t.Fatal("fresh reviewer lost check result or claims")
	}
}

func TestPreparedReviewPackageWithholdsHistoryAndHostPaths(t *testing.T) {
	ctx := context.Background()
	r, c := inputRunner(t)
	mechanism, next := "one cause", "one test"
	var err error
	c, err = r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &next}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	f := newRepo(t, nil)
	r.Ship = shipWithSandbox(r.Store, f, Sandbox{})
	r.Ship.Dir = r.Dir
	p, err := r.Ship.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "notes.txt"), "One change\n")
	p = fixtureCandidate(t, r.Ship, p)
	p.Title, p.Body, p.Rollback = "One change", "One cause", "Revert"
	p.Reviews = []Review{{Text: "marker-7f3a earlier finding"}}
	p.Check = &Check{Tree: strings.Repeat("a", 40), CommandInput: "host-only", Before: Outcome{Exit: 1, Tail: "marker-7f3a output"}}
	if err := r.Ship.save(&p); err != nil {
		t.Fatal(err)
	}
	r.pass = &passInputs{}
	defer r.pass.close()
	if err := r.prepareInputs(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := r.prepareRun(ctx, c, c.Family, StepReview)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := readHostInput(HostDir(run.Dir), "input/package.json")
	if err != nil || !json.Valid(raw) {
		t.Fatal("review input is unreadable", err)
	}
	for _, private := range []string{"marker-7f3a", "host-only", p.Dir, p.Host, p.Work} {
		if bytes.Contains(raw, []byte(private)) {
			t.Fatal("prepared reviewer received history or host paths")
		}
	}
}

func TestReviewRoundCapSkipsANewHead(t *testing.T) {
	s, _, c, p := b4Reviewed(t)
	p.Reviews = []Review{{Head: "a"}, {Head: "b"}, {Head: "c"}}
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: s.Store, Ship: s, Budgets: DefaultBudgets}
	// Without the round cap, preparation fails and produces a Done instead of skipping.
	if done := r.reviews(context.Background(), "", c.Family); len(done) != 0 {
		t.Fatal("fourth review was attempted")
	}
}

func TestLessonRequiresImprovedBeforeReserving(t *testing.T) {
	s, _, c, _ := b4Reviewed(t)
	if _, err := s.Store.CloseCase(context.Background(), c.ID, store.NotWorthIt, "", "fixture", "fixture"); err != nil {
		t.Fatal(err)
	}
	err := PublishLesson(context.Background(), s.Store, s, c.ID, "One lesson.", "false", "fixture")
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "only for a case closed as improved") {
		t.Fatal("lesson reached a later guard", err)
	}
}

func TestReviewReportNeedsSummaryAndEveryChangedFile(t *testing.T) {
	valid := `{"summary":"Checked both files.","findings":[],"reviewed_files":["b.go","a.go"]}`
	if _, err := readReviewReport([]byte(valid), []string{"a.go", "b.go"}); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"summary":"","findings":[],"reviewed_files":["a.go","b.go"]}`,
		`{"summary":" \n ","findings":[],"reviewed_files":["a.go","b.go"]}`,
		`{"summary":"Checked.","findings":[]}`,
		`{"summary":"Checked.","findings":[],"reviewed_files":["a.go"]}`,
		`{"summary":"Checked.","findings":[],"reviewed_files":["a.go","b.go","b.go"]}`,
		`{"summary":"Checked.","findings":[],"reviewed_files":["a.go","other.go"]}`,
	} {
		if _, err := readReviewReport([]byte(raw), []string{"a.go", "b.go"}); err == nil {
			t.Fatal("incomplete review accepted", raw)
		}
	}
}

func TestTheLastBlockingReviewCannotStartAnotherBuild(t *testing.T) {
	s, _, c, p := b4Reviewed(t)
	p.Reviews = []Review{{Head: "a"}, {Head: "b"}}
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	got, err := s.Reviewed(context.Background(), c.ID, Review{Head: currentReviewHead(t, s, c.ID), Text: "One finding.", Comment: "posted",
		Findings: []Finding{{Severity: "medium", Where: "fake.go", Problem: "marker-7f3a", Fix: "one fix"}}})
	if err != nil || got.State != Proposed || len(got.Reviews) != MaxRounds {
		t.Fatalf("last blocking review became a revision: %s %v", got.State, err)
	}
}

func TestEveryPushKindRechecksBusyImmediately(t *testing.T) {
	for _, kind := range []string{"proposal", "rollback", "cleanup"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _, p := b4Reviewed(t)
			s.Gate.Busy = func(context.Context, string) (string, error) { return "marker-7f3a busy", nil }
			args := []string{"origin", "HEAD:refs/heads/" + p.Branch}
			if kind == "rollback" {
				args = []string{"origin", "HEAD:refs/heads/" + p.Base}
			} else if kind == "cleanup" {
				args = []string{"origin", "--delete", p.Branch}
			}
			if _, err := s.push(context.Background(), p, p.Host, args...); !errors.Is(err, ErrRefused) {
				t.Fatalf("%s push crossed the busy gate: %v", kind, err)
			}
		})
	}
}

func TestPatchAttributesInTheProposedTreeAreInert(t *testing.T) {
	s, _, _, p, _ := b4Checked(t)
	write(t, filepath.Join(p.Work, ".gitattributes"), "fake.go export-ignore\nnotes.txt export-subst\n")
	write(t, filepath.Join(p.Work, "notes.txt"), "$Format:%H$\n")
	patch := fixturePatch(t, s, p.Case)
	data, err := readPatch(patch)
	if err != nil || !bytes.Contains(data, []byte("diff --git a/.gitattributes")) {
		t.Fatal("fixture did not put attributes in the patch")
	}
	p = fixtureCandidate(t, s, p)
	dir, err := s.export(context.Background(), p, "attributes", p.Head)
	if err != nil {
		t.Fatal(err)
	}
	defer s.unexport(p, dir)
	if _, err := os.Stat(filepath.Join(dir, "fake.go")); err != nil {
		t.Fatal("patch's export-ignore removed a file")
	}
	b, err := os.ReadFile(filepath.Join(dir, "notes.txt"))
	if err != nil || string(b) != "$Format:%H$\n" {
		t.Fatal("patch's export-subst transformed a file")
	}
}

func TestPinnedAttributeSourceOverridesTheTree(t *testing.T) {
	f := newRepo(t, map[string]string{".gitattributes": "fake.go marker-7f3a\n"})
	got, err := hostGit(context.Background(), f.main, nil, "check-attr", "marker-7f3a", "--", "fake.go")
	if err != nil || strings.TrimSpace(got) != "fake.go: marker-7f3a: unspecified" {
		t.Fatal("host Git read the tree's attributes")
	}
}

func TestBinaryPatchDeclarationsAndDeltaTargetsAreBounded(t *testing.T) {
	valid := fixtureBinaryLiteral([]byte("marker-7f3a"))
	if _, _, err := checkBinary(strings.Split(valid, "\n"), 0); err != nil {
		t.Fatal(err)
	}
	// A small declaration must not admit a larger zlib expansion.
	bad := strings.Replace(valid, "literal 11", "literal 1", 1)
	if _, _, err := checkBinary(strings.Split(bad, "\n"), 0); !errors.Is(err, ErrRefused) {
		t.Fatal("binary expansion ignored its declaration")
	}
	for _, sizes := range [][2]uint64{{1, FileMax + 1}, {FileMax + 1, 1}, {1, 1}} {
		payload := binary.AppendUvarint(nil, sizes[0])
		payload = binary.AppendUvarint(payload, sizes[1])
		block := strings.Replace(fixtureBinaryLiteral(payload), "literal ", "delta ", 1)
		_, _, err := checkBinary(strings.Split(block, "\n"), 0)
		if (err == nil) != (sizes[0] == 1 && sizes[1] == 1) {
			t.Fatalf("delta size boundary: %v", err)
		}
	}
}

func TestPrepareLeasesAProposalBranchLeftAfterRollback(t *testing.T) {
	fakeManager(t)
	s, f, c, p := b4Reviewed(t)
	before := originHead(t, f, "main")
	s.Gate.Busy = func(context.Context, string) (string, error) {
		if originHead(t, f, "main") != before {
			return "marker-7f3a busy after landing", nil
		}
		return "", nil
	}
	landed, err := s.Land(context.Background(), c.ID)
	if !errors.Is(err, ErrRefused) || landed.State != Landed {
		t.Fatal("fixture did not leave a cleanup pending", err)
	}
	s.Gate.Busy = nil
	if _, err := s.RollBack(context.Background(), c.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	next, err := s.Prepare(context.Background(), c.ID, f.main)
	if err != nil || next.PublishedHead != originHead(t, f, p.Branch) {
		t.Fatalf("replacement lost the remote branch lease: %s %v", next.PublishedHead, err)
	}
	if err := s.publish(context.Background(), &next, next.BaseCommit); err != nil {
		t.Fatal("replacement cannot update the leftover branch", err)
	}
}

func TestRollbackGatePrecedesHostWork(t *testing.T) {
	s, _, c, p := b4Reviewed(t)
	p.State, p.Landed = Landed, []string{p.Head}
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	s.Gate.Busy = func(context.Context, string) (string, error) { return "marker-7f3a busy", nil }
	script(t, filepath.Join(s.Dir, "rollback", c.ID), "keep", "exit 0")
	if _, err := s.RollBack(context.Background(), c.ID, "fixture"); !errors.Is(err, ErrRefused) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "rollback", c.ID, "keep")); err != nil {
		t.Fatal("rollback changed host work before its gate", err)
	}
}
