package scorecard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

// Inputs are where the observation reads.
type Inputs struct {
	Store       *store.Store
	DeliveryDir string // tacit's delivery log
	TacitStore  string // tacit's store, whose curated facts the owner's restatements are matched against
}

// DefaultDeliveryDir is tacit's delivery log: $TACIT_STATE_DIR/delivery, default ~/.local/state/tacit/delivery.
func DefaultDeliveryDir() string {
	if d := os.Getenv("TACIT_STATE_DIR"); d != "" {
		return filepath.Join(filepath.Clean(d), "delivery")
	}
	return filepath.Join(paths.Home(), ".local", "state", "tacit", "delivery")
}

// Observation is a week of both systems' numbers, with the weeks before it that the rules compare with.
type Observation struct {
	Week           Week
	Tacit          TacitWeek
	TacitBefore    []TacitWeek // the newest first
	Momentum       MomentumWeek
	MomentumBefore []MomentumWeek
	Facts          int // the curated facts matched against
	Skipped        int // delivery log lines that could not be read
	// Top and Worst are the week's most delivered facts and the ones marked or labeled not relevant most, by name,
	// with their text when the store holds them: the model pass's sample.
	Top, Worst []Fact
}

// Observe computes a week's numbers and those of the BaselineWeeks before it.
func Observe(ctx context.Context, in Inputs, w Week) (*Observation, error) {
	from := w.Prev(BaselineWeeks)
	log, err := ReadLog(in.DeliveryDir, from.Start, w.End)
	if errors.Is(err, os.ErrNotExist) {
		log, err = &Log{}, nil // no log: tacit delivered nothing that it recorded
	}
	if err != nil {
		return nil, fmt.Errorf("tacit's delivery log: %w", err)
	}
	facts, err := LoadFacts(in.TacitStore)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("tacit's store: %w", err)
	}
	m := NewMatcher(facts)
	kind, err := classifier(ctx, in.Store, from.Start, w.End)
	if err != nil {
		return nil, err
	}
	firstOwner, err := in.Store.FirstOf(ctx, "owner_text")
	if err != nil {
		return nil, err
	}
	owner, err := ownerMessages(ctx, in.Store, from.Start.Add(-ownerHistory), w.End)
	if err != nil {
		return nil, err
	}
	o := &Observation{Week: w, Facts: m.Facts(), Skipped: log.Skipped}
	for i := 0; i <= BaselineWeeks; i++ {
		wk := w.Prev(i)
		t := log.tacitWeek(wk, kind)
		t.OwnerCovered = !firstOwner.IsZero() && !firstOwner.After(wk.Start)
		if err := mcpCalls(ctx, in.Store, wk, kind, &t); err != nil {
			return nil, err
		}
		restatements(m, log, owner, wk, &t)
		mw, err := momentumWeek(ctx, in.Store, wk)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			t.Latency, t.Subagent = hookTimes(log.transcripts(wk), wk)
			o.Tacit, o.Momentum = t, mw
			byName := map[string]Fact{}
			for _, f := range facts {
				byName[f.Name] = f
			}
			for _, list := range []struct {
				names []string
				out   *[]Fact
			}{{t.top, &o.Top}, {t.worst, &o.Worst}} {
				for _, n := range list.names {
					f, ok := byName[n]
					if !ok {
						f = Fact{Name: n}
					}
					*list.out = append(*list.out, f)
				}
			}
			continue
		}
		o.TacitBefore = append(o.TacitBefore, t)
		o.MomentumBefore = append(o.MomentumBefore, mw)
	}
	return o, nil
}

// classifier names each session's kind: interactive when the owner wrote in it over the span, headless when
// momentum's store holds it otherwise, unknown when it holds nothing of it.
func classifier(ctx context.Context, st *store.Store, since, until time.Time) (Classifier, error) {
	owner, err := st.OwnerSessions(ctx, since, until)
	if err != nil {
		return nil, err
	}
	kinds := map[string]string{"": Unknown}
	for _, s := range owner {
		kinds[s] = Interactive
	}
	return func(session string) string {
		if k, ok := kinds[session]; ok {
			return k
		}
		k := Unknown
		if has, err := st.HasSession(ctx, session); err == nil && has {
			k = Headless
		}
		kinds[session] = k
		return k
	}, nil
}

// mcpTools are tacit's MCP tools, under whatever server name the harness gave tacit.
var mcpTools = []string{"tacit_search", "tacit_fact", "tacit_tree", "tacit_add", "tacit_mark", "tacit_episode"}

// mcpCalls counts tacit's MCP tool calls in the week's transcripts, by tool and by session kind.
func mcpCalls(ctx context.Context, st *store.Store, w Week, kind Classifier, t *TacitWeek) error {
	evs, err := st.ToolCallsLike(ctx, "mcp%tacit%", w.Start, w.End)
	if err != nil {
		return err
	}
	for _, e := range evs {
		i := strings.LastIndex(e.Tool, "__")
		if i < 0 || !slices.Contains(mcpTools, e.Tool[i+2:]) {
			continue
		}
		t.MCP[e.Tool[i+2:]]++
		k := kind(e.Session)
		if e.Agent != "" {
			k = Subagent
		}
		t.MCPByKind[k]++
	}
	return nil
}

// ownerHistory is how far before the observed weeks the owner's earlier messages are read: a message whose text
// came before is a copy (a forked thread, a relayed broadcast), not a restatement.
const ownerHistory = 8 * 7 * 24 * time.Hour

// restatements finds the week's owner messages that restate a curated fact, and whether the session was given the
// fact before. A message counts once, in the first session that holds its text, and only when no earlier message
// and no other session holds the same text: the owner does not type the same message twice word for word, a forked
// thread or an agent relaying a broadcast does.
func restatements(m *Matcher, log *Log, owner []store.Event, w Week, t *TacitWeek) {
	first := map[string]time.Time{}
	sessions := map[string]map[string]bool{}
	for _, e := range owner { // oldest first
		k := normal(e.Summary)
		if _, ok := first[k]; !ok {
			first[k] = e.TS
		}
		if !e.TS.Before(w.Start) && e.TS.Before(w.End) {
			if sessions[k] == nil {
				sessions[k] = map[string]bool{}
			}
			sessions[k][e.Session] = true
		}
	}
	seen := map[string]bool{}
	for _, e := range owner {
		if e.TS.Before(w.Start) || !e.TS.Before(w.End) {
			continue
		}
		k := normal(e.Summary)
		if len(sessions[k]) > 1 || first[k].Before(e.TS.Add(-time.Minute)) || seen[k] {
			continue
		}
		seen[k] = true
		f, cover, ok := m.Match(e.Summary, e.Session, e.TS)
		if !ok {
			continue
		}
		r := Restatement{At: e.TS, Session: e.Session, Fact: f.Name, Score: float64(int(cover*100)) / 100,
			Delivered: log.deliveredBefore(e.Session, f.Name, e.TS), Text: e.Summary, FactText: f.Text}
		t.Restatements++
		if r.Delivered {
			t.RestatedSeen++
		}
		t.Restated = append(t.Restated, r)
	}
}

// ownerMessages are the owner's own messages in a span, the oldest first.
func ownerMessages(ctx context.Context, st *store.Store, since, until time.Time) ([]store.Event, error) {
	evs, err := st.Events(ctx, store.Query{Kinds: []string{"owner_text"}, Actor: store.ActorOwner, Since: since, Until: until, Main: true, Limit: allEvents})
	slices.Reverse(evs)
	return evs, err
}

// normal is a message's text with its case and spacing set aside.
func normal(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }
