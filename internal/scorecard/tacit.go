package scorecard

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// The tacit side reads tacit's delivery log (one JSON object per hook block in <state>/delivery/<UTC day>.jsonl):
// each block's hook, session, transcript and candidates, which of them it delivered, the self-mark asks and the
// marks agents gave, and the dreamer's judge labels. Only the fields read here are decoded; a line of another
// schema version, or one that does not parse, is counted and skipped.

// Session kinds a hook block ran in.
const (
	Interactive = "interactive" // the owner typed in the session
	Headless    = "headless"    // momentum's store holds the session and no word of the owner's in it
	Unknown     = "unknown"     // momentum's store holds nothing of the session (a harness it does not ingest)
	Subagent    = "subagent"    // a subagent's hook, as its transcript records it (Claude Code only)
)

// Kinds are the session kinds of the delivery log's blocks, in their order.
var Kinds = []string{Interactive, Headless, Unknown}

// DeliveryHooks are the hooks that deliver a block, in their order; nav, mark and judge entries are no block.
var DeliveryHooks = []string{"start", "resume", "compact", "summary", "prompt", "push"}

type logCand struct {
	Name      string `json:"name"`
	Prov      string `json:"prov"`
	Delivered bool   `json:"delivered"`
}

type logEntry struct {
	V              int       `json:"v"`
	ID             string    `json:"id"`
	TS             string    `json:"ts"`
	Hook           string    `json:"hook"`
	Session        string    `json:"session"`
	TranscriptPath string    `json:"transcript_path"`
	Cands          []logCand `json:"cands"`
	MarkAsked      bool      `json:"mark_asked"`
	Mark           *struct {
		Block     string   `json:"block"`
		Helped    []string `json:"helped"`
		NotHelped []string `json:"not_helped"`
	} `json:"mark"`
	Judge *struct {
		Block    string `json:"block"`
		Fact     string `json:"fact"`
		Relevant string `json:"relevant"`
		Novel    string `json:"novel"`
	} `json:"judge"`
	Nav *struct {
		Tool string `json:"tool"`
	} `json:"nav"`
	at time.Time
}

func (e logEntry) delivered() []string {
	var out []string
	for _, c := range e.Cands {
		if c.Delivered {
			out = append(out, c.Name)
		}
	}
	return out
}

// Log is what the delivery log holds over a span: its entries in time order, and what could not be read.
type Log struct {
	entries []logEntry
	// First is the oldest day file the log still holds; a week before it is partial.
	First   time.Time
	Skipped int
}

// ReadLog reads the delivery log's day files that can hold entries from since to until (a day's file is named by
// its UTC day).
func ReadLog(dir string, since, until time.Time) (*Log, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	l := &Log{}
	for _, de := range des {
		day, err := time.Parse("2006-01-02.jsonl", de.Name())
		if err != nil || !de.Type().IsRegular() {
			continue
		}
		if l.First.IsZero() || day.Before(l.First) {
			l.First = day
		}
		if day.Before(since.UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)) || !day.Before(until.UTC().AddDate(0, 0, 1)) {
			continue
		}
		if err := l.readFile(filepath.Join(dir, de.Name()), since, until); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(l.entries, func(i, j int) bool { return l.entries[i].at.Before(l.entries[j].at) })
	return l, nil
}

func (l *Log) readFile(path string, since, until time.Time) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return eachLine(f, 64<<20, func(line []byte) {
		var e logEntry
		if json.Unmarshal(line, &e) != nil || e.V != 1 {
			l.Skipped++
			return
		}
		t, err := time.Parse(time.RFC3339Nano, e.TS)
		if err != nil {
			l.Skipped++
			return
		}
		if t.Before(since) || !t.Before(until) {
			return
		}
		e.at = t
		l.entries = append(l.entries, e)
	})
}

// eachLine calls fn on each line of r up to max bytes; a longer line is skipped whole.
func eachLine(r io.Reader, max int, fn func([]byte)) error {
	br := bufio.NewReaderSize(r, 1<<20)
	var buf []byte
	long := false
	for {
		chunk, err := br.ReadSlice('\n')
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			if !long && len(buf)+len(chunk) <= max {
				buf = append(buf, chunk...)
			} else {
				long, buf = true, buf[:0]
			}
			continue
		case err != nil && !errors.Is(err, io.EOF):
			return err
		}
		if !long {
			line := chunk
			if len(buf) > 0 {
				buf = append(buf, chunk...)
				line = buf
			}
			if line = bytes.TrimRight(line, "\r\n"); len(line) > 0 && len(line) <= max {
				fn(line)
			}
		}
		buf, long = buf[:0], false
		if err != nil {
			return nil
		}
	}
}

// Count is a number of hook blocks and how many of them delivered a line.
type Count struct {
	Calls     int `json:"calls"`
	Delivered int `json:"delivered"`
}

// Share is the share of the calls that delivered, 0 with none.
func (c Count) Share() float64 {
	if c.Calls == 0 {
		return 0
	}
	return float64(c.Delivered) / float64(c.Calls)
}

