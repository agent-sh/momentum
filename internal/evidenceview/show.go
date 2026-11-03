package evidenceview

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

const ShowMost = 10

// ParseShow reads the arguments of evidence show: the ids and --bytes N.
func ParseShow(args []string, stderr io.Writer) ([]string, int, error) {
	ids, cut, _, err := parseShow(args, stderr, false)
	return ids, cut, err
}

// ParseHostShow is ParseShow for the host command, which also takes --separate: the loop's records pass asks one child
// for up to ten events and gets them back apart. The run's reader never parses it.
func ParseHostShow(args []string, stderr io.Writer) (ids []string, cut int, separate bool, err error) {
	return parseShow(args, stderr, true)
}

func parseShow(args []string, stderr io.Writer, host bool) ([]string, int, bool, error) {
	fs := flag.NewFlagSet("evidence show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cut := fs.Int("bytes", 0, "each record's first N bytes, once it is scrubbed whole (default: all of it)")
	separate := new(bool)
	if host {
		separate = fs.Bool("separate", false, "separate the events by the record separator (U+001E); one that cannot be shown is empty")
	}
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
		boolean := host && (a == "-separate" || a == "--separate")
		if !boolean && !strings.Contains(a, "=") && a != "-h" && a != "--help" && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	if err := fs.Parse(flags); err != nil {
		return nil, 0, false, err
	}
	if fs.NArg() > 0 { // a flag's value that is no value: nothing given is left unread
		return nil, 0, false, fmt.Errorf("evidence show: %q is no flag's value: give each event's id, flags as --bytes N", fs.Arg(0))
	}
	if len(ids) == 0 || len(ids) > ShowMost || *cut < 0 {
		return nil, 0, false, fmt.Errorf("evidence show takes 1 to %d events, each its id or ev:ID, and --bytes N", ShowMost)
	}
	return ids, *cut, *separate, nil
}
