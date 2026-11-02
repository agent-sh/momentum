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
	"io/fs"
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

// DoorEnv is the variable that holds the door's socket inside: with it set, the momentum and tacit commands are the
// door's client (Inside).
const DoorEnv = "MOMENTUM_DOOR"

// InitCommand is the subcommand of momentum's binary that runs first inside a sandbox with a route (Init).
const InitCommand = "sandbox-init"

// MinRoutePort is the lowest port a route may use: the program has no capability, so it cannot bind a lower one.
const MinRoutePort = 1024

// maxPath is the most bytes a path can have on Linux (PATH_MAX, which counts the byte that ends it).
const maxPath = 4096

// A Mount is a path of the machine's that exists inside, at the same path: read-only unless Write.
//
// Its path is absolute and clean. Mounts are made in the order listed, so a parent comes before what it holds (one
// listed after a mount it holds would cover it), and none is or holds the home, /etc, /proc, /dev or /run, which the
// sandbox makes itself: Command refuses all of these.
//
// What a program writes under a writable mount is input to every process outside the sandbox that reads or runs it
// afterwards. Command makes the control files git runs in a repository the mount holds read-only (gitControl); the
// rest is for the caller's gates to check before it reads what the program left, not the sandbox's to prevent.
type Mount struct {
	Path  string
	Write bool
}

// Spec is one sandbox.
type Spec struct {
	Bwrap  string   // the bubblewrap binary; "bwrap" on the path when empty
	Self   string   // momentum's binary: the sandbox's init, and the momentum and tacit commands inside (BinDir)
	Home   string   // the home inside: an empty directory at this path, which nothing outside sees
	Mounts []Mount  // what exists inside besides the system's directories, at the same paths (see Mount)
	Env    []string // the program's environment: these variables, DoorEnv when there is a door, and PWD, which bubblewrap sets
	Dir    string   // the working directory inside: a path a mount holds, or the home
	// Route is a Unix socket outside: 127.0.0.1:RoutePort inside leads to it, and nothing else is reachable. With
	// no route the sandbox has no network at all. The port is MinRoutePort or above.
	Route     string
	RoutePort int
	// Door is a Unix socket outside, served by ServeDoor: the momentum and tacit commands inside send it their
	// arguments. With no door they have nothing to read.
	Door string
}

// etc are the files of /etc a program may need to start, read-only: the dynamic loader's cache, the alternatives'
// links, name service and protocol tables, time zones, and the public certificate authorities (the certs directory
// and the OpenSSL configuration, not /etc/ssl whole: its private directory holds keys that its group can read, and
// the sandbox keeps the caller's groups). The certs directory links into /usr/share/ca-certificates, which /usr
// holds; on a system whose links lead elsewhere under /etc they dangle inside until that directory is added here.
// passwd, group and hosts are the sandbox's own (identity). Nothing else of /etc is there.
var etc = []string{"/etc/alternatives", "/etc/gai.conf", "/etc/host.conf", "/etc/ld.so.cache", "/etc/ld.so.conf",
	"/etc/ld.so.conf.d", "/etc/localtime", "/etc/mime.types", "/etc/nsswitch.conf", "/etc/os-release", "/etc/protocols",
	"/etc/services", "/etc/ssl/certs", "/etc/ssl/openssl.cnf", "/etc/timezone"}

// system are the directories of programs and libraries, read-only. On a merged /usr the others are links into it.
var system = []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/lib32", "/libx32"}

// gitControl are the paths in a repository's .git that git runs or reads as commands: its hooks, its configuration
// (fsmonitor, sshCommand, aliases, filter drivers) and its info directory (attributes). What a run writes there is
// run by git outside the sandbox, with that process's environment and credentials, and a reviewed diff does not show
// it, since git does not track .git. A writable mount that holds a .git directory gets these read-only.
var gitControl = []string{".git/hooks", ".git/config", ".git/info"}

// Command is argv run in the sandbox. The command it returns is bubblewrap's; its environment is the program's.
//
// Bubblewrap is the sandbox's first process (pid 1) and dies with its caller, and the program's every process with
// it: they share a process namespace of their own. With a route, momentum sandbox-init is the first process bubblewrap
// starts (pid 2) and runs the program.
//
// The exit code is the program's, or 128 plus the signal that killed it. Where the sandbox fails instead, its code
// collides with the program's own: bubblewrap's failures (a path it cannot mount, a program it cannot start) exit 1
// with a message that starts "bwrap:", and sandbox-init's exit 2 (its arguments, the route's port) or 127 (the
// program did not start) with a message that starts "momentum sandbox-init:". So a caller that got no result from the
// program (no reply, no transcript) treats the exit as the sandbox's failure and keeps the last line of stderr as its
// reason; the code alone does not tell the two apart.
func (s Spec) Command(ctx context.Context, argv ...string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, errors.New("jail: no program")
	}
	home, mounts, err := s.check()
	if err != nil {
		return nil, err
	}
	bwrap := s.Bwrap
	if bwrap == "" {
		bwrap = "bwrap"
	}
	ident, err := identity(home)
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
		r, err := dataFile(f.data)
		if err != nil {
			closeAll(files)
			return nil, err
		}
		files = append(files, r)
		args = append(args, "--ro-bind-data", strconv.Itoa(2+len(files)), f.path)
	}
	args = append(args, "--dir", home)
	args = append(args, mounts...)
	if s.Self != "" {
		args = append(args, "--ro-bind", s.Self, BinDir+"/momentum", "--symlink", "momentum", BinDir+"/tacit")
	}
	env := append([]string{}, s.Env...)
	if s.Door != "" {
		args = append(args, "--bind", s.Door, DoorSocket)
		env = append(env, DoorEnv+"="+DoorSocket)
	}
	if s.Route != "" {
		args = append(args, "--bind", s.Route, RouteSocket)
	}
	args = append(args, "--chdir", firstOf(s.Dir, home), "--")
	if s.Route != "" {
		args = append(args, BinDir+"/momentum", InitCommand, "--port", strconv.Itoa(s.RoutePort), "--")
	}
	args = append(args, argv...)
	cmd := exec.CommandContext(ctx, bwrap, args...)
	cmd.Env = env
	cmd.ExtraFiles = files
	return cmd, nil
}

