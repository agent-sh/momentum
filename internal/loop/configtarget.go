package loop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agent-sh/momentum/internal/measure"
	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
	"golang.org/x/sys/unix"
)

// A configuration fix is advice. The investigation may name one configuration file; the build changes a copy of it in
// a host-only one-file repository and the host checks the change there, as it checks a repository's. The proposal (the
// file, the hash of the content it was built on, the hash of the content it leaves, the patch, the check and the plan,
// as information) stays under the loop's directory, goes through the loop's clean-context review, and a clean one is
// filed as a low problem note for a person. The loop never writes the file and never measures the change: the case's
// close, as already solved with the fix the note names or as not worth it, ends it, and the detector's recurrence is
// its only watch.

// Package kinds. A repository package lands through a pull request; a configuration package is a proposal a person
// applies by hand to one file under the home.
const (
	KindRepo   = "repo"
	KindConfig = "config"
)

// fixedOwn are home-relative files and directories that are never a configuration target, whatever the
// configuration says: momentum's own configuration and data, and the policy every scheduled run reads.
var fixedOwn = []string{".config/momentum", ".local/share/momentum", ".config/agents/scheduled-ai-policy.md"}

// ConfigTargets are the configuration files the loop may propose a change to (loop.config_targets): none unless
// Enabled, and then only the existing files Allow names exactly. An empty Allow admits nothing. The refusals of
// targetRefusal and of the file checks hold whatever Allow says.
type ConfigTargets struct {
	Enabled bool
	// Allow names the targets: exact files, relative to the home or under ~/. Never a directory or a pattern.
	Allow []string
	Home  string // the real home the targets resolve under
	// Own are the loop's own files and directories as configured (loop.rules, its programs, its relay environment,
	// its tacit store, its configuration directory): absolute, under ~/ or home-relative. No target is one of them,
	// below one, or the file one of them resolves to.
	Own []string
}

// configTargetName is a target's clean home-relative name: a path under the home, under ~/, or under the run's
// /ro/config copy.
func configTargetName(name string) (string, error) {
	n := strings.TrimSpace(name)
	if rest, ok := strings.CutPrefix(n, "/ro/config/"); ok {
		n = rest
	} else if rest, ok := strings.CutPrefix(n, "~/"); ok {
		n = rest
	}
	if !cleanTargetName(n) {
		return "", refused("%q is no clean path of a file under the home", name)
	}
	return n, nil
}

// allowName is an allow entry's name: home-relative or under ~/, clean, exact (no pattern, no trailing slash).
func allowName(entry string) (string, bool) {
	n := strings.TrimSpace(entry)
	if rest, ok := strings.CutPrefix(n, "~/"); ok {
		n = rest
	}
	return n, cleanTargetName(n)
}

// cleanTargetName says a name is a configuration path's one form (store.CheckConfigPath).
func cleanTargetName(n string) bool { return store.CheckConfigPath(n) == nil }

// configRootOf is the configuration copy's root a name lies in, or "".
func configRootOf(name string) string {
	for _, root := range configRoots {
		if name == root || strings.HasPrefix(name, root+"/") {
			return root
		}
	}
	return ""
}

// credentialWords mark a credential, key or auth file in any component of a name.
var credentialWords = []string{"credential", "secret", "token", "passwd", "password", ".pem", ".key", "id_rsa", "id_ed25519", "id_ecdsa",
	"id_dsa", "keyring", "keychain", "keystore", "oauth", "apikey", "api_key", "api-key", "vault", "pairing", ".netrc", ".npmrc",
	".pypirc", "known_hosts", "authorized_keys", ".gnupg", ".gpg", ".p12", ".pfx", ".kdbx", "cookie", "bedrock"}

// credentialDirs are components that hold credentials whatever their files are named.
var credentialDirs = []string{".ssh", ".aws", ".claude.json", ".kube", ".docker", ".azure", ".password-store", ".fireworks", "gh", "gcloud"}

func credentialName(part string) bool {
	l := strings.ToLower(part)
	for _, w := range credentialWords {
		if strings.Contains(l, w) {
			return true
		}
	}
	if slices.Contains(credentialDirs, l) {
		return true
	}
	if l == "auth" || strings.HasPrefix(l, "auth.") || strings.HasPrefix(l, "auth-") || strings.HasPrefix(l, "auth_") ||
		strings.Contains(l, "-auth") || strings.Contains(l, "_auth") || strings.Contains(l, ".auth") {
		return true
	}
	return l == ".env" || strings.HasPrefix(l, ".env.") || strings.HasSuffix(l, ".env") || strings.Contains(l, ".env.")
}

// databaseWords mark a database, its journal or its store in any component of a name.
var databaseWords = []string{".sqlite", ".db", ".mdb", ".ldb", ".lmdb", ".duckdb", ".realm", ".redb", ".rdb", "leveldb", "indexeddb"}

// databaseSuffixes end a database's journal or shared memory file.
var databaseSuffixes = []string{"-wal", "-shm", "-journal"}

// codeExtensions are the source of programs a harness or a shell runs: such a file is no configuration text.
var codeExtensions = []string{".sh", ".bash", ".zsh", ".fish", ".py", ".js", ".mjs", ".cjs", ".ts", ".mts", ".cts", ".jsx", ".tsx",
	".rb", ".pl", ".lua", ".go", ".rs", ".ps1", ".bat", ".cmd", ".exe", ".so", ".dylib", ".wasm", ".el", ".vim", ".php", ".java",
	".kt", ".swift", ".c", ".cc", ".cpp", ".h"}

// codeDirs hold programs a harness loads: nothing under them is a target.
var codeDirs = []string{"hooks", "mods", "plugins", "extensions", "scripts", "node_modules"}

