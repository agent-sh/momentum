package proc_test

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/agent-sh/momentum/sessions/internal/fake"
	"github.com/agent-sh/momentum/sessions/proc"
)

func TestReadParsesTheProcessTable(t *testing.T) {
	cwd := t.TempDir()
	root := fake.ProcRoot(t,
		fake.Proc{PID: 10, PPID: 1, Argv: []string{"/usr/bin/bash"}, Start: 500, Cwd: cwd},
		fake.Proc{PID: 12, PPID: 10, Argv: []string{"/opt/tool", "--flag", "x y"}, Start: 700, Files: []string{"/data/a.jsonl"}, Env: []string{"A=1", "B=two=2"}},
		fake.Proc{PID: 11, PPID: 10, Argv: []string{"node", "/lib/pi"}, Start: 600},
	)
	fake.File(t, filepath.Join(root, "2", "stat"), "2 (kthreadd) S 0 0 0 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 1 0 0\n") // no cmdline: a kernel thread
	tb, err := proc.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(tb.ByPID) != 3 {
		t.Fatalf("kernel threads are left out; got %d processes", len(tb.ByPID))
	}
	p := tb.ByPID[12]
	if p.PPID != 10 || p.StartTicks != 700 || !reflect.DeepEqual(p.Argv, []string{"/opt/tool", "--flag", "x y"}) {
		t.Fatalf("parsed %+v", p)
	}
	if want := time.Unix(fake.Boot, 0).Add(7 * time.Second); !p.Start.Equal(want) {
		t.Fatalf("start %v, want %v", p.Start, want)
	}
	if got := tb.Children(10); !reflect.DeepEqual(got, []int{11, 12}) {
		t.Fatalf("children %v", got)
	}
	if tb.Cwd(10) != cwd || tb.Cwd(12) != "" {
		t.Fatalf("cwd %q %q", tb.Cwd(10), tb.Cwd(12))
	}
	if got := tb.Files(12); !reflect.DeepEqual(got, []string{"/data/a.jsonl"}) {
		t.Fatalf("files %v", got)
	}
	if tb.Env(12, "B") != "two=2" || tb.Env(12, "C") != "" {
		t.Fatalf("env %q", tb.Env(12, "B"))
	}
	if !tb.ByPID[11].Is("pi") || tb.ByPID[11].Is("node2") || !p.Has("--flag") || p.Has("-p") || p.Exe() != "tool" || p.Arg(5) != "" {
		t.Fatal("argv helpers")
	}
}

func TestAliveTellsARecycledPidByItsStart(t *testing.T) {
	tb, err := proc.Read(fake.ProcRoot(t, fake.Proc{PID: 40, Argv: []string{"x"}, Start: 900}))
	if err != nil {
		t.Fatal(err)
	}
	if !tb.Alive(40, 900) || !tb.Alive(40, 0) {
		t.Fatal("the same process is alive")
	}
	if tb.Alive(40, 800) {
		t.Fatal("a pid now held by a process with another start is not the recorded process")
	}
	if tb.Alive(41, 0) {
		t.Fatal("a missing pid is not alive")
	}
}

func TestSlotsGiveEachProcessTheNextStartInItsDirectory(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	tb, err := proc.Read(fake.ProcRoot(t,
		fake.Proc{PID: 1, Argv: []string{"pi"}, Start: 100, Cwd: a},
		fake.Proc{PID: 2, Argv: []string{"pi"}, Start: 300, Cwd: a},
		fake.Proc{PID: 3, Argv: []string{"pi"}, Start: 200, Cwd: b},
	))
	if err != nil {
		t.Fatal(err)
	}
	var ps []*proc.Proc
	for _, pid := range []int{1, 2, 3} {
		ps = append(ps, tb.ByPID[pid])
	}
	slots := tb.Slots(ps)
	if len(slots) != 3 || slots[0].Proc.PID != 2 || slots[1].Proc.PID != 3 || slots[2].Proc.PID != 1 {
		t.Fatalf("newest first: %+v", slots)
	}
	if !slots[0].Next.IsZero() || !slots[1].Next.IsZero() || !slots[2].Next.Equal(tb.ByPID[2].Start) || slots[2].Cwd != a {
		t.Fatalf("next starts: %+v", slots)
	}
}

func TestACorruptStartIsLeftUnset(t *testing.T) {
	tb, err := proc.Read(fake.ProcRoot(t, fake.Proc{PID: 3, Argv: []string{"x"}, Start: 18446744073709551615}))
	if err != nil {
		t.Fatal(err)
	}
	if p := tb.ByPID[3]; p.StartTicks != 18446744073709551615 || !p.Start.IsZero() {
		t.Fatalf("a start no Duration can hold must not wrap around: %+v", p)
	}
}
