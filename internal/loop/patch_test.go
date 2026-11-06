package loop

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These fixture helpers stand in for the builder, never for the host ship tool.
func fixturePatch(t *testing.T, s *Ship, id string) Patch {
	t.Helper()
	p, ok, err := s.Load(id)
	if err != nil || !ok {
		return Patch{}
	}
	run(t, p.Work, "git", "add", "-A")
	data := run(t, p.Work, "git", "diff", "--cached", "--binary", p.WorkBase)
	dir := filepath.Join(s.Dir, "runs", "fixture-"+id)
	write(t, filepath.Join(dir, "change.patch"), data)
	return Patch{Dir: dir}
}

func checkPatch(t *testing.T, s *Ship, ctx context.Context, id, test string, files []string) (Package, error) {
	t.Helper()
	return s.Check(ctx, id, test, files, fixturePatch(t, s, id))
}

func proposePatch(t *testing.T, s *Ship, ctx context.Context, id string, pr Proposal) (Package, error) {
	t.Helper()
	return s.Propose(ctx, id, pr, fixturePatch(t, s, id))
}

// fixtureCandidate imports the builder's patch and records a host commit for tests of review inputs.
// It omits check and publication because these fixtures test the review, not proposal admission.
func fixtureCandidate(t *testing.T, s *Ship, p Package) Package {
	t.Helper()
	ctx := context.Background()
	tree, changed, err := s.applyPatch(ctx, p, []Patch{fixturePatch(t, s, p.Case)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.commitTree(ctx, p, tree, p.BaseCommit, "fixture change"); err != nil {
		t.Fatal(err)
	}
	p.Head, p.State = strings.TrimSpace(run(t, p.Host, "git", "rev-parse", "HEAD")), Proposed
	p.Changed = changed
	return p
}

func TestPrepareUsesIndependentClones(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	p, err := s.Prepare(context.Background(), testingCase(t, st).ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	if p.BaseCommit != originHead(t, f, "main") || p.Work == p.Host || p.Host == p.Dir {
		t.Fatalf("clone identity: %+v", p)
	}
	if run(t, p.Work, "git", "remote") != "" {
		t.Fatal("run clone has a remote")
	}
	hostObjects := filepath.Join(p.Host, ".git", "objects")
	workObjects := filepath.Join(p.Work, ".git", "objects")
	entries, err := os.ReadDir(hostObjects)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() || len(e.Name()) != 2 {
			continue
		}
		files, err := os.ReadDir(filepath.Join(hostObjects, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			a, err := os.Stat(filepath.Join(hostObjects, e.Name(), file.Name()))
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.Stat(filepath.Join(workObjects, e.Name(), file.Name()))
			if err != nil || os.SameFile(a, b) {
				t.Fatal("shared or absent run object")
			}
		}
	}
	for _, dir := range []string{p.Host, p.Work} {
		if _, err := os.Lstat(filepath.Join(dir, ".git", "objects", "info", "alternates")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("clone borrows object storage")
		}
	}
	if _, err := s.git(context.Background(), p.Work, "status"); err == nil {
		t.Fatal("host Git reads the run clone")
	}
}

func TestHostShipsOnlyThePatchAfterTheRunDestroysItsGit(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	c, p, pr := recoveryChecked(t, s, st, f)
	in := fixturePatch(t, s, c.ID)
	if err := os.RemoveAll(filepath.Join(p.Work, ".git")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, ".git"), "untrusted Git metadata")
	write(t, filepath.Join(p.Work, "fake.go"), fixed("99"))
	write(t, filepath.Join(p.Work, "internal/sandbox/probe.go"), "package sandbox // marker-7f3a\n")
	p, err := s.Propose(ctx, c.ID, pr, in)
	if err != nil {
		t.Fatalf("host depended on run files or Git metadata: %v", err)
	}
	if tree := strings.TrimSpace(run(t, p.Host, "git", "rev-parse", p.Head+"^{tree}")); tree != p.Check.Tree {
		t.Fatal("run mutation changed the shipped tree")
	}
}

func TestCheckUsesPatchBytesInsteadOfTheRunFiles(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	c := testingCase(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "fake.go"), fixed("42"))
	write(t, filepath.Join(p.Work, "fake_test.go"), answerTest)
	in := fixturePatch(t, s, c.ID)
	write(t, filepath.Join(p.Work, "fake.go"), fixed("99"))
	if _, err := s.Check(ctx, c.ID, `python3 -c 'from pathlib import Path; assert "return 42" in Path("fake.go").read_text()'`, nil, in); err != nil {
		t.Fatalf("host staged the run's files instead of applying captured bytes: %v", err)
	}
}

func TestUnsafePatchIsRefusedEvenWhenTheRunFilesWouldPass(t *testing.T) {
	for _, tc := range []struct{ name, path, mode string }{
		{"symlink", "probe", "120000"}, {"gitlink", "probe", "160000"}, {"mode", "probe", "100600"},
		{"parent", "../escape", "100644"}, {"absolute", "/escape", "100644"}, {"git-config", ".git/config", "100644"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			f := newRepo(t, nil)
			s := recoveryShip(t, st, f)
			c, p, _ := recoveryChecked(t, s, st, f)
			in := fixturePatch(t, s, c.ID)
			bad := "diff --git a/" + tc.path + " b/" + tc.path + "\nnew file mode " + tc.mode + "\n--- /dev/null\n+++ b/" + tc.path + "\n@@ -0,0 +1 @@\n+marker-7f3a\n"
			write(t, filepath.Join(in.Dir, "change.patch"), bad)
			if _, err := s.Check(ctx, c.ID, `python3 -c 'from pathlib import Path; assert "return 42" in Path("fake.go").read_text()'`, nil, in); err == nil {
				t.Fatal("host ignored the refused patch and staged passing run files")
			}
			if got := run(t, p.Host, "git", "log", "--format=%s", p.BaseCommit+"..HEAD"); got != "" {
				t.Fatal("refused patch created a commit")
			}
		})
	}
}

