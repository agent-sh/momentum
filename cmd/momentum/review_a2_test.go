package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

func TestLoopQueriesRefuseUnboundedLimitsBeforeOpeningTheStore(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	t.Setenv("MOMENTUM_LOOP_RUN", "fixture")
	for _, args := range [][]string{
		{"case", "show", "fixture", "--log", "0"},
		{"case", "show", "fixture", "--evidence", "0"},
		{"case", "show", "fixture", "--log", "201"},
		{"evidence", "events", "--limit", "9223372036854775807"},
	} {
		var out, diag bytes.Buffer
		if code := runCommand(context.Background(), args, &out, &diag); code != 2 ||
			!strings.Contains(diag.String(), "narrow the query") || out.Len() != 0 {
			t.Fatalf("%v: code %d, %s%s", args, code, &out, &diag)
		}
	}
}

func TestLoopCaseListStopsAtTwoHundredRows(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	t.Setenv("MOMENTUM_LOOP_RUN", "fixture")
	fakeScrubber(t)
	st, err := store.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i := 0; i < 201; i++ {
		_, _, err := st.ObserveCase(context.Background(), store.Finding{Family: "failures", Signature: fmt.Sprintf("fixture:%d", i), Title: "marker-7f3a"}, "test")
		if err != nil {
			t.Fatal(err)
		}
	}
	var out, diag bytes.Buffer
	code := run(context.Background(), []string{"case", "list", "--json", "--all"}, &out, &diag)
	if code != 0 || strings.Count(out.String(), "\n") != 200 || !strings.Contains(diag.String(), "more cases") {
		t.Fatalf("case listing: code %d, %d rows, diagnostics %s", code, strings.Count(out.String(), "\n"), &diag)
	}
}

func TestCommandOutputWithholdsOversizedProduction(t *testing.T) {
	b, err := commandOutput(exec.Command("head", "-c", "8388610", "/dev/zero"), true)
	if err == nil || b != nil || !strings.Contains(err.Error(), "output withheld") {
		t.Fatalf("oversized helper output: %d bytes, %v", len(b), err)
	}
}
