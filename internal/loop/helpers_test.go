package loop

import (
	"github.com/agent-sh/momentum/internal/store"
	"os"

	"path/filepath"

	"testing"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, _ := testStoreAt(t)
	return st
}

func testStoreAt(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, path
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	write(t, p, "#!/bin/sh\n"+body+"\n")
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// shortScratch keeps fixture relay sockets under Unix's pathname bound (about 108 bytes): a directory directly under
// the user's runtime directory when there is one, else under the temporary directory. It is removed by the test.
func shortScratch(t *testing.T) string {
	t.Helper()
	base := os.Getenv("XDG_RUNTIME_DIR")
	if st, err := os.Stat(base); base == "" || err != nil || !st.IsDir() {
		base = ""
	}
	dir, err := os.MkdirTemp(base, "l-")
	if err != nil {
		t.Fatal(err)
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	t.Cleanup(func() { removeInputTree(dir) })
	return dir
}
