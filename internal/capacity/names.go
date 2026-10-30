package capacity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// A path that a systemd unit (enabled, loaded in any state, or started by an enabled timer, path or socket), a
// crontab line or an enabled Hermes job names is never deleted: the job would fail the next time it runs. Nor is
// what an installed command's symlink points into. A unit names what its files say (its fragment and drop-ins)
// and what systemd expands from them (its commands, environment and working directory), and what it hands its
// program, one level deep: its EnvironmentFiles, and a script its commands run. A word relative to a unit's
// working directory, or to a `cd` before it on a crontab line, names the path it resolves to. A file of the user's
// own jobs (a user unit's, a crontab script, a Hermes job's script) that cannot be read makes the names incomplete,
// and the caller deletes nothing. A system unit's file the user cannot read (root's configuration) is a warning:
// root's processes are as unreadable to the sampler, which is a limit of an unprivileged run. A script in a
// repository that no job runs names nothing: the owner ruled that models are downloaded again when work on them
// resumes.

// Named are the paths and repository ids the scheduled jobs name, each with the job that names it, and the system
// units' files that could not be read.
type Named struct {
	Paths    map[string]string // canonical path -> the job that names it
	RepoIDs  map[string]string // org/name -> the job that names it
	Warnings []string
}

// Protects reports whether a job names p or a path inside it, and which job.
func (n *Named) Protects(p string) (string, bool) {
	if n == nil {
		return "", false
	}
	if j, ok := n.Paths[p]; ok {
		return j, true
	}
	for q, j := range n.Paths {
		if under(q, p) {
			return j, true
		}
	}
	return "", false
}

// JobText is a job's text as capacity reads it, and the directory a relative word in it is relative to.
type JobText struct {
	Name, Text, Dir string
}

// Jobs reads what the machine's scheduled jobs are.
type Jobs interface {
	// Units are the text of every systemd unit capacity reads, for the user's manager and the system's, and the
	// system units' files that could not be read. An error is a user unit's file, or a listing, that could not be.
	Units(ctx context.Context) ([]JobText, []string, error)
	// Crontab is the user's crontab.
	Crontab(ctx context.Context) (string, error)
}

