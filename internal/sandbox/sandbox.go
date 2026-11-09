// Package sandbox runs a fixed bubblewrap layout in a transient systemd user service.
// The caller prepares the sources and bounds the output. Run owns the unit's lifetime.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	RelayPort         = 8787
	DefaultMemory     = "4G"
	DefaultCPUQuota   = "400%"
	DefaultTasks      = 512
	DefaultRuntimeSec = 1800

	// relayCommand starts the relay's forwarder and runs the program only once it listens, so a harness's first request
	// never meets a forwarder still starting. The probe reads the sandbox's own socket table (8787 is 2253 in hex, 0A
	// is LISTEN) and opens no connection to the relay. It gives up when the forwarder has exited, or after 200 probes
	// 50 ms apart (about 10 s): the run then ends before the program, with relayNotListening and the reason on stderr.
	// socat listens within a probe or two; the bound is for a host too loaded to start it, and still ends a broken
	// forwarder long before a run's own limits.
	relayCommand = `socat TCP-LISTEN:8787,bind=127.0.0.1,fork,reuseaddr UNIX-CONNECT:/run/relay.sock & i=0; ` +
		`until grep -qs ':2253 [0-9A-F]*:0000 0A ' /proc/net/tcp /proc/net/tcp6; do ` +
		`kill -0 $! 2>/dev/null || { echo 'sandbox: the relay forwarder exited before it listened' >&2; exit 125; }; ` +
		`i=$((i+1)); [ $i -lt 200 ] || { echo 'sandbox: the relay forwarder did not listen in 200 probes 50 ms apart' >&2; exit 125; }; ` +
		`sleep 0.05; done; exec "$@"`
	// relayNotListening is the exit status of a relay run whose forwarder never listened: the program did not run.
	relayNotListening = 125
	// A stop normally completes at once. Allow systemd's default 90-second
	// stop timeout and some time for the client to observe unit deactivation.
	stopTimeout = 2 * time.Minute
	// killGrace is how long a queued stop has to end the client before the unit's processes are killed.
	killGrace = 500 * time.Millisecond
)

var (
	unitPattern = regexp.MustCompile(`^momentum-run-[a-z0-9-]+$`)
	envPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// prefixPattern is what StopLeftover accepts: the reserved prefix and a name that ends in a hyphen.
	prefixPattern = regexp.MustCompile(`^momentum-run-([a-z0-9]+-)+$`)
)

type Bind struct {
	Src, Dst string
	Write    bool
}

// Zero fields take the defaults. Nonzero fields are systemd property values.
type Limits struct {
	Memory, CPUQuota  string
	Tasks, RuntimeSec int
}

type Spec struct {
	Unit, Host string
	Binds      []Bind
	Relay      string
	Env        map[string]string
	Dir        string
	Limits     Limits
	Argv       []string
}

// Result separates an observed launcher failure from the program's exit status.
// Failed is diagnostic, not an authentication boundary: program stderr can imitate
// the launcher's diagnostics, and some systemd failures have no distinct message.
type Result struct {
	Code   int
	Failed bool
}

// Command builds the fixed argv without writing files. Run creates the identity
// files and stops the unit on cancellation. A caller using Command directly must
// prepare those files and own unit cleanup itself.
func (s Spec) Command(ctx context.Context) (*exec.Cmd, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	l := s.Limits.defaults()
	args := []string{
		"--user", "--wait", "--pipe", "--collect", "--quiet",
		"--expand-environment=no", "--unit=" + s.Unit,
		"-p", "MemoryMax=" + l.Memory,
		"-p", "MemorySwapMax=0",
		"-p", "CPUQuota=" + l.CPUQuota,
		"-p", "TasksMax=" + strconv.Itoa(l.Tasks),
		"-p", "RuntimeMaxSec=" + strconv.Itoa(l.RuntimeSec),
		"-p", "NoNewPrivileges=yes",
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
	}
	for _, b := range s.Binds {
		flag := "--ro-bind"
		if b.Write {
			flag = "--bind"
		}
		args = append(args, flag, b.Src, b.Dst)
	}
	if s.Relay != "" {
		args = append(args, "--ro-bind", s.Relay, "/run/relay.sock")
	}
	args = append(args, "--chdir", s.Dir)
	env := map[string]string{
		"HOME": "/home/agent", "PATH": "/usr/bin:/bin",
		"LANG": "C.UTF-8", "TMPDIR": "/tmp",
	}
	for name, value := range s.Env {
		env[name] = value
	}
	// Keep the four defaults in the reference order; sort added names.
	names := []string{"HOME", "PATH", "LANG", "TMPDIR"}
	var added []string
	for name := range s.Env {
		if name != "HOME" && name != "PATH" && name != "LANG" && name != "TMPDIR" {
			added = append(added, name)
		}
	}
	sort.Strings(added)
	for _, name := range append(names, added...) {
		args = append(args, "--setenv", name, env[name])
	}
	if s.Relay != "" {
		args = append(args, "/bin/sh", "-c", relayCommand, "sh")
	}
	args = append(args, s.Argv...)
	return exec.CommandContext(ctx, "systemd-run", args...), nil
}

