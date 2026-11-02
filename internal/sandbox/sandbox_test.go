package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
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
		"-p", "MemoryMax=4G", "-p", "CPUQuota=400%", "-p", "TasksMax=512",
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
		`socat TCP-LISTEN:8787,bind=127.0.0.1,fork,reuseaddr UNIX-CONNECT:/run/relay.sock & exec "$@"`,
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
	for _, text := range []string{"bwrap: setup failed\n", "Failed to start transient service unit\n", "systemd-run: failed\n"} {
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
