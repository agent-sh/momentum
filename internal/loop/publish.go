package loop

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// publish leases against the last receipt we saved, never a mutable tracking ref.
// A lost response is adopted only when the remote holds the exact candidate.
// errPublicationUncertain marks a push that failed when the remote could not be read to tell whether it was accepted:
// the caller keeps the candidate it journaled and reconciles it before doing anything else.
var errPublicationUncertain = errors.New("publication uncertain")

func (s *Ship) publish(ctx context.Context, p *Package, candidate string) error {
	args := []string{"--no-verify", "--force-with-lease=refs/heads/" + p.Branch + ":" + p.PublishedHead}
	args = append(args, "origin", candidate+":refs/heads/"+p.Branch)
	_, pushErr := s.push(ctx, *p, p.Host, args...)
	if pushErr != nil {
		out, err := s.git(ctx, p.Host, "ls-remote", "--refs", "origin", "refs/heads/"+p.Branch)
		if err != nil {
			return fmt.Errorf("%w: the push failed (%v) and the remote could not be read (%v)", errPublicationUncertain, pushErr, err)
		}
		if fields := strings.Fields(out); len(fields) != 2 || fields[0] != candidate {
			return pushErr // the remote was read and does not hold the candidate: the push was not accepted
		}
	}
	p.PublishedHead = candidate
	return s.saveAuthorized(ctx, p)
}
