package loop

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Records and unit logs share these limits. The total byte limit also applies
// to index.txt. The count and per-record limits normally stop collection first.
const (
	MaxRecords      = 200
	MaxRecordBytes  = 16 << 10
	MaxRecordsBytes = 8 << 20
	MaxNeighbours   = 5
	NeighbourWindow = 10 * time.Minute
	MaxUnits        = 25
	// recordSlack is what the child's header and clip marker add to a record clipped at the bound.
	recordSlack = 4 << 10
	// unitLogLines is how much of a unit's log is asked for: its end.
	unitLogLines = 120
	// ActiveUnitWindow is how far back a unit's journal must have entries to be rendered without being named.
	ActiveUnitWindow = 3 * 24 * time.Hour
)

type RecordsOptions struct {
	Store       string // the scrubbed plain store copy
	IDs         []string
	Units       []string
	Destination string
	Binary      string
	Env         []string
	Home        string // the child command's MOMENTUM_HOME
	AsOf        time.Time
	Neighbours  int // zero uses five per side; negative disables neighbours
	Bytes       int // zero uses 16 KiB; a larger value is refused
}

type RecordFile struct {
	Name  string    `json:"name"`
	Event string    `json:"event,omitempty"`
	At    time.Time `json:"at,omitempty"`
	Unit  string    `json:"unit,omitempty"`
	Bytes int       `json:"bytes"`
}

type RecordsManifest struct {
	Written  int          `json:"written"`
	Withheld int          `json:"withheld"`
	Skipped  int          `json:"skipped"`
	Bytes    int          `json:"bytes"`
	Stopped  string       `json:"stopped,omitempty"`
	Files    []RecordFile `json:"files"`
}

var unitNameRE = regexp.MustCompile(`^[A-Za-z0-9@._-]+\.(service|timer)$`)
var unitFinderRE = regexp.MustCompile(`[A-Za-z0-9@._-]+\.(?:service|timer)`)

// FindUnits finds up to MaxUnits unique unit names in text order. It refuses matches
// embedded in paths or larger names, rather than extracting a path's basename.
func FindUnits(texts ...string) []string {
	var names []string
	seen := map[string]bool{}
	for _, text := range texts {
		for _, loc := range unitFinderRE.FindAllStringIndex(text, -1) {
			if loc[0] > 0 && unitBoundary(text[loc[0]-1]) || loc[1] < len(text) && unitBoundary(text[loc[1]]) {
				continue
			}
			name := text[loc[0]:loc[1]]
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
				if len(names) == MaxUnits {
					return names
				}
			}
		}
	}
	return names
}

// ActiveUnits are the user services the machine runs of its own (a unit file in Home/.config/systemd/user) whose journal
// holds entries in the window, busiest first, at most MaxUnits: the logs a run might want without being able to name
// them in advance, since it has no command to ask for one. journalctl cannot list a field's values inside a window,
// so it prints the field of every entry of the window and the names are counted here. A journal that cannot be read
// gives none.
func ActiveUnits(ctx context.Context, home string, since, until time.Time) []string {
	files, err := os.ReadDir(filepath.Join(home, ".config", "systemd", "user"))
	if err != nil {
		return nil
	}
	own := map[string]bool{}
	for _, f := range files {
		if f.Type().IsRegular() && unitNameRE.MatchString(f.Name()) && !strings.Contains(f.Name(), "@") {
			own[f.Name()] = true
		}
	}
	if len(own) == 0 {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "journalctl", "--user", "--no-pager", "--since", since.Local().Format("2006-01-02 15:04:05"),
		"--until", until.Local().Format("2006-01-02 15:04:05"), "-o", "cat", "--output-fields=_SYSTEMD_USER_UNIT")
	cmd.Env = CleanEnv(nil)
	out := Capture{Limit: 64 << 20}
	cmd.Stdout = &out
	if cmd.Run() != nil && out.Len() == 0 || out.Over() {
		return nil
	}
	counts := map[string]int{}
	for _, line := range strings.Split(out.String(), "\n") {
		if own[line] {
			counts[line]++
		}
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if counts[names[i]] != counts[names[j]] {
			return counts[names[i]] > counts[names[j]]
		}
		return names[i] < names[j]
	})
	if len(names) > MaxUnits {
		names = names[:MaxUnits]
	}
	return names
}

