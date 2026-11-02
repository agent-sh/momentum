package jail

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/jail/jailtest"
)

// A program name is looked up in the PATH of the environment the program is given, which is not always this
// process's own, and in the C library's default path when that environment has none.
func TestLookPathResolvesAgainstTheEnvironmentTheProgramIsGiven(t *testing.T) {
	dir := t.TempDir()
	prog, data := filepath.Join(dir, "fixture-prog"), filepath.Join(dir, "fixture-data")
	write(t, prog, "#!/bin/sh\n")
	write(t, data, "not a program\n")
	if err := os.Chmod(prog, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		env  []string
		want string // empty when the name is not found
	}{
		{"fixture-prog", []string{"PATH=" + dir}, prog},
		{"fixture-prog", []string{"PATH=/nowhere:" + dir}, prog},
		{"fixture-prog", []string{"HOME=/h", "PATH=/nowhere", "PATH=" + dir}, prog},
		{"fixture-prog", []string{"PATH=/nowhere"}, ""},
		{"fixture-data", []string{"PATH=" + dir}, ""},
		{"fixture-prog", nil, ""},
		{"fixture-prog", []string{"HOME=/h"}, ""},
		{"sh", nil, "/sh"},
		{"sh", []string{"HOME=/h"}, "/sh"},
		{"/an/absolute/path", []string{"PATH=" + dir}, "/an/absolute/path"},
		{"./relative", nil, "./relative"},
	} {
		got, err := lookPath(c.name, c.env)
		switch {
		case c.want == "" && err == nil:
			t.Errorf("%s in %v: found %s", c.name, c.env, got)
		case c.want != "" && err != nil:
			t.Errorf("%s in %v: %v", c.name, c.env, err)
		case c.want != "" && !strings.HasSuffix(got, c.want):
			t.Errorf("%s in %v: found %s, want %s", c.name, c.env, got, c.want)
		}
	}
}

// A bare program name starts with a route and without one, though the environment has no PATH: bubblewrap looks it up
// in the C library's default path when it runs the program itself, and sandbox-init does the same.
func TestABareProgramNameStartsWithAndWithoutARouteAndNoPath(t *testing.T) {
	for _, routed := range []bool{false, true} {
		s := spec(t)
		s.Env = []string{"HOME=" + s.Home}
		if routed {
			s = withRoute(t, s)
		}
		if out, err := runArgv(t, s, "true"); err != nil {
			t.Errorf("route %v: %v: %s", routed, err, out)
		}
	}
}

// The exit codes the package documents, where the program does not start: bubblewrap's own failure is 1 with a message
// that starts "bwrap:", and sandbox-init's is 127 with one that starts "momentum sandbox-init:", or 2 for its own
// arguments. A caller treats both as the sandbox's failure.
func TestAProgramThatDoesNotStartExitsAsDocumented(t *testing.T) {
	s := spec(t)
	out, err := runArgv(t, s, "/no/such/program-xyz")
	if exitCode(err) != 1 || !strings.Contains(out, "bwrap:") {
		t.Errorf("without a route: exit %d, %q, want 1 and a bwrap message", exitCode(err), out)
	}
	out, err = runArgv(t, withRoute(t, s), "/no/such/program-xyz")
	if exitCode(err) != 127 || !strings.Contains(out, "momentum sandbox-init:") {
		t.Errorf("with a route: exit %d, %q, want 127 and a sandbox-init message", exitCode(err), out)
	}
	out, err = runArgv(t, withRoute(t, s), "no-such-program-xyz")
	if exitCode(err) != 127 || !strings.Contains(out, "momentum sandbox-init:") {
		t.Errorf("a bare name with a route: exit %d, %q, want 127 and a sandbox-init message", exitCode(err), out)
	}
	for _, args := range [][]string{nil, {"--port", "80", "--", "true"}, {"--port", "x", "--", "true"}, {"--port", "4170"}} {
		if code := Init(args); code != 2 {
			t.Errorf("Init(%v) = %d, want 2", args, code)
		}
	}
}

