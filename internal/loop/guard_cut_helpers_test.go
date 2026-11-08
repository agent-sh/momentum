package loop

import (
	"context"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// landByHand records a hand landing at commit, landed at at, first seen on its base at reached.
func landByHand(ctx context.Context, s *Ship, c store.Case, p *Package, commit string, at, reached time.Time) error {
	return s.landHand(ctx, c, p, commit, at, reached)
}

// reachedOf is the instant a package keeps for when its change reached its base.
func reachedOf(p Package) time.Time { return p.ReachedAt }

// clearReached takes a package's instant away, as a build from before it was kept saves the package.
func clearReached(p *Package) { p.ReachedAt = time.Time{} }

// noteMax is the longest a case note is stored.
func noteMax() int { return store.CaseNoteMax }
