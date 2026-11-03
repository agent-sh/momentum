// Package ro reads only the copies supplied to a run. It has no host command path.
package ro

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/agent-sh/momentum/internal/evidenceview"
	"github.com/agent-sh/momentum/internal/store"
)

const Refusal = "momentum-ro reads the run's copies and changes nothing"
const Usage = `usage:
  momentum-ro evidence events [--since WHEN] [--until WHEN] [--kind K,K] [--source S]
                              [--harness H] [--session ID] [--limit N] [--json]
  momentum-ro evidence show ID [ID...] [--bytes N]  up to ten events and captured records
  momentum-ro evidence status [--since DUR]         events per source and kind
  momentum-ro evidence unit NAME                   captured unit output
  momentum-ro case show [--json]                   the run's case, evidence and log
Locations: /ro/store.sqlite, /ro/records, /run/input/case.json.
--store FILE, --records DIR and --case FILE override these for tests only.
Events use the host's filters, time syntax, ordering and limits (default 50, nonpositive 200).
Records are already scrubbed. No configuration, host store, network or other program is used.`

type locations struct{ store, records, caseFile string }
type usageError struct{ error }

var eventID = regexp.MustCompile(`^[a-f0-9]{24}$`)
var unitName = regexp.MustCompile(`^[A-Za-z0-9@._-]+[.](service|timer)$`)

// Run is the entire command set of an executable named momentum-ro.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	loc := locations{"/ro/store.sqlite", "/ro/records", "/run/input/case.json"}
	var rest []string
	for i := 0; i < len(args); i++ {
		name, value, equal := strings.Cut(args[i], "=")
		var dst *string
		switch name {
		case "--store":
			dst = &loc.store
		case "--records":
			dst = &loc.records
		case "--case":
			dst = &loc.caseFile
		default:
			rest = append(rest, args[i])
			continue
		}
		if !equal {
			i++
			if i == len(args) {
				fmt.Fprintln(stderr, Refusal)
				return 2
			}
			value = args[i]
		}
		*dst = value
	}
	if len(rest) == 1 && (rest[0] == "--help" || rest[0] == "-h" || rest[0] == "help") {
		fmt.Fprintln(stdout, Usage)
		fmt.Fprintln(stderr, Refusal)
		return 2
	}
	if len(rest) < 2 || !(rest[0] == "evidence" && (rest[1] == "events" || rest[1] == "show" || rest[1] == "status" || rest[1] == "unit") || rest[0] == "case" && rest[1] == "show") {
		fmt.Fprintln(stderr, Refusal)
		return 2
	}
	var err error
	switch {
	case rest[0] == "case":
		err = showCase(rest[2:], loc, stdout, stderr)
	case rest[1] == "events":
		err = events(ctx, rest[2:], loc, stdout, stderr)
	case rest[1] == "show":
		err = show(ctx, rest[2:], loc, stdout, stderr)
	case rest[1] == "status":
		err = status(ctx, rest[2:], loc, stdout, stderr)
	case rest[1] == "unit":
		err = unit(rest[2:], loc, stdout)
	}
	if err == nil {
		return 0
	}
	var ue usageError
	if errors.As(err, &ue) || errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(stderr, Refusal+":", err)
		return 2
	}
	fmt.Fprintln(stderr, "momentum-ro:", err)
	return 3
}

