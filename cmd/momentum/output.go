package main

import (
	"fmt"
	"os/exec"

	"github.com/agent-sh/momentum/internal/loop"
)

// commandOutput bounds both streams during production, including helpers called by a host evidence command.
func commandOutput(cmd *exec.Cmd, combined bool) ([]byte, error) {
	out, diag := loop.Capture{Limit: loop.MaxStream}, loop.Capture{Limit: loop.MaxStream}
	cmd.Stdout, cmd.Stderr = &out, &diag
	if combined {
		cmd.Stderr = &out
	}
	err := cmd.Run()
	if out.Over() || diag.Over() {
		return nil, fmt.Errorf("(output withheld: over the output limit)")
	}
	return out.Bytes(), err
}