func TestPatchRejectsPathsModesAndBaseLinks(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "a/../escape", ".git/config", "nested/.git/hooks/probe", "nested/.GIT/config"} {
		t.Run(name, func(t *testing.T) {
			data := []byte("diff --git a/" + name + " b/" + name + "\nnew file mode 100644\n--- /dev/null\n+++ b/" + name + "\n@@ -0,0 +1 @@\n+marker-7f3a\n")
			if _, err := patchPaths(data); err == nil {
				t.Fatal("accepted unsafe patch path")
			}
		})
	}
	for _, mode := range []string{"120000", "160000", "100600", "040000"} {
		t.Run(mode, func(t *testing.T) {
			if _, err := patchPaths([]byte("diff --git a/probe b/probe\nnew file mode " + mode + "\n")); err == nil {
				t.Fatal("accepted unsafe patch mode")
			}
		})
	}
	st := testStore(t)
	f := newRepo(t, nil)
	if err := os.Symlink("fake.go", filepath.Join(f.main, "link")); err != nil {
		t.Fatal(err)
	}
	run(t, f.main, "git", "add", "link")
	run(t, f.main, "git", "commit", "-qm", "base link")
	run(t, f.main, "git", "push", "-q", "origin", "main")
	s := shipWithSandbox(st, f, Sandbox{})
	p, err := s.Prepare(context.Background(), testingCase(t, st).ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "change.patch"), "diff --git a/link/child b/link/child\nnew file mode 100644\n--- /dev/null\n+++ b/link/child\n@@ -0,0 +1 @@\n+marker-7f3a\n")
	if _, _, err := s.applyPatch(context.Background(), p, []Patch{{Dir: dir}}); err == nil || !strings.Contains(err.Error(), "has a symlink or gitlink in the base") {
		t.Fatalf("accepted a base link component: %v", err)
	}
}

