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
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

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
	// a gate that exits at once, leaving a child in a session of its own that holds its output: the wait for the
	// output ends at the wait delay, and the hold comes then. The child is out of the gate's group, so the test ends it.
	pid := filepath.Join(t.TempDir(), "pid")
	t.Cleanup(func() { endPid(t, pid) })
	start = time.Now()
	if err := run(nil, "setsid sleep 7.654 & echo $! > '"+pid+"'; exit 1"); !isHold(err) {
		t.Fatalf("a gate whose child keeps its output: %T %v", err, err)
	}
	if took := time.Since(start); took >= 3*time.Second {
		t.Fatalf("a gate whose child keeps its output held the check %v", took)
	}
}

// endPid kills the process whose id the file holds, if it is there.
func endPid(t *testing.T, file string) {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		return
	}
	if n, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && n > 0 {
		syscall.Kill(n, syscall.SIGKILL)
	}
}

// shortGate runs the loop's capacity gate (setupShip) on a shell line once, with a wait delay short enough for a test.
func shortGate(t *testing.T, line string) error {
	t.Helper()
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	oldTimeout, oldDelay := gateTimeout, gateWaitDelay
	gateTimeout, gateWaitDelay = 5*time.Second, 300*time.Millisecond
	t.Cleanup(func() { gateTimeout, gateWaitDelay = oldTimeout, oldDelay })
	ship := setupShip(nil, config.Sources{Loop: config.Loop{CapacityGate: []string{"sh", "-c", line}}})
	return ship.Sandbox.Gate(context.Background())
}

// A gate that exits on its own ends its children with it, whether it said room or no room: its process group is
// killed after it, as at its deadline.
func TestAGateThatExitsOnItsOwnEndsItsChildren(t *testing.T) {
	// room, its child off its output
	if err := shortGate(t, "sleep 8.761 >/dev/null 2>&1 & exit 0"); err != nil {
		t.Fatalf("a gate that said room: %v", err)
	}
	if left := processesWith(t, "8.761"); len(left) > 0 {
		t.Fatalf("the child of a gate that said room outlived it: %v", left)
	}
	// no room, its child off its output
	var exit *exec.ExitError
	if err := shortGate(t, "sleep 8.762 >/dev/null 2>&1 & exit 1"); !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("a gate that said no room: %v", err)
	}
	if left := processesWith(t, "8.762"); len(left) > 0 {
		t.Fatalf("the child of a gate that said no room outlived it: %v", left)
	}
}

// A gate that exits 0 says there is room, even when a child it left holds its output past the wait delay: the wait for
// the output ends then, and the gate's 0 stands.
func TestAGateThatExitsZeroWhileAChildHoldsItsOutputSaysRoom(t *testing.T) {
	start := time.Now()
	if err := shortGate(t, "sleep 8.763 & exit 0"); err != nil {
		t.Fatalf("a gate that said room while its child held its output: %v", err)
	}
	if took := time.Since(start); took >= 3*time.Second {
		t.Fatalf("a gate whose child held its output held the check %v", took)
	}
	if left := processesWith(t, "8.763"); len(left) > 0 {
		t.Fatalf("the child that held a gate's output outlived it: %v", left)
	}
}

// What the gate printed reaches the log and the note on one line, and only its last 300 characters, after "...".
func TestGateSaidKeepsTheEndOfWhatTheGatePrinted(t *testing.T) {
	whole := strings.Repeat("ü", 300)
	if got := gateSaid([]byte(whole)); got != whole {
		t.Fatalf("a line of 300 characters was clipped: %q", got)
	}
	if got := gateSaid([]byte("a" + whole)); got != "..."+whole {
		t.Fatalf("a line of 301 characters: %q", got)
	}
	long := []byte("load 9 of 4\n" + strings.Repeat("ü", 296) + "  end\n")
	want := "..." + string([]rune("load 9 of 4 " + strings.Repeat("ü", 296) + " end")[12:])
	if got := gateSaid(long); got != want || !utf8.ValidString(got) || utf8.RuneCountInString(got) != 303 {
		t.Fatalf("what a gate printed past 300 characters: %q", got)
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
