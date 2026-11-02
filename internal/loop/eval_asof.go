package loop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
)

// What both arms of a trial read, as it stood at the trial's end: the store, the repositories' checkouts and tacit's
// store with its index. They are made once for the pair, in the pair's directory, and each arm runs on a copy of
// the store and of the checkouts, so a source that moves between the arms (a repository reset, a transcript
// rotated) changes neither. None of it is in a sandbox until the trial's door or mounts give it: a trial reads the
// store only through the door's commands, run on the trial's clock, and the checkouts as read-only mounts.

// pairInputs are what both arms of a trial read, made once before either runs.
type pairInputs struct {
	dir string    // the pair's directory
	end time.Time // the end of store
	// store is the store as of the end; repos its checkouts, numbered from repo-1 over the repositories that existed
	store string
	repos []string
	// tacit is a clone of tacit's store as of the end, tacitIndex the index both arms share (built before either
	// runs), tacitHead the clone's commit; tacitErr says why the trials read no tacit when the machine has its store
	tacit, tacitIndex, tacitHead, tacitErr string
	// err says why the pair could not be made: each arm's trial records it, and none runs
	err string
}

// makePair makes a trial's inputs once, for both its arms, and says how to remove them.
func (e *Eval) makePair(ctx context.Context, t GoldenTask, n int, repositories []string) (pairInputs, func()) {
	var in pairInputs
	fail := func(why string) pairInputs {
		in.err = why
		if e.Tacit != "" { // with no pair there is no tacit either: the trials say why
			in.tacitErr = "tacit's store as of the end: the pair's inputs could not be made"
		}
		return in
	}
	end, err := e.snapshotEnd(ctx, t)
	if err != nil {
		return fail(err.Error()), func() {}
	}
	in.end = end
	dir := filepath.Join(e.Dir, "pairs", digest(fmt.Sprintf("%s\x00%s\x00%d", e.ID, t.ID, n))[:16])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(err.Error()), func() {}
	}
	done := func() { os.RemoveAll(dir) }
	in.dir, in.store = dir, filepath.Join(dir, "momentum.db")
	if err := store.AsOf(ctx, e.Source, in.store, end.Add(-store.CaseRetention), end); err != nil {
		return fail("the store as of the cutoff: " + err.Error()), done
	}
	if in.repos, err = cloneRepositories(ctx, repositories, end, filepath.Join(dir, "repos")); err != nil {
		return fail("the repositories as of the cutoff: " + err.Error()), done
	}
	// tacit's store is a git repository: the trial reads it as the machine held it at the end, outside the trial's
	// directory; both arms share it and its index, built before either runs
	if e.Tacit != "" {
		clone := filepath.Join(dir, "tacit")
		made, err := repositoryAsOf(ctx, e.Tacit, end, clone)
		switch {
		case err != nil:
			in.tacitErr = "tacit's store as of the end: " + err.Error()
		case !made:
			in.tacitErr = "tacit's store held no commit by the end"
		case e.programs.tacit == "":
			in.tacitErr = "tacit's store as of the end: tacit is not on this machine"
		default:
			in.tacit, in.tacitIndex = clone, filepath.Join(dir, "tacit-index")
			os.MkdirAll(in.tacitIndex, 0o700)
			if why := e.warmTacit(ctx, &in, dir); why != "" {
				in.tacit, in.tacitHead, in.tacitErr = "", "", why
			}
		}
	}
	return in, done
}

// warmTacit builds the index of a pair's tacit clone before either arm runs, so neither pays for it inside its own
// command and its deadline, with the tacit the arms run (the evaluation's copy), and checks that the index of the
// clone's commit is there: tacit's hook index exits 0 whatever happened, and a build cut short leaves none. It is
// given twice tacit's own five minutes. It says why the trials read no tacit, or nothing.
func (e *Eval) warmTacit(ctx context.Context, in *pairInputs, dir string) string {
	head, err := evalGit(ctx, "-C", in.tacit, "rev-parse", "HEAD").Output()
	if err != nil {
		return "tacit's store as of the end: no commit: " + err.Error()
	}
	in.tacitHead = strings.TrimSpace(string(head))
	warm := filepath.Join(dir, "tacit-warm")
	if err := os.MkdirAll(warm, 0o700); err != nil {
		return "tacit's index of its store as of the end: " + err.Error()
	}
	warmUp := e.TacitWarm
	if len(warmUp) == 0 {
		warmUp = []string{e.programs.tacit, "hook", "index"} // builds the index of the store's commit, and waits for it
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, warmUp[0], warmUp[1:]...)
	cmd.Dir, cmd.Env = warm, CleanEnv(tacitAsOf(*in, warm), e.Runner.Bins...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("tacit's index of its store as of the end: %v: %s", err, plain.Clip(plain.Line(string(out)), 200))
	}
	if built, _ := filepath.Glob(filepath.Join(in.tacitIndex, "index-v*-"+in.tacitHead+".db")); len(built) == 0 {
		return "tacit's index of its store as of the end was not built: " + plain.Clip(plain.Line(string(out)), 200)
	}
	return ""
}

