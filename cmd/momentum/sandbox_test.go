package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/jail"
)

// momentumBinary is momentum's binary, built for a test that runs it as a program.
func momentumBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "momentum")
	if out, err := exec.Command("go", "build", "-o", path, ".").CombinedOutput(); err != nil {
		t.Fatalf("build momentum: %v: %s", err, out)
	}
	return path
}

func socketDir(t *testing.T) string {
	t.Helper()
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = t.TempDir()
	}
	b := make([]byte, 4)
	rand.Read(b)
	d := filepath.Join(base, "mc-"+hex.EncodeToString(b))
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// With MOMENTUM_DOOR set, momentum and tacit (a link to momentum by that name) send their arguments to the door
// and print its reply, as they do inside a sandbox: momentum's main reads nothing of the machine's then.
func TestMomentumAndTacitAreTheDoorsClient(t *testing.T) {
	bin := momentumBinary(t)
	sock := filepath.Join(socketDir(t), "d.sock")
	var (
		mu  sync.Mutex
		got [][]string
	)
	stop, err := jail.ServeDoor(sock, func(_ context.Context, r jail.Request) jail.Reply {
		mu.Lock()
		got = append(got, r.Argv)
		mu.Unlock()
		return jail.Reply{Stdout: []byte("door: " + strings.Join(r.Argv, " ") + "\n"), Code: 3}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	tacit := filepath.Join(filepath.Dir(bin), "tacit")
	os.Remove(tacit)
	if err := os.Symlink(bin, tacit); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		prog string
		args []string
		want string
	}{{bin, []string{"evidence", "status"}, "door: momentum evidence status"}, {tacit, []string{"search", "lock"}, "door: tacit search lock"}} {
		cmd := exec.Command(c.prog, c.args...)
		cmd.Env = append(os.Environ(), "MOMENTUM_DOOR="+sock, "MOMENTUM_HOME="+t.TempDir())
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		ee, ok := err.(*exec.ExitError)
		if !ok || ee.ExitCode() != 3 || !strings.Contains(out.String(), c.want) {
			t.Fatalf("%s %v: %v, %q: want the door's reply and its code", c.prog, c.args, err, out.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("the door read %v", got)
	}
}

// momentum sandbox-init, the first process of a sandbox with a route, forwards the route's port inside to the
// route's socket outside, runs its program and exits as the program does.
func TestSandboxInitForwardsTheRoute(t *testing.T) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("no bubblewrap")
	}
	bin := momentumBinary(t)
	sock := filepath.Join(socketDir(t), "r.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("through-the-route\n"))
			c.Close()
		}
	}()
	work := t.TempDir()
	home := os.Getenv("HOME")
	s := jail.Spec{Bwrap: bwrap, Self: bin, Home: home, Dir: work, Mounts: []jail.Mount{{Path: work, Write: true}},
		Env: []string{"HOME=" + home, "PATH=" + jail.BinDir + ":/usr/bin:/bin"}, Route: sock, RoutePort: 4171}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd, err := s.Command(ctx, "/bin/bash", "-c", `exec 3<>/dev/tcp/127.0.0.1/4171 && head -c 40 <&3; exit 7`)
	if err != nil {
		t.Fatal(err)
	}
	defer jail.Close(cmd)
	out, err := cmd.CombinedOutput()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 7 || !strings.Contains(string(out), "through-the-route") {
		t.Fatalf("%v: %s: want the route's answer and the program's exit code", err, out)
	}
}