func TestRunGitAndPatchRacesCannotChangeAProposal(t *testing.T) {
	for _, replacement := range []string{"link", "fifo", "file"} {
		t.Run(replacement, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			f := newRepo(t, nil)
			s := recoveryShip(t, st, f)
			c, p, pr := recoveryChecked(t, s, st, f)
			in := fixturePatch(t, s, c.ID)
			marker := filepath.Join(f.dir, "outside-marker")
			// Tamper only after the builder produced its patch. There is no host Git in this clone.
			script(t, filepath.Join(p.Work, ".git", "hooks"), "pre-commit", "touch '"+marker+"'")
			write(t, filepath.Join(p.Work, ".git", "config"), "[core]\nfsmonitor = touch "+marker+"\n")
			s.AfterPatchRead = func() {
				s.AfterPatchRead = nil
				if err := os.Remove(filepath.Join(in.Dir, "change.patch")); err != nil {
					t.Fatal(err)
				}
				switch replacement {
				case "link":
					if err := os.Symlink(marker, filepath.Join(in.Dir, "change.patch")); err != nil {
						t.Fatal(err)
					}
				case "fifo":
					if err := syscall.Mkfifo(filepath.Join(in.Dir, "change.patch"), 0600); err != nil {
						t.Fatal(err)
					}
				case "file":
					write(t, filepath.Join(in.Dir, "change.patch"), "not the snapshot")
				}
			}
			s.BeforeCommit = func() {
				write(t, filepath.Join(p.Work, "internal/sandbox/probe.go"), "package sandbox // marker-7f3a\n")
				// Also corrupt the run's HEAD and index before commit-tree.
				write(t, filepath.Join(p.Work, ".git", "HEAD"), "bad")
				write(t, filepath.Join(p.Work, ".git", "index"), "bad")
			}
			proposed, err := s.Propose(ctx, c.ID, pr, in)
			if err != nil {
				t.Fatal(err)
			}
			tree := strings.TrimSpace(run(t, p.Host, "git", "rev-parse", proposed.Head+"^{tree}"))
			if tree != p.Check.Tree || strings.Contains(run(t, p.Host, "git", "ls-tree", "-r", proposed.Head), "internal/sandbox") {
				t.Fatal("proposal differs from the checked tree")
			}
			if originHead(t, f, p.Branch) != proposed.Head {
				t.Fatal("pushed another object")
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("run Git executed on the host")
			}
			if replacement != "file" {
				if _, err := readPatch(in); err == nil {
					t.Fatal("second read accepted a nonregular patch")
				}
			}
		})
	}
}

func TestProposalRefusesAChangedOrReplacedPatchAfterCheck(t *testing.T) {
	for _, replacement := range []string{"link", "fifo", "edit"} {
		t.Run(replacement, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			f := newRepo(t, nil)
			s := recoveryShip(t, st, f)
			c, p, pr := recoveryChecked(t, s, st, f)
			in := fixturePatch(t, s, c.ID)
			if replacement == "edit" {
				write(t, filepath.Join(p.Work, "internal/sandbox/probe.go"), "package sandbox // marker-7f3a\n")
				in = fixturePatch(t, s, c.ID)
			} else {
				if err := os.Remove(filepath.Join(in.Dir, "change.patch")); err != nil {
					t.Fatal(err)
				}
				if replacement == "link" {
					if err := os.Symlink(filepath.Join(f.dir, "private"), filepath.Join(in.Dir, "change.patch")); err != nil {
						t.Fatal(err)
					}
				} else if err := syscall.Mkfifo(filepath.Join(in.Dir, "change.patch"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Propose(ctx, c.ID, pr, in); err == nil {
				t.Fatal("proposal accepted a replaced or edited patch")
			}
			if got := run(t, f.main, "git", "ls-remote", "origin", "refs/heads/"+p.Branch); got != "" {
				t.Fatal("refused patch was pushed")
			}
		})
	}
}

func TestPatchFileTypesAndLimits(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "private")
	write(t, target, "marker-7f3a")
	if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"link", "fifo", "../private"} {
		if _, err := readPatch(Patch{Dir: dir, File: name}); err == nil {
			t.Fatal("read a patch link, pipe or outside path")
		}
	}
	large, err := os.Create(filepath.Join(dir, "large"))
	if err != nil {
		t.Fatal(err)
	}
	if err := large.Truncate(PatchMax + 1); err != nil {
		t.Fatal(err)
	}
	large.Close()
	if _, err := readPatch(Patch{Dir: dir, File: "large"}); err == nil {
		t.Fatal("read oversized patch")
	}
}

