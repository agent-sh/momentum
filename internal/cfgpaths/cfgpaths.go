// Package cfgpaths is the one list of the agent system's configuration paths: what a run's configuration copy reads,
// and, under a stricter denylist and the owner's explicit allowlist, the files a configuration package may change.
// The loop and the configuration loader both read it, so the two cannot drift.
package cfgpaths

import (
	"fmt"
	pathpkg "path"
	"path/filepath"
	"strings"
)

// Roots are where the agent system's own configuration lives, for every harness the machine runs: Claude Code,
// Codex, Hermes, pi, opencode and grok (kimi has no directory of its own here: its launcher is a script in .local/bin),
// the files agent-sync writes for all of them, the units that run them and momentum's own settings.
var Roots = []string{".claude/mods", ".claude/hooks", ".claude/settings.json",
	".codex", ".hermes", ".pi/agent", ".config/opencode", ".grok",
	".config/agents", ".config/systemd/user", ".config/momentum", ".local/bin"}

// Allow is the allowlist inside the roots that hold much besides configuration (sessions, logs, databases,
// credentials, caches): only these relative paths are taken. A pattern is a path.Match pattern, or a directory and
// "/**" for everything under it. A root not listed here is taken whole, under the denylist.
var Allow = map[string][]string{
	".codex":           {"config.toml", "*.config.toml", "AGENTS.md", "hooks.json", "rules/**", "skills/**", "agents/**"},
	".hermes":          {"config.yaml", "profile.yaml", "SOUL.md", "skills/**", "scripts/**", "plugins/**", "hooks/**", "cron/jobs.json"},
	".pi/agent":        {"settings.json", "AGENTS.md", "extensions/**"},
	".config/opencode": {"opencode.json", "cli.json", "service.json", "package.json", "plugins/**"},
	".grok":            {"config.toml", "AGENTS.md", "sandbox.toml", "hooks-paths", "hooks/**", "skills/**"},
	".config/agents":   {"*.md", "*.txt", "procedures/**"},
}

// PathAllowed says whether a path under a root is taken (file) or walked (directory) by the root's allowlist.
func PathAllowed(root, path string, dir bool) bool {
	patterns, ok := Allow[root]
	if !ok || path == root {
		return true
	}
	rel := strings.TrimPrefix(filepath.ToSlash(path), root+"/")
	for _, p := range patterns {
		if base, tree := strings.CutSuffix(p, "/**"); tree {
			if rel == base || strings.HasPrefix(rel, base+"/") || dir && strings.HasPrefix(base, rel+"/") {
				return true
			}
			continue
		}
		if matched, _ := pathpkg.Match(p, rel); matched && !dir {
			return true
		}
		if dir && strings.HasPrefix(p, rel+"/") {
			return true
		}
	}
	return false
}

// Denied is the copy's denylist: why a path is left out, or "".
func Denied(root, path string) string {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i, part := range parts {
		lower := strings.ToLower(part)
		for _, denied := range []string{"credential", "secret", "token", "passwd", ".pem", ".key", "id_rsa", ".env", "keyring",
			"oauth", "apikey", "api_key", "api-key", "google_", "vault", "pairing", ".sqlite", ".db", ".jsonl", ".log"} {
			if strings.Contains(lower, denied) {
				return "denied name"
			}
		}
		if lower == "auth" || strings.HasPrefix(lower, "auth.") || strings.HasPrefix(lower, "auth-") || strings.HasPrefix(lower, "auth_") {
			return "denied name"
		}
		if lower == ".ssh" || lower == ".aws" || lower == ".fireworks" || lower == ".claude.json" {
			return "denied name"
		}
		if i < len(parts)-1 && ExcludedDir(part) {
			return "excluded directory"
		}
	}
	if root == ".local/bin" && path != root && filepath.Dir(path) != root {
		return "not a direct script"
	}
	return ""
}

// ExcludedDir names the build and dependency directories every root leaves out.
func ExcludedDir(name string) bool {
	switch name {
	case "node_modules", "dist", "build", "out", "target", ".next", "__pycache__", ".git":
		return true
	}
	return false
}

// RootOf is the root a home-relative path lies in, or "".
func RootOf(name string) string {
	for _, root := range Roots {
		if name == root || strings.HasPrefix(name, root+"/") {
			return root
		}
	}
	return ""
}

// noTargetRoots are copy roots no target lies in: the launchers on PATH (the credential tools and the programs the
// loop resolves at runtime among them) and the loop's own configuration.
var noTargetRoots = map[string]string{".local/bin": "a program on PATH", ".config/momentum": "the loop's own configuration"}

