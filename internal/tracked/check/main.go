// Command check fails when a range of commits adds a file over 1 MiB or an executable, object or archive file,
// in any of its commits. CI runs it on each pull request (go run ./internal/tracked/check BASE..HEAD) and on each
// push to main (HEAD: the whole history). It exits 0 when the range holds nothing to refuse, 1 when it does, and 2
// when it could not read the range.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/agent-sh/momentum/internal/tracked"
)

func main() { os.Exit(run(".", os.Args[1:], os.Stdout, os.Stderr)) }

func run(dir string, revs []string, stdout, stderr io.Writer) int {
	if len(revs) == 0 {
		fmt.Fprintln(stderr, "usage: check REVISION... (git rev-list's arguments, such as BASE..HEAD)")
		return 2
	}
	problems, err := tracked.Check(context.Background(), dir, revs...)
	if err != nil {
		fmt.Fprintln(stderr, "check:", err)
		return 2
	}
	for _, p := range problems {
		fmt.Fprintln(stdout, p)
	}
	if len(problems) > 0 {
		fmt.Fprintf(stderr, "check: %d file(s) the repository does not take: none over %d bytes, and no executable, object or archive file\n",
			len(problems), tracked.Limit)
		return 1
	}
	return 0
}
