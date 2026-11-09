package loop

import (
	"context"
	"slices"
	"strings"
)

// A patch is refused rather than rewritten. Scan the host's cumulative tree
// diff, including binary-classified files as text, before any publication.
func (s *Ship) checkPublicPatch(ctx context.Context, p Package, candidate string) error {
	private, err := s.repositoryPrivate(ctx, p)
	if err != nil || private {
		return err
	}
	base := p.BaseCommit
	if p.State == Landing {
		base = "origin/" + p.Base
	}
	names, err := s.git(ctx, p.Host, "diff", "--no-ext-diff", "--no-textconv", "--no-renames",
		"--name-only", "--diff-filter=A", "-z", base, candidate, "--")
	if err != nil {
		return err
	}
	if err := patchText(ctx, strings.ReplaceAll(names, "\x00", "\n"), "added file names"); err != nil {
		return err
	}
	diff, err := s.git(ctx, p.Host, "diff", "--no-ext-diff", "--no-textconv", "--no-renames",
		"--no-color", "--text", "-U0", base, candidate, "--")
	if err != nil {
		return err
	}
	var added strings.Builder
	inHunk := false
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			inHunk = false
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case inHunk && strings.HasPrefix(line, "+"):
			added.WriteString(line[1:])
			added.WriteByte('\n')
		}
	}
	return patchText(ctx, added.String(), "added lines")
}

func patchText(ctx context.Context, text, part string) error {
	if checked := withoutStandardHomes(text); maskPersonal(checked) != checked {
		return refused("patch %s contain personal content (a home path, address, IP or domain-shaped host; "+
			"a test fixture names a path under its temporary directory, not the home)", part)
	}
	clean, err := Redact(ctx, text)
	if err != nil {
		return err
	}
	if clean != text {
		return refused("patch %s contain content the secret scrubber would mask", part)
	}
	return nil
}

// standardHomePaths are the harnesses' and momentum's own directories and files under the home that momentum's sources
// and docs name, each one every install of its tool has. A patch that names one says nothing of its owner. Each is
// admitted as it is and nothing below it: a name a user gives a hook, a mod, a script or a program is his own. Only the
// public-patch check reads this list; evidence masks these paths as any other home path (maskPersonal).
var standardHomePaths = []string{
	// directories
	"~/.claude",                  // Claude Code (CLAUDE_CONFIG_DIR)
	"~/.codex",                   // Codex (CODEX_HOME)
	"~/.hermes",                  // Hermes (HERMES_HOME)
	"~/.hermes/scripts",          // Hermes's job scripts
	"~/.pi/agent",                // pi (PI_CODING_AGENT_DIR)
	"~/.config/opencode",         // opencode's configuration
	"~/.local/share/opencode",    // opencode's sessions
	"~/.grok",                    // Grok (GROK_HOME)
	"~/.kimi-code",               // Kimi (KIMI_CODE_HOME)
	"~/.config/momentum",         // momentum's configuration
	"~/.local/share/momentum",    // momentum's cases and evidence
	"~/.local/share/tacit/store", // tacit's store
	"~/.local/share",             // XDG_DATA_HOME's default
	"~/.local/bin",               // where the harnesses' installers put their launchers
	"~/.cargo/bin",               // where cargo installs programs
	"~/.config/systemd/user",     // the user's units
	// files
	"~/.claude.json",                      // Claude Code's user configuration
	"~/.claude/settings.json",             // Claude Code's user settings
	"~/.local/bin/claude",                 // Claude Code's native launcher
	"~/.codex/config.toml",                // Codex's configuration
	"~/.hermes/config.yaml",               // Hermes's configuration
	"~/.hermes/cron/jobs.json",            // Hermes's scheduled jobs
	"~/.config/momentum/sources.json",     // momentum's sources
	"~/.local/share/momentum/momentum.db", // momentum's store
}

// standardHome says whether a home path a patch names (a homePathRe match, with what ends a sentence after it) names
// nothing of its owner: the home itself, or an entry of standardHomePaths (a directory with or without its slash).
func standardHome(match string) bool {
	path := strings.TrimRight(match, ".,;:!?]}")
	return path == "~/" || slices.Contains(standardHomePaths, strings.TrimSuffix(path, "/"))
}

// withoutStandardHomes is text with the home prefix taken off each standard home path it names, so every other home
// path stays for the personal masks.
func withoutStandardHomes(text string) string {
	return homePathRe.ReplaceAllStringFunc(text, func(match string) string {
		if standardHome(match) {
			return strings.TrimPrefix(match, "~/")
		}
		return match
	})
}
