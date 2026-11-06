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

	"github.com/agent-sh/momentum/internal/store"
	"golang.org/x/sys/unix"
)

// Package kinds. A repository package lands through a pull request; a configuration package changes the content of
// one existing file under the home, in place, from the host.
const (
	KindRepo   = "repo"
	KindConfig = "config"
)

// fixedOwn are home-relative files and directories that are never a configuration target, whatever the
// configuration says: momentum's own configuration and data, and the policy every scheduled run reads.
var fixedOwn = []string{".config/momentum", ".local/share/momentum", ".config/agents/scheduled-ai-policy.md"}

// ConfigTargets is the loop's authority over configuration files (loop.config_targets): off unless Enabled, and then
// only the existing files Allow names exactly. An empty Allow admits nothing. The refusals of targetRefusal and of
// the file checks hold whatever Allow says.
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

func cleanTargetName(n string) bool {
	return n != "" && !strings.ContainsAny(n, "\\\x00\r\n*?[") && !filepath.IsAbs(n) && filepath.IsLocal(n) && filepath.Clean(n) == n
}

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
	n, err := ct.structural(name)
	if err != nil {
		return "", err
	}
	return n, ct.allowed(n)
}

// structural is a target's clean name when it passes the refusals, without allow: the owner's rollback and the
// in-effect check read a landed change's file so, after he narrowed allow.
func (ct ConfigTargets) structural(name string) (string, error) {
	n, err := configTargetName(name)
	if err != nil {
		return "", err
	}
	if why := ct.targetRefusal(n); why != "" {
		return "", refused("%s is no configuration target: %s", n, why)
	}
	return n, nil
}

// ConfigState is a target file as the host read it: its hash, mode, owner and group, and in a journal its content.
type ConfigState struct {
	SHA256  string `json:"sha256"`
	Mode    uint32 `json:"mode"`
	UID     uint32 `json:"uid"`
	GID     uint32 `json:"gid"`
	Content []byte `json:"content,omitempty"`
}

func (c ConfigState) bare() *ConfigState {
	return &ConfigState{SHA256: c.SHA256, Mode: c.Mode, UID: c.UID, GID: c.GID}
}

