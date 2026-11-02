package jail

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// Request is one command a sandboxed program asks the door to run: momentum's or tacit's, as typed inside.
type Request struct {
	Argv []string `json:"argv"` // the program's name (momentum or tacit) and its arguments
	Dir  string   `json:"dir"`  // the working directory it was typed in, inside
}

// Reply is what the command printed and how it ended.
type Reply struct {
	Stdout []byte `json:"stdout"`
	Stderr []byte `json:"stderr"`
	Code   int    `json:"code"`
}

// Refuse is a reply that ran nothing: the door's caller does not allow the command.
func Refuse(format string, a ...any) Reply {
	return Reply{Stderr: []byte(fmt.Sprintf(format, a...) + "\n"), Code: 126}
}

// The door's bounds. The program in the sandbox is the untrusted peer of a server that runs in the host process, so
// the server bounds what it reads, what it writes, how long it waits and how many it serves; SPEC (Sandbox) states
// the same numbers.
const (
	// MaxRequest is the most bytes of a request, its newline included: one line of JSON. A longer one is refused
	// and nothing runs.
	MaxRequest = 1 << 20
	// MaxStream is the most of each stream a reply carries. A longer stream is cut there and ends with a marker.
	MaxStream = 8 << 20
	// MaxInFlight is the most requests the door holds at once, a connection counting from accept to its reply; the
	// rest are answered at once with a refusal. A run and its subagents type a handful of commands at once, so 32 is
	// several times what they need, and each request in flight holds a descriptor, a goroutine and up to about
	// 40 MiB for a full reply (two streams of 8 MiB, and their base64 form), which the host shares with the store and
	// every other run.
	MaxInFlight = 32
)

const (
	// cutMarker ends a stream that was cut at MaxStream. SPEC states its length (42 bytes).
	cutMarker = "\n[cut: the rest is over the door's limit]\n"
	// firstRead is the buffer a connection starts with, grown up to MaxRequest only as the request arrives: an idle
	// connection costs a descriptor and a goroutine, not a request's worth of memory.
	firstRead = 4 << 10
	// replyLimit is what the client reads of a reply: two streams of MaxStream bytes, each 4/3 as long as base64,
	// are about 21.4 MiB, which 3 * MaxStream (24 MiB) covers with the rest of the JSON.
	replyLimit = 3 * MaxStream
	// busyTimeout bounds the one small write that refuses a request when the door is full.
	busyTimeout = time.Second
	// dialTimeout is how long the client waits to connect. The door is a local socket that answers at once, so a
	// server that has not accepted in 5 seconds is stuck. The client sets no deadline on the reply: a command may
	// take long, and the loop ends the whole sandbox at the run's time.
	dialTimeout = 5 * time.Second
	// maxSocketPath is the longest path of a Unix socket: sun_path holds 108 bytes and one of them ends the string.
	maxSocketPath = 107
)

// These two are variables only so that a test can shorten them.
var (
	// requestTimeout is how long a connection has to send its request. The client writes its one line at once, so
	// a longer wait is a stuck peer.
	requestTimeout = 10 * time.Second
	// replyTimeout is how long a connection has to take its reply. 8 MiB over a Unix socket takes milliseconds when
	// the peer reads, so a peer that has not read in 30 seconds is not reading.
	replyTimeout = 30 * time.Second
)

// ServeDoor serves a sandbox's door on a Unix socket at path and returns the function that stops it: each
// connection is one request, which run decides and runs, and its reply. run sees only the request; what it runs,
// with what environment and where, is the caller's. run must return when its context ends: stop cancels it and
// waits for every request, but never for a client.
//
// The socket is the owner's alone (mode 0600). Its directory is made with mode 0700 when it does not exist; one
// that does must be the caller's and 0700, or ServeDoor refuses it. A socket already at path is replaced only if
// nothing answers on it; any other file there, or a live server, is refused.
//
// A request has requestTimeout to arrive and at most MaxRequest bytes; a reply has replyTimeout to be taken and at
// most MaxStream bytes of each stream; at most MaxInFlight requests are served at once and the rest are refused
// with exit code 126, "the door is busy". A command that is empty, or that run panics on, is refused too.
func ServeDoor(path string, run func(context.Context, Request) Reply) (stop func(), err error) {
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf("jail: the door's socket path is %d bytes and a Unix socket's has at most %d: %s",
			len(path), maxSocketPath, path)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	if err := removeStale(path); err != nil {
		return nil, err
	}
	ln, err := listenPrivate(path)
	if err != nil {
		return nil, fmt.Errorf("jail: the door: %w", err)
	}
	return serve(ln, path, run), nil
}

