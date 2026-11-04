package loop

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// buildTreeEntry is one entry of the base commit's tree.
type buildTreeEntry struct{ mode, typ, sha, path string }

// buildTreeCommitter is the fixed identity and time of a build tree's one commit: it names no host or person.
const buildTreeCommitter = "momentum <> 0 +0000"

// writeBuildTree makes dest, a new empty directory, a repository that holds one commit with no parent whose tree is
// the tree of commit in the host clone host, and returns the commit's id. It holds no other object, no ref, no remote
// and no reflog: a run gets the default branch's history only as the scrubbed text of /ro/history and /ro/diffs.
// What the tree holds is the caller's to scan.
func writeBuildTree(ctx context.Context, host, commit, dest string) (string, error) {
	out, err := hostInputGitLimit(ctx, host, treeListMax, "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return "", err
	}
	var gitlinks, blobs []buildTreeEntry
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		tab := bytes.IndexByte(rec, '\t')
		fields := strings.Fields(string(rec[:max(tab, 0)]))
		if tab < 0 || len(fields) != 3 {
			return "", fmt.Errorf("invalid tree listing")
		}
		e := buildTreeEntry{mode: fields[0], typ: fields[1], sha: fields[2], path: string(rec[tab+1:])}
		switch {
		case e.typ == "blob" && (e.mode == "100644" || e.mode == "100755" || e.mode == "120000"):
			blobs = append(blobs, e)
		case e.typ == "commit" && e.mode == "160000":
			gitlinks = append(gitlinks, e)
		default:
			return "", fmt.Errorf("invalid tree entry")
		}
	}
	format, err := hostInputGit(ctx, host, "rev-parse", "--show-object-format")
	if err != nil {
		return "", err
	}
	if _, err := hostGit(ctx, dest, nil, "init", "--quiet", "--template=", "--object-format="+strings.TrimSpace(string(format))); err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(ctx, 2*gitStepTimeout)
	defer cancel()
	imp := exec.CommandContext(cctx, "git", append(append([]string{"-C", dest}, GitSafety()...), "fast-import", "--quiet", "--done")...)
	imp.Env = GitEnv()
	impOut := Capture{Limit: ScrubBatchBytes}
	imp.Stdout, imp.Stderr = &impOut, &impOut
	stream, err := imp.StdinPipe()
	if err != nil {
		return "", err
	}
	if err = imp.Start(); err != nil {
		return "", fmt.Errorf("build tree writer failed")
	}
	waited := false
	defer func() {
		if !waited {
			cancel()
			stream.Close()
			imp.Wait()
		}
	}()
	w := bufio.NewWriter(stream)
	message := "momentum build base: the tree of " + commit + "\n"
	fmt.Fprintf(w, "commit refs/momentum/base\nauthor %s\ncommitter %s\ndata %d\n%s\n", buildTreeCommitter, buildTreeCommitter, len(message), message)
	for _, e := range gitlinks {
		fmt.Fprintf(w, "M 160000 %s %s\n", e.sha, fastImportPath(e.path))
	}
	if err := copyBuildBlobs(cctx, host, blobs, w); err != nil {
		if cctx.Err() != nil {
			return "", fmt.Errorf("the build tree write ran out of time or was cancelled")
		}
		return "", err
	}
	fmt.Fprint(w, "done\n")
	if err = w.Flush(); err != nil {
		return "", fmt.Errorf("build tree writer failed")
	}
	stream.Close()
	waited = true
	if err = imp.Wait(); err != nil || impOut.Over() {
		return "", fmt.Errorf("build tree writer failed")
	}
	id, err := hostGit(ctx, dest, nil, "rev-parse", "--verify", "refs/momentum/base^{commit}")
	if err != nil {
		return "", err
	}
	id = strings.TrimSpace(id)
	if _, err := hostGit(ctx, dest, nil, "checkout", "--quiet", "--detach", id); err != nil {
		return "", err
	}
	if _, err := hostGit(ctx, dest, nil, "update-ref", "-d", "refs/momentum/base"); err != nil {
		return "", err
	}
	return id, nil
}

// copyBuildBlobs streams the blobs from one git process into the import stream, one at a time, so memory stays at a
// buffer whatever a file's size.
func copyBuildBlobs(ctx context.Context, host string, blobs []buildTreeEntry, w io.Writer) error {
	if len(blobs) == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", append(append([]string{"-C", host}, GitSafety()...), "cat-file", "--batch")...)
	cmd.Env = GitEnv()
	stderr := Capture{Limit: ScrubBatchBytes}
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return err
	}
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("build tree reader failed")
	}
	go func() {
		bw := bufio.NewWriter(stdin)
		for _, e := range blobs {
			fmt.Fprintln(bw, e.sha)
		}
		bw.Flush()
		stdin.Close()
	}()
	// on an early return, stop the reader before waiting for it
	waited := false
	defer func() {
		if !waited {
			stdout.Close()
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	reader := bufio.NewReader(stdout)
	for _, e := range blobs {
		header, err := reader.ReadString('\n')
		if err != nil || len(header) > 256 {
			return fmt.Errorf("invalid build tree object header")
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[0] != e.sha || fields[1] != "blob" {
			return fmt.Errorf("build tree object is missing")
		}
		n, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || n < 0 {
			return fmt.Errorf("invalid build tree object size")
		}
		if _, err := fmt.Fprintf(w, "M %s inline %s\ndata %d\n", e.mode, fastImportPath(e.path), n); err != nil {
			return fmt.Errorf("build tree writer failed")
		}
		if _, err := io.CopyN(w, reader, n); err != nil {
			return fmt.Errorf("incomplete build tree object")
		}
		if end, err := reader.ReadByte(); err != nil || end != '\n' {
			return fmt.Errorf("invalid build tree object boundary")
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return fmt.Errorf("build tree writer failed")
		}
	}
	waited = true
	if err = cmd.Wait(); err != nil || stderr.Over() {
		return fmt.Errorf("build tree reader failed")
	}
	return nil
}

// fastImportPath quotes a path the way fast-import reads a C-style string: every byte but the quote, the backslash
// and control characters stands for itself.
func fastImportPath(p string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, "\\%03o", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