// targetRefusal says why a clean home-relative name is never a configuration target, whatever allow says, or "".
func (ct ConfigTargets) targetRefusal(name string) string {
	if why := ct.own(name); why != "" {
		return why
	}
	if systemdUnit(name) {
		return "a systemd unit"
	}
	if harnessSettings(name) {
		return "a harness's settings, which carry hooks, commands, permissions or sandbox settings"
	}
	parts := strings.Split(name, "/")
	for i, part := range parts {
		l := strings.ToLower(part)
		switch {
		case credentialName(part):
			return "a credential or auth file"
		case l == "bin" || l == ".bin" || l == "sbin" || l == "libexec":
			return "a program in a bin directory"
		}
		for _, w := range databaseWords {
			if strings.Contains(l, w) {
				return "a database"
			}
		}
		for _, w := range databaseSuffixes {
			if strings.HasSuffix(l, w) {
				return "a database"
			}
		}
		if i < len(parts)-1 && slices.Contains(codeDirs, l) {
			return "a program a harness loads"
		}
	}
	if ext := strings.ToLower(filepath.Ext(name)); ext != "" && slices.Contains(codeExtensions, ext) {
		return "a program a harness loads"
	}
	root := configRootOf(name)
	if root == "" || root == ".local/bin" || deniedConfigPath(root, name) != "" || !configPathAllowed(root, name, false) {
		return "outside the configuration the loop reads"
	}
	return ""
}

// own says why a name is the loop's own configuration, rules or policy, or "": it is one of them, lies below one, or
// is the file one of them resolves to.
func (ct ConfigTargets) own(name string) string {
	lower := strings.ToLower(name)
	for _, o := range ct.ownNames() {
		if o = strings.ToLower(o); lower == o || strings.HasPrefix(lower, o+"/") {
			return "the loop's own configuration, rules or policy"
		}
	}
	return ""
}

// ownNames are the home-relative names of fixedOwn and Own, as written and as they resolve.
func (ct ConfigTargets) ownNames() []string {
	var names []string
	add := func(p string) {
		if p = filepath.ToSlash(filepath.Clean(p)); p != "" && p != "." {
			names = append(names, p)
		}
	}
	for _, o := range append(append([]string(nil), fixedOwn...), ct.Own...) {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(o, "~/"); ok {
			o = rest
		}
		abs := o
		if !filepath.IsAbs(o) {
			add(o)
			if ct.Home == "" {
				continue
			}
			abs = filepath.Join(ct.Home, o)
		} else if ct.Home != "" {
			if rel, err := filepath.Rel(ct.Home, filepath.Clean(o)); err == nil && filepath.IsLocal(rel) {
				add(rel)
			}
		}
		if ct.Home == "" {
			continue
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			continue
		}
		home, err := filepath.EvalSymlinks(ct.Home)
		if err != nil {
			continue
		}
		if rel, err := filepath.Rel(home, real); err == nil && filepath.IsLocal(rel) {
			add(rel)
		}
	}
	return names
}

// ownPaths are fixedOwn and Own as absolute paths, for the identity check of an opened target.
func (ct ConfigTargets) ownPaths() []string {
	var out []string
	for _, o := range append(append([]string(nil), fixedOwn...), ct.Own...) {
		if o = strings.TrimSpace(o); o == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(o, "~/"); ok {
			o = rest
		}
		if !filepath.IsAbs(o) {
			if ct.Home == "" {
				continue
			}
			o = filepath.Join(ct.Home, o)
		}
		out = append(out, o)
	}
	return out
}

// fileID is a file's identity on the machine.
type fileID struct{ dev, ino uint64 }

// allowed says whether allow names a file exactly.
func (ct ConfigTargets) allowed(name string) error {
	if len(ct.Allow) == 0 {
		return refused("loop.config_targets.allow names no file: no configuration target is admitted")
	}
	for _, entry := range ct.Allow {
		if n, ok := allowName(entry); ok && n == name {
			return nil
		}
	}
	return refused("%s is not a file loop.config_targets.allow names", name)
}

// admit is a target's clean name when it passes the refusals and allow names it.
func (ct ConfigTargets) admit(name string) (string, error) {
	n, err := configTargetName(name)
	if err != nil {
		return "", err
	}
	if why := ct.targetRefusal(n); why != "" {
		return "", refused("%s is no configuration target: %s", n, why)
	}
	return n, ct.allowed(n)
}

// systemdDirs hold systemd units, user and system, quadlets and generators among them: nothing under them is a target.
var systemdDirs = []string{".config/systemd", ".local/share/systemd", ".config/containers/systemd", ".local/share/containers/systemd"}

// unitExtensions are the systemd unit and quadlet file types: such a file is no target wherever it lies.
var unitExtensions = []string{".service", ".timer", ".socket", ".path", ".mount", ".automount", ".swap", ".target", ".slice", ".scope",
	".device", ".container", ".volume", ".network", ".kube", ".pod", ".image", ".build", ".link", ".netdev", ".nspawn"}

// systemdUnit says whether a clean home-relative name is a systemd unit, a unit's drop-in, or lies in a unit directory.
func systemdUnit(name string) bool {
	lower := strings.ToLower(name)
	for _, dir := range systemdDirs {
		if lower == dir || strings.HasPrefix(lower, dir+"/") {
			return true
		}
	}
	parts := strings.Split(lower, "/")
	for i, part := range parts {
		if part == "systemd" || part == "systemd-generators" || part == "user-generators" {
			return true
		}
		if i < len(parts)-1 && strings.HasSuffix(part, ".d") && slices.Contains(unitExtensions, filepath.Ext(strings.TrimSuffix(part, ".d"))) {
			return true // a unit's drop-in directory (NAME.service.d)
		}
	}
	return slices.Contains(unitExtensions, filepath.Ext(lower))
}

