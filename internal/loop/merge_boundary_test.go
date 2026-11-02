package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/jail"
	"github.com/agent-sh/momentum/internal/store"
)

func TestEvaluationChargesTheHigherHarnessOrRelayCost(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  Result
		want float64
	}{
		{"relay", Result{Started: true, CostKnown: true, USD: 1, RelayUSD: 2}, 2},
		{"harness", Result{Started: true, CostKnown: true, USD: 3, RelayUSD: 2}, 3},
		{"unknown harness", Result{Started: true, RelayUSD: 2}, 4},
		{"unknown harness above reservation", Result{Started: true, RelayUSD: 5}, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, started := settleRun(store.LoopRun{Started: time.Now()}, tc.res, nil, 4)
			if !started || run.USD != tc.want {
				t.Fatalf("started %v, cost %v, want %v", started, run.USD, tc.want)
			}
		})
	}
}

func TestDropRemovesBothClonesUnderTheShipLock(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	c := testingCase(t, st)
	p, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	// No host step can rely on the run's Git metadata, even while dropping it.
	if err := os.RemoveAll(filepath.Join(p.Work, ".git")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, ".git"), "untrusted metadata")
	unlock, err := s.lock(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Drop(ctx, c.ID, "sent back")
	unlock()
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "another ship step is running") {
		t.Fatalf("drop escaped the ship lock: %v", err)
	}
	for _, dir := range []string{p.Host, p.Work} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("locked drop removed a clone: %v", err)
		}
	}
	if _, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseInvestigating}, "fixture"); err != nil {
		t.Fatal(err)
	}
	r := localRunner(t, st)
	r.Ship = s
	r.dropLeft(ctx)
	for _, dir := range []string{p.Host, p.Work} {
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("dropped clone remains: %s: %v", dir, err)
		}
	}
	dropped, ok, err := s.Load(c.ID)
	if err != nil || !ok || dropped.State != Dropped {
		t.Fatalf("drop lost its receipt: %+v %v", dropped, err)
	}
	if _, err := st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting}, "fixture"); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.Prepare(ctx, c.ID, f.main)
	if err != nil || fresh.State != Prepared || fresh.Host == "" || fresh.BaseCommit == "" {
		t.Fatalf("dropped package cannot prepare again: %+v %v", fresh, err)
	}
}

func TestEvaluationDoorKeepsTheCommandOutsideTheRun(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	write(t, filepath.Join(dir, "marker"), "untrusted")
	prog := script(t, t.TempDir(), "momentum", `printf '%s\n' "$PWD" "$MOMENTUM_NOW"
if test -e marker; then exit 1; fi`)
	calls := 0
	d := doorServer{dir: dir, momentum: prog, timeout: time.Minute, env: []string{"MOMENTUM_NOW=synthetic-clock"},
		allow: func(argv []string) string { return "" },
		ran:   func(argv []string) { calls++ }}
	reply := d.serve(context.Background(), jail.Request{Dir: dir, Argv: []string{"momentum", "evidence", "status"}})
	if reply.Code != 0 || string(reply.Stdout) != HostDir(dir)+"\nsynthetic-clock\n" || calls != 1 {
		t.Fatalf("evaluation door lost its boundary, clock or callback: %+v, calls %d", reply, calls)
	}
}
