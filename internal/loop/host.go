package loop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// HostDir is the run's host-only sibling. No run mount may contain it.
func HostDir(dir string) string {
	return filepath.Join(filepath.Dir(dir), "."+filepath.Base(dir)+"-host")
}

func hostDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode().Perm() != 0o700 {
		return fmt.Errorf("host directory is not a private directory")
	}
	return nil
}

func writeExclusive(dir, name string, data []byte) error {
	f, err := exclusiveOutput(dir, name)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}

func exclusiveOutput(dir, name string) (*os.File, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
}

// Reused writable directories are metadata only. Never follow a link left by a primary attempt.
func runDirectories(dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, name := range []string{"tmp", "claude"} {
		st, err := root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			if err := root.Mkdir(name, 0700); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !st.IsDir() {
			return fmt.Errorf("run directory %s is not a directory", name)
		}
	}
	return nil
}