// targetDenied are parts of a name a target never has, anywhere in any component, on top of the copy's denylist:
// credentials, keys, brokers and auth of any spelling, other key stores, and databases and their journals.
var targetDenied = []string{"cred", "key", "broker", "auth", "secret", "token", ".netrc", ".npmrc", ".pypirc", ".gnupg",
	"known_hosts", ".kube", ".docker", ".sqlite3", "-wal", "-shm", "-journal", ".p12", ".pfx", ".gpg", ".asc", "bedrock",
	"models.json"}

// unitTypes are systemd's unit types, whose top-level and prefix drop-in directories apply to many units.
var unitTypes = []string{"service", "socket", "timer", "path", "target", "mount", "automount", "swap", "slice", "scope", "device"}

// targetPrograms are the programs the loop and its units resolve at runtime: no target is named as one of them.
var targetPrograms = []string{"git", "gh", "waitfor", "claude", "codex", "node", "python3", "go", "bwrap", "systemd-run", "sh", "bash"}

// TargetDenied is why a path is no configuration target, or "": the copy's denylist, the roots no target lies in,
// the target denylist, and any component that begins with momentum (the loop's units and programs).
func TargetDenied(root, name string) string {
	if reason := Denied(root, name); reason != "" {
		return reason
	}
	if why, ok := noTargetRoots[root]; ok {
		return why
	}
	for _, part := range strings.Split(name, "/") {
		lower := strings.ToLower(part)
		if strings.HasPrefix(lower, "momentum") {
			return "the loop's own"
		}
		for _, denied := range targetDenied {
			if strings.Contains(lower, denied) {
				return "denied name"
			}
		}
		// systemd's top-level drop-ins (service.d/, timer.d/) and prefix drop-ins (foo-.service.d/) reach many units
		if dir, ok := strings.CutSuffix(lower, ".d"); ok {
			for _, unit := range unitTypes {
				if dir == unit || strings.HasSuffix(dir, "-."+unit) {
					return "a drop-in directory for many units"
				}
			}
		}
		stem := strings.TrimSuffix(lower, pathpkg.Ext(lower))
		for _, program := range targetPrograms {
			if lower == program || stem == program {
				return "a program the loop runs"
			}
		}
	}
	return ""
}

// TargetName is a target's clean home-relative name: a path under /ro/config, or under ~/, as a run names it.
func TargetName(name string) (string, error) {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, "/ro/config/")
	name = strings.TrimPrefix(name, "~/")
	if name == "" || strings.ContainsAny(name, "\\\x00\r\n") || pathpkg.IsAbs(name) || pathpkg.Clean(name) != name {
		return "", fmt.Errorf("a configuration target is a clean path under the home: %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == "." || part == ".." {
			return "", fmt.Errorf("a configuration target has a refused component: %q", name)
		}
	}
	return name, nil
}

// AllowEntry checks one loop.config_targets.allow entry: an exact file, or a directory (written plain or with "/**")
// whose existing files it admits, inside a copy root that admits targets and not denied. It returns the entry's clean
// path and whether it is a directory entry by its "/**".
func AllowEntry(entry string) (string, bool, error) {
	literal, tree := strings.CutSuffix(strings.TrimPrefix(strings.TrimSpace(entry), "~/"), "/**")
	name, err := TargetName(literal)
	if err != nil {
		return "", false, fmt.Errorf("loop.config_targets.allow entry %q: %v", entry, err)
	}
	if strings.ContainsAny(name, "*?[") {
		return "", false, fmt.Errorf("loop.config_targets.allow entry %q is a pattern: name exact files or directories (a trailing /** is the directory)", entry)
	}
	root := RootOf(name)
	switch {
	case root == "":
		return "", false, fmt.Errorf("loop.config_targets.allow entry %q lies outside the configuration the loop reads", entry)
	case TargetDenied(root, name) != "":
		return "", false, fmt.Errorf("loop.config_targets.allow entry %q is no configuration target: %s", entry, TargetDenied(root, name))
	case !PathAllowed(root, name, false) && !PathAllowed(root, name, true):
		return "", false, fmt.Errorf("loop.config_targets.allow entry %q is no configuration", entry)
	}
	return name, tree, nil
}

// CheckAllow checks every allow entry; an empty list is valid and admits nothing.
func CheckAllow(entries []string) error {
	for _, e := range entries {
		if _, _, err := AllowEntry(e); err != nil {
			return err
		}
	}
	return nil
}