// sameFile is the same content in the same mode, owner and group.
func (c ConfigState) sameFile(o ConfigState) bool {
	return c.SHA256 == o.SHA256 && c.Mode == o.Mode && c.UID == o.UID && c.GID == o.GID
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// targetUID is the owner a target's file must have: the caller (a test names another).
var targetUID = os.Getuid

// targetGroups are the groups a target's file may have: the caller's, whose group a rewrite can keep.
var targetGroups = func() []int {
	groups, _ := os.Getgroups()
	return append(groups, os.Getgid())
}

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

// targetDir is a target's directory, opened from the home one component at a time without following a link and
// without leaving the home's file system.
type targetDir struct {
	fd         int
	dev        uint64
	name, base string
	dirs       []fileID // the directories of its path, the home's own excluded
	own        []string // the loop's own files and directories, absolute
	stage      func(string) error
}

// at is a test hook between a write's steps (Ship.ConfigStage): an error stops the step there, as a crash would.
func (d *targetDir) at(stage string) error {
	if d.stage != nil {
		return d.stage(stage)
	}
	return nil
}

func (d *targetDir) Close() { unix.Close(d.fd) }

// open resolves a target under the real home. Structural skips allow, never a refusal.
func (ct ConfigTargets) open(name string, structural bool) (*targetDir, error) {
	n, err := ct.structural(name)
	if err != nil {
		return nil, err
	}
	if !structural {
		if err := ct.allowed(n); err != nil {
			return nil, err
		}
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
			return nil, errors.Join(refused("%s is not there: a configuration change edits a file that exists", n), errTargetGone)
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
// home's file system, without extended attributes, within the limits.
func (d *targetDir) state() (ConfigState, error) {
	// the name's type first, so a device or a FIFO is never opened
	var pre unix.Stat_t
	if err := unix.Fstatat(d.fd, d.base, &pre, unix.AT_SYMLINK_NOFOLLOW); err == unix.ENOENT {
		return ConfigState{}, errors.Join(refused("%s is not there: a configuration change edits a file that exists", d.name), errTargetGone)
	} else if err != nil {
		return ConfigState{}, err
	}
	switch {
	case pre.Mode&unix.S_IFMT == unix.S_IFLNK:
		return ConfigState{}, refused("%s is a link", d.name)
	case pre.Mode&unix.S_IFMT != unix.S_IFREG:
		return ConfigState{}, refused("%s is no regular file", d.name)
	}
	// every check below is of the descriptor the content is read from
	got, st, ok, err := d.read(d.base)
	if err != nil {
		return ConfigState{}, err
	}
	switch {
	case !ok || st.Dev != pre.Dev || st.Ino != pre.Ino:
		return ConfigState{}, refused("%s changed while it was read", d.name)
	case st.Dev != d.dev:
		return ConfigState{}, refused("%s is on another file system, so it resolves outside the home", d.name)
	case st.Mode&0o111 != 0:
		return ConfigState{}, refused("%s is executable: a configuration target is not", d.name)
	case st.Mode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) != 0:
		return ConfigState{}, refused("%s has a set-id or sticky bit", d.name)
	case int(st.Uid) != targetUID():
		return ConfigState{}, refused("%s is not owned by the caller", d.name)
	case !slices.Contains(targetGroups(), int(st.Gid)):
		return ConfigState{}, refused("%s has a group the caller is not in: a rewrite could not keep it", d.name)
	case st.Nlink > 1:
		return ConfigState{}, refused("%s has another hard link: a change to it would not be the file's alone", d.name)
	case st.Size > MaxConfigFileBytes || len(got.Content) > MaxConfigFileBytes:
		return ConfigState{}, refused("%s exceeds the configuration file limit", d.name)
	}
	if reason := targetText(got.Content); reason != "" {
		return ConfigState{}, refused("%s is no configuration text: %s", d.name, reason)
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
			return ConfigState{}, refused("%s is no configuration target: the loop's own configuration, rules or policy", d.name)
		}
	}
	return got, nil
}

// peek reads a regular file of the directory as it is, with no target checks (a swap file, a displaced file): ok is
// false when it is absent.
func (d *targetDir) peek(file string) (ConfigState, bool, error) {
	got, _, ok, err := d.read(file)
	return got, ok, err
}

// read opens a regular file of the directory once, without following a link, and reads it and its descriptor's
// status: ok is false when it is absent.
func (d *targetDir) read(file string) (ConfigState, unix.Stat_t, bool, error) {
	var st unix.Stat_t
	fd, err := unix.Openat(d.fd, file, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err == unix.ENOENT {
		return ConfigState{}, st, false, nil
	}
	if err == unix.ELOOP {
		return ConfigState{}, st, false, refused("%s beside %s is a link", file, d.name)
	}
	if err != nil {
		return ConfigState{}, st, false, err
	}
	f := os.NewFile(uintptr(fd), file)
	defer f.Close()
	if err := unix.Fstat(fd, &st); err != nil {
		return ConfigState{}, st, false, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return ConfigState{}, st, false, refused("%s beside %s is no regular file", file, d.name)
	}
	// a rewrite keeps the mode, the owner and the group, not an ACL, a label or other extended attributes
	if n, err := unix.Flistxattr(fd, nil); err == nil && n > 0 {
		return ConfigState{}, st, false, refused("%s carries extended attributes (an ACL, a label or other metadata) a rewrite would drop", d.name)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxConfigFileBytes+1))
	if err != nil {
		return ConfigState{}, st, false, err
	}
	return ConfigState{SHA256: hashOf(b), Mode: uint32(st.Mode & 0o7777), UID: st.Uid, GID: st.Gid, Content: b}, st, true, nil
}

func (d *targetDir) sync() error { return unix.Fsync(d.fd) }

// settle clears what an interrupted write left beside the target, before a step reads the target. The scratch file
// is the step's own, whole or cut short, and was never exchanged: it goes. At the swap name, the content the step
// writes or the content it displaces is the loop's, and journaled: it goes. Anything else there came from an exchange
// over the owner's edit, and none of it goes unkept: it goes back into the file when the file holds the step's own
// content or was written before it, and what is left at the swap name then, unless it is the loop's, is kept by keep
// before it goes. The step then decides by what the file holds.
func (d *targetDir) settle(swap, ours, expected string, keep func(ConfigState) error) error {
	if err := d.clearScratch(swap); err != nil {
		return err
	}
	left, lst, ok, err := d.read(swap)
	if err != nil {
		return refused("an interrupted write left %s beside %s, which the loop cannot read: the owner decides: %v", swap, d.name, err)
	}
	if !ok {
		return nil
	}
	if left.SHA256 != ours && left.SHA256 != expected {
		cur, cst, curOK, err := d.read(d.base)
		if err != nil {
			return refused("%s cannot be read beside what an interrupted write left at %s: the owner decides: %v", d.name, swap, err)
		}
		if curOK && (cur.SHA256 == ours || newer(lst, cst)) {
			// the owner's edit was displaced by the step's own write, or is newer than what the file holds: it goes back
			if err := exchange(d.fd, swap, d.fd, d.base, unix.RENAME_EXCHANGE); err != nil {
				return fmt.Errorf("%s: the exchange back failed, the journal stays: %w", d.name, err)
			}
			if err := d.sync(); err != nil {
				return err
			}
			left = cur
		}
		if left.SHA256 != ours && left.SHA256 != expected {
			if err := keep(left); err != nil {
				return err
			}
		}
	}
	return d.unlinkIf(swap, left.SHA256)
}

// newer says whether a file was last written after another.
func newer(a, b unix.Stat_t) bool {
	return a.Mtim.Sec > b.Mtim.Sec || a.Mtim.Sec == b.Mtim.Sec && a.Mtim.Nsec > b.Mtim.Nsec
}

// scratchName is where a write puts its new file together, before the file is whole at the swap name.
func scratchName(swap string) string { return swap + ".new" }

// clearScratch removes what an interrupted write left at the scratch name, whatever it holds: only a write of the
// loop's puts a file there, and none was exchanged.
func (d *targetDir) clearScratch(swap string) error {
	if err := unix.Unlinkat(d.fd, scratchName(swap), 0); err != nil && err != unix.ENOENT {
		return err
	}
	return nil
}

// errTargetDrift marks a refusal because the target is not what the step read.
var errTargetDrift = errors.New("the target changed")

// errTargetGone marks a refusal because the target, or a directory of its path, is not there.
var errTargetGone = errors.New("the target is gone")

// unlinkIf removes the swap file only while it holds what the step knows it holds. Anything else stays for the
// owner; that is no refusal, and the step's journal and state stay.
func (d *targetDir) unlinkIf(swap, hash string) error {
	left, ok, err := d.peek(swap)
	if err == nil && !ok {
		return nil
	}
	if err != nil || left.SHA256 != hash {
		return fmt.Errorf("%s beside %s does not hold what the step put there: it is left for the owner", swap, d.name)
	}
	return errors.Join(unix.Unlinkat(d.fd, swap, 0), d.sync())
}

// replace puts content in place of the target atomically, in the mode, owner and group expect has, only while the
// target is what expect says: the target is exchanged with the new file, the displaced file is compared with expect,
// and it is exchanged back when it differs. A change never creates, removes or renames the target. The new file is put
// together and synced at the scratch name before it takes the swap name, so the swap name only ever holds a whole
// file: the step's, or the one an exchange displaced.
func (d *targetDir) replace(swap string, content []byte, expect ConfigState) error {
	if err := d.clearScratch(swap); err != nil {
		return err
	}
	scratch := scratchName(swap)
	fd, err := unix.Openat(d.fd, scratch, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), scratch)
	half := len(content) / 2
	_, err = f.Write(content[:half])
	if err == nil {
		if err := d.at("swap writing"); err != nil {
			f.Close()
			return err // as a power loss would: the scratch file stays cut short, and the next step removes it
		}
		_, err = f.Write(content[half:])
	}
	err = errors.Join(err, f.Chown(int(expect.UID), int(expect.GID)), unix.Fchmod(fd, expect.Mode&0o777))
	if n, xerr := swapXattrs(fd); err == nil && xerr == nil && n > 0 {
		// a default ACL of the directory, or a label, would give the new file access the old one lacked
		err = refused("a new file in the directory of %s carries extended attributes (a default ACL or a label): the change is not applied", d.name)
	}
	err = errors.Join(err, f.Sync(), f.Close())
	if err == nil {
		err = unix.Renameat2(d.fd, scratch, d.fd, swap, unix.RENAME_NOREPLACE)
	}
	if err != nil {
		unix.Unlinkat(d.fd, scratch, 0) // created exclusively by this step and never exchanged
		return err
	}
	if err := d.sync(); err != nil {
		return err
	}
	if err := d.at("swap written"); err != nil {
		return err
	}
	ours := hashOf(content)
	if err := exchange(d.fd, swap, d.fd, d.base, unix.RENAME_EXCHANGE); err != nil {
		cleanup := d.unlinkIf(swap, ours)
		switch err {
		case unix.ENOENT:
			return errors.Join(refused("%s is gone: the change is not applied", d.name), errTargetGone, cleanup)
		case unix.EINVAL:
			return errors.Join(refused("the file system of %s cannot exchange files atomically", d.name), cleanup)
		}
		return errors.Join(err, cleanup)
	}
	if err := d.at("exchanged"); err != nil {
		return err
	}
	displaced, ok, err := d.peek(swap)
	if err != nil || !ok || !displaced.sameFile(expect) {
		if back := exchange(d.fd, swap, d.fd, d.base, unix.RENAME_EXCHANGE); back != nil {
			// not a refusal: the file holds the change, and what it held sits at the swap name for the next step's
			// settle to put back
			return fmt.Errorf("%s changed while it was written, and the exchange back failed (%v): the next step puts it back", d.name, back)
		}
		if err := d.at("exchanged back"); err != nil {
			return err
		}
		if err := d.unlinkIf(swap, ours); err != nil {
			return err
		}
		return errors.Join(refused("%s changed while it was written: the change is not applied", d.name), errTargetDrift)
	}
	return d.unlinkIf(swap, expect.SHA256)
}