// Run returns program exits in Result, including nonzero exits, without an error.
// Validation, startup, output-copy and cleanup errors return an error and Failed.
// Neither stdin nor the caller's environment is forwarded into the sandbox.
func (s Spec) Run(ctx context.Context, stdout, stderr io.Writer) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{Code: -1, Failed: true}, err
	}
	// Cancel the unit before ending the systemd-run client, not the other way round.
	cmd, err := s.Command(context.WithoutCancel(ctx))
	if err != nil {
		return Result{Code: -1, Failed: true}, err
	}
	// Two live runs with one name would stop each other: a cancelled second start stops the first run's unit.
	if inUse, err := unitLoaded(ctx, s.Unit); err != nil {
		return Result{Code: -1, Failed: true}, err
	} else if inUse {
		return Result{Code: -1, Failed: true}, fmt.Errorf("sandbox: unit %s is already in use", s.Unit)
	}
	remove, err := s.identity()
	if err != nil {
		return Result{Code: -1, Failed: true}, err
	}
	defer remove()
	return runCommand(ctx, s.Unit, cmd, stdout, stderr)
}

func runCommand(ctx context.Context, unit string, cmd *exec.Cmd, stdout, stderr io.Writer) (Result, error) {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	// The first failing write by an output writer ends the run as a cancellation does, through the same stop path:
	// a caller that bounds its writers by failing them expects the run to end, not to go on writing nowhere.
	stopCtx, stopNow := context.WithCancel(ctx)
	defer stopNow()
	// One lock serves both streams: os/exec copies them in two goroutines, and serializes the writes only when they go
	// to the same writer, which these wrappers hide. A caller may pass one writer for both.
	var writes writeFailure
	var order sync.Mutex
	diagnostic := &failureWriter{dst: &failSafeWriter{w: stderr, f: &writes, stop: stopNow, mu: &order}}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, &failSafeWriter{w: stdout, f: &writes, stop: stopNow, mu: &order}, diagnostic
	// Close inherited output pipes if a broken manager leaves descendants alive
	// after the client exits. Normal unit cleanup finishes before this delay.
	cmd.WaitDelay = stopTimeout
	if err := cmd.Start(); err != nil {
		return Result{Code: -1, Failed: true}, fmt.Errorf("sandbox: start launcher: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr, cleanupErr error
	select {
	case waitErr = <-done:
	case <-stopCtx.Done():
		cleanupCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
		// A stop can race the unit's creation. A SIGTERM that reaches bubblewrap while it is still setting up its
		// namespaces can leave the program running (measured: a 30 s sleep ran to its end after a stop 20 ms in), and a
		// blocking stop waits for that. So the stop is queued without waiting, and a SIGKILL to every process of the
		// unit follows when the client has not ended within a short grace; both are issued again, every 100 ms, until
		// the client has ended or the deadline passes.
		for ended, stopErr := false, error(nil); !ended; {
			stopErr = errors.Join(stopUnit(cleanupCtx, unit))
			select {
			case waitErr = <-done:
				ended = true
				continue
			case <-cleanupCtx.Done():
			case <-time.After(killGrace):
				stopErr = errors.Join(stopErr, killUnit(cleanupCtx, unit))
				select {
				case waitErr = <-done:
					ended = true
					continue
				case <-cleanupCtx.Done():
				case <-time.After(100 * time.Millisecond):
					continue
				}
			}
			// Do not leave a client or its output goroutines behind if the user manager is broken. Report the
			// cleanup failure; RuntimeMaxSec still applies.
			cleanupErr = errors.Join(stopErr, errors.New("sandbox: unit cleanup timed out"))
			_ = cmd.Process.Kill()
			waitErr = <-done
			ended = true
		}
		r := result(waitErr, diagnostic.failed)
		r.Failed = cleanupErr != nil || writes.err() != nil
		reason := ctx.Err()
		if werr := writes.err(); werr != nil {
			reason = errors.Join(reason, fmt.Errorf("sandbox: write output: %w", werr))
		}
		return r, errors.Join(reason, cleanupErr)
	}
	r := result(waitErr, diagnostic.failed)
	if werr := writes.err(); werr != nil {
		r.Failed = true
		return r, fmt.Errorf("sandbox: write output: %w", werr)
	}
	var exit *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exit) {
		// The client itself failed (not the program): make sure no unit is left.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
		cleanupErr = stopUnit(cleanupCtx, unit)
		r.Failed = true
		return r, errors.Join(fmt.Errorf("sandbox: wait for launcher: %w", waitErr), cleanupErr)
	}
	return r, nil
}

// writeFailure keeps the first error an output writer returned.
type writeFailure struct {
	mu sync.Mutex
	e  error
}

func (f *writeFailure) set(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.e == nil {
		f.e = err
	}
}

func (f *writeFailure) err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.e
}

