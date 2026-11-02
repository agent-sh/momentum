package jail

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/jail/jailtest"
)

// The sandbox tests check isolation the ordinary way, with a harmless marker word planted in a file: a read of a path
// that must not exist inside, a connection that must refuse, a write to a path that must be read-only. Where a test
// can, it runs the same probe outside the sandbox first, where the file reads, the port answers or the write lands, so
// the test fails without the sandbox; the comment on each test says what it proves and how. Nothing here tries to
// leave the sandbox.
const marker = "marker-7f3a"

// TestMain makes the test binary the sandbox's momentum, as momentum's main is: its init and the door's client.
func TestMain(m *testing.M) {
	if code, ok := Inside(os.Args, os.Stdout, os.Stderr); ok {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func self(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

func randomHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// spec is a sandbox whose home is a temporary directory and that has one writable mount: no test depends on the
// machine's real home, its environment or a desktop session.
func spec(t *testing.T) Spec {
	t.Helper()
	bwrap := jailtest.NeedBwrap(t)
	home, work := t.TempDir(), t.TempDir()
	return Spec{Bwrap: bwrap, Self: self(t), Home: home, Mounts: []Mount{{Path: work, Write: true}}, Dir: work,
		Env: []string{"HOME=" + home, "PATH=" + BinDir + ":/usr/bin:/bin", "LANG=C.UTF-8"}}
}

// withRoute gives s a route: a socket that answers every connection with "the-route", reached inside at port 4170.
func withRoute(t *testing.T, s Spec) Spec {
	t.Helper()
	sock := filepath.Join(jailtest.SocketDir(t), "r.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go serveText(ln, "the-route")
	s.Route, s.RoutePort = sock, 4170
	return s
}

// runArgv runs a program in the sandbox and returns what it printed.
func runArgv(t *testing.T, s Spec, argv ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd, err := s.Command(ctx, argv...)
	if err != nil {
		t.Fatal(err)
	}
	defer Close(cmd)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	return out.String(), err
}

// run runs a shell command in the sandbox and returns what it printed.
func run(t *testing.T, s Spec, script string) (string, error) {
	t.Helper()
	return runArgv(t, s, "/bin/bash", "-c", script)
}

// outside runs the same shell command on the machine itself.
func outside(script string) string {
	out, _ := exec.Command("/bin/bash", "-c", script).CombinedOutput()
	return string(out)
}

// exitCode is the exit code of a finished command: 0 for none, -1 when it did not exit at all.
func exitCode(err error) int {
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode()
	}
	return -1
}

// pidsWith lists the processes whose command line holds word, whoever started them.
func pidsWith(word string) []int {
	ents, _ := os.ReadDir("/proc")
	var pids []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		if b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline"); err == nil && bytes.Contains(b, []byte(word)) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// killMarked ends what a failed test left running: the processes that carry its own random word.
func killMarked(word string) {
	for _, pid := range pidsWith(word) {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

// waitFor polls cond until it holds or d has passed, and says whether it held.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A file named as a private file is, planted with a harmless marker in the directory used as the sandbox's home:
// inside, that path is an empty home, so a program cannot read the file, list it, or find it by any path. Outside,
// the same command reads and lists it, so the test fails without the sandbox. A mount under the home shows only itself.
func TestASandboxedProgramCannotReadAFileOfTheHomePath(t *testing.T) {
	s := spec(t)
	planted := filepath.Join(s.Home, "private-notes")
	write(t, planted, marker+"\n")
	proj := filepath.Join(s.Home, "proj")
	write(t, filepath.Join(proj, "readme.txt"), "marker-proj-7f3a\n")
	s.Mounts = append(s.Mounts, Mount{Path: proj})
	script := fmt.Sprintf("cat %q 2>/dev/null; echo '== ls'; ls -A %q; echo '== grep'; grep -rs %q %q 2>/dev/null | head -1; echo '== proj'; cat %q; echo done",
		planted, s.Home, marker, s.Home, filepath.Join(proj, "readme.txt"))
	if out := outside(script); !strings.Contains(out, marker) || !strings.Contains(out, "private-notes") {
		t.Fatalf("outside the sandbox the planted file reads and lists: %s", out)
	}
	inside, err := run(t, s, script)
	if err != nil {
		t.Fatalf("the probe did not run: %v: %s", err, inside)
	}
	if strings.Contains(inside, marker) || strings.Contains(inside, "private-notes") {
		t.Fatalf("the sandbox reads or lists the planted file:\n%s", inside)
	}
	listing := strings.Fields(strings.Split(strings.Split(inside, "== ls")[1], "== grep")[0])
	if len(listing) != 1 || listing[0] != "proj" {
		t.Fatalf("the home holds %v, want only the mount under it", listing)
	}
	if !strings.Contains(inside, "marker-proj-7f3a") || !strings.Contains(inside, "done") {
		t.Fatalf("the mount under the home does not read:\n%s", inside)
	}
}

// The home inside holds only the mounts under it; what a program writes there stays in the sandbox, and a
// read-only mount takes no write. Outside, all of these writes land, so each refusal is the sandbox's.
func TestWritesStayInTheSandboxAndItsWritableMounts(t *testing.T) {
	s := spec(t)
	ro := t.TempDir()
	write(t, filepath.Join(ro, "kept.txt"), "kept\n")
	s.Mounts = append(s.Mounts, Mount{Path: ro})
	work := s.Mounts[0].Path
	name := "written-inside-" + randomHex(t)
	script := fmt.Sprintf(`echo a > %q/%s && echo home-ok; echo b > %q/new.txt && echo rw-ok; echo c > %q/kept.txt || echo ro-refused; cat %q/kept.txt`,
		s.Home, name, work, ro, ro)
	if out := outside(script); strings.Contains(out, "ro-refused") || !strings.Contains(out, "rw-ok") {
		t.Fatalf("outside the sandbox every write lands: %s", out)
	}
	os.Remove(filepath.Join(s.Home, name))
	os.Remove(filepath.Join(work, "new.txt"))
	write(t, filepath.Join(ro, "kept.txt"), "kept\n")
	out, err := run(t, s, script)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, w := range []string{"home-ok", "rw-ok", "ro-refused", "kept"} {
		if !strings.Contains(out, w) {
			t.Fatalf("want %q in %s", w, out)
		}
	}
	if _, err := os.Stat(filepath.Join(s.Home, name)); !os.IsNotExist(err) {
		t.Fatalf("a write to the sandbox's home reached the real one: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(work, "new.txt")); string(got) != "b\n" {
		t.Fatalf("a writable mount kept %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(ro, "kept.txt")); string(got) != "kept\n" {
		t.Fatalf("a read-only mount changed: %q", got)
	}
}

// The only way out is the route: a listener on the machine's loopback and one on its other addresses are out of
// reach, a name resolves to nothing, and the loopback port inside answers with the route's socket. Outside, the
// same probes reach both listeners, so the test fails without the sandbox.
func TestASandboxReachesNoHostButItsRoute(t *testing.T) {
	s := withRoute(t, spec(t))
	host, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	go serveText(host, "outside-listener")
	port := host.Addr().(*net.TCPAddr).Port
	other := ""
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ip, ok := a.(*net.IPNet); ok && ip.IP.To4() != nil && !ip.IP.IsLoopback() {
				other = ip.IP.String()
				break
			}
		}
	}
	probe := func(addr string, p int) string {
		return fmt.Sprintf(`if exec 3<>/dev/tcp/%s/%d; then head -c 40 <&3; echo; else echo refused-%s; fi; exec 3>&-;`, addr, p, addr)
	}
	script := probe("127.0.0.1", port) + "getent hosts example.com || echo no-name;" + probe("127.0.0.1", s.RoutePort)
	if other != "" {
		script += probe(other, port)
	}
	out := outside(probe("127.0.0.1", port) + probe(firstOf(other, "127.0.0.1"), port))
	if strings.Count(out, "outside-listener") != 2 {
		t.Fatalf("outside the sandbox both probes reach the listener: %s", out)
	}
	inside, err := run(t, s, script)
	if err != nil {
		t.Fatalf("%v: %s", err, inside)
	}
	if strings.Contains(inside, "outside-listener") {
		t.Fatalf("the sandbox reached a listener of the machine's:\n%s", inside)
	}
	for _, w := range []string{"refused-127.0.0.1", "no-name", "the-route"} {
		if !strings.Contains(inside, w) {
			t.Fatalf("want %q in:\n%s", w, inside)
		}
	}
	if other != "" && !strings.Contains(inside, "refused-"+other) {
		t.Fatalf("the machine's other address answered:\n%s", inside)
	}
}

func serveText(ln net.Listener, text string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		c.Write([]byte(text + "\n"))
		c.Close()
	}
}

// Nothing of the caller's environment, and none of the machine's sockets, are there: not the session bus, not the
// run time directory, not an agent's socket. The caller's environment is set here with synthetic values, whatever
// shell runs the test, and the sockets exist outside; a Command that passed the environment on would show them.
func TestTheSandboxHoldsNoSocketOrVariableOfTheCallers(t *testing.T) {
	s := spec(t)
	rt := jailtest.SocketDir(t)
	bus, agent := filepath.Join(rt, "bus"), filepath.Join(rt, "agent.sock")
	for _, p := range []string{bus, agent} {
		ln, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
	}
	t.Setenv("XDG_RUNTIME_DIR", rt)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+bus)
	t.Setenv("SSH_AUTH_SOCK", agent)
	t.Setenv("PRIVATE_SETTING", marker)
	script := fmt.Sprintf(`env; echo '== run time directory'; ls -A %[1]q 2>&1; echo '== sockets'; for p in %[2]q %[3]q; do test -S $p && echo "socket $p" || echo "no socket $p"; done; echo '== run'; ls -A /run`,
		rt, bus, agent)
	if out := outside(script); !strings.Contains(out, "socket "+bus) || !strings.Contains(out, "SSH_AUTH_SOCK=") {
		t.Fatalf("outside the sandbox the sockets and the variables are there: %s", out)
	}
	out, err := run(t, s, script)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, w := range []string{"DBUS_SESSION_BUS_ADDRESS", "SSH_AUTH_SOCK", "XDG_RUNTIME_DIR", "PRIVATE_SETTING", marker, "socket " + bus, "socket " + agent} {
		if strings.Contains(strings.ReplaceAll(out, "no socket", "none"), w) {
			t.Fatalf("%q reached the sandbox:\n%s", w, out)
		}
	}
	if !strings.Contains(out, "No such file") || strings.Count(out, "no socket") != 2 {
		t.Fatalf("the run time directory is there:\n%s", out)
	}
	if run := strings.TrimSpace(out[strings.LastIndex(out, "== run")+len("== run"):]); run != "momentum" {
		t.Fatalf("/run holds %q, want only momentum", run)
	}
}

// The momentum and tacit commands inside send their arguments to the door, which runs what its caller allows and
// refuses the rest; a command typed in a directory of the sandbox's says so.
func TestTheDoorRunsWhatItsCallerAllows(t *testing.T) {
	s := spec(t)
	sock := filepath.Join(jailtest.SocketDir(t), "d.sock")
	var (
		mu  sync.Mutex
		got []Request
	)
	stop, err := ServeDoor(sock, func(_ context.Context, r Request) Reply {
		mu.Lock()
		got = append(got, r)
		mu.Unlock()
		if r.Argv[0] == "momentum" && len(r.Argv) > 2 && r.Argv[1] == "evidence" {
			return Reply{Stdout: []byte("ran " + strings.Join(r.Argv, " ") + "\n")}
		}
		if r.Argv[0] == "tacit" {
			return Reply{Stdout: []byte("tacit answers\n"), Code: 0}
		}
		return Refuse("%s is not among this run's commands", strings.Join(r.Argv, " "))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	s.Door = sock
	out, err := run(t, s, `momentum evidence show ev:1 --bytes 9; tacit search lock; momentum loop; echo "code=$?"`)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, w := range []string{"ran momentum evidence show ev:1 --bytes 9", "tacit answers", "momentum loop is not among this run's commands", "code=126"} {
		if !strings.Contains(out, w) {
			t.Fatalf("want %q in:\n%s", w, out)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 || got[0].Dir != s.Dir {
		t.Fatalf("the door read %+v", got)
	}
}

// A sandbox with no route has no network at all, and its program still runs. Outside, the same port answers (the
// test's own listener on it), so the refusal is the sandbox's.
func TestASandboxWithNoRouteHasNoNetwork(t *testing.T) {
	s := spec(t)
	host, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	go serveText(host, "outside-listener")
	port := host.Addr().(*net.TCPAddr).Port
	script := fmt.Sprintf(`if exec 3<>/dev/tcp/127.0.0.1/%d; then echo open; else echo refused; fi; cat /proc/net/dev | tail -n +3 | cut -d: -f1`, port)
	if out := outside(script); !strings.Contains(out, "open") {
		t.Fatalf("outside the sandbox the port answers: %s", out)
	}
	out, err := run(t, s, script)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(out, "refused") || strings.Contains(out, "\nopen") {
		t.Fatalf("want the port refused: %s", out)
	}
	ifaces := strings.Fields(out[strings.LastIndex(out, "refused")+len("refused"):])
	if len(ifaces) != 1 || ifaces[0] != "lo" {
		t.Fatalf("want only the loopback interface, got %v in %s", ifaces, out)
	}
}

// Command refuses a Spec that undoes the sandbox, and accepts the ones that look like them but do not. No sandbox
// is made: the refusals come before anything is read.
func TestCommandRefusesWhatUndoesTheSandbox(t *testing.T) {
	const home = "/synthetic/home"
	mounts := func(m ...Mount) Spec { return Spec{Home: home, Mounts: m} }
	route := func(port int) Spec {
		return Spec{Home: home, Self: "/synthetic/momentum", Route: "/synthetic/r.sock", RoutePort: port}
	}
	for _, c := range []struct {
		name string
		spec Spec
		want string // a word of the refusal; empty when the Spec is fine
	}{
		{"no home", Spec{}, "home"},
		{"a relative home", Spec{Home: "rel"}, "home"},
		{"a home longer than a path", Spec{Home: "/" + strings.Repeat("h", 70000)}, "at most"},
		{"a relative mount", mounts(Mount{Path: "rel/path"}), "absolute"},
		{"a mount with dots", mounts(Mount{Path: "/data/../other"}), "clean"},
		{"a mount with a trailing slash", mounts(Mount{Path: "/data/"}), "clean"},
		{"a mount with a double slash", mounts(Mount{Path: "/data//x"}), "clean"},
		{"the root", mounts(Mount{Path: "/"}), "whole machine"},
		{"the root, writable", mounts(Mount{Path: "/", Write: true}), "whole machine"},
		{"the home", mounts(Mount{Path: home}), "the home"},
		{"the home's parent", mounts(Mount{Path: "/synthetic"}), "the home"},
		{"etc", mounts(Mount{Path: "/etc"}), "/etc"},
		{"proc", mounts(Mount{Path: "/proc"}), "/proc"},
		{"dev", mounts(Mount{Path: "/dev"}), "/dev"},
		{"run", mounts(Mount{Path: "/run"}), "/run"},
		{"a child before its parent", mounts(Mount{Path: "/data/x", Write: true}, Mount{Path: "/data"}), "listed after"},
		{"the same mount twice", mounts(Mount{Path: "/data"}, Mount{Path: "/data", Write: true}), "listed after"},
		{"a route without momentum's binary", Spec{Home: home, Route: "/synthetic/r.sock", RoutePort: 4170}, "Self"},
		{"a door without momentum's binary", Spec{Home: home, Door: "/synthetic/d.sock"}, "Self"},
		{"a route with no port", route(0), "port"},
		{"a route on port 1", route(1), "port"},
		{"a route on port 80", route(80), "port"},
		{"a route on port 1023", route(1023), "port"},
		{"a route above the last port", route(65536), "port"},
		// the same shapes, fine
		{"a parent before its child", mounts(Mount{Path: "/data"}, Mount{Path: "/data/x", Write: true}), ""},
		{"a sibling with the same prefix", mounts(Mount{Path: "/data"}, Mount{Path: "/data2", Write: true}), ""},
		{"a mount under the home", mounts(Mount{Path: home + "/proj"}), ""},
		{"a mount under run", mounts(Mount{Path: "/run/synthetic-dir"}), ""},
		{"a route on port 1024", route(1024), ""},
		{"a route on port 65535", route(65535), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			cmd, err := c.spec.Command(context.Background(), "true")
			if cmd != nil {
				Close(cmd)
			}
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.want != "" && err == nil:
				t.Fatal("accepted")
			case c.want != "" && !strings.Contains(err.Error(), c.want):
				t.Fatalf("refused for another reason: %v (want %q)", err, c.want)
			}
		})
	}
}

// The data files bubblewrap reads are written by a goroutine: more than a pipe holds must not block the caller
// while nothing reads yet.
func TestDataFilesLargerThanAPipeDoNotBlockTheCaller(t *testing.T) {
	data := strings.Repeat("0123456789abcdef", 1<<16) // 1 MiB: more than any pipe holds
	type result struct {
		r   *os.File
		err error
	}
	made := make(chan result, 1)
	go func() {
		r, err := dataFile(data)
		made <- result{r, err}
	}()
	select {
	case m := <-made:
		if m.err != nil {
			t.Fatal(m.err)
		}
		defer m.r.Close()
		got, err := io.ReadAll(m.r)
		if err != nil || string(got) != data {
			t.Fatalf("read %d bytes of %d: %v", len(got), len(data), err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dataFile blocks until something reads the pipe")
	}
}

// /etc holds only the files a program needs to start, and the sandbox's own passwd, group and hosts: none of the
// machine's other configuration.
func TestEtcHoldsOnlyItsNamedFiles(t *testing.T) {
	s := spec(t)
	out, err := run(t, s, `ls -A /etc; echo '== passwd'; cat /etc/passwd; echo '== id'; id -un`)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	listing, rest, _ := strings.Cut(out, "== passwd")
	allowed := map[string]bool{"passwd": true, "group": true, "hosts": true}
	for _, f := range etc {
		allowed[strings.Split(strings.TrimPrefix(f, "/etc/"), "/")[0]] = true
	}
	for _, f := range strings.Fields(listing) {
		if !allowed[f] {
			t.Errorf("/etc holds %s", f)
		}
	}
	if lines := strings.Split(strings.TrimSpace(strings.Split(rest, "== id")[0]), "\n"); len(lines) != 1 {
		t.Errorf("passwd holds more than the user: %q", lines)
	}
}

// /etc/ssl holds the public certificate authorities and the OpenSSL configuration, and no private directory: on
// Debian and Ubuntu that directory is readable by a group whose members the sandbox keeps. A certificate in the
// directory reads inside, so what its links lead to is there too (the certs directory links into /usr/share).
func TestSslHoldsOnlyItsCertsAndConfig(t *testing.T) {
	s := spec(t)
	if _, err := os.Stat("/etc/ssl/private"); err != nil {
		t.Skip("this machine has no /etc/ssl/private, so there is nothing for the sandbox to keep out of it")
	}
	bundle := "/etc/ssl/certs/ca-certificates.crt"
	if _, err := os.Stat(bundle); err != nil {
		bundle = ""
		ents, _ := os.ReadDir("/etc/ssl/certs")
		for _, e := range ents {
			if p := filepath.Join("/etc/ssl/certs", e.Name()); strings.HasSuffix(p, ".pem") || strings.HasSuffix(p, ".crt") {
				if _, err := os.Stat(p); err == nil {
					bundle = p
					break
				}
			}
		}
	}
	if bundle == "" {
		t.Skip("this machine has no certificate in /etc/ssl/certs to read")
	}
	script := fmt.Sprintf(`ls -A /etc/ssl; test -e /etc/ssl/private && echo private-present || echo private-absent; head -c 27 %q`, bundle)
	if out := outside(script); !strings.Contains(out, "private") || !strings.Contains(out, "-----BEGIN CERTIFICATE-----") {
		t.Fatalf("outside the sandbox the private directory is listed and a certificate reads: %s", out)
	}
	out, err := run(t, s, script)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(out, "private-absent") || strings.Contains(out, "private-present") {
		t.Fatalf("/etc/ssl/private is inside:\n%s", out)
	}
	if !strings.Contains(out, "-----BEGIN CERTIFICATE-----") {
		t.Fatalf("a certificate of %s does not read inside:\n%s", bundle, out)
	}
	for _, f := range strings.Fields(strings.Split(out, "private-")[0]) {
		if f != "certs" && f != "openssl.cnf" {
			t.Errorf("/etc/ssl holds %s", f)
		}
	}
}

// Inside, /proc lists only the sandbox's own processes (bubblewrap is the first), and no capability set holds a
// capability: the program cannot do what only a privileged process can. Outside, the test's own process is in /proc,
// and it is not inside.
func TestTheSandboxSeesOnlyItsOwnProcessesAndHoldsNoCapability(t *testing.T) {
	s := spec(t)
	if os.Getpid() <= 64 {
		t.Skip("the test's own process number is too small to tell it from the sandbox's")
	}
	script := fmt.Sprintf(`n=0; for p in /proc/[0-9]*; do n=$((n+1)); done; echo "procs=$n"; test -e /proc/%d && echo "test-visible" || echo "test-absent"; echo "first=$(cat /proc/1/comm)"; grep -E '^(CapInh|CapPrm|CapEff|CapBnd|CapAmb|NoNewPrivs):' /proc/self/status`,
		os.Getpid())
	if out := outside(script); !strings.Contains(out, "test-visible") {
		t.Fatalf("outside the sandbox the test's process is in /proc: %s", out)
	}
	out, err := run(t, s, script)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(out, "test-absent") {
		t.Fatalf("the test's own process is in the sandbox's /proc:\n%s", out)
	}
	if !strings.Contains(out, "first=bwrap") {
		t.Fatalf("the first process of the sandbox is not bubblewrap's:\n%s", out)
	}
	var procs int
	fmt.Sscanf(out, "procs=%d", &procs)
	if procs < 1 || procs > 12 {
		t.Fatalf("the sandbox lists %d processes, want only its own:\n%s", procs, out)
	}
	caps := 0
	for _, line := range strings.Split(out, "\n") {
		if name, value, ok := strings.Cut(line, ":"); ok && strings.HasPrefix(name, "Cap") {
			caps++
			if strings.Trim(strings.TrimSpace(value), "0") != "" {
				t.Errorf("%s holds a capability: %s", name, line)
			}
		}
	}
	if caps != 5 || !strings.Contains(out, "NoNewPrivs:\t1") {
		t.Fatalf("want five empty capability sets and no new privileges:\n%s", out)
	}
}

// Killing the caller leaves no process of the sandbox. The caller is a shell that starts the sandbox and waits for
// it; the program carries a word of the test's own, and the machine's process table is read after a bounded wait.
func TestKillingTheCallerLeavesNoProcessOfTheSandbox(t *testing.T) {
	s := spec(t)
	word := "marker-" + randomHex(t)
	cmd, err := s.Command(context.Background(), "/bin/bash", "-c", "sleep 30; echo done", word)
	if err != nil {
		t.Fatal(err)
	}
	defer Close(cmd)
	t.Cleanup(func() { killMarked(word) })
	caller := exec.Command("/bin/sh", append([]string{"-c", `"$0" "$@" & wait`, cmd.Path}, cmd.Args[1:]...)...)
	caller.Env, caller.ExtraFiles = cmd.Env, cmd.ExtraFiles
	if err := caller.Start(); err != nil {
		t.Fatal(err)
	}
	programRuns := func() bool {
		for _, pid := range pidsWith(word) {
			if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); err == nil && bytes.HasPrefix(b, []byte("/bin/bash\x00-c\x00")) {
				return true
			}
		}
		return false
	}
	if !waitFor(10*time.Second, programRuns) {
		caller.Process.Kill()
		caller.Wait()
		t.Fatal("the sandbox's program did not start")
	}
	caller.Process.Kill()
	caller.Wait()
	if !waitFor(10*time.Second, func() bool { return len(pidsWith(word)) == 0 }) {
		t.Fatalf("processes of the sandbox are left after its caller was killed: %v", pidsWith(word))
	}
}

// A context deadline ends Run in the time set, even with a child that holds the output pipe: the sandbox dies with
// bubblewrap, so nothing is left to hold it.
func TestAContextDeadlineEndsRunInTheTimeSet(t *testing.T) {
	s := spec(t)
	word := "marker-" + randomHex(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd, err := s.Command(ctx, "/bin/bash", "-c", "sleep 60 & wait", word)
	if err != nil {
		t.Fatal(err)
	}
	defer Close(cmd)
	t.Cleanup(func() { killMarked(word) })
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned no error: the program ran to its end")
		}
		if took := time.Since(start); took > 8*time.Second {
			t.Fatalf("Run took %v for a deadline of 1s", took)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not end within 15s of a 1s deadline: a process of the sandbox survives and holds the output")
	}
}

// The momentum binary inside is the caller's file, bound read-only: a run cannot change what the host runs next. The
// test binary is copied for it, so a failing test would write to a copy. Outside, the copy takes the same write.
func TestMomentumsBinaryInsideIsReadOnly(t *testing.T) {
	s := spec(t)
	s.Self = filepath.Join(t.TempDir(), "momentum")
	src, err := os.Open(self(t))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(s.Self, os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	// opening for append with no byte written: the copy is left whole whichever way it goes
	if out := outside(fmt.Sprintf(`: >> %q && echo copy-writable || echo copy-refused`, s.Self)); !strings.Contains(out, "copy-writable") {
		t.Fatalf("outside the sandbox the copy takes a write: %s", out)
	}
	script := fmt.Sprintf(`: >> %[1]s/momentum && echo momentum-writable || echo momentum-refused; : >> %[1]s/tacit && echo tacit-writable || echo tacit-refused`, BinDir)
	out, err := run(t, s, script)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(out, "momentum-refused") || !strings.Contains(out, "tacit-refused") || strings.Contains(out, "-writable") {
		t.Fatalf("momentum's binary takes a write inside:\n%s", out)
	}
}

// A writable mount of a repository leaves its hooks, its configuration and its info directory read-only: git runs
// what is written there when it next runs outside the sandbox. Outside, the same writes land, so each refusal is the
// sandbox's. The rest of .git and the working tree stay writable. The fixture is a directory shaped like a
// repository, not one git would use.
func TestAWritableMountOfARepositoryKeepsItsControlFilesReadOnly(t *testing.T) {
	s := spec(t)
	repo := s.Mounts[0].Path
	git := filepath.Join(repo, ".git")
	write(t, filepath.Join(git, "config"), "[core]\n\tbare = false\n")
	write(t, filepath.Join(git, "hooks", "pre-commit.sample"), "# sample\n")
	write(t, filepath.Join(git, "info", "exclude"), "# none\n")
	write(t, filepath.Join(git, "HEAD"), "ref: refs/heads/main\n")
	script := fmt.Sprintf(`cd %q
for p in .git/config .git/hooks/post-commit .git/hooks/pre-commit.sample .git/info/attributes .git/info/exclude; do
  if echo "%s" >> $p; then echo "wrote $p"; else echo "refused $p"; fi
done
echo "%s" >> .git/HEAD && echo "wrote .git/HEAD"; echo "%s" > work.txt && echo "wrote work.txt"`, repo, marker, marker, marker)
	out, err := run(t, s, script)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, p := range []string{".git/config", ".git/hooks/post-commit", ".git/hooks/pre-commit.sample", ".git/info/attributes", ".git/info/exclude"} {
		if !strings.Contains(out, "refused "+p) {
			t.Errorf("a write to %s is not refused:\n%s", p, out)
		}
	}
	for _, p := range []string{".git/HEAD", "work.txt"} {
		if !strings.Contains(out, "wrote "+p) {
			t.Errorf("a write to %s is refused:\n%s", p, out)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(git, "config")); string(got) != "[core]\n\tbare = false\n" {
		t.Errorf(".git/config changed: %q", got)
	}
	for _, p := range []string{"hooks/post-commit", "info/attributes"} {
		if _, err := os.Stat(filepath.Join(git, p)); err == nil {
			t.Errorf("%s was created", p)
		}
	}
	if o := outside(script); strings.Contains(o, "refused") {
		t.Fatalf("outside the sandbox the same writes land:\n%s", o)
	}
}

// Command binds the control files that exist after a writable mount that holds a .git directory, and only then. This
// is the shape of the arguments; the sandbox itself is the test above.
func TestCommandBindsGitsControlFilesAfterAWritableMountOfARepository(t *testing.T) {
	repo := t.TempDir()
	git := filepath.Join(repo, ".git")
	write(t, filepath.Join(git, "config"), "[core]\n")
	write(t, filepath.Join(git, "hooks", "pre-commit.sample"), "# sample\n")
	// no info directory: whatever exists is bound
	argsOf := func(s Spec) []string {
		t.Helper()
		cmd, err := s.Command(context.Background(), "true")
		if err != nil {
			t.Fatal(err)
		}
		Close(cmd)
		return cmd.Args
	}
	contains := func(args []string, seq ...string) bool {
		for i := 0; i+len(seq) <= len(args); i++ {
			if strings.Join(args[i:i+len(seq)], "\x00") == strings.Join(seq, "\x00") {
				return true
			}
		}
		return false
	}
	home := t.TempDir()
	hooks, config, info := filepath.Join(git, "hooks"), filepath.Join(git, "config"), filepath.Join(git, "info")
	args := argsOf(Spec{Home: home, Mounts: []Mount{{Path: repo, Write: true}}})
	if !contains(args, "--bind", repo, repo, "--ro-bind", hooks, hooks, "--ro-bind", config, config) {
		t.Errorf("want hooks and config read-only right after the writable mount: %v", args)
	}
	if contains(args, "--ro-bind", info, info) {
		t.Errorf("an info directory that does not exist is bound: %v", args)
	}
	if args := argsOf(Spec{Home: home, Mounts: []Mount{{Path: repo}}}); contains(args, "--ro-bind", hooks, hooks) || contains(args, "--ro-bind", config, config) {
		t.Errorf("a read-only mount needs no extra binds: %v", args)
	}
	linked := t.TempDir() // a linked work tree: .git is a file that links outside
	write(t, filepath.Join(linked, ".git"), "gitdir: /synthetic/main/.git/worktrees/w\n")
	if args := argsOf(Spec{Home: home, Mounts: []Mount{{Path: linked, Write: true}}}); strings.Contains(strings.Join(args, " "), linked+"/.git") {
		t.Errorf("a .git file is bound: %v", args)
	}
	file := filepath.Join(t.TempDir(), "result.json") // a writable single file is not a repository
	write(t, file, "{}\n")
	argsOf(Spec{Home: home, Mounts: []Mount{{Path: file, Write: true}}})
	// a control file that is a link is refused: a mount over it lands on its target, and the run could replace the link
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), filepath.Join(git, "info")); err != nil {
		t.Fatal(err)
	}
	if _, err := (Spec{Home: home, Mounts: []Mount{{Path: repo, Write: true}}}).Command(context.Background(), "true"); err == nil || !strings.Contains(err.Error(), "link") {
		t.Errorf("a link in place of .git/info: %v", err)
	}
}
