package loop

import (
	"context"
	"strings"
)

// publish leases against the last receipt we saved, never a mutable tracking ref.
// A lost response is adopted only when the remote holds the exact candidate.
func (s *Ship) publish(ctx context.Context, p *Package, candidate string) error {
	args := []string{"--no-verify"}
	if p.PublishedHead != "" {
		args = append(args, "--force-with-lease=refs/heads/"+p.Branch+":"+p.PublishedHead)
	}
	args = append(args, "origin", candidate+":refs/heads/"+p.Branch)
	_, pushErr := s.push(ctx, *p, p.Work, args...)
	if pushErr != nil {
		out, err := s.git(ctx, p.Work, "ls-remote", "--refs", "origin", "refs/heads/"+p.Branch)
		fields := strings.Fields(out)
		if err != nil || len(fields) != 2 || fields[0] != candidate {
			return pushErr
		}
	}
	p.PublishedHead = candidate
	return s.save(p)
}