// failSafeWriter passes writes on, and on the first failure records it and stops the run.
type failSafeWriter struct {
	w    io.Writer
	f    *writeFailure
	stop func()
	mu   *sync.Mutex
}

func (w *failSafeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.w.Write(p)
	w.mu.Unlock()
	if err != nil {
		w.f.set(err)
		w.stop()
	}
	return n, err
}

func result(err error, failed bool) Result {
	r := Result{Code: 0}
	if err != nil {
		r.Code = -1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			r.Code = exit.ExitCode()
			if r.Code < 0 {
				if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					r.Code = 128 + int(status.Signal())
				}
			}
		}
	}
	r.Failed = failed && r.Code != 0
	return r
}

// unitLoaded says whether the user manager holds a unit of that name: a running run's, or one left by a dead caller.
func unitLoaded(ctx context.Context, unit string) (bool, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "--user", "show", "--property=LoadState", "--value", unit+".service").Output()
	if err != nil {
		return false, fmt.Errorf("sandbox: ask the user manager about %s: %w", unit, err)
	}
	return strings.TrimSpace(string(out)) != "not-found", nil
}

// stopUnit queues the unit's stop (SIGTERM, then SIGKILL after systemd's own timeout) and returns without waiting.
func stopUnit(ctx context.Context, unit string) error {
	cmd := exec.CommandContext(ctx, "systemctl", "--user", "stop", "--no-block", unit+".service")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	return cmd.Run()
}

// killUnit sends SIGKILL to every process of the unit, the namespace's init included.
func killUnit(ctx context.Context, unit string) error {
	cmd := exec.CommandContext(ctx, "systemctl", "--user", "kill", "--kill-whom=all", "--signal=SIGKILL", unit+".service")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	return cmd.Run()
}

// StopLeftover is for startup, before the caller begins any new runs. It stops the loaded services whose names begin
// with prefix, inactive and failed ones included, and no other: each kind of caller names its units with a prefix of
// its own (momentum-run-loop-, momentum-run-eval-), so one caller's startup never stops another's runs. The prefix is
// momentum-run- followed by words of [a-z0-9]+ that each end in a hyphen.
func StopLeftover(ctx context.Context, prefix string) error {
	if !prefixPattern.MatchString(prefix) {
		return fmt.Errorf("sandbox: leftover prefix %q is not momentum-run- and a name ending in a hyphen", prefix)
	}
	cmd := exec.CommandContext(ctx, "systemctl", "--user", "list-units",
		"--all", "--type=service", "--plain", "--no-legend", "--no-pager", prefix+"*.service")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("sandbox: list leftover units: %w", err)
	}
	var units []string
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		// Cleanup also covers stale names from earlier callers. Spec's stricter
		// name validation applies to new launches, not the reserved prefix.
		if !strings.HasPrefix(fields[0], prefix) || !strings.HasSuffix(fields[0], ".service") {
			return errors.New("sandbox: unexpected unit in leftover list")
		}
		units = append(units, fields[0])
	}
	if len(units) == 0 {
		return nil
	}
	cmd = exec.CommandContext(ctx, "systemctl", append([]string{"--user", "stop"}, units...)...)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sandbox: stop leftover units: %w", err)
	}
	return nil
}

