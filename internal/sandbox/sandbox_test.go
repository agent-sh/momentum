package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func basicSpec(t *testing.T) Spec {
	t.Helper()
	host := shortTemp(t)
	if err := os.Chmod(host, 0700); err != nil {
		t.Fatal(err)
	}
	return Spec{
		Unit: "momentum-run-golden-7f3a", Host: host, Dir: "/work",
		Binds: []Bind{{Src: shortTemp(t), Dst: "/work", Write: true}},
		Argv:  []string{"/bin/true"},
	}
}

func TestCommandGolden(t *testing.T) {
	s := basicSpec(t)
	out, input := t.TempDir(), t.TempDir()
	store := filepath.Join(t.TempDir(), "store.sqlite")
	if err := os.WriteFile(store, []byte("marker-7f3a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(s.Host, "relay.sock")
	socketFixture(t, socket)
	s.Binds = append(s.Binds,
		Bind{Src: out, Dst: "/out", Write: true},
		Bind{Src: store, Dst: "/ro/store.sqlite"},
		Bind{Src: input, Dst: "/run/input"},
	)
	s.Relay = socket
	s.Env = map[string]string{"ZETA": "marker-7f3a", "ALPHA": "marker-7f3a", "PATH": "/opt/tool:/usr/bin:/bin"}
	s.Argv = []string{"/bin/sh", "-c", "exit 7", "arg with spaces", "${UNCHANGED}"}
	cmd, err := s.Command(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"systemd-run", "--user", "--wait", "--pipe", "--collect", "--quiet",
		"--expand-environment=no", "--unit=momentum-run-golden-7f3a",
		"-p", "MemoryMax=4G", "-p", "MemorySwapMax=0", "-p", "CPUQuota=400%", "-p", "TasksMax=512",
		"-p", "RuntimeMaxSec=1800", "-p", "NoNewPrivileges=yes",
		"-p", "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK", "--",
		"bwrap", "--unshare-all", "--unshare-user", "--disable-userns",
		"--die-with-parent", "--new-session", "--clearenv",
		"--ro-bind", "/usr", "/usr",
		"--symlink", "usr/bin", "/bin", "--symlink", "usr/sbin", "/sbin",
		"--symlink", "usr/lib", "/lib", "--symlink", "usr/lib64", "/lib64",
		"--ro-bind", "/etc/ld.so.cache", "/etc/ld.so.cache",
		"--ro-bind", "/etc/alternatives", "/etc/alternatives",
		"--ro-bind", filepath.Join(s.Host, "passwd"), "/etc/passwd",
		"--ro-bind", filepath.Join(s.Host, "group"), "/etc/group",
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
		"--dir", "/home/agent", "--dir", "/run",
		"--bind", s.Binds[0].Src, "/work", "--bind", out, "/out",
		"--ro-bind", store, "/ro/store.sqlite",
		"--ro-bind", input, "/run/input", "--ro-bind", socket, "/run/relay.sock",
		"--chdir", "/work",
		"--setenv", "HOME", "/home/agent", "--setenv", "PATH", "/opt/tool:/usr/bin:/bin",
		"--setenv", "LANG", "C.UTF-8", "--setenv", "TMPDIR", "/tmp",
		"--setenv", "ALPHA", "marker-7f3a", "--setenv", "ZETA", "marker-7f3a",
		"/bin/sh", "-c",
		`socat TCP-LISTEN:8787,bind=127.0.0.1,fork,reuseaddr UNIX-CONNECT:/run/relay.sock & i=0; ` +
			`until grep -qs ':2253 [0-9A-F]*:0000 0A ' /proc/net/tcp /proc/net/tcp6; do ` +
			`kill -0 $! 2>/dev/null || { echo 'sandbox: the relay forwarder exited before it listened' >&2; exit 125; }; ` +
			`i=$((i+1)); [ $i -lt 200 ] || { echo 'sandbox: the relay forwarder did not listen in 200 probes 50 ms apart' >&2; exit 125; }; ` +
			`sleep 0.05; done; exec "$@"`,
		"sh", "/bin/sh", "-c", "exit 7", "arg with spaces", "${UNCHANGED}",
	}
	assertArgv(t, cmd.Args, want)
	if cmd.Stdin != nil {
		t.Fatal("stdin must be empty")
	}
	s.Relay = ""
	s.Env = nil
	s.Argv = []string{"/bin/true"}
	cmd, err = s.Command(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range cmd.Args {
		if strings.Contains(arg, "relay.sock") || strings.Contains(arg, "socat") {
			t.Fatal("direct run has a relay argument")
		}
	}
	if !slices.Equal(cmd.Args[len(cmd.Args)-1:], s.Argv) {
		t.Fatal("direct run does not end with the program")
	}
}

// A mismatch reports an index, never an environment value or the full argv.
func assertArgv(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("argv length %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv differs at index %d", i)
		}
	}
}

func TestDestinationRules(t *testing.T) {
	for _, dst := range []string{"/work/child", "/out/child", "work", "/ro/../work", "/ro//store", "/ro", "/opt", "/run", "/etc/passwd"} {
		t.Run(strings.ReplaceAll(dst, "/", "-"), func(t *testing.T) {
			s := basicSpec(t)
			s.Binds[0].Dst = dst
			if _, err := s.Command(context.Background()); err == nil {
				t.Fatal("unsafe destination accepted")
			}
		})
	}
	for _, destinations := range [][]string{
		{"/ro/tree", "/ro/tree/child"}, {"/ro/tree/child", "/ro/tree"},
		{"/opt/tool", "/opt/tool"}, {"/opt/tool", "/opt/tool/child"},
	} {
		s := basicSpec(t)
		for _, dst := range destinations {
			s.Binds = append(s.Binds, Bind{Src: t.TempDir(), Dst: dst})
		}
		if _, err := s.Command(context.Background()); err == nil {
			t.Fatal("overlapping destinations accepted")
		}
	}
	s := basicSpec(t)
	s.Binds = append(s.Binds, Bind{Src: t.TempDir(), Dst: "/ro/tree"}, Bind{Src: t.TempDir(), Dst: "/ro/tree-two"})
	if _, err := s.Command(context.Background()); err != nil {
		t.Fatalf("distinct sibling destinations refused: %v", err)
	}
}

func TestSourceRules(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		s := basicSpec(t)
		s.Binds[0].Src = filepath.Join(t.TempDir(), "missing")
		if _, err := s.Command(context.Background()); err == nil {
			t.Fatal("missing source accepted")
		}
	})
	t.Run("link", func(t *testing.T) {
		s := basicSpec(t)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(s.Binds[0].Src, link); err != nil {
			t.Fatal(err)
		}
		s.Binds[0].Src = link
		if _, err := s.Command(context.Background()); err == nil {
			t.Fatal("symbolic link source accepted")
		}
	})
	t.Run("nested-socket", func(t *testing.T) {
		s := basicSpec(t)
		child := filepath.Join(s.Binds[0].Src, "child")
		if err := os.Mkdir(child, 0700); err != nil {
			t.Fatal(err)
		}
		socketFixture(t, filepath.Join(child, "fixture.sock"))
		if _, err := s.Command(context.Background()); err == nil {
			t.Fatal("source holding a socket accepted")
		}
	})
	t.Run("link-in-a-parent", func(t *testing.T) {
		s := basicSpec(t)
		parent := t.TempDir()
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(parent, link); err != nil {
			t.Fatal(err)
		}
		inner := filepath.Join(parent, "inner")
		if err := os.Mkdir(inner, 0700); err != nil {
			t.Fatal(err)
		}
		s.Binds[0].Src = filepath.Join(link, "inner")
		if _, err := s.Command(context.Background()); err == nil {
			t.Fatal("a source reached through a link in its parent was accepted")
		}
		s.Binds[0].Src = inner
		if _, err := s.Command(context.Background()); err != nil {
			t.Fatalf("the same directory by its real path was refused: %v", err)
		}
	})
	t.Run("host-mode", func(t *testing.T) {
		s := basicSpec(t)
		if err := os.Chmod(s.Host, 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Command(context.Background()); err == nil {
			t.Fatal("a host directory of mode 0755 was accepted")
		}
	})
	t.Run("unclean", func(t *testing.T) {
		s := basicSpec(t)
		s.Binds[0].Src += "/."
		if _, err := s.Command(context.Background()); err == nil {
			t.Fatal("unclean source accepted")
		}
	})
}

