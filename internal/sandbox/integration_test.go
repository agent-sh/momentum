package sandbox

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// Short paths keep Unix sockets below sockaddr_un's path limit. TMPDIR is the
// caller's disk scratch directory, also used by the rest of the Go tests.
func shortTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "s-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return real
	}
	return dir
}

func needSandbox(t *testing.T, relay bool) {
	t.Helper()
	if err := Available(relay); err != nil {
		// A skipped suite proves nothing about the boundary. A run that must prove it (a check before a release, a
		// machine that is meant to have the sandbox) sets MOMENTUM_REQUIRE_SANDBOX, and a skip then fails.
		if os.Getenv("MOMENTUM_REQUIRE_SANDBOX") != "" {
			t.Fatalf("the real sandbox is required and unavailable: %v", err)
		}
		t.Skipf("real sandbox unavailable: %v", err)
	}
}

type fixture struct {
	spec     Spec
	mu       sync.Mutex
	requests []string
}

func standardLayout(t *testing.T) *fixture {
	t.Helper()
	root := shortTemp(t)
	run, host := filepath.Join(root, "run"), filepath.Join(root, "host")
	for _, dir := range []string{
		run, host, filepath.Join(run, "work"), filepath.Join(run, "out"),
		filepath.Join(run, "ro"), filepath.Join(run, "ro", "clone"),
		filepath.Join(host, "input"), filepath.Join(root, "tools"),
	} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	script, err := os.ReadFile("testdata/inside-checks.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host, "input", "inside-checks.sh"), script, 0600); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(run, "ro", "clone")
	if err := os.WriteFile(filepath.Join(clone, "README"), []byte("marker-7f3a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// This repository is a synthetic fixture. Its identity is synthetic too.
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "README"},
		{"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-q", "-m", "fixture"},
	} {
		cmd := exec.Command("git", append([]string{
			"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false",
			"-c", "init.templateDir=", "-C", clone,
		}, args...)...)
		cmd.Env = []string{
			"PATH=/usr/bin:/bin", "HOME=" + host,
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid",
			"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid",
		}
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if err := cmd.Run(); err != nil {
			t.Fatalf("prepare synthetic git fixture: %v", err)
		}
	}
	store := filepath.Join(run, "ro", "store.sqlite")
	db, err := sql.Open("sqlite", filepath.Join(host, "fixture.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE fixture(marker TEXT); INSERT INTO fixture VALUES ('marker-7f3a')"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec("VACUUM INTO ?", store); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	f := &fixture{}
	socket := filepath.Join(host, "relay.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0600); err != nil {
		ln.Close()
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	other := hostHTTP(t, "127.0.0.1:0")
	outside := hostHTTP(t, "127.0.0.2:0")
	f.spec = Spec{
		Unit: "momentum-run-checks-" + filepath.Base(root), Host: host, Dir: "/work", Relay: socket,
		Binds: []Bind{
			{Src: filepath.Join(run, "work"), Dst: "/work", Write: true},
			{Src: filepath.Join(run, "out"), Dst: "/out", Write: true},
			{Src: store, Dst: "/ro/store.sqlite"},
			{Src: clone, Dst: "/ro/clone"},
			{Src: filepath.Join(host, "input"), Dst: "/run/input"},
			{Src: filepath.Join(root, "tools"), Dst: "/opt/tools"},
		},
		Env: map[string]string{
			"EXPECT_UID":        strconv.Itoa(os.Getuid()),
			"EXPECT_ROOT":       "bin dev etc home lib lib64 opt out proc ro run sbin tmp usr work ",
			"EXPECT_ETC":        "alternatives group ld.so.cache passwd ",
			"EXPECT_NO_VSOCK":   "1",
			"EXPECT_SECCOMP":    "2",
			"HOST_HOME":         homePath(t),
			"OTHER_HOST_PORT":   other,
			"OUTSIDE_HOST_PORT": outside,
		},
		Argv: []string{"/bin/sh", "/run/input/inside-checks.sh"},
	}
	var names []string
	for _, name := range []string{"HOME", "PATH", "LANG", "TMPDIR", "EXPECT_ENV"} {
		names = append(names, name)
	}
	for name := range f.spec.Env {
		names = append(names, name)
	}
	slices.Sort(names)
	f.spec.Env["EXPECT_ENV"] = strings.Join(names, " ") + " "
	return f
}

func homePath(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) || home == "/" || home == "/home/agent" {
		t.Skip("a distinct absolute host home is required for the home assertions")
	}
	return home
}

func hostHTTP(t *testing.T, address string) string {
	t.Helper()
	ln, err := net.Listen("tcp", address)
	if err != nil {
		t.Skipf("host loopback fixture listener unavailable: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	go server.Serve(ln)
	t.Cleanup(func() { server.Close() })
	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

// pinRelayPort holds the relay's port on the host's loopback. With --share-net, socat inside would listen on the host's
// loopback itself and route-answers would still read the relay's 503; with something else already on the port (a
// service of the machine's) it cannot listen and reads that service's answer. Either way the port must answer something
// other than the relay for the widening to flip route-answers, so the test pins it when it can, and goes on when the
// machine's own service already does.
func pinRelayPort(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:8787")
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return
		}
		t.Skipf("host loopback fixture listener unavailable: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })}
	go server.Serve(ln)
	t.Cleanup(func() { server.Close() })
}

func assertChecks(t *testing.T, out string, wantFailures []string) {
	t.Helper()
	var failures []string
	seen := map[string]bool{}
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || (fields[0] != "PASS" && fields[0] != "FAIL") {
			continue
		}
		name := strings.TrimSuffix(fields[1], ":")
		if seen[name] {
			t.Fatalf("duplicate assertion %s", name)
		}
		seen[name] = true
		if fields[0] == "FAIL" {
			failures = append(failures, name)
		}
	}
	slices.Sort(failures)
	want := slices.Clone(wantFailures)
	slices.Sort(want)
	if !slices.Equal(failures, want) {
		t.Fatalf("failed checks %v, want %v", failures, want)
	}
	// Counting and naming every assertion prevents a partial script from passing.
	for _, name := range []string{
		"uid", "no-capabilities", "seccomp", "no-new-privileges", "root-entries", "etc-entries",
		"home-entries", "home-empty", "no-host-home", "work-writable", "out-writable",
		"store-readable", "store-read-only", "clone-readable", "clone-read-only", "usr-read-only",
		"env-names", "pid1", "few-processes", "relay-socket", "nested-userns", "vsock",
		"interfaces", "route-answers", "other-loopback-port", "outside-address", "dns",
	} {
		if !seen[name] {
			t.Fatalf("missing assertion %s", name)
		}
	}
	if len(seen) != 27 || !strings.Contains(out, fmt.Sprintf("failures=%d\n", len(want))) {
		t.Fatal("assertion count or summary differs")
	}
}

func TestInsideChecks(t *testing.T) {
	needSandbox(t, true)
	f := standardLayout(t)
	// Pin the relay's host port too. With --share-net, socat cannot listen here
	// and route-answers reads this host fixture's 200 instead of the relay's 503.
	pinRelayPort(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	r, err := f.spec.Run(ctx, &stdout, &stderr)
	if err != nil || r.Code != 0 || r.Failed {
		t.Fatalf("inside checks: exit %d, launcher failure %v, error %v", r.Code, r.Failed, err)
	}
	assertChecks(t, stdout.String(), nil)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("relay received no request")
	}
	for _, request := range f.requests {
		if request != "GET /route-probe" {
			t.Fatal("relay received an unexpected request")
		}
	}
}

// forwarder puts the test's own socat first on the run's PATH, where the relay command finds its forwarder.
func forwarder(t *testing.T, f *fixture, body string) {
	t.Helper()
	i := slices.IndexFunc(f.spec.Binds, func(b Bind) bool { return b.Dst == "/opt/tools" })
	if err := os.WriteFile(filepath.Join(f.spec.Binds[i].Src, "socat"), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	f.spec.Env["PATH"] = "/opt/tools:/usr/bin:/bin"
}

// A relay run starts its program only once the relay's forwarder listens: a first request with no retry reaches the
// relay even from a forwarder that is slow to start.
func TestTheProgramStartsOnceTheRelayListens(t *testing.T) {
	needSandbox(t, true)
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	f := standardLayout(t)
	forwarder(t, f, `sleep 0.5; exec /usr/bin/socat "$@"`)
	f.spec.Argv = []string{"/bin/sh", "-c", `curl --noproxy '*' -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:8787/first-request`}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	r, err := f.spec.Run(ctx, &stdout, &stderr)
	if err != nil || r.Failed || r.Code != 0 || stdout.String() != "503" {
		t.Fatalf("the first request answered %q: exit %d, launcher failure %v, error %v", stdout.String(), r.Code, r.Failed, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Equal(f.requests, []string{"GET /first-request"}) {
		t.Fatalf("the relay received %q", f.requests)
	}
}

// A relay whose forwarder exits, or never listens, ends the run before its program, with its own exit status and why.
func TestARelayThatNeverListensEndsTheRunBeforeItsProgram(t *testing.T) {
	needSandbox(t, true)
	for _, c := range []struct{ name, body, why string }{
		{"exits", "exit 1", "the relay forwarder exited before it listened"},
		{"never listens", "exec sleep 60", "the relay forwarder did not listen in 200 probes 50 ms apart"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := standardLayout(t)
			forwarder(t, f, c.body)
			f.spec.Argv = []string{"/bin/sh", "-c", "echo the program ran"}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			var stdout, stderr bytes.Buffer
			r, err := f.spec.Run(ctx, &stdout, &stderr)
			if err != nil || r.Failed || r.Code != relayNotListening || stdout.Len() != 0 || !strings.Contains(stderr.String(), c.why) {
				t.Fatalf("exit %d, launcher failure %v, error %v, stdout %q, stderr %q", r.Code, r.Failed, err, stdout.String(), stderr.String())
			}
		})
	}
}

func removeArg(args []string, target string) []string {
	i := slices.Index(args, target)
	if i < 0 {
		panic("missing mutation target")
	}
	return append(args[:i:i], args[i+1:]...)
}

func removeProperty(args []string, prefix string) []string {
	for i, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			return append(args[:i-1:i-1], args[i+1:]...)
		}
	}
	panic("missing property mutation target")
}

func insertBefore(args []string, target string, added ...string) []string {
	i := slices.Index(args, target)
	if i < 0 {
		panic("missing insertion target")
	}
	tail := slices.Clone(args[i:])
	return append(append(args[:i:i], added...), tail...)
}

func TestWidenings(t *testing.T) {
	needSandbox(t, true)
	for _, widening := range []struct {
		name string
		want []string
		edit func(*testing.T, []string) []string
	}{
		{"share-net", []string{"interfaces", "route-answers", "other-loopback-port", "outside-address"},
			func(_ *testing.T, args []string) []string {
				return insertBefore(args, "--disable-userns", "--share-net")
			}},
		{"host-home", []string{"home-entries", "no-host-home"},
			func(t *testing.T, args []string) []string {
				home := homePath(t)
				// Read-only access is confined to this deliberate test widening.
				return insertBefore(args, "--chdir", "--ro-bind", home, home)
			}},
		{"keep-env", []string{"env-names"},
			func(_ *testing.T, args []string) []string {
				args = removeArg(args, "--clearenv")
				// Ensure the unit has a harmless extra name even on a minimal manager.
				return insertBefore(args, "--unit="+findUnit(args), "--setenv=SANDBOX_TEST_LEAK=marker-7f3a")
			}},
		{"shared-pids", []string{"pid1", "few-processes"},
			func(_ *testing.T, args []string) []string {
				args = removeArg(args, "--unshare-all")
				return insertBefore(args, "--unshare-user", "--unshare-ipc", "--unshare-net", "--unshare-uts", "--unshare-cgroup-try")
			}},
		{"address-families", []string{"vsock"},
			func(t *testing.T, args []string) []string {
				args = removeProperty(args, "RestrictAddressFamilies=")
				// Measure vsock availability and inherited seccomp under the same
				// unfiltered unit before asserting that removing the filter exposes it.
				i := slices.Index(args, "/bin/sh")
				probe := exec.Command("systemd-run", append(slices.Clone(args[1:i]),
					"python3", "-I", "-c",
					`import socket; socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM); print(next(line.split()[1] for line in open("/proc/self/status") if line.startswith("Seccomp:")))`)...)
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				var out bytes.Buffer
				r, err := runCommand(ctx, findUnit(args), probe, &out, io.Discard)
				if err != nil || r.Code != 0 {
					t.Skip("unfiltered unit cannot create AF_VSOCK here; the expected failure set remains vsock")
				}
				seccomp := strings.TrimSpace(out.String())
				if seccomp != "0" && seccomp != "2" {
					t.Fatal("unfiltered seccomp observation is invalid")
				}
				for j := range args {
					if args[j] == "EXPECT_SECCOMP" && j > 0 && args[j-1] == "--setenv" {
						args[j+1] = seccomp
					}
				}
				t.Logf("unfiltered AF_VSOCK opens; inherited Seccomp is %s", seccomp)
				return args
			}},
	} {
		t.Run(widening.name, func(t *testing.T) {
			f := standardLayout(t)
			pinRelayPort(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			cmd, err := f.spec.Command(context.WithoutCancel(ctx))
			if err != nil {
				t.Fatal(err)
			}
			remove, err := f.spec.identity()
			if err != nil {
				t.Fatal(err)
			}
			defer remove()
			cmd.Args = widening.edit(t, cmd.Args)
			var stdout, stderr bytes.Buffer
			r, err := runCommand(ctx, f.spec.Unit, cmd, &stdout, &stderr)
			if widening.name == "shared-pids" && r.Failed &&
				strings.Contains(stderr.String(), "bwrap:") &&
				strings.Contains(strings.ToLower(stderr.String()), "proc") &&
				strings.Contains(strings.ToLower(stderr.String()), "permitted") {
				t.Skip("kernel refused the proc mount with shared PIDs; the expected failure set remains pid1, few-processes")
			}
			if err != nil || r.Code != len(widening.want) {
				t.Fatalf("widening exit %d, want %d; error %v", r.Code, len(widening.want), err)
			}
			assertChecks(t, stdout.String(), widening.want)
			t.Logf("measured failing checks: %s", strings.Join(widening.want, ", "))
		})
	}
}

func findUnit(args []string) string {
	for _, arg := range args {
		if strings.HasPrefix(arg, "--unit=") {
			return strings.TrimPrefix(arg, "--unit=")
		}
	}
	panic("missing unit argument")
}

func TestLimitsAndCancellation(t *testing.T) {
	needSandbox(t, false)
	s := basicSpec(t)
	s.Unit = "momentum-run-limits-" + filepath.Base(s.Host)
	s.Argv = []string{"/bin/sh", "-c", `echo ready; exec sleep 60`}
	s.Limits = Limits{Memory: "128M", CPUQuota: "50%", Tasks: 32, RuntimeSec: 90}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	writer := &readyWriter{ready: ready}
	done := make(chan struct{})
	var r Result
	var runErr error
	go func() {
		r, runErr = s.Run(ctx, writer, io.Discard)
		close(done)
	}()
	// Always join a started run, including when a property assertion fails.
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(stopTimeout + 10*time.Second):
			t.Error("run did not finish after cleanup")
		}
	}()
	select {
	case <-ready:
	case <-done:
		t.Fatalf("run ended before readiness: exit %d, error %v", r.Code, runErr)
	case <-time.After(20 * time.Second):
		t.Fatal("unit did not become ready")
	}
	showCtx, showCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer showCancel()
	cmd := exec.CommandContext(showCtx, "systemctl", "--user", "show", s.Unit+".service",
		"-p", "MemoryMax", "-p", "MemorySwapMax", "-p", "CPUQuotaPerSecUSec", "-p", "TasksMax", "-p", "RuntimeMaxUSec",
		"-p", "NoNewPrivileges", "-p", "RestrictAddressFamilies")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("read live unit properties: %v", err)
	}
	properties := map[string]string{}
	for line := range strings.SplitSeq(string(out), "\n") {
		name, value, _ := strings.Cut(line, "=")
		properties[name] = value
	}
	for name, want := range map[string]string{
		"MemoryMax": "134217728", "MemorySwapMax": "0", "CPUQuotaPerSecUSec": "500ms",
		"TasksMax": "32", "RuntimeMaxUSec": "1min 30s", "NoNewPrivileges": "yes",
	} {
		if properties[name] != want {
			t.Errorf("unit property %s differs: got %s, want %s", name, properties[name], want)
		}
	}
	families := strings.Fields(properties["RestrictAddressFamilies"])
	slices.Sort(families)
	if !slices.Equal(families, []string{"AF_INET", "AF_INET6", "AF_NETLINK", "AF_UNIX"}) {
		t.Error("unit address families differ")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(stopTimeout + 10*time.Second):
		t.Fatal("cancel did not join the launcher")
	}
	if !errors.Is(runErr, context.Canceled) || r.Failed {
		t.Fatalf("cancel returned error %v, sandbox failure %v", runErr, r.Failed)
	}
	assertUnitGone(t, s.Unit)
	assertUnitsGone(t, s.Unit+"*.service") // this test's units only: a live loop or evaluation has others
}

type readyWriter struct {
	mu    sync.Mutex
	ready chan struct{}
	text  string
	once  sync.Once
}

func (w *readyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.text += string(p)
	if strings.Contains(w.text, "ready\n") {
		w.once.Do(func() { close(w.ready) })
	}
	return len(p), nil
}

func assertUnitGone(t *testing.T, unit string) {
	t.Helper()
	assertUnitsGone(t, unit+".service")
}

func assertUnitsGone(t *testing.T, pattern string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for {
		cmd := exec.CommandContext(ctx, "systemctl", "--user", "list-units",
			"--all", "--plain", "--no-legend", "--no-pager", pattern)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("inspect collected units: %v", err)
		}
		if len(bytes.TrimSpace(out)) == 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("unit remains loaded after collection deadline")
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func TestDirectRunAndExitStatus(t *testing.T) {
	needSandbox(t, false)
	for _, program := range [][]string{
		{"/bin/sh", "-c", "test ! -e /run/relay.sock || exit 8; exit 7"},
		{"/bin/sh", "-c", `read line && exit 9; test "$(awk -F: 'NR>2 {gsub(/ /, "", $1); print $1}' /proc/net/dev)" = lo && exit 0; exit 8`},
		{"/bin/sh", "-c", `kill -TERM $$`},
		{"/missing-program"},
	} {
		s := basicSpec(t)
		s.Unit = "momentum-run-exit-" + filepath.Base(s.Host)
		s.Argv = program
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		var stderr bytes.Buffer
		r, err := s.Run(ctx, io.Discard, &stderr)
		cancel()
		if err != nil {
			t.Fatalf("direct run: %v", err)
		}
		wantCode, wantFailure := 0, false
		switch {
		case program[0] == "/missing-program":
			wantCode, wantFailure = 1, true
		case strings.Contains(program[2], "exit 7"):
			wantCode = 7
		case strings.Contains(program[2], "kill -TERM"):
			wantCode = 143
		}
		if r.Code != wantCode || r.Failed != wantFailure {
			t.Fatalf("exit %d, failure %v; want %d, failure %v", r.Code, r.Failed, wantCode, wantFailure)
		}
		assertUnitGone(t, s.Unit)
	}
}

func TestStopLeftover(t *testing.T) {
	needSandbox(t, false)
	s := basicSpec(t)
	s.Unit = "momentum-run-leftover-test-" + filepath.Base(s.Host)
	s.Argv = []string{"/bin/sh", "-c", `echo ready; exec sleep 60`}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// Command deliberately does not own cleanup. This models a previous client.
	cmd, err := s.Command(context.WithoutCancel(ctx))
	if err != nil {
		t.Fatal(err)
	}
	remove, err := s.identity()
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	ready := make(chan struct{})
	cmd.Stdout, cmd.Stderr = &readyWriter{ready: ready}, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cleanupCancel()
		_ = stopUnit(cleanupCtx, s.Unit)
	})
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("leftover run ended before ready: %v", err)
	case <-ctx.Done():
		t.Fatal("leftover did not become ready")
	}
	// A unit of another prefix, started now, must outlive the call: one caller's startup never stops another's runs.
	other := basicSpec(t)
	other.Unit = "momentum-run-other-test-" + filepath.Base(other.Host)
	other.Argv = []string{"/bin/sh", "-c", `echo ready; exec sleep 60`}
	otherCtx, stopOther := context.WithCancel(ctx)
	otherReady, otherDone := make(chan struct{}), make(chan struct{})
	go func() {
		other.Run(otherCtx, &readyWriter{ready: otherReady}, io.Discard)
		close(otherDone)
	}()
	defer func() {
		stopOther()
		<-otherDone
	}()
	select {
	case <-otherReady:
	case <-ctx.Done():
		t.Fatal("the other unit did not become ready")
	}
	for _, bad := range []string{"", "momentum-run-", "momentum-run-loop", "loop-", "momentum-run-Loop-"} {
		if err := StopLeftover(ctx, bad); err == nil {
			t.Fatalf("prefix %q was accepted", bad)
		}
	}
	if err := StopLeftover(ctx, "momentum-run-leftover-test-"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("leftover client did not exit after stop")
	}
	assertUnitGone(t, s.Unit)
	if out, err := exec.Command("systemctl", "--user", "is-active", other.Unit+".service").Output(); err != nil || strings.TrimSpace(string(out)) != "active" {
		t.Fatal("a unit of another prefix was stopped")
	}
}

// A context cancelled right after the run starts ends it at once, whenever in the unit's creation the cancel lands:
// a stop that systemctl accepts while the unit is still being made must not let the program run on.
func TestCancelRightAfterStartEndsTheRun(t *testing.T) {
	needSandbox(t, false)
	for i, delay := range []time.Duration{0, 5, 10, 15, 20, 25, 30, 35, 40, 50, 60, 80} {
		s := basicSpec(t)
		s.Unit = fmt.Sprintf("momentum-run-early-cancel-%d-%s", i, filepath.Base(s.Host))
		s.Argv = []string{"/bin/sh", "-c", "sleep 30"}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(delay * time.Millisecond)
			cancel()
		}()
		start := time.Now()
		_, err := s.Run(ctx, io.Discard, io.Discard)
		cancel()
		if took := time.Since(start); took > 10*time.Second {
			t.Fatalf("a cancel after %d ms left the program running for %s (err %v)", delay, took, err)
		}
		assertUnitGone(t, s.Unit)
	}
}

