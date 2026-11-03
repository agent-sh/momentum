package loop

import (
	"context"
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
	if maskPersonal(text) != text {
		return refused("patch %s contain personal content (a home path, address, IP or domain-shaped host)", part)
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