// harnessSettings says whether a clean home-relative name is a harness file that loads commands or sets what a
// harness may do: hooks, commands it runs (MCP servers, notifiers, scheduled jobs, plugins and packages), permissions,
// execution policy, sandbox or approval settings. Compared without case.
func harnessSettings(name string) bool {
	l := strings.ToLower(name)
	dir, base := filepath.Dir(l), filepath.Base(l)
	switch {
	case dir == ".claude" && strings.HasPrefix(base, "settings") && strings.HasSuffix(base, ".json"):
		return true // settings.json, settings.local.json: hooks, permissions, sandbox, status line
	case dir == ".codex" && (base == "config.toml" || strings.HasSuffix(base, ".config.toml") || base == "hooks.json"):
		return true // configuration and profiles: MCP servers, notify, sandbox and approval; hooks
	case l == ".codex/rules" || strings.HasPrefix(l, ".codex/rules/") || l == ".codex/agents" || strings.HasPrefix(l, ".codex/agents/"):
		return true // execution policy; agents with their own sandbox and approval settings
	case l == ".grok" || strings.HasPrefix(l, ".grok/"):
		return true // configuration, sandbox, hooks and hook paths, and what they load
	case strings.HasPrefix(l, ".config/opencode/") && strings.HasSuffix(base, ".json"):
		return true // opencode.json, cli.json, service.json, package.json: MCP commands, permissions, packages
	case l == ".hermes/cron" || strings.HasPrefix(l, ".hermes/cron/") || l == ".hermes/config.yaml" || l == ".hermes/profile.yaml":
		return true // scheduled jobs; terminal, tools and MCP servers
	case l == ".pi/agent/settings.json":
		return true // packages and extensions it loads
	}
	return false
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// targetUID is the owner a target's file must have: the caller (a test names another).
var targetUID = os.Getuid

// targetText says why content is no configuration text: over the copy's file limit, invalid UTF-8, a NUL, or a
// program (a #! line).
func targetText(b []byte) string {
	switch {
	case len(b) > MaxConfigFileBytes:
		return "over the file limit"
	case bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b):
		return "not text"
	case bytes.HasPrefix(b, []byte("#!")):
		return "a program (#!)"
	}
	return ""
}

// targetFile is a target as the host read it.
type targetFile struct {
	SHA256  string
	Content []byte
}

// homeError is a refusal of the home the targets resolve under (none, or a link in its path): it says nothing of a
// target, so a read of a proposal's file that meets it decides nothing.
type homeError struct{ err error }

func (e *homeError) Error() string { return e.err.Error() }
func (e *homeError) Unwrap() error { return e.err }

// absentError is a refusal of a target that is not there, or was replaced while it was read: an owner's editor may be
// writing it, so a read that meets it decides nothing for the pass, and the next pass reads it again.
type absentError struct{ err error }

func (e *absentError) Error() string { return e.err.Error() }
func (e *absentError) Unwrap() error { return e.err }

// targetDir is a target's directory, opened read-only from the home one component at a time without following a link
// and without leaving the home's file system. Nothing is written through it.
type targetDir struct {
	fd         int
	dev        uint64
	name, base string
	dirs       []fileID // the directories of its path, the home's own excluded
	own        []string // the loop's own files and directories, absolute
}

func (d *targetDir) Close() { unix.Close(d.fd) }

// open resolves a target under the real home: one that passes the refusals and that allow names.
func (ct ConfigTargets) open(name string) (*targetDir, error) {
	n, err := ct.admit(name)
	if err != nil {
		return nil, err
	}
	if ct.Home == "" || !filepath.IsAbs(ct.Home) {
		return nil, &homeError{refused("configuration targets need the real home")}
	}
	if err := noInputLinks(ct.Home); err != nil {
		return nil, &homeError{refused("the home path holds a link")}
	}
	fd, err := unix.Open(ct.Home, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, err
	}
	dev := st.Dev
	var dirs []fileID
	parts := strings.Split(n, "/")
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		switch {
		case err == unix.ELOOP || err == unix.ENOTDIR:
			return nil, refused("%s: a directory of its path is a link or no directory", n)
		case err == unix.ENOENT:
			return nil, &absentError{refused("%s is not there: a configuration change edits a file that exists", n)}
		case err != nil:
			return nil, err
		}
		fd = next
		if err := unix.Fstat(fd, &st); err != nil {
			unix.Close(fd)
			return nil, err
		}
		if st.Dev != dev {
			unix.Close(fd)
			return nil, refused("%s: another file system is mounted on its path, so it resolves outside the home", n)
		}
		dirs = append(dirs, fileID{uint64(st.Dev), st.Ino})
	}
	return &targetDir{fd: fd, dev: dev, name: n, base: parts[len(parts)-1], dirs: dirs, own: ct.ownPaths()}, nil
}

// state reads the target once: an existing regular non-executable text file of the caller's, with one link, on the
// home's file system, within the limits, and not the loop's own by its identity.
func (d *targetDir) state() (targetFile, error) {
	// the name's type first, so a device or a FIFO is never opened
	var pre unix.Stat_t
	if err := unix.Fstatat(d.fd, d.base, &pre, unix.AT_SYMLINK_NOFOLLOW); err == unix.ENOENT {
		return targetFile{}, &absentError{refused("%s is not there: a configuration change edits a file that exists", d.name)}
	} else if err != nil {
		return targetFile{}, err
	}
	switch {
	case pre.Mode&unix.S_IFMT == unix.S_IFLNK:
		return targetFile{}, refused("%s is a link", d.name)
	case pre.Mode&unix.S_IFMT != unix.S_IFREG:
		return targetFile{}, refused("%s is no regular file", d.name)
	}
	// every check below is of the descriptor the content is read from
	fd, err := unix.Openat(d.fd, d.base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	case err == unix.ENOENT:
		return targetFile{}, &absentError{refused("%s is not there: a configuration change edits a file that exists", d.name)}
	case err == unix.ELOOP:
		return targetFile{}, refused("%s is a link", d.name)
	case err != nil:
		return targetFile{}, err
	}
	f := os.NewFile(uintptr(fd), d.base)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return targetFile{}, err
	}
	switch {
	case st.Mode&unix.S_IFMT != unix.S_IFREG || st.Dev != pre.Dev || st.Ino != pre.Ino:
		return targetFile{}, &absentError{refused("%s changed while it was read", d.name)}
	case st.Dev != d.dev:
		return targetFile{}, refused("%s is on another file system, so it resolves outside the home", d.name)
	case st.Mode&0o111 != 0:
		return targetFile{}, refused("%s is executable: a configuration target is not", d.name)

	case st.Mode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) != 0:
		return targetFile{}, refused("%s has a set-id or sticky bit", d.name)
	case int(st.Uid) != targetUID():
		return targetFile{}, refused("%s is not owned by the caller", d.name)
	case st.Nlink > 1:
		return targetFile{}, refused("%s has another hard link: a change to it would not be the file's alone", d.name)
	case st.Size > MaxConfigFileBytes:
		return targetFile{}, refused("%s exceeds the configuration file limit", d.name)
	}

	b, err := io.ReadAll(io.LimitReader(f, MaxConfigFileBytes+1))
	if err != nil {
		return targetFile{}, err
	}
	if reason := targetText(b); reason != "" {
		return targetFile{}, refused("%s is no configuration text: %s", d.name, reason)
	}
	// a file mounted on its name (a bind mount on the same device) is not the directory's
	var fst, dst unix.Statx_t
	if unix.Statx(d.fd, d.base, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MNT_ID, &fst) == nil &&
		unix.Statx(d.fd, "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &dst) == nil &&
		fst.Mask&dst.Mask&unix.STATX_MNT_ID != 0 && fst.Mnt_id != dst.Mnt_id {
		return targetFile{}, refused("%s is a mount point", d.name)
	}
	// by identity too: a name that reaches the loop's own file or directory another way (a bind mount, a
	// case-folding directory) is that file
	self := fileID{uint64(st.Dev), st.Ino}
	for _, o := range d.own {
		var ost unix.Stat_t
		if unix.Stat(o, &ost) != nil {
			continue
		}
		id := fileID{uint64(ost.Dev), ost.Ino}
		if id == self || slices.Contains(d.dirs, id) {
			return targetFile{}, refused("%s is no configuration target: the loop's own configuration, rules or policy", d.name)
		}
	}
	return targetFile{SHA256: hashOf(b), Content: b}, nil
}

