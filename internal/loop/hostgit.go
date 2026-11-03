package loop

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type hostStorageKey struct{}

func (s *Ship) gitContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, hostStorageKey{}, []string{
		filepath.Join(s.Dir, "work"), filepath.Join(s.Dir, "runs"), s.Sandbox.Cache,
	})
}

func gitResolved(dir string) string {
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return dir
	}
	return p
}

// CheckHostStorage checks host repository metadata before Git can read it.
// Run roots are refused before inspecting any file within them.
func CheckHostStorage(ctx context.Context, dir string) error {
	if dir == "" {
		return refused("host Git needs an explicit repository")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	roots, _ := ctx.Value(hostStorageKey{}).([]string)
	plain := func(p string) error {
		for _, root := range roots {
			if root != "" && under(p, root) {
				return refused("host Git never reads a run directory or cache")
			}
		}
		return noInputLinks(p)
	}
	if err := plain(abs); err != nil {
		return err
	}
	meta := filepath.Join(abs, ".git")
	if st, err := os.Lstat(meta); err == nil && st.Mode().IsRegular() {
		f, err := OpenFile(abs, ".git")
		if err != nil {
			return err
		}
		b, err := ReadFile(f, 4096)
		f.Close()
		if err != nil {
			return err
		}
		path, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
		if !ok {
			return refused("invalid repository pointer")
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(abs, path)
		}
		meta = filepath.Clean(path)
	}
	if err := plain(meta); err != nil {
		return err
	}
	metadata := []string{meta}
	if f, err := OpenFile(meta, "commondir"); err == nil {
		b, err := ReadFile(f, 4096)
		f.Close()
		if err != nil {
			return err
		}
		common := strings.TrimSpace(string(b))
		if !filepath.IsAbs(common) {
			common = filepath.Join(meta, common)
		}
		common = filepath.Clean(common)
		if err := plain(common); err != nil {
			return err
		}
		metadata = append(metadata, common)
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, root := range metadata {
		if err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.Type()&os.ModeSymlink != 0 || !e.IsDir() && !e.Type().IsRegular() {
				return refused("repository metadata is not a plain file or directory")
			}
			if filepath.Base(p) == "alternates" && !e.IsDir() {
				st, err := e.Info()
				if err != nil {
					return err
				}
				if st.Size() != 0 {
					return refused("repository uses alternate object storage")
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func hostGit(ctx context.Context, dir string, input []byte, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	pins := append(append([]string{"-C", dir}, GitSafety()...),
		"-c", "diff.external=", "-c", "protocol.ext.allow=never", "-c", "uploadpack.packObjectsHook=")
	cmd := exec.CommandContext(cctx, "git", append(pins, args...)...)
	cmd.Env = GitEnv()
	cmd.Stdin = bytes.NewReader(input)
	out, diag := Capture{Limit: MaxStream}, Capture{Limit: MaxStream}
	cmd.Stdout, cmd.Stderr = &out, &diag
	err := cmd.Run()
	if out.Over() || diag.Over() {
		return "", refused("Git output exceeds the whole-stream limit")
	}
	if err != nil {
		return "", fmt.Errorf("host git failed: %w", err)
	}
	return out.String(), nil
}

// cloneConfined reads a checked host source into fresh host-owned storage.
func (s *Ship) cloneConfined(ctx context.Context, source, dest string) error {
	if err := CheckHostStorage(s.gitContext(ctx), source); err != nil {
		return err
	}
	if err := CheckGitConfig(ctx, source); err != nil {
		return err
	}
	_, err := hostGit(ctx, source, nil, "clone", "--quiet", "--no-local", "--no-checkout", "--template=", "--", source, dest)
	return err
}

// Configuration inspection names includes without loading them. It carries no
// execution overrides, so a pinned false cannot hide a repository's program.
func hostConfig(ctx context.Context, dir string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", append([]string{"-C", dir, "config", "--no-includes"}, args...)...)
	cmd.Env = GitEnv()
	out, diag := Capture{Limit: MaxStream}, Capture{Limit: MaxStream}
	cmd.Stdout, cmd.Stderr = &out, &diag
	err := cmd.Run()
	if out.Over() || diag.Over() {
		return "", refused("configuration output exceeds its limit")
	}
	return out.String(), err
}
