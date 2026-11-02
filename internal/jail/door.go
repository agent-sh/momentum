package jail

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
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

// The largest request the door reads, and the most of each stream a reply carries.
const (
	MaxRequest = 1 << 20
	MaxStream  = 8 << 20
)

// ServeDoor serves a sandbox's door on a Unix socket at path, which it makes (only its owner may connect) and
// removes when stop is called: each connection is one request, which run decides and runs, and its reply. run sees
// only the request; what it runs, with what environment and where, is the caller's.
func ServeDoor(path string, run func(context.Context, Request) Reply) (stop func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("jail: the door: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer c.Close()
				serveOne(ctx, c, run)
			}(c)
		}
	}()
	return func() {
		cancel()
		ln.Close()
		wg.Wait()
		os.Remove(path)
	}, nil
}

func serveOne(ctx context.Context, c net.Conn, run func(context.Context, Request) Reply) {
	var req Request
	line, err := bufio.NewReaderSize(io.LimitReader(c, MaxRequest), 64<<10).ReadBytes('\n')
	var reply Reply
	switch {
	case err != nil && !errors.Is(err, io.EOF):
		reply = Refuse("momentum: the door read no request: %v", err)
	case json.Unmarshal(line, &req) != nil || len(req.Argv) == 0:
		reply = Refuse("momentum: the door read no command")
	default:
		reply = run(ctx, req)
	}
	reply.Stdout, reply.Stderr = clip(reply.Stdout), clip(reply.Stderr)
	json.NewEncoder(c).Encode(reply)
}

func clip(b []byte) []byte {
	if len(b) > MaxStream {
		return append(b[:MaxStream:MaxStream], []byte("\n[cut: the rest is over the door's limit]\n")...)
	}
	return b
}

// Call is the door's client, the momentum and tacit commands inside a sandbox: it sends name and args to the door
// at socket, writes what the command printed, and returns its exit code.
func Call(socket, name string, args []string, stdout, stderr io.Writer) int {
	dir, _ := os.Getwd()
	c, err := net.Dial("unix", socket)
	if err != nil {
		fmt.Fprintf(stderr, "%s: the sandbox's door is closed: %v\n", name, err)
		return 126
	}
	defer c.Close()
	if err := json.NewEncoder(c).Encode(Request{Argv: append([]string{name}, args...), Dir: dir}); err != nil {
		fmt.Fprintf(stderr, "%s: the sandbox's door: %v\n", name, err)
		return 126
	}
	var reply Reply
	if err := json.NewDecoder(io.LimitReader(c, 3*MaxStream)).Decode(&reply); err != nil {
		fmt.Fprintf(stderr, "%s: the sandbox's door gave no reply: %v\n", name, err)
		return 126
	}
	stdout.Write(reply.Stdout)
	stderr.Write(reply.Stderr)
	return reply.Code
}
