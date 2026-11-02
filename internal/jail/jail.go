// Package jail runs a program where what it must not reach does not exist. The program runs in a bubblewrap
// sandbox with every namespace its own and no capability: an empty root that holds the system's programs and
// libraries read-only, an /etc of a few named files, an empty home, and only the paths it is given, each at its own
// path. It has no network but one route: a loopback port inside that leads to a Unix socket outside, which a relay
// of the caller's serves. No other socket of the machine's is there (the session bus, an agent's, a display's):
// only the route and the door. What it may read or do on the machine beyond its paths, it asks the door for: a
// server outside that runs the commands its caller allows, as the caller runs them, and returns what they print.
// The momentum and tacit commands inside are the door's client.
package jail

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// Where the sandbox's own things are, inside.
const (
	RouteSocket = "/run/momentum/route.sock" // the route's socket, which the loopback port leads to
	DoorSocket  = "/run/momentum/door.sock"  // the door's socket
	BinDir      = "/run/momentum/bin"        // momentum, and tacit beside it: the door's client
)

// InitCommand is the subcommand of momentum's binary that runs first inside a sandbox with a route (Init).
const InitCommand = "sandbox-init"

// A Mount is a path of the machine's that exists inside, at the same path: read-only unless Write.
type Mount struct {
	Path  string
	Write bool
}

// Spec is one sandbox.
type Spec struct {
	Bwrap  string   // the bubblewrap binary; "bwrap" on the path when empty
	Self   string   // momentum's binary: the sandbox's init, and the momentum and tacit commands inside (BinDir)
	Home   string   // the home inside: an empty directory at this path, which nothing outside sees
	Mounts []Mount  // what exists inside besides the system's directories, at the same paths, a parent before what it holds
	Env    []string // the program's whole environment; the door's socket is added when there is a door
	Dir    string   // the working directory inside: a path a mount holds, or the home
	// Route is a Unix socket outside: 127.0.0.1:RoutePort inside leads to it, and nothing else is reachable. With
	// no route the sandbox has no network at all.
	Route     string
	RoutePort int
	// Door is a Unix socket outside, served by ServeDoor: the momentum and tacit commands inside send it their
	// arguments. With no door they have nothing to read.
	Door string
}

// etc are the files of /etc a program may need to start, read-only: the dynamic loader's cache, the alternatives'
// links, name service and protocol tables, time zones and the public certificate authorities. passwd, group and
// hosts are the sandbox's own (identity). Nothing else of /etc is there.
var etc = []string{"/etc/alternatives", "/etc/ca-certificates", "/etc/gai.conf", "/etc/host.conf", "/etc/ld.so.cache", "/etc/ld.so.conf",
	"/etc/ld.so.conf.d", "/etc/localtime", "/etc/mime.types", "/etc/nsswitch.conf", "/etc/os-release", "/etc/protocols",
	"/etc/services", "/etc/ssl", "/etc/timezone"}

// system are the directories of programs and libraries, read-only. On a merged /usr the others are links into it.
var system = []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/lib32", "/libx32"}

