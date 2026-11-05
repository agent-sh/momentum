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
	notes, err := s.Store.Notes(ctx)
	if err != nil {
		return err
	}
	for _, n := range notes {
		if n.ID == handNoteID(c.ID) && n.Detail == detail {
			return nil // the same proposal stays as its reader left it, resolved or not
		}
	}
	_, err = s.Store.PutNote(ctx, store.Note{ID: handNoteID(c.ID), Kind: store.NoteProblem, Importance: store.ImportanceLow,
		Source: "momentum", At: s.now(), Title: plainClip("The loop's change for case "+c.ID+" is "+store.HandStage+": "+c.Title, store.TitleMax),
		Detail: detail, Links: prLinks(p.PR)})
	return err
}

// handEnded closes a hand landing's record, once: its log line while the case still shows the stage, and its note.
func (s *Ship) handEnded(ctx context.Context, caseID, why string) error {
	c, err := s.Store.CaseByID(ctx, caseID)
	if err != nil {
		return err
	}
	log, err := s.Store.CaseLog(ctx, caseID)
	if err != nil {
		return err
	}
	if store.HandOf(c, log) != "" {
		if err := s.Store.CaseNote(cleanedNote(ctx), caseID, store.ActionHandEnded, why, s.Actor); err != nil {
			return err
		}
	}
	if _, _, err := s.Store.DoneNote(ctx, handNoteID(caseID), s.now()); err != nil && !errors.Is(err, store.ErrNoNote) {
		return err
	}
	return nil
}

// reconcileHand reads a hand landing proposal's pull request. Open, nothing changes (a proposal whose record was not
// written writes it). Merged, with its merge commit on the fetched default branch, the package is landed at that commit
// and the case deployed: the measure step reads the change from the default branch from then on, as it reads a landing
// of its own. Closed without merging, the package is withdrawn and counts as a closed proposal; the case reopens for a
// new investigation, and the next build prepares a new package, unless this was the package's second closed proposal:
// then the case waits for a person, as any package does after two (proposalsUsed). Nothing is pushed. A landing or a
// withdrawal that stopped partway is finished at the next call.
func (s *Ship) reconcileHand(ctx context.Context, caseID string) (Package, error) {
	ctx = s.gitContext(shipContext(ctx, caseID))
	unlock, err := s.lock(ctx, caseID)
	if err != nil {
		return Package{}, err
	}
	defer unlock()
	p, ok, err := s.Load(caseID)
	if err != nil || !ok || len(p.Hand) == 0 {
		return p, err
	}
	c, err := s.Store.CaseByID(ctx, caseID)
	if err != nil {
		return p, err
	}
	if c.Status != store.CaseTesting || c.Into != "" {
		return p, nil
	}
	switch p.State {
	case Landed:
		return p, s.finishHand(ctx, p)
	case Withdrawn:
		if p.PR == "" {
			return p, nil // withdrawn and recorded; a new build prepares a new package
		}
		return s.withdrawHand(ctx, p)
	case Proposed, Revising, Checked:
	default:
		return p, nil
	}
	if p.PR == "" {
		return p, nil
	}
	raw, err := s.gh(ctx, p.Host, "pr", "view", p.PR, "--repo", p.Repo, "--json", "state,mergedAt,mergeCommit,headRefOid")
	if err != nil {
		return p, err
	}
	var pr struct {
		State       string    `json:"state"`
		MergedAt    time.Time `json:"mergedAt"`
		Head        string    `json:"headRefOid"`
		MergeCommit *struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
	}
	if err := json.Unmarshal([]byte(raw), &pr); err != nil {
		return p, fmt.Errorf("read the hand landing's pull request: %w", err)
	}
	switch pr.State {
	case "OPEN":
		log, err := s.Store.CaseLog(ctx, caseID)
		if err != nil {
			return p, err
		}
		if store.HandOf(c, log) != p.PR {
			return p, s.handProposed(ctx, c, p)
		}
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
		if pr.Head != "" && pr.Head != p.Head {
			// a person may change a proposal before landing it: what is measured is what landed
			if err := s.caseNoteOnce(ctx, caseID, "landed head differs",
				fmt.Sprintf("%s landed head %s, not the checked %s; the landing at %s is measured", p.PR, pr.Head, p.Head, pr.MergeCommit.OID)); err != nil {
				return p, err
			}
		}
		at := pr.MergedAt
		if at.IsZero() {
			at = s.now()
		}
		return p, s.landHand(ctx, c, &p, pr.MergeCommit.OID, at)
	case "CLOSED":
		// a person who lands a rebased head by a push to the default branch and then closes the request landed it all
		// the same: the change's files on the default branch as the proposal has them say so
		if commit, at, landed, err := s.landedByContent(ctx, p); err != nil {
			return p, err
		} else if landed {
			if err := s.caseNoteOnce(ctx, caseID, "landed by content", fmt.Sprintf("%s was closed, and %s holds its change's files as proposed at %s", p.PR, p.Base, commit)); err != nil {
				return p, err
			}
			return p, s.landHand(ctx, c, &p, commit, at)
		}
		p.State = Withdrawn
		p.ClosedProposals++
		if err := s.saveAuthorized(ctx, &p); err != nil {
			return p, err
		}
		return s.withdrawHand(ctx, p)
	}
	return p, refused("the hand landing's pull request %s is %q", p.PR, pr.State)
}

