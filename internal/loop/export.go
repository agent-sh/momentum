package loop

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// export reads only objects of the host clone. No registration or .git file goes into a sandbox.
// Archive honors no repository or machine attributes: even export-ignore and export-subst are inert.
func (s *Ship) export(ctx context.Context, p Package, name, treeish string) (string, error) {
	dir := filepath.Join(s.Dir, "exports", "."+name+"-"+p.Case)
	if err := removeAll(dir); err != nil {
		return "", fmt.Errorf("a scratch tree of an earlier step cannot be removed: %w", err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := CheckHostStorage(s.gitContext(ctx), p.Host); err != nil {
		return "", err
	}
	if err := CheckGitConfig(ctx, p.Host); err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	attrs, err := os.MkdirTemp(filepath.Dir(dir), ".empty-attributes-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(attrs)
	cmd := exec.CommandContext(cctx, "git", append(append([]string{"-C", p.Host, "--work-tree=" + attrs}, GitSafety()...),
		"-c", "protocol.allow=never", "archive", "--worktree-attributes", "--format=tar", treeish)...)
	cmd.Env = GitEnv()
	stream, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	diag := Capture{Limit: MaxStream}
	cmd.Stderr = &diag
	if err := cmd.Start(); err != nil {
		return "", err
	}
	err = extractTree(dir, tar.NewReader(stream))
	if err != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if err != nil || waitErr != nil || diag.Over() {
		return "", errors.Join(fmt.Errorf("export tree: %w: %s", errors.Join(err, waitErr), diag.String()), s.unexport(p, dir))
	}
	return dir, nil
}

func extractTree(dir string, reader *tar.Reader) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for {
		h, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue // Git puts the commit id in this metadata record.
		}
		name := strings.TrimSuffix(h.Name, "/")
		if err := patchPath(name); err != nil {
			return err
		}
		// A tree cannot have entries below a symlink. Reject one even in a malformed archive.
		parts := strings.Split(name, "/")
		for i := 1; i < len(parts); i++ {
			st, err := root.Lstat(filepath.Join(parts[:i]...))
			if err != nil || !st.IsDir() {
				return refused("export path has a non-directory parent: %s", name)
			}
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = root.Mkdir(name, 0700)
		case tar.TypeReg:
			// Git regular blobs have only these two modes. Match the baseline
			// copy, independent of the caller's umask or archive permission bits.
			mode := os.FileMode(0o644)
			if h.Mode&0100 != 0 {
				mode = 0o755
			}
			var f *os.File
			f, err = root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err == nil {
				_, err = io.Copy(f, reader)
				err = errors.Join(err, f.Chmod(mode), f.Close())
			}
		case tar.TypeSymlink:
			// Existing base symlinks are exported as links. Patches cannot add or change them.
			err = root.Symlink(h.Linkname, name)
		default:
			err = refused("unexpected archive entry: %s", name)
		}
		if err != nil {
			return err
		}
	}
}

func (s *Ship) unexport(p Package, dir string) error {
	if err := removeAll(dir); err != nil {
		return fmt.Errorf("export scratch tree cannot be removed: %w", err)
	}
	return nil
}

func (s *Ship) dropRollback(p Package, dir string) error {
	_, _ = s.git(context.Background(), p.Host, "worktree", "remove", "--force", dir)
	err := removeAll(dir)
	_, pruneErr := s.git(context.Background(), p.Host, "worktree", "prune")
	if err != nil {
		err = fmt.Errorf("rollback scratch tree cannot be removed: %w", err)
	}
	return errors.Join(err, pruneErr)
}
