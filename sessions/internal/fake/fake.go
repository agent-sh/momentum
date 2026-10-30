// Package fake builds synthetic machines for tests: a proc filesystem with processes, their working
// directories, open files and environments, and JSONL files written line by line.
package fake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Boot is the boot time every fake proc root reports, in Unix seconds.
const Boot = 1_700_000_000

// Proc is one fake process.
type Proc struct {
	PID   int
	PPID  int
	Argv  []string
	Start uint64   // start ticks after boot
	Cwd   string   // working directory; "" for none
	Files []string // what its file descriptors point at
	Env   []string // KEY=VALUE
	// Its state (default S), process group (default 1), controlling terminal (tty_nr, default 0: none) and its
	// terminal's foreground group (default -1, none).
	State byte
	PGRP  int
	TTY   int
	TPGID int
}

// ProcRoot writes a proc filesystem holding ps and returns its root.
func ProcRoot(t testing.TB, ps ...Proc) string {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "stat"), fmt.Sprintf("cpu  1 2 3\nbtime %d\nprocesses 9\n", Boot))
	for _, p := range ps {
		dir := filepath.Join(root, strconv.Itoa(p.PID))
		comm := filepath.Base(p.Argv[0])
		if len(comm) > 15 {
			comm = comm[:15]
		}
		write(t, filepath.Join(dir, "cmdline"), strings.Join(p.Argv, "\x00")+"\x00")
		// a command name with spaces and parentheses, which stat parsing must survive
		state, pgrp, tpgid := p.State, p.PGRP, p.TPGID
		if state == 0 {
			state = 'S'
		}
		if pgrp == 0 {
			pgrp = 1
		}
		if tpgid == 0 {
			tpgid = -1
		}
		write(t, filepath.Join(dir, "stat"), fmt.Sprintf("%d (%s) x) %c %d %d 1 %d %d 4194560 1 0 0 0 0 0 0 0 20 0 1 0 %d 1000 100 18446744073709551615\n",
			p.PID, comm, state, p.PPID, pgrp, p.TTY, tpgid, p.Start))
		if p.Cwd != "" {
			link(t, p.Cwd, filepath.Join(dir, "cwd"))
		}
		if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o700); err != nil {
			t.Fatal(err)
		}
		for i, f := range p.Files {
			link(t, f, filepath.Join(dir, "fd", strconv.Itoa(i+3)))
		}
		write(t, filepath.Join(dir, "environ"), strings.Join(p.Env, "\x00"))
	}
	return root
}

// Lines writes JSON values to path, one per line, creating its directory. Like the harnesses (JSON.stringify,
// serde), it leaves <, > and & unescaped.
func Lines(t testing.TB, path string, vs ...any) {
	t.Helper()
	var b strings.Builder
	for _, v := range vs {
		b.Write(encode(t, v))
	}
	write(t, path, b.String())
}

func encode(t testing.TB, v any) []byte {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// Append adds JSON values to path, one per line.
func Append(t testing.TB, path string, vs ...any) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, v := range vs {
		if _, err := f.Write(encode(t, v)); err != nil {
			t.Fatal(err)
		}
	}
}

// File writes s to path, creating its directory.
func File(t testing.TB, path, s string) { t.Helper(); write(t, path, s) }

func write(t testing.TB, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func link(t testing.TB, target, at string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(at), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, at); err != nil {
		t.Fatal(err)
	}
}