// finishHand deploys a package landed by hand whose case is still in testing; each step is safe to repeat.
func (s *Ship) finishHand(ctx context.Context, p Package) error {
	if err := s.handEnded(ctx, p.Case, "landed by hand: "+p.PR+" at "+lastOf(p.Landed)); err != nil {
		return err
	}
	if err := s.deploymentReceipt(ctx, p); err != nil {
		return err
	}
	_, err := s.Store.DeployCase(ctx, p.Case, s.pkgDir(p.Case), p.LandedAt, p.PR+": "+p.Title, s.Actor)
	return err
}

// withdrawHand records a closed hand landing proposal: its record ends and the case reopens, or, after the package's
// second closed proposal, stays in testing for a person. Its pull request is cleared last, so a withdrawal that stopped
// partway is finished at the next call; the case log's ended line journals it, so a case that already left testing
// after it is not reopened again.
func (s *Ship) withdrawHand(ctx context.Context, p Package) (Package, error) {
	pr := p.PR
	ended := "closed without merging: " + pr
	if err := s.caseNoteOnce(ctx, p.Case, store.ActionHandEnded, ended); err != nil {
		return p, err
	}
	if err := s.handEnded(ctx, p.Case, ended); err != nil {
		return p, err
	}
	log, err := s.Store.CaseLog(ctx, p.Case)
	if err != nil {
		return p, err
	}
	reopened := false
	for i := len(log) - 1; i >= 0 && !(log[i].Action == store.ActionHandEnded && log[i].Detail == store.CaseNoteText(ended)); i-- {
		reopened = reopened || log[i].Action == "set" && strings.Contains(log[i].Detail, "status testing -> ")
	}
	if reopened {
		// it left testing after the close: the reopen went in, and the case came back since
	} else if p.ClosedProposals < MaxClosedProposals {
		if _, err := s.Store.UpdateCase(ctx, p.Case, store.CaseUpdate{Status: store.CaseOpen}, s.Actor); err != nil {
			return p, err
		}
		if err := s.Store.CaseNote(cleanedNote(ctx), p.Case, "reopened",
			"Its hand landing proposal "+pr+" was closed without merging. The case is investigated again.", s.Actor); err != nil {
			return p, err
		}
	} else if err := s.caseNoteOnce(ctx, p.Case, "proposal closed",
		"Its hand landing proposal "+pr+" was closed without merging, its second closed proposal. The loop builds this case no more."); err != nil {
		return p, err
	}
	p.PR = ""
	return p, s.saveAuthorized(ctx, &p)
}

