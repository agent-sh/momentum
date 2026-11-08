package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/config"
	"github.com/agent-sh/momentum/internal/loop"
)

// The loop's capacity gate (setupShip) on a gate program that is not there is no hold: a check that waits at it fails
// as the host's error, which the stop rule counts, instead of being held as a machine with no room every pass.
func TestAGateProgramThatIsNotThereIsNoHold(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	ship := setupShip(nil, config.Sources{Loop: config.Loop{CapacityGate: []string{"momentum-test-absent-gate", "--capacity", "4,1"}}})
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
	// what the gate printed is the loop's to log and note, never in the error's text, which becomes case lines
	err := run(nil, "echo 'load 9 of 4' >&2; exit 1")
	var said *loop.GateSaid
	if !isHold(err) || !errors.As(err, &said) || said.Said != "load 9 of 4" || strings.Contains(err.Error(), "load 9") {
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
	// a gate whose shell has a child (sleep, then another command) ends at its deadline, its child with it
	start := time.Now()
	if err := run([]int{4}, "sleep 6.789; true"); !isHold(err) || !strings.Contains(err.Error(), "its wait passed") {
		t.Fatalf("a gate its own deadline ended: %T %v", err, err)
	}
	if took := time.Since(start); took >= time.Second {
		t.Fatalf("a gate past its deadline held the check %v", took)
	}
	if left := processesWith(t, "6.789"); len(left) > 0 {
		t.Fatalf("the gate's child outlived its deadline: %v", left)
	}
}

// processesWith lists the processes whose command line holds word, from /proc, after a moment for a killed group's
// exit to be seen.
func processesWith(t *testing.T, word string) []string {
	t.Helper()
	time.Sleep(100 * time.Millisecond)
	dirs, err := os.ReadDir("/proc")
	if err != nil {
		t.Skip(err)
	}
	var out []string
	for _, d := range dirs {
		if _, err := strconv.Atoi(d.Name()); err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", d.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if line := strings.ReplaceAll(string(raw), "\x00", " "); strings.Contains(line, word) {
			out = append(out, d.Name()+": "+line)
		}
	}
	return out
}
