package capacity

import (
	"os"
	"path/filepath"
	"testing"
)

// These fixtures deliberately put build output outside every repository. A managed scratch root can expose
// synthetic ancestor .git metadata, which prevents that fixture from being made.
func requireLooseScratch(t *testing.T, dir string) {
	t.Helper()
	for parent := filepath.Dir(dir); ; parent = filepath.Dir(parent) {
		if _, err := os.Lstat(filepath.Join(parent, ".git")); err == nil {
			t.Skipf("loose scratch fixture cannot be made beneath ancestor Git metadata: %s", parent)
		}
		if filepath.Dir(parent) == parent {
			return
		}
	}
}