// Available probes the actual layout in a real unit, with a 20-second deadline.
// Pass true to also require socat for a relay run. No argument probes a direct run.
func Available(withRelay ...bool) error {
	for _, program := range []string{"systemd-run", "systemctl", "bwrap"} {
		if _, err := exec.LookPath(program); err != nil {
			return fmt.Errorf("sandbox: %s is unavailable", program)
		}
	}
	relay := len(withRelay) != 0 && withRelay[0]
	if relay {
		if _, err := exec.LookPath("socat"); err != nil {
			return errors.New("sandbox: socat is unavailable for the relay")
		}
	}
	host, err := os.MkdirTemp("", "sandbox-probe-")
	if err != nil {
		return fmt.Errorf("sandbox: probe directory: %w", err)
	}
	defer os.RemoveAll(host)
	// A TMPDIR that goes through a link makes a host path the sources rule refuses: the probe uses the real one.
	if real, err := filepath.EvalSymlinks(host); err == nil {
		host = real
	}
	s := Spec{Unit: "momentum-run-probe-" + filepath.Base(host), Host: host, Dir: "/", Argv: []string{"/bin/true"}}
	// MkdirTemp's suffix consists of decimal digits.
	if relay {
		// The real probe checks socat in the fixed /usr mount and PATH.
		// It does not need to listen or connect to a relay.
		s.Argv = []string{"/bin/sh", "-c", "command -v socat >/dev/null"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var output limitedOutput
	r, err := s.Run(ctx, io.Discard, &output)
	if err != nil {
		return fmt.Errorf("sandbox: real unit probe: %w", err)
	}
	if r.Code != 0 || r.Failed {
		return fmt.Errorf("sandbox: real unit probe failed (exit %d): %s", r.Code, strings.TrimSpace(output.text))
	}
	return nil
}

func (l Limits) defaults() Limits {
	if l.Memory == "" {
		l.Memory = DefaultMemory
	}
	if l.CPUQuota == "" {
		l.CPUQuota = DefaultCPUQuota
	}
	if l.Tasks == 0 {
		l.Tasks = DefaultTasks
	}
	if l.RuntimeSec == 0 {
		l.RuntimeSec = DefaultRuntimeSec
	}
	return l
}

func (s Spec) validate() error {
	if !unitPattern.MatchString(s.Unit) {
		return errors.New("sandbox: unit must be momentum-run- followed by lowercase letters, digits or dashes")
	}
	if len(s.Argv) == 0 || s.Argv[0] == "" || strings.HasPrefix(s.Argv[0], "-") {
		return errors.New("sandbox: a program is required")
	}
	for _, arg := range s.Argv {
		if strings.ContainsRune(arg, 0) {
			return errors.New("sandbox: a program argument contains NUL")
		}
	}
	if !cleanAbsolute(s.Dir) {
		return errors.New("sandbox: working directory must be absolute and clean")
	}
	if err := privateDirectory(s.Host); err != nil {
		return err
	}
	l := s.Limits.defaults()
	if l.Tasks < 1 || l.RuntimeSec < 1 {
		return errors.New("sandbox: tasks and runtime must be positive")
	}
	// Properties are separate argv entries, but reject control characters too.
	for _, value := range []string{l.Memory, l.CPUQuota} {
		if strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("sandbox: invalid resource property")
		}
	}
	for name, value := range s.Env {
		if !envPattern.MatchString(name) {
			return fmt.Errorf("sandbox: invalid environment name %q", name)
		}
		if strings.ContainsRune(value, 0) {
			return fmt.Errorf("sandbox: environment variable %s contains NUL", name)
		}
	}
	var destinations []string
	for _, b := range s.Binds {
		if !cleanAbsolute(b.Dst) || !(b.Dst == "/work" || b.Dst == "/out" || b.Dst == "/run/input" ||
			strings.HasPrefix(b.Dst, "/ro/") || strings.HasPrefix(b.Dst, "/opt/")) {
			return fmt.Errorf("sandbox: destination %q is not allowed", b.Dst)
		}
		for _, previous := range destinations {
			if containsPath(previous, b.Dst) || containsPath(b.Dst, previous) {
				return errors.New("sandbox: bind destinations must not overlap")
			}
		}
		destinations = append(destinations, b.Dst)
		if err := source(b.Src); err != nil {
			return err
		}
	}
	if s.Relay != "" {
		if !cleanAbsolute(s.Relay) {
			return errors.New("sandbox: relay path must be absolute and clean")
		}
		st, err := os.Lstat(s.Relay)
		if err != nil {
			return fmt.Errorf("sandbox: relay: %w", err)
		}
		if st.Mode()&os.ModeSocket == 0 || st.Mode().Perm() != 0600 || !owned(st) {
			return errors.New("sandbox: relay must be a socket owned by the caller with mode 0600")
		}
	}
	return nil
}

func cleanAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, 0)
}