// configTarget opens a target a step builds on: refused while configuration targets are off, when allow does not
// name it, or by any refusal. A refusal is a hold: it says nothing of the agent.
func (s *Ship) configTarget(target string) (*targetDir, error) {
	if target == "" {
		return nil, refused("the configuration package names no target")
	}
	if !s.ConfigTargets.Enabled {
		return nil, &shipHold{err: refused("configuration targets are off (loop.config_targets.enabled)")}
	}
	d, err := s.ConfigTargets.open(target)
	if errors.Is(err, ErrRefused) {
		return nil, &shipHold{err: err}
	}
	return d, err
}

// readTarget reads an unfiled proposal's file as it is now. why says why the file cannot be read as a target now (a
// link, refused); err is an error of the machine, of the home itself, or of a file that is not there or was replaced
// while it was read (absentError), which decide nothing.
func (s *Ship) readTarget(name string) (targetFile, string, error) {
	d, err := s.ConfigTargets.open(name)
	var home *homeError
	var absent *absentError
	if errors.As(err, &home) || errors.As(err, &absent) {
		return targetFile{}, "", err
	}
	if errors.Is(err, ErrRefused) {
		return targetFile{}, strings.TrimPrefix(err.Error(), ErrRefused.Error()+": "), nil
	}
	if err != nil {
		return targetFile{}, "", err
	}
	defer d.Close()
	f, err := d.state()
	if errors.As(err, &absent) {
		return targetFile{}, "", err
	}
	if errors.Is(err, ErrRefused) {
		return targetFile{}, strings.TrimPrefix(err.Error(), ErrRefused.Error()+": "), nil
	}
	return f, "", err
}

// PrepareConfig makes a configuration package: a host-only Git repository whose base commit holds the target as the
// host reads it now, and a build tree of it. The target must read through the scrubber unchanged, so the run's
// /ro/config copy and its build tree hold the bytes the patch applies to. copied, when set, is the hash of the run's
// /ro/config copy of the target, which the file must still hold.
func (s *Ship) PrepareConfig(ctx context.Context, caseID, target, copied string) (Package, error) {
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
	if had && old.State == Withdrawn && old.PR != "" {
		return old, refused("case %s's hand landing withdrawal is not finished yet: the next selection finishes it", c.ID)
	}
	d, err := s.configTarget(target)
	if err != nil {
		return Package{}, err
	}
	defer d.Close()
	if had && old.State != RolledBack && old.State != Kept && old.State != Withdrawn && !stalePackage(c, old, KindConfig, d.name) {
		return old, refused("case %s has a package already (in %s)", c.ID, old.State)
	}
	base, err := d.state()
	if err != nil {
		return Package{}, &shipHold{err: err}
	}
	if err := s.scrubbedTarget(ctx, d.name, base); err != nil {
		return Package{}, err // the run's copy is the scrubbed text: its hash says nothing of the file
	}
	if copied != "" && copied != base.SHA256 {
		return Package{}, &shipHold{err: refused("%s changed since the run's configuration copy was made: the run did not read the file it would change", d.name)}
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
	p := Package{Case: c.ID, Kind: KindConfig, Target: d.name, TargetBase: base.SHA256, Branch: branch, Work: work, Host: host,
		State: Prepared, Created: s.now(), Episode: c.Episode}
	if had && old.State == Withdrawn && old.Kind == KindConfig && old.Episode == c.Episode {
		// within an episode the withdrawals after the first count toward the case's two closed proposals
		p.ClosedProposals, p.Withdrawals = old.ClosedProposals, old.Withdrawals
	}
	if p.BaseCommit, err = s.configBase(ctx, p, base.Content); err != nil {
		return Package{}, err
	}
	if err := s.freshRunClone(ctx, &p); err != nil {
		return Package{}, err
	}
	// the old package is archived only once its replacement is ready, as Prepare does
	if had {
		if err := s.archive(old); err != nil {
			return Package{}, err
		}
	}
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return Package{}, err
	}
	return p, s.Store.CaseNote(cleanedNote(ctx), c.ID, "prepared", fmt.Sprintf("configuration %s at %.12s", p.Target, p.BaseCommit), s.Actor)
}

// scrubbedTarget holds a step on a file whose text the scrubber would mask: the run's copy would not be the file.
func (s *Ship) scrubbedTarget(ctx context.Context, target string, cur targetFile) error {
	clean, err := RedactAll(ctx, []string{target, string(cur.Content)})
	if err != nil {
		return err
	}
	if clean[0] != target || clean[1] != string(cur.Content) {
		return &shipHold{err: refused("%s holds text the scrubber would mask: a run's copy of it is not the file, so the loop cannot change it", target)}
	}
	return nil
}