// check refuses a Spec that undoes the sandbox's guarantees or that bubblewrap would fail on later, and returns the
// home (cleaned) and the bubblewrap arguments of the mounts.
func (s Spec) check() (home string, mounts []string, err error) {
	if s.Home == "" || !filepath.IsAbs(s.Home) {
		return "", nil, fmt.Errorf("jail: the home must be an absolute path, got %q", s.Home)
	}
	home = filepath.Clean(s.Home)
	if len(home) >= maxPath {
		return "", nil, fmt.Errorf("jail: the home is %d bytes, and a path has at most %d", len(home), maxPath-1)
	}
	if s.Route != "" && (s.RoutePort < MinRoutePort || s.RoutePort > 65535) {
		return "", nil, fmt.Errorf("jail: a route needs a port from %d to 65535, got %d", MinRoutePort, s.RoutePort)
	}
	if (s.Route != "" || s.Door != "") && s.Self == "" {
		return "", nil, errors.New("jail: a route or a door needs momentum's binary (Self)")
	}
	own := []struct{ path, name string }{{home, "the home"}, {"/etc", "/etc"}, {"/proc", "/proc"}, {"/dev", "/dev"}, {"/run", "/run"}}
	for i, m := range s.Mounts {
		switch {
		case !filepath.IsAbs(m.Path):
			return "", nil, fmt.Errorf("jail: a mount must be an absolute path, got %q", m.Path)
		case filepath.Clean(m.Path) != m.Path:
			return "", nil, fmt.Errorf("jail: a mount must be a clean path, got %q (%q)", m.Path, filepath.Clean(m.Path))
		case m.Path == "/":
			return "", nil, errors.New(`jail: a mount of "/" would put the whole machine inside`)
		}
		for _, o := range own {
			if holds(m.Path, o.path) {
				return "", nil, fmt.Errorf("jail: the mount %q is or holds %s, which the sandbox makes itself", m.Path, o.name)
			}
		}
		for _, before := range s.Mounts[:i] {
			if holds(m.Path, before.Path) {
				return "", nil, fmt.Errorf("jail: the mount %q is listed after %q, which it holds and would cover: a parent comes before what it holds",
					m.Path, before.Path)
			}
		}
		if !m.Write {
			mounts = append(mounts, "--ro-bind", m.Path, m.Path)
			continue
		}
		mounts = append(mounts, "--bind", m.Path, m.Path)
		guarded, err := gitReadOnly(m.Path)
		if err != nil {
			return "", nil, err
		}
		for _, p := range guarded {
			mounts = append(mounts, "--ro-bind", p, p)
		}
	}
	return home, mounts, nil
}

// holds reports whether the clean path a is b or a directory above it.
func holds(a, b string) bool {
	return a == b || a == "/" || strings.HasPrefix(b, a+"/")
}

// gitReadOnly returns the paths of gitControl that exist in the repository at root, when root holds a .git directory:
// they are bound read-only over the writable mount, after it. A .git that is a file (a linked work tree: it links to
// a directory outside) has none of them, and what it links to is the caller's to check. A control path that is a link
// is refused: a mount over a link lands on its target, and the run could replace the link. The paths are looked at
// when Command runs, so a control file that does not exist then is not guarded.
func gitReadOnly(root string) ([]string, error) {
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil, nil // a file, or nothing: bubblewrap says what is wrong with a mount it cannot make
	}
	switch st, err := os.Lstat(filepath.Join(root, ".git")); {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	case !st.IsDir():
		return nil, nil
	}
	var paths []string
	for _, rel := range gitControl {
		p := filepath.Join(root, rel)
		switch st, err := os.Lstat(p); {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, err
		case st.Mode()&os.ModeSymlink != 0:
			return nil, fmt.Errorf("jail: %s is a link, and a mount cannot make a link read-only", p)
		default:
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// dataFile is the read end of a pipe that holds data, for bubblewrap's --ro-bind-data. The data is written from a
// goroutine: nothing reads the pipe until the command runs, and a pipe holds as little as one page when the user's
// pipes are over their soft limit, so no size of data may be written before then. Closing the read end (Close) ends
// the write of data that was never read.
func dataFile(data string) (*os.File, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	go func() {
		w.WriteString(data)
		w.Close()
	}()
	return r, nil
}

// Close releases what a command made for its sandbox's files, once it has started (or will not).
func Close(cmd *exec.Cmd) { closeAll(cmd.ExtraFiles) }

func closeAll(files []*os.File) {
	for _, f := range files {
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
