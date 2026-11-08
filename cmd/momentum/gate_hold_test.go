package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/config"
)

// The loop's capacity gate (setupShip) on a gate program that is not there is no hold: a check that waits at it fails
// as the host's error, which the stop rule counts, instead of being held as a machine with no room every pass.
func TestAGateProgramThatIsNotThereIsNoHold(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	ship := setupShip(nil, config.Sources{Loop: config.Loop{CapacityGate: []string{"momentum-test-absent-gate", "--capacity", "20,8"}}})
	_, err := ship.Sandbox.Run(context.Background(), t.TempDir(), []string{"go", "test", "./..."}, nil)
	if !errors.Is(err, exec.ErrNotFound) || fmt.Sprintf("%T", err) == "*loop.shipHold" {
		t.Fatalf("a gate program that is not there: %T %v", err, err)
	}
}

// The real gate holds on its hold exits and on its own deadline, and says what the gate printed; another exit is no
// hold.
func TestTheRealGateHoldsOnItsHoldExitsAndItsDeadline(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	run := func(holds []int, line string) error {
		ship := setupShip(nil, config.Sources{Loop: config.Loop{CapacityGate: []string{"sh", "-c", line}, CapacityHoldExit: holds}})
		_, err := ship.Sandbox.Run(context.Background(), t.TempDir(), []string{"go", "test", "./..."}, nil)
		return err
	}
	isHold := func(err error) bool { return fmt.Sprintf("%T", err) == "*loop.shipHold" }
	if err := run(nil, "echo 'load 31 of 20' >&2; exit 1"); !isHold(err) || !strings.Contains(err.Error(), "it said: load 31 of 20") {
		t.Fatalf("exit 1 with no hold exits configured: %T %v", err, err)
	}
	if err := run(nil, "exit 4"); err == nil || isHold(err) {
		t.Fatalf("exit 4 with no hold exits configured: %T %v", err, err)
	}
	if err := run([]int{4}, "exit 4"); !isHold(err) {
		t.Fatalf("exit 4 configured to hold: %T %v", err, err)
	}
	oldTimeout, oldDelay := gateTimeout, gateWaitDelay
	gateTimeout, gateWaitDelay = 200*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { gateTimeout, gateWaitDelay = oldTimeout, oldDelay })
	// a gate whose shell has a child that outlives it (sleep, then another command) ends at the deadline too
	start := time.Now()
	if err := run([]int{4}, "sleep 5; true"); !isHold(err) || !strings.Contains(err.Error(), "its wait passed") {
		t.Fatalf("a gate its own deadline ended: %T %v", err, err)
	}
	if took := time.Since(start); took >= time.Second {
		t.Fatalf("a gate past its deadline held the check %v", took)
	}
}