func containsPath(parent, child string) bool {
	return parent == child || strings.HasPrefix(child, parent+"/")
}

func owned(st os.FileInfo) bool {
	stat, ok := st.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}

func privateDirectory(path string) error {
	if !cleanAbsolute(path) {
		return errors.New("sandbox: Host must be absolute and clean")
	}
	if err := resolved(path); err != nil {
		return fmt.Errorf("sandbox: Host: %w", err)
	}
	st, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("sandbox: Host: %w", err)
	}
	if !st.IsDir() || st.Mode().Perm() != 0700 || !owned(st) {
		return errors.New("sandbox: Host must be a directory owned by the caller with mode 0700")
	}
	return nil
}

// resolved says a path goes through no link: its parents are as real as its last element. Callers pass paths they
// resolved themselves (filepath.EvalSymlinks), so what a check saw is what bubblewrap mounts.
func resolved(path string) error {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if real != path {
		return errors.New("the path goes through a symbolic link")
	}
	return nil
}

func source(path string) error {
	if !cleanAbsolute(path) {
		return errors.New("sandbox: source must be absolute and clean")
	}
	if err := resolved(path); err != nil {
		return fmt.Errorf("sandbox: source: %w", err)
	}
	st, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("sandbox: source: %w", err)
	}
	if !st.IsDir() && !st.Mode().IsRegular() {
		return errors.New("sandbox: source must be a regular file or directory, not a link or socket")
	}
	if !st.IsDir() {
		return nil
	}
	return filepath.WalkDir(path, func(name string, _ fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("sandbox: walk source: %w", err)
		}
		// Lstat each entry, including mount points. DirEntry.Type alone can
		// describe the covered inode of a socket mount instead of its source.
		info, err := os.Lstat(name)
		if err != nil {
			return fmt.Errorf("sandbox: inspect source: %w", err)
		}
		if info.Mode()&os.ModeSocket != 0 {
			return errors.New("sandbox: source directory holds a socket")
		}
		return nil
	})
}

func (s Spec) identity() (func(), error) {
	var created []string
	remove := func() {
		for _, path := range created {
			_ = os.Remove(path)
		}
	}
	for _, item := range []struct{ name, text string }{
		{"passwd", fmt.Sprintf("agent:x:%d:%d::/home/agent:/bin/sh\n", os.Getuid(), os.Getgid())},
		{"group", fmt.Sprintf("agent:x:%d:\n", os.Getgid())},
	} {
		path := filepath.Join(s.Host, item.name)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			remove()
			return nil, fmt.Errorf("sandbox: create identity file: %w", err)
		}
		created = append(created, path)
		_, writeErr := io.WriteString(file, item.text)
		closeErr := file.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			remove()
			return nil, fmt.Errorf("sandbox: write identity file: %w", err)
		}
	}
	return remove, nil
}

// Match diagnostic prefixes without retaining stderr bytes. The caller's writer
// receives the original bytes and is responsible for bounding them.
type failureWriter struct {
	dst      io.Writer
	position int
	possible uint8
	failed   bool
}

func (w *failureWriter) Write(p []byte) (int, error) {
	marks := [...]string{"bwrap:", "systemd-run:", "Failed to ", "Job for "}
	for _, b := range p {
		if b == '\n' {
			w.position = 0
			continue
		}
		if w.position == 0 {
			w.possible = 0x0f
		}
		for i, mark := range marks {
			bit := uint8(1 << i)
			if w.possible&bit != 0 && w.position < len(mark) {
				if mark[w.position] != b {
					w.possible &^= bit
				} else if w.position+1 == len(mark) {
					w.failed = true
				}
			}
		}
		// Longer lines cannot add a match; keep the counter bounded.
		w.position = min(w.position+1, len("systemd-run:"))
	}
	return w.dst.Write(p)
}

// Only the fixed, credential-free availability probe uses a diagnostic buffer.
type limitedOutput struct{ text string }

func (w *limitedOutput) Write(p []byte) (int, error) {
	if left := 2048 - len(w.text); left > 0 {
		w.text += string(p[:min(left, len(p))])
	}
	return len(p), nil
}
