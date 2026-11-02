package loop

import (
	"fmt"
	"os"
	"path/filepath"
)

// DoorClient finds the small sandbox binary beside momentum, or at the configured path.
func DoorClient(configured, momentum string) (string, error) {
	path := configured
	if path == "" {
		if real, err := filepath.EvalSymlinks(momentum); err == nil {
			momentum = real
		}
		path = filepath.Join(filepath.Dir(momentum), "momentum-door")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return path, CheckDoorClient(path)
}

// CheckDoorClient refuses to start a headless run without the sandbox's binary.
func CheckDoorClient(path string) error {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("the sandbox's momentum-door binary is missing or not executable at %q: install both commands with go install ./cmd/... or set loop.door_client", path)
	}
	return nil
}
