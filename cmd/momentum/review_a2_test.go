package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestCommandOutputWithholdsOversizedProduction(t *testing.T) {
	b, err := commandOutput(exec.Command("head", "-c", "8388610", "/dev/zero"), true)
	if err == nil || b != nil || !strings.Contains(err.Error(), "output withheld") {
		t.Fatalf("oversized helper output: %d bytes, %v", len(b), err)
	}
}