func TestPatchGitAttributesAndHooksAreInert(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	c, p, pr := recoveryChecked(t, s, st, f)
	marker := filepath.Join(f.dir, "outside-marker")
	script(t, filepath.Join(p.Work, ".githooks"), "pre-commit", "touch '"+marker+"'")
	write(t, filepath.Join(p.Work, ".gitattributes"), "*.go filter=probe diff=probe export-ignore\n")
	in := fixturePatch(t, s, c.ID)
	// Configuration of the run is immaterial, even an external helper and a planted hook.
	run(t, p.Work, "git", "config", "filter.probe.clean", "touch "+marker)
	run(t, p.Work, "git", "config", "filter.probe.smudge", "touch "+marker)
	run(t, p.Work, "git", "config", "core.hooksPath", ".githooks")
	p, err := s.Check(ctx, c.ID, `python3 -c 'from pathlib import Path; assert "return 42" in Path("fake.go").read_text()'`, nil, in)
	if err != nil {
		t.Fatal(err)
	}
	p, err = s.Propose(ctx, c.ID, pr, in)
	if err != nil {
		t.Fatal(err)
	}
	export, err := s.export(ctx, p, "probe", p.Head)
	if err != nil {
		t.Fatal(err)
	}
	defer s.unexport(p, export)
	if _, err := os.Stat(filepath.Join(export, "fake.go")); err != nil {
		t.Fatal("repository export-ignore changed the export")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("patch code ran on host")
	}
	// A synthetic .git/config diff is refused before any application.
	write(t, filepath.Join(in.Dir, "change.patch"), "diff --git a/.git/config b/.git/config\nnew file mode 100644\n--- /dev/null\n+++ b/.git/config\n@@ -0,0 +1 @@\n+[core]\n")
	if _, _, err := s.applyPatch(ctx, p, []Patch{in}); err == nil {
		t.Fatal("applied a .git/config patch")
	}
}

func TestBinaryPatchAndExecutableMode(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	c, p, pr := recoveryChecked(t, s, st, f)
	data := bytes.Repeat([]byte{0, 1, 2, 255}, 2048)
	if err := os.WriteFile(filepath.Join(p.Work, "binary.dat"), data, 0644); err != nil {
		t.Fatal(err)
	}
	script(t, p.Work, "probe", "echo marker-7f3a")
	in := fixturePatch(t, s, c.ID)
	p, err := s.Check(ctx, c.ID, `python3 -c 'from pathlib import Path; assert "return 42" in Path("fake.go").read_text()'`, nil, in)
	if err != nil {
		t.Fatal(err)
	}
	// A second cumulative patch produces a binary delta against a binary base.
	p, err = s.Propose(ctx, c.ID, pr, in)
	if err != nil {
		t.Fatal(err)
	}
	got := run(t, p.Host, "git", "show", p.Head+":binary.dat")
	if !bytes.Equal([]byte(got), data) {
		t.Fatal("binary changed")
	}
	mode := run(t, p.Host, "git", "ls-tree", p.Head, "--", "probe")
	if !strings.HasPrefix(mode, "100755") {
		t.Fatal("executable mode lost")
	}
}

func TestBinaryDeltaIsBoundedAndPreserved(t *testing.T) {
	ctx := context.Background()
	data := make([]byte, 65536)
	_, _ = rand.New(rand.NewSource(7)).Read(data)
	data[0] = 0
	f := newRepo(t, map[string]string{"binary.dat": string(data)})
	st := testStore(t)
	s := recoveryShip(t, st, f)
	c, p, _ := recoveryChecked(t, s, st, f)
	copy(data[256:512], bytes.Repeat([]byte{0}, 256))
	if err := os.WriteFile(filepath.Join(p.Work, "binary.dat"), data, 0644); err != nil {
		t.Fatal(err)
	}
	in := fixturePatch(t, s, c.ID)
	b, err := readPatch(in)
	if err != nil || !bytes.Contains(b, []byte("\ndelta ")) {
		t.Fatalf("fixture produced no binary delta: %v", err)
	}
	tree, _, err := s.applyPatch(ctx, p, []Patch{in})
	if err != nil {
		t.Fatal(err)
	}
	if got := run(t, p.Host, "git", "show", tree+":binary.dat"); !bytes.Equal([]byte(got), data) {
		t.Fatal("binary delta changed its result")
	}
	b = bytes.Replace(b, []byte("\ndelta "), []byte("\ndelta 999999999\nliteral "), 1)
	if _, err := patchPaths(b); err == nil {
		t.Fatal("accepted an unbounded binary delta")
	}
}