// swapXattrs is the size of a new file's attribute list; a test stands one in.
var swapXattrs = func(fd int) (int, error) { return unix.Flistxattr(fd, nil) }

func swapName(caseID string) string { return ".momentum-" + caseID + ".swap" }

// exchange is renameat2; a test injects a failure.
var exchange = unix.Renameat2

// ConfigJournal is a configuration landing's record, written before the file is: what the file held (its content,
// hash, mode, owner and group) and what the change puts there.
type ConfigJournal struct {
	Target string      `json:"target"`
	Old    ConfigState `json:"old"`
	New    ConfigState `json:"new"`
	Head   string      `json:"head"`
}

// ConfigRollback is a configuration rollback's record, written before the file is: what it overwrites (the landed
// content, in the file's mode then) and the hash it restores.
type ConfigRollback struct {
	From ConfigState `json:"from"`
	To   string      `json:"to"`
	Why  string      `json:"why"`
	// Landing marks the owner's rollback of an interrupted landing, whose case is still in testing.
	Landing bool `json:"landing,omitempty"`
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

// configTarget opens a package's target as a step may: with configuration targets on and allow naming it, or,
// structural, by the refusals alone. A refusal is a hold: it says nothing of the agent.
func (s *Ship) configTarget(target string, structural bool) (*targetDir, error) {
	if target == "" {
		return nil, refused("the configuration package names no target")
	}
	if !structural && !s.ConfigTargets.Enabled {
		return nil, &shipHold{err: refused("configuration targets are off (loop.config_targets.enabled)")}
	}
	d, err := s.openTarget(target, structural)
	if errors.Is(err, ErrRefused) {
		return nil, &shipHold{err: err}
	}
	return d, err
}

// records is a configuration package's own directory (Package.Records), which no later package of its case moves.
func (s *Ship) records(p Package) string {
	if p.Records == "" || !filepath.IsLocal(p.Records) {
		return s.pkgDir(p.Case)
	}
	return filepath.Join(s.pkgDir(p.Case), p.Records)
}

// openTarget opens a target with the ship's test hook between a write's steps.
func (s *Ship) openTarget(target string, structural bool) (*targetDir, error) {
	d, err := s.ConfigTargets.open(target, structural)
	if err == nil {
		d.stage = s.configStage
	}
	return d, err
}

// mayApply says why the loop may not write a configuration package's file now, or "": the switch is off, the loop is
// not live, or the target is refused or no longer named by allow.
func (s *Ship) mayApply(p Package) string {
	switch {
	case p.Kind != KindConfig:
		return ""
	case !s.ConfigTargets.Enabled:
		return "configuration targets are off (loop.config_targets.enabled): the loop writes no configuration file"
	case !s.Live:
		return "the loop is not live (loop.live): it proposes and does not land"
	}
	if _, err := s.ConfigTargets.admit(p.Target); err != nil {
		return "its target is not admitted now: " + strings.TrimPrefix(err.Error(), ErrRefused.Error()+": ")
	}
	return ""
}

// PrepareConfig makes a configuration package: a host-only Git repository whose base commit holds the target as the
// host reads it now, and an independent run clone of it. The target must read through the scrubber unchanged, so
// the run's /ro/config copy and its clone hold the bytes the host will patch. copied, when set, is the hash of the
// run's /ro/config copy of the target, which the file must still hold.
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
	if had && old.State != RolledBack && old.State != Kept && old.State != Withdrawn {
		return old, refused("case %s has a package already (in %s)", c.ID, old.State)
	}
	if had && old.State == Withdrawn && old.PR != "" {
		return old, refused("case %s's hand landing withdrawal is not finished yet: the next selection finishes it", c.ID)
	}
	d, err := s.configTarget(target, false)
	if err != nil {
		return Package{}, err
	}
	defer d.Close()
	base, err := d.state()
	if err != nil {
		return Package{}, &shipHold{err: err}
	}
	if copied != "" && copied != base.SHA256 {
		return Package{}, &shipHold{err: refused("%s changed since the run's configuration copy was made: the run did not read the file it would change", d.name)}
	}
	unlockTarget, err := s.lockTarget(d.name)
	if err != nil {
		return Package{}, err
	}
	defer unlockTarget()
	if other, err := s.targetBusy(ctx, c.ID, d.name, base.SHA256); err != nil {
		return Package{}, err
	} else if other != "" {
		return Package{}, &shipHold{err: refused("%s is the target of case %s's change, which is not done: the loop leaves it alone", d.name, other)}
	}
	clean, err := RedactAll(ctx, []string{d.name, string(base.Content)})
	if err != nil {
		return Package{}, err
	}
	if clean[0] != d.name || clean[1] != string(base.Content) {
		return Package{}, &shipHold{err: refused("%s holds text the scrubber would mask: a run's copy of it is not the file, so the loop cannot change it", d.name)}
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
	var id [4]byte
	rand.Read(id[:])
	p := Package{Case: c.ID, Kind: KindConfig, Target: d.name, TargetBase: base.bare(), Branch: branch, Work: work, Host: host,
		State: Prepared, Created: s.now()}
	p.Records = filepath.Join("config", p.Created.UTC().Format("20060102T150405Z")+"-"+hex.EncodeToString(id[:]))
	if had && old.State == Withdrawn {
		p.ClosedProposals = old.ClosedProposals // a closed hand landing proposal counts toward the case's two
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

// landConfig writes a reviewed configuration change to the real file: only while configuration targets are on and
// the loop is live, its last review is clean and posted on its head, the file passes every refusal again and still
// holds what the package was prepared on. The file's content, hash, mode, owner and group are journaled before the
// write, and the new content takes the file's mode, owner and group as they are then. A file the owner changed since
// the proposal is never overwritten: the change goes back to its build, on the file as it is (rebuildOver).
func (s *Ship) landConfig(ctx context.Context, p Package) (Package, error) {
	if p.State == Landing {
		return s.resumeConfigLanding(ctx, p)
	}
	switch {
	case p.State != Proposed:
		return p, refused("case %s has no proposed package to land", p.Case)
	case s.mayApply(p) != "":
		return p, &shipHold{err: refused("%s", s.mayApply(p))}
	case !p.Clean():
		return p, refused("case %s's last review is not clean on its head, or was not posted: no landing", p.Case)
	case p.Check == nil || p.TargetBase == nil:
		return p, refused("case %s's package has no check", p.Case)
	}
	if _, err := s.working(ctx, p.Case); err != nil {
		return p, err
	}
	if head, err := s.git(ctx, p.Host, "rev-parse", "HEAD"); err != nil || strings.TrimSpace(head) != p.Head {
		return p, refused("the host repository's head is not the reviewed %s: no landing", p.Head)
	}
	content, err := s.configContent(ctx, p, p.Head)
	if err != nil {
		return p, err
	}
	d, err := s.configTarget(p.Target, false)
	if err != nil {
		return p, err
	}
	defer d.Close()
	unlockTarget, err := s.lockTarget(p.Target)
	if err != nil {
		return p, err
	}
	defer unlockTarget()
	if err := d.settle(swapName(p.Case), hashOf(content), p.TargetBase.SHA256, s.keepDisplaced(ctx, p)); err != nil {
		return p, &shipHold{err: err}
	}
	cur, err := d.state()
	if err != nil {
		return p, &shipHold{err: err}
	}
	// against the file's bytes when this package was prepared and proposed, not any earlier landing's
	if cur.SHA256 != p.TargetBase.SHA256 {
		return s.rebuildOver(ctx, p, refused("%s changed since the proposal: the loop does not overwrite it", p.Target))
	}
	if other, err := s.targetBusy(ctx, p.Case, p.Target, cur.SHA256); err != nil {
		return p, err
	} else if other != "" {
		return p, &shipHold{err: refused("%s is the target of case %s's change, which is not done: no landing", p.Target, other)}
	}
	j := ConfigJournal{Target: p.Target, Old: cur, New: ConfigState{SHA256: hashOf(content), Content: content}, Head: p.Head}
	if err := s.assignedAuthority(ctx); err != nil {
		return p, err
	}
	if err := writeDurable(s.records(p), configJournal, j); err != nil {
		return p, err
	}
	if err := s.configStage("journal written"); err != nil {
		return p, err
	}
	p.State, p.LandingHead = Landing, p.Head
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, err
	}
	if err := s.configStage("journaled"); err != nil {
		return p, err
	}
	if err := d.replace(swapName(p.Case), j.New.Content, cur); err != nil {
		return s.landingRefused(ctx, p, err)
	}
	if err := s.configStage("written"); err != nil {
		return p, err
	}
	return s.finishConfigLanding(ctx, p, j)
}

// landingRefused takes a landing the file refused, with nothing of the change in it, out of Landing. A file the owner
// changed while it was written goes back to the build (rebuildOver); any other refusal proposes the package again,
// held, with a case note, and spends no review round. An error that is no refusal keeps the journal and the Landing
// state for the next step.
func (s *Ship) landingRefused(ctx context.Context, p Package, cause error) (Package, error) {
	if !errors.Is(cause, ErrRefused) {
		return p, cause
	}
	if errors.Is(cause, errTargetDrift) {
		return s.rebuildOver(ctx, p, cause)
	}
	p.State, p.LandingHead = Proposed, ""
	cause = &shipHold{err: cause}
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, errors.Join(cause, err)
	}
	if err := os.Remove(filepath.Join(s.records(p), configJournal)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return p, errors.Join(cause, err)
	}
	return p, errors.Join(cause, s.runner().caseNoteOnce(ctx, p.Case, "not landed", cause.Error()))
}

// rebuildOver takes a package whose target the owner changed before anything of its change went in back to its
// build: the next build brings it to the file as it is (refreshConfigBase), and the change is built, checked and
// reviewed again on his content. Nothing waits for him.
func (s *Ship) rebuildOver(ctx context.Context, p Package, cause error) (Package, error) {
	p.State, p.LandingHead = Revising, ""
	cause = &shipHold{err: fmt.Errorf("%w; the loop builds the change again on the file as it is", cause)}
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, errors.Join(cause, err)
	}
	if err := os.Remove(filepath.Join(s.records(p), configJournal)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return p, errors.Join(cause, err)
	}
	return p, errors.Join(cause, s.runner().caseNoteOnce(ctx, p.Case, "not landed", cause.Error()))
}

