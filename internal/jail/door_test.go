package jail

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/jail/jailtest"
)

// These tests serve a door on a socket and talk to it as its client does, or as a stuck or hostile one would: with
// ordinary connections, no more than a few dozen of them, none of which tries to leave anything.

// stubDoor serves a door whose run is f, on a short socket path, and stops it when the test ends.
func stubDoor(t *testing.T, f func(context.Context, Request) Reply) string {
	t.Helper()
	sock := filepath.Join(jailtest.SocketDir(t), "d.sock")
	stop, err := ServeDoor(sock, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return sock
}

// shorten sets one of the door's timeouts for the test. Call it before the door starts.
func shorten(t *testing.T, p *time.Duration, d time.Duration) {
	t.Helper()
	old := *p
	*p = d
	t.Cleanup(func() { *p = old })
}

// exchange is a raw client: it sends line and returns the reply, however the door ends the connection.
func exchange(t *testing.T, sock string, line []byte) (Reply, error) {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(20 * time.Second))
	go c.Write(line) // it may fail when the door answers before it has read the whole request
	var r Reply
	err = json.NewDecoder(c).Decode(&r)
	return r, err
}

// requestOf is a request line of exactly n bytes, its newline included.
func requestOf(t *testing.T, n int) []byte {
	t.Helper()
	prefix, suffix := `{"argv":["momentum","`, `"],"dir":"/"}`+"\n"
	pad := n - len(prefix) - len(suffix)
	if pad < 0 {
		t.Fatalf("no request is %d bytes", n)
	}
	return []byte(prefix + strings.Repeat("a", pad) + suffix)
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// emfile is the error a listener returns when the process is out of descriptors.
func emfile() error {
	return &net.OpError{Op: "accept", Net: "unix", Err: os.NewSyscallError("accept", syscall.EMFILE)}
}

// flaky is a listener that returns the errors it is given before it accepts anything.
type flaky struct {
	net.Listener
	mu   sync.Mutex
	errs []error
}

func (l *flaky) Accept() (net.Conn, error) {
	l.mu.Lock()
	if len(l.errs) > 0 {
		err := l.errs[0]
		l.errs = l.errs[1:]
		l.mu.Unlock()
		return nil, err
	}
	l.mu.Unlock()
	return l.Listener.Accept()
}

// An idle connection costs the buffer it starts with and no more: a request's own length grows it. A buffer of the
// request's largest size, allocated before the first byte arrives, cost the host 65 KiB for each connection that
// only connected. A request longer than the first buffer still reads whole.
func TestAnIdleConnectionAllocatesOnlyASmallBuffer(t *testing.T) {
	const n = 200
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < n; i++ {
		readLine(strings.NewReader("")) // a connection that closes without a byte
	}
	runtime.ReadMemStats(&after)
	if per := (after.TotalAlloc - before.TotalAlloc) / n; per > 16<<10 {
		t.Errorf("an idle connection allocates %d bytes, want about the %d of the first buffer", per, firstRead)
	}
	long := strings.Repeat("a", 100<<10) + "\n"
	if got, err := readLine(strings.NewReader(long)); err != nil || string(got) != long {
		t.Errorf("a request of %d bytes read as %d bytes (%v)", len(long), len(got), err)
	}
}

// A request over MaxRequest bytes is refused and run is not called; one of exactly that size, and one well under it,
// run. The size counts the request's newline.
func TestTheDoorRefusesARequestOverItsLimitAndRunsNothing(t *testing.T) {
	var ran atomic.Int32
	sock := stubDoor(t, func(context.Context, Request) Reply { ran.Add(1); return Reply{Stdout: []byte("ran")} })
	for _, c := range []struct {
		n    int
		runs bool
	}{{MaxRequest - 4096, true}, {MaxRequest, true}, {MaxRequest + 1, false}, {MaxRequest + 4096, false}} {
		before := ran.Load()
		r, err := exchange(t, sock, requestOf(t, c.n))
		if err != nil {
			t.Fatalf("a request of %d bytes: %v", c.n, err)
		}
		if c.runs && (string(r.Stdout) != "ran" || ran.Load() != before+1) {
			t.Errorf("a request of %d bytes did not run: %+v", c.n, r)
		}
		if !c.runs && (r.Code != 126 || !strings.Contains(string(r.Stderr), "limit") || ran.Load() != before) {
			t.Errorf("a request of %d bytes is not refused, or run was called: %+v", c.n, r)
		}
	}
}

// A stream over MaxStream is cut there and ends with the marker SPEC counts; two streams of exactly MaxStream
// bytes of incompressible data arrive whole, so the client's cap covers them in base64.
func TestTheDoorCutsAReplyAtItsLimitAndCarriesTwoFullStreams(t *testing.T) {
	if len(cutMarker) != 42 {
		t.Fatalf("SPEC says the marker is 42 bytes, it is %d", len(cutMarker))
	}
	long := bytes.Repeat([]byte("x"), MaxStream+1<<20)
	whole1, whole2 := randomBytes(t, MaxStream), randomBytes(t, MaxStream)
	sock := stubDoor(t, func(_ context.Context, r Request) Reply {
		if r.Argv[1] == "long" {
			return Reply{Stdout: long, Stderr: long}
		}
		return Reply{Stdout: whole1, Stderr: whole2, Code: 3}
	})
	var out, errb bytes.Buffer
	if code := Call(sock, "momentum", []string{"long"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	for name, b := range map[string][]byte{"stdout": out.Bytes(), "stderr": errb.Bytes()} {
		if len(b) != MaxStream+len(cutMarker) || !strings.HasSuffix(string(b), cutMarker) {
			t.Errorf("%s is %d bytes, want %d and the marker at its end", name, len(b), MaxStream+len(cutMarker))
		}
	}
	out.Reset()
	errb.Reset()
	if code := Call(sock, "momentum", []string{"whole"}, &out, &errb); code != 3 || !bytes.Equal(out.Bytes(), whole1) || !bytes.Equal(errb.Bytes(), whole2) {
		t.Fatalf("two streams of %d bytes did not arrive whole (exit %d, %d and %d bytes)", MaxStream, code, out.Len(), errb.Len())
	}
}

// The socket is the owner's alone and so is the directory the door makes, whatever the umask: with umask 0 they are
// 0600 and 0700 only because the door set them.
func TestTheDoorsSocketAndItsDirectoryAreTheOwners(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	dir := filepath.Join(jailtest.SocketDir(t), "new")
	sock := filepath.Join(dir, "d.sock")
	stop, err := ServeDoor(sock, func(context.Context, Request) Reply { return Reply{} })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	st, err := os.Stat(sock)
	if err != nil || st.Mode().Perm() != 0o600 || st.Mode()&os.ModeSocket == 0 {
		t.Errorf("the socket is %v (%v), want a socket of mode 0600", st, err)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("the directory is %v (%v), want mode 0700", st, err)
	}
	stop()
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Errorf("the socket is still there after stop: %v", err)
	}
}

// watched is a listener whose connections tell the test when the door first reads from one and when it first writes
// to one: the door is then waiting on the peer for a request, or stuck in a reply the peer does not read.
type watched struct {
	net.Listener
	reading, writing    chan struct{}
	readOnce, writeOnce sync.Once
}

func newWatched(ln net.Listener) *watched {
	return &watched{Listener: ln, reading: make(chan struct{}), writing: make(chan struct{})}
}

func (l *watched) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &watchedConn{Conn: c, l: l}, nil
}

type watchedConn struct {
	net.Conn
	l *watched
}

func (c *watchedConn) Read(p []byte) (int, error) {
	c.l.readOnce.Do(func() { close(c.l.reading) })
	return c.Conn.Read(p)
}

func (c *watchedConn) Write(p []byte) (int, error) {
	c.l.writeOnce.Do(func() { close(c.l.writing) })
	return c.Conn.Write(p)
}

// stop never waits on a client: not on one that connects and sends nothing, and not on one that sent its request and
// never reads the 8 MiB reply (the door's write is stuck in it). stop is called only once the door is waiting on the
// peer - reading for a request, or writing a reply - so that it is stop that has to end the wait: before that, a
// connection the door has not accepted yet makes the case pass whether the door closes connections on stop or not.
func TestAClientThatSendsNothingOrNeverReadsDoesNotHoldStop(t *testing.T) {
	big := make([]byte, MaxStream)
	for _, c := range []struct {
		name string
		send bool
	}{{"sends nothing", false}, {"never reads its reply", true}} {
		t.Run(c.name, func(t *testing.T) {
			sock := filepath.Join(jailtest.SocketDir(t), "d.sock")
			ln, err := listenPrivate(sock)
			if err != nil {
				t.Fatal(err)
			}
			w := newWatched(ln)
			stop := serve(w, sock, func(context.Context, Request) Reply { return Reply{Stdout: big} })
			conn, err := net.Dial("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			waiting := w.reading
			if c.send {
				conn.Write(requestOf(t, 100))
				waiting = w.writing
			}
			select {
			case <-waiting:
			case <-time.After(5 * time.Second):
				conn.Close()
				stop()
				t.Fatal("the door never took up the connection")
			}
			done := make(chan struct{})
			go func() { stop(); close(done) }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("stop waits on a client")
			}
		})
	}
}

// A connection that sends nothing is answered and closed when the request's time is up, and the answer does not name
// the socket's path on the host.
func TestASilentConnectionIsDroppedAtTheReadDeadline(t *testing.T) {
	shorten(t, &requestTimeout, 200*time.Millisecond)
	sock := stubDoor(t, func(context.Context, Request) Reply { t.Error("run was called"); return Reply{} })
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	var r Reply
	if err := json.NewDecoder(c).Decode(&r); err != nil {
		t.Fatalf("the door held a silent connection %v, past its deadline: %v", time.Since(start), err)
	}
	if r.Code != 126 || !strings.Contains(string(r.Stderr), "did not arrive") {
		t.Errorf("the answer to a silent connection is %+v", r)
	}
	if strings.Contains(string(r.Stderr), filepath.Dir(sock)) {
		t.Errorf("the answer names the host's socket path: %s", r.Stderr)
	}
}

// A client that does not read its reply is dropped when the reply's time is up: the door gives up on the write, so
// what the client reads afterwards is cut short instead of whole.
func TestAClientThatDoesNotReadItsReplyIsDroppedAtTheWriteDeadline(t *testing.T) {
	shorten(t, &replyTimeout, 200*time.Millisecond)
	big := make([]byte, MaxStream)
	sock := stubDoor(t, func(context.Context, Request) Reply { return Reply{Stdout: big} })
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write(requestOf(t, 100))
	time.Sleep(time.Second) // not reading: the door's write stalls, and gives up at its deadline
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	var r Reply
	if err := json.NewDecoder(c).Decode(&r); err == nil {
		t.Fatal("the door waited for a client that was not reading, and the whole reply arrived")
	}
}

// At most MaxInFlight requests are in run at once; the next one is answered at once with a refusal, run is not
// called for it, and a slot that frees takes the next request.
func TestAtMost32RequestsAreInFlight(t *testing.T) {
	release := make(chan struct{})
	var inside atomic.Int32
	sock := stubDoor(t, func(context.Context, Request) Reply { inside.Add(1); <-release; return Reply{Stdout: []byte("ok")} })
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	defer free()
	codes := make(chan int, MaxInFlight)
	for i := 0; i < MaxInFlight; i++ {
		go func() { codes <- Call(sock, "momentum", []string{"x"}, io.Discard, io.Discard) }()
	}
	if !waitFor(10*time.Second, func() bool { return int(inside.Load()) == MaxInFlight }) {
		t.Fatalf("only %d of %d requests reached run", inside.Load(), MaxInFlight)
	}
	type answer struct {
		code int
		err  string
	}
	got := make(chan answer, 1)
	go func() {
		var errb bytes.Buffer
		got <- answer{Call(sock, "momentum", []string{"x"}, io.Discard, &errb), errb.String()}
	}()
	select {
	case a := <-got:
		if a.code != 126 || !strings.Contains(a.err, "the door is busy") {
			t.Fatalf("the request past the bound got exit %d and %q", a.code, a.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request past the bound was not answered at once")
	}
	if n := inside.Load(); n != MaxInFlight {
		t.Fatalf("run was called %d times, want %d", n, MaxInFlight)
	}
	free()
	for i := 0; i < MaxInFlight; i++ {
		select {
		case c := <-codes:
			if c != 0 {
				t.Fatalf("a request in flight ended with exit %d", c)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a request in flight was not answered once run returned")
		}
	}
	var out bytes.Buffer
	if code := Call(sock, "momentum", []string{"x"}, &out, io.Discard); code != 0 || out.String() != "ok" {
		t.Fatalf("a request after the slots freed got exit %d and %q", code, out.String())
	}
}

// An error from Accept that is not the listener's close (descriptors used up, say) does not end the loop: the
// requests that wait in the backlog are served once it accepts again.
func TestATemporaryAcceptErrorDoesNotEndTheDoorsLoop(t *testing.T) {
	sock := filepath.Join(jailtest.SocketDir(t), "d.sock")
	real, err := listenPrivate(sock)
	if err != nil {
		t.Fatal(err)
	}
	stop := serve(&flaky{Listener: real, errs: []error{emfile(), emfile(), emfile()}}, sock, func(context.Context, Request) Reply {
		return Reply{Stdout: []byte("served")}
	})
	defer stop()
	got := make(chan string, 1)
	go func() {
		var out bytes.Buffer
		Call(sock, "momentum", []string{"x"}, &out, io.Discard)
		got <- out.String()
	}()
	select {
	case s := <-got:
		if s != "served" {
			t.Fatalf("got %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the accept loop ended at its first error, and the request waits in the backlog")
	}
}

// gate is a listener whose first Accept returns its connection only when the test lets it: the connection the
// kernel handed over as stop began. Close does not unblock it, as a real close would not undo an accept that
// returned.
type gate struct {
	mu        sync.Mutex
	calls     int
	accepting chan struct{}
	release   chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
	conn      net.Conn
}

func (g *gate) Accept() (net.Conn, error) {
	g.mu.Lock()
	n := g.calls
	g.calls++
	g.mu.Unlock()
	if n == 0 {
		close(g.accepting)
		<-g.release
		return g.conn, nil
	}
	return nil, net.ErrClosed
}
func (g *gate) Close() error   { g.closeOnce.Do(func() { close(g.closed) }); return nil }
func (g *gate) Addr() net.Addr { return &net.UnixAddr{Name: "gate", Net: "unix"} }

type recorded struct {
	net.Conn
	closed atomic.Bool
}

func (r *recorded) Close() error { r.closed.Store(true); return r.Conn.Close() }

// A connection accepted as stop runs is served before stop returns: the handler's count is taken while the accept
// loop still holds its own, so Wait cannot return between them (the race detector reported the old order).
func TestStopWaitsForAConnectionAcceptedAsItRuns(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	conn := &recorded{Conn: server}
	g := &gate{accepting: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{}), conn: conn}
	stop := serve(g, "", func(context.Context, Request) Reply { return Reply{} })
	<-g.accepting // the accept loop is inside Accept
	var servedAtStop bool
	done := make(chan struct{})
	go func() { stop(); servedAtStop = conn.closed.Load(); close(done) }()
	<-g.closed // stop has ended the door's context and closed its listener, and now waits
	time.Sleep(50 * time.Millisecond)
	close(g.release) // the connection arrives as stop runs
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return")
	}
	if !servedAtStop {
		t.Fatal("stop returned before the connection it was accepting was served and closed")
	}
}

// A panic in the caller's run is answered with a refusal; the door goes on and the host process lives.
func TestAPanicInRunIsAnsweredWithARefusal(t *testing.T) {
	sock := stubDoor(t, func(_ context.Context, r Request) Reply { return Reply{Stdout: []byte(r.Argv[1])} })
	var out, errb bytes.Buffer
	if code := Call(sock, "momentum", nil, &out, &errb); code != 126 || !strings.Contains(errb.String(), "failed") {
		t.Fatalf("a panic in run got exit %d and %q", code, errb.String())
	}
	out.Reset()
	if code := Call(sock, "momentum", []string{"fine"}, &out, &errb); code != 0 || out.String() != "fine" {
		t.Fatalf("the door after a panic got exit %d and %q", code, out.String())
	}
}

// A request that names no command is refused, and run is not called: not an empty list, not an empty name, not text
// that is not a request.
func TestTheDoorRefusesAnEmptyCommand(t *testing.T) {
	var ran atomic.Int32
	sock := stubDoor(t, func(context.Context, Request) Reply { ran.Add(1); return Reply{} })
	for _, line := range []string{`{"argv":[""]}`, `{"argv":[]}`, `{}`, `[]`, `not json`, `null`, ``} {
		r, err := exchange(t, sock, []byte(line+"\n"))
		if err != nil || r.Code != 126 || !strings.Contains(string(r.Stderr), "no command") {
			t.Errorf("%q: %+v, %v", line, r, err)
		}
	}
	if ran.Load() != 0 {
		t.Errorf("run was called %d times", ran.Load())
	}
}

// stop can be called twice, and a second call does not take down a door that has started at the same path since.
func TestStopRemovesTheSocketAndCanBeCalledAgain(t *testing.T) {
	sock := filepath.Join(jailtest.SocketDir(t), "d.sock")
	stop, err := ServeDoor(sock, func(context.Context, Request) Reply { return Reply{} })
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Fatalf("the socket is still there after stop: %v", err)
	}
	stop2, err := ServeDoor(sock, func(context.Context, Request) Reply { return Reply{Stdout: []byte("second")} })
	if err != nil {
		t.Fatal(err)
	}
	defer stop2()
	stop() // the first door's stop, again
	var out bytes.Buffer
	if code := Call(sock, "momentum", []string{"x"}, &out, io.Discard); code != 0 || out.String() != "second" {
		t.Fatalf("the second door after the first one's stop got exit %d and %q", code, out.String())
	}
}

// ServeDoor refuses a path it cannot serve safely, and says why.
func TestServeDoorRefusesWhatItCannotServeSafely(t *testing.T) {
	noRun := func(context.Context, Request) Reply { return Reply{} }
	t.Run("a path that is too long", func(t *testing.T) {
		dir := filepath.Join(jailtest.SocketDir(t), strings.Repeat("a", 100))
		_, err := ServeDoor(filepath.Join(dir, "d.sock"), noRun)
		if err == nil || !strings.Contains(err.Error(), "107") {
			t.Fatalf("got %v, want an error that names the limit", err)
		}
		if _, err := os.Stat(dir); err == nil {
			t.Error("the directory was made")
		}
	})
	t.Run("a directory that is not private", func(t *testing.T) {
		for _, mode := range []os.FileMode{0o755, 0o750, 0o711, 0o770, 0o777} {
			dir := filepath.Join(jailtest.SocketDir(t), "shared")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatal(err)
			}
			_, err := ServeDoor(filepath.Join(dir, "d.sock"), noRun)
			if err == nil || !strings.Contains(err.Error(), "0700") {
				t.Errorf("a directory of mode %04o: %v", mode, err)
			}
			if _, err := os.Lstat(filepath.Join(dir, "d.sock")); err == nil {
				t.Errorf("a socket was made in a directory of mode %04o", mode)
			}
			if st, _ := os.Stat(dir); st.Mode().Perm() != mode {
				t.Errorf("the directory of mode %04o was changed to %04o", mode, st.Mode().Perm())
			}
		}
	})
	t.Run("a directory of another user", func(t *testing.T) {
		st, err := os.Stat("/root")
		if os.Geteuid() == 0 || err != nil || st.Mode().Perm() != 0o700 {
			t.Skip("this machine has no /root of mode 0700 that another user owns")
		}
		if _, err := ServeDoor("/root/d.sock", noRun); err == nil || !strings.Contains(err.Error(), "not the caller's") {
			t.Fatalf("got %v, want a refusal of a directory the caller does not own", err)
		}
	})
	t.Run("a file at its path", func(t *testing.T) {
		path := filepath.Join(jailtest.SocketDir(t), "d.sock")
		if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ServeDoor(path, noRun); err == nil || !strings.Contains(err.Error(), "not a socket") {
			t.Fatalf("got %v, want a refusal of a path that is a file", err)
		}
		if got, _ := os.ReadFile(path); string(got) != "keep" {
			t.Errorf("the file was replaced: %q", got)
		}
	})
	t.Run("a live server at its path", func(t *testing.T) {
		sock := stubDoor(t, func(context.Context, Request) Reply { return Reply{Stdout: []byte("first")} })
		if _, err := ServeDoor(sock, noRun); err == nil || !strings.Contains(err.Error(), "another process") {
			t.Fatalf("got %v, want a refusal of a path another server answers on", err)
		}
		var out bytes.Buffer
		if code := Call(sock, "momentum", []string{"x"}, &out, io.Discard); code != 0 || out.String() != "first" {
			t.Fatalf("the first server after the refusal got exit %d and %q", code, out.String())
		}
	})
	t.Run("a stale socket is replaced", func(t *testing.T) {
		path := filepath.Join(jailtest.SocketDir(t), "d.sock")
		old, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		old.SetUnlinkOnClose(false)
		old.Close() // a server that died: its socket stays, and nothing answers on it
		if st, err := os.Lstat(path); err != nil || st.Mode()&os.ModeSocket == 0 {
			t.Fatalf("no stale socket to replace: %v", err)
		}
		stop, err := ServeDoor(path, func(context.Context, Request) Reply { return Reply{Stdout: []byte("new")} })
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		var out bytes.Buffer
		if code := Call(path, "momentum", []string{"x"}, &out, io.Discard); code != 0 || out.String() != "new" {
			t.Fatalf("the replacement got exit %d and %q", code, out.String())
		}
	})
}

// Call refuses, with a message that says why, what the door would rewrite or refuse: an argument or a working
// directory that is not valid UTF-8 (JSON would turn the bad bytes into U+FFFD, and the command outside would name
// something else), and a request over the limit. It refuses before it connects, so the same message comes from a
// door that is closed, and nothing reaches a door that is open.
func TestCallRefusesWhatTheDoorWouldRewriteOrRefuse(t *testing.T) {
	var mu sync.Mutex
	var seen []Request
	open := stubDoor(t, func(_ context.Context, r Request) Reply {
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
		return Reply{}
	})
	closed := filepath.Join(jailtest.SocketDir(t), "none.sock") // nothing listens here
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"an argument that is not UTF-8", []string{"note", "name-\xff\xfe-end"}, "UTF-8"},
		{"a request over the limit", []string{strings.Repeat("a", MaxRequest)}, "over the door's limit of"},
	} {
		for _, door := range []string{open, closed} {
			var out, errb bytes.Buffer
			if code := Call(door, "momentum", c.args, &out, &errb); code != 126 || !strings.Contains(errb.String(), c.want) || strings.Contains(errb.String(), "closed") {
				t.Errorf("%s, door %s: exit %d and %q, want 126 and a message with %q", c.name, filepath.Base(door), code, errb.String(), c.want)
			}
		}
	}
	t.Run("a working directory that is not UTF-8", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "dir-\xff")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Skipf("this file system takes no name that is not UTF-8: %v", err)
		}
		t.Chdir(dir)
		for _, door := range []string{open, closed} {
			var errb bytes.Buffer
			if code := Call(door, "momentum", []string{"x"}, io.Discard, &errb); code != 126 || !strings.Contains(errb.String(), "UTF-8") {
				t.Errorf("door %s: exit %d and %q, want 126 and a message with UTF-8", filepath.Base(door), code, errb.String())
			}
		}
	})
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 0 {
		t.Errorf("the door read %d requests that Call should have refused: %+v", len(seen), seen)
	}
}