// serve serves the door on ln, and removes the socket at path when stopped.
func serve(ln net.Listener, path string, run func(context.Context, Request) Reply) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	slots := make(chan struct{}, MaxInFlight)
	// The accept loop holds a count of its own, so a request's wg.Add never runs against a zero count that stop is
	// waiting on: a connection accepted as stop runs is served before stop returns.
	wg.Add(1)
	go func() {
		defer wg.Done()
		acceptLoop(ctx, ln, func(c net.Conn) {
			select {
			case slots <- struct{}{}:
			default:
				refuseBusy(c)
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-slots }()
				serveOne(ctx, c, run)
			}()
		})
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			ln.Close()
			wg.Wait()
			os.Remove(path)
		})
	}
}

// acceptLoop calls handle with each connection ln accepts, until ln is closed or ctx ends. Any other error ends
// nothing: running out of descriptors, say, passes when a connection closes, so the loop waits and accepts again,
// 5 ms at first and twice as long each time up to 1 s, as net/http's does.
func acceptLoop(ctx context.Context, ln net.Listener, handle func(net.Conn)) {
	var delay time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			select {
			case <-time.After(delay):
				continue
			case <-ctx.Done():
				return
			}
		}
		delay = 0
		handle(c)
	}
}

// refuseBusy answers a connection the door has no room for, without reading it.
func refuseBusy(c net.Conn) {
	defer c.Close()
	c.SetWriteDeadline(time.Now().Add(busyTimeout))
	json.NewEncoder(c).Encode(Refuse("momentum: the door is busy: %d commands are running", MaxInFlight))
}

func serveOne(ctx context.Context, c net.Conn, run func(context.Context, Request) Reply) {
	defer c.Close()
	// When the door's context ends the connection closes under whatever read or write holds it, so stop never waits
	// on a peer.
	defer context.AfterFunc(ctx, func() { c.Close() })()
	reply := answer(ctx, c, run)
	reply.Stdout, reply.Stderr = clip(reply.Stdout), clip(reply.Stderr)
	c.SetWriteDeadline(time.Now().Add(replyTimeout))
	json.NewEncoder(c).Encode(reply)
}

func answer(ctx context.Context, c net.Conn, run func(context.Context, Request) Reply) Reply {
	c.SetReadDeadline(time.Now().Add(requestTimeout))
	line, err := readLine(c)
	c.SetReadDeadline(time.Time{})
	var req Request
	// A refusal never quotes the read's error: it names the socket's path on the host, which the sandbox must not learn.
	switch {
	case errors.Is(err, errTooLong):
		return Refuse("momentum: the request is over the door's limit of %d bytes", MaxRequest)
	case errors.Is(err, os.ErrDeadlineExceeded):
		return Refuse("momentum: the request did not arrive within %v", requestTimeout)
	case err != nil:
		return Refuse("momentum: the door read no request")
	case json.Unmarshal(line, &req) != nil || len(req.Argv) == 0 || req.Argv[0] == "":
		return Refuse("momentum: the door read no command")
	}
	return safely(ctx, run, req)
}

var errTooLong = errors.New("request over the limit")

// readLine reads the request: one line of at most MaxRequest bytes, or fewer when the peer half-closes first. It
// reads one byte more than the limit, which is how a request over it is told from one that fits.
func readLine(r io.Reader) ([]byte, error) {
	line, err := bufio.NewReaderSize(io.LimitReader(r, MaxRequest+1), firstRead).ReadBytes('\n')
	switch {
	case len(line) > MaxRequest:
		return nil, errTooLong
	case err != nil && !errors.Is(err, io.EOF):
		return nil, err
	}
	return line, nil
}

// safely runs the caller's run. A panic in it is answered with a refusal and does not end the host process; the
// host's stderr names the command's program (not its arguments) and the panic.
func safely(ctx context.Context, run func(context.Context, Request) Reply, req Request) (reply Reply) {
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintf(os.Stderr, "momentum: the door's command %q panicked: %v\n", req.Argv[0], p)
			reply = Refuse("momentum: the door failed on this command")
		}
	}()
	return run(ctx, req)
}

func clip(b []byte) []byte {
	if len(b) > MaxStream {
		return append(b[:MaxStream:MaxStream], cutMarker...)
	}
	return b
}

