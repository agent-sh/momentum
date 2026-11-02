// Package tracked checks the files a range of commits adds to a repository: none over a mebibyte and none an
// executable binary. main keeps every commit of a pull request, so a file one commit adds stays in its history
// even when a later commit deletes it: the check reads every commit of the range, not only its last.
package tracked

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// Limit is the largest file a commit may add: 1 MiB.
const Limit = 1 << 20

// Problem is a file a commit of the range added that the repository does not take.
type Problem struct {
	Path string // where the range first has it
	Size int64
	Why  string
}

func (p Problem) String() string { return fmt.Sprintf("%s (%d bytes): %s", p.Path, p.Size, p.Why) }

// Check reads every file the commits of revs add, in every commit: revs are git rev-list's arguments, BASE..HEAD
// for what a pull request or a push adds, HEAD for the whole history. A file is refused when it is over Limit or
// starts as an executable binary does (Executable).
func Check(ctx context.Context, dir string, revs ...string) ([]Problem, error) {
	if len(revs) == 0 {
		return nil, fmt.Errorf("no revisions to check")
	}
	list := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "rev-list", "--objects"}, revs...)...)
	out, err := list.Output()
	if err != nil {
		return nil, fmt.Errorf("git rev-list: %w%s", err, stderrOf(err))
	}
	paths := map[string]string{}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id, path, _ := strings.Cut(line, " ")
		if id == "" {
			continue
		}
		if _, seen := paths[id]; !seen {
			ids = append(ids, id)
		}
		paths[id] = path
	}
	if len(ids) == 0 {
		return nil, nil
	}
	// each object's type and size, and the content of every blob, in one pass
	cat := exec.CommandContext(ctx, "git", "-C", dir, "cat-file", "--batch")
	cat.Stdin = strings.NewReader(strings.Join(ids, "\n") + "\n")
	stdout, err := cat.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cat.Stderr = &stderr
	if err := cat.Start(); err != nil {
		return nil, err
	}
	fail := func(err error) ([]Problem, error) {
		cat.Process.Kill()
		cat.Wait()
		return nil, fmt.Errorf("git cat-file: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var problems []Problem
	r := bufio.NewReader(stdout)
	for range ids {
		head, err := r.ReadString('\n')
		if err != nil {
			return fail(err)
		}
		f := strings.Fields(head)
		size, perr := int64(0), fmt.Errorf("%q", strings.TrimSpace(head))
		if len(f) == 3 {
			size, perr = strconv.ParseInt(f[2], 10, 64)
		}
		if perr != nil {
			return fail(perr)
		}
		body := make([]byte, size+1) // the content and its newline
		if _, err := io.ReadFull(r, body); err != nil {
			return fail(err)
		}
		if f[1] != "blob" {
			continue
		}
		path := paths[f[0]]
		if size > Limit {
			problems = append(problems, Problem{Path: path, Size: size, Why: fmt.Sprintf("over %d bytes", Limit)})
		}
		if kind := Executable(body[:size]); kind != "" {
			problems = append(problems, Problem{Path: path, Size: size, Why: "an executable binary (" + kind + ")"})
		}
	}
	if err := cat.Wait(); err != nil {
		return nil, fmt.Errorf("git cat-file: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return problems, nil
}

// Executable names the executable or object format b starts as: ELF, Mach-O (one architecture or several), PE
// or WebAssembly; "" for anything else. A PE file is told by its PE signature at the offset its MZ header names,
// so a text that opens with "MZ" is not one.
func Executable(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x7fELF")):
		return "ELF"
	case bytes.HasPrefix(b, []byte{0xfe, 0xed, 0xfa, 0xce}), bytes.HasPrefix(b, []byte{0xfe, 0xed, 0xfa, 0xcf}),
		bytes.HasPrefix(b, []byte{0xce, 0xfa, 0xed, 0xfe}), bytes.HasPrefix(b, []byte{0xcf, 0xfa, 0xed, 0xfe}):
		return "Mach-O"
	case bytes.HasPrefix(b, []byte{0xca, 0xfe, 0xba, 0xbe}), bytes.HasPrefix(b, []byte{0xbe, 0xba, 0xfe, 0xca}):
		return "Mach-O universal or Java class"
	case bytes.HasPrefix(b, []byte("\x00asm")):
		return "WebAssembly"
	case bytes.HasPrefix(b, []byte("MZ")) && len(b) >= 0x40:
		off := int64(binary.LittleEndian.Uint32(b[0x3c:0x40]))
		if off >= 0x40 && off+4 <= int64(len(b)) && bytes.Equal(b[off:off+4], []byte("PE\x00\x00")) {
			return "PE"
		}
	}
	return ""
}

func stderrOf(err error) string {
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		return ": " + strings.TrimSpace(string(ee.Stderr))
	}
	return ""
}
