package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/scorecard"
	"github.com/agent-sh/momentum/internal/store"
)

// weeklyFixture is an empty machine: momentum's home and configuration, tacit's log and store, all synthetic.
func weeklyFixture(t *testing.T) (*store.Store, weeklyDeps, *[]string) {
	t.Helper()
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := t.TempDir()
	var prompts []string
	d := weeklyDeps{
		sw: scorecard.Switch{TacitOff: filepath.Join(cfg, "tacit", "off"), MomentumOff: filepath.Join(cfg, "momentum", "off"),
			Systemctl: func(context.Context, ...string) ([]byte, error) { return nil, nil }},
		now:         func() time.Time { return time.Date(2030, 3, 17, 8, 0, 0, 0, time.UTC) },
		deliveryDir: t.TempDir(), tacitStore: t.TempDir(), constraints: []string{"never wake the owner"},
	}
	d.advise = func(_ context.Context, system, prompt, schema string) (json.RawMessage, string, float64, error) {
		prompts = append(prompts, system, prompt)
		return json.RawMessage(`{"recommendations":[],"handled":[]}`), "fixture-model", 0, nil
	}
	return st, d, &prompts
}

// A weekly run keeps the week's report and its note, files each recommendation as a case of family tacit (the same
// change in the same case), drops one that would promote facts into an agent rules file, and closes an open
// recommendation the numbers show handled. A dry run asks no model and writes none of it.
func TestWeeklyKeepsTheReportAndTracksTacitChanges(t *testing.T) {
	ctx := context.Background()
	st, d, prompts := weeklyFixture(t)
	old, _, err := st.ObserveCase(ctx, store.Finding{Family: store.TacitFamily, Signature: "tacit:older-change", Title: "an older change",
		Evidence: []store.Evidence{{Ref: "file:/fixture/2030-W10.md", At: time.Now()}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	report := `{"recommendations":[
	 {"key":"Cut Push Noise!","title":"Gate the push hook harder","change":"raise the push floor","evidence":"push delivers 1 percent","expected":"push share"},
	 {"key":"promote","title":"Promote the top facts","change":"copy them into AGENTS.md","evidence":"x","expected":"y"}],
	 "handled":[{"case":"` + old.ID + `","why":"the numbers moved"},{"case":"c00000000","why":"not one of ours"}]}`
	advise := d.advise
	d.advise = func(ctx context.Context, system, prompt, schema string) (json.RawMessage, string, float64, error) {
		advise(ctx, system, prompt, schema)
		return json.RawMessage(report), "fixture-model", 0.5, nil
	}
	var out, errOut bytes.Buffer
	if err := weekly(ctx, st, d, "", true, false, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if len(*prompts) != 0 || !strings.Contains(out.String(), "# momentum weekly 2030-W11") || !strings.Contains(out.String(), "a dry run asks no model") {
		t.Fatalf("dry: %d prompts, %s", len(*prompts), out.String())
	}
	if _, err := os.Stat(filepath.Join(weeklyDir(), "2030-W11.md")); !os.IsNotExist(err) {
		t.Fatalf("a dry run wrote the report: %v", err)
	}
	if notes, _ := st.Notes(ctx); len(notes) != 0 {
		t.Fatalf("a dry run kept notes: %+v", notes)
	}

	out.Reset()
	if err := weekly(ctx, st, d, "", false, false, &out, &errOut); err != nil {
		t.Fatal(err, errOut.String())
	}
	if len(*prompts) != 2 || !strings.Contains((*prompts)[0], "Never propose promoting facts into AGENTS.md, CLAUDE.md, SHARED.md") ||
		!strings.Contains((*prompts)[0], "never wake the owner") || !strings.Contains((*prompts)[1], old.ID) {
		t.Fatalf("the model pass's input: %q", *prompts)
	}
	cases, err := st.Cases(ctx, store.CaseFilter{Family: store.TacitFamily, All: true})
	if err != nil {
		t.Fatal(err)
	}
	var made store.Case
	for _, c := range cases {
		if slices.Contains(c.Signatures, "tacit:cut-push-noise") {
			made = c
		}
		if slices.Contains(c.Signatures, "tacit:promote") {
			t.Fatalf("a recommendation into an agent rules file was filed: %+v", c)
		}
	}
	if made.ID == "" || made.Status == store.CaseClosed || made.NextTest != "raise the push floor" || made.Benefit != "push share" {
		t.Fatalf("the recommendation's case: %+v (all %+v)", made, cases)
	}
	if c, _ := st.CaseByID(ctx, old.ID); c.Status != store.CaseClosed || c.Reason != store.Solved {
		t.Fatalf("the handled case: %+v", c)
	}
	b, err := os.ReadFile(filepath.Join(weeklyDir(), "2030-W11.md"))
	if err != nil || !strings.Contains(string(b), "Gate the push hook harder** (case "+made.ID+")") ||
		!strings.Contains(string(b), "dropped (it would promote facts into an agent rules file): Promote the top facts") ||
		!strings.Contains(string(b), "closed "+old.ID+" as handled") {
		t.Fatalf("report: %s %v", b, err)
	}
	notes, err := st.Notes(ctx)
	if err != nil || len(notes) != 1 || notes[0].ID != "weekly:2030-W11" || notes[0].Kind != store.NoteDecided || notes[0].Importance != store.ImportanceHigh {
		t.Fatalf("notes: %+v %v", notes, err)
	}
	if code, shown, _ := momentum("weekly", "show"); code != 0 || shown != string(b) {
		t.Fatalf("weekly show: %d %q", code, shown)
	}
	if code, _, errOut := momentum("weekly", "show", "2030-W12"); code == 0 {
		t.Fatalf("show of a week with no report: %s", errOut)
	}
	// the same change next week lands in the same case
	d.now = func() time.Time { return time.Date(2030, 3, 24, 8, 0, 0, 0, time.UTC) }
	if err := weekly(ctx, st, d, "", false, false, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	again, _ := st.Cases(ctx, store.CaseFilter{Family: store.TacitFamily})
	if len(again) != 1 || again[0].ID != made.ID || again[0].Evidence != 2 {
		t.Fatalf("next week: %+v", again)
	}
}

// Two failing weeks in a row shut tacit down: the off-file, a high note that says why and how to turn it back on,
// and the scorecard command shows it off.
func TestWeeklyShutdownLeavesANote(t *testing.T) {
	ctx := context.Background()
	st, d, _ := weeklyFixture(t)
	for _, day := range []int{12, 19} { // a hook block in each scored week: a week with none is not scored
		ts := time.Date(2030, 3, day, 9, 0, 0, 0, time.UTC)
		line := `{"v":1,"id":"b` + ts.Format("02") + `","ts":"` + ts.Format(time.RFC3339) + `","hook":"prompt","session":"s1","cands":[]}` + "\n"
		os.WriteFile(filepath.Join(d.deliveryDir, ts.Format("2006-01-02")+".jsonl"), []byte(line), 0o600)
	}
	os.WriteFile(filepath.Join(d.deliveryDir, "2030-03-01.jsonl"), nil, 0o600) // the log reaches back before the weeks
	var out, errOut bytes.Buffer
	d.now = func() time.Time { return time.Date(2030, 3, 9, 8, 0, 0, 0, time.UTC) } // the deployment, mid-week
	if err := weekly(ctx, st, d, "", false, false, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	for _, day := range []int{17, 24} {
		d.now = func() time.Time { return time.Date(2030, 3, day, 8, 0, 0, 0, time.UTC) }
		if err := weekly(ctx, st, d, "", false, false, &out, &errOut); err != nil {
			t.Fatal(err)
		}
	}
	if !d.sw.Off(scorecard.Tacit) || !d.sw.Off(scorecard.Momentum) {
		t.Fatal("both failed two scored weeks and are still on")
	}
	notes, _ := st.Notes(ctx)
	var shut []store.Note
	for _, n := range notes {
		if strings.HasPrefix(n.ID, "scorecard-shutdown:") {
			shut = append(shut, n)
		}
	}
	if len(shut) != 2 || shut[0].Importance != store.ImportanceHigh || !strings.Contains(shut[0].Detail, "Turn it back on: rm ") {
		t.Fatalf("shutdown notes: %+v", shut)
	}
	var sc bytes.Buffer
	if err := printScorecard(ctx, st, d.sw, false, &sc); err != nil || !strings.Contains(sc.String(), "tacit: off, shut down in 2030-W12") {
		t.Fatalf("scorecard: %s %v", sc.String(), err)
	}
}

// The loop and supervision honor momentum's off-file, and the loop never takes the tacit family.
func TestTheLoopHonorsTheOffFileAndLeavesTacitCases(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	cfg := t.TempDir()
	t.Setenv("MOMENTUM_CONFIG", cfg)
	os.WriteFile(filepath.Join(cfg, "off"), []byte("momentum is off: a test\n"), 0o644)
	if code, _, errOut := momentum("loop"); code != 0 || !strings.Contains(errOut, "the loop is off (momentum is off: a test)") {
		t.Fatalf("loop while off: %d %s", code, errOut)
	}
	if code, _, errOut := momentum("supervise"); code != 0 {
		t.Fatalf("supervise while off: %d %s", code, errOut)
	}
	if _, err := os.Stat(paths.DB()); !os.IsNotExist(err) {
		t.Fatalf("supervision ran while off: %v", err)
	}
	os.Remove(filepath.Join(cfg, "off"))
	os.WriteFile(filepath.Join(cfg, "sources.json"), []byte(`{"loop":{"families":["failures"]}}`), 0o600)
	if code, _, errOut := momentum("loop", "--dry", "--family", "failures, tacit"); code != 2 || !strings.Contains(errOut, "report-only") {
		t.Fatalf("loop --family tacit: %d %s", code, errOut)
	}
}

// A failed model pass still keeps the week's report and verdicts, and the run fails so its unit shows it.
func TestAFailedModelPassFailsTheRunAfterTheReport(t *testing.T) {
	st, d, _ := weeklyFixture(t)
	d.advise = func(context.Context, string, string, string) (json.RawMessage, string, float64, error) {
		return nil, "", 0, os.ErrDeadlineExceeded
	}
	var out, errOut bytes.Buffer
	err := weekly(context.Background(), st, d, "", false, false, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "the model pass") {
		t.Fatalf("run: %v", err)
	}
	if b, rerr := os.ReadFile(filepath.Join(weeklyDir(), "2030-W11.md")); rerr != nil || !strings.Contains(string(b), "No model pass:") {
		t.Fatalf("report: %s %v", b, rerr)
	}
	if err := weekly(context.Background(), st, d, "2030-W09", false, false, &out, &errOut); err == nil || !strings.Contains(err.Error(), "only --dry") {
		t.Fatalf("an older week judged: %v", err)
	}
}

func TestWeeklyInstallWritesAPersistentSundayTimer(t *testing.T) {
	dir := t.TempDir()
	code, out, errOut := momentum("weekly", "install", "--dir", dir)
	if code != 0 || !strings.Contains(out, "systemctl --user enable --now momentum-weekly.timer") {
		t.Fatalf("install: %d %s %s", code, out, errOut)
	}
	timer, _ := os.ReadFile(filepath.Join(dir, "momentum-weekly.timer"))
	service, _ := os.ReadFile(filepath.Join(dir, "momentum-weekly.service"))
	if !strings.Contains(string(timer), "OnCalendar=Sun *-*-* 08:00:00\n") || !strings.Contains(string(timer), "Persistent=true") ||
		!strings.Contains(string(service), " weekly\n") || !strings.Contains(string(service), "Wants=momentum-ingest.service") {
		t.Fatalf("units:\n%s\n%s", timer, service)
	}
}