// Latency is a hook's run time as the harness recorded it.
type Latency struct {
	N   int   `json:"n"`
	P50 int64 `json:"p50_ms"`
	P90 int64 `json:"p90_ms"`
}

// Marks are agents' self-marks: the asks, the answers, and the lines they marked.
type Marks struct {
	Asked       int `json:"asked"`
	Answered    int `json:"answered"`
	Used        int `json:"used"`
	NotRelevant int `json:"not_relevant"`
}

// Judge is the dreamer's judge labels of delivered lines.
type Judge struct {
	Labeled     int            `json:"labeled"`
	Relevant    map[string]int `json:"relevant"` // yes, no, unsure
	Novel       map[string]int `json:"novel"`
	RelevantNew int            `json:"relevant_and_new"`
}

// Restatement is an owner message that says again what a curated fact already holds.
type Restatement struct {
	At        time.Time `json:"at"`
	Session   string    `json:"session"`
	Fact      string    `json:"fact"`
	Score     float64   `json:"score"`
	Delivered bool      `json:"delivered_before"` // the session was given the fact before the message
	Text      string    `json:"-"`                // the message, for the model pass only; never in the numbers
	FactText  string    `json:"-"`
}

// TacitWeek is a week of tacit's numbers.
type TacitWeek struct {
	Week string `json:"week"`
	// Partial: the delivery log does not reach back to the week's start (it keeps a month).
	Partial bool             `json:"partial,omitempty"`
	ByHook  map[string]Count `json:"by_hook"`
	ByKind  map[string]Count `json:"by_kind"`
	Lines   int              `json:"lines"` // delivered lines
	Facts   int              `json:"distinct_facts"`
	Marks   Marks            `json:"marks"`
	// InteractiveMarks are the marks of blocks in interactive sessions: the scorecard's first test.
	InteractiveMarks Marks              `json:"interactive_marks"`
	Judge            Judge              `json:"judge"`
	Nav              map[string]int     `json:"nav"`         // tree, search and fact steps tacit logged (MCP and CLI)
	MCP              map[string]int     `json:"mcp"`         // tacit's MCP tool calls in transcripts, by tool
	MCPByKind        map[string]int     `json:"mcp_by_kind"` // the same by session kind
	Latency          map[string]Latency `json:"latency,omitempty"`
	// Subagent is the hook calls inside subagents and how many delivered, from the transcripts (Claude Code only).
	Subagent     *Count        `json:"subagent,omitempty"`
	Restatements int           `json:"restatements"`
	RestatedSeen int           `json:"restated_after_delivery"` // of them, the fact was delivered in the session first
	OwnerCovered bool          `json:"owner_covered"`           // momentum's store holds the owner's messages for the whole week
	Restated     []Restatement `json:"-"`
	// top are the most delivered facts, worst are the lines labeled or marked not relevant: the model pass's sample.
	top, worst []string
}

// Blocks is how many hook blocks the week's log holds.
func (t TacitWeek) Blocks() int {
	n := 0
	for _, c := range t.ByHook {
		n += c.Calls
	}
	return n
}

// Classifier says what kind of session a session id is.
type Classifier func(session string) string

// tacitWeek computes a week's numbers from the log.
func (l *Log) tacitWeek(w Week, kind Classifier) TacitWeek {
	t := TacitWeek{Week: w.Name, ByHook: map[string]Count{}, ByKind: map[string]Count{}, Nav: map[string]int{},
		Judge: Judge{Relevant: map[string]int{}, Novel: map[string]int{}}, MCP: map[string]int{}, MCPByKind: map[string]int{}}
	t.Partial = l.First.IsZero() || l.First.After(w.Start.UTC())
	asked := map[string]logEntry{}
	for _, e := range l.entries {
		if e.MarkAsked {
			asked[e.ID] = e
		}
	}
	facts := map[string]int{}
	answered := map[string]bool{}
	notRelevant := map[string]int{}
	for _, e := range l.entries {
		if e.at.Before(w.Start) || !e.at.Before(w.End) {
			continue
		}
		switch {
		case slices.Contains(DeliveryHooks, e.Hook):
			names := e.delivered()
			c, k := t.ByHook[e.Hook], t.ByKind[kind(e.Session)]
			c.Calls++
			k.Calls++
			if len(names) > 0 {
				c.Delivered++
				k.Delivered++
			}
			t.ByHook[e.Hook], t.ByKind[kind(e.Session)] = c, k
			t.Lines += len(names)
			for _, n := range names {
				facts[n]++
			}
			if e.MarkAsked {
				t.Marks.Asked++
				if kind(e.Session) == Interactive {
					t.InteractiveMarks.Asked++
				}
			}
		case e.Hook == "mark" && e.Mark != nil:
			if answered[e.Mark.Block] {
				continue // one answer a block: tacit takes the first
			}
			answered[e.Mark.Block] = true
			t.Marks.Answered++
			t.Marks.Used += len(e.Mark.Helped)
			t.Marks.NotRelevant += len(e.Mark.NotHelped)
			for _, n := range e.Mark.NotHelped {
				notRelevant[n]++
			}
			if b, ok := asked[e.Mark.Block]; ok && kind(b.Session) == Interactive {
				t.InteractiveMarks.Answered++
				t.InteractiveMarks.Used += len(e.Mark.Helped)
				t.InteractiveMarks.NotRelevant += len(e.Mark.NotHelped)
			}
		case e.Hook == "judge" && e.Judge != nil:
			t.Judge.Labeled++
			t.Judge.Relevant[label(e.Judge.Relevant)]++
			t.Judge.Novel[label(e.Judge.Novel)]++
			if e.Judge.Relevant == "yes" && e.Judge.Novel == "yes" {
				t.Judge.RelevantNew++
			}
			if e.Judge.Relevant == "no" {
				notRelevant[e.Judge.Fact]++
			}
		case e.Hook == "nav" && e.Nav != nil:
			t.Nav[e.Nav.Tool]++
		}
	}
	t.Facts = len(facts)
	t.top = topNames(facts, 15)
	t.worst = topNames(notRelevant, 15)
	return t
}

