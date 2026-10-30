package capacity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A path an enabled systemd unit, an enabled timer's unit, a crontab line or an enabled Hermes job names is never
// deleted: the job would fail the next time it runs. Nor is what an installed command's symlink points into. The unit's own files count (its fragment and drop-ins), and
// so do the files it hands its program, one level deep: an EnvironmentFile, and a script its command runs. A
// script in a repository that no job runs names nothing; the owner ruled that models are downloaded again when
// work on them resumes.

// Named are the paths and repository ids the scheduled jobs name, each with the job that names it.
type Named struct {
	Paths   map[string]string // canonical path -> the job that names it
	RepoIDs map[string]string // org/name -> the job that names it
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

// Jobs reads what the machine's scheduled jobs are.
type Jobs interface {
	// Units are the text of every enabled systemd unit and of every unit an enabled timer starts, for the user's
	// manager and the system's, each with its name.
	Units(ctx context.Context) (map[string]string, error)
	// Crontab is the user's crontab.
	Crontab(ctx context.Context) (string, error)
}

// LoadNamed gathers the names from the jobs and from Hermes's enabled jobs. A source that cannot be read is an
// error, and the names it would have given are missing: the caller deletes nothing that depends on them.
func LoadNamed(ctx context.Context, jobs Jobs, cfg NamesConfig, m *Mounts) (*Named, error) {
	n := &Named{Paths: map[string]string{}, RepoIDs: map[string]string{}}
	var errs []string
	addText := func(job, text string, follow bool) {
		for _, w := range words(text) {
			switch {
			case filepath.IsAbs(w):
				p := m.resolve(w)
				if _, ok := n.Paths[p]; !ok {
					n.Paths[p] = job
				}
				if follow {
					if b, ok := smallText(w); ok {
						for _, w2 := range words(b) {
							if filepath.IsAbs(w2) {
								if p2 := m.resolve(w2); n.Paths[p2] == "" {
									n.Paths[p2] = job + " (via " + filepath.Base(w) + ")"
								}
							} else if isRepoID(w2) && n.RepoIDs[w2] == "" {
								n.RepoIDs[w2] = job + " (via " + filepath.Base(w) + ")"
							}
						}
					}
				}
			case isRepoID(w):
				if _, ok := n.RepoIDs[w]; !ok {
					n.RepoIDs[w] = job
				}
			}
		}
	}
	if jobs != nil {
		units, err := jobs.Units(ctx)
		if err != nil {
			errs = append(errs, "systemd units: "+err.Error())
		}
		names := make([]string, 0, len(units))
		for k := range units {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, u := range names {
			addText("unit "+u, units[u], true)
		}
		tab, err := jobs.Crontab(ctx)
		if err != nil {
			errs = append(errs, "crontab: "+err.Error())
		}
		for _, l := range strings.Split(tab, "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
				addText("crontab", l, true)
			}
		}
	}
	for _, dir := range cfg.Links {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.Type()&os.ModeSymlink == 0 {
				continue
			}
			l := filepath.Join(dir, e.Name())
			if t, err := filepath.EvalSymlinks(l); err == nil {
				if p := m.Canon(t); n.Paths[p] == "" {
					n.Paths[p] = "link " + l
				}
			}
		}
	}
	if cfg.HermesJobs != "" {
		texts, err := hermesJobs(cfg.HermesJobs, cfg.HermesScripts)
		if err != nil {
			errs = append(errs, "hermes jobs: "+err.Error())
		}
		for job, t := range texts {
			addText("hermes job "+job, t, true)
		}
	}
	if len(errs) > 0 {
		return n, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return n, nil
}

// smallText is a file's text when it is a regular file of at most 1 MiB that holds no NUL byte: a script or an
// environment file, not a program.
func smallText(p string) (string, bool) {
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() > 1<<20 {
		return "", false
	}
	b, err := os.ReadFile(p)
	if err != nil || bytes.IndexByte(b, 0) >= 0 {
		return "", false
	}
	return string(b), true
}

// hermesJobs is the text of each enabled Hermes job: its prompt, script, working directory and the rest of its
// record, and its script's text when the script is in scripts. A missing jobs file is no jobs.
func hermesJobs(path, scripts string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
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
	out := map[string]string{}
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
		if sc, ok := j["script"].(string); ok && sc != "" && scripts != "" {
			p := sc
			if !filepath.IsAbs(p) {
				p = filepath.Join(scripts, sc)
			}
			if t, ok := smallText(p); ok {
				text += "\n" + p + "\n" + t
			}
		}
		out[name] = text
	}
	return out, nil
}

