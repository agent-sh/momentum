package loop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoorClientUsesTheSiblingOrConfiguredBinary(t *testing.T) {
	dir := t.TempDir()
	momentum := filepath.Join(dir, "momentum")
	sibling := filepath.Join(dir, "momentum-door")
	if _, err := DoorClient("", momentum); err == nil || !strings.Contains(err.Error(), "go install ./cmd/...") || !strings.Contains(err.Error(), "loop.door_client") {
		t.Fatalf("missing binary had no useful refusal: %v", err)
	}
	write(t, sibling, "marker-7f3a")
	if _, err := DoorClient("", momentum); err == nil {
		t.Fatal("non-executable client accepted")
	}
	if err := os.Chmod(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := DoorClient("", momentum); err != nil || got != sibling {
		t.Fatalf("sibling: %s %v", got, err)
	}
	override := filepath.Join(t.TempDir(), "client")
	write(t, override, "marker-7f3a")
	os.Chmod(override, 0o700)
	if got, err := DoorClient(override, momentum); err != nil || got != override {
		t.Fatalf("configured: %s %v", got, err)
	}
}