func fixtureBinaryLiteral(data []byte) string {
	var compressed bytes.Buffer
	z := zlib.NewWriter(&compressed)
	_, _ = z.Write(data)
	_ = z.Close()
	var out strings.Builder
	out.WriteString("literal " + strconv.Itoa(len(data)) + "\n")
	b := compressed.Bytes()
	for len(b) > 0 {
		n := min(52, len(b))
		if n <= 26 {
			out.WriteByte(byte('A' + n - 1))
		} else {
			out.WriteByte(byte('a' + n - 27))
		}
		for offset := 0; offset < n; offset += 4 {
			var word [4]byte
			copy(word[:], b[offset:min(offset+4, n)])
			v := binary.BigEndian.Uint32(word[:])
			var chars [5]byte
			for i := 4; i >= 0; i-- {
				chars[i] = gitBase85[v%85]
				v /= 85
			}
			out.Write(chars[:])
		}
		out.WriteByte('\n')
		b = b[n:]
	}
	out.WriteByte('\n')
	return out.String()
}

func TestBinaryExpansionIsBoundedBeforeApply(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		data := make([]byte, FileMax+1)
		copy(data, "marker-7f3a")
		patch := "diff --git a/probe b/probe\nnew file mode 100644\nGIT binary patch\n" + fixtureBinaryLiteral(data) + fixtureBinaryLiteral(nil)
		if _, err := patchPaths([]byte(patch)); err == nil {
			t.Fatal("accepted per-file binary expansion beyond the limit")
		}
	})
	t.Run("total", func(t *testing.T) {
		data := make([]byte, FileMax)
		copy(data, "marker-7f3a")
		block := fixtureBinaryLiteral(data) + fixtureBinaryLiteral(nil)
		var patch strings.Builder
		for i := 0; i < TotalMax/FileMax+1; i++ {
			name := "probe-" + strconv.Itoa(i)
			patch.WriteString("diff --git a/" + name + " b/" + name + "\nnew file mode 100644\nGIT binary patch\n" + block)
		}
		if _, err := patchPaths([]byte(patch.String())); err == nil {
			t.Fatal("accepted aggregate binary expansion beyond the limit")
		}
	})
}

