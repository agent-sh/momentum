// momentum-door is the sandbox's init and command client. It holds no local momentum commands.
package main

import (
	"fmt"
	"os"

	"github.com/agent-sh/momentum/internal/jail"
)

func main() {
	if code, inside := jail.Inside(os.Args, os.Stdout, os.Stderr); inside {
		os.Exit(code)
	}
	fmt.Fprintln(os.Stderr, "usage: momentum-door sandbox-init --port P -- PROGRAM [ARG]...\n       momentum or tacit through the sandbox's door")
	os.Exit(2)
}
