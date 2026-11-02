package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/jail"
	"github.com/agent-sh/momentum/internal/jail/jailtest"
)

func TestDoorDependenciesContainOnlyJailAndTheStandardLibrary(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("list dependencies: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "github.com/agent-sh/momentum/internal/jail") {
		t.Fatal("sandbox binary has no jail implementation")
	}
	for _, dependency := range strings.Fields(string(out)) {
		if dependency != "github.com/agent-sh/momentum/cmd/momentum-door" && dependency != "github.com/agent-sh/momentum/internal/jail" {
			t.Errorf("sandbox binary imports %s", dependency)
		}
	}
}

func smallBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "momentum-door")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build door: %v: %s", err, out)
	}
	return bin
}

func TestClearingTheEnvironmentCannotRunLocalMomentumCommands(t *testing.T) {
	bin := smallBinary(t)
	for _, args := range [][]string{{"supervise", "replay"}, {"loop"}, {"loop", "eval"}, {"evidence", "show", "marker-7f3a"}, {"case", "list"}} {
		cmd := exec.Command(bin, args...)
		cmd.Env = []string{}
		cmd.Dir = t.TempDir()
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "usage:") {
			t.Fatalf("%v ran locally: %v: %s", args, err, out)
		}
	}
}

func TestRealDoorBinaryRunsCommandsThroughTheSandboxDoor(t *testing.T) {
	bwrap := jailtest.NeedBwrap(t)
	bin := smallBinary(t)
	work := t.TempDir()
	path := filepath.Join(jailtest.SocketDir(t), "door.sock")
	stop, err := jail.ServeDoor(path, func(_ context.Context, request jail.Request) jail.Reply {
		if strings.Join(request.Argv, " ") != "momentum evidence show marker-7f3a" || request.Dir != work {
			return jail.Refuse("unexpected request")
		}
		return jail.Reply{Stdout: []byte("marker-7f3a door reply\n")}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	spec := jail.Spec{Bwrap: bwrap, Self: bin, Home: "/empty", Dir: work, Door: path,
		Mounts: []jail.Mount{{Path: work, Write: true}}, Env: []string{"PATH=" + jail.BinDir + ":/usr/bin:/bin"}}
	cmd, err := spec.Command(ctx, "/bin/sh", "-c", `momentum evidence show marker-7f3a && env -i /run/momentum/bin/momentum supervise replay; test "$?" -ne 0`)
	if err != nil {
		t.Fatal(err)
	}
	defer jail.Close(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "marker-7f3a door reply") || !strings.Contains(string(out), "usage:") {
		t.Fatalf("door run: %v: %s", err, out)
	}
}