// LoadNamed gathers the names from the jobs, from Hermes's enabled jobs and from the links directories. A source or
// a file that cannot be read is an error, and the names it would have given are missing: the caller deletes
// nothing.
func LoadNamed(ctx context.Context, jobs Jobs, cfg NamesConfig, m *Mounts) (*Named, error) {
	n := &Named{Paths: map[string]string{}, RepoIDs: map[string]string{}}
	var errs []string
	addPath := func(job, p string) {
		// both as written and through its symlinks: a Hugging Face snapshot's file resolves into the shared blob
		// store, under no item, while its written path is under the repository that holds it
		for _, c := range []string{m.Canon(filepath.Clean(p)), m.resolve(p)} {
			if n.Paths[c] == "" {
				n.Paths[c] = job
			}
		}
	}
	addText := func(job, text, dir string, follow bool) {
		for _, w := range words(text) {
			switch {
			case filepath.IsAbs(w):
			case dir != "" && !strings.Contains(w, "://") && (strings.Contains(w, "/") || exists(filepath.Join(dir, w))):
				// relative to the job's directory: a path with a slash, or a bare name that is there
				if isRepoID(w) && n.RepoIDs[w] == "" {
					n.RepoIDs[w] = job
				}
				w = filepath.Join(dir, w)
			case isRepoID(w):
				if n.RepoIDs[w] == "" {
					n.RepoIDs[w] = job
				}
				continue
			default:
				continue
			}
			addPath(job, w)
			if !follow {
				continue
			}
			b, ok, err := smallText(w)
			if err != nil {
				errs = append(errs, job+": "+err.Error())
			}
			if !ok {
				continue
			}
			for _, w2 := range words(b) {
				switch {
				case filepath.IsAbs(w2):
					addPath(job+" (via "+filepath.Base(w)+")", w2)
				case isRepoID(w2) && n.RepoIDs[w2] == "":
					n.RepoIDs[w2] = job + " (via " + filepath.Base(w) + ")"
				}
			}
		}
	}
	if jobs != nil {
		units, warn, err := jobs.Units(ctx)
		if err != nil {
			errs = append(errs, "systemd units: "+err.Error())
		}
		n.Warnings = append(n.Warnings, warn...)
		for _, u := range units {
			addText("unit "+u.Name, u.Text, u.Dir, false) // a unit's scripts and environment files are in its text
		}
		tab, err := jobs.Crontab(ctx)
		if err != nil {
			errs = append(errs, "crontab: "+err.Error())
		}
		for _, l := range strings.Split(tab, "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
				dir := cdOf(l)
				if dir == "" {
					dir = homeDir() // cron runs a line in the home directory
				}
				addText("crontab", l, dir, true)
			}
		}
	}
	for _, dir := range cfg.Links {
		ents, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, "links "+dir+": "+err.Error())
			continue
		}
		for _, e := range ents {
			if e.Type()&os.ModeSymlink == 0 {
				continue
			}
			l := filepath.Join(dir, e.Name())
			if t, err := os.Readlink(l); err == nil {
				if !filepath.IsAbs(t) {
					t = filepath.Join(dir, t)
				}
				addPath("link "+l, t)
			}
		}
	}
	if cfg.HermesJobs != "" {
		texts, err := hermesJobs(cfg.HermesJobs, cfg.HermesScripts)
		if err != nil {
			errs = append(errs, "hermes jobs: "+err.Error())
		}
		for _, j := range texts {
			addText("hermes job "+j.Name, j.Text, j.Dir, false) // its script is in its text; its prompt's words only name
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return n, fmt.Errorf("%s", strings.Join(errs[:min(len(errs), 5)], "; "))
	}
	return n, nil
}

// exists reports whether a path is there.
func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// cdOf is the directory a shell line changes to before its command (`cd /x && ...`), or "".
func cdOf(line string) string {
	f := strings.Fields(line)
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "cd" || strings.HasSuffix(f[i], ";cd") {
			if d := expandHome(strings.TrimRight(f[i+1], ";&|")); filepath.IsAbs(d) {
				return d
			}
		}
	}
	return ""
}

// expandHome expands a leading ~/ or $HOME/.
func expandHome(p string) string {
	for _, pre := range []string{"~/", "$HOME/", "${HOME}/"} {
		if strings.HasPrefix(p, pre) {
			return filepath.Join(homeDir(), p[len(pre):])
		}
	}
	return p
}

// smallText is a file's text when it is a regular file of at most 1 MiB that holds no NUL byte: a script or an
// environment file, not a program. A file that exists but cannot be read is an error.
func smallText(p string) (string, bool, error) {
	st, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return "", false, nil
		}
		return "", false, err
	}
	if !st.Mode().IsRegular() || st.Size() > 1<<20 {
		return "", false, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	head := make([]byte, 8<<10) // a program holds a NUL early: it is not read further
	n, _ := f.Read(head)
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return "", false, nil
	}
	rest, err := io.ReadAll(f)
	if err != nil {
		return "", false, err
	}
	b := append(head[:n], rest...)
	if bytes.IndexByte(b, 0) >= 0 {
		return "", false, nil
	}
	return string(b), true, nil
}