// SystemJobs reads the machine's jobs with systemctl and crontab.
type SystemJobs struct{}

// Units lists the enabled units and the units the enabled timers start, of the user's manager and the system's,
// and reads each one's fragment and drop-ins.
func (SystemJobs) Units(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	var errs []string
	for _, scope := range [][]string{{"--user"}, nil} {
		if err := unitsOf(ctx, scope, out); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return out, nil
}

func unitsOf(ctx context.Context, scope []string, out map[string]string) error {
	label := "system"
	if len(scope) > 0 {
		label = "user"
	}
	list, err := run(ctx, 20*time.Second, "systemctl", append(scope, "list-unit-files", "--state=enabled", "--no-legend", "--no-pager", "--plain")...)
	if err != nil {
		return fmt.Errorf("%s units: %w", label, err)
	}
	var units, timers []string
	for _, l := range strings.Split(list, "\n") {
		f := strings.Fields(l)
		if len(f) == 0 || strings.Contains(f[0], "@.") { // a template names no instance
			continue
		}
		units = append(units, f[0])
		if strings.HasSuffix(f[0], ".timer") || strings.HasSuffix(f[0], ".path") || strings.HasSuffix(f[0], ".socket") {
			timers = append(timers, f[0])
		}
	}
	if len(timers) > 0 {
		props, err := show(ctx, scope, timers, "Triggers")
		if err != nil {
			return fmt.Errorf("%s triggers: %w", label, err)
		}
		for _, p := range props {
			units = append(units, strings.Fields(p["Triggers"])...)
		}
	}
	props, err := show(ctx, scope, units, "Id", "FragmentPath", "DropInPaths")
	if err != nil {
		return fmt.Errorf("%s unit files: %w", label, err)
	}
	for _, p := range props {
		var text strings.Builder
		for _, f := range append([]string{p["FragmentPath"]}, strings.Fields(p["DropInPaths"])...) {
			if f == "" {
				continue
			}
			if b, err := os.ReadFile(f); err == nil {
				text.Write(b)
				text.WriteByte('\n')
			}
		}
		if p["Id"] != "" {
			out[label+":"+p["Id"]] = text.String()
		}
	}
	return nil
}

// show runs systemctl show for some units and returns each unit's properties.
func show(ctx context.Context, scope, units []string, props ...string) ([]map[string]string, error) {
	var out []map[string]string
	for len(units) > 0 { // in batches: a command line has a limit
		n := min(len(units), 200)
		args := append(append([]string{}, scope...), "show", "--no-pager", "-p", strings.Join(props, ","), "--")
		txt, err := run(ctx, 30*time.Second, "systemctl", append(args, units[:n]...)...)
		if err != nil {
			return out, err
		}
		cur := map[string]string{}
		sc := bufio.NewScanner(strings.NewReader(txt))
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			l := sc.Text()
			if l == "" {
				if len(cur) > 0 {
					out = append(out, cur)
				}
				cur = map[string]string{}
				continue
			}
			if k, v, ok := strings.Cut(l, "="); ok {
				cur[k] = v
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
	cmd := exec.CommandContext(ctx, "crontab", "-l")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if strings.Contains(errb.String(), "no crontab") {
			return "", nil
		}
		if _, lookErr := exec.LookPath("crontab"); lookErr != nil {
			return "", nil
		}
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// run runs a command with a timeout and returns its standard output.
func run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return out.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args[:min(len(args), 3)], " "), err, msg)
	}
	return out.String(), nil
}
