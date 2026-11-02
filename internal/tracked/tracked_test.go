package tracked

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo is a synthetic repository a test commits to.
type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) repo {
	t.Helper()
	r := repo{t, t.TempDir()}
	r.git("init", "-q", "-b", "main")
	r.commit("README.md", []byte("a small text file\n"))
	return r
}

func (r repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid",
		"-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes a file, or deletes it when data is nil, and commits.
func (r repo) commit(name string, data []byte) string {
	r.t.Helper()
	p := filepath.Join(r.dir, name)
	if data == nil {
		r.git("rm", "-q", name)
	} else {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			r.t.Fatal(err)
		}
		r.git("add", name)
	}
	r.git("commit", "-q", "-m", "change "+name)
	return r.git("rev-parse", "HEAD")
}

func check(t *testing.T, r repo, revs ...string) []Problem {
	t.Helper()
	ps, err := Check(context.Background(), r.dir, revs...)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

func wantOne(t *testing.T, ps []Problem, path, why string) {
	t.Helper()
	if len(ps) != 1 || ps[0].Path != path || !strings.Contains(ps[0].Why, why) {
		t.Fatalf("want one problem for %s (%s), got %v", path, why, ps)
	}
}

func TestAFileOverAMebibyteFailsTheCheck(t *testing.T) {
	r := newRepo(t)
	base := r.git("rev-parse", "HEAD")
	r.commit("data/exact.txt", bytes.Repeat([]byte("a"), Limit)) // at the limit: taken
	if ps := check(t, r, base+"..HEAD"); len(ps) != 0 {
		t.Fatalf("a file of exactly %d bytes is taken, got %v", Limit, ps)
	}
	r.commit("data/big.txt", bytes.Repeat([]byte("a"), Limit+1))
	wantOne(t, check(t, r, base+"..HEAD"), "data/big.txt", "over")
}

func TestAnExecutableBinaryFailsTheCheck(t *testing.T) {
	elf := append([]byte("\x7fELF\x02\x01\x01"), bytes.Repeat([]byte{0}, 64)...)
	machO := append([]byte{0xcf, 0xfa, 0xed, 0xfe}, bytes.Repeat([]byte{0}, 64)...)
	pe := make([]byte, 0x80)
	copy(pe, "MZ")
	binary.LittleEndian.PutUint32(pe[0x3c:], 0x40)
	copy(pe[0x40:], "PE\x00\x00")
	for _, c := range []struct {
		name string
		data []byte
		kind string
	}{{"momentum", elf, "ELF"}, {"bin/tool", machO, "Mach-O"}, {"tool.exe", pe, "PE"}, {"m.wasm", []byte("\x00asm\x01\x00\x00\x00"), "WebAssembly"}} {
		t.Run(c.kind, func(t *testing.T) {
			r := newRepo(t)
			base := r.git("rev-parse", "HEAD")
			r.commit(c.name, c.data)
			wantOne(t, check(t, r, base+"..HEAD"), c.name, c.kind)
		})
	}
}

func TestAFileACommitAddsAndALaterCommitDeletesStillFails(t *testing.T) {
	r := newRepo(t)
	base := r.git("rev-parse", "HEAD")
	r.commit("momentum", append([]byte("\x7fELF"), bytes.Repeat([]byte{1}, 100)...))
	r.commit("momentum", nil)
	r.commit("notes.md", []byte("the change\n"))
	wantOne(t, check(t, r, base+"..HEAD"), "momentum", "ELF")
	if _, err := os.Stat(filepath.Join(r.dir, "momentum")); !os.IsNotExist(err) {
		t.Fatalf("the head should not hold the binary: %v", err)
	}
}

func TestTextAndOrdinaryDataPassTheCheck(t *testing.T) {
	r := newRepo(t)
	base := r.git("rev-parse", "HEAD")
	r.commit("doc/MZ.md", []byte("MZ is not a PE file without its signature\n"+strings.Repeat("x", 200)))
	r.commit("testdata/image.png", []byte("\x89PNG\r\n\x1a\nsynthetic"))
	r.commit("run.sh", []byte("#!/bin/sh\necho ok\n"))
	if ps := check(t, r, base+"..HEAD"); len(ps) != 0 {
		t.Fatalf("text and ordinary data are taken, got %v", ps)
	}
	// the whole history, as a push to main reads it
	if ps := check(t, r, "HEAD"); len(ps) != 0 {
		t.Fatalf("the whole history holds nothing to refuse, got %v", ps)
	}
}

func TestOnlyWhatTheRangeAddsIsRead(t *testing.T) {
	r := newRepo(t)
	r.commit("old/big.txt", bytes.Repeat([]byte("b"), Limit+1))
	base := r.git("rev-parse", "HEAD")
	r.commit("new.txt", []byte("small\n"))
	if ps := check(t, r, base+"..HEAD"); len(ps) != 0 {
		t.Fatalf("a file the base already holds is not the range's, got %v", ps)
	}
	wantOne(t, check(t, r, "HEAD"), "old/big.txt", "over")
}

// Every header Executable names is refused: each Mach-O magic (32 and 64 bit, either byte order, one architecture
// or universal), ELF, PE, WebAssembly and an ar archive.
func TestEveryHeaderItNamesIsRefused(t *testing.T) {
	pad := func(b ...byte) []byte { return append(b, make([]byte, 64)...) }
	pe := make([]byte, 0x80)
	copy(pe, "MZ")
	binary.LittleEndian.PutUint32(pe[0x3c:], 0x40)
	copy(pe[0x40:], "PE\x00\x00")
	for name, b := range map[string][]byte{
		"ELF":               pad(0x7f, 'E', 'L', 'F'),
		"Mach-O 32 BE":      pad(0xfe, 0xed, 0xfa, 0xce),
		"Mach-O 64 BE":      pad(0xfe, 0xed, 0xfa, 0xcf),
		"Mach-O 32 LE":      pad(0xce, 0xfa, 0xed, 0xfe),
		"Mach-O 64 LE":      pad(0xcf, 0xfa, 0xed, 0xfe),
		"universal":         pad(0xca, 0xfe, 0xba, 0xbe),
		"universal LE":      pad(0xbe, 0xba, 0xfe, 0xca),
		"universal 64":      pad(0xca, 0xfe, 0xba, 0xbf),
		"universal 64 LE":   pad(0xbf, 0xba, 0xfe, 0xca),
		"PE":                pe,
		"WebAssembly":       []byte("\x00asm\x01\x00\x00\x00"),
		"ar archive (a .a)": []byte("!<arch>\n__.PKGDEF        0           0     0     644     0         `\n"),
	} {
		if Executable(b) == "" {
			t.Errorf("%s: not refused", name)
		}
	}
}

func TestExecutableNamesOnlyBinaries(t *testing.T) {
	for _, b := range [][]byte{nil, []byte("MZ"), []byte("MZ" + strings.Repeat(" ", 100)), []byte("\x7fEL"), []byte("plain text")} {
		if k := Executable(b); k != "" {
			t.Errorf("Executable(%q) = %q, want none", b, k)
		}
	}
}

// A pull request is read from its head's whole history, not from a range that starts at its base: a base that moves,
// is rewound or was never read would hide a commit the head still reaches. The first half pins the premise with the
// check itself (main holds a large file, a pull request is made from it, main is rewound before it: the range from
// the old base is clean and the head's history is not); the second pins the workflow, which fails on a range.
func TestAPullRequestReadsItsHeadsWholeHistory(t *testing.T) {
	r := newRepo(t)
	oldBase := r.commit("data/big.bin", bytes.Repeat([]byte("b"), Limit+1))
	head := r.commit("notes.md", []byte("the change\n"))
	if ps := check(t, r, oldBase+".."+head); len(ps) != 0 {
		t.Fatalf("the range from the old base is clean, got %v", ps)
	}
	wantOne(t, check(t, r, head), "data/big.bin", "over")

	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, `go run ./internal/tracked/check "$HEAD"`) {
		t.Errorf("the workflow's tracked step must run the check on the head alone")
	}
	for _, forbidden := range []string{"base.sha", "$BASE", "BASE.."} {
		if strings.Contains(text, forbidden) {
			t.Errorf("the workflow reads %q: a pull request must read its head's whole history, not a range from its base", forbidden)
		}
	}
}