// UnionUnits puts the first list's names first and adds the second's, without repeats, at most MaxUnits.
func UnionUnits(first, second []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, name := range append(append([]string(nil), first...), second...) {
		if !seen[name] && len(out) < MaxUnits {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

func unitBoundary(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune("@._-/\\", rune(b)) || b >= 128
}

// Records writes /ro/records from host evidence commands. Evidence IDs retain
// priority over neighbours. The pinned host children scrub output once, before
// clipping. Their already-scrubbed captures are published without another pass.
func Records(ctx context.Context, o RecordsOptions) (RecordsManifest, error) {
	if err := CheckScrubber(ctx); err != nil {
		return RecordsManifest{}, err
	}
	if err := recordOptions(o); err != nil {
		return RecordsManifest{}, err
	}
	db, err := sql.Open("sqlite", sqliteInputURI(o.Store, true))
	if err != nil {
		return RecordsManifest{}, err
	}
	defer db.Close()
	files, skipped, err := recordSelection(ctx, db, o)
	if err != nil {
		return RecordsManifest{}, err
	}
	m := RecordsManifest{Skipped: skipped}
	return writeRecords(ctx, o, files, m, false)
}

// UnitLogs appends unit logs to an existing records directory, sharing its
// count and byte bounds. It may also create a directory for unit logs alone.
func UnitLogs(ctx context.Context, o RecordsOptions) (RecordsManifest, error) {
	if err := CheckScrubber(ctx); err != nil {
		return RecordsManifest{}, err
	}
	if err := recordOptions(o); err != nil {
		return RecordsManifest{}, err
	}
	m := RecordsManifest{}
	if _, err := os.Lstat(o.Destination); err == nil {
		if err := noInputLinks(o.Destination); err != nil {
			return m, err
		}
		var readErr error
		m, readErr = readRecordIndex(o.Destination)
		if readErr != nil {
			return m, readErr
		}
		return writeRecords(ctx, o, nil, m, true)
	} else if !os.IsNotExist(err) {
		return m, err
	}
	return writeRecords(ctx, o, nil, m, false)
}

func recordOptions(o RecordsOptions) error {
	if o.Binary == "" || o.Home == "" {
		return fmt.Errorf("records need a host binary and MOMENTUM_HOME")
	}
	if o.Bytes < 0 || o.Bytes > MaxRecordBytes || o.Neighbours > MaxNeighbours {
		return fmt.Errorf("record options exceed the input bounds")
	}
	return nil
}

func recordSelection(ctx context.Context, db *sql.DB, o RecordsOptions) ([]RecordFile, int, error) {
	var files, anchors []RecordFile
	seen := map[string]bool{}
	skipped := 0
	for _, raw := range o.IDs {
		id := strings.TrimPrefix(raw, "ev:")
		if seen[id] {
			continue
		}
		seen[id] = true
		if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(id) {
			return nil, 0, fmt.Errorf("invalid evidence id")
		}
		var ts int64
		err := db.QueryRowContext(ctx, "SELECT ts FROM events WHERE id=?", id).Scan(&ts)
		if err == sql.ErrNoRows {
			skipped++
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		file := RecordFile{Name: "ev-" + id + ".txt", Event: id, At: time.UnixMilli(ts).UTC()}
		files, anchors = append(files, file), append(anchors, file)
	}
	n := o.Neighbours
	if n == 0 {
		n = MaxNeighbours
	}
	for _, anchor := range anchors {
		if n < 0 {
			break
		}
		var session string
		if err := db.QueryRowContext(ctx, "SELECT session FROM events WHERE id=?", anchor.Event).Scan(&session); err != nil {
			return nil, 0, err
		}
		if session == "" {
			continue
		}
		for _, before := range []bool{true, false} {
			comp, order := ">", "ASC"
			if before {
				comp, order = "<", "DESC"
			}
			rows, err := db.QueryContext(ctx, `SELECT id,ts FROM events WHERE session=? AND ts>=? AND ts<=?
				AND (ts,id) `+comp+` (?,?) ORDER BY ts `+order+`,id `+order+` LIMIT ?`,
				session, anchor.At.Add(-NeighbourWindow).UnixMilli(), anchor.At.Add(NeighbourWindow).UnixMilli(),
				anchor.At.UnixMilli(), anchor.Event, n)
			if err != nil {
				return nil, 0, err
			}
			for rows.Next() {
				var id string
				var ts int64
				if err = rows.Scan(&id, &ts); err != nil {
					rows.Close()
					return nil, 0, err
				}
				if !seen[id] {
					seen[id] = true
					files = append(files, RecordFile{Name: "ev-" + id + ".txt", Event: id, At: time.UnixMilli(ts).UTC()})
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, 0, err
			}
		}
	}
	return files, skipped, nil
}

func writeRecords(ctx context.Context, o RecordsOptions, files []RecordFile, m RecordsManifest, appendOnly bool) (RecordsManifest, error) {
	seenUnits := map[string]bool{}
	for _, file := range m.Files {
		if file.Unit != "" {
			seenUnits[file.Unit] = true
		}
	}
	var names []string
	for _, name := range o.Units {
		if !unitNameRE.MatchString(name) {
			m.Skipped++
			continue
		}
		names = append(names, name)
	}
	// one scrubber call for every name: a start costs most of a second
	cleanNames, err := RedactAll(ctx, names)
	if err != nil {
		return m, err
	}
	for i, name := range names {
		if cleanNames[i] != name {
			m.Skipped++
			continue
		}
		if seenUnits[name] {
			continue
		}
		if len(seenUnits) == MaxUnits {
			m.Skipped++
			continue
		}
		seenUnits[name] = true
		files = append(files, RecordFile{Name: "unit-" + name + ".txt", Unit: name})
	}
	if !appendOnly {
		if err := absentInput(o.Destination); err != nil {
			return m, err
		}
	}
	// Stage new files until every scrubber call succeeded. On an append failure,
	// the earlier index and files remain unchanged.
	tmp, err := os.MkdirTemp(filepath.Dir(o.Destination), ".records-input-")
	if err != nil {
		return m, err
	}
	defer os.RemoveAll(tmp)
	if appendOnly {
		for _, file := range m.Files {
			f, err := OpenFile(o.Destination, file.Name)
			if err != nil {
				return m, err
			}
			b, err := ReadFile(f, MaxRecordBytes)
			f.Close()
			if err != nil {
				return m, err
			}
			if err = writeReadonlyInput(filepath.Join(tmp, file.Name), b); err != nil {
				return m, err
			}
		}
	}
	index := recordIndex(m.Files)
	// Bytes includes the existing index. Account for its replacement once.
	dataBytes := m.Bytes - len(index)
	limit := o.Bytes
	if limit == 0 {
		limit = MaxRecordBytes
	}
	// The pinned children scrub each complete invocation once, before clipping.
	// Their captured output needs no second scrub.
	type fetched struct {
		file RecordFile
		text string
	}
	var got []fetched
	for i, file := range files {
		if m.Written+len(got) >= MaxRecords {
			m.Stopped = "record count"
			m.Skipped += len(files) - i
			break
		}
		// The child clips an event's record at --bytes and adds its header and a marker, and a unit's log is its state
		// and the end of the log: both are asked for less than the bound and read with the slack the extras need.
		args := []string{"evidence", "unit", file.Unit, "--lines", strconv.Itoa(unitLogLines), "--bytes", strconv.Itoa(limit)}
		if file.Event != "" {
			args = []string{"evidence", "show", "--bytes", strconv.Itoa(limit), file.Event}
		}
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		cmd := exec.CommandContext(cctx, o.Binary, args...)
		cmd.Env = recordEnv(o)
		out, stderr := Capture{Limit: limit + recordSlack}, Capture{Limit: limit}
		cmd.Stdout, cmd.Stderr = &out, &stderr
		runErr := cmd.Run()
		cancel()
		if ctx.Err() != nil {
			return m, ctx.Err()
		}
		if out.Over() || stderr.Over() {
			// one record over its bound is withheld; the next may fit
			m.Withheld++
			continue
		}
		if runErr != nil || out.Len() == 0 || recordWithheld(out.String()) {
			m.Withheld++
			continue
		}
		got = append(got, fetched{file, out.String()})
	}
	for i, g := range got {
		file, clean := g.file, g.text
		if len(clean) > limit+recordSlack {
			m.Withheld++
			continue
		}
		file.Bytes = len(clean)
		line := recordIndex([]RecordFile{file})
		if !recordFits(dataBytes, len(index), file.Bytes, len(line)) {
			m.Stopped = "total bytes"
			m.Skipped += len(got) - i
			break
		}
		if err = writeReadonlyInput(filepath.Join(tmp, file.Name), []byte(clean)); err != nil {
			return m, err
		}
		m.Files = append(m.Files, file)
		m.Written++
		dataBytes += file.Bytes
		index += line
	}
	m.Bytes = dataBytes + len(index)
	if err = writeReadonlyInput(filepath.Join(tmp, "index.txt"), []byte(index)); err != nil {
		return m, err
	}
	if appendOnly {
		// The caller owns this input tree. Publish files before its new index.
		for _, file := range m.Files {
			target := filepath.Join(o.Destination, file.Name)
			if _, err := os.Lstat(target); os.IsNotExist(err) {
				if err = os.Link(filepath.Join(tmp, file.Name), target); err != nil {
					return m, err
				}
			}
		}
		if err = os.Rename(filepath.Join(tmp, "index.txt"), filepath.Join(o.Destination, "index.txt")); err != nil {
			return m, err
		}
	} else if err = os.Rename(tmp, o.Destination); err != nil {
		return m, err
	}
	return m, nil
}

func recordFits(data, index, next, line int) bool {
	return data+index+next+line <= MaxRecordsBytes
}

func recordEnv(o RecordsOptions) []string {
	var env []string
	for _, kv := range o.Env {
		if !strings.HasPrefix(kv, "MOMENTUM_HOME=") && !strings.HasPrefix(kv, "MOMENTUM_NOW=") {
			env = append(env, kv)
		}
	}
	env = append(env, "MOMENTUM_HOME="+o.Home)
	if !o.AsOf.IsZero() {
		env = append(env, "MOMENTUM_NOW="+o.AsOf.UTC().Format(time.RFC3339Nano))
	}
	return env
}

func recordWithheld(text string) bool {
	for _, marker := range []string{"record withheld", "output withheld", "(its record is gone)", "(its record is not a regular file)"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func recordIndex(files []RecordFile) string {
	var b strings.Builder
	for _, file := range files {
		if file.Event != "" {
			fmt.Fprintf(&b, "%s\t%s\t%s\n", file.Name, file.Event, file.At.UTC().Format(time.RFC3339Nano))
		} else {
			fmt.Fprintf(&b, "%s\tunit\t%s\n", file.Name, file.Unit)
		}
	}
	return b.String()
}

func readRecordIndex(dir string) (RecordsManifest, error) {
	m := RecordsManifest{}
	f, err := OpenFile(dir, "index.txt")
	if err != nil {
		return m, err
	}
	b, err := ReadFile(f, MaxRecordsBytes)
	f.Close()
	if err != nil {
		return m, err
	}
	m.Bytes = len(b)
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || seen[fields[0]] {
			return m, fmt.Errorf("invalid record index")
		}
		seen[fields[0]] = true
		file := RecordFile{Name: fields[0]}
		if fields[1] == "unit" {
			file.Unit = fields[2]
			if !unitNameRE.MatchString(file.Unit) || file.Name != "unit-"+file.Unit+".txt" {
				return m, fmt.Errorf("invalid unit index entry")
			}
		} else {
			file.Event = fields[1]
			file.At, err = time.Parse(time.RFC3339Nano, fields[2])
			if err != nil || file.Name != "ev-"+file.Event+".txt" {
				return m, fmt.Errorf("invalid event index entry")
			}
		}
		f, err := OpenFile(dir, file.Name)
		if err != nil {
			return m, err
		}
		text, err := ReadFile(f, MaxRecordBytes)
		f.Close()
		if err != nil {
			return m, err
		}
		file.Bytes = len(text)
		m.Bytes += len(text)
		m.Files = append(m.Files, file)
		m.Written++
	}
	if m.Written > MaxRecords || m.Bytes > MaxRecordsBytes {
		return m, fmt.Errorf("existing records exceed the input bounds")
	}
	return m, nil
}