// tacitAsOf is the environment of tacit's commands in a trial: the store cloned as of the end, the index both arms
// share, and settings, state and a local layer of the run's own and empty, so no fact of the machine's comes in
// beside the clone's.
func tacitAsOf(in pairInputs, home string) map[string]string {
	settings := filepath.Join(home, "tacit-settings.yaml")
	os.WriteFile(settings, []byte("{}\n"), 0o600)
	for _, d := range []string{"tacit-state", "tacit-local"} {
		os.MkdirAll(filepath.Join(home, d), 0o700)
	}
	return map[string]string{"TACIT_STORE": in.tacit, "TACIT_INDEX": in.tacitIndex, "TACIT_SETTINGS": settings, "TACIT_STATE_DIR": filepath.Join(home, "tacit-state"),
		"TACIT_LOCAL": filepath.Join(home, "tacit-local"), "TACIT_LOCAL_LAYER": "off"}
}

// copyStore copies a closed store, its write-ahead log with it when there is one.
func copyStore(src, dst string) error {
	for _, suffix := range []string{"", "-wal"} {
		in, err := os.Open(src + suffix)
		if os.IsNotExist(err) && suffix != "" {
			continue
		}
		if err != nil {
			return err
		}
		out, err := os.OpenFile(dst+suffix, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			in.Close()
			return err
		}
		_, err = io.Copy(out, in)
		in.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// snapshotEnd is the earlier of the task's cutoff and one minute after its newest symptom.
func (e *Eval) snapshotEnd(ctx context.Context, t GoldenTask) (time.Time, error) {
	st, err := store.OpenReadOnly(e.Source)
	if err != nil {
		return time.Time{}, err
	}
	defer st.Close()
	var newest time.Time
	for _, id := range t.Symptom.Query.EventIDs {
		ev, ok, err := st.EventByID(ctx, strings.TrimPrefix(id, "ev:"))
		if err != nil {
			return time.Time{}, err
		}
		if !ok {
			return time.Time{}, fmt.Errorf("missing symptom event %s", id)
		}
		if ev.TS.After(newest) {
			newest = ev.TS
		}
	}
	if newest.IsZero() || t.Cutoff.IsZero() {
		return time.Time{}, fmt.Errorf("a trial needs dated symptoms and a cutoff")
	}
	end := newest.Add(time.Minute)
	if t.Cutoff.Before(end) {
		end = t.Cutoff
	}
	return end, nil
}

// cloneRepositories gives every pair the same repository set, with no hint of which one changed: each repository
// as its default branch stood at the end of store, the fix's own repository too, since its fix came after. A
// repository with no commit by then did not exist yet and is left out, and the rest are numbered from repo-1.
func cloneRepositories(ctx context.Context, sources []string, end time.Time, dir string) ([]string, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var out []string
	for _, src := range sources {
		dst := filepath.Join(dir, fmt.Sprintf("repo-%d", len(out)+1))
		made, err := repositoryAsOf(ctx, src, end, dst)
		if err != nil {
			return nil, err
		}
		if made {
			out = append(out, dst)
		}
	}
	return out, nil
}

// copyCheckouts gives an arm its own copy of a pair's checkouts, numbered as they are: a --no-local clone of each,
// with the cleanup and the checks of the first (cloneAsOf), so both arms read the same commits whatever the sources
// do meanwhile.
func copyCheckouts(ctx context.Context, repos []string, dir string) ([]string, error) {
	if len(repos) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var out []string
	for _, src := range repos {
		head, err := evalGit(ctx, "-C", src, "rev-parse", "HEAD").Output()
		if err != nil {
			return nil, fmt.Errorf("the pair's checkout %s: %w", filepath.Base(src), err)
		}
		dst := filepath.Join(dir, filepath.Base(src))
		if err := cloneAsOf(ctx, src, strings.TrimSpace(string(head)), dst); err != nil {
			return nil, err
		}
		out = append(out, dst)
	}
	return out, nil
}

// cloneAsOf clones a repository at a commit with nothing after it: every other ref deleted and what only they
// reached pruned, so no command of the agent's finds the fix.
func cloneAsOf(ctx context.Context, src, commit, dst string) error {
	steps := [][]string{
		{"clone", "-q", "--no-local", "--no-checkout", src, dst},
		{"-C", dst, "checkout", "-q", "--detach", commit},
	}
	for _, s := range steps {
		if out, err := evalGit(ctx, s...).CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %v: %s", subcommand(s), err, out)
		}
	}
	refs, err := evalGit(ctx, "-C", dst, "for-each-ref", "--format=%(refname)").Output()
	if err != nil {
		return err
	}
	for _, r := range strings.Fields(string(refs)) {
		if out, err := evalGit(ctx, "-C", dst, "update-ref", "--no-deref", "-d", r).CombinedOutput(); err != nil {
			return fmt.Errorf("delete %s: %v: %s", r, err, out)
		}
	}
	for _, s := range [][]string{{"-C", dst, "checkout", "-q", "-b", "main"}, {"-C", dst, "remote", "remove", "origin"},
		{"-C", dst, "reflog", "expire", "--expire=now", "--all"}, {"-C", dst, "gc", "-q", "--prune=now"}} {
		if out, err := evalGit(ctx, s...).CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %v: %s", strings.Join(s[2:], " "), err, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
		return fmt.Errorf("clone has an alternates file or cannot verify its absence: %v", err)
	}
	out, err := evalGit(ctx, "-C", dst, "fsck", "--no-reflogs", "--unreachable").CombinedOutput()
	if err != nil || strings.Contains(string(out), "unreachable ") {
		return fmt.Errorf("clone retains unreachable objects: %s: %v", out, err)
	}
	return removeEscapingLinks(ctx, dst)
}

// subcommand is the git command of a call's arguments, past its -C DIR.
func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-C" {
			i++
			continue
		}
		return args[i]
	}
	return "?"
}