func TestPatchRenamesQuotedAndUnquotedPaths(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, map[string]string{"notes.txt": "marker-7f3a\n"})
	s := recoveryShip(t, st, f)
	c, p, _ := recoveryChecked(t, s, st, f)
	run(t, p.Work, "git", "mv", "notes.txt", "é.txt")
	tree, paths, err := s.applyPatch(ctx, p, []Patch{fixturePatch(t, s, c.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(paths, "\n"), "notes.txt") || !strings.Contains(strings.Join(paths, "\n"), "é.txt") {
		t.Fatal("rename gates did not receive both paths")
	}
	if got := run(t, p.Host, "git", "show", tree+":é.txt"); got != "marker-7f3a\n" {
		t.Fatal("quoted rename changed the data")
	}
}

func TestConflictingLandingPreparesANewBaseWithoutReadingTheRunClone(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	c, p := recoveryReviewed(t, s, st, f)
	oldPatch, err := os.ReadFile(filepath.Join(s.pkgDir(c.ID), "change.patch"))
	if err != nil {
		t.Fatal(err)
	}
	newBase := pushOther(t, f, "main", map[string]string{"fake.go": fixed("43")})
	if _, err := s.Land(ctx, c.ID); err == nil {
		t.Fatal("conflicting landing passed")
	}
	p, _, err = s.Load(c.ID)
	if err != nil || p.State != Revising || !p.RefreshBase {
		t.Fatalf("conflict did not go back to build: %+v %v", p, err)
	}
	// A broken run clone cannot participate in recovery.
	if err := os.RemoveAll(filepath.Join(p.Work, ".git")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, ".git"), "untrusted")
	p, err = s.buildBase(s.gitContext(shipContext(ctx, p.Case)), p, "") // RefreshBase is set: the landing found a conflict
	if err != nil || p.BaseCommit != newBase || p.State != Prepared || p.Check != nil {
		t.Fatalf("fresh preparation: %+v %v", p, err)
	}
	if got := strings.TrimSpace(run(t, p.Work, "git", "rev-parse", "HEAD^{tree}")); got != strings.TrimSpace(run(t, p.Host, "git", "rev-parse", newBase+"^{tree}")) || p.WorkBase == "" {
		t.Fatal("replacement run clone starts on the old base")
	}
	if got := run(t, p.Work, "git", "remote"); got != "" {
		t.Fatal("replacement clone has a remote")
	}
	kept, err := os.ReadFile(filepath.Join(s.pkgDir(c.ID), "change.patch"))
	if err != nil || !bytes.Equal(oldPatch, kept) {
		t.Fatal("old patch was not preserved in host data")
	}
}

func TestFallbackRefusesLinksInItsWritableDirectories(t *testing.T) {
	for _, name := range []string{"work", "out"} {
		t.Run(name, func(t *testing.T) {
			dir, outside := t.TempDir(), t.TempDir()
			write(t, filepath.Join(outside, "marker"), "marker-7f3a")
			if err := os.Symlink(outside, filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			if err := runDirectories(dir); err == nil {
				t.Fatal("fallback followed a planted directory link")
			}
			if b, err := os.ReadFile(filepath.Join(outside, "marker")); err != nil || string(b) != "marker-7f3a" {
				t.Fatal("outside fixture changed")
			}
		})
	}
}

func TestShipHelpersPassEveryGitPinToTheirChildren(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	p, err := s.Prepare(ctx, testingCase(t, st).ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	check := `test "$(git config --get core.hooksPath)" = /dev/null || exit 1
test "$(git config --get core.fsmonitor)" = false || exit 2
test "$(git config --get core.attributesFile)" = /dev/null || exit 3
test "$(git config --get maintenance.auto)" = false || exit 4
test "$(git config --get push.followTags)" = false || exit 5
`
	s.GH = script(t, f.dir, "pinned-gh", check+"echo marker-7f3a")
	if got, err := s.gh(ctx, p.Host, "fixture"); err != nil || got != "marker-7f3a\n" {
		t.Fatalf("GitHub CLI children lost Git pins: %q %v", got, err)
	}
	s.Gate.RepoGuard = []string{script(t, f.dir, "pinned-scope", check+`echo '{"allowed":true}'`)}
	if err := s.Gate.scope(ctx, p.Repo, p.Dir); err != nil {
		t.Fatalf("scope helper children lost Git pins: %v", err)
	}
}

func TestRevisionUsesThePreviousProposalAsParent(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	c, p := recoveryReviewed(t, s, st, f)
	first := p.Head
	// Send the package back for a synthetic finding.
	p.Reviews[len(p.Reviews)-1].Medium = 1
	p.State = Revising
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, "notes.txt"), "marker-7f3a\n")
	if _, err := checkPatch(t, s, ctx, c.ID, `python3 -c 'from pathlib import Path; assert "return 42" in Path("fake.go").read_text()'`, nil); err != nil {
		t.Fatal(err)
	}
	p, err := proposePatch(t, s, ctx, c.ID, Proposal{Title: "Fixture", Body: "Fix.", Rollback: "revert", Signal: goodSignal, Window: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if parent := strings.TrimSpace(run(t, p.Host, "git", "rev-parse", p.Head+"^")); parent != first {
		t.Fatal("revision did not grow the PR history")
	}
}
