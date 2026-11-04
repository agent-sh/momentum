package loop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"

	"github.com/agent-sh/momentum/internal/store"
)

// heldBuild says why the configuration holds a case's build, or "" when it does not. It runs no model and writes
// nothing: builds are off while loop.owners is empty, the repository the investigation named must be a
// loop.read_dirs checkout whose origin the loop may push to, and it must pass the gate's policy (owner, exclusion,
// scope check configured). A checkout whose repository cannot be read (missing, not git, no origin) holds with
// that reason too. known is false only when the pass itself is stopping: the case takes no slot, and nothing is
// recorded. Busy work, scope verdicts and locks change from minute to minute; preparation still checks them,
// and everything here again.
func (r *Runner) heldBuild(ctx context.Context, repo string) (why string, known bool) {
	g := r.Ship.Gate
	if len(g.Owners) == 0 {
		return "builds are off (loop.owners is empty): report only", true
	}
	source := r.readDirSource(repo)
	if source == "" {
		return "the investigation's repository " + repo + " is not a loop.read_dirs repository: report only", true
	}
	resolve := g.Resolve
	if resolve == nil {
		resolve = RepoOf
	}
	full, err := resolve(ctx, source)
	if errors.Is(err, ErrRefused) { // an origin the loop may not push to stays so until someone changes it
		return err.Error(), true
	} else if err != nil {
		if ctx.Err() != nil {
			return "the pass is stopping: " + ctx.Err().Error(), false
		}
		return "the checkout of " + repo + " cannot be read: " + err.Error() + ": report only", true
	}
	if err := g.policy(full); err != nil {
		return err.Error(), true
	}
	return "", true
}

// readDirSource maps a repository name to its loop.read_dirs checkout, named as the snapshot names it.
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
		if snapshotName(source, counts) == name {
			return source
		}
	}
	return ""
}

const holdReasonMax = 1500

// repoNoteID is the note of a case waiting for a repository (waitingRepoNote): it goes with the hold.
func repoNoteID(caseID string) string { return "loop-repo:" + store.IDPart(caseID) }

func holdNotePrefix(caseID string) string { return "loop-build:" + store.IDPart(caseID) + ":held:" }

// markHold records a case's hold when it changes: a hold with its reason and the case's episode, or the hold's
// end. An unchanged hold writes nothing, so a held case adds no line pass after pass, and its report note stays
// as its reader left it. A new or changed hold puts its low problem note (a case waiting for a repository has its
// own, waitingRepoNote), standing again if it was done; a lifted hold resolves its notes.
func (r *Runner) markHold(ctx context.Context, c store.Case, why string, waiting bool) error {
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
	clean = plainClip(store.CaseNoteText(clean), holdReasonMax) // the held line's record fits a case log line
	if clean == current {
		return nil
	}
	// the notes come first: a held line written stands as the hold, so its note must already be there
	if err := r.doneHoldNotes(ctx, c.ID); err != nil { // a changed reason's old note goes with it
		return err
	}
	sum := sha256.Sum256([]byte(clean))
	n := store.Note{ID: holdNotePrefix(c.ID) + hex.EncodeToString(sum[:]), Kind: store.NoteProblem,
		Importance: store.ImportanceLow, At: r.now(), Title: "The loop's build for case " + c.ID + " is held", Detail: clean}
	if waiting {
		n = waitingRepoNote(c, r.now())
	}
	if _, err := r.Store.PutNote(ctx, n); err != nil {
		return err
	}
	return r.Store.CaseNote(cleanedNote(ctx), c.ID, store.ActionHeld, store.HoldDetail(clean, c.Episode), "loop")
}

// doneHoldNotes resolves the standing hold notes of a case.
func (r *Runner) doneHoldNotes(ctx context.Context, caseID string) error {
	notes, err := r.Store.Notes(ctx)
	if err != nil {
		return err
	}
	for _, n := range notes {
		if n.Done.IsZero() && (strings.HasPrefix(n.ID, holdNotePrefix(caseID)) || n.ID == repoNoteID(caseID) || policyPrepareNote(caseID, n)) {
			if _, _, err := r.Store.DoneNote(ctx, n.ID, r.now()); err != nil {
				return err
			}
		}
	}
	return nil
}

// policyPrepareNote is a note a build's preparation left for a refusal of the configuration's policy, before the
// runner held such builds itself: the hold's note takes its place.
func policyPrepareNote(caseID string, n store.Note) bool {
	if !strings.HasPrefix(n.ID, "loop-build:"+store.IDPart(caseID)+":prepare:") {
		return false
	}
	for _, cause := range []string{"(loop.owners)", "(loop.exclude)", "(loop.repo_guard)", "not an admitted read_dirs repository"} {
		if strings.Contains(n.Detail, cause) {
			return true
		}
	}
	return false
}

// releaseHolds ends the holds of cases that are no longer in testing (closed, merged, deployed or back to
// investigating): their hold ended with the step it held. A case not closed whose log still holds a hold gets the
// hold's end, so a return to testing in the same episode records its hold again; every such case's standing notes
// resolve.
func (r *Runner) releaseHolds(ctx context.Context, cs []store.Case) error {
	notes, err := r.Store.Notes(ctx)
	if err != nil {
		return err
	}
	standing := map[string]bool{}
	for _, n := range notes {
		if n.Done.IsZero() && (strings.HasPrefix(n.ID, "loop-repo:") || strings.HasPrefix(n.ID, "loop-build:") && strings.Contains(n.ID, ":held:")) {
			standing[n.ID] = true
		}
	}
	for _, c := range cs {
		if c.Status == store.CaseTesting {
			continue
		}
		if c.Status != store.CaseClosed {
			log, err := r.Store.CaseLog(ctx, c.ID)
			if err != nil {
				return err
			}
			as := c
			as.Status = store.CaseTesting
			if store.HoldOf(as, log) != "" {
				if err := r.Store.CaseNote(cleanedNote(ctx), c.ID, store.ActionUnheld, "the case left testing", "loop"); err != nil {
					return err
				}
			}
		}
		for id := range standing {
			if strings.HasPrefix(id, holdNotePrefix(c.ID)) || id == repoNoteID(c.ID) {
				if _, _, err := r.Store.DoneNote(ctx, id, r.now()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