func evalGit(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append(GitSafety(), args...)...)
	cmd.Env = GitEnv()
	return cmd
}

// repositoryAsOf clones a repository as its default branch stood at the end, and says whether there was one: a
// repository with no commit by then did not exist yet.
func repositoryAsOf(ctx context.Context, src string, end time.Time, dst string) (bool, error) {
	ref, err := evalGit(ctx, "-C", src, "symbolic-ref", "refs/remotes/origin/HEAD").Output()
	// origin/HEAD names the default branch, but only its local head is carried by --no-local.
	if err == nil {
		ref = []byte(strings.Replace(strings.TrimSpace(string(ref)), "refs/remotes/origin/", "refs/heads/", 1))
		_, err = evalGit(ctx, "-C", src, "show-ref", "--verify", string(ref)).Output()
	}
	if err != nil {
		for _, branch := range []string{"refs/heads/main", "refs/heads/master"} {
			if _, checkErr := evalGit(ctx, "-C", src, "show-ref", "--verify", branch).Output(); checkErr == nil {
				ref = []byte(branch)
				err = nil
				break
			}
		}
	}
	if err != nil {
		return false, fmt.Errorf("default branch: %w", err)
	}
	raw, err := branchAsOf(ctx, src, strings.TrimSpace(string(ref)), end)
	if err != nil {
		return false, fmt.Errorf("the default branch of %s: %w", src, err)
	}
	commit := strings.TrimSpace(string(raw))
	if commit == "" {
		return false, nil
	}
	return true, cloneAsOf(ctx, src, commit, dst)
}

// branchAsOf prefers the branch's recorded position when its reflog covers the end.
// Without that history, commit dates are the available approximation.
func branchAsOf(ctx context.Context, src, branch string, end time.Time) ([]byte, error) {
	log, err := evalGit(ctx, "-C", src, "reflog", "show", "--format=%gD", "--date=iso-strict", branch).Output()
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
		_, stamp, ok := strings.Cut(line, "@{")
		if !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339, strings.TrimSuffix(stamp, "}"))
		if err != nil || at.After(end) {
			continue
		}
		out, err := evalGit(ctx, "-C", src, "rev-parse", "--verify", branch+"@{"+end.Format(time.RFC3339)+"}").Output()
		if err != nil {
			return nil, err
		}
		// a clone carries what the source's branches and tags reach: a commit only the reflog holds (the branch reset
		// or rebased after the end) is not in it, so the commit dates set the clone then
		held, err := evalGit(ctx, "-C", src, "for-each-ref", "--contains", strings.TrimSpace(string(out)), "--count=1", "--format=%(refname)",
			"refs/heads", "refs/tags").Output()
		if err == nil && strings.TrimSpace(string(held)) != "" {
			return out, nil
		}
		break
	}
	before := time.Unix(end.Add(-time.Nanosecond).Unix(), 0)
	return evalGit(ctx, "-C", src, "rev-list", "-1", "--before="+before.Format(time.RFC3339), branch, "--").Output()
}