func label(s string) string {
	switch s {
	case "yes", "no", "unsure":
		return s
	}
	return "other"
}

// topNames are the n names counted most, most first, ties by name.
func topNames(m map[string]int, n int) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		if m[names[i]] != m[names[j]] {
			return m[names[i]] > m[names[j]]
		}
		return names[i] < names[j]
	})
	if len(names) > n {
		names = names[:n]
	}
	return names
}

// deliveredBefore says whether the session was given the fact in a block before t.
func (l *Log) deliveredBefore(session, fact string, t time.Time) bool {
	for _, e := range l.entries {
		if !e.at.Before(t) {
			break
		}
		if e.Session == session && slices.Contains(DeliveryHooks, e.Hook) && slices.Contains(e.delivered(), fact) {
			return true
		}
	}
	return false
}

// transcripts are the transcripts the log's blocks of a week ran in.
func (l *Log) transcripts(w Week) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range l.entries {
		if e.at.Before(w.Start) || !e.at.Before(w.End) || e.TranscriptPath == "" || seen[e.TranscriptPath] {
			continue
		}
		seen[e.TranscriptPath] = true
		out = append(out, e.TranscriptPath)
	}
	sort.Strings(out)
	return out
}

// hookRecord is a Claude Code transcript line that records a hook's run.
type hookRecord struct {
	Timestamp  string `json:"timestamp"`
	Sidechain  bool   `json:"isSidechain"`
	Attachment *struct {
		Type       string `json:"type"`
		Command    string `json:"command"`
		Content    string `json:"content"`
		DurationMs *int64 `json:"durationMs"`
	} `json:"attachment"`
}

// hookTimes reads tacit's hook runs in a week from the transcripts and their subagents' (Claude Code records each
// hook's command, output and time): the run times by hook, and the calls inside subagents. A transcript that cannot
// be read is skipped; one last written before the week holds none of it.
func hookTimes(paths []string, w Week) (map[string]Latency, *Count) {
	times := map[string][]int64{}
	sub := &Count{}
	var files []string
	for _, p := range paths {
		files = append(files, p)
		if m, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(p, ".jsonl"), "subagents", "*.jsonl")); len(m) > 0 {
			files = append(files, m...)
		}
	}
	for _, p := range files {
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.ModTime().Before(w.Start) {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		eachLine(f, 4<<20, func(line []byte) {
			if !bytes.Contains(line, []byte(`"hook_success"`)) || !bytes.Contains(line, []byte("tacit")) {
				return
			}
			var r hookRecord
			if json.Unmarshal(line, &r) != nil || r.Attachment == nil || r.Attachment.Type != "hook_success" {
				return
			}
			hook := tacitHook(r.Attachment.Command)
			at, err := time.Parse(time.RFC3339Nano, r.Timestamp)
			if hook == "" || err != nil || at.Before(w.Start) || !at.Before(w.End) {
				return
			}
			if r.Attachment.DurationMs != nil {
				times[hook] = append(times[hook], *r.Attachment.DurationMs)
			}
			if r.Sidechain {
				sub.Calls++
				if strings.TrimSpace(r.Attachment.Content) != "" {
					sub.Delivered++
				}
			}
		})
		f.Close()
	}
	out := map[string]Latency{}
	for h, ts := range times {
		slices.Sort(ts)
		out[h] = Latency{N: len(ts), P50: quantile(ts, 0.5), P90: quantile(ts, 0.9)}
	}
	return out, sub
}

// tacitHook is the hook a command runs when it is tacit's (`.../tacit hook NAME ...`), else "".
func tacitHook(command string) string {
	w := strings.Fields(command)
	for i := 0; i+2 < len(w); i++ {
		if (filepath.Base(w[i]) == "tacit" || filepath.Base(w[i]) == "tacit.exe") && w[i+1] == "hook" {
			return w[i+2]
		}
	}
	return ""
}

// quantile is the nearest-rank quantile of sorted values.
func quantile(sorted []int64, q float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q*float64(len(sorted))+0.999999) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}
