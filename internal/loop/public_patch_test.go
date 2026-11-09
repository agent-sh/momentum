package loop

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A harness's standard directory or file names nothing of its owner: every install of the harness has it. A patch line
// that names one, as it is, passes the public check, while anything below it and any other home path is still refused
// there as the build's own cause, with what a fixture names instead, and evidence still masks them all.
func TestPublicPatchAdmitsStandardHomePaths(t *testing.T) {
	ctx := context.Background()
	lines := []string{
		`var defaultMCP = []string{"~/.claude.json", "~/.codex/config.toml", "~/.hermes/config.yaml"}`,
		"Claude Code's `~/.claude.json`, at its top and in each of its projects (~/.claude.json).",
		`"names": {"hermes_jobs": "~/.hermes/cron/jobs.json", "hermes_scripts": "~/.hermes/scripts", "links": ["~/.local/bin"]}`,
		"its settings are in ~/.config/momentum/sources.json, its cases in ~/.local/share/momentum/.",
		"~/, $HOME/ and ${HOME}/ become the home directory",
	}
	for _, entry := range standardHomePaths {
		lines = append(lines, "read "+entry, "read `"+entry+"/`")
	}
	for _, line := range lines {
		if err := patchText(ctx, line, "added lines"); err != nil {
			t.Errorf("a standard path was refused: %q: %v", line, err)
		}
	}
	for _, line := range []string{
		"~/projects/private/x", "~/.ssh/id_rsa", "/home/someone/x", "~/.config/agents/rules.md",
		"~/.local/bin/x", "~/.claude/hooks/x.sh", "~/.claude/mods/x", "~/.claude/mcp-servers/web/server.mjs",
		"~/.claude.json.bak", "~/.claude/../projects/x", "~/.codex/*.toml", "~/.hermes/scripts/x.py",
		"~/.claude/projects/-home-someone-work/s.jsonl", "~/.claude/build-box-7.internal.example",
	} {
		err := patchText(ctx, line, "added lines")
		if !errors.Is(err, ErrRefused) || stageCause("build propose", err) != err.Error() ||
			!strings.Contains(err.Error(), "a test fixture names a path under its temporary directory, not the home") {
			t.Errorf("a personal home path was not refused as the build's own, with what to do: %q: %v", line, err)
		}
	}
	for _, path := range []string{"~/.claude.json", "~/.codex/config.toml", "~/.hermes/config.yaml"} {
		if got := maskPersonal("read " + path); got != "read [personal]" {
			t.Errorf("evidence kept a standard path: %q", got)
		}
	}
}
