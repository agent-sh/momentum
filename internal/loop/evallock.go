package loop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// LockEval takes the lock of the evaluations whose units begin with EvalUnitPrefix. It lives where that unit
// namespace does, the user's service manager: $XDG_RUNTIME_DIR, else /run/user/UID, else the user's cache directory.
// It is never one data home, since an evaluation's sweep of leftovers stops every unit of its prefix whatever home
// started it. A test process has a prefix of its own, so it neither holds nor waits on a live evaluation's lock. A
// second evaluation fails at once.
func LockEval() (func(), error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if st, err := os.Stat(dir); dir == "" || err != nil || !st.IsDir() {
		dir = fmt.Sprintf("/run/user/%d", os.Getuid()) // the user manager's own runtime directory
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		cache, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(cache, "momentum")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, strings.TrimSuffix(EvalUnitPrefix, "-")+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another evaluation runs now")
		}
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