// hermesJobs is the text of each enabled Hermes job: its record, its prompt, script and working directory, and its
// script's text when the script is in scripts. A missing jobs file is no jobs.
func hermesJobs(path, scripts string) ([]JobText, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	var out []JobText
	var errs []string
	for _, j := range f.Jobs {
		if on, ok := j["enabled"].(bool); ok && !on {
			continue
		}
		name, _ := j["name"].(string)
		if name == "" {
			name, _ = j["id"].(string)
		}
		raw, _ := json.Marshal(j)
		text := string(raw)
		for _, k := range []string{"prompt", "workdir", "script"} { // the words themselves, unescaped
			if v, ok := j[k].(string); ok {
				text += "\n" + v
			}
		}
		dir, _ := j["workdir"].(string)
		if sc, ok := j["script"].(string); ok && sc != "" && scripts != "" {
			p := sc
			if !filepath.IsAbs(p) {
				p = filepath.Join(scripts, sc)
			}
			t, ok, err := smallText(p)
			if err != nil {
				errs = append(errs, name+": "+err.Error())
			}
			if ok {
				text += "\n" + p + "\n" + t
			}
		}
		out = append(out, JobText{Name: name, Text: text, Dir: dir})
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return out, nil
}

// SystemJobs reads the machine's jobs with systemctl and crontab.
type SystemJobs struct{}

// unitProps are the properties of a unit capacity reads: its files, and what systemd expands from them.
var unitProps = []string{"Id", "FragmentPath", "DropInPaths", "ExecStartPre", "ExecStart", "ExecStartPost", "ExecReload",
	"ExecStop", "ExecStopPost", "Environment", "EnvironmentFiles", "WorkingDirectory", "Triggers"}

// Units lists, for the user's manager and the system's, the enabled unit files, every loaded unit in any state
// (template instances, generated and transient units among them) and what the enabled timers, paths and sockets
// start, and reads each one: its fragment and drop-ins, its expanded commands and environment, its
// EnvironmentFiles, and the scripts its commands run.
func (SystemJobs) Units(ctx context.Context) ([]JobText, []string, error) {
	var out []JobText
	var errs, warn []string
	for _, scope := range [][]string{{"--user"}, nil} {
		u, unread, err := unitsOf(ctx, scope, func(args ...string) (string, error) {
			return run(ctx, 30*time.Second, "systemctl", args...)
		})
		out = append(out, u...)
		if err != nil {
			errs = append(errs, err.Error())
		}
		if len(unread) > 0 && len(scope) > 0 {
			errs = append(errs, "user units: "+strings.Join(unread[:min(len(unread), 5)], "; "))
		} else {
			warn = append(warn, unread...)
		}
	}
	if len(errs) > 0 {
		return out, warn, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return out, warn, nil
}

// unitsOf reads one manager's units with systemctl, and names the files of them it could not read.
func unitsOf(ctx context.Context, scope []string, systemctl func(args ...string) (string, error)) ([]JobText, []string, error) {
	label := "system"
	if len(scope) > 0 {
		label = "user"
	}
	files, err := systemctl(append(scope, "list-unit-files", "--state=enabled", "--no-legend", "--no-pager", "--plain")...)
	if err != nil {
		return nil, nil, fmt.Errorf("%s unit files: %w", label, err)
	}
	loaded, err := systemctl(append(scope, "list-units", "--all", "--no-legend", "--no-pager", "--plain", "--full")...)
	if err != nil {
		return nil, nil, fmt.Errorf("%s units: %w", label, err)
	}
	seen := map[string]bool{}
	var units []string
	for _, l := range strings.Split(files+"\n"+loaded, "\n") {
		f := strings.Fields(strings.TrimLeft(l, "●* "))
		if len(f) == 0 || strings.HasSuffix(f[0], "@.service") || strings.Contains(f[0], "@.") || seen[f[0]] {
			continue // a template names no instance: its instances are among the loaded units
		}
		seen[f[0]] = true
		units = append(units, f[0])
	}
	props, err := show(scope, units, systemctl, "Triggers")
	if err != nil {
		return nil, nil, fmt.Errorf("%s triggers: %w", label, err)
	}
	for _, p := range props {
		for _, t := range strings.Fields(p.one("Triggers")) {
			if !seen[t] {
				seen[t] = true
				units = append(units, t)
			}
		}
	}
	props, err = show(scope, units, systemctl, unitProps...)
	if err != nil {
		return nil, nil, fmt.Errorf("%s units: %w", label, err)
	}
	var out []JobText
	var unread []string
	for _, p := range props {
		id := p.one("Id")
		if id == "" {
			continue
		}
		var text strings.Builder
		for _, f := range append([]string{p.one("FragmentPath")}, strings.Fields(p.one("DropInPaths"))...) {
			if f == "" {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				unread = append(unread, id+": "+err.Error())
			}
			text.Write(b)
			text.WriteByte('\n')
		}
		for _, k := range unitProps[3:] {
			for _, v := range p[k] {
				text.WriteString(k + "=" + v + "\n")
			}
		}
		// what the unit hands its program: its EnvironmentFiles, and the scripts its commands run
		for _, v := range p["EnvironmentFiles"] {
			path, opts, _ := strings.Cut(v, " ")
			t, ok, err := smallText(path)
			if err != nil && !strings.Contains(opts, "ignore_errors=yes") {
				unread = append(unread, id+": "+err.Error())
			}
			if ok {
				text.WriteString(t + "\n")
			}
		}
		for _, k := range []string{"ExecStartPre", "ExecStart", "ExecStartPost", "ExecReload", "ExecStop", "ExecStopPost"} {
			for _, v := range p[k] {
				for i, a := range argvOf(v) {
					if !filepath.IsAbs(a) || strings.ContainsAny(a, "*?[") {
						continue // a glob is the program's to expand
					}
					t, ok, err := smallText(a)
					if err != nil && i > 0 { // a program the user cannot read is root's: a binary, mostly
						unread = append(unread, id+": "+err.Error())
					}
					if ok {
						text.WriteString(t + "\n")
					}
				}
			}
		}
		out = append(out, JobText{Name: label + ":" + id, Text: text.String(), Dir: strings.TrimLeft(p.one("WorkingDirectory"), "!-~")})
	}
	sort.Strings(unread)
	return out, unread, nil
}

// argvOf is the argv of a command property as systemctl show prints it: { path=... ; argv[]=a b c ; ... }.
func argvOf(v string) []string {
	_, rest, ok := strings.Cut(v, "argv[]=")
	if !ok {
		return nil
	}
	argv, _, _ := strings.Cut(rest, " ;")
	return strings.Fields(argv)
}

// unitShow is one unit's properties; a property systemd prints more than once holds each value.
type unitShow map[string][]string

func (u unitShow) one(k string) string {
	if v := u[k]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// show runs systemctl show for some units and returns each unit's properties.
func show(scope, units []string, systemctl func(args ...string) (string, error), props ...string) ([]unitShow, error) {
	var out []unitShow
	for len(units) > 0 { // in batches: a command line has a limit
		n := min(len(units), 200)
		args := append(append([]string{}, scope...), "show", "--no-pager", "-p", strings.Join(props, ","), "--")
		txt, err := systemctl(append(args, units[:n]...)...)
		if err != nil {
			return out, err
		}
		cur := unitShow{}
		sc := bufio.NewScanner(strings.NewReader(txt))
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			l := sc.Text()
			if l == "" {
				if len(cur) > 0 {
					out = append(out, cur)
				}
				cur = unitShow{}
				continue
			}
			if k, v, ok := strings.Cut(l, "="); ok && v != "" {
				cur[k] = append(cur[k], v)
			}
		}
		if len(cur) > 0 {
			out = append(out, cur)
		}
		units = units[n:]
	}
	return out, nil
}

// Crontab is `crontab -l`; a user without one has none.
func (SystemJobs) Crontab(ctx context.Context) (string, error) {
	if _, err := exec.LookPath("crontab"); err != nil {
		return "", nil
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "crontab", "-l")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if strings.Contains(errb.String(), "no crontab") {
			return "", nil
		}
		if cctx.Err() != nil {
			return "", fmt.Errorf("crontab -l: %w", errTimeout)
		}
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// run runs a command with a timeout and returns its standard output.
func run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if cctx.Err() != nil {
		return out.String(), fmt.Errorf("%s %s: %w after %s", name, strings.Join(args[:min(len(args), 3)], " "), errTimeout, timeout)
	}
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return out.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args[:min(len(args), 3)], " "), err, msg)
	}
	return out.String(), nil
}