// failing is a listener whose Accept fails until a time has passed, counting the calls that failed, and then hands
// out the connections it was given.
type failing struct {
	until  time.Time
	failed atomic.Int32
	conns  chan net.Conn
	done   chan struct{}
	once   sync.Once
}

func (l *failing) Accept() (net.Conn, error) {
	if time.Now().Before(l.until) {
		l.failed.Add(1)
		return nil, emfile()
	}
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *failing) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *failing) Addr() net.Addr { return &net.UnixAddr{Name: "failing", Net: "unix"} }

// The accept loop waits between errors, as SPEC says: 5 ms at first and twice as long each time. A listener that
// fails for 100 ms is asked about 5 times (at 0, 5, 15, 35 and 75 ms), where a loop that does not wait asks it
// thousands of times, and the connection that comes after the errors is served.
func TestTheAcceptLoopWaitsBetweenErrorsAndServesOnceTheyEnd(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	l := &failing{until: time.Now().Add(100 * time.Millisecond), conns: make(chan net.Conn, 1), done: make(chan struct{})}
	l.conns <- server
	served := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan struct{})
	go func() {
		acceptLoop(ctx, l, func(c net.Conn) { c.Close(); served <- struct{}{} })
		close(ended)
	}()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Error("the loop did not serve the connection that came after the errors")
	}
	failed := l.failed.Load()
	cancel()
	l.Close()
	<-ended
	if failed < 3 || failed > 8 {
		t.Errorf("the listener was asked %d times while it failed for 100 ms, want about 5", failed)
	}
}