// Command is argv run in the sandbox. The command it returns is bubblewrap's; its environment is the program's.
// Bubblewrap dies with its caller, and the program's every process dies with it: they share a process namespace
// whose first process is bubblewrap's.
func (s Spec) Command(ctx context.Context, argv ...string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, errors.New("jail: no program")
	}
	if s.Home == "" || !filepath.IsAbs(s.Home) {
		return nil, fmt.Errorf("jail: the home must be an absolute path, got %q", s.Home)
	}
	if s.Route != "" && (s.RoutePort <= 0 || s.RoutePort > 65535) {
		return nil, fmt.Errorf("jail: a route needs a port, got %d", s.RoutePort)
	}
	if (s.Route != "" || s.Door != "") && s.Self == "" {
		return nil, errors.New("jail: a route or a door needs momentum's binary (Self)")
	}
	bwrap := s.Bwrap
	if bwrap == "" {
		bwrap = "bwrap"
	}
	ident, err := identity(s.Home)
	if err != nil {
		return nil, err
	}
	args := []string{"--unshare-all", "--die-with-parent", "--new-session", "--cap-drop", "ALL", "--hostname", "sandbox",
		"--tmpfs", "/", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--tmpfs", "/var/tmp", "--tmpfs", "/run"}
	for _, d := range system {
		switch st, err := os.Lstat(d); {
		case err != nil:
		case st.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(d)
			if err != nil {
				return nil, err
			}
			args = append(args, "--symlink", target, d)
		default:
			args = append(args, "--ro-bind", d, d)
		}
	}
	for _, f := range etc {
		if _, err := os.Stat(f); err == nil {
			args = append(args, "--ro-bind", f, f)
		}
	}
	var files []*os.File
	for _, f := range []struct{ path, data string }{{"/etc/passwd", ident.passwd}, {"/etc/group", ident.group},
		{"/etc/hosts", "127.0.0.1 localhost\n::1 localhost\n"}} {
		r, w, err := os.Pipe()
		if err != nil {
			closeAll(files)
			return nil, err
		}
		// a file this small fits the pipe's buffer: it is written whole before bubblewrap reads it
		_, werr := w.WriteString(f.data)
		w.Close()
		if werr != nil {
			r.Close()
			closeAll(files)
			return nil, werr
		}
		files = append(files, r)
		args = append(args, "--ro-bind-data", strconv.Itoa(2+len(files)), f.path)
	}
	args = append(args, "--dir", s.Home)
	for _, m := range s.Mounts {
		if !filepath.IsAbs(m.Path) {
			closeAll(files)
			return nil, fmt.Errorf("jail: a mount must be an absolute path, got %q", m.Path)
		}
		flag := "--ro-bind"
		if m.Write {
			flag = "--bind"
		}
		args = append(args, flag, m.Path, m.Path)
	}
	if s.Self != "" {
		args = append(args, "--ro-bind", s.Self, BinDir+"/momentum", "--symlink", "momentum", BinDir+"/tacit")
	}
	env := append([]string{}, s.Env...)
	if s.Door != "" {
		args = append(args, "--bind", s.Door, DoorSocket)
		env = append(env, "MOMENTUM_DOOR="+DoorSocket)
	}
	if s.Route != "" {
		args = append(args, "--bind", s.Route, RouteSocket)
	}
	args = append(args, "--chdir", firstOf(s.Dir, s.Home), "--")
	if s.Route != "" {
		args = append(args, BinDir+"/momentum", InitCommand, "--port", strconv.Itoa(s.RoutePort), "--")
	}
	args = append(args, argv...)
	cmd := exec.CommandContext(ctx, bwrap, args...)
	cmd.Env = env
	cmd.ExtraFiles = files
	return cmd, nil
}

// Close releases what a command made for its sandbox's files, once it has started (or will not).
func Close(cmd *exec.Cmd) { closeAll(cmd.ExtraFiles) }

func closeAll(fs []*os.File) {
	for _, f := range fs {
		f.Close()
	}
}

type ident struct{ passwd, group string }

// identity is the sandbox's /etc/passwd and /etc/group: the user alone, with the sandbox's home, so a program that
// asks who it is gets an answer and nothing of the machine's other accounts.
func identity(home string) (ident, error) {
	uid, gid := os.Getuid(), os.Getgid()
	name, group := "user", "user"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	if g, err := user.LookupGroupId(strconv.Itoa(gid)); err == nil && g.Name != "" {
		group = g.Name
	}
	for _, s := range []string{name, group, home} {
		if strings.ContainsAny(s, ":\n") {
			return ident{}, fmt.Errorf("jail: %q cannot go in a passwd line", s)
		}
	}
	return ident{
		passwd: fmt.Sprintf("%s:x:%d:%d::%s:/bin/bash\n", name, uid, gid, home),
		group:  fmt.Sprintf("%s:x:%d:\n", group, gid),
	}, nil
}

func firstOf(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
