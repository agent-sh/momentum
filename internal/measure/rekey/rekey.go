// Package rekey compares what two builds of the detectors key over the same stored history: which events a change
// keys by another signature (re-keyed), which it stops keying (dropped) and which it starts keying (added). The loop
// reads a change to its own detectors by it before the change may land (SPEC.md, Authority). It is part of the
// loop's measurement, which no change of the loop's touches.
package rekey

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"
)

// Line is one keyed event as a replay writes it: the event's id and the signature a build keys it by.
type Line struct {
	ID        string `json:"id"`
	Signature string `json:"sig"`
}

// Header opens a replay's output: the window it keyed.
type Header struct {
	Replay int       `json:"replay"` // the format, Format
	Since  time.Time `json:"since"`
	Until  time.Time `json:"until"`
}

// Format is the replay output's format.
const Format = 1

// MaxOutput bounds a replay's output: at 64 bytes of overhead and a 300-byte signature an event, about 180,000 keyed
// events, several times a week's failures.
const MaxOutput = 64 << 20

// Write writes a replay's output: its header, then one line per keyed event.
func Write(w io.Writer, h Header, lines []Line) error {
	h.Replay = Format
	enc := json.NewEncoder(w)
	if err := enc.Encode(h); err != nil {
		return err
	}
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			return err
		}
	}
	return nil
}

// Read reads a replay's output into its header and each event's signature. An event keyed twice keeps its first
// signature. Output that is not a replay's (no header, another format, a line that does not parse, an empty id) is
// an error: the host reads the output of code it did not write, and takes nothing it cannot read.
func Read(b []byte) (Header, map[string]string, error) {
	var h Header
	if len(b) > MaxOutput {
		return h, nil, fmt.Errorf("the replay's output is over %d MiB", MaxOutput>>20)
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	if !sc.Scan() {
		return h, nil, errors.New("the replay wrote nothing")
	}
	if err := json.Unmarshal(sc.Bytes(), &h); err != nil || h.Replay != Format {
		return h, nil, errors.New("the replay's output has no header of this format")
	}
	keys := map[string]string{}
	for sc.Scan() {
		var l Line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil || l.ID == "" {
			return h, nil, errors.New("a line of the replay's output does not parse")
		}
		if _, ok := keys[l.ID]; !ok {
			keys[l.ID] = l.Signature
		}
	}
	if err := sc.Err(); err != nil {
		return h, nil, err
	}
	return h, keys, nil
}

// Move is a signature some of whose events a change keys by another.
type Move struct {
	From string `json:"from"`
	To   string `json:"to"`
	N    int    `json:"n"`
}

// Count is a signature and a number of its events.
type Count struct {
	Signature string `json:"sig"`
	N         int    `json:"n"`
}

// Diff is what a change does to the keys of the events a replay read.
type Diff struct {
	Before  int     `json:"before"` // events the base keys
	After   int     `json:"after"`  // events the change keys
	Rekeyed []Move  `json:"rekeyed,omitempty"`
	Dropped []Count `json:"dropped,omitempty"` // events the base keys and the change does not, by the base's signature
	Added   []Count `json:"added,omitempty"`   // events the change keys and the base does not, by the change's signature
}

// Compare is the difference between the keys of the base and of a change, over the same events.
func Compare(before, after map[string]string) Diff {
	d := Diff{Before: len(before), After: len(after)}
	moved := map[[2]string]int{}
	dropped, added := map[string]int{}, map[string]int{}
	for id, from := range before {
		to, ok := after[id]
		switch {
		case !ok:
			dropped[from]++
		case to != from:
			moved[[2]string{from, to}]++
		}
	}
	for id, to := range after {
		if _, ok := before[id]; !ok {
			added[to]++
		}
	}
	for k, n := range moved {
		d.Rekeyed = append(d.Rekeyed, Move{From: k[0], To: k[1], N: n})
	}
	sort.Slice(d.Rekeyed, func(i, j int) bool {
		a, b := d.Rekeyed[i], d.Rekeyed[j]
		if a.N != b.N {
			return a.N > b.N
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	})
	d.Dropped, d.Added = counts(dropped), counts(added)
	return d
}

func counts(m map[string]int) []Count {
	var out []Count
	for s, n := range m {
		out = append(out, Count{Signature: s, N: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Signature < out[j].Signature
	})
	return out
}

// Events counts the events of a list.
func Events(cs []Count) int {
	n := 0
	for _, c := range cs {
		n += c.N
	}
	return n
}

// Moved counts the events of a list of moves.
func Moved(ms []Move) int {
	n := 0
	for _, m := range ms {
		n += m.N
	}
	return n
}

// Foreign are the dropped signatures that are not among own: the events a change makes disappear that the case it
// fixes does not hold. Only the case's own investigation says its events are false positives, so these are the
// events a change drops without that evidence.
func (d Diff) Foreign(own []string) []Count {
	mine := map[string]bool{}
	for _, s := range own {
		mine[s] = true
	}
	var out []Count
	for _, c := range d.Dropped {
		if !mine[c.Signature] {
			out = append(out, c)
		}
	}
	return out
}

// ForeignMoves are the re-keys of signatures that are not among own: events of another case's problem that the change
// moves away from that case's signature, which the case would read as its problem gone.
func (d Diff) ForeignMoves(own []string) []Move {
	mine := map[string]bool{}
	for _, s := range own {
		mine[s] = true
	}
	var out []Move
	for _, m := range d.Rekeyed {
		if !mine[m.From] {
			out = append(out, m)
		}
	}
	return out
}

// Kept counts the events the change keys by the same signature as the base, of the signatures given.
func Kept(before, after map[string]string, sigs []string) int {
	mine := map[string]bool{}
	for _, s := range sigs {
		mine[s] = true
	}
	n := 0
	for id, s := range before {
		if mine[s] && after[id] == s {
			n++
		}
	}
	return n
}

// Touches says whether the change re-keys or drops events of any of the signatures: of the case it fixes, its
// problem would no longer be seen under them because the detector keys it otherwise, not because it stopped.
func (d Diff) Touches(sigs []string) (rekeyed, dropped int) {
	mine := map[string]bool{}
	for _, s := range sigs {
		mine[s] = true
	}
	for _, m := range d.Rekeyed {
		if mine[m.From] {
			rekeyed += m.N
		}
	}
	for _, c := range d.Dropped {
		if mine[c.Signature] {
			dropped += c.N
		}
	}
	return rekeyed, dropped
}
