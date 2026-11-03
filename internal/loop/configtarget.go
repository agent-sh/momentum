package loop

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/agent-sh/momentum/internal/cfgpaths"
	"github.com/agent-sh/momentum/internal/store"
	"golang.org/x/sys/unix"
)

// Package kinds. A repository package lands through a pull request; a configuration package changes one file of the
// agent system's configuration under the home directory, in place, from the host.
const (
	KindRepo   = "repo"
	KindConfig = "config"
)

// ConfigTargets is the loop's authority over configuration files: off unless the owner turns it on, and then only the
// files Allow names, each inside the files the configuration copy reads (cfgpaths, one list for both) and under the
// stricter target denylist. An empty Allow admits nothing.
type ConfigTargets struct {
	Enabled bool
	// Allow names the targets: exact home-relative files, or directories whose existing files they admit.
	Allow []string
	Home  string // the real home the targets resolve under
}

func configTargetName(name string) (string, error) {
	n, err := cfgpaths.TargetName(name)
	if err != nil {
		return "", refused("%v", err)
	}
	return n, nil
}

func configRootOf(name string) string { return cfgpaths.RootOf(name) }

// dropIn says whether a path lies in a drop-in directory (NAME.d), whose files a program reads by their presence.
func dropIn(name string) bool {
	parts := strings.Split(name, "/")
	for _, part := range parts[:len(parts)-1] {
		if strings.HasSuffix(part, ".d") {
			return true
		}
	}
	return false
}

// admit says whether a file is a configuration target: its clean name, or the refusal.
func (ct ConfigTargets) admit(name string) (string, error) {
	name, err := configTargetName(name)
	if err != nil {
		return "", err
	}
	root := configRootOf(name)
	if root == "" {
		return "", refused("%s lies outside the configuration the loop reads", name)
	}
	if reason := cfgpaths.TargetDenied(root, name); reason != "" {
		return "", refused("%s is no configuration target: %s", name, reason)
	}
	if !configPathAllowed(root, name, false) {
		return "", refused("%s is no configuration target: not configuration", name)
	}
	if len(ct.Allow) == 0 {
		return "", refused("loop.config_targets.allow names no file: no configuration target is admitted")
	}
	exact, below := false, false
	for _, entry := range ct.Allow {
		path, tree, err := cfgpaths.AllowEntry(entry)
		if err != nil {
			return "", refused("%v: no target is admitted", err)
		}
		exact = exact || !tree && name == path
		below = below || strings.HasPrefix(name, path+"/")
	}
	switch {
	case !exact && !below:
		return "", refused("%s is outside loop.config_targets.allow", name)
	case dropIn(name) && !exact:
		return "", refused("%s is in a drop-in directory: a target there is one loop.config_targets.allow names exactly", name)
	}
	return name, nil
}