// privateDir checks that dir is a directory of the caller's that only the caller can enter, since a socket in a
// directory others can enter is theirs to connect to whatever its own mode is.
func privateDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	sys, _ := st.Sys().(*syscall.Stat_t)
	switch {
	case !st.IsDir():
		return fmt.Errorf("jail: the door's directory %s is not a directory", dir)
	case sys == nil || int(sys.Uid) != os.Geteuid():
		return fmt.Errorf("jail: the door's directory %s is not the caller's", dir)
	case st.Mode().Perm() != 0o700:
		return fmt.Errorf("jail: the door's directory %s has mode %04o, and only 0700 is private", dir, st.Mode().Perm())
	}
	return nil
}

// removeStale clears path of a socket that nothing answers on, such as one a server that died left. Anything else
// stays and is refused: a file or directory, a server that is alive, a socket it cannot tell about.
func removeStale(path string) error {
	st, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case st.Mode()&os.ModeSocket == 0:
		return fmt.Errorf("jail: the door's path %s exists and is not a socket", path)
	}
	c, err := net.DialTimeout("unix", path, time.Second)
	switch {
	case err == nil:
		c.Close()
		return fmt.Errorf("jail: the door's path %s is served by another process", path)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case !errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("jail: the door's path %s: %w", path, err)
	}
	return os.Remove(path)
}

// listenPrivate binds a Unix socket at path, makes it the owner's alone, and only then listens. A socket that is
// bound and not yet listening takes no connection, so none gets in with the mode the umask gave it.
func listenPrivate(path string) (net.Listener, error) {
	// close-on-exec is set under the fork lock, so a process started meanwhile does not inherit the socket and keep it
	// open after the door stops (the one flag SOCK_CLOEXEC would set, on systems that have it)
	syscall.ForkLock.RLock()
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, os.NewSyscallError("socket", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		syscall.Close(fd)
		return nil, os.NewSyscallError("bind", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		syscall.Close(fd)
		os.Remove(path)
		return nil, err
	}
	if err := syscall.Listen(fd, syscall.SOMAXCONN); err != nil {
		syscall.Close(fd)
		os.Remove(path)
		return nil, os.NewSyscallError("listen", err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	ln, err := net.FileListener(f)
	if err != nil {
		os.Remove(path)
		return nil, err
	}
	return ln, nil
}

// Call is the door's client, the momentum and tacit commands inside a sandbox: it sends name and args to the door
// at socket, writes what the command printed, and returns its exit code. It refuses, with exit code 126 and a
// message that says why, an argument that is not valid UTF-8 (JSON would rewrite it, and the command outside would
// name something else) and a request over MaxRequest bytes.
func Call(socket, name string, args []string, stdout, stderr io.Writer) int {
	dir, _ := os.Getwd()
	line, err := encodeRequest(Request{Argv: append([]string{name}, args...), Dir: dir})
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 126
	}
	c, err := net.DialTimeout("unix", socket, dialTimeout)
	if err != nil {
		fmt.Fprintf(stderr, "%s: the sandbox's door is closed: %v\n", name, err)
		return 126
	}
	defer c.Close()
	// A door that is busy refuses without reading, and may have closed before the request was written, so the reply
	// is read even when the write failed. There is no deadline on it: see dialTimeout.
	_, werr := c.Write(line)
	var reply Reply
	if err := json.NewDecoder(io.LimitReader(c, replyLimit)).Decode(&reply); err != nil {
		if werr != nil {
			err = werr
		}
		fmt.Fprintf(stderr, "%s: the sandbox's door gave no reply: %v\n", name, err)
		return 126
	}
	stdout.Write(reply.Stdout)
	stderr.Write(reply.Stderr)
	return reply.Code
}

// encodeRequest is the request as the door reads it: one line of JSON.
func encodeRequest(r Request) ([]byte, error) {
	if !utf8.ValidString(r.Dir) {
		return nil, fmt.Errorf("the working directory is not valid UTF-8, and the door carries text: %.40q", r.Dir)
	}
	for i, a := range r.Argv {
		if !utf8.ValidString(a) {
			return nil, fmt.Errorf("argument %d is not valid UTF-8, and the door carries text: %.40q", i, a)
		}
	}
	line, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	line = append(line, '\n')
	if len(line) > MaxRequest {
		return nil, fmt.Errorf("the command is %d bytes, over the door's limit of %d", len(line), MaxRequest)
	}
	return line, nil
}