// refreshConfigBase brings a package that has not landed to its target as the owner changed it: a new base commit of
// the file's content, no check or proposed head, its reviews kept, so its rounds count on. The next build adapts
// previous.patch to it. As PrepareConfig, it needs the file free of other cases' changes and clean of the scrubber.
// The caller holds the case's lock.
func (s *Ship) refreshConfigBase(ctx context.Context, p Package, cur ConfigState) (Package, error) {
	if roundsUsed(p) {
		return p, refused("review rounds used; the change waits for a person")
	}
	unlockTarget, err := s.lockTarget(p.Target)
	if err != nil {
		return p, err
	}
	defer unlockTarget()
	if other, err := s.targetBusy(ctx, p.Case, p.Target, cur.SHA256); err != nil {
		return p, err
	} else if other != "" {
		return p, &shipHold{err: refused("%s is the target of case %s's change, which is not done: the loop leaves it alone", p.Target, other)}
	}
	clean, err := RedactAll(ctx, []string{string(cur.Content)})
	if err != nil {
		return p, err
	}
	if clean[0] != string(cur.Content) {
		return p, &shipHold{err: refused("%s holds text the scrubber would mask: a run's copy of it is not the file, so the loop cannot change it", p.Target)}
	}
	base, err := s.configBase(s.gitContext(shipContext(ctx, p.Case)), p, cur.Content)
	if err != nil {
		return p, err
	}
	p.BaseCommit, p.TargetBase, p.Check, p.Head, p.State = base, cur.bare(), nil, "", Prepared
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, err
	}
	return p, s.Store.CaseNote(cleanedNote(ctx), p.Case, "prepared", fmt.Sprintf("configuration %s at %.12s, the file as the owner changed it", p.Target, p.BaseCommit), s.Actor)
}

