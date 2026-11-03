package loop

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func deepClosedTree(t *testing.T, path string) func() {
	t.Helper()
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	current := root
	for i := 0; i < 25; i++ {
		name := strings.Repeat("d", 200)
		if err := current.Mkdir(name, 0700); err != nil {
			t.Fatal(err)
		}
		next, err := current.OpenRoot(name)
		if current != root {
			current.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
		current = next
	}
	defer current.Close()
	if err := current.Mkdir("closed", 0700); err != nil {
		t.Fatal(err)
	}
	closed, err := current.OpenRoot("closed")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.WriteFile("file", []byte("marker-7f3a"), 0600); err != nil {
		t.Fatal(err)
	}
	handle, err := closed.Open(".")
	closed.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Chmod(0); err != nil {
		t.Fatal(err)
	}
	// Keep a bottom descriptor so even the unfixed implementation can clean up
	// this regression fixture without leaving an unreadable tree.
	return func() {
		if err := handle.Chmod(0700); err != nil {
			t.Error(err)
		}
		handle.Close()
		if err := os.RemoveAll(path); err != nil {
			t.Error(err)
		}
	}
}

func TestRemoveAllReachesClosedDirectoriesBeyondPathMax(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read mode 000 directories")
	}
	path := filepath.Join(t.TempDir(), "tree")
	repair := deepClosedTree(t, path)
	defer repair()
	outside := t.TempDir()
	write(t, filepath.Join(outside, "marker"), "marker-7f3a")
	if err := os.Chmod(outside, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(outside, 0700)
	if err := os.Symlink(outside, filepath.Join(path, "link")); err != nil {
		t.Fatal(err)
	}
	if err := removeAll(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("deep tree remains", err)
	}
	st, err := os.Stat(outside)
	if err != nil || st.Mode().Perm() != 0500 {
		t.Fatal("cleanup followed or chmodded a link", err)
	}
}

func TestExportRefusesAnUnremovableEarlierTree(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	s, _, _, p, _ := b4Checked(t)
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.Mkdir(blocked, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(blocked, 0700)
	s.Dir = blocked
	_, err := s.export(context.Background(), p, "before", p.BaseCommit)
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "earlier step cannot be removed") {
		t.Fatal("export ignored its removal error", err)
	}
}

func TestSandboxReportsItsRemovalErrorOnce(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	base := t.TempDir()
	bin := t.TempDir()
	// Refuse the scratch directory's deletion after the fake launcher ends.
	script(t, bin, "systemd-run", `chmod 500 '`+base+`'
exit 0`)
	script(t, bin, "systemctl", `echo not-found`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	defer os.Chmod(base, 0700)
	_, err := (Sandbox{Scratch: base}).Run(context.Background(), t.TempDir(), []string{"python3", "-c", "pass"}, nil)
	if !errors.Is(err, os.ErrPermission) || strings.Count(err.Error(), "cannot be removed") != 1 {
		t.Fatal("sandbox discarded or repeated its cleanup error", err)
	}
}

func TestCheckAndLandingReportExportCleanupFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	for _, step := range []string{"check", "land"} {
		t.Run(step, func(t *testing.T) {
			s, f, c, p, pr := b4Checked(t)
			ctx := context.Background()
			if step == "land" {
				var err error
				p, err = proposePatch(t, s, ctx, c.ID, pr)
				if err != nil {
					t.Fatal(err)
				}
				p, err = s.Reviewed(ctx, c.ID, Review{Head: currentReviewHead(t, s, c.ID), Text: "One review.", Comment: "https://example.com/review/1"})
				if err != nil {
					t.Fatal(err)
				}
			}
			parent := filepath.Join(s.Dir, "exports")
			if err := os.MkdirAll(parent, 0700); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(parent, 0700)
			bin := t.TempDir()
			script(t, bin, "systemd-run", `chmod 500 '`+parent+`'
exit 0`)
			script(t, bin, "systemctl", `echo not-found`)
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			// Keep the fake unit's own scratch away from the blocked exports.
			s.Sandbox.Scratch = filepath.Join(f.dir, "sandbox-scratch")
			before, err := os.ReadFile(filepath.Join(s.pkgDir(c.ID), "package.json"))
			if err != nil {
				t.Fatal(err)
			}
			if step == "check" {
				_, err = checkPatch(t, s, ctx, c.ID, "python3 -c pass", nil)
			} else {
				_, err = s.Land(ctx, c.ID)
			}
			if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "export scratch tree cannot be removed") {
				t.Fatal("step dropped its export cleanup error", err)
			}
			if originHead(t, f, "main") == p.Head {
				t.Fatal("cleanup failure reached the default branch")
			}
			if step == "check" {
				after, err := os.ReadFile(filepath.Join(s.pkgDir(c.ID), "package.json"))
				if err != nil || string(before) != string(after) {
					t.Fatal("cleanup failure persisted a successful check", err)
				}
			}
		})
	}
}