func TestEnvironmentAndUnitRules(t *testing.T) {
	for _, name := range []string{"", "3NAME", "BAD-NAME", "NAME=OTHER", "NAME\nOTHER"} {
		s := basicSpec(t)
		s.Env = map[string]string{name: "marker-7f3a"}
		if _, err := s.Command(context.Background()); err == nil {
			t.Fatal("invalid environment name accepted")
		}
	}
	for _, unit := range []string{"", "momentum-run-", "other-7f3a", "momentum-run-A", "momentum-run-a.service", "momentum-run-a/b", "momentum-run-a_b"} {
		s := basicSpec(t)
		s.Unit = unit
		if _, err := s.Command(context.Background()); err == nil {
			t.Fatal("invalid unit accepted")
		}
	}
	s := basicSpec(t)
	s.Env = map[string]string{"GOOD_NAME7": "marker-7f3a\x00"}
	_, err := s.Command(context.Background())
	if err == nil || strings.Contains(err.Error(), "marker-7f3a") {
		t.Fatal("invalid environment value was accepted or included in the error")
	}
}

func TestRelayRules(t *testing.T) {
	s := basicSpec(t)
	s.Relay = filepath.Join(s.Host, "relay.sock")
	if err := os.WriteFile(s.Relay, []byte("marker-7f3a"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Command(context.Background()); err == nil {
		t.Fatal("regular file relay accepted")
	}
	if err := os.Remove(s.Relay); err != nil {
		t.Fatal(err)
	}
	socketFixture(t, s.Relay)
	for _, mode := range []os.FileMode{0644, 0666} {
		if err := os.Chmod(s.Relay, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Command(context.Background()); err == nil {
			t.Fatal("nonprivate relay socket accepted")
		}
	}
	if err := os.Chmod(s.Relay, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Command(context.Background()); err != nil {
		t.Fatalf("private relay refused: %v", err)
	}
}

// Validation only needs the inode type. Mknod avoids socket networking and can
// run under a harness that forbids bind or setsockopt.
func socketFixture(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mknod(path, syscall.S_IFSOCK|0600, 0); err != nil {
		t.Fatal(err)
	}
}

func TestIdentity(t *testing.T) {
	s := basicSpec(t)
	remove, err := s.identity()
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"passwd": "agent:x:" + strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()) + "::/home/agent:/bin/sh\n",
		"group":  "agent:x:" + strconv.Itoa(os.Getgid()) + ":\n",
	} {
		data, err := os.ReadFile(filepath.Join(s.Host, name))
		if err != nil || string(data) != want {
			t.Fatal("identity file differs from the caller's identity")
		}
	}
	remove()
	for _, name := range []string{"passwd", "group"} {
		if _, err := os.Lstat(filepath.Join(s.Host, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("identity file remains after cleanup")
		}
	}
	target := filepath.Join(t.TempDir(), "fixture")
	if err := os.WriteFile(target, []byte("marker-7f3a"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(s.Host, "passwd")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.identity(); err == nil {
		t.Fatal("existing identity link accepted")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "marker-7f3a" {
		t.Fatal("identity creation changed the existing target")
	}
}

func TestFailurePrefixes(t *testing.T) {
	for _, text := range []string{"bwrap: setup failed\n", "Failed to start transient service unit\n", "systemd-run: failed\n", "Job for momentum-run-x.service failed\n"} {
		var out bytes.Buffer
		w := failureWriter{dst: &out}
		for _, b := range []byte(text) {
			if _, err := w.Write([]byte{b}); err != nil {
				t.Fatal(err)
			}
		}
		if !w.failed || out.String() != text {
			t.Fatal("launcher prefix was missed or output changed")
		}
	}
	w := failureWriter{dst: io.Discard}
	_, _ = w.Write([]byte("program failed\n"))
	if w.failed {
		t.Fatal("ordinary program stderr classified as launcher failure")
	}
}

// Each rule of Spec.validate that nothing else exercises.
func TestValidationRules(t *testing.T) {
	refused := map[string]func(s *Spec){
		"program starting with a dash":   func(s *Spec) { s.Argv = []string{"-x"} },
		"empty program":                  func(s *Spec) { s.Argv = []string{""} },
		"no program":                     func(s *Spec) { s.Argv = nil },
		"NUL in an argument":             func(s *Spec) { s.Argv = []string{"/bin/true", "a\x00b"} },
		"no tasks":                       func(s *Spec) { s.Limits.Tasks = -1 },
		"no runtime":                     func(s *Spec) { s.Limits.RuntimeSec = -1 },
		"relative directory":             func(s *Spec) { s.Dir = "work" },
		"unit not anchored at the start": func(s *Spec) { s.Unit = "xmomentum-run-a" },
		"unit with a capital":            func(s *Spec) { s.Unit = "momentum-run-A" },
		"memory with a newline":          func(s *Spec) { s.Limits.Memory = "1G\n" },
		"top-level socket as a source": func(s *Spec) {
			path := filepath.Join(shortTemp(t), "top.sock")
			socketFixture(t, path)
			s.Binds[0].Src = path
		},
		"fifo as a source": func(s *Spec) {
			path := filepath.Join(shortTemp(t), "pipe")
			if err := syscall.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
			s.Binds[0].Src = path
		},
		"host reached through a link in a parent": func(s *Spec) {
			link := filepath.Join(shortTemp(t), "link")
			if err := os.Symlink(filepath.Dir(s.Host), link); err != nil {
				t.Fatal(err)
			}
			s.Host = filepath.Join(link, filepath.Base(s.Host))
		},
	}
	for name, mutate := range refused {
		t.Run(name, func(t *testing.T) {
			s := basicSpec(t)
			mutate(&s)
			if _, err := s.Command(context.Background()); err == nil {
				t.Fatal("the spec was accepted")
			}
		})
	}
}

// Run gives the launcher no standard input, whatever the command held.
func TestStdinIsNotForwarded(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "cat; echo done")
	cmd.Stdin = strings.NewReader("marker-7f3a\n")
	var out bytes.Buffer
	if _, err := runCommand(context.Background(), "momentum-run-stdin-test", cmd, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if out.String() != "done\n" {
		t.Fatalf("the command read input: %q", out.String())
	}
}

// One writer for both streams is serialized (os/exec would have done it for a pair of equal writers).
func TestOneWriterForBothStreams(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "for i in 1 2 3 4 5 6 7 8 9 10; do echo out$i; echo err$i >&2; done")
	var shared lockedCounter
	if _, err := runCommand(context.Background(), "momentum-run-shared-writer-test", cmd, &shared, &shared); err != nil {
		t.Fatal(err)
	}
	if shared.bytes() == 0 {
		t.Fatal("nothing was written")
	}
	if shared.overlap.Load() {
		t.Fatal("two goroutines wrote to the shared writer at once")
	}
}

// lockedCounter notes when two goroutines are inside Write at once, with or without the race detector.
type lockedCounter struct {
	n       int
	in      atomic.Bool
	overlap atomic.Bool
}

func (c *lockedCounter) Write(p []byte) (int, error) {
	if !c.in.CompareAndSwap(false, true) {
		c.overlap.Store(true)
	}
	time.Sleep(time.Millisecond)
	c.n += len(p)
	c.in.Store(false)
	return len(p), nil
}
func (c *lockedCounter) bytes() int { return c.n }

// A client killed by a signal reads as 128 plus the signal; a launcher failure prefix marks a failure only with a
// nonzero exit.
func TestResultsOfTheLauncher(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "kill -TERM $$")
	r, err := runCommand(context.Background(), "momentum-run-signal-test", cmd, io.Discard, io.Discard)
	if err != nil || r.Code != 128+int(syscall.SIGTERM) {
		t.Fatalf("a client killed by SIGTERM: %+v %v", r, err)
	}
	for exit, wantFailed := range map[string]bool{"0": false, "1": true} {
		cmd = exec.Command("/bin/sh", "-c", "echo 'bwrap: marker-7f3a' >&2; exit "+exit)
		r, err = runCommand(context.Background(), "momentum-run-prefix-test", cmd, io.Discard, io.Discard)
		if err != nil || r.Failed != wantFailed {
			t.Fatalf("exit %s after a launcher prefix: %+v %v", exit, r, err)
		}
	}
}