// resumeConfigLanding finishes a landing a crash interrupted, from its journal, with every check of a landing again:
// the file holds the new content (done), the old (written again, in its mode now), or neither (the owner's: the
// landing is set aside, setAside).
func (s *Ship) resumeConfigLanding(ctx context.Context, p Package) (Package, error) {
	if s.landingRollback(p) {
		// the owner's rollback of this landing recorded itself and stopped before its state: it is his to finish
		return p, &shipHold{err: refused("the owner is rolling back this landing: momentum case rollback %s finishes it", p.Case)}
	}
	if why := s.mayApply(p); why != "" {
		return p, &shipHold{err: refused("resuming the landing: %s", why)}
	}
	if _, err := s.working(ctx, p.Case); err != nil {
		return p, err
	}
	var j ConfigJournal
	ok, err := readRecord(s.records(p), configJournal, &j)
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
	d, err := s.configTarget(p.Target, false)
	if err != nil {
		return p, err
	}
	defer d.Close()
	unlockTarget, err := s.lockTarget(p.Target)
	if err != nil {
		return p, err
	}
	defer unlockTarget()
	if err := d.settle(swapName(p.Case), j.New.SHA256, j.Old.SHA256, s.keepDisplaced(ctx, p)); err != nil {
		return p, s.landingHeld(ctx, p, err)
	}
	cur, err := d.state()
	if errors.Is(err, errTargetGone) {
		return s.setAside(ctx, p)
	} else if err != nil {
		return p, &shipHold{err: err}
	}
	switch cur.SHA256 {
	case j.New.SHA256:
	case j.Old.SHA256:
		if err := d.replace(swapName(p.Case), j.New.Content, cur); err != nil {
			return s.landingRefused(ctx, p, err)
		}
		if err := s.configStage("written"); err != nil {
			return p, err
		}
	default:
		return s.setAside(ctx, p)
	}
	return s.finishConfigLanding(ctx, p, j)
}

