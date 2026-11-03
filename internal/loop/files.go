package loop

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// OpenFile opens once within a directory the caller owns. Root confines path resolution while another
// process changes it. Links present during validation are refused, and the opened descriptor must be the file the
// last lstat saw (a root follows an inside link whatever O_NOFOLLOW says). O_NONBLOCK keeps a substituted pipe from
// holding the caller before it can check the descriptor.
func OpenFile(dir, name string) (*os.File, error) {
	if !filepath.IsLocal(name) {
		return nil, fmt.Errorf("file path is not inside its directory: %s", name)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	parts := strings.Split(name, string(filepath.Separator))
	var last os.FileInfo
	for i := range parts {
		st, err := root.Lstat(filepath.Join(parts[:i+1]...))
		if err != nil {
			return nil, err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("file path contains a symlink: %s", name)
		}
		last = st
	}
	openFileChecked()
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	// A root follows a link that stays inside it whatever the open's flags say: the descriptor must be the file the
	// last lstat saw, so a link swapped in after the checks is refused.
	if st, err := f.Stat(); err != nil || !os.SameFile(last, st) {
		f.Close()
		return nil, fmt.Errorf("file changed while it was opened: %s", name)
	}
	return f, nil
}

// openFileChecked runs between OpenFile's checks and its open; only a test sets it, to race them.
var openFileChecked = func() {}

// ReadFile checks and reads the same descriptor. A growing file cannot bypass the size limit.
func ReadFile(f *os.File, limit int64) ([]byte, error) {
	st, err := regularOwned(f)
	if err != nil {
		return nil, err
	}
	if st.Size() > limit {
		return nil, fmt.Errorf("%s exceeds the file limit of %d bytes", f.Name(), limit)
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s exceeds the file limit of %d bytes", f.Name(), limit)
	}
	return b, nil
}

func regularOwned(f *os.File) (os.FileInfo, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", f.Name())
	}
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Getuid()) {
		return nil, fmt.Errorf("%s is not owned by the caller", f.Name())
	}
	return st, nil
}
