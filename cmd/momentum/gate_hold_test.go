package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"

	"github.com/agent-sh/momentum/internal/config"
)

// The loop's capacity gate (setupShip) on a gate program that is not there is no hold: a check that waits at it fails
// as the host's error, which the stop rule counts, instead of being held as a machine with no room every pass.
func TestAGateProgramThatIsNotThereIsNoHold(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ship := setupShip(nil, config.Sources{Loop: config.Loop{CapacityGate: []string{"momentum-test-absent-gate", "--capacity", "20,8"}}})
	_, err := ship.Sandbox.Run(context.Background(), t.TempDir(), []string{"go", "test", "./..."}, nil)
	if !errors.Is(err, exec.ErrNotFound) || fmt.Sprintf("%T", err) == "*loop.shipHold" {
		t.Fatalf("a gate program that is not there: %T %v", err, err)
	}
}
