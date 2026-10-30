package capacity

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// fakeProc is one process of a synthetic /proc.
type fakeProc struct {
	pid, ppid int
	argv      []string
	cwd, exe  string
	fds       []string
	maps      []string
	written   int64
}

// procRoot builds a synthetic /proc with these processes.
func procRoot(t *testing.T, ps ...fakeProc) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "proc")
	for _, p := range ps {
		dir := filepath.Join(root, strconv.Itoa(p.pid))
		must(t, os.MkdirAll(filepath.Join(dir, "fd"), 0o755))
		write(t, filepath.Join(dir, "cmdline"), strings.Join(p.argv, "\x00")+"\x00")
		write(t, filepath.Join(dir, "comm"), filepath.Base(p.argv[0])+"\n")
		write(t, filepath.Join(dir, "stat"), strconv.Itoa(p.pid)+" (x) S "+strconv.Itoa(p.ppid)+" 0 0 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 100 0 0\n")
		write(t, filepath.Join(dir, "io"), "rchar: 0\nwrite_bytes: "+strconv.FormatInt(p.written, 10)+"\n")
		if p.cwd != "" {
			must(t, os.Symlink(p.cwd, filepath.Join(dir, "cwd")))
		}
		if p.exe != "" {
			must(t, os.Symlink(p.exe, filepath.Join(dir, "exe")))
		}
		for i, f := range p.fds {
			must(t, os.Symlink(f, filepath.Join(dir, "fd", strconv.Itoa(i+3))))
		}
		var maps strings.Builder
		for i, m := range p.maps {
			maps.WriteString("7f00000" + strconv.Itoa(i) + "000-7f00000" + strconv.Itoa(i) + "fff r--s 00000000 103:02 4242 " + m + "\n")
		}
		maps.WriteString("7fff0000-7fff1000 rw-p 00000000 00:00 0 [stack]\n")
		write(t, filepath.Join(dir, "maps"), maps.String())
	}
	must(t, os.MkdirAll(root, 0o755))
	return root
}

func write(t *testing.T, p, s string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(t, os.WriteFile(p, []byte(s), 0o644))
}

// blob writes a file of n bytes, so it holds blocks on disk.
func blob(t *testing.T, p string, n int) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(p), 0o755))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	must(t, os.WriteFile(p, b, 0o644))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// openStore is a fresh store in a temporary directory.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "momentum.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// fakeJobs are scheduled jobs a test sets: each unit's text by its name.
type fakeJobs struct {
	units   map[string]string
	crontab string
	err     error
}

func (j fakeJobs) Units(context.Context) ([]JobText, []string, error) {
	var out []JobText
	for n, t := range j.units {
		out = append(out, JobText{Name: n, Text: t})
	}
	return out, nil, j.err
}
func (j fakeJobs) Crontab(context.Context) (string, error) { return j.crontab, nil }

// fakeTools answers uv and docker from a table and records what ran.
type fakeTools struct {
	answers map[string]string
	ran     *[]string
}

func (f fakeTools) UV(_ context.Context, _ time.Duration, args ...string) (string, error) {
	return f.answer("uv", args)
}

func (f fakeTools) Docker(_ context.Context, _ time.Duration, args ...string) (string, error) {
	return f.answer("docker", args)
}

func (f fakeTools) answer(tool string, args []string) (string, error) {
	cmd := tool + " " + strings.Join(args, " ")
	if f.ran != nil {
		*f.ran = append(*f.ran, cmd)
	}
	for k, v := range f.answers {
		if strings.HasPrefix(cmd, k) {
			switch v {
			case "ERR":
				return "", errNoTool
			case "ERR-INUSE":
				return "", fmt.Errorf("uv cache prune: exit status 2: error: Cache is currently in-use, waiting for other uv processes to finish")
			}
			return v, nil
		}
	}
	return "", nil
}

// options are a test run's options: the synthetic /proc, no mounts, the jobs given, and a clock.
func options(t *testing.T, proc string, now time.Time, jobs Jobs) Options {
	t.Helper()
	mi := filepath.Join(t.TempDir(), "mountinfo")
	write(t, mi, "")
	if jobs == nil {
		jobs = fakeJobs{}
	}
	return Options{ProcRoot: proc, MountInfo: mi, Jobs: jobs, Clock: func() time.Time { return now },
		Tools: fakeTools{answers: map[string]string{"uv": "ERR", "docker": "ERR"}}, WriterWait: time.Millisecond}
}

// gitEnv isolates git from the user's configuration and gives commits an author.
func gitEnv(t *testing.T) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	write(t, cfg, "[init]\n\tdefaultBranch = main\n[user]\n\tname = Test\n\temail = test@example.com\n")
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// gitT runs git in dir and fails the test on an error.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// watchedSince gives a store a sampler record from from to now: a run every 20 minutes, and each model root of cfg
// watched from from.
func watchedSince(t *testing.T, st *store.Store, cfg Config, from, now time.Time) {
	t.Helper()
	var runs []store.CapacityRun
	for at := from; at.Before(now.Add(-time.Minute)); at = at.Add(20 * time.Minute) {
		runs = append(runs, store.CapacityRun{At: at, Procs: 1})
	}
	ctx := context.Background()
	must(t, st.PutRuns(ctx, runs))
	for _, r := range cfg.Models.Roots {
		must(t, st.SetMark(ctx, "watch:"+filepath.Clean(r), from))
	}
}

// findings are a rule's findings by path.
func findings(res *Result, rule string) map[string]Finding {
	out := map[string]Finding{}
	for _, r := range res.Rules {
		if r.Rule == rule {
			for _, f := range r.Findings {
				out[f.Path] = f
			}
		}
	}
	return out
}