// caseNoteOnce writes a case log line unless the log holds the same one.
func (s *Ship) caseNoteOnce(ctx context.Context, id, action, detail string) error {
	detail = store.CaseNoteText(detail)
	log, err := s.Store.CaseLog(ctx, id)
	if err != nil {
		return err
	}
	for _, entry := range log {
		if entry.Action == action && entry.Detail == detail {
			return nil
		}
	}
	return s.Store.CaseNote(cleanedNote(ctx), id, action, detail, s.Actor)
}

// landHand records a hand landing at commit, a commit of the default branch, landed at at. A plan counted more than a
// window before the landing no longer stands for the windows before the change: it is counted again as of the
// landing. A count that waits on a reader holds the landing for the next pass; one refused leaves the change to be
// measured by hand (a plan without units), with a case log line that says why.
func (s *Ship) landHand(ctx context.Context, c store.Case, p *Package, commit string, at time.Time) error {
	if p.Plan != nil && at.Sub(p.Plan.At) > p.Plan.Window {
		plan, err := s.validPlan(ctx, c, p.Plan.Signal, p.Plan.Window, p.Plan.Installed, at)
		var wait *waitingPlan
		switch {
		case errors.As(err, &wait):
			return &shipHold{err: fmt.Errorf("the hand landing's plan is counted again at its landing, which waits: %w", err)}
		case err != nil:
			p.Plan.Units = ""
			if err := s.caseNoteOnce(ctx, c.ID, "measured by hand", "landed by hand more than a window after its plan, and the count at its landing was refused: "+err.Error()); err != nil {
				return err
			}
		default:
			p.Plan = &plan
			if err := s.caseNoteOnce(ctx, c.ID, "plan counted again", fmt.Sprintf("landed by hand more than a window after its plan: counted again at %s, %d of %d in the baseline, gate %d of %d",
				at.UTC().Format(time.RFC3339), plan.Before, plan.BeforeBase, plan.Gate, plan.GateBase)); err != nil {
				return err
			}
		}
	}
	p.State, p.LandingHead, p.Landed, p.Cleanup, p.LandedAt = Landed, commit, []string{commit}, "", at
	if err := s.saveAuthorized(ctx, p); err != nil {
		return err
	}
	return s.finishHand(ctx, *p)
}

// landedByContent says whether the fetched default branch holds every file the change touches as the proposal's head
// has it (a deleted file deleted), and if so the newest commit of the branch that touched them and its time.
func (s *Ship) landedByContent(ctx context.Context, p Package) (string, time.Time, bool, error) {
	if p.Head == "" || len(p.Changed) == 0 {
		return "", time.Time{}, false, nil
	}
	if _, err := s.git(ctx, p.Host, "fetch", "--quiet", "origin", p.Base); err != nil {
		return "", time.Time{}, false, err
	}
	base := "origin/" + p.Base
	for _, path := range p.Changed {
		want, werr := s.git(ctx, p.Host, "rev-parse", "--verify", "--quiet", p.Head+":"+path)
		got, gerr := s.git(ctx, p.Host, "rev-parse", "--verify", "--quiet", base+":"+path)
		if (werr == nil) != (gerr == nil) || strings.TrimSpace(want) != strings.TrimSpace(got) {
			return "", time.Time{}, false, nil
		}
	}
	out, err := s.git(ctx, p.Host, append([]string{"log", "-1", "--format=%H %cI", base, "--"}, p.Changed...)...)
	if err != nil {
		return "", time.Time{}, false, err
	}
	fields := strings.Fields(out)
	if len(fields) != 2 || !commitRe.MatchString(fields[0]) {
		return "", time.Time{}, false, nil
	}
	at, err := time.Parse(time.RFC3339, fields[1])
	if err != nil {
		return "", time.Time{}, false, err
	}
	return fields[0], at, true, nil
}