// landingHeld holds an interrupted landing the loop cannot finish (what it left beside the file cannot be read), its
// journal and state kept: the owner's way out is momentum case rollback.
func (s *Ship) landingHeld(ctx context.Context, p Package, cause error) error {
	cause = &shipHold{err: fmt.Errorf("%w; its content from before the change is in %s, and momentum case rollback %s undoes the landing",
		cause, filepath.Join(s.records(p), configJournal), p.Case)}
	return errors.Join(cause, s.runner().caseNoteOnce(ctx, p.Case, "not landed", cause.Error()))
}

// keptOwners is what the case log says of a write that found the owner's content in its file and wrote nothing over
// it: where the file's content from before the change is.
func (s *Ship) keptOwners(p Package) string {
	return fmt.Sprintf("%s holds neither its old nor its new content (an edit of the owner's, or no file), so the loop leaves it as it is; its content from before the change is in %s",
		p.Target, filepath.Join(s.records(p), configJournal))
}

// keepDisplaced keeps a content an interrupted write displaced, which is the owner's, in the package's records, and
// names it in the case log.
func (s *Ship) keepDisplaced(ctx context.Context, p Package) func(ConfigState) error {
	return func(c ConfigState) error {
		name := "displaced-" + c.SHA256[:16] + ".json"
		if err := writeDurable(s.records(p), name, c); err != nil {
			return err
		}
		return s.Store.CaseNote(cleanedNote(ctx), p.Case, "displaced content kept",
			fmt.Sprintf("an interrupted write left an earlier content of %s beside it, which is kept in %s", p.Target, filepath.Join(s.records(p), name)), s.Actor)
	}
}

// setAside ends the loop's landing that finds the owner's content in its file: the change may lie under his edit,
// so nothing is written. The package is rolled back with its journal kept and named in the case log, which frees the
// file, and the case, still in testing, builds again on the file as it is.
func (s *Ship) setAside(ctx context.Context, p Package) (Package, error) {
	p.State, p.LandingHead = RolledBack, ""
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, err
	}
	kept := s.keptOwners(p)
	if err := s.Store.CaseNote(cleanedNote(ctx), p.Case, "landing rolled back", kept, s.Actor); err != nil {
		return p, err
	}
	return p, &shipHold{err: refused("%s; the landing is rolled back, and the case builds again on the file as it is", kept)}
}

