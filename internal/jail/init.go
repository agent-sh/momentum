package jail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Inside runs what momentum's binary does when it is the sandbox's own, and says whether it did: args are the
// process's arguments (os.Args). As the first process bubblewrap starts (momentum sandbox-init ...) it is Init. With
// DoorEnv set, the momentum and tacit commands (the same binary, under the name tacit for the second) are the door's
// client, since nothing of the machine's is there for them to read. Anywhere else it returns false. The binary's main
// and the tests that stand in for it both call this, so there is one copy of these two branches.
func Inside(args []string, stdout, stderr io.Writer) (code int, ok bool) {
	if len(args) > 1 && args[1] == InitCommand {
		return Init(args[2:]), true
	}
	if door := os.Getenv(DoorEnv); door != "" && len(args) > 0 {
		name := "momentum"
		if filepath.Base(args[0]) == "tacit" {
			name = "tacit"
		}
		return Call(door, name, args[1:], stdout, stderr), true
	}
	return 0, false
}

// Init is the first program a sandbox with a route starts (momentum sandbox-init --port P -- PROGRAM ARGS...): it
// listens on 127.0.0.1:P inside, forwards each connection to the route's socket, runs the program with the
// sandbox's environment, passes it the signals it gets, and exits as the program does (128 plus the signal for a
// program a signal killed). The loopback interface is the sandbox's own, so the port is the only way out, and it
// leads only to the route.
//
// It exits 2 when its arguments or the route's port are wrong, and 127 when the program does not start; bubblewrap's
// own failures exit 1 (Spec.Command).
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
	// The program gets this process's environment, and a bare name is looked up in that environment's PATH, as
	// bubblewrap's own execvp does when it runs the program itself: so a program starts with and without a route.
	env := os.Environ()
	path, err := lookPath(prog[0], env)
	if err != nil {
		fmt.Fprintln(os.Stderr, "momentum "+InitCommand+":", err)
		return 127
	}
	cmd := &exec.Cmd{Path: path, Args: prog, Env: env, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
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
	if err != nil || port < MinRoutePort || port > 65535 {
		return 0, nil, fmt.Errorf("a port from %d to 65535, got %q", MinRoutePort, args[1])
	}
	return port, args[3:], nil
}

// defaultPath is where a program is looked for when its environment has no PATH: the C library's default for
// execvp, which is what bubblewrap uses to start the program when sandbox-init does not.
const defaultPath = "/bin:/usr/bin"

// lookPath is the file a program name stands for in env, as execvp finds it: a name with a slash is itself, any other
// is the first executable file of that name in the directories of env's PATH (an empty entry is the current
// directory), or of defaultPath when env has none. Go's exec.LookPath reads this process's PATH instead, which
// is not the program's when the two differ, and fails when there is none.
func lookPath(name string, env []string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	path := defaultPath
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v // the last one wins, as in a process's environment
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

// forward copies each connection to the listener both ways to a new connection to the socket.
func forward(ln net.Listener, socket string) {
	acceptLoop(context.Background(), ln, func(c net.Conn) {
		go func() {
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
		}()
	})
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}