// A program a signal killed ends as 128 plus the signal, through sandbox-init and bubblewrap alike, and an exit
// code passes through both unchanged.
func TestAProgramKilledBySignalExitsAs128PlusTheSignal(t *testing.T) {
	for _, routed := range []bool{false, true} {
		for _, c := range []struct {
			name, script string
			code         int
		}{{"TERM", "kill -TERM $$", 143}, {"KILL", "kill -KILL $$", 137}, {"SEGV", "ulimit -c 0; kill -SEGV $$", 139},
			{"exit 7", "exit 7", 7}, {"exit 0", "exit 0", 0}} {
			t.Run(fmt.Sprintf("route %v, %s", routed, c.name), func(t *testing.T) {
				s := spec(t)
				if routed {
					s = withRoute(t, s)
				}
				out, err := run(t, s, c.script)
				if got := exitCode(err); got != c.code {
					t.Fatalf("exit %d, want %d: %s", got, c.code, out)
				}
			})
		}
	}
}

// A signal sent to sandbox-init reaches the program. The program sends one to its parent, which is sandbox-init, and
// then waits a bounded time to be told; without forwarding it never is.
func TestASignalSentToSandboxInitReachesTheProgram(t *testing.T) {
	s := withRoute(t, spec(t))
	start := time.Now()
	out, err := run(t, s, `trap 'echo got-term; exit 0' TERM; kill -TERM $PPID; for i in $(seq 1 100); do sleep 0.1; done; echo no-term; exit 9`)
	if exitCode(err) != 0 || !strings.Contains(out, "got-term") {
		t.Fatalf("the program did not get the signal sandbox-init got (exit %d): %s", exitCode(err), out)
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Errorf("the program took %v to get the signal", took)
	}
}

// The forwarder, like the door, goes on accepting after an error that is not the listener's close.
func TestTheForwarderKeepsAcceptingAfterATemporaryError(t *testing.T) {
	route, err := net.Listen("unix", filepath.Join(jailtest.SocketDir(t), "r.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer route.Close()
	go serveText(route, "the-route")
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	go forward(&flaky{Listener: tcp, errs: []error{emfile(), emfile()}}, route.Addr().String())
	c, err := net.Dial("tcp", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if b, err := io.ReadAll(c); err != nil || string(b) != "the-route\n" {
		t.Fatalf("the forwarder gave %q (%v) after two errors of Accept", b, err)
	}
}

// Inside runs the sandbox's own commands and nothing else: sandbox-init first, then the door's client under the
// name it was started with (tacit or momentum), and it says no when it is neither.
func TestInsideRunsOnlyTheSandboxsOwnCommands(t *testing.T) {
	t.Setenv(DoorEnv, "")
	if code, ok := Inside([]string{"/x/momentum", "evidence", "status"}, io.Discard, io.Discard); ok {
		t.Fatalf("outside a sandbox, Inside ran something (exit %d)", code)
	}
	if code, ok := Inside([]string{"/x/momentum", InitCommand}, io.Discard, io.Discard); !ok || code != 2 {
		t.Errorf("sandbox-init with no arguments: %d, %v, want its usage error 2", code, ok)
	}
	var mu sync.Mutex
	var names []string
	sock := stubDoor(t, func(_ context.Context, r Request) Reply {
		mu.Lock()
		names = append(names, strings.Join(r.Argv, " "))
		mu.Unlock()
		return Reply{Stdout: []byte("door\n"), Code: 3}
	})
	t.Setenv(DoorEnv, sock)
	for _, bin := range []string{"/x/momentum", "/y/tacit"} {
		var out bytes.Buffer
		if code, ok := Inside([]string{bin, "search", "lock"}, &out, io.Discard); !ok || code != 3 || out.String() != "door\n" {
			t.Errorf("%s: %d, %v, %q, want the door's reply and its code", bin, code, ok, out.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(names, ";") != "momentum search lock;tacit search lock" {
		t.Errorf("the door read %q", names)
	}
}