// refreshConfigBase brings a package not yet proposed to its target as it is now: a new base commit of the file's
// content, no check and no head. The caller holds the case's lock.
func (s *Ship) refreshConfigBase(ctx context.Context, p Package, cur targetFile) (Package, error) {
	if err := s.scrubbedTarget(ctx, p.Target, cur); err != nil {
		return p, err
	}
	base, err := s.configBase(s.gitContext(shipContext(ctx, p.Case)), p, cur.Content)
	if err != nil {
		return p, err
	}
	p.BaseCommit, p.TargetBase, p.Check, p.Head, p.State = base, cur.SHA256, nil, "", Prepared
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, err
	}
	return p, s.Store.CaseNote(cleanedNote(ctx), p.Case, "prepared", fmt.Sprintf("configuration %s at %.12s, the file as it is now", p.Target, p.BaseCommit), s.Actor)
}

// configBase commits the target's content, as a regular non-executable file, as the host repository's base commit
// and points the package branch at it.
func (s *Ship) configBase(ctx context.Context, p Package, content []byte) (string, error) {
	if _, err := s.git(ctx, p.Host, "read-tree", "--empty"); err != nil {
		return "", err
	}
	blob, err := s.gitInput(ctx, p.Host, content, "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	if _, err := s.git(ctx, p.Host, "update-index", "--add", "--cacheinfo", "100644,"+strings.TrimSpace(blob)+","+p.Target); err != nil {
		return "", err
	}
	tree, err := s.git(ctx, p.Host, "write-tree")
	if err != nil {
		return "", err
	}
	commit, err := s.git(ctx, p.Host, "commit-tree", strings.TrimSpace(tree), "-m", "Configuration base for case "+p.Case)
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

// checkConfigChange refuses a configuration change that does more than edit its target's content and add its tests
// under tests/: no other file, no deletion, rename or mode change, and the edited target stays text.
func (s *Ship) checkConfigChange(ctx context.Context, p Package, tree string, paths, testFiles []string) error {
	if !slices.Contains(paths, p.Target) {
		return refused("the change does not change its target %s", p.Target)
	}
	for _, f := range testFiles {
		if !strings.HasPrefix(f, "tests/") || f == p.Target {
			return refused("%s: a configuration change's tests lie under tests/", f)
		}
	}
	for _, f := range paths {
		if f != p.Target && !slices.Contains(testFiles, f) {
			return refused("%s is neither the target nor a test file: a configuration change changes one file", f)
		}
	}
	_, err := s.configContent(ctx, p, tree)
	return err
}

// configContent is the target's content in a tree of the host repository: a regular non-executable file of text.
func (s *Ship) configContent(ctx context.Context, p Package, treeish string) ([]byte, error) {
	entry, err := s.git(ctx, p.Host, "ls-tree", treeish, "--", p.Target)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(entry)
	switch {
	case len(fields) < 3:
		return nil, refused("the change deletes or renames its target: a configuration change edits its content")
	case fields[1] != "blob" || fields[0] != "100644":
		return nil, refused("the change changes its target's mode or type: a configuration change keeps a regular non-executable file")
	}
	raw, err := s.gitInput(ctx, p.Host, nil, "cat-file", "blob", fields[2])
	if err != nil {
		return nil, err
	}
	if reason := targetText([]byte(raw)); reason != "" {
		return nil, refused("the changed target is no configuration text: %s", reason)
	}
	return []byte(raw), nil
}

// ConfigProposal is the record of a configuration proposal, kept under the package's directory beside its patch,
// which no later package of the case moves: the file, the hash of the content it was built on and of the content it
// leaves, the patch, a line that applies it on its base (applyLine: information, an agent applies it with its own
// tools), the fix reference its case closes with, the check and the measurement plan.
type ConfigProposal struct {
	Case   string    `json:"case"`
	Target string    `json:"target"` // home-relative
	File   string    `json:"file"`   // absolute
	Base   string    `json:"base_sha256"`
	New    string    `json:"new_sha256"`
	Patch  string    `json:"patch"`
	Apply  string    `json:"apply"`
	Fix    string    `json:"fix"`
	Head   string    `json:"head"` // the host commit of the change, which names the proposal
	Check  *Check    `json:"check"`
	Plan   *Plan     `json:"plan"`
	At     time.Time `json:"at"`
}

// proposalFile is where a configuration proposal's record (.json) or patch (.patch) is.
func (s *Ship) proposalFile(p Package, ext string) string {
	return filepath.Join(s.pkgDir(p.Case), "proposals", fmt.Sprintf("%.12s%s", p.Head, ext))
}

// targetPath is the absolute path of a configuration package's file.
func (s *Ship) targetPath(p Package) string { return filepath.Join(s.ConfigTargets.Home, p.Target) }

// applyLine is a shell line that applies a proposal's patch to its file only on the content the proposal was built on:
// sha256sum checks the file's hash first (its HASH *FILE form, which a note's one plain line keeps whole), then GNU
// patch applies the patch with no fuzz, forward only, and no backup or reject file. It is information for the agent,
// which applies the change with its own tools. momentum runs none.
func (s *Ship) applyLine(p Package) string {
	return "echo " + ShellQuote(p.TargetBase+" *"+s.targetPath(p)) + " | sha256sum -c --status - && " +
		shellLine([]string{"patch", "-p1", "-F0", "--forward", "--no-backup-if-mismatch", "--reject-file=-", "-d", s.ConfigTargets.Home, "-i", s.proposalFile(p, ".patch")})
}

// configPlan is a configuration proposal's measurement plan, kept as information: the loop never measures the change,
// so the plan is neither counted nor waited on. Its signal and window keep their form's checks.
func configPlan(sig Signal, window time.Duration, at time.Time) (Plan, error) {
	switch {
	case sig.Source == "" || len(sig.Kinds) == 0 || len(sig.Match) == 0:
		return Plan{}, refused("a measurement signal names a source, kinds and at least one text its events hold")
	case !measure.WholeWeeks(window):
		return Plan{}, refused("a measurement window is a whole number of weeks (7d, 14d and so on)")
	}
	for _, m := range sig.Match {
		if len(strings.TrimSpace(m)) < 4 {
			return Plan{}, refused("the signal's text %q is too short to name a problem", m)
		}
	}
	return Plan{Signal: sig, Window: window, At: at}, nil
}

// configFix is the fix reference a configuration proposal's case closes with once an agent applied it: the file and
// the hash its change leaves (store.CloseSolvedBy).
func configFix(p Package) string { return "config:~/" + p.Target + "@" + p.TargetNew }

// shellLine is a POSIX shell line of words: a word with any character outside [A-Za-z0-9_./:@%+,=-], and the empty
// word, is quoted.
func shellLine(args []string) string {
	unsafe := func(r rune) bool {
		return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || strings.ContainsRune("_./:@%+,=-", r))
	}
	words := make([]string, len(args))
	for i, a := range args {
		words[i] = a
		if a == "" || strings.IndexFunc(a, unsafe) >= 0 {
			words[i] = ShellQuote(a)
		}
	}
	return strings.Join(words, " ")
}