// An output writer that fails ends the run: the program does not go on with its output going nowhere, and the
// writer's error is returned whatever the program's own exit.
func TestAFailingOutputWriterEndsTheRun(t *testing.T) {
	needSandbox(t, false)
	for name, argv := range map[string][]string{
		"exit-nonzero": {"/bin/sh", "-c", "echo first; sleep 30; echo after > /work/after.txt; exit 3"},
		"exit-zero":    {"/bin/sh", "-c", "trap '' PIPE; echo first; sleep 30; echo after > /work/after.txt"},
	} {
		t.Run(name, func(t *testing.T) {
			s := basicSpec(t)
			s.Unit = "momentum-run-writer-" + name + "-" + filepath.Base(s.Host)
			s.Argv = argv
			start := time.Now()
			r, err := s.Run(context.Background(), failingWriter{}, io.Discard)
			if took := time.Since(start); took > 15*time.Second {
				t.Fatalf("the run went on for %s after its writer failed", took)
			}
			if err == nil || !strings.Contains(err.Error(), "write output") || !r.Failed {
				t.Fatalf("the writer's error was not returned: %+v %v", r, err)
			}
			if _, statErr := os.Stat(filepath.Join(s.Binds[0].Src, "after.txt")); statErr == nil {
				t.Fatal("the program went on to write after its writer failed")
			}
			assertUnitGone(t, s.Unit)
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("the writer is full") }

// Run removes the identity files it wrote into Host.
func TestRunRemovesItsIdentityFiles(t *testing.T) {
	needSandbox(t, false)
	s := basicSpec(t)
	s.Unit = "momentum-run-identity-" + filepath.Base(s.Host)
	if r, err := s.Run(context.Background(), io.Discard, io.Discard); err != nil || r.Code != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	for _, name := range []string{"passwd", "group"} {
		if _, err := os.Stat(filepath.Join(s.Host, name)); err == nil {
			t.Fatalf("%s is still in Host after the run", name)
		}
	}
}

// A TMPDIR that goes through a link does not make the sandbox unavailable.
func TestAvailableWithATmpdirThroughALink(t *testing.T) {
	needSandbox(t, false)
	link := filepath.Join(shortTemp(t), "tmp-link")
	if err := os.Symlink(shortTemp(t), link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", link)
	if err := Available(); err != nil {
		t.Fatalf("unavailable with a linked TMPDIR: %v", err)
	}
}

// A unit stopped from outside reads as an exit 0 with the output so far: the documented case a caller must not take
// for a finished program.
func TestAUnitStoppedFromOutsideReadsAsExitZero(t *testing.T) {
	needSandbox(t, false)
	s := basicSpec(t)
	s.Unit = "momentum-run-outside-stop-" + filepath.Base(s.Host)
	s.Argv = []string{"/bin/sh", "-c", "echo ready; sleep 30; echo finished"}
	ready := make(chan struct{})
	var out strings.Builder
	w := &readyWriter{ready: ready}
	done := make(chan struct{})
	var r Result
	var err error
	go func() {
		r, err = s.Run(context.Background(), io.MultiWriter(&out, w), io.Discard)
		close(done)
	}()
	select {
	case <-ready:
	case <-time.After(20 * time.Second):
		t.Fatal("unit did not become ready")
	}
	if stopErr := exec.Command("systemctl", "--user", "stop", s.Unit+".service").Run(); stopErr != nil {
		t.Fatal(stopErr)
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("run did not end after its unit was stopped")
	}
	if err != nil || r.Code != 0 || r.Failed || strings.Contains(out.String(), "finished") {
		t.Fatalf("a unit stopped from outside: %+v %v", r, err)
	}
}

// Two live runs never share a unit name.
func TestARunRefusesAUnitNameInUse(t *testing.T) {
	needSandbox(t, false)
	first := basicSpec(t)
	first.Unit = "momentum-run-twice-" + filepath.Base(first.Host)
	first.Argv = []string{"/bin/sh", "-c", "echo ready; sleep 30"}
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan struct{}), make(chan struct{})
	go func() {
		first.Run(ctx, &readyWriter{ready: ready}, io.Discard)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	select {
	case <-ready:
	case <-time.After(20 * time.Second):
		t.Fatal("first unit did not become ready")
	}
	second := basicSpec(t)
	second.Unit = first.Unit
	if _, err := second.Run(context.Background(), io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("a second run with the same unit name: %v", err)
	}
	if out, err := exec.Command("systemctl", "--user", "is-active", first.Unit+".service").Output(); err != nil || strings.TrimSpace(string(out)) != "active" {
		t.Fatal("the first run was disturbed")
	}
}