// removeEscapingLinks leaves historical link blobs intact but removes unsafe work-tree links without following them.
func removeEscapingLinks(ctx context.Context, dst string) error {
	root, err := filepath.EvalSymlinks(dst)
	if err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	raw, err := evalGit(ctx, "-C", dst, "ls-files", "--stage", "-z").Output()
	if err != nil {
		return err
	}
	var remove []string
	for _, entry := range strings.Split(string(raw), "\x00") {
		if !strings.HasPrefix(entry, "120000 ") {
			continue
		}
		_, name, ok := strings.Cut(entry, "\t")
		if !ok {
			return fmt.Errorf("malformed tracked link")
		}
		link := filepath.Join(root, name)
		target, err := filepath.EvalSymlinks(link)
		// Unresolvable links cannot prove containment either.
		rel, relErr := filepath.Rel(root, target)
		if err != nil || relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			remove = append(remove, link)
		}
	}
	for _, link := range remove {
		if err := os.Remove(link); err != nil {
			return err
		}
	}
	return nil
}

// The programs a trial runs are the evaluation's own copies, made once when it starts: a rebuild or a reinstall of
// momentum or tacit while it runs changes neither what the door runs nor what a sandbox holds, between the two arms
// of a pair or between trials, and results.json names the build, the hash and tacit's version that did run.
type programs struct {
	momentum string            // the door's momentum
	inside   string            // the sandbox's: its init and the door's client inside
	tacit    string            // the door's tacit, "" when the evaluation reads none
	hashes   map[string]string // the SHA-256 of each copy, by role
	version  string            // tacit's version
}

// pin copies the programs a trial runs into the evaluation's own bin directory.
func (e *Eval) pin() error {
	momentum := firstOf(e.Runner.Momentum, e.Runner.Agent.Self)
	inside := e.Runner.Agent.Self
	if err := CheckDoorClient(inside); err != nil {
		return err
	}
	if momentum == "" {
		return fmt.Errorf("no momentum to run the trials' commands: set the runner's Momentum")
	}
	bin := filepath.Join(e.Dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		return err
	}
	p := programs{hashes: map[string]string{}}
	var err error
	if p.momentum, p.hashes["momentum"], err = pinProgram(momentum, bin, "momentum"); err != nil {
		return fmt.Errorf("momentum: %w", err)
	}
	if p.inside, p.hashes["sandbox"], err = pinProgram(inside, bin, "momentum-door"); err != nil {
		return fmt.Errorf("the sandbox's momentum-door: %w", err)
	}
	if e.Tacit != "" && e.Runner.Tacit != "" {
		if p.tacit, p.hashes["tacit"], err = pinProgram(e.Runner.Tacit, bin, "tacit"); err != nil {
			return fmt.Errorf("tacit: %w", err)
		}
		p.version = programVersion(p.tacit, e.Runner.Bins)
	}
	e.programs = p
	return nil
}

// pinProgram copies a program, a copy and not a link, so replacing the original changes nothing: it returns the
// copy's path and the SHA-256 of its content.
func pinProgram(src, dir, name string) (path, hash string, err error) {
	in, err := os.Open(src)
	if err != nil {
		return "", "", err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return "", "", err
	}
	defer os.Remove(tmp.Name())
	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, sum), in); err != nil {
		tmp.Close()
		return "", "", err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		return "", "", err
	}
	path = filepath.Join(dir, name)
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", "", err
	}
	return path, hex.EncodeToString(sum.Sum(nil)), nil
}

// programVersion is what a program says for `version`, one line, or "unknown".
func programVersion(prog string, bins []string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, prog, "version")
	cmd.Env = CleanEnv(nil, bins...)
	out, err := cmd.Output()
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if err != nil || line == "" {
		return "unknown"
	}
	return plain.Clip(plain.Line(line), 80)
}