func TestSandboxRemovesADeepClosedOutputTree(t *testing.T) {
	s := testSandbox(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "probe.py"), `import os
os.chdir("/out")
n = "d" * 200
for i in range(25):
    os.mkdir(n)
    os.chdir(n)
os.mkdir("closed")
with open("closed/file", "w") as f:
    f.write("marker-7f3a")
os.chmod("closed", 0)
`)
	got, err := s.Run(context.Background(), dir, []string{"python3", "probe.py"}, nil)
	if err != nil || got.Exit != 0 {
		t.Fatal("deep sandbox tree could not be removed", err)
	}
	entries, err := os.ReadDir(s.Scratch)
	if err != nil || len(entries) != 0 {
		t.Fatal("sandbox output tree remains", err)
	}
}

func TestSignalEditMatchesBothCases(t *testing.T) {
	for _, removed := range []string{"MARKER-7F3A", "marker-7f3a"} {
		t.Run(removed, func(t *testing.T) {
			s, _, _, p, _ := b4Checked(t)
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			script(t, bin, "git", `for arg do
if [ "$arg" = diff ]; then printf '%s\n' 'diff --git a/file b/file' '@@ -1 +1 @@' '-`+removed+`' '+changed'; exit 0; fi
done
exec '`+realGit+`' "$@"`)
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			match := "MARKER-7F3A"
			if removed == match {
				match = strings.ToLower(match)
			}
			err = s.editsSignal(context.Background(), p, Signal{Match: []string{match}})
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "measurement counts") {
				t.Fatal("signal removal was case-sensitive", err)
			}
		})
	}
}

func archiveB5(t *testing.T, entries ...*tar.Header) *tar.Reader {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, h := range entries {
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := w.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return tar.NewReader(&b)
}

func TestExportChecksItsEntryPathDirectly(t *testing.T) {
	err := extractTree(t.TempDir(), archiveB5(t, &tar.Header{Name: ".GiT", Typeflag: tar.TypeDir, Mode: 0700}))
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "refused component") {
		t.Fatal("export accepted a Git control path", err)
	}
}

func TestPatchIndexModeAndQuotedAmbiguity(t *testing.T) {
	for _, tc := range []struct{ header, cause string }{
		{"diff --git a/file b/file\nindex 123..456 120000\n", "patch index has refused mode"},
		{`diff --git a/file "b/file" "b/other"` + "\n", "ambiguous diff header"},
		{"diff --git a/dir b/f b.txt b/dir b/f b.txt\n", "ambiguous diff header"},
	} {
		_, err := patchPaths([]byte(tc.header))
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), tc.cause) {
			t.Fatal("header guard did not refuse directly", err)
		}
	}
}

func TestExportForcesFileModesAfterCreation(t *testing.T) {
	dir := t.TempDir()
	mask := syscall.Umask(077)
	defer syscall.Umask(mask)
	err := extractTree(dir, archiveB5(t,
		&tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0600, Size: 1},
		&tar.Header{Name: "program", Typeflag: tar.TypeReg, Mode: 0700, Size: 1},
	))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{"file": 0644, "program": 0755} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil || st.Mode().Perm() != want {
			t.Fatalf("%s mode was not forced to %o: %v", name, want, err)
		}
	}
}

func TestCheckRefusesATestEntryThatIsNotABlob(t *testing.T) {
	s, _, c, _, _ := b4Checked(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script(t, bin, "git", `tree=no
file=no
for arg do
case "$arg" in ls-tree) tree=yes;; --) file=yes;; esac
done
if [ "$tree" = yes ] && [ "$file" = yes ]; then
 printf '100644 tree 1234567890\tfake_test.go\n'
 exit 0
fi
exec '`+realGit+`' "$@"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	_, err = checkPatch(t, s, context.Background(), c.ID, "python3 -c pass", []string{"fake_test.go"})
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "test file is not a regular blob") {
		t.Fatal("check ignored its blob guard", err)
	}
}
