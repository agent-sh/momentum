package jail

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain makes the test binary the sandbox's momentum: its init and the door's client, as momentum's main is.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == InitCommand {
		os.Exit(Init(os.Args[2:]))
	}
	if door := os.Getenv("MOMENTUM_DOOR"); door != "" {
		name := "momentum"
		if filepath.Base(os.Args[0]) == "tacit" {
			name = "tacit"
		}
		os.Exit(Call(door, name, os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func needBwrap(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("no bubblewrap on this machine")
	}
	if out, err := exec.Command(p, "--unshare-all", "--ro-bind", "/", "/", "true").CombinedOutput(); err != nil {
		t.Skipf("bubblewrap cannot make a sandbox here: %v: %s", err, out)
	}
	return p
}

func self(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// shortDir is a directory for sockets: a socket's path must fit 108 bytes, which a test's temporary directory may
// not. It is under the run time directory when there is one, else under the test's own.
func shortDir(t *testing.T) string {
	t.Helper()
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = t.TempDir()
	}
	b := make([]byte, 4)
	rand.Read(b)
	d := filepath.Join(base, "mj-"+hex.EncodeToString(b))
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// run runs a shell command in the sandbox and returns what it printed.
func run(t *testing.T, s Spec, script string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd, err := s.Command(ctx, "/bin/bash", "-c", script)
	if err != nil {
		t.Fatal(err)
	}
	defer Close(cmd)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	return out.String(), err
}

func spec(t *testing.T) Spec {
	work := t.TempDir()
	return Spec{Bwrap: needBwrap(t), Self: self(t), Home: os.Getenv("HOME"), Mounts: []Mount{{Path: work, Write: true}}, Dir: work,
		Env: []string{"HOME=" + os.Getenv("HOME"), "PATH=" + BinDir + ":/usr/bin:/bin", "LANG=C.UTF-8"}}
}

// A file planted in the real home, named as a credential file is, with a harmless marker: the sandbox's home is
// empty, so a program in it cannot read the file, list it, or find it by any path. Outside, the same command
// reads it, so the test fails without the sandbox.
func TestASandboxedProgramCannotReadAFileOfTheRealHome(t *testing.T) {
	s := spec(t)
	home := os.Getenv("HOME")
	if home == "" {
		t.Skip("no home")
	}
	b := make([]byte, 4)
	rand.Read(b)
	dir := filepath.Join(home, ".momentum-jail-probe-"+hex.EncodeToString(b))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	planted := filepath.Join(dir, "credentials")
	marker := "marker-" + hex.EncodeToString(b)
	if err := os.WriteFile(planted, []byte(marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("cat %q 2>/dev/null; echo '== ls'; ls -A %q; echo '== grep'; grep -rs %q %q 2>/dev/null | head -1; echo done",
		planted, home, marker, dir)
	outside, _ := exec.Command("/bin/bash", "-c", script).CombinedOutput()
	if !strings.Contains(string(outside), marker) || !strings.Contains(string(outside), filepath.Base(dir)) {
		t.Fatalf("outside the sandbox the planted file reads and lists: %s", outside)
	}
	inside, err := run(t, s, script)
	if err != nil {
		t.Fatalf("the probe did not run: %v: %s", err, inside)
	}
	if strings.Contains(inside, marker) || strings.Contains(inside, filepath.Base(dir)) {
		t.Fatalf("the sandbox reads or lists the planted file of the real home:\n%s", inside)
	}
	if !strings.Contains(inside, "done") {
		t.Fatalf("the probe did not finish: %s", inside)
	}
}

// The home inside holds only the mounts under it; what a program writes there stays in the sandbox, and a
// read-only mount takes no write.
func TestWritesStayInTheSandboxAndItsWritableMounts(t *testing.T) {
	s := spec(t)
	ro := t.TempDir()
	if err := os.WriteFile(filepath.Join(ro, "kept.txt"), []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Mounts = append(s.Mounts, Mount{Path: ro})
	work := s.Mounts[0].Path
	b := make([]byte, 4)
	rand.Read(b)
	name := ".momentum-jail-write-" + hex.EncodeToString(b)
	out, err := run(t, s, fmt.Sprintf(`echo a > %q/%s && echo home-ok; echo b > %q/new.txt && echo rw-ok; echo c > %q/kept.txt || echo ro-refused; cat %q/kept.txt`,
		s.Home, name, work, ro, ro))
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, w := range []string{"home-ok", "rw-ok", "ro-refused", "kept"} {
		if !strings.Contains(out, w) {
			t.Fatalf("want %q in %s", w, out)
		}
	}
	if _, err := os.Stat(filepath.Join(s.Home, name)); !os.IsNotExist(err) {
		os.Remove(filepath.Join(s.Home, name))
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
	s := spec(t)
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
	sock := filepath.Join(shortDir(t), "r.sock")
	route, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer route.Close()
	go serveText(route, "the-route")
	s.Route, s.RoutePort = sock, 4170
	probe := func(addr string, p int) string {
		return fmt.Sprintf(`if exec 3<>/dev/tcp/%s/%d; then head -c 40 <&3; echo; else echo refused-%s; fi; exec 3>&-;`, addr, p, addr)
	}
	script := probe("127.0.0.1", port) + "getent hosts example.com || echo no-name;" + probe("127.0.0.1", 4170)
	if other != "" {
		script += probe(other, port)
	}
	outside, _ := exec.Command("/bin/bash", "-c", probe("127.0.0.1", port)+probe(firstOf(other, "127.0.0.1"), port)).CombinedOutput()
	if strings.Count(string(outside), "outside-listener") != 2 {
		t.Fatalf("outside the sandbox both probes reach the listener: %s", outside)
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
// run time directory, not an agent's socket.
func TestTheSandboxHoldsNoSocketOrVariableOfTheCallers(t *testing.T) {
	s := spec(t)
	rt := os.Getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		rt = "/run/user/" + fmt.Sprint(os.Getuid())
	}
	out, err := run(t, s, fmt.Sprintf(`env; ls -A %q 2>&1 || true; ls -A /run; ls -A /run/momentum 2>&1 || true`, rt))
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, w := range []string{"DBUS_SESSION_BUS_ADDRESS", "SSH_AUTH_SOCK", "XDG_RUNTIME_DIR", "bus"} {
		if strings.Contains(out, w) {
			t.Fatalf("%q reached the sandbox:\n%s", w, out)
		}
	}
	if !strings.Contains(out, "No such file") {
		t.Fatalf("the run time directory is there:\n%s", out)
	}
}

// The momentum and tacit commands inside send their arguments to the door, which runs what its caller allows and
// refuses the rest; a command typed in a directory of the sandbox's says so.
func TestTheDoorRunsWhatItsCallerAllows(t *testing.T) {
	s := spec(t)
	sock := filepath.Join(shortDir(t), "d.sock")
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
		return Refuse("%s %s is not among this run's commands", r.Argv[0], r.Argv[1])
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

// A sandbox with no route has no network at all, and its program still runs.
func TestASandboxWithNoRouteHasNoNetwork(t *testing.T) {
	s := spec(t)
	out, err := run(t, s, `if exec 3<>/dev/tcp/127.0.0.1/4170; then echo open; else echo refused; fi; cat /proc/net/dev | tail -n +3 | cut -d: -f1`)
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

func TestASpecWithoutAHomeOrPortIsRefused(t *testing.T) {
	for _, s := range []Spec{{}, {Home: "rel"}, {Home: "/h", Route: "/r.sock", Self: "/m"}, {Home: "/h", Door: "/d.sock"}} {
		if _, err := s.Command(context.Background(), "true"); err == nil {
			t.Errorf("%+v: want a refusal", s)
		}
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
		allowed[filepath.Base(f)] = true
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
