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

// Names and addresses set aside for documentation and tests (RFC 2606, 6761, 5737, 3849) and loopback name no one: a
// patch line with them passes the public check, while a real-shaped host or address, a home path or an email is still
// refused there, and evidence still masks them all.
func TestPublicPatchAdmitsReservedHostsAndAddresses(t *testing.T) {
	ctx := context.Background()
	// added lines of a detector test the check once refused
	fixture := strings.Join([]string{
		`		{"WebFetch", "connect ECONNREFUSED 192.0.2.10:443", ` + "`" + `{"url":"https://site-a.example/x","prompt":"p"}` + "`" + `, "host:webfetch:site-a.example"},`,
		`		{"WebFetch", "connect ECONNREFUSED 192.0.2.10:443", ` + "`" + `{"url":"https://Site-A.example/other","prompt":"p"}` + "`" + `, "host:webfetch:site-a.example"},`,
		`		{"WebFetch", "connect ECONNREFUSED 198.51.100.7:443", ` + "`" + `{"url":"https://site-b.example/","prompt":"p"}` + "`" + `, "host:webfetch:site-b.example"},`,
		`		{"WebFetch", "connect ETIMEDOUT 203.0.113.5:443", ` + "`" + `{"url":"https://site-c.example/y"}` + "`" + `, "host:webfetch:site-c.example"},`,
		`		{"WebFetch", "connect ECONNREFUSED 127.0.0.1:443", ` + "`" + `{"url":"https://site-d.example/"}` + "`" + `, "host:webfetch:site-d.example"},`,
		`		{"WebFetch", "connect ECONNREFUSED 192.0.2.10:443", ` + "`" + `{"url":"https://site-a.exa` + "`" + `, ""},`,
		`		{"mcp__web-tools__fetch", "connect ECONNREFUSED 127.0.0.1:8080", "", "mcp:web-tools:connect econnrefused N:N"},`,
		`	a, okA := mistakeKey("tool-error", "WebFetch", "WebFetch: connect ECONNREFUSED 192.0.2.10:443",`,
		`		mistakeCall(` + "`" + `{"input":{"url":"https://site-a.example/x","prompt":"p"}}` + "`" + `))`,
		`		mistakeCall(` + "`" + `{"input":"{\"url\":\"https://site-b.example/x\"}"}` + "`" + `))`,
		`	if !okA || !okB || a.String() != "host:webfetch:site-a.example" || b.String() != "host:webfetch:site-b.example" {`,
	}, "\n")
	lines := []string{
		fixture,
		"curl https://api.example.com/v1 and http://www.example.net/ and https://docs.example.org/x",
		"ssh build-box.test; ping host.invalid; open http://app.localhost:3000/ and http://localhost.",
		"dial [2001:db8::5]:443, [2001:DB8:1::7]:80, [::1]:8080 and 127.10.0.1",
		"dial 2001:db8::192.0.2.1 or ::ffff:192.0.2.1; resolve EXAMPLE.COM and a.b.test.",
		"listen: 192.0.2.1:8080, host:198.51.100.2 and [2001:db8::1]:443",
	}
	for _, line := range lines {
		if err := patchText(ctx, line, "added lines"); err != nil {
			t.Errorf("a reserved name or address was refused: %q: %v", line, err)
		}
	}
	for _, line := range []string{
		"https://build-box-7.corp-name.com/x", "https://evil-example.com/x", "https://example.com.evil.net/x",
		"https://site.example.co/x", "ssh build-box.local", "connect ECONNREFUSED 192.0.3.10:443",
		"connect ECONNREFUSED 198.51.101.7:443", "dial [2001:db9::5]:443", "mail someone@site.example",
		"read ~/projects/private/x next to https://site-a.example/x", "/root.test/x", "mail someone@192.0.2.1",
		// a reserved name or address next to a real one
		"dial 2001:db9::192.0.2.1", "dial 2001:db8::198.18.4.5", "dial ::1.18.4.5", "ssh a.corp.internal.192.0.2.1",
		"ssh 192.0.2.1.corp.com", "dial 2001:db9::1:192.0.2.1", "dial 2001:db9:1:2:3:4:192.0.2.1",
		"dial fe80::1:127.0.0.1", "dial [2001:db8::1%build-box-7]:443",
		"https://corp-name.site.example-corp.com/x",
	} {
		if err := patchText(ctx, line, "added lines"); !errors.Is(err, ErrRefused) {
			t.Errorf("personal content was not refused: %q: %v", line, err)
		}
	}
	for _, line := range []string{"https://site-a.example/x", "connect 192.0.2.10:443"} {
		if got := maskPersonal(line); !strings.Contains(got, "[personal]") {
			t.Errorf("evidence kept a reserved name or address: %q", got)
		}
	}
}
