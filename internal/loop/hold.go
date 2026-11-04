package loop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/agent-sh/momentum/internal/store"
)

// heldBuild says why the configuration holds a case's build, or "" when only the build's own preparation can
// tell. It runs no model and writes nothing: builds are off while loop.owners is empty, the repository the
// investigation named must be a loop.read_dirs checkout, and it must pass the gate's policy (owner, exclusion,
// scope check configured). Busy work, scope verdicts and locks change from minute to minute; preparation still
// checks them, and everything here again.
func (r *Runner) heldBuild(ctx context.Context, repo string) string {
	g := r.Ship.Gate
	if len(g.Owners) == 0 {
		return "builds are off (loop.owners is empty): report only"
	}
	source := r.readDirSource(repo)
	if source == "" {
		return "the investigation's repository " + repo + " is not a loop.read_dirs repository: report only"
	}
	resolve := g.Resolve
	if resolve == nil {
		resolve = RepoOf
	}
	full, err := resolve(ctx, source)
	if err != nil {
		return "" // preparation names what is wrong with the checkout
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

func holdNotePrefix(caseID string) string { return "loop-build:" + store.IDPart(caseID) + ":held:" }

// markHold records a case's hold when it changes: a hold with its reason and the case's episode, or the hold's
// end. An unchanged hold writes nothing, so a held case adds no line pass after pass, and its report note stays
// as its reader left it. A new or changed hold puts its low problem note when note is set (a case waiting for a
// repository has its own), standing again if it was done; a lifted hold resolves its notes.
func (r *Runner) markHold(ctx context.Context, c store.Case, why string, note bool) error {
	log, err := r.Store.CaseLog(ctx, c.ID)
	if err != nil {
		return err
	}
	current := store.HoldOf(c, log)
	if why == "" {
		if current == "" {
			return nil
		}
		if err := r.doneHoldNotes(ctx, c.ID); err != nil {
			return err
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
	if err := r.Store.CaseNote(cleanedNote(ctx), c.ID, store.ActionHeld, store.HoldDetail(clean, c.Episode), "loop"); err != nil {
		return err
	}
	if !note {
		return nil
	}
	sum := sha256.Sum256([]byte(clean))
	_, err = r.Store.PutNote(ctx, store.Note{ID: holdNotePrefix(c.ID) + hex.EncodeToString(sum[:]), Kind: store.NoteProblem,
		Importance: store.ImportanceLow, At: r.now(), Title: "The loop's build for case " + c.ID + " is held", Detail: clean})
	return err
}

// doneHoldNotes resolves the standing hold notes of a case.
func (r *Runner) doneHoldNotes(ctx context.Context, caseID string) error {
	notes, err := r.Store.Notes(ctx)
	if err != nil {
		return err
	}
	for _, n := range notes {
		if strings.HasPrefix(n.ID, holdNotePrefix(caseID)) && n.Done.IsZero() {
			if _, _, err := r.Store.DoneNote(ctx, n.ID, r.now()); err != nil {
				return err
			}
		}
	}
	return nil
}

// releaseHolds resolves the standing hold notes of cases that are no longer in testing (closed, merged, deployed
// or back to investigating): their hold ended with the step it held.
func (r *Runner) releaseHolds(ctx context.Context, cs []store.Case) error {
	notes, err := r.Store.Notes(ctx)
	if err != nil {
		return err
	}
	for _, c := range cs {
		if c.Status == store.CaseTesting {
			continue
		}
		for _, n := range notes {
			if strings.HasPrefix(n.ID, holdNotePrefix(c.ID)) && n.Done.IsZero() {
				if _, _, err := r.Store.DoneNote(ctx, n.ID, r.now()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
