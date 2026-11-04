package loop

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/agent-sh/momentum/internal/store"
)

// heldBuild says why the configuration holds a case's build, or "" when only the build's own preparation can
// tell. It runs no model and writes nothing: builds are off while loop.owners is empty, and the repository the
// investigation named must pass the gate's policy (owner, exclusion, scope check configured). Busy work, scope
// verdicts and locks change from minute to minute; preparation still checks them, and everything here again.
func (r *Runner) heldBuild(ctx context.Context, repo string) string {
	g := r.Ship.Gate
	if len(g.Owners) == 0 {
		return "builds are off (loop.owners is empty): report only"
	}
	source := r.readDirSource(repo)
	if source == "" {
		return "" // not an admitted read_dirs repository: preparation refuses it with its own reason
	}
	resolve := g.Resolve
	if resolve == nil {
		resolve = RepoOf
	}
	full, err := resolve(ctx, source)
	if err != nil {
		return ""
	}
	if err := g.policy(full); err != nil {
		return err.Error()
	}
	return ""
}

// readDirSource maps a repository name to its loop.read_dirs checkout, named as the snapshot names it (the base
// name, with a short hash of the path when two share it).
func (r *Runner) readDirSource(name string) string {
	var sources []string
	seen, counts := map[string]bool{}, map[string]int{}
	for _, dir := range r.Inputs.ReadDirs {
		p, err := filepath.Abs(dir)
		if err != nil || seen[p] {
			continue
		}
		seen[p] = true
		sources = append(sources, p)
		counts[filepath.Base(p)]++
	}
	for _, source := range sources {
		n := filepath.Base(source)
		if counts[n] > 1 {
			hash := sha256.Sum256([]byte(source))
			n += fmt.Sprintf("-%x", hash[:4])
		}
		if n == name {
			return source
		}
	}
	return ""
}

// markHold records a case's hold in its log when it changes: a hold with its reason, or the hold's end. An
// unchanged hold writes nothing, so a held case adds no line pass after pass.
func (r *Runner) markHold(ctx context.Context, c store.Case, why string) error {
	log, err := r.Store.CaseLog(ctx, c.ID)
	if err != nil {
		return err
	}
	current := store.HoldOf(c, log)
	if why == "" {
		if current == "" {
			return nil
		}
		// the report's problem notes of the hold go with it
		notes, err := r.Store.Notes(ctx)
		if err != nil {
			return err
		}
		prefix := "loop-build:" + store.IDPart(c.ID) + ":held:"
		for _, n := range notes {
			if strings.HasPrefix(n.ID, prefix) && n.Done.IsZero() {
				if _, _, err := r.Store.DoneNote(ctx, n.ID, r.now()); err != nil {
					return err
				}
			}
		}
		return r.Store.CaseNote(cleanedNote(ctx), c.ID, store.ActionUnheld, "the configuration no longer holds its build", "loop")
	}
	clean, err := Redact(ctx, why)
	if err != nil {
		return err
	}
	clean = store.CaseNoteText(clean)
	if clean == current {
		return nil
	}
	return r.Store.CaseNote(cleanedNote(ctx), c.ID, store.ActionHeld, clean, "loop")
}
