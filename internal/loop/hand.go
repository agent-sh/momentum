package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// A change whose right fix needs paths the loop may not land itself (PathScope) is still built, checked and reviewed
// under the same gates, and proposed for a person to land by hand. Its pull request carries HandLabel and HandLine
// first in its body; the loop reviews it and never lands it. A session lands it by the normal pull request rules, or
// closes it. Once it is on the default branch the loop records the landing and measures it as it measures its own;
// closed without merging, the case reopens.

// HandLabel marks a loop proposal for a hand landing on its pull request.
const HandLabel = "momentum-hand-landing"

// HandLine is the first line of a hand landing proposal's body.
const HandLine = "Loop proposal for a hand landing: momentum checked this change and reviews it, but never lands it. Land it by the normal pull request rules, or close it."

var commitRe = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)

func addLabels(labels []string) []string {
	var out []string
	for i := 0; i+1 < len(labels); i += 2 {
		out = append(out, "--add-label", labels[i+1])
	}
	return out
}

func handNoteID(caseID string) string { return "loop-hand:" + store.IDPart(caseID) }

// handProposed records a hand landing proposal: a line in the case's log, which case list and the report read, and a
// low problem note, which pages nobody. Both are written once per pull request and episode.
func (s *Ship) handProposed(ctx context.Context, c store.Case, p Package) error {
	log, err := s.Store.CaseLog(ctx, c.ID)
	if err != nil {
		return err
	}
	if store.HandOf(c, log) != p.PR {
		if err := s.Store.CaseNote(cleanedNote(ctx), c.ID, store.ActionHandProposed, store.HandDetail(p.PR, c.Episode), s.Actor); err != nil {
			return err
		}
	}
	detail := fmt.Sprintf("Its fix changes paths the loop may not land itself: %s. The loop checks and reviews it and never lands it. "+
		"A session lands %s by the normal pull request rules, or closes it. Its measurement starts once it is on the default branch; "+
		"closed without merging, the case reopens.", strings.Join(p.Hand, ", "), p.PR)
	_, err = s.Store.PutNote(ctx, store.Note{ID: handNoteID(c.ID), Kind: store.NoteProblem, Importance: store.ImportanceLow,
		Source: "momentum", At: s.now(), Title: plainClip("The loop's change for case "+c.ID+" is "+store.HandStage+": "+c.Title, store.TitleMax),
		Detail: detail, Links: prLinks(p.PR)})
	return err
}

// handEnded closes a hand landing's record: its log line and its note.
func (s *Ship) handEnded(ctx context.Context, caseID, why string) error {
	if err := s.Store.CaseNote(cleanedNote(ctx), caseID, store.ActionHandEnded, why, s.Actor); err != nil {
		return err
	}
	if _, _, err := s.Store.DoneNote(ctx, handNoteID(caseID), s.now()); err != nil && !errors.Is(err, store.ErrNoNote) {
		return err
	}
	return nil
}

// reconcileHand reads a hand landing proposal's pull request. Open, nothing changes. Merged, with its merge commit on
// the fetched default branch, the package is landed at that commit and the case deployed: the measure step reads the
// change from the default branch from then on, as it reads a landing of its own. Closed without merging, the package is
// withdrawn (archived; a new build prepares a new one) and the case reopens for a new investigation. Nothing is pushed.
func (s *Ship) reconcileHand(ctx context.Context, caseID string) (Package, error) {
	ctx = s.gitContext(shipContext(ctx, caseID))
	unlock, err := s.lock(ctx, caseID)
	if err != nil {
		return Package{}, err
	}
	defer unlock()
	p, ok, err := s.Load(caseID)
	if err != nil || !ok {
		return p, err
	}
	if len(p.Hand) == 0 || p.PR == "" || (p.State != Proposed && p.State != Revising && p.State != Checked) {
		return p, nil
	}
	c, err := s.Store.CaseByID(ctx, caseID)
	if err != nil {
		return p, err
	}
	if c.Status != store.CaseTesting || c.Into != "" {
		return p, nil
	}
	raw, err := s.gh(ctx, p.Host, "pr", "view", p.PR, "--repo", p.Repo, "--json", "state,mergedAt,mergeCommit")
	if err != nil {
		return p, err
	}
	var pr struct {
		State       string    `json:"state"`
		MergedAt    time.Time `json:"mergedAt"`
		MergeCommit *struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
	}
	if err := json.Unmarshal([]byte(raw), &pr); err != nil {
		return p, fmt.Errorf("read the hand landing's pull request: %w", err)
	}
	switch pr.State {
	case "OPEN":
		return p, nil
	case "MERGED":
		if pr.MergeCommit == nil || !commitRe.MatchString(pr.MergeCommit.OID) {
			return p, refused("the merged pull request %s names no merge commit", p.PR)
		}
		if _, err := s.git(ctx, p.Host, "fetch", "--quiet", "origin", p.Base); err != nil {
			return p, err
		}
		if _, err := s.git(ctx, p.Host, "merge-base", "--is-ancestor", pr.MergeCommit.OID, "origin/"+p.Base); err != nil {
			return p, refused("the merge commit of %s is not on %s yet", p.PR, p.Base)
		}
		p.State, p.LandingHead, p.Landed, p.Cleanup = Landed, pr.MergeCommit.OID, []string{pr.MergeCommit.OID}, ""
		p.LandedAt = pr.MergedAt
		if p.LandedAt.IsZero() {
			p.LandedAt = s.now()
		}
		if err := s.saveAuthorized(ctx, &p); err != nil {
			return p, err
		}
		if err := s.handEnded(ctx, caseID, "landed by hand: "+p.PR+" at "+pr.MergeCommit.OID); err != nil {
			return p, err
		}
		if err := s.deploymentReceipt(ctx, p); err != nil {
			return p, err
		}
		_, err = s.Store.DeployCase(ctx, p.Case, s.pkgDir(p.Case), p.LandedAt, p.PR+": "+p.Title, s.Actor)
		return p, err
	case "CLOSED":
		p.State = Withdrawn
		if err := s.saveAuthorized(ctx, &p); err != nil {
			return p, err
		}
		if err := s.archive(p); err != nil {
			return p, err
		}
		if err := s.handEnded(ctx, caseID, "closed without merging: "+p.PR); err != nil {
			return p, err
		}
		_, err = s.Store.UpdateCase(ctx, caseID, store.CaseUpdate{Status: store.CaseOpen}, s.Actor)
		if err != nil {
			return p, err
		}
		return p, s.Store.CaseNote(ctx, caseID, "reopened",
			"Its hand landing proposal "+p.PR+" was closed without merging. The case is investigated again.", s.Actor)
	}
	return p, refused("the hand landing's pull request %s is %q", p.PR, pr.State)
}
