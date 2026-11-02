// Command check fails when a range of commits adds a file over 1 MiB or an executable binary, in any of its
// commits. CI runs it on each pull request and push: go run ./internal/tracked/check BASE..HEAD (HEAD alone reads
// the whole history).
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/agent-sh/momentum/internal/tracked"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: check REVISION... (git rev-list's arguments, such as BASE..HEAD)")
		os.Exit(2)
	}
	problems, err := tracked.Check(context.Background(), ".", os.Args[1:]...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check:", err)
		os.Exit(2)
	}
	for _, p := range problems {
		fmt.Println(p)
	}
	if len(problems) > 0 {
		fmt.Fprintf(os.Stderr, "check: %d file(s) the repository does not take: no file over %d bytes and no build output\n", len(problems), tracked.Limit)
		os.Exit(1)
	}
}