// abandonConfigLanding is the owner's rollback of an interrupted landing: the file gets its journaled content back
// when it holds the change, and is left when it holds its old content or neither (his edit, or no file, kept, with the
// journal named in the case log). The package is then rolled back and the case, still in testing, builds again.
func (s *Ship) abandonConfigLanding(ctx context.Context, p Package, why string) (Package, error) {
	var j ConfigJournal
	ok, err := readRecord(s.records(p), configJournal, &j)
	if err != nil {
		return p, err
	}
	var rb ConfigRollback
	if p.State == RollingBack {
		if had, err := readRecord(s.records(p), configRollback, &rb); err != nil {
			return p, err
		} else if !had || !rb.Landing {
			return p, refused("case %s's rollback journal is missing", p.Case)
		}
	}
	if ok && j.Target == p.Target && j.Head == p.LandingHead {
		d, err := s.openTarget(p.Target, true)
		if err != nil {
			if errors.Is(err, ErrRefused) {
				return p, &shipHold{err: err}
			}
			return p, err
		}
		defer d.Close()
		unlockTarget, err := s.lockTarget(p.Target)
		if err != nil {
			return p, err
		}
		defer unlockTarget()
		// what lies at the swap name is the landing's own (it wrote the change) until this rollback writes
		ours, displaced := j.New.SHA256, j.Old.SHA256
		if p.State == RollingBack {
			ours, displaced = j.Old.SHA256, j.New.SHA256
		}
		if err := d.settle(swapName(p.Case), ours, displaced, s.keepDisplaced(ctx, p)); err != nil {
			return p, &shipHold{err: err}
		}
		cur, err := d.state()
		gone := errors.Is(err, errTargetGone)
		if err != nil && !gone {
			return p, &shipHold{err: err}
		}
		switch {
		case gone || cur.SHA256 != j.Old.SHA256 && cur.SHA256 != j.New.SHA256:
			why += "; " + s.keptOwners(p)
		case cur.SHA256 == j.New.SHA256:
			if p.State != RollingBack {
				// the rollback's own record and state before the write: a crash after it never lands the change again
				rb = ConfigRollback{From: *cur.bare(), To: j.Old.SHA256, Why: why, Landing: true}
				if err := s.assignedAuthority(ctx); err != nil {
					return p, err
				}
				if err := writeDurable(s.records(p), configRollback, rb); err != nil {
					return p, err
				}
				if err := s.configStage("rollback record written"); err != nil {
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
			if err := d.replace(swapName(p.Case), j.Old.Content, cur); errors.Is(err, errTargetDrift) {
				why += "; " + s.keptOwners(p) // his edit came while it was written, and is back in the file
			} else if errors.Is(err, ErrRefused) {
				return p, &shipHold{err: err}
			} else if err != nil {
				return p, err
			} else if err := s.configStage("rolled back"); err != nil {
				return p, err
			}
		}
	}
	p.State, p.LandingHead = RolledBack, ""
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, err
	}
	return p, s.Store.CaseNote(cleanedNote(ctx), p.Case, "landing rolled back", why, s.Actor)
}

func (s *Ship) finishConfigLanding(ctx context.Context, p Package, j ConfigJournal) (Package, error) {
	p.Head, p.State, p.Landed, p.TargetLanded = p.LandingHead, Landed, []string{p.LandingHead}, j.New.SHA256
	if p.LandedAt.IsZero() {
		p.LandedAt = s.now()
	}
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, err
	}
	if err := s.configStage("landed"); err != nil {
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

// rollBackConfig restores a landed configuration file's journaled content, in the file's mode, owner and group as
// they are then, with every refusal checked again. The loop's own rollbacks (the measure step's, a resumed one) also
// need configuration targets on, the loop live and allow naming the file; the owner's (owner) does not. A file that
// holds neither the landed content nor the journaled one is the owner's, or he removed it: nothing is written over it
// (keepRolledBack). A resumed rollback reads its record: the file holds the restored content (done), what the
// rollback overwrites (written again), or the owner's (kept).
func (s *Ship) rollBackConfig(ctx context.Context, p Package, why string, owner bool) (Package, error) {
	if hold := s.mayApply(p); !owner && hold != "" {
		return p, &shipHold{err: refused("the rollback waits: %s", hold)}
	}
	c, err := s.Store.CaseByID(ctx, p.Case)
	if err != nil {
		return p, err
	}
	if c.Status != store.CaseDeployed && c.Status != store.CaseMeasuring {
		return p, refused("only a deployed or measuring case has a change to roll back")
	}
	var j ConfigJournal
	ok, err := readRecord(s.records(p), configJournal, &j)
	if err != nil {
		return p, err
	}
	if !ok || j.Target != p.Target || p.TargetLanded == "" || j.New.SHA256 != p.TargetLanded {
		return p, refused("the landing's journal of %s is missing or does not match its package: restore it by hand", p.Target)
	}
	d, err := s.openTarget(p.Target, owner)
	if errors.Is(err, errTargetGone) {
		return s.keepRolledBack(ctx, p, why)
	} else if err != nil {
		if errors.Is(err, ErrRefused) {
			return p, &shipHold{err: err}
		}
		return p, err
	}
	defer d.Close()
	unlockTarget, err := s.lockTarget(p.Target)
	if err != nil {
		return p, err
	}
	defer unlockTarget()
	var rb ConfigRollback
	if p.State == RollingBack {
		ok, err := readRecord(s.records(p), configRollback, &rb)
		if err != nil {
			return p, err
		}
		if !ok {
			p.State = Landed // the step stopped before its record: nothing was written
		}
	}
	// what lies at the swap name is the landing's own until this rollback writes
	ours, displaced := j.New.SHA256, j.Old.SHA256
	if p.State == RollingBack {
		ours, displaced = j.Old.SHA256, rb.From.SHA256
	}
	if err := d.settle(swapName(p.Case), ours, displaced, s.keepDisplaced(ctx, p)); err != nil {
		return p, &shipHold{err: err}
	}
	cur, err := d.state()
	if errors.Is(err, errTargetGone) {
		return s.keepRolledBack(ctx, p, why)
	} else if err != nil {
		return p, &shipHold{err: err}
	}
	switch {
	case cur.SHA256 == j.Old.SHA256: // restored already, by hand or by an earlier step
	case p.State == Landed && cur.SHA256 == j.New.SHA256 || p.State == RollingBack && cur.SHA256 == rb.From.SHA256:
		if p.State == Landed {
			rb = ConfigRollback{From: *cur.bare(), To: j.Old.SHA256, Why: why}
			if err := s.assignedAuthority(ctx); err != nil {
				return p, err
			}
			if err := writeDurable(s.records(p), configRollback, rb); err != nil {
				return p, err
			}
			if err := s.configStage("rollback record written"); err != nil {
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
		if err := d.replace(swapName(p.Case), j.Old.Content, cur); errors.Is(err, errTargetDrift) {
			return s.keepRolledBack(ctx, p, why) // his edit came while it was written, and is back in the file
		} else if errors.Is(err, ErrRefused) {
			return p, &shipHold{err: err}
		} else if err != nil {
			return p, err
		}
	default:
		return s.keepRolledBack(ctx, p, why)
	}
	if err := s.configStage("rolled back"); err != nil {
		return p, err
	}
	p.State = RolledBack
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, err
	}
	if err := s.configStage("rollback saved"); err != nil {
		return p, err
	}
	return p, s.rollbackReceipt(ctx, p, why)
}

// keepRolledBack ends a rollback that finds the owner's content in its file, or no file: nothing is written over it.
// The package is rolled back with its journal kept and named in the case log, which frees the file, and its case goes
// back to testing, where the loop builds again on the file as it is.
func (s *Ship) keepRolledBack(ctx context.Context, p Package, why string) (Package, error) {
	if p.Undo == nil {
		p.Undo = &RollbackJournal{}
	}
	if p.Undo.Why != "" {
		why = p.Undo.Why
	}
	why += "; " + s.keptOwners(p)
	p.State, p.Undo.Why = RolledBack, why
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, err
	}
	if err := s.configStage("rollback saved"); err != nil {
		return p, err
	}
	return p, s.rollbackReceipt(ctx, p, why)
}

// configInEffect: a configuration change is in effect while its target, read by the refusals, holds the landed content.
func (s *Ship) configInEffect(p *Package) (string, error) {
	d, err := s.ConfigTargets.open(p.Target, true)
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
	if cur.SHA256 != p.TargetLanded {
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

// targetBusy names another case whose configuration package on the same file is not done: its change is on its way
// in or out (landing, rolling back), or in the file now (landed, its case deployed or measuring, and the file, whose
// hash is cur, still holding what it landed), or its case is live and the package neither landed, rolled back nor
// kept. A landed change the owner edited since, or whose case is over, holds nothing up: a later case proposes against
// the file as it is. Two changes in flight on one file would overwrite each other and confound both measurements.
func (s *Ship) targetBusy(ctx context.Context, self, target, cur string) (string, error) {
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
		if !ok || p.Kind != KindConfig || p.Target != target {
			continue
		}
		switch p.State {
		case Landing, RollingBack:
			return p.Case, nil
		case RolledBack, Kept:
			continue
		}
		c, err := s.Store.CaseByID(ctx, p.Case)
		if errors.Is(err, store.ErrNoCase) {
			continue
		}
		if err != nil {
			return "", err
		}
		if p.State == Landed {
			// in the file and its case not over, whether it is deployed yet or not (a landing whose deployment step
			// failed or was cut short is still the file's change)
			if c.Status != store.CaseClosed && c.Into == "" && cur == p.TargetLanded {
				return p.Case, nil
			}
			continue
		}
		if c.Status != store.CaseClosed && c.Into == "" {
			return p.Case, nil
		}
	}
	return "", nil
}

// lockTarget serializes the steps of one file's packages: a busy check and the write or the package it allows are
// one step.
func (s *Ship) lockTarget(target string) (func(), error) {
	dir := filepath.Join(s.Dir, "targets")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, hashOf([]byte(target))+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if err == unix.EWOULDBLOCK {
			return nil, &shipHold{err: refused("another step on %s is running: the loop leaves it alone", target)}
		}
		return nil, err
	}
	return func() { unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }, nil
}

// landingRollback says whether a package rolling back is the owner's rollback of an interrupted landing.
func (s *Ship) landingRollback(p Package) bool {
	var rb ConfigRollback
	ok, err := readRecord(s.records(p), configRollback, &rb)
	return err == nil && ok && rb.Landing
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

// heldTarget is heldBuild for a repository target; a configuration target has no repository gate of its own, its
// authority is checked by the ship tool at every step (configTarget).
func (r *Runner) heldTarget(ctx context.Context, repo, target string) (string, bool) {
	if target != "" {
		return "", true
	}
	return r.heldBuild(ctx, repo)
}