// proposeConfig records a checked and committed configuration change as a proposal: its patch and record under the
// package's directory and the proposed line in the case log. The loop's clean-context review comes next; only a clean
// one files it for an agent (fileConfig). Nothing is written to the file. The caller holds the case's lock.
func (s *Ship) proposeConfig(ctx context.Context, c store.Case, p *Package, title string, plan Plan) error {
	if err := s.packageAuthority(ctx, *p); err != nil {
		return err
	}
	content, err := s.configContent(ctx, *p, p.Head)
	if err != nil {
		return err
	}
	diff, err := s.git(ctx, p.Host, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--src-prefix=a/", "--dst-prefix=b/",
		p.BaseCommit, p.Head, "--", p.Target)
	if err != nil {
		return err
	}
	p.TargetNew, p.Episode, p.Filed, p.Raised, p.GoneSince = hashOf(content), c.Episode, time.Time{}, time.Time{}, time.Time{}
	patch := s.proposalFile(*p, ".patch")
	if err := os.MkdirAll(filepath.Dir(patch), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(patch, []byte(diff), 0o600); err != nil {
		return err
	}
	rec := ConfigProposal{Case: p.Case, Target: p.Target, File: s.targetPath(*p), Base: p.TargetBase, New: p.TargetNew, Patch: patch,
		Apply: s.applyLine(*p), Fix: configFix(*p), Head: p.Head, Check: p.Check, Plan: &plan, At: s.now()}
	if err := writeJSON(s.proposalFile(*p, ".json"), rec); err != nil {
		return err
	}
	p.State = Proposed
	if err := s.saveAuthorized(ctx, p); err != nil {
		return err
	}
	note, err := Redact(ctx, fmt.Sprintf("configuration %s %.12s %s; checked and reviewed, never measured by the loop", p.Target, p.Head, title))
	if err != nil {
		return err
	}
	return s.Store.CaseNote(cleanedNote(ctx), c.ID, "proposed", note, s.Actor)
}

func configNoteID(caseID string) string { return "loop-config:" + store.IDPart(caseID) }

// configRaise is how long a filed proposal waits before its note is raised, once, from low to normal importance: a
// week, the loop's shortest measurement window and the wait after which its other waiting notes speak up.
const configRaise = 7 * 24 * time.Hour

// fileConfig files a configuration proposal whose last review is clean and posted for an agent to apply by hand: its
// filing time, then its note and the case log's proposed (configuration) line (configNote). filed says this call filed
// it.
func (s *Ship) fileConfig(ctx context.Context, caseID string) (p Package, filed bool, err error) {
	ctx = shipContext(ctx, caseID)
	unlock, err := s.lock(ctx, caseID)
	if err != nil {
		return Package{}, false, err
	}
	defer unlock()
	p, ok, err := s.Load(caseID)
	if err != nil || !ok || p.Kind != KindConfig || p.State != Proposed || !p.Clean() {
		return p, false, err
	}
	c, err := s.Store.CaseByID(ctx, caseID)
	if err != nil {
		return p, false, err
	}
	if c.Into != "" || c.Status != store.CaseTesting || c.Episode != p.Episode {
		return p, false, nil // its case left testing: the next selection withdraws it
	}
	if p.Filed.IsZero() {
		p, filed, err = s.file(ctx, c, p)
	}
	return p, filed, errors.Join(err, s.configNote(ctx, c, &p))
}

// file files a proposal on its file as it is at the filing, not as the pass read it: a move since withdraws it. The
// caller holds the case's lock.
func (s *Ship) file(ctx context.Context, c store.Case, p Package) (Package, bool, error) {
	if err := s.packageAuthority(ctx, p); err != nil {
		return p, false, err
	}
	f, why, err := s.readTarget(p.Target)
	var absent *absentError
	switch {
	case errors.As(err, &absent):
		return p, false, &shipHold{err: fmt.Errorf("%w: the proposal is filed once it is back", err)}
	case err != nil:
		return p, false, err
	case why != "" || f.SHA256 != p.TargetBase:
		if why == "" {
			why = p.Target + " holds something other than the content the proposal was built on"
		}
		p, err = s.withdrawConfig(ctx, c, p, why, true)
		return p, false, err
	}
	p.Filed = s.now()
	return p, true, s.saveAuthorized(ctx, &p)
}

// configWaiting is what a configuration package waits on a person for: the note that says why and what to do, when the
// wait began, and whether the case shows as proposed (configuration).
type configWaiting struct {
	title, detail string
	since         time.Time
	stage         bool
}

// configWait is what a configuration package waits on a person for, if anything: a filed proposal, an unfiled one whose
// review rounds are used, or one whose file has been gone a day. It waits on nobody once it is withdrawn, or its case
// left testing, was merged or began a new episode.
func (s *Ship) configWait(c store.Case, p Package) (configWaiting, bool) {
	if p.Kind != KindConfig || caseEnded(c, p) != "" {
		return configWaiting{}, false
	}
	closeLine := func(reason string) string {
		return "momentum case close " + c.ID + " --reason " + reason + " --why ..."
	}
	switch {
	case configFiled(c, p):
		return configWaiting{
			title: "The loop proposes a configuration change for case " + c.ID + ": " + c.Title,
			detail: fmt.Sprintf("Apply: %s. Then: momentum case close %s --reason already-solved --fix %s --why .... Or decline it: %s. "+
				"The change to %s was checked in the sandbox and reviewed clean (the review: %s). The line applies the patch %s only to the content the proposal "+
				"was built on; review it first. momentum never writes the file or measures the change. If the problem comes back, new evidence reopens the case "+
				"and the loop builds again. The rest of the proposal: %s.",
				s.applyLine(p), c.ID, ShellQuote(configFix(p)), closeLine("not-worth-it"), s.targetPath(p), s.proposalFile(p, ".review.md"),
				s.proposalFile(p, ".patch"), s.proposalFile(p, ".json")),
			since: p.Filed, stage: true}, true
	case p.State == Proposed && p.Filed.IsZero() && roundsUsed(p):
		return configWaiting{
			title: fmt.Sprintf("The loop's change for case %s used its %d review rounds and waits for a person", c.ID, MaxRounds),
			detail: fmt.Sprintf("The current change of %s used its %d review rounds: the loop builds and reviews it no more, and never writes the file. "+
				"A person may close the case (not-worth-it), or apply your own fix and close it as already-solved: %s, or %s.",
				s.targetPath(p), MaxRounds, closeLine("not-worth-it"), closeLine("already-solved")),
			since: p.Reviews[len(p.Reviews)-1].At, stage: true}, true
	case p.State == Proposed && p.Filed.IsZero() && !p.GoneSince.IsZero() && s.now().Sub(p.GoneSince) >= configGone:
		return configWaiting{
			title: "The file of a configuration proposal is gone for case " + c.ID + ": " + c.Title,
			detail: fmt.Sprintf("%s has not been there since %s, so the proposal built on it waits. Put the file back, or close the case: %s.",
				s.targetPath(p), p.GoneSince.UTC().Format(time.RFC3339), closeLine("not-worth-it")),
			since: p.GoneSince}, true
	}
	return configWaiting{}, false
}

// configNote keeps the one invariant of a configuration package's notes. While it waits on a person, in any of its
// waiting states (configWait), exactly one standing note says why and what to do: it is put back when it was resolved,
// and raised once from low to normal importance after configRaise. The case log's proposed (configuration) line shows
// a filed proposal's stage, and an unfiled one's whose rounds are used. Once it waits on nobody, its note is resolved.
// A pass calls it for every configuration package; the caller holds the case's lock.
func (s *Ship) configNote(ctx context.Context, c store.Case, p *Package) error {
	id := configNoteID(c.ID)
	w, waits := s.configWait(c, *p)
	if !waits {
		if _, _, err := s.Store.DoneNote(ctx, id, s.now()); err != nil && !errors.Is(err, store.ErrNoNote) {
			return err
		}
		return nil
	}
	if w.stage {
		log, err := s.Store.CaseLog(ctx, c.ID)
		if err != nil {
			return err
		}
		if store.ConfigOf(c, log) != p.Target {
			if err := s.Store.CaseNote(cleanedNote(ctx), c.ID, store.ActionConfigProposed, store.ConfigDetail(p.Target, c.Episode), s.Actor); err != nil {
				return err
			}
		}
	}
	note := store.Note{ID: id, Kind: store.NoteProblem, Importance: store.ImportanceLow, Source: "momentum", At: s.now(),
		Title: plainClip(w.title, store.TitleMax), Detail: w.detail}
	raise := s.now().Sub(w.since) >= configRaise && p.Raised.Before(w.since)
	if raise {
		note.Importance = store.ImportanceNormal
		note.Title = plainClip("A configuration proposal has waited a week for a person, case "+c.ID+": "+c.Title, store.TitleMax)
	} else if n, ok, err := s.Store.Note(ctx, id); err != nil {
		return err
	} else if ok && n.Done.IsZero() && n.Detail == plain.Line(w.detail) {
		return nil // it stands as it is
	}
	if _, err := s.Store.PutNote(ctx, note); err != nil {
		return err
	}
	if raise {
		p.Raised = s.now()
		return s.save(p)
	}
	return nil
}

// reconcileConfig settles a configuration package at a pass, and then its note (configNote). A package not yet
// finished (prepared, checked, revising or proposed) whose case left testing, merged or began a new episode is
// withdrawn, and a withdrawn one's counts start again with its case's new episode. A proposal whose file the loop may
// no longer change is withdrawn. A filed one
// then waits, as proposed (configuration), for a person to close the case, and so does an unfiled one whose review
// rounds are used: nothing reads its file. An unfiled one, still before its review, is withdrawn when its file holds
// anything but the content it was built on; a file that is not there, or was replaced while it was read, decides
// nothing for the pass. Nothing is written to the file.
func (s *Ship) reconcileConfig(ctx context.Context, caseID string) (Package, error) {
	ctx = s.gitContext(shipContext(ctx, caseID))
	unlock, err := s.lock(ctx, caseID)
	if err != nil {
		return Package{}, err
	}
	defer unlock()
	p, ok, err := s.Load(caseID)
	if err != nil || !ok || p.Kind != KindConfig {
		return p, err
	}
	c, err := s.Store.CaseByID(ctx, caseID)
	if err != nil {
		return p, err
	}
	switch ended := caseEnded(c, p); {
	case ended != "" && unfinished(p):
		p, err = s.withdrawConfig(ctx, c, p, ended, false)
	case p.State == Proposed:
		p, err = s.settleProposal(ctx, c, p)
	}
	if err == nil && p.State == Withdrawn && c.Into == "" && c.Status == store.CaseTesting && c.Episode != p.Episode {
		// a new episode is a new instance of the problem: the case's closed proposals and withdrawals start again
		p.Episode, p.ClosedProposals, p.Withdrawals = c.Episode, 0, 0
		err = s.save(&p)
	}
	return p, errors.Join(err, s.configNote(ctx, c, &p))
}

// caseEnded says why a configuration package's case holds it no more, or "": the case was merged, left testing or
// began a new episode.
func caseEnded(c store.Case, p Package) string {
	switch {
	case c.Into != "":
		return "its case was merged into " + c.Into
	case c.Status != store.CaseTesting:
		return fmt.Sprintf("its case is %s now", c.Status)
	case c.Episode != p.Episode:
		return "its case began a new episode"
	}
	return ""
}

// settleProposal is reconcileConfig for a proposal of the case's episode in testing. The caller holds the case's lock.
func (s *Ship) settleProposal(ctx context.Context, c store.Case, p Package) (Package, error) {
	switch {
	case !s.ConfigTargets.Enabled:
		return s.withdrawConfig(ctx, c, p, "configuration targets are off (loop.config_targets.enabled): the loop proposes no change to "+p.Target, false)
	}
	if _, err := s.ConfigTargets.admit(p.Target); err != nil {
		return s.withdrawConfig(ctx, c, p, strings.TrimPrefix(err.Error(), ErrRefused.Error()+": "), false)
	}
	if !p.Filed.IsZero() || roundsUsed(p) {
		return p, nil // it waits for a person: its file is not read
	}
	f, why, err := s.readTarget(p.Target)
	var absent *absentError
	switch {
	case errors.As(err, &absent):
		return p, s.holdGone(&p, err)
	case err != nil:
		return p, err
	case why == "" && f.SHA256 == p.TargetBase:
		return p, s.configBack(&p)
	case why == "":
		why = p.Target + " holds something other than the content the proposal was built on"
	}
	if err := s.configBack(&p); err != nil {
		return p, err
	}
	return s.withdrawConfig(ctx, c, p, why, true)
}

// configGone is how long an unfiled proposal's file, or its directory, may be gone before the proposal waits on a
// person (configWait): a day. An editor's replace, or a reorganization of the directory, takes minutes; a file gone a
// day was moved or removed.
const configGone = 24 * time.Hour

// holdGone holds an unfiled proposal whose file is not there for the pass, from the first pass that found it gone.
// The caller holds the case's lock.
func (s *Ship) holdGone(p *Package, gone error) error {
	hold := &shipHold{err: fmt.Errorf("%w: it is read again next pass", gone)}
	if p.GoneSince.IsZero() {
		p.GoneSince = s.now()
		return errors.Join(hold, s.save(p))
	}
	return hold
}

// configBack ends a gone file's wait once the file reads again.
func (s *Ship) configBack(p *Package) error {
	if p.GoneSince.IsZero() {
		return nil
	}
	p.GoneSince = time.Time{}
	return s.save(p)
}

// configFiled says a configuration package is a filed proposal that waits for its case's close (configWait).
func configFiled(c store.Case, p Package) bool {
	return p.Kind == KindConfig && caseEnded(c, p) == "" && p.State == Proposed && !p.Filed.IsZero()
}

// FiledConfigFix is the fix reference a case's filed configuration proposal names, for the close that ends it: a
// close as already solved by a configuration fix names exactly this one. ok is false when the case has none waiting
// (configFiled).
func FiledConfigFix(dir string, c store.Case) (fix string, ok bool, err error) {
	p, had, err := (&Ship{Dir: dir}).Load(c.ID)
	if err != nil || !had || !configFiled(c, p) {
		return "", false, err
	}
	return configFix(p), true, nil
}

// withdrawConfig withdraws a configuration proposal: its state and a case log line (its caller's configNote resolves
// its note). A merged case's withdrawal writes no line: the store writes a merged case's lines to the case it went
// into, whose own proposal they would end. moved says its file moved under it before its review; each such withdrawal after the case's first counts as a closed proposal, so a
// file that keeps moving stops the case after two more (proposalsUsed). The caller holds the case's lock.
func (s *Ship) withdrawConfig(ctx context.Context, c store.Case, p Package, why string, moved bool) (Package, error) {
	what := fmt.Sprintf("proposal %.12s", p.Head)
	if p.State != Proposed {
		what = fmt.Sprintf("package %.12s (%s)", p.BaseCommit, p.State)
	}
	p.State = Withdrawn
	if moved {
		p.Withdrawals++
		if p.Withdrawals > 1 {
			p.ClosedProposals++
		}
	}
	if err := s.save(&p); err != nil { // the loop's own record, even of a merged case
		return p, err
	}
	if c.Into != "" {
		return p, nil
	}
	detail := what + ": " + why
	if moved {
		detail += "; the case builds again on the file as it is"
	}
	return p, s.caseNoteOnce(ctx, p.Case, store.ActionConfigWithdrawn, detail)
}

// withdrawStale settles the unfinished configuration packages of cases that left testing or merged (a close ends a
// filed proposal's wait): they are withdrawn and their notes resolved. A testing case's package is settled by its
// selection (reconcileConfig); a pass narrowed to one case (only) settles that case's alone.
func (r *Runner) withdrawStale(ctx context.Context, cs []store.Case, only string) {
	if r.Ship == nil {
		return
	}
	for _, c := range cs {
		if c.Status == store.CaseTesting && c.Into == "" || only != "" && c.ID != only {
			continue
		}
		if p, ok, err := r.Ship.Load(c.ID); err != nil || !ok || p.Kind != KindConfig || !unfinished(p) {
			continue
		}
		if _, err := r.Ship.reconcileConfig(ctx, c.ID); err != nil {
			r.logf("withdraw the configuration proposal of %s: %v", c.ID, err)
		}
	}
}

// packageAuthority checks again what a package rests on: its repository's gate, or its configuration target (with
// configuration targets on, allow naming it and every refusal passed).
func (s *Ship) packageAuthority(ctx context.Context, p Package) error {
	if p.Kind == KindConfig {
		d, err := s.configTarget(p.Target)
		if err != nil {
			return err
		}
		d.Close()
		return nil
	}
	_, err := s.Gate.Repository(ctx, p.Dir)
	return err
}

// heldTarget is heldBuild for a repository target. A configuration target holds while configuration targets are off,
// allow does not name it or it is refused by name; the file itself is read when the build prepares.
func (r *Runner) heldTarget(ctx context.Context, repo, target string) (string, bool) {
	if target == "" {
		return r.heldBuild(ctx, repo)
	}
	if !r.Ship.ConfigTargets.Enabled {
		return "configuration targets are off (loop.config_targets.enabled): the loop proposes no change to " + target, true
	}
	if _, err := r.Ship.ConfigTargets.admit(target); err != nil {
		return strings.TrimPrefix(err.Error(), ErrRefused.Error()+": "), true
	}
	return "", true
}