// ConfigState is a target file's content as the host read it: absent, or its hash and mode (with the content, in a
// journal).
type ConfigState struct {
	Absent  bool   `json:"absent,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Mode    uint32 `json:"mode,omitempty"`
	Content []byte `json:"content,omitempty"`
}

// targetUID is the owner a target's file must have: the caller (a test names another).
var targetUID = os.Getuid

func (c ConfigState) same(o ConfigState) bool { return c.Absent == o.Absent && c.SHA256 == o.SHA256 }

func (c ConfigState) bare() *ConfigState {
	return &ConfigState{Absent: c.Absent, SHA256: c.SHA256, Mode: c.Mode}
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// targetText says why a target's content is refused: more than the copy's file limit, invalid UTF-8, a NUL anywhere, or a
// launcher of .local/bin that is no script.
func targetText(root string, b []byte) string {
	switch {
	case len(b) > MaxConfigFileBytes:
		return "file bytes"
	case bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b):
		return "not text"
	case root == ".local/bin" && !scriptShebang(b):
		return "not a script"
	}
	return ""
}

// targetDir is a target's directory, opened from the home one component at a time without following a link.
type targetDir struct {
	fd         int
	name, base string // the home-relative name and its last component
	root       string
}

func (d *targetDir) Close() { unix.Close(d.fd) }

// open resolves a target under the real home: every directory without a link, the file itself no link.
func (ct ConfigTargets) open(name string) (*targetDir, error) {
	name, err := ct.admit(name)
	if err != nil {
		return nil, err
	}
	if ct.Home == "" || !filepath.IsAbs(ct.Home) {
		return nil, refused("configuration targets need the real home")
	}
	if err := noInputLinks(ct.Home); err != nil {
		return nil, refused("the home path holds a link")
	}
	fd, err := unix.Open(ct.Home, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(name, "/")
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		switch {
		case err == unix.ELOOP || err == unix.ENOTDIR:
			return nil, refused("%s: a directory of its path is a link or no directory", name)
		case err == unix.ENOENT:
			return nil, refused("%s: its directory is not there (a target is a file in a directory that exists)", name)
		case err != nil:
			return nil, err
		}
		fd = next
	}
	return &targetDir{fd: fd, name: name, base: parts[len(parts)-1], root: configRootOf(name)}, nil
}

// read reads a file of the directory once: absent, or a regular file of the caller's without a link, within the limits.
func (d *targetDir) read(file string) (ConfigState, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(d.fd, file, &st, unix.AT_SYMLINK_NOFOLLOW); err == unix.ENOENT {
		return ConfigState{Absent: true}, nil
	} else if err != nil {
		return ConfigState{}, err
	}
	switch {
	case st.Mode&unix.S_IFMT == unix.S_IFLNK:
		return ConfigState{}, refused("%s is a link", d.name)
	case st.Mode&unix.S_IFMT != unix.S_IFREG:
		return ConfigState{}, refused("%s is no regular file", d.name)
	case int(st.Uid) != targetUID():
		return ConfigState{}, refused("%s is not owned by the caller", d.name)
	case st.Nlink > 1:
		return ConfigState{}, refused("%s has another hard link: a change to it would not be the file's alone", d.name)
	case st.Size > MaxConfigFileBytes:
		return ConfigState{}, refused("%s exceeds the configuration file limit", d.name)
	}
	fd, err := unix.Openat(d.fd, file, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return ConfigState{}, err
	}
	f := os.NewFile(uintptr(fd), file)
	defer f.Close()
	var again unix.Stat_t
	if err := unix.Fstat(fd, &again); err != nil {
		return ConfigState{}, err
	}
	if again.Ino != st.Ino || again.Dev != st.Dev || again.Mode&unix.S_IFMT != unix.S_IFREG {
		return ConfigState{}, refused("%s changed while it was read", d.name)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxConfigFileBytes+1))
	if err != nil {
		return ConfigState{}, err
	}
	if reason := targetText(d.root, b); reason != "" {
		return ConfigState{}, refused("%s is no configuration text: %s", d.name, reason)
	}
	return ConfigState{SHA256: hashOf(b), Mode: uint32(again.Mode & 0o777), Content: b}, nil
}

func (d *targetDir) state() (ConfigState, error) { return d.read(d.base) }

func (d *targetDir) sync() error { return unix.Fsync(d.fd) }

// settle clears what an interrupted write left beside the target (swap, its fixed scratch name): the content the step
// wrote or the content it displaced as expected. Anything else was displaced by an exchange that should not have
// happened: it goes back, and the step is refused.
func (d *targetDir) settle(swap string, ours, expected ConfigState) error {
	left, err := d.read(swap)
	if err != nil {
		return refused("an interrupted write left %s beside %s, which the loop cannot read: the owner decides: %v", swap, d.name, err)
	}
	if left.Absent {
		return nil
	}
	if left.SHA256 == ours.SHA256 || left.SHA256 == expected.SHA256 {
		return d.unlinkIf(swap, left.SHA256)
	}
	cur, err := d.state()
	if err == nil && !cur.Absent && cur.SHA256 == ours.SHA256 {
		if err := exchange(d.fd, swap, d.fd, d.base, unix.RENAME_EXCHANGE); err != nil {
			return fmt.Errorf("%s: the exchange back failed, the journal stays: %w", d.name, err)
		}
		if err := d.unlinkIf(swap, ours.SHA256); err != nil {
			return err
		}
		return refused("%s had changed: its content is back and the change is not applied", d.name)
	}
	return refused("an interrupted write left %s beside %s: the owner decides", swap, d.name)
}

// unlinkIf removes the swap file only while it holds what the step knows it holds; anything else stays for the owner.
// That is no refusal: the step's journal and state stay as they are.
func (d *targetDir) unlinkIf(swap, hash string) error {
	left, err := d.read(swap)
	if err == nil && left.Absent {
		return nil
	}
	if err != nil || left.SHA256 != hash {
		return fmt.Errorf("%s beside %s does not hold what the step put there: it is left for the owner", swap, d.name)
	}
	return errors.Join(unix.Unlinkat(d.fd, swap, 0), d.sync())
}

// replace puts content in place of the target atomically, only while the target is what expect says: the target is
// exchanged with the new file, the displaced file is checked, and it is exchanged back when it was not the content
// expected. The file exists: a configuration change never creates or removes one.
func (d *targetDir) replace(swap string, content []byte, mode uint32, expect ConfigState) error {
	if expect.Absent {
		return refused("%s: a configuration change edits a file that exists", d.name)
	}
	fd, err := unix.Openat(d.fd, swap, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), swap)
	_, err = f.Write(content)
	err = errors.Join(err, f.Chmod(os.FileMode(mode&0o777)), f.Sync(), f.Close())
	if err != nil {
		unix.Unlinkat(d.fd, swap, 0) // created exclusively by this step and never exchanged
		return err
	}
	ours := hashOf(content)
	drift := refused("%s changed since it was read: the change is not applied", d.name)
	if err := exchange(d.fd, swap, d.fd, d.base, unix.RENAME_EXCHANGE); err != nil {
		cleanup := d.unlinkIf(swap, ours)
		switch err {
		case unix.ENOENT:
			return errors.Join(drift, cleanup)
		case unix.EINVAL:
			return errors.Join(refused("the file system of %s cannot exchange files atomically", d.name), cleanup)
		}
		return errors.Join(err, cleanup)
	}
	displaced, err := d.read(swap)
	if err != nil || displaced.Absent || displaced.SHA256 != expect.SHA256 {
		if back := exchange(d.fd, swap, d.fd, d.base, unix.RENAME_EXCHANGE); back != nil {
			// not a refusal: the file may hold the change while what it held sits at the swap name
			return fmt.Errorf("%s changed while it was written, and the exchange back failed (%v): the journal stays, the owner decides", d.name, back)
		}
		if err := d.unlinkIf(swap, ours); err != nil {
			return err
		}
		return drift
	}
	return d.unlinkIf(swap, expect.SHA256)
}

func swapName(caseID string) string { return ".momentum-" + caseID + ".swap" }

// ConfigJournal is a configuration landing's record, written before the file is: what the file held (its content,
// hash and mode) and what the change puts there.
type ConfigJournal struct {
	Target string      `json:"target"`
	Old    ConfigState `json:"old"`
	New    ConfigState `json:"new"`
	Head   string      `json:"head"`
}

// ConfigRollback is a configuration rollback's record, written before the file is: what it overwrites (the landed
// content, or what the owner forced it over) and the content it restores.
type ConfigRollback struct {
	From  ConfigState `json:"from"`
	To    ConfigState `json:"to"`
	Force bool        `json:"force,omitempty"`
	Why   string      `json:"why"`
}

const (
	configJournal  = "config-journal.json"
	configRollback = "config-rollback.json"
)

// writeDurable writes a host-only record (0600) and syncs it and its directory before it returns.
func writeDurable(dir, name string, v any) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	var suffix [6]byte
	rand.Read(suffix[:])
	tmp := filepath.Join(dir, "."+name+"."+hex.EncodeToString(suffix[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(raw)
	if err = errors.Join(err, f.Sync(), f.Close()); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		os.Remove(tmp)
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

func readRecord(dir, name string, v any) (bool, error) {
	f, err := OpenFile(dir, name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	raw, err := ReadFile(f, 4<<20)
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, v)
}

// configAuthority checks a configuration package's target again: admitted now, and readable under the home without a
// link.
func (s *Ship) configAuthority(p Package) (*targetDir, error) {
	if p.Target == "" {
		return nil, refused("the configuration package names no target")
	}
	d, err := s.ConfigTargets.open(p.Target)
	if err != nil {
		if errors.Is(err, ErrRefused) {
			return nil, &shipHold{err: err}
		}
		return nil, err
	}
	return d, nil
}

// mayApply says why the loop may not write a configuration file now: the switch is off or the loop is not live.
func (s *Ship) mayApply(p Package) string {
	switch {
	case p.Kind != KindConfig:
		return ""
	case !s.ConfigTargets.Enabled:
		return "configuration targets are off (loop.config_targets.enabled): the loop proposes and reviews configuration changes and applies none"
	case !s.Live:
		return "the loop is not live (loop.live): it proposes and does not land"
	}
	return ""
}

// PrepareConfig makes a configuration package: a host-only Git repository whose base commit holds the target as the
// host reads it now, and an independent run clone of it. The target must read through the
// scrubber unchanged, so the run's /ro/config copy and its clone hold the bytes the host will patch.
func (s *Ship) PrepareConfig(ctx context.Context, caseID, target string) (Package, error) {
	ctx = s.gitContext(shipContext(ctx, caseID))
	unlock, err := s.lock(ctx, caseID)
	if err != nil {
		return Package{}, err
	}
	defer unlock()
	c, err := s.working(ctx, caseID)
	if err != nil {
		return Package{}, err
	}
	old, had, err := s.Load(c.ID)
	if err != nil {
		return Package{}, err
	}
	if had && old.State != RolledBack && old.State != Kept {
		return old, refused("case %s has a package already (in %s)", c.ID, old.State)
	}
	d, err := s.configAuthority(Package{Target: target})
	if err != nil {
		return Package{}, err
	}
	defer d.Close()
	base, err := d.state()
	if err != nil {
		return Package{}, err
	}
	if base.Absent {
		return Package{}, refused("%s is not there: a configuration change edits a file that exists", d.name)
	}
	if other, err := s.targetBusy(c.ID, d.name); err != nil {
		return Package{}, err
	} else if other != "" {
		return Package{}, &shipHold{err: refused("%s is the target of case %s's change, which is not done: the loop leaves it alone", d.name, other)}
	}
	clean, err := RedactAll(ctx, []string{d.name, string(base.Content)})
	if err != nil {
		return Package{}, err
	}
	if clean[0] != d.name || clean[1] != string(base.Content) {
		return Package{}, refused("%s holds text the scrubber would mask: a run's copy of it is not the file, so the loop cannot change it", d.name)
	}
	if had {
		if err := s.archive(old); err != nil {
			return Package{}, err
		}
	}
	host, work := filepath.Join(s.Dir, "host", c.ID), s.workDir(c.ID)
	for _, dir := range []string{host, work} {
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			return Package{}, err
		}
		if err := removeAll(dir); err != nil {
			return Package{}, err
		}
	}
	branch := "momentum/" + c.ID
	if err := os.Mkdir(host, 0o700); err != nil {
		return Package{}, err
	}
	if _, err := hostGit(ctx, host, nil, "init", "--quiet", "--template=", "-b", branch); err != nil {
		return Package{}, err
	}
	p := Package{Case: c.ID, Kind: KindConfig, Target: d.name, TargetBase: base.bare(), Branch: branch, Work: work, Host: host,
		State: Prepared, Created: s.now()}
	if p.BaseCommit, err = s.configBase(ctx, p, base, ""); err != nil {
		return Package{}, err
	}
	if err := s.freshRunClone(ctx, p); err != nil {
		return Package{}, err
	}
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return Package{}, err
	}
	return p, s.Store.CaseNote(cleanedNote(ctx), c.ID, "prepared", fmt.Sprintf("configuration %s at %.12s", p.Target, p.BaseCommit), s.Actor)
}

// configBase commits the target's content as a base commit of the host repository (after parent, when there is one)
// and points the package branch at it.
func (s *Ship) configBase(ctx context.Context, p Package, base ConfigState, parent string) (string, error) {
	args := []string{"read-tree", "--empty"}
	if _, err := s.git(ctx, p.Host, args...); err != nil {
		return "", err
	}
	blob, err := s.gitInput(ctx, p.Host, base.Content, "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	mode := "100644"
	if base.Mode&0o111 != 0 {
		mode = "100755"
	}
	if _, err := s.git(ctx, p.Host, "update-index", "--add", "--cacheinfo", mode+","+strings.TrimSpace(blob)+","+p.Target); err != nil {
		return "", err
	}
	tree, err := s.git(ctx, p.Host, "write-tree")
	if err != nil {
		return "", err
	}
	commitArgs := []string{"commit-tree", strings.TrimSpace(tree), "-m", "Configuration base for case " + p.Case}
	if parent != "" {
		commitArgs = append(commitArgs, "-p", parent)
	}
	commit, err := s.git(ctx, p.Host, commitArgs...)
	if err != nil {
		return "", err
	}
	commit = strings.TrimSpace(commit)
	if _, err := s.git(ctx, p.Host, "update-ref", "refs/heads/"+p.Branch, commit); err != nil {
		return "", err
	}
	if _, err := s.git(ctx, p.Host, "read-tree", "--reset", "-u", commit); err != nil {
		return "", err
	}
	return commit, nil
}

// refreshConfigBase takes a moved target as the new base: the file changed since preparation, so the build starts again
// from what it holds now.
func (s *Ship) refreshConfigBase(ctx context.Context, p Package) (Package, error) {
	d, err := s.configAuthority(p)
	if err != nil {
		return p, err
	}
	defer d.Close()
	base, err := d.state()
	if err != nil {
		return p, err
	}
	if base.Absent {
		return p, refused("%s is gone: a configuration change edits a file that exists", d.name)
	}
	clean, err := Redact(ctx, string(base.Content))
	if err != nil {
		return p, err
	}
	if clean != string(base.Content) {
		return p, refused("%s holds text the scrubber would mask: the loop cannot change it", d.name)
	}
	if p.BaseCommit, err = s.configBase(ctx, p, base, p.BaseCommit); err != nil {
		return p, err
	}
	p.TargetBase = base.bare()
	if err := s.freshRunClone(ctx, p); err != nil {
		return p, err
	}
	p.Check, p.Head, p.RefreshBase, p.State = nil, "", false, Prepared
	return p, s.saveAuthorized(ctx, &p)
}

// checkConfigChange refuses a configuration change that does more than change its target and add its tests: every
// other changed path is a test file, the target stays (a change never deletes it), keeps its mode and stays text.
func (s *Ship) checkConfigChange(ctx context.Context, p Package, tree string, paths, testFiles []string) error {
	if !slices.Contains(paths, p.Target) {
		return refused("the change does not change its target %s", p.Target)
	}
	for _, f := range testFiles {
		if !strings.HasPrefix(f, "tests/") {
			return refused("%s: a configuration change's tests lie under tests/", f)
		}
	}
	for _, f := range paths {
		if f != p.Target && !slices.Contains(testFiles, f) {
			return refused("%s is neither the target nor a test file: a configuration change changes one file", f)
		}
	}
	if slices.Contains(testFiles, p.Target) {
		return refused("the target is not a test file")
	}
	_, _, err := s.configContent(ctx, p, tree)
	return err
}

// configContent is the target's content and mode in a tree of the host repository, checked as a target's.
func (s *Ship) configContent(ctx context.Context, p Package, treeish string) ([]byte, uint32, error) {
	entry, err := s.git(ctx, p.Host, "ls-tree", treeish, "--", p.Target)
	if err != nil {
		return nil, 0, err
	}
	fields := strings.Fields(entry)
	switch {
	case len(fields) < 3:
		return nil, 0, refused("the change deletes its target: a configuration change edits or creates it")
	case fields[1] != "blob" || fields[0] != "100644" && fields[0] != "100755":
		return nil, 0, refused("the target is no regular file in the change")
	}
	mode := uint32(0o644)
	if fields[0] == "100755" {
		mode = 0o755
	}
	if p.TargetBase == nil || p.TargetBase.Absent {
		return nil, 0, refused("the configuration package has no base file")
	}
	{
		if (p.TargetBase.Mode&0o111 != 0) != (mode == 0o755) {
			return nil, 0, refused("the change changes its target's mode: a configuration change keeps it")
		}
		mode = p.TargetBase.Mode
	}
	raw, err := s.gitInput(ctx, p.Host, nil, "cat-file", "blob", fields[2])
	if err != nil {
		return nil, 0, err
	}
	if reason := targetText(configRootOf(p.Target), []byte(raw)); reason != "" {
		return nil, 0, refused("the changed target is no configuration text: %s", reason)
	}
	return []byte(raw), mode, nil
}

// landConfig applies a reviewed configuration change to the real file: only when the switch is on and the loop is
// live, its last review is clean and posted on its head, the file is what the base held, and the recorded test
// passes on the head again. The previous content is journaled before the file is written.
func (s *Ship) landConfig(ctx context.Context, p Package) (Package, error) {
	if p.State == Landing {
		return s.resumeConfigLanding(ctx, p)
	}
	switch {
	case p.State != Proposed:
		return p, refused("case %s has no proposed package to land", p.Case)
	case s.mayApply(p) != "":
		return p, refused("%s", s.mayApply(p))
	case !p.Clean():
		return p, refused("case %s's last review is not clean on its head, or was not posted: no landing", p.Case)
	case p.Check == nil:
		return p, refused("case %s's package has no check", p.Case)
	}
	if _, err := s.working(ctx, p.Case); err != nil {
		return p, err
	}
	if head, err := s.git(ctx, p.Host, "rev-parse", "HEAD"); err != nil || strings.TrimSpace(head) != p.Head {
		return p, refused("the host repository's head is not the reviewed %s: no landing", p.Head)
	}
	d, err := s.configAuthority(p)
	if err != nil {
		return p, err
	}
	defer d.Close()
	cur, err := d.state()
	if err != nil {
		return p, err
	}
	if p.TargetBase == nil || !cur.same(*p.TargetBase) {
		p.State, p.RefreshBase = Revising, true
		if err := s.saveAuthorized(ctx, &p); err != nil {
			return p, err
		}
		return p, refused("%s changed since the package was prepared: the change is built again on what it holds now", p.Target)
	}
	tree, err := s.export(ctx, p, "land", p.Head)
	if err != nil {
		return p, err
	}
	command, err := s.checkCommand(p)
	if err != nil {
		return p, errors.Join(err, s.unexport(p, tree))
	}
	a, err := s.Sandbox.Run(ctx, tree, command, s.Env)
	if err = errors.Join(err, s.unexport(p, tree)); err != nil {
		return p, err
	}
	if a.Exit != 0 {
		return p, refused("the test fails on the reviewed head (exit %d): no landing", a.Exit)
	}
	content, mode, err := s.configContent(ctx, p, p.Head)
	if err != nil {
		return p, err
	}
	j := ConfigJournal{Target: p.Target, Old: cur, New: ConfigState{SHA256: hashOf(content), Mode: mode, Content: content}, Head: p.Head}
	if err := s.assignedAuthority(ctx); err != nil {
		return p, err
	}
	if err := writeDurable(s.pkgDir(p.Case), configJournal, j); err != nil {
		return p, err
	}
	p.State, p.LandingHead = Landing, p.Head
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, err
	}
	if err := s.configStage("journaled"); err != nil {
		return p, err
	}
	if err := d.settle(swapName(p.Case), j.New, j.Old); err != nil {
		return p, err
	}
	if err := d.replace(swapName(p.Case), j.New.Content, j.New.Mode, j.Old); err != nil {
		return s.landingRefused(ctx, p, err)
	}
	if err := s.configStage("written"); err != nil {
		return p, err
	}
	return s.finishConfigLanding(ctx, p, j)
}

// landingRefused takes a landing the file refused (it changed under the step) back to its proposal, journal and all.
func (s *Ship) landingRefused(ctx context.Context, p Package, cause error) (Package, error) {
	if !errors.Is(cause, ErrRefused) {
		return p, cause
	}
	p.State, p.LandingHead, p.RefreshBase = Revising, "", true
	if err := os.Remove(filepath.Join(s.pkgDir(p.Case), configJournal)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return p, errors.Join(cause, err)
	}
	return p, errors.Join(cause, s.saveAuthorized(ctx, &p))
}

// resumeConfigLanding finishes a landing a crash interrupted, from its journal: the file holds the new content (done),
// the old (written again), or neither (the owner changed it: refused, journal kept).
func (s *Ship) resumeConfigLanding(ctx context.Context, p Package) (Package, error) {
	if why := s.mayApply(p); why != "" {
		return p, refused("resuming the landing: %s", why)
	}
	if _, err := s.working(ctx, p.Case); err != nil {
		return p, err
	}
	var j ConfigJournal
	ok, err := readRecord(s.pkgDir(p.Case), configJournal, &j)
	if err != nil {
		return p, err
	}
	if !ok || j.Target != p.Target || j.Head != p.LandingHead {
		// the step stopped before its journal: nothing was written
		p.State, p.LandingHead = Proposed, ""
		if err := s.saveAuthorized(ctx, &p); err != nil {
			return p, err
		}
		return s.landConfig(ctx, p)
	}
	d, err := s.configAuthority(p)
	if err != nil {
		return p, err
	}
	defer d.Close()
	if err := d.settle(swapName(p.Case), j.New, j.Old); err != nil {
		return p, err
	}
	cur, err := d.state()
	if err != nil {
		return p, err
	}
	switch {
	case cur.same(j.New):
	case cur.same(j.Old):
		if err := d.replace(swapName(p.Case), j.New.Content, j.New.Mode, j.Old); err != nil {
			return s.landingRefused(ctx, p, err)
		}
	default:
		return p, refused("%s holds neither its old nor its new content: the interrupted landing waits for the owner", p.Target)
	}
	return s.finishConfigLanding(ctx, p, j)
}

func (s *Ship) finishConfigLanding(ctx context.Context, p Package, j ConfigJournal) (Package, error) {
	p.Head, p.State, p.Landed, p.TargetLanded = p.LandingHead, Landed, []string{p.LandingHead}, j.New.SHA256
	if p.LandedAt.IsZero() {
		p.LandedAt = s.now()
	}
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, err
	}
	if err := s.deploymentReceipt(ctx, p); err != nil {
		return p, err
	}
	if _, err := s.Store.DeployCase(ctx, p.Case, s.pkgDir(p.Case), p.LandedAt, deployWhat(p), s.Actor); err != nil {
		return p, err
	}
	return p, nil
}

// deployWhat is what a deployed case's log says went in.
func deployWhat(p Package) string {
	if p.Kind == KindConfig {
		return "configuration " + p.Target + ": " + p.Title
	}
	return p.PR + ": " + p.Title
}

// rollBackConfig restores a landed configuration file's journaled content. The file must still hold what landed;
// one changed since is refused unless the owner forces it, and what it held then is journaled too. A resumed
// rollback reads its journal: the file holds the restored content (done), what the rollback overwrites (written
// again), or neither (refused).
func (s *Ship) rollBackConfig(ctx context.Context, p Package, why string, force, owner bool) (Package, error) {
	if hold := s.mayApply(p); !owner && hold != "" {
		// the loop's own rollbacks (the measure step's, a resumed one) write only while it may write
		return p, refused("the rollback waits: %s", hold)
	}
	c, err := s.Store.CaseByID(ctx, p.Case)
	if err != nil {
		return p, err
	}
	if c.Status != store.CaseDeployed && c.Status != store.CaseMeasuring {
		return p, refused("only a deployed or measuring case has a change to roll back")
	}
	var j ConfigJournal
	ok, err := readRecord(s.pkgDir(p.Case), configJournal, &j)
	if err != nil {
		return p, err
	}
	if !ok || j.Target != p.Target || p.TargetLanded == "" || j.New.SHA256 != p.TargetLanded {
		return p, refused("the landing's journal of %s is missing or does not match its package: restore it by hand", p.Target)
	}
	d, err := s.configAuthority(p)
	if err != nil {
		return p, err
	}
	defer d.Close()
	var rb ConfigRollback
	if p.State == RollingBack {
		ok, err := readRecord(s.pkgDir(p.Case), configRollback, &rb)
		if err != nil {
			return p, err
		}
		if !ok {
			p.State = Landed // the step stopped before its journal: nothing was written
		}
	}
	if p.State == Landed {
		cur, err := d.state()
		if err != nil {
			return p, err
		}
		if cur.same(j.Old) {
			// restored already, by hand or by an earlier step
			rb = ConfigRollback{From: *cur.bare(), To: *j.Old.bare(), Why: why}
		} else {
			if !cur.same(j.New) && !force {
				return p, refused("%s changed since its change landed: the rollback would overwrite that; the owner may force it (--force)", p.Target)
			}
			rb = ConfigRollback{From: cur, To: j.Old, Force: force && !cur.same(j.New), Why: why}
		}
		if err := s.assignedAuthority(ctx); err != nil {
			return p, err
		}
		if err := writeDurable(s.pkgDir(p.Case), configRollback, rb); err != nil {
			return p, err
		}
		p.State, p.Undo = RollingBack, &RollbackJournal{Head: j.Old.SHA256, Why: why}
		if err := s.saveAuthorized(ctx, &p); err != nil {
			return p, err
		}
		if err := s.configStage("rollback journaled"); err != nil {
			return p, err
		}
	}
	if err := d.settle(swapName(p.Case), j.Old, rb.From); err != nil {
		return p, err
	}
	cur, err := d.state()
	if err != nil {
		return p, err
	}
	switch {
	case cur.same(j.Old):
	case cur.same(rb.From):
		if err := d.replace(swapName(p.Case), j.Old.Content, j.Old.Mode, rb.From); err != nil {
			return p, err
		}
	default:
		return p, refused("%s holds neither the content the rollback restores nor what it overwrites: the owner decides", p.Target)
	}
	if err := s.configStage("rolled back"); err != nil {
		return p, err
	}
	p.State = RolledBack
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, err
	}
	return p, s.rollbackReceipt(ctx, p, why)
}

// configInEffect: a configuration change is in effect while its target holds the landed content.
func (s *Ship) configInEffect(p *Package) (string, error) {
	d, err := s.ConfigTargets.open(p.Target)
	if errors.Is(err, ErrRefused) {
		return err.Error(), nil
	}
	if err != nil {
		return "", err
	}
	defer d.Close()
	cur, err := d.state()
	if errors.Is(err, ErrRefused) {
		return err.Error(), nil
	}
	if err != nil {
		return "", err
	}
	if cur.Absent || cur.SHA256 != p.TargetLanded {
		return fmt.Sprintf("%s does not hold the landed content", filepath.Base(p.Target)), nil
	}
	return "", nil
}

// configStage is a test hook between a configuration write's steps: an error stops the step there, as a crash would.
func (s *Ship) configStage(stage string) error {
	if s.ConfigStage != nil {
		return s.ConfigStage(stage)
	}
	return nil
}

// targetBusy names another case whose configuration package targets the same file and is not done (rolled back or
// kept): two changes to one file would overwrite each other and confound both measurements.
func (s *Ship) targetBusy(self, target string) (string, error) {
	entries, err := os.ReadDir(filepath.Join(s.Dir, "packages"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == self || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p, ok, err := s.Load(e.Name())
		if err != nil {
			return "", err
		}
		if ok && p.Kind == KindConfig && p.Target == target && p.State != RolledBack && p.State != Kept {
			return p.Case, nil
		}
	}
	return "", nil
}

// exchange is renameat2; a test injects a failure of the exchange back.
var exchange = unix.Renameat2