// openCopy accepts a filesystem path, never a caller-supplied SQLite URI.
// The copy has no schema bookkeeping tables and needs no migration.
func openCopy(ctx context.Context, path string) (*sql.DB, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("store copy is not a regular file")
	}
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&immutable=1"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// anchor makes relative times ("3d") count back from the newest event of the copy: a copy cut as of a past end
// would otherwise show nothing for the window the prompt suggests.
func anchor(ctx context.Context, db *sql.DB) {
	var ms sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT max(ts) FROM events`).Scan(&ms); err == nil && ms.Valid && ms.Int64 > 0 {
		end := time.UnixMilli(ms.Int64)
		evidenceview.Now = func() time.Time { return end }
	}
}

func events(ctx context.Context, args []string, loc locations, stdout, stderr io.Writer) error {
	db, err := openCopy(ctx, loc.store)
	if err != nil {
		return err
	}
	defer db.Close()
	anchor(ctx, db)
	opts, err := evidenceview.ParseEvents(args, stderr)
	if err != nil {
		return usageError{err}
	}
	evs, err := store.ReadEvents(ctx, db, opts.Query)
	if err != nil {
		return err
	}
	if len(evs) > opts.Limit {
		evs = evs[:opts.Limit]
		defer fmt.Fprintf(stderr, "momentum: more events than --limit %d; raise it, or narrow with --since and --until\n", opts.Limit)
	}
	return evidenceview.Events(stdout, evs, opts.JSON)
}

func status(ctx context.Context, args []string, loc locations, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("evidence status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	since := fs.String("since", "7d", "how far back")
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() != 0 {
		return usageError{errors.New("evidence status takes no arguments")}
	}
	d, err := evidenceview.ParseSince(*since)
	if err != nil {
		return usageError{err}
	}
	db, err := openCopy(ctx, loc.store)
	if err != nil {
		return err
	}
	defer db.Close()
	anchor(ctx, db)
	counts, err := store.ReadCounts(ctx, db, evidenceview.Now().Add(-d))
	if err != nil {
		return err
	}
	return evidenceview.Status(stdout, counts, nil)
}

func show(ctx context.Context, args []string, loc locations, stdout, stderr io.Writer) error {
	ids, cut, err := evidenceview.ParseShow(args, stderr)
	if err != nil {
		return usageError{err}
	}
	for _, raw := range ids {
		if !eventID.MatchString(strings.TrimPrefix(raw, "ev:")) {
			return usageError{errors.New("event id must be 24 hex characters, or ev:ID")}
		}
	}
	db, err := openCopy(ctx, loc.store)
	if err != nil {
		return err
	}
	defer db.Close()
	shown := 0
	for i, raw := range ids {
		id := strings.TrimPrefix(raw, "ev:")
		e, ok, err := store.ReadEventByID(ctx, db, id)
		if err != nil {
			return err
		}
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		if !ok {
			if len(ids) == 1 {
				return usageError{fmt.Errorf("the store holds no event %s", id)}
			}
			fmt.Fprintf(stdout, "%s: the store holds no event %s\n", raw, id)
			continue
		}
		shown++
		header := evidenceview.EventHeaderID(e, id)
		fmt.Fprint(stdout, header)
		record, err := readFile(loc.records, "ev-"+id+".txt", 256<<10)
		switch {
		case errors.Is(err, os.ErrNotExist):
			fmt.Fprintln(stdout, "  (record is not among the run's records)")
		case err != nil:
			return err
		default:
			// Host captures include this header. Print it once.
			text := strings.TrimPrefix(string(record), header)
			fmt.Fprint(stdout, evidenceview.RecordPrefix(text, cut))
		}
		if err := neighbours(ctx, db, loc.records, id, e, stdout); err != nil {
			return err
		}
	}
	if shown == 0 {
		return usageError{errors.New("none of them could be shown")}
	}
	return nil
}

func neighbours(ctx context.Context, db *sql.DB, records, id string, e store.Event, stdout io.Writer) error {
	if e.Session == "" {
		return nil
	}
	index, err := readFile(records, "index.txt", 8<<20)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(index), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 3 || !eventID.MatchString(parts[1]) || parts[1] == id || parts[0] != "ev-"+parts[1]+".txt" {
			continue
		}
		other, ok, err := store.ReadEventByID(ctx, db, parts[1])
		if err != nil {
			return err
		}
		if ok && other.Session == e.Session {
			fmt.Fprintf(stdout, "  neighbour ev:%s %s %s\n", parts[1], other.TS.Local().Format("2006-01-02 15:04:05"), parts[0])
		}
	}
	return nil
}

func unit(args []string, loc locations, stdout io.Writer) error {
	if len(args) != 1 || !unitName.MatchString(args[0]) {
		return usageError{errors.New("evidence unit takes one captured service or timer name")}
	}
	raw, err := readFile(loc.records, "unit-"+args[0]+".txt", 256<<10)
	if errors.Is(err, os.ErrNotExist) {
		return usageError{errors.New("unit is not among the run's records")}
	}
	if err != nil {
		return err
	}
	_, err = stdout.Write(raw)
	return err
}

func showCase(args []string, loc locations, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("case show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the case input file")
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() != 0 {
		return usageError{errors.New("case show takes no id: it reads this run's case")}
	}
	raw, err := readFile(filepath.Dir(loc.caseFile), filepath.Base(loc.caseFile), 8<<20)
	if err != nil {
		return err
	}
	var c struct {
		Case     store.Case        `json:"case"`
		Evidence []store.Evidence  `json:"evidence"`
		Log      []store.CaseEntry `json:"log"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return err
	}
	if *asJSON {
		_, err = stdout.Write(raw)
		return err
	}
	evs, log := c.Evidence, c.Log
	if len(evs) > 20 {
		evs = evs[:20]
	}
	if len(log) > 20 {
		log = log[len(log)-20:]
	}
	return evidenceview.Case(stdout, c.Case, evs, log, len(c.Log))
}

func readFile(dir, name string, limit int64) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// A root follows a link that stays inside it whatever the open's flags say, so the descriptor opened must be the
	// name itself, as lstat saw it: a link is another file than its target.
	link, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(link, info) || !info.Mode().IsRegular() {
		return nil, errors.New("run input is not a regular file")
	}
	// the bound is the read's: a file that grows after the stat is cut there too
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(raw)) > limit {
		return nil, errors.New("run input exceeds its limit")
	}
	return raw, err
}
