package jail

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
)

// Init is the first program of a sandbox with a route (momentum sandbox-init --port P -- PROGRAM ARGS...): it
// listens on 127.0.0.1:P inside, forwards each connection to the route's socket, runs the program with the
// sandbox's environment, passes it the signals it gets, and exits as the program does. The loopback interface is
// the sandbox's own, so the port is the only way out, and it leads only to the route.
func Init(args []string) int {
	port, prog, err := initArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "momentum "+InitCommand+":", err)
		return 2
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "momentum "+InitCommand+": the route's port:", err)
		return 2
	}
	go forward(ln, RouteSocket)
	cmd := exec.Command(prog[0], prog[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "momentum "+InitCommand+":", err)
		return 127
	}
	go func() {
		for s := range sigs {
			cmd.Process.Signal(s)
		}
	}()
	err = cmd.Wait()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	fmt.Fprintln(os.Stderr, "momentum "+InitCommand+":", err)
	return 1
}

func initArgs(args []string) (int, []string, error) {
	if len(args) < 4 || args[0] != "--port" || args[2] != "--" {
		return 0, nil, errors.New("usage: " + InitCommand + " --port P -- PROGRAM [ARG]...")
	}
	port, err := strconv.Atoi(args[1])
	if err != nil || port <= 0 || port > 65535 {
		return 0, nil, fmt.Errorf("a port, got %q", args[1])
	}
	return port, args[3:], nil
}

// forward copies each connection to the listener both ways to a new connection to the socket.
func forward(ln net.Listener, socket string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			up, err := net.Dial("unix", socket)
			if err != nil {
				return
			}
			defer up.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(up, c); closeWrite(up); done <- struct{}{} }()
			go func() { io.Copy(c, up); closeWrite(c); done <- struct{}{} }()
			<-done
			<-done
		}(c)
	}
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}
