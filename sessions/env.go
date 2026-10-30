package sessions

import (
	"os"
	"path/filepath"
	"strings"
)

// Env says where each harness keeps its files on this machine.
type Env struct {
	Home       string
	ClaudeDir  string   // Claude Code: $CLAUDE_CONFIG_DIR, default ~/.claude
	CodexHome  string   // Codex: $CODEX_HOME, default ~/.codex
	DataHome   string   // opencode's database lives under it: $XDG_DATA_HOME, default ~/.local/share
	HermesHome string   // Hermes: $HERMES_HOME, default ~/.hermes
	PiDir      string   // pi: $PI_CODING_AGENT_DIR, default ~/.pi/agent
	Proc       string   // the proc filesystem, default /proc
	Projects   []string // directories whose subpaths name projects; none by default
}

// EnvFromOS reads the harnesses' own variables. A relative value is ignored, as the XDG rules ask.
func EnvFromOS() Env {
	home, _ := os.UserHomeDir()
	dir := func(key string, def ...string) string {
		v := os.Getenv(key)
		if rest, ok := strings.CutPrefix(v, "~/"); ok {
			v = filepath.Join(home, rest)
		}
		if v != "" && filepath.IsAbs(v) {
			return filepath.Clean(v)
		}
		return filepath.Join(append([]string{home}, def...)...)
	}
	return Env{
		Home:       home,
		ClaudeDir:  dir("CLAUDE_CONFIG_DIR", ".claude"),
		CodexHome:  dir("CODEX_HOME", ".codex"),
		DataHome:   dir("XDG_DATA_HOME", ".local", "share"),
		HermesHome: dir("HERMES_HOME", ".hermes"),
		PiDir:      dir("PI_CODING_AGENT_DIR", ".pi", "agent"),
		Proc:       "/proc",
	}
}

// Project names the project a directory belongs to: its path under the first project root that holds it, else
// "home", a path under the home directory as ~/..., or the directory itself.
func (e Env) Project(cwd string) string {
	if cwd == "" {
		return ""
	}
	for _, root := range e.Projects {
		if rel, ok := under(root, cwd); ok {
			return rel
		}
	}
	if e.Home != "" && cwd == e.Home {
		return "home"
	}
	if rel, ok := under(e.Home, cwd); ok {
		return "~/" + rel
	}
	return cwd
}

// under returns path's part below root, when path is strictly below it.
func under(root, path string) (string, bool) {
	if root == "" {
		return "", false
	}
	rel, ok := strings.CutPrefix(path, strings.TrimSuffix(root, "/")+"/")
	return rel, ok && rel != ""
}
