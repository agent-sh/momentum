package evidenceview

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

const ShowMost = 10

func ParseShow(args []string, stderr io.Writer) ([]string, int, error) {
	fs := flag.NewFlagSet("evidence show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cut := fs.Int("bytes", 0, "each record's first N bytes, once it is scrubbed whole (default: all of it)")
	var flags, ids []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" { // the end of the flags: every word after it is an id
			ids = append(ids, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			ids = append(ids, a)
			continue
		}
		flags = append(flags, a)
		if !strings.Contains(a, "=") && a != "-h" && a != "--help" && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	if err := fs.Parse(flags); err != nil {
		return nil, 0, err
	}
	if fs.NArg() > 0 { // a flag's value that is no value: nothing given is left unread
		return nil, 0, fmt.Errorf("evidence show: %q is no flag's value: give each event's id, flags as --bytes N", fs.Arg(0))
	}
	if len(ids) == 0 || len(ids) > ShowMost || *cut < 0 {
		return nil, 0, fmt.Errorf("evidence show takes 1 to %d events, each its id or ev:ID, and --bytes N", ShowMost)
	}
	return ids, *cut, nil
}
