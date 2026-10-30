// Package saved keeps trackers' checkpoints in memory the way momentum's store keeps them between runs: each
// state copied, each log entry by entry through Log.Unsaved and Log.Saved. Tests use it in place of a store.
package saved

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"github.com/agent-sh/momentum/sessions"
)

type kept struct {
	state []byte
	base  int
	log   map[int]sessions.Entry
}

// Mem is an in-memory checkpoint store.
type Mem struct {
	cps   map[string]*kept
	Loads int // checkpoints handed to a Source
}

// New returns an empty store.
func New() *Mem { return &Mem{cps: map[string]*kept{}} }

// Checkpoint returns what Save kept for a tracker, or nil.
func (m *Mem) Checkpoint(_ context.Context, tool, key string) (*sessions.Checkpoint, error) {
	k := m.cps[tool+"\x00"+key]
	if k == nil {
		return nil, nil
	}
	seqs := make([]int, 0, len(k.log))
	for seq := range k.log {
		seqs = append(seqs, seq)
	}
	sort.Ints(seqs)
	items := make([]sessions.Entry, 0, len(seqs))
	for _, seq := range seqs {
		items = append(items, k.log[seq])
	}
	l := sessions.Kept(k.base, items)
	m.Loads++
	return &sessions.Checkpoint{Tool: tool, Key: key, State: bytes.Clone(k.state), Log: &l}, nil
}

// Save keeps checkpoints as a store does at the end of a run.
func (m *Mem) Save(cps []sessions.Checkpoint) {
	for _, cp := range cps {
		id := cp.Tool + "\x00" + cp.Key
		k := m.cps[id]
		if k == nil {
			k = &kept{log: map[int]sessions.Entry{}}
			m.cps[id] = k
		}
		k.state = bytes.Clone(cp.State)
		from, items := cp.Log.Unsaved()
		k.base = cp.Log.Base()
		for seq := range k.log {
			if seq < k.base || seq >= from {
				delete(k.log, seq)
			}
		}
		for i, e := range items {
			k.log[from+i] = e
		}
		cp.Log.Saved()
	}
}

// Same reports how two runs differ in what they derived: every reported field of every session, its size and its
// whole log. It returns "" when they derived the same. Both lists are sorted by key first.
func Same(a, b []*sessions.Session) string {
	if len(a) != len(b) {
		return fmt.Sprintf("%d sessions against %d", len(a), len(b))
	}
	byKey := func(ss []*sessions.Session) {
		sort.Slice(ss, func(i, j int) bool { return ss[i].Key < ss[j].Key })
	}
	byKey(a)
	byKey(b)
	for i := range a {
		x, y := a[i], b[i]
		xj, _ := json.Marshal(x)
		yj, _ := json.Marshal(y)
		if !bytes.Equal(xj, yj) || x.Size != y.Size {
			return fmt.Sprintf("%s differs:\n%s\n%s", y.Key, xj, yj)
		}
		xi, xgap := x.Log.Since(0)
		yi, ygap := y.Log.Since(0)
		if x.Log.Seq() != y.Log.Seq() || x.Log.Base() != y.Log.Base() || xgap != ygap || !reflect.DeepEqual(xi, yi) {
			return fmt.Sprintf("%s: the logs differ: seq %d/%d, base %d/%d, %d/%d entries", y.Key, x.Log.Seq(), y.Log.Seq(),
				x.Log.Base(), y.Log.Base(), len(xi), len(yi))
		}
	}
	return ""
}
