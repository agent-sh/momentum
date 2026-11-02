package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

// momentum measure runs the measure step once. A dry one opens the store read-only and takes no lock: with no store
// it makes none. A live one takes the loop's lock, so it refuses while a loop runs, and a loop's own run cannot run
// it. Its JSON is one object per case, with lower-case keys.
func TestTheMeasureCommand(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	conf := t.TempDir()
	t.Setenv("MOMENTUM_CONFIG", conf)
	if err := os.WriteFile(filepath.Join(conf, "sources.json"), []byte(`{"loop": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := momentum("measure", "--dry"); code == 0 {
		t.Fatal("a dry measure with no store went through")
	}
	if _, err := os.Stat(paths.DB()); !os.IsNotExist(err) {
		t.Fatalf("a dry measure made a store: %v", err)
	}
	ctx := context.Background()
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := st.ObserveCase(ctx, store.Finding{Family: "failures", Signature: "failures/v1:m", Title: "a thing breaks",
		Evidence: []store.Evidence{{Ref: "file:/t/m", At: time.Now().Add(-2 * time.Hour)}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	mech, next := "m", "n"
	if _, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeployCase(ctx, c.ID, "/t/packages/"+c.ID, time.Now().Add(-time.Hour), "landed", "test"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	unlock, err := lockLoop(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := momentum("measure"); code == 0 || !strings.Contains(errOut, "another loop runs") {
		t.Fatalf("a measure while a loop runs: exit %d %s", code, errOut)
	}
	code, out, errOut := momentum("measure", "--dry", "--json")
	unlock()
	if code != 0 {
		t.Fatalf("a dry measure while a loop runs: exit %d %s", code, errOut)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); err != nil {
		t.Fatalf("its JSON %q: %v", out, err)
	}
	for _, k := range []string{"case", "title", "status", "why"} {
		if _, ok := m[k]; !ok {
			t.Errorf("its JSON lacks %q: %s", k, out)
		}
	}
	for _, key := range []string{"due", "result"} {
		if _, ok := m[key]; ok {
			t.Errorf("unmeasured JSON holds %s: %s", key, out)
		}
	}
	if _, ok := m["Case"]; ok || m["case"] != c.ID || m["status"] != "failed" {
		t.Errorf("its JSON %s", out)
	}
	t.Setenv("MOMENTUM_LOOP_RUN", "r1")
	if code, _, _ := momentum("measure", "--dry"); code != 2 {
		t.Fatalf("a measure inside a loop's run: exit %d", code)
	}
}

// Help describes the independent baseline, read watch windows and the installed file's recorded checkout time.
func TestMeasurementHelpNamesTheCurrentContract(t *testing.T) {
	_, _, help := momentum("ship", "propose", "--help")
	if !strings.Contains(help, "after the checkout first held the change") || !strings.Contains(help, "whole weeks") || strings.Contains(help, "after the landing") {
		t.Fatalf("ship help: %s", help)
	}
	_, out, errOut := momentum("--help")
	text := out + errOut
	if !strings.Contains(text, "three windows before the gate") || !strings.Contains(text, "three read watch windows") {
		t.Fatalf("measure help: %s", text)
	}
}
