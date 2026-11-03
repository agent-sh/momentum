package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// lock takes a case's ship lock, a file in its package directory. A case's steps (prepare, check, propose, land,
// rollback) share its work tree, the scratch trees beside it and its package file, so two of them at once would
// overwrite each other. A step that finds the lock held fails at once and says so. The lock goes with the process
// that holds it: a step killed midway leaves none.
func (s *Ship) lock(ctx context.Context, caseID string) (func(), error) {
	if caseID == "" || caseID != filepath.Base(caseID) || strings.HasPrefix(caseID, ".") {
		return nil, refused("%q is no case id", caseID)
	}
	if _, err := s.Store.CaseByID(ctx, caseID); err != nil {
		return nil, err
	}
	dir := s.pkgDir(caseID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &shipHold{err: refused("another ship step is running on case %s: wait for it to end, then run this step again", caseID)}
		}
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// removeAll removes a tree a sandboxed program may have written: it restores the owner's access to every directory
// first (a test can leave a directory it made mode 000, which RemoveAll cannot enter) and never follows a link.
func removeAll(path string) error {
	path = filepath.Clean(path)
	if path == "." || path == string(filepath.Separator) {
		return fmt.Errorf("refusing to remove a tree without a parent")
	}
	parent, err := os.Open(filepath.Dir(path))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := removeEntry(int(parent.Fd()), filepath.Base(path)); err != nil {
		return &os.PathError{Op: "remove tree", Path: path, Err: err}
	}
	return nil
}

// Every lookup, chmod and unlink is relative to an open parent. Directory
// descriptors keep the walk below PATH_MAX and O_NOFOLLOW keeps links inert.
func removeEntry(parent int, name string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if err == unix.ENOENT {
			return nil
		}
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unix.Unlinkat(parent, name, 0)
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err == unix.EACCES {
		if err := unix.Fchmodat(parent, name, 0700, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		fd, err = unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(fd), name)
	defer dir.Close()
	if err := dir.Chmod(0700); err != nil {
		return err
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := removeEntry(fd, entry.Name()); err != nil {
			return err
		}
	}
	return unix.Unlinkat(parent, name, unix.AT_REMOVEDIR)
}
