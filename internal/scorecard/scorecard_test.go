package scorecard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// The fixtures are synthetic: a week in 2030, made-up sessions, facts and messages.

var week = Week{Name: "2030-W11", Start: time.Date(2030, 3, 10, 0, 0, 0, 0, time.UTC), End: time.Date(2030, 3, 17, 0, 0, 0, 0, time.UTC)}

func at(day, hour int) time.Time {
	return week.Start.AddDate(0, 0, day).Add(time.Duration(hour) * time.Hour)
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "momentum.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func put(t *testing.T, st *store.Store, evs ...store.Event) {
	t.Helper()
	for i := range evs {
		if evs[i].Source == "" {
			evs[i].Source = store.Transcripts
		}
		if evs[i].SourceKey == "" {
			evs[i].SourceKey = fmt.Sprintf("k%d-%d", evs[i].TS.UnixNano(), i)
		}
	}
	if _, err := st.Put(context.Background(), evs); err != nil {
		t.Fatal(err)
	}
}

func owner(session, text string, ts time.Time) store.Event {
	return store.Event{TS: ts, Session: session, Harness: "claude", Actor: store.ActorOwner, Kind: "owner_text", Summary: text}
}

// writeLog writes delivery log entries into their UTC days' files.
func writeLog(t *testing.T, dir string, entries ...map[string]any) {
	t.Helper()
	for _, e := range entries {
		e["v"] = 1
		ts := e["ts"].(time.Time)
		e["ts"] = ts.UTC().Format(time.RFC3339Nano)
		b, _ := json.Marshal(e)
		f, err := os.OpenFile(filepath.Join(dir, ts.UTC().Format("2006-01-02")+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(append(b, '\n'))
		f.Close()
	}
}

func block(id, hook, session string, ts time.Time, delivered ...string) map[string]any {
	var cands []map[string]any
	for _, n := range delivered {
		cands = append(cands, map[string]any{"name": n, "prov": "agent", "delivered": true})
	}
	cands = append(cands, map[string]any{"name": "never-shown", "delivered": false})
	return map[string]any{"id": id, "ts": ts, "hook": hook, "session": session, "cands": cands, "nothing": len(delivered) == 0}
}

func writeFact(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWeeksRunSundayToSundayAndAreNamedByTheirMonday(t *testing.T) {
	w := WeekOf(time.Date(2030, 3, 16, 23, 0, 0, 0, time.UTC)) // a Saturday
	if w.Name != "2030-W11" || !w.Start.Equal(week.Start) || !w.End.Equal(week.End) {
		t.Fatalf("WeekOf: %+v", w)
	}
	if got := LastComplete(time.Date(2030, 3, 17, 8, 0, 0, 0, time.UTC)); got.Name != "2030-W11" {
		t.Fatalf("on the Sunday after, the last complete week is %s", got.Name)
	}
	for _, name := range []string{"2030-W11", "2026-W47", "2027-W01", "2026-W53"} {
		got, err := ParseWeek(name, time.UTC)
		if err != nil || got.Name != name || got.Start.Weekday() != time.Sunday || got.Prev(1).Next().Name != name {
			t.Fatalf("ParseWeek(%s): %+v %v", name, got, err)
		}
	}
	if _, err := ParseWeek("2030-W60", time.UTC); err == nil {
		t.Fatal("a week that does not exist parsed")
	}
}

// The tacit side: hook blocks by hook and by session kind, delivered lines and facts, self-mark asks and marks
// (the interactive ones apart), judge labels, nav steps, MCP calls and hook run times from the transcripts.
func TestTacitWeekCountsTheDeliveryLog(t *testing.T) {
	st := testStore(t)
	logDir, tacitStore := t.TempDir(), t.TempDir()
	put(t, st, owner("s-owner", "start the run", at(0, 9)),
		store.Event{TS: at(0, 9), Session: "s-batch", Harness: "claude", Actor: store.ActorAgent, Kind: "agent_text", Summary: "x"},
		store.Event{TS: at(1, 9), Session: "s-owner", Harness: "claude", Actor: store.ActorAgent, Kind: "tool_call", Tool: "mcp__tacit__tacit_search"},
		store.Event{TS: at(1, 9), Session: "s-owner", Agent: "a1", Harness: "claude", Actor: store.ActorAgent, Kind: "tool_call", Tool: "mcp__tacit__tacit_fact"},
		store.Event{TS: at(1, 9), Session: "s-batch", Harness: "claude", Actor: store.ActorAgent, Kind: "tool_call", Tool: "mcp__other__tacit_like"})
	transcript := filepath.Join(t.TempDir(), "s-owner.jsonl")
	asked := block("b-asked", "prompt", "s-owner", at(1, 10), "fact-a", "fact-b")
	asked["mark_asked"], asked["transcript_path"] = true, transcript
	writeLog(t, logDir,
		asked,
		block("b2", "push", "s-owner", at(1, 11)),
		block("b3", "push", "s-batch", at(1, 12), "fact-a"),
		block("b4", "start", "s-ghost", at(2, 1)),
		map[string]any{"id": "m1", "ts": at(1, 13), "hook": "mark", "mark": map[string]any{"block": "b-asked", "helped": []string{"fact-a"}, "not_helped": []string{"fact-b"}}},
		map[string]any{"id": "m2", "ts": at(1, 14), "hook": "mark", "mark": map[string]any{"block": "b-asked", "helped": []string{"fact-b"}}},
		map[string]any{"id": "j1", "ts": at(2, 3), "hook": "judge", "judge": map[string]any{"block": "b3", "fact": "fact-a", "relevant": "yes", "novel": "yes"}},
		map[string]any{"id": "j2", "ts": at(2, 3), "hook": "judge", "judge": map[string]any{"block": "b3", "fact": "fact-b", "relevant": "no", "novel": "no"}},
		map[string]any{"id": "n1", "ts": at(2, 4), "hook": "nav", "session": "s-owner", "nav": map[string]any{"tool": "search"}},
		block("b-last-week", "prompt", "s-owner", at(-1, 0), "fact-c"))
	hook := func(ts time.Time, side bool, cmd string, ms int, content string) string {
		b, _ := json.Marshal(map[string]any{"type": "attachment", "timestamp": ts.Format(time.RFC3339Nano), "isSidechain": side,
			"attachment": map[string]any{"type": "hook_success", "command": cmd, "durationMs": ms, "content": content}})
		return string(b)
	}
	os.WriteFile(transcript, []byte(strings.Join([]string{
		hook(at(1, 10), false, "/x/bin/tacit hook prompt --mcp", 100, "# tacit: facts"),
		hook(at(1, 10), false, "/x/bin/tacit hook prompt --mcp", 300, ""),
		hook(at(1, 10), false, "/x/bin/other hook prompt", 999, ""),
		hook(at(-1, 0), false, "/x/bin/tacit hook prompt --mcp", 5000, ""),
	}, "\n")+"\n"), 0o600)
	sub := filepath.Join(strings.TrimSuffix(transcript, ".jsonl"), "subagents")
	os.MkdirAll(sub, 0o700)
	os.WriteFile(filepath.Join(sub, "agent-1.jsonl"), []byte(hook(at(1, 11), true, "tacit hook pre-tool", 50, "# tacit: x")+"\n"), 0o600)
	for _, p := range []string{transcript, filepath.Join(sub, "agent-1.jsonl")} {
		os.Chtimes(p, at(2, 0), at(2, 0)) // written in the week
	}

	o, err := Observe(context.Background(), Inputs{Store: st, DeliveryDir: logDir, TacitStore: tacitStore}, week)
	if err != nil {
		t.Fatal(err)
	}
	tw := o.Tacit
	if tw.ByHook["prompt"] != (Count{1, 1}) || tw.ByHook["push"] != (Count{2, 1}) || tw.ByHook["start"] != (Count{1, 0}) {
		t.Errorf("by hook: %+v", tw.ByHook)
	}
	if tw.ByKind[Interactive] != (Count{2, 1}) || tw.ByKind[Headless] != (Count{1, 1}) || tw.ByKind[Unknown] != (Count{1, 0}) {
		t.Errorf("by kind: %+v", tw.ByKind)
	}
	if tw.Lines != 3 || tw.Facts != 2 {
		t.Errorf("lines %d, facts %d", tw.Lines, tw.Facts)
	}
	if tw.Marks != (Marks{Asked: 1, Answered: 1, Used: 1, NotRelevant: 1}) || tw.InteractiveMarks != tw.Marks {
		t.Errorf("marks %+v, interactive %+v (one answer a block)", tw.Marks, tw.InteractiveMarks)
	}
	if tw.Judge.Labeled != 2 || tw.Judge.Relevant["no"] != 1 || tw.Judge.RelevantNew != 1 {
		t.Errorf("judge: %+v", tw.Judge)
	}
	if tw.Nav["search"] != 1 || tw.MCP["tacit_search"] != 1 || tw.MCP["tacit_fact"] != 1 || len(tw.MCP) != 2 ||
		tw.MCPByKind[Interactive] != 1 || tw.MCPByKind[Subagent] != 1 {
		t.Errorf("nav %v, mcp %v by kind %v", tw.Nav, tw.MCP, tw.MCPByKind)
	}
	if l := tw.Latency["prompt"]; l.N != 2 || l.P50 != 100 || l.P90 != 300 || tw.Subagent == nil || *tw.Subagent != (Count{1, 1}) || tw.Latency["pre-tool"].N != 1 {
		t.Errorf("latency %+v, subagent %+v", tw.Latency, tw.Subagent)
	}
	if len(o.TacitBefore) != BaselineWeeks || o.TacitBefore[0].ByHook["prompt"] != (Count{1, 1}) {
		t.Errorf("the week before: %+v", o.TacitBefore)
	}
	if !slices.ContainsFunc(o.Worst, func(f Fact) bool { return f.Name == "fact-b" }) {
		t.Errorf("worst sample: %+v", o.Worst)
	}
}

const factBody = `---
name: %s
description: %s
type: preference
created: "%s"
source:
  kind: observed
  owner_words: %s
  owner_words_from: %s
---

body
`

// A restatement is an owner message that covers most of an older curated fact's words. A fact newer than the
// message, one in the inbox, one made from the same session, a copy of the text in another session, a paste and a
// message that shares only a little do not count. Whether the session was given the fact first is read from the log.
func TestRestatementsMatchOlderCuratedFactsOnly(t *testing.T) {
	st := testStore(t)
	logDir, facts := t.TempDir(), t.TempDir()
	writeFact(t, facts, "user/zebra.md", fmt.Sprintf(factBody, "zebra-cache-on-quartz-disk", "Keep the zebra build cache on the quartz disk, never on tmpfs", "2030-01-02", "zebra cache lives on quartz", "codex:old-session"))
	writeFact(t, facts, "inbox/agent/walrus.md", fmt.Sprintf(factBody, "walrus-lanterns-flicker-hourly", "The walrus lanterns flicker hourly near the harbor gate", "2030-01-02", "walrus lanterns flicker", "x"))
	writeFact(t, facts, "user/new.md", fmt.Sprintf(factBody, "pelican-ledger-closes-friday", "The pelican ledger closes every friday at noon sharp", "2030-03-13", "pelican ledger closes friday", "x"))
	writeFact(t, facts, "user/mine.md", fmt.Sprintf(factBody, "otter-routers-need-reboots", "Otter routers need weekly reboots after firmware pushes", "2030-01-02", "otter routers need reboots", "claude:s-self"))
	writeFact(t, facts, "user/old.md", "---\nname: heron-kiln-fires-at-dawn\ndescription: The heron kiln fires at dawn every single day\ncreated: \"2030-01-02\"\nretired: 2030-02-01\n---\n")
	for i := 0; i < 20; i++ { // filler so the distinctive words stay rare
		writeFact(t, facts, fmt.Sprintf("projects/p/f%d.md", i), fmt.Sprintf(factBody, fmt.Sprintf("filler-%d", i), fmt.Sprintf("generic filler item number %d about builds and disks", i), "2030-01-01", "", ""))
	}
	writeLog(t, logDir, block("b1", "prompt", "s-seen", at(1, 8), "zebra-cache-on-quartz-disk"))
	put(t, st,
		owner("s-seen", "again: the zebra build cache stays on the quartz disk, not tmpfs!", at(1, 9)),
		owner("s-fresh", "the zebra cache goes on the quartz disk, keep build output off tmpfs", at(2, 9)),
		owner("s-copy1", "keep the zebra build cache on the quartz disk never tmpfs please", at(3, 9)),
		owner("s-copy2", "keep the zebra build cache on the quartz disk never tmpfs please", at(3, 10)),
		owner("s-x", "the walrus lanterns flicker hourly near the harbor gate", at(3, 11)),
		owner("s-x", "the pelican ledger closes every friday at noon sharp", at(3, 12)),
		owner("s-self", "otter routers need weekly reboots after firmware pushes", at(3, 13)),
		owner("s-x", "- zebra build cache on the quartz disk, never on tmpfs", at(3, 14)),
		owner("s-x", "the zebra thing again", at(3, 15)),
		owner("s-x", "the heron kiln fires at dawn every single day", at(3, 16)),
		owner("s-old", "an older word", week.Prev(1).Start))
	o, err := Observe(context.Background(), Inputs{Store: st, DeliveryDir: logDir, TacitStore: facts}, week)
	if err != nil {
		t.Fatal(err)
	}
	tw := o.Tacit
	if tw.Restatements != 2 || tw.RestatedSeen != 1 {
		t.Fatalf("restatements %d, seen first %d: %+v", tw.Restatements, tw.RestatedSeen, tw.Restated)
	}
	if r := tw.Restated[0]; r.Session != "s-seen" || !r.Delivered || r.Fact != "zebra-cache-on-quartz-disk" || tw.Restated[1].Session != "s-fresh" || tw.Restated[1].Delivered {
		t.Fatalf("restated: %+v", tw.Restated)
	}
	if !tw.OwnerCovered || !o.TacitBefore[0].OwnerCovered || o.TacitBefore[1].OwnerCovered {
		t.Fatalf("owner coverage: %v, %v, %v (the store's first owner message is in the week before)", tw.OwnerCovered, o.TacitBefore[0].OwnerCovered, o.TacitBefore[1].OwnerCovered)
	}
}

func TestRulesAsCoded(t *testing.T) {
	base := func(ns ...int) []TacitWeek {
		var out []TacitWeek
		for _, n := range ns {
			out = append(out, TacitWeek{Restatements: n, OwnerCovered: true})
		}
		return out
	}
	for _, c := range []struct {
		name   string
		cur    TacitWeek
		before []TacitWeek
		want   bool
	}{
		{"used beats noise, restatements at the median", TacitWeek{InteractiveMarks: Marks{Used: 2, NotRelevant: 2}, Restatements: 2}, base(1, 2, 3, 9), true},
		{"no used mark", TacitWeek{InteractiveMarks: Marks{Used: 0, NotRelevant: 0}}, base(0, 0, 0, 0), false},
		{"noise beats used", TacitWeek{InteractiveMarks: Marks{Used: 1, NotRelevant: 2}}, base(0, 0, 0, 0), false},
		{"restatements above the median", TacitWeek{InteractiveMarks: Marks{Used: 1}, Restatements: 3}, base(1, 2, 3, 9), false},
		{"uncovered weeks are no baseline", TacitWeek{InteractiveMarks: Marks{Used: 1}, Restatements: 3}, []TacitWeek{{Restatements: 0}}, true},
	} {
		if got, why := JudgeTacit(c.cur, c.before); got != c.want {
			t.Errorf("%s: %v (%s)", c.name, got, why)
		}
	}
	if ok, _ := JudgeMomentum(MomentumWeek{Improved: 1}); !ok {
		t.Error("one improved case is the floor")
	}
	if ok, _ := JudgeMomentum(MomentumWeek{Landed: 3, SpendUSD: 50}); ok {
		t.Error("landing and spending without an improved case does not deliver")
	}
}

// fakeSystemctl records its calls and lists the given timers.
type fakeSystemctl struct {
	calls  [][]string
	timers map[string][]string
}

func (f *fakeSystemctl) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if args[0] == "list-unit-files" {
		var out []string
		for _, pat := range args[4:] {
			for _, u := range f.timers[pat] {
				state := "enabled"
				if strings.HasPrefix(u, "off:") {
					u, state = strings.TrimPrefix(u, "off:"), "disabled"
				}
				out = append(out, u+" "+state+" enabled")
			}
		}
		return []byte(strings.Join(out, "\n")), nil
	}
	return nil, nil
}

// Scoring counts from the first run that is not dry, and its first week is the first full week after it. Two
// failing scored weeks in a row shut a system down: its off-file says why and how to turn it back on, and its
// spending timers stop. A system turned back on starts over. A dry run writes and shuts nothing.
func TestTwoFailingScoredWeeksShutASystemDown(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cfg := t.TempDir()
	fs := &fakeSystemctl{timers: map[string][]string{"tacit-dream*.timer": {"tacit-dream.timer", "tacit-dream-idle.timer"},
		"tacit-curate*.timer": {"tacit-curate.timer", "off:tacit-curate-extra.timer"}, "momentum-loop*.timer": {"momentum-loop.timer"}}}
	sw := Switch{TacitOff: filepath.Join(cfg, "tacit", "off"), MomentumOff: filepath.Join(cfg, "momentum", "off"), Systemctl: fs.run}
	w0 := week.Prev(1)
	obs := func(w Week, improved int) *Observation {
		return &Observation{Week: w, Momentum: MomentumWeek{Week: w.Name, Improved: improved},
			Tacit: TacitWeek{Week: w.Name, ByHook: map[string]Count{"prompt": {Calls: 3}}}}
	}
	deploy := w0.Start.Add(time.Hour) // mid-week: that week is not scored
	if sc, err := Score(ctx, st, sw, obs(w0, 0), true, deploy); err != nil || len(sc.Shutdowns) != 0 || sc.Verdicts[0].Scored {
		t.Fatalf("dry before deployment: %+v %v", sc, err)
	}
	if _, _, ok, _ := st.ScorecardValue(ctx, keyDeployed); ok {
		t.Fatal("a dry run recorded the deployment")
	}
	if sc, err := Score(ctx, st, sw, obs(w0, 0), false, deploy); err != nil || sc.Verdicts[0].Scored || sc.Verdicts[1].Scored {
		t.Fatalf("the deployment week is not scored: %+v %v", sc, err)
	}
	sc, err := Score(ctx, st, sw, obs(week, 1), false, week.End.Add(8*time.Hour))
	if err != nil || !sc.Verdicts[0].Scored || sc.Verdicts[0].Delivers || !sc.Verdicts[1].Delivers || len(sc.Shutdowns) != 0 || sc.Standings[0].ToShutdown != 1 {
		t.Fatalf("first scored week: %+v %v", sc, err)
	}
	next := week.Next()
	if sc, err := Score(ctx, st, sw, obs(next, 1), true, next.End.Add(8*time.Hour)); err != nil || len(sc.Shutdowns) != 1 || sw.Off(Tacit) {
		t.Fatalf("a dry second failing week says it would shut tacit, and shuts nothing: %+v %v", sc, err)
	}
	sc, err = Score(ctx, st, sw, obs(next, 1), false, next.End.Add(8*time.Hour))
	if err != nil || len(sc.Shutdowns) != 1 || sc.Shutdowns[0].System != Tacit {
		t.Fatalf("second failing week: %+v %v", sc, err)
	}
	b, err := os.ReadFile(sw.TacitOff)
	if err != nil || !strings.Contains(string(b), "why: 2 scored weeks in a row") || !strings.Contains(string(b), "week: "+next.Name) ||
		!strings.Contains(string(b), "rm "+sw.TacitOff+" && systemctl --user enable --now tacit-dream.timer tacit-dream-idle.timer tacit-curate.timer") {
		t.Fatalf("off-file: %q %v", b, err)
	}
	var disabled []string
	for _, c := range fs.calls {
		if c[0] == "disable" {
			disabled = append(disabled, c[2])
		}
	}
	if !slices.Equal(disabled, []string{"tacit-dream.timer", "tacit-dream-idle.timer", "tacit-curate.timer"}) || sw.Off(Momentum) {
		t.Fatalf("disabled %v; momentum off %v", disabled, sw.Off(Momentum))
	}
	s, err := Stand(ctx, st, sw, Tacit)
	if err != nil || s.On || s.Shut != next.Name || s.Failing != 2 {
		t.Fatalf("standing after the shutdown: %+v %v", s, err)
	}
	// the same week run again after the shutdown keeps its scored verdict
	if sc, err := Score(ctx, st, sw, obs(next, 1), false, next.End.Add(9*time.Hour)); err != nil || !sc.Verdicts[0].Scored || sc.Verdicts[0].Delivers || len(sc.Shutdowns) != 0 {
		t.Fatalf("a re-run of the shutdown week: %+v %v", sc, err)
	}
	// while it is off, a week is not scored and nothing is shut again
	third := next.Next()
	if sc, err := Score(ctx, st, sw, obs(third, 1), false, third.End.Add(8*time.Hour)); err != nil || len(sc.Shutdowns) != 0 || sc.Verdicts[0].Scored {
		t.Fatalf("off week: %+v %v", sc, err)
	}
	// turned back on: scoring starts over with the next full week
	os.Remove(sw.TacitOff)
	fourth := third.Next()
	sc, err = Score(ctx, st, sw, obs(fourth, 1), false, fourth.End.Add(8*time.Hour))
	if err != nil || !slices.Equal(sc.Reenabled, []string{Tacit}) || sc.Verdicts[0].Scored || len(sc.Shutdowns) != 0 {
		t.Fatalf("turned back on: %+v %v", sc, err)
	}
}

// A week whose log holds no hook block, or does not reach its start, is no evidence: it is not scored, so a log that
// is off or moved never shuts tacit down. tacit's default off-file counts as off too.
func TestAWeekWithNoRecordsIsNotScored(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cfg := t.TempDir()
	sw := Switch{TacitOff: filepath.Join(cfg, "x", "tacit", "off"), TacitDefaultOff: filepath.Join(cfg, "home", "tacit", "off"),
		MomentumOff: filepath.Join(cfg, "momentum", "off"), Systemctl: (&fakeSystemctl{}).run}
	if _, err := Score(ctx, st, sw, &Observation{Week: week.Prev(1)}, false, week.Prev(1).Start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, tw := range []TacitWeek{{Week: week.Name}, {Week: week.Name, Partial: true, ByHook: map[string]Count{"prompt": {Calls: 9}}}} {
		sc, err := Score(ctx, st, sw, &Observation{Week: week, Tacit: tw}, true, week.End.Add(time.Hour))
		if err != nil || sc.Verdicts[0].Scored || !strings.Contains(sc.Verdicts[0].Why, "holds no hook block") || !sc.Verdicts[1].Scored {
			t.Fatalf("%+v: %+v %v", tw, sc.Verdicts, err)
		}
	}
	os.MkdirAll(filepath.Dir(sw.TacitDefaultOff), 0o755)
	os.WriteFile(sw.TacitDefaultOff, []byte("off\n"), 0o644)
	if !sw.Off(Tacit) {
		t.Fatal("tacit's default off-file does not read as off")
	}
}

func TestPastedTextIsNoRestatement(t *testing.T) {
	for text, want := range map[string]bool{"- a list item": true, "│ box │": true, "https://example.com/x": true, "12 34 56 78 90": true,
		"keep the cache on the quartz disk": false} {
		if pasted(text) != want {
			t.Errorf("pasted(%q) = %v", text, !want)
		}
	}
}