// SPEC states the door's numbers next to what they bound, and the code holds them in constants. A test written with
// the constants passes whatever they are, so this one reads SPEC and fails when the two disagree.
func TestSpecStatesTheNumbersTheDoorUses(t *testing.T) {
	doc, err := os.ReadFile("../../SPEC.md")
	if err != nil {
		t.Fatalf("SPEC.md is what the door's numbers are checked against: %v", err)
	}
	_, section, ok := strings.Cut(string(doc), "\n## Sandbox\n")
	if !ok {
		t.Fatal("SPEC.md has no Sandbox section")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	mib := func(name string, n int) string {
		if n%(1<<20) != 0 {
			t.Fatalf("%s is %d bytes, not a whole number of MiB", name, n)
		}
		return fmt.Sprintf("%d MiB", n>>20)
	}
	secs := func(name string, d time.Duration) string {
		if d%time.Second != 0 {
			t.Fatalf("%s is %v, not a whole number of seconds", name, d)
		}
		return fmt.Sprintf("%d s", d/time.Second)
	}
	for _, c := range []struct{ name, phrase string }{
		{"MaxRequest", "one line of at most " + mib("MaxRequest", MaxRequest)},
		{"requestTimeout", "arrives within " + secs("requestTimeout", requestTimeout)},
		{"MaxStream", "carries at most " + mib("MaxStream", MaxStream) + " of each stream"},
		{"replyTimeout", "taken within " + secs("replyTimeout", replyTimeout)},
		{"MaxInFlight", fmt.Sprintf("At most %d requests", MaxInFlight)},
		{"cutMarker", fmt.Sprintf("%d-byte marker", len(cutMarker))},
	} {
		if !strings.Contains(section, c.phrase) {
			t.Errorf("%s: SPEC's Sandbox section does not say %q", c.name, c.phrase)
		}
	}
}

// A command whose client has gone ends with it: its context is cancelled while the door keeps serving others, and a
// client that ends its request by half-closing instead of a newline keeps its command running.
func TestACommandEndsWhenItsClientGoesAway(t *testing.T) {
	started, ended := make(chan struct{}, 4), make(chan string, 4)
	sock := stubDoor(t, func(ctx context.Context, r Request) Reply {
		if r.Argv[0] == "ping" {
			return Reply{Stdout: []byte("pong")}
		}
		started <- struct{}{}
		select {
		case <-ctx.Done():
			ended <- r.Argv[0] + " cancelled"
			return Reply{}
		case <-time.After(3 * time.Second):
			ended <- r.Argv[0] + " finished"
			return Reply{Stdout: []byte("done")}
		}
	})
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(append(mustJSON(t, Request{Argv: []string{"slow"}, Dir: "/"}), '\n')); err != nil {
		t.Fatal(err)
	}
	<-started
	c.Close()
	select {
	case got := <-ended:
		if got != "slow cancelled" {
			t.Fatalf("the command ended as %q, want cancelled", got)
		}
	case <-time.After(time.Second):
		t.Fatal("the command kept running after its client closed the connection")
	}
	// the door still serves another client
	r, err := exchange(t, sock, append(mustJSON(t, Request{Argv: []string{"ping"}, Dir: "/"}), '\n'))
	if err != nil || string(r.Stdout) != "pong" {
		t.Fatalf("the door stopped serving: %q, %v", r.Stdout, err)
	}
	// a request ended by half-closing, with no newline, is not a client that has gone
	c2, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	c2.Write(mustJSON(t, Request{Argv: []string{"halfclosed"}, Dir: "/"}))
	c2.(*net.UnixConn).CloseWrite()
	<-started
	select {
	case got := <-ended:
		if got != "halfclosed finished" {
			t.Fatalf("a half-closed request ended as %q, want it to run to its end", got)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("the half-closed request never ended")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
