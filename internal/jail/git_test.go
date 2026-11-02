package jail

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A writable mount that is a repository. A run tries to plant a hook, a planted command that only creates a marker
// file, and a process outside the sandbox (the loop, the owner, another agent) commits afterwards. The tests make their
// repositories and run their commits with /usr/bin/git, the git the sandbox holds under /usr, so one git does both.
// Outside the sandbox the same script plants a hook that does run at that commit, so each test can fail; inside, none
// does.
const gitBin = "/usr/bin/git"

// gitIdentity is who commits, in the sandbox and outside it, so a commit fails only for what the test is about.
var gitIdentity = []string{"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid",
	"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid"}

// needGit skips the test where the sandbox would hold no git, since it holds only what is under /usr.
func needGit(t *testing.T) {
	t.Helper()
	if st, err := os.Stat(gitBin); err != nil || st.Mode().Perm()&0o111 == 0 {
		t.Skipf("this machine has no %s, and the sandbox holds only the git under /usr", gitBin)
	}
}

// gitEnv is the environment git runs in outside the sandbox: an empty home and no system file, so none of the
// machine's or the user's configuration reaches it.
func gitEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	return append([]string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, "xdg"), "GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0", "PATH=/usr/bin:/bin"}, gitIdentity...)
}

// runGit runs git in dir and fails the test when it fails.
func runGit(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(gitBin, args...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// A fixture is a repository made outside the sandbox: two commits that hold a file with the marker word.
type fixture struct {
	noHooks bool // made with git init --template=<an empty directory>: it has no hooks directory
	sparse  bool // git sparse-checkout init has run: extensions.worktreeConfig is on and .git/config.worktree exists
}

// newRepo makes the repository, a file name for the planted command's marker that no sandbox mounts, and the
// environment git runs in outside.
func (f fixture) newRepo(t *testing.T) (repo, ran string, env []string) {
	t.Helper()
	repo, ran, env = t.TempDir(), filepath.Join(t.TempDir(), "hook-ran-"+marker), gitEnv(t)
	args := []string{"init", "-q"}
	if f.noHooks {
		args = append(args, "--template="+t.TempDir())
	}
	runGit(t, repo, env, args...)
	runGit(t, repo, env, "symbolic-ref", "HEAD", "refs/heads/main")
	write(t, filepath.Join(repo, "file.txt"), marker+"\n")
	runGit(t, repo, env, "add", "file.txt")
	runGit(t, repo, env, "commit", "-q", "-m", "first")
	runGit(t, repo, env, "commit", "-q", "--allow-empty", "-m", "second")
	if f.sparse {
		runGit(t, repo, env, "sparse-checkout", "init")
		if _, err := os.Stat(filepath.Join(repo, ".git", "config.worktree")); err != nil {
			t.Fatalf("the sparse fixture has no .git/config.worktree: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "hooks")); f.noHooks && err == nil {
		t.Fatal("the fixture was made with an empty template and has a hooks directory")
	}
	return repo, ran, env
}

// treeDigest is a hash of every path under dir with its mode and its content or link target: two digests are equal
// when nothing under dir changed.
func treeDigest(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(h, "%s %o\n", rel, info.Mode())
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			io.WriteString(h, target)
		case info.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			h.Write(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// commitOutside is what a process outside the sandbox does after a run: it commits in the repository. It says whether
// the planted command ran then, which it knows by the file the command creates.
func commitOutside(t *testing.T, repo, ran string, env []string) bool {
	t.Helper()
	runGit(t, repo, env, "commit", "-q", "--allow-empty", "-m", "after the run")
	_, err := os.Stat(ran)
	return err == nil
}

// plant is the shell function every attack script starts with: it writes the planted command, a hook that only
// creates the file $RAN, and makes it executable.
const plant = `plant() { printf '#!/bin/sh\ntouch %s\n' "$RAN" > "$1" && chmod +x "$1"; }
`

// An attack is one way a run could plant a hook in a repository that is its writable mount: the repository it gets,
// what it tries (a shell script that runs in the repository), the words its output must hold (what was refused) and
// the paths of the repository that must not exist after the run.
type attack struct {
	fixture
	script string
	want   []string
	absent []string
}

// result is what an attack left behind.
type result struct {
	repo    string // the repository
	out     string // what the script printed
	changed bool   // whether anything under .git changed
	ran     bool   // whether the planted command ran at the commit outside afterwards
}

// runOutside runs the script on a repository of its own on the machine itself, then commits.
func (a attack) runOutside(t *testing.T) result {
	t.Helper()
	repo, ran, env := a.newRepo(t)
	before := treeDigest(t, filepath.Join(repo, ".git"))
	cmd := exec.Command("/bin/bash", "-c", plant+a.script)
	cmd.Dir, cmd.Env = repo, append(env, "RAN="+ran)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("outside the sandbox the script failed: %v: %s", err, out)
	}
	changed := treeDigest(t, filepath.Join(repo, ".git")) != before
	return result{repo, string(out), changed, commitOutside(t, repo, ran, env)}
}

// runInside runs the script in a sandbox whose writable mount is a repository of its own, then commits outside.
func (a attack) runInside(t *testing.T, s Spec) result {
	t.Helper()
	repo, ran, env := a.newRepo(t)
	s.Mounts, s.Dir = []Mount{{Path: repo, Write: true}}, repo
	s.Env = append(s.Env, append([]string{"RAN=" + ran}, gitIdentity...)...)
	before := treeDigest(t, filepath.Join(repo, ".git"))
	out, err := run(t, s, plant+a.script)
	if err != nil {
		t.Fatalf("the sandbox did not run the script: %v: %s", err, out)
	}
	changed := treeDigest(t, filepath.Join(repo, ".git")) != before
	return result{repo, out, changed, commitOutside(t, repo, ran, env)}
}

// checkAttack is the whole scenario for one attack. The control comes first: outside the sandbox the script plants a
// hook and a commit runs it, or the test could not tell the sandbox's work from a script that plants nothing. Inside,
// the script is refused, nothing under .git changes, and the commit outside runs no planted command.
func checkAttack(t *testing.T, a attack) {
	t.Helper()
	s := spec(t)
	needGit(t)
	if ctl := a.runOutside(t); !ctl.changed || !ctl.ran {
		t.Fatalf("outside the sandbox the script does not plant a hook that a commit runs (.git changed: %v, command ran: %v):\n%s",
			ctl.changed, ctl.ran, ctl.out)
	}
	in := a.runInside(t, s)
	for _, w := range a.want {
		if !strings.Contains(in.out, w) {
			t.Errorf("want %q in what the run printed:\n%s", w, in.out)
		}
	}
	for _, p := range a.absent {
		if _, err := os.Lstat(filepath.Join(in.repo, p)); err == nil {
			t.Errorf("%s exists after the run", p)
		}
	}
	if in.changed {
		t.Errorf("the run changed something under .git:\n%s", in.out)
	}
	if in.ran {
		t.Errorf("the planted command ran at a commit outside the sandbox, after the run:\n%s", in.out)
	}
}

// Repro 1 of the second review: .git is moved away and a copy takes its place, so the read-only binds stay on the
// moved directory and the copy is an ordinary one. With .git a mount point, the kernel refuses the move and the copy
// over it.
func TestARunCannotReplaceTheGitDirectoryOfAWritableMount(t *testing.T) {
	checkAttack(t, attack{
		script: `cp -a .git .git-copy
mv -T .git-copy .git && echo copied-over || echo refused-copy-over
mv .git .git-old && cp -a .git-old .git && echo replaced-git || echo refused-move
mkdir -p .git/hooks; plant .git/hooks/post-commit && echo planted || echo refused-plant
true`,
		want:   []string{"refused-copy-over", "refused-move", "refused-plant", "Device or resource busy", "Read-only file system"},
		absent: []string{".git-old", ".git/hooks/post-commit"},
	})
}

// Repro 2: a commondir file makes git take its configuration and its hooks from another directory, which the run
// made in its work tree.
func TestARunCannotRedirectGitWithACommondirFile(t *testing.T) {
	checkAttack(t, attack{
		script: `mkdir -p elsewhere/hooks
ln -s "$PWD/.git/objects" elsewhere/objects
ln -s "$PWD/.git/refs" elsewhere/refs
cp .git/config elsewhere/config
plant elsewhere/hooks/post-commit
echo ../elsewhere > .git/commondir && echo redirected || echo refused-commondir
true`,
		want:   []string{"refused-commondir", "Read-only file system"},
		absent: []string{".git/commondir"},
	})
}

// Repro 3: a repository made without a hooks directory, so a guard of the paths that exist when Command runs has
// nothing to bind there. The run cannot make the directory.
func TestARunCannotMakeTheHooksDirectoryARepositoryLacks(t *testing.T) {
	checkAttack(t, attack{
		fixture: fixture{noHooks: true},
		script: `mkdir -p .git/hooks && plant .git/hooks/post-commit && echo planted || echo refused-hooks
true`,
		want:   []string{"refused-hooks", "Read-only file system"},
		absent: []string{".git/hooks"},
	})
}

// Repro 4: .git/config.worktree, which git reads when extensions.worktreeConfig is on (git sparse-checkout init turns
// it on and writes the file), takes a hooksPath that names a directory the run made.
func TestARunCannotWriteTheWorktreeConfigOfARepository(t *testing.T) {
	checkAttack(t, attack{
		fixture: fixture{sparse: true},
		script: `mkdir -p hooks-made
plant hooks-made/post-commit
printf '[core]\n\thooksPath = %s\n' "$PWD/hooks-made" >> .git/config.worktree && echo configured || echo refused-config-worktree
true`,
		want: []string{"refused-config-worktree", "Read-only file system"},
	})
}

// What the whole .git costs a run, and what it leaves: inside a run reads the history and writes its tree, and
// everything that writes the index, a ref or the configuration fails (SPEC states these). Outside the sandbox the same
// commands all work, so each refusal is the sandbox's. The remote is a bare repository on the machine, mounted writable
// like any path the caller gives.
func TestARunReadsAReposHistoryAndWritesItsTreeButCannotCommit(t *testing.T) {
	s := spec(t)
	needGit(t)
	costs := []struct{ name, cmd string }{
		{"add", "git add file.txt"},
		{"commit", "git commit -q -a -m inside"},
		{"config", "git config user.name nobody"},
		{"remote add", "git remote add other /nowhere"},
		{"push -u", "git push -u origin main"},
	}
	var sb strings.Builder
	sb.WriteString("git log --oneline; echo '== tree'; echo changed >> file.txt && echo wrote-tree; git status --short; git diff --stat; git show --stat HEAD; git branch\n")
	for _, c := range costs {
		fmt.Fprintf(&sb, "echo '== %s'; %s 2>&1\n", c.name, c.cmd)
	}
	sb.WriteString("true")
	script := sb.String()
	setup := func() (repo, remote string, env []string) {
		repo, _, env = fixture{}.newRepo(t)
		remote = t.TempDir()
		runGit(t, remote, env, "init", "-q", "--bare")
		runGit(t, repo, env, "remote", "add", "origin", remote)
		return repo, remote, env
	}
	state := func(repo string, env []string) (commits, name, other, upstream string) {
		cfg := func(key string) string {
			cmd := exec.Command(gitBin, "config", "--get", key)
			cmd.Dir, cmd.Env = repo, env
			out, _ := cmd.Output() // unset is a non-zero exit and no output
			return strings.TrimSpace(string(out))
		}
		return strings.TrimSpace(runGit(t, repo, env, "rev-list", "--count", "HEAD")), cfg("user.name"), cfg("remote.other.url"), cfg("branch.main.remote")
	}

	// the control, outside the sandbox: every command works
	repo, _, env := setup()
	cmd := exec.Command("/bin/bash", "-c", script)
	cmd.Dir, cmd.Env = repo, env
	if out, err := cmd.CombinedOutput(); err != nil || strings.Contains(string(out), "Read-only file system") {
		t.Fatalf("outside the sandbox the commands do not all work: %v: %s", err, out)
	}
	if commits, name, other, upstream := state(repo, env); commits != "3" || name != "nobody" || other != "/nowhere" || upstream != "origin" {
		t.Fatalf("outside the sandbox the commands leave %q commits, user.name %q, remote.other.url %q, upstream %q", commits, name, other, upstream)
	}

	// inside
	repo, remote, env := setup()
	s.Mounts, s.Dir = []Mount{{Path: repo, Write: true}, {Path: remote, Write: true}}, repo
	s.Env = append(s.Env, gitIdentity...)
	before := treeDigest(t, filepath.Join(repo, ".git"))
	out, err := run(t, s, script)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	history, _, _ := strings.Cut(out, "== tree")
	if !strings.Contains(history, "second") || !strings.Contains(history, "first") {
		t.Errorf("git log inside does not show the history:\n%s", out)
	}
	for _, w := range []string{"wrote-tree", " M file.txt", "file.txt | 1 +", "* main"} {
		if !strings.Contains(out, w) {
			t.Errorf("want %q in:\n%s", w, out)
		}
	}
	for _, c := range costs {
		_, section, ok := strings.Cut(out, "== "+c.name+"\n")
		if !ok {
			t.Fatalf("the run printed nothing for git %s:\n%s", c.name, out)
		}
		section, _, _ = strings.Cut(section, "\n== ")
		if !strings.Contains(section, "Read-only file system") {
			t.Errorf("git %s is not refused with a read-only file system:\n%s", c.name, section)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "file.txt")); string(got) != marker+"\nchanged\n" {
		t.Errorf("the run's write to its tree is %q", got)
	}
	if treeDigest(t, filepath.Join(repo, ".git")) != before {
		t.Error("the run changed something under .git")
	}
	if commits, name, other, upstream := state(repo, env); commits != "2" || name != "" || other != "" || upstream != "" {
		t.Errorf("the run left %q commits, user.name %q, remote.other.url %q, upstream %q, want 2 commits and no configuration", commits, name, other, upstream)
	}
}

// Command binds the whole .git read-only right after a writable mount that holds a .git directory, and does nothing
// else for a repository. This is the shape of the arguments; the sandbox itself is the tests above.
func TestCommandBindsTheWholeGitDirectoryReadOnlyAfterAWritableMount(t *testing.T) {
	home := t.TempDir()
	argsOf := func(s Spec) []string {
		t.Helper()
		cmd, err := s.Command(context.Background(), "true")
		if err != nil {
			t.Fatal(err)
		}
		Close(cmd)
		return cmd.Args
	}
	contains := func(args []string, seq ...string) bool {
		for i := 0; i+len(seq) <= len(args); i++ {
			if strings.Join(args[i:i+len(seq)], "\x00") == strings.Join(seq, "\x00") {
				return true
			}
		}
		return false
	}
	// binds counts the binds whose source is the path or under it
	binds := func(args []string, path string) (n int) {
		for i, a := range args {
			if (a == "--bind" || a == "--ro-bind") && i+1 < len(args) && (args[i+1] == path || strings.HasPrefix(args[i+1], path+"/")) {
				n++
			}
		}
		return n
	}
	repo := t.TempDir()
	dotgit := filepath.Join(repo, ".git")
	write(t, filepath.Join(dotgit, "config"), "[core]\n")
	write(t, filepath.Join(dotgit, "hooks", "pre-commit.sample"), "# sample\n")
	args := argsOf(Spec{Home: home, Mounts: []Mount{{Path: repo, Write: true}}})
	if !contains(args, "--bind", repo, repo, "--ro-bind", dotgit, dotgit) {
		t.Errorf("want .git read-only right after the writable mount: %v", args)
	}
	if n := binds(args, repo); n != 2 {
		t.Errorf("want the mount and .git, and no bind of a path in .git, got %d binds under the repository: %v", n, args)
	}
	if args := argsOf(Spec{Home: home, Mounts: []Mount{{Path: repo}}}); binds(args, repo) != 1 {
		t.Errorf("a read-only mount needs no extra bind: %v", args)
	}
	second := t.TempDir() // every writable mount that holds a .git gets it read-only, not the first alone
	write(t, filepath.Join(second, ".git", "config"), "[core]\n")
	secondGit := filepath.Join(second, ".git")
	if args := argsOf(Spec{Home: home, Mounts: []Mount{{Path: repo, Write: true}, {Path: second, Write: true}}}); !contains(args, "--bind", repo, repo, "--ro-bind", dotgit, dotgit) ||
		!contains(args, "--bind", second, second, "--ro-bind", secondGit, secondGit) {
		t.Errorf("want .git read-only after each of two writable mounts: %v", args)
	}
	noGit := t.TempDir()
	if args := argsOf(Spec{Home: home, Mounts: []Mount{{Path: noGit, Write: true}}}); binds(args, noGit) != 1 {
		t.Errorf("a directory with no .git needs no extra bind: %v", args)
	}
	linked := t.TempDir() // a linked work tree: .git is a file that links outside
	write(t, filepath.Join(linked, ".git"), "gitdir: /synthetic/main/.git/worktrees/w\n")
	if args := argsOf(Spec{Home: home, Mounts: []Mount{{Path: linked, Write: true}}}); binds(args, linked) != 1 {
		t.Errorf("a .git file is bound: %v", args)
	}
	linkedDir := t.TempDir() // a .git that is a link: left alone as well, and not refused
	if err := os.Symlink(dotgit, filepath.Join(linkedDir, ".git")); err != nil {
		t.Fatal(err)
	}
	if args := argsOf(Spec{Home: home, Mounts: []Mount{{Path: linkedDir, Write: true}}}); binds(args, linkedDir) != 1 {
		t.Errorf("a .git link is bound: %v", args)
	}
	file := filepath.Join(t.TempDir(), "result.json") // a writable single file is not a repository
	write(t, file, "{}\n")
	if args := argsOf(Spec{Home: home, Mounts: []Mount{{Path: file, Write: true}}}); binds(args, file) != 1 {
		t.Errorf("a writable file has an extra bind: %v", args)
	}
}
