package loop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/agent-sh/momentum/internal/store"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

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
	out, _, err := cloneRepositoriesCounted(ctx, sources, end, dir)
	return out, err
}

// cloneRepositoriesCounted also returns how many repositories the scrubber refused (what it would change in them):
// they are left out and counted, as the loop's own pass does, not a failure of the task's inputs.
func cloneRepositoriesCounted(ctx context.Context, sources []string, end time.Time, dir string) ([]string, int, error) {
	if len(sources) == 0 {
		return nil, 0, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, 0, err
	}
	var out []string
	refused := 0
	for _, src := range sources {
		dst := filepath.Join(dir, fmt.Sprintf("repo-%d", len(out)+1))
		made, err := repositoryAsOf(ctx, src, end, dst)
		if errors.Is(err, errNeedsScrubbing) {
			refused++
			removeInputTree(dst)
			continue
		}
		if err != nil {
			return nil, refused, err
		}
		if made {
			out = append(out, dst)
		}
	}
	return out, refused, nil
}

// cloneAsOf clones a repository at a commit with nothing after it: every other ref deleted and what only they
// reached pruned, so no command of the agent's finds the fix.
func cloneAsOf(ctx context.Context, src, commit, dst string) error {
	steps := [][]string{
		{"clone", "-q", "--no-local", "--no-checkout", src, dst},
		{"-C", dst, "checkout", "-q", "--detach", commit},
	}
	for _, s := range steps {
		if _, err := evalGit(ctx, s...).CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w", subcommand(s), err)
		}
	}
	refs, err := evalGit(ctx, "-C", dst, "for-each-ref", "--format=%(refname)").Output()
	if err != nil {
		return err
	}
	for _, r := range strings.Fields(string(refs)) {
		if _, err := evalGit(ctx, "-C", dst, "update-ref", "--no-deref", "-d", r).CombinedOutput(); err != nil {
			return fmt.Errorf("delete ref: %w", err)
		}
	}
	for _, s := range [][]string{{"-C", dst, "checkout", "-q", "-b", "main"}, {"-C", dst, "remote", "remove", "origin"},
		{"-C", dst, "reflog", "expire", "--expire=now", "--all"}, {"-C", dst, "gc", "-q", "--prune=now"}} {
		if _, err := evalGit(ctx, s...).CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w", subcommand(s), err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
		return fmt.Errorf("clone has an alternates file or cannot verify its absence: %v", err)
	}
	out, err := evalGit(ctx, "-C", dst, "fsck", "--no-reflogs", "--unreachable").CombinedOutput()
	if err != nil || strings.Contains(string(out), "unreachable ") {
		return fmt.Errorf("clone retains unreachable objects or failed verification")
	}
	if err := os.RemoveAll(filepath.Join(dst, ".git", "hooks")); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(dst, ".git", "logs")); err != nil {
		return err
	}
	if err := removeEscapingLinks(ctx, dst); err != nil {
		return err
	}
	return checkCloneObjects(ctx, dst)
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
	cmd := exec.CommandContext(ctx, "git", append(append(GitSafety(), "-c", "uploadpack.packObjectsHook=", "-c", "protocol.file.allow=always"), args...)...)
	cmd.Env = GitEnv()
	return cmd
}

// repositoryAsOf clones a repository as its default branch stood at the end, and says whether there was one: a
// repository with no commit by then did not exist yet.
func repositoryAsOf(ctx context.Context, src string, end time.Time, dst string) (bool, error) {
	var err error
	src, err = filepath.Abs(src)
	if err != nil {
		return false, err
	}
	if err := cloneSource(ctx, src, []string{dst}); err != nil {
		return false, err
	}
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
	before := time.Unix(end.Add(-time.Nanosecond).Unix(), 0)
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
		if err != nil || at.After(before) {
			continue
		}
		out, err := evalGit(ctx, "-C", src, "rev-parse", "--verify", branch+"@{"+before.Format(time.RFC3339)+"}").Output()
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

// programs pins the same evaluating binary for host records and run readers.
type programs struct {
	momentum string
	hashes   map[string]string
}

func (e *Eval) pinRecordsBinary() error {
	src := e.Runner.Inputs.Momentum
	if src == "" {
		return fmt.Errorf("records need the evaluating momentum binary")
	}
	bin := filepath.Join(e.Dir, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		return err
	}
	path, hash, err := copyRecordsProgram(src, bin, "momentum")
	if err != nil {
		return err
	}
	e.programs = programs{momentum: path, hashes: map[string]string{"momentum": hash, "momentum-ro": hash}}
	return nil
}

// copyRecordsProgram copies a program, a copy and not a link, so replacing the original changes nothing: it returns the
// copy's path and the SHA-256 of its content.
func copyRecordsProgram(src, dir, name string) (path, hash string, err error) {
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
