package loop

import (
	"context"
	"net/netip"
	"regexp"
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
	// Home paths and addresses are refused whatever host they name; the rest is checked with the reserved names and
	// addresses taken out.
	checked := withoutStandardHomes(text)
	public := withoutReserved(checked)
	if homePathRe.MatchString(checked) || addressRe.MatchString(checked) || maskPersonal(public) != public {
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

// reservedDomains are the names RFC 2606 and RFC 6761 set aside for documentation and tests, and localhost: no one
// owns one, so a patch that names one or a name below it says nothing of its owner. Only the public-patch check reads
// this list; evidence masks these names as any other host (maskPersonal).
var reservedDomains = []string{"example.com", "example.net", "example.org", "example", "test", "invalid", "localhost"}

// reservedPrefixes are the address ranges RFC 5737 (IPv4) and RFC 3849 (IPv6) set aside for documentation. Loopback
// addresses are admitted too (reservedAddress).
var reservedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// reservedHost says whether a host (a hostNameRe match) is a reserved name or a name below one.
func reservedHost(host string) bool {
	host = strings.ToLower(host)
	for _, domain := range reservedDomains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// reservedAddress says whether an address is a documentation or a loopback address.
func reservedAddress(address string) bool {
	ip, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	ip = ip.WithZone("").Unmap()
	if ip.IsLoopback() {
		return true
	}
	for _, prefix := range reservedPrefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// ipv6DottedRe is ipv6Re with a dotted tail and a zone, so a token that runs on into an IPv4 address is read whole.
var ipv6DottedRe = regexp.MustCompile(`(?i)[a-z0-9_]*:[a-z0-9_:]+(?:\.[a-z0-9_]+)*(?:%[a-z0-9_-]+)?`)

// withoutReserved is text with each reserved address and host (reservedAddress, reservedHost) that stands alone
// blanked to a space, so every other address and host stays whole for the personal masks: a blank joins nothing to
// its neighbours. An IPv4 address that ends an IPv6 one is read with it, never alone; an IPv6 token with a zone is
// left to the masks, since a zone names an interface.
func withoutReserved(text string) string {
	text = blankReserved(text, ipv4Re, true, func(match string) (int, bool) {
		return 0, reservedAddress(match)
	})
	text = blankReserved(text, ipv6DottedRe, false, func(token string) (int, bool) {
		keep, ip := 0, token
		if label, rest, ok := strings.Cut(ip, ":"); ok && label != "" && !hexLabel(label) {
			keep, ip = len(label)+1, rest
		}
		if strings.Contains(ip, "%") || !strings.Contains(ip, ":") {
			return keep, false
		}
		// an IPv4 tail is reserved itself, or the whole is a mapped or documentation address with a real one in it
		if tail := ip[strings.LastIndexByte(ip, ':')+1:]; strings.Contains(tail, ".") && !reservedAddress(tail) {
			return keep, false
		}
		return keep, reservedAddress(ip)
	})
	return blankReserved(text, hostNameRe, false, func(host string) (int, bool) {
		return 0, reservedHost(host)
	})
}

// blankReserved is text with each match of re that reserved admits, from the byte reserved says to keep onwards,
// blanked to a space when it stands alone (standsAlone).
func blankReserved(text string, re *regexp.Regexp, ipv4 bool, reserved func(match string) (keep int, ok bool)) string {
	var out strings.Builder
	end := 0
	for _, span := range re.FindAllStringIndex(text, -1) {
		keep, ok := reserved(text[span[0]:span[1]])
		start := span[0] + keep
		if !ok || !standsAlone(text, start, span[1], ipv4) {
			continue
		}
		out.WriteString(text[end:start])
		out.WriteByte(' ')
		end = span[1]
	}
	out.WriteString(text[end:])
	return out.String()
}

// standsAlone says whether text[start:end] is joined to no name around it: no dot, dash or underscore before it,
// and no dash, underscore or dotted label after it. An IPv4 address after a colon that follows a hex digit or a
// colon is an IPv6 address's tail, not alone.
func standsAlone(text string, start, end int, ipv4 bool) bool {
	if start > 0 && strings.IndexByte(".-_", text[start-1]) >= 0 {
		return false
	}
	if ipv4 && start > 0 && text[start-1] == ':' && (start == 1 || text[start-2] == ':' || hexLabel(text[start-2:start-1])) {
		return false
	}
	if end < len(text) && strings.IndexByte("-_", text[end]) >= 0 {
		return false
	}
	return end+1 >= len(text) || text[end] != '.' || !identifierByte(text[end+1])
}
