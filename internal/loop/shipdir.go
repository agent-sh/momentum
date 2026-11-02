package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
			return nil, refused("another ship step is running on case %s: wait for it to end, then run this step again", caseID)
		}
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
