package loop

import (
	"github.com/agent-sh/momentum/internal/store"
	"os"

	"path/filepath"

	"testing"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
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

// shortScratch keeps fixture relay sockets under Unix's pathname bound. It is
// inside the test checkout and removed by the test that owns it.
func shortScratch(t *testing.T) string {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(root) == "loop" && filepath.Base(filepath.Dir(root)) == "internal" {
		root = filepath.Dir(filepath.Dir(root))
	}
	dir, err := os.MkdirTemp(root, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeInputTree(dir) })
	return dir
}
