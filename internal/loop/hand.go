package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
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

// handStays refuses a revision of a hand landing proposal that would make it a landing of the loop's: while the package's
// request is a hand landing one (its paths recorded and its request open), every check and proposal of it keeps hand
// paths, so the record, the label and the loop's watch of the request stay true.
func handStays(p Package, hand []string) error {
	if len(p.Hand) > 0 && p.PR != "" && len(hand) == 0 {
		return refused("case %s's proposal %s is a hand landing: a revision of it stays one; close it to propose a change the loop lands", p.Case, p.PR)
	}
	return nil
}

func handNoteID(caseID string) string { return "loop-hand:" + store.IDPart(caseID) }

// handProposed records a hand landing proposal: a line in the case's log, which case list and the report read, and a
// low problem note, which pages nobody. Both are written once per pull request and episode.
func (s *Ship) handProposed(ctx context.Context, c store.Case, p Package) error {
	log, err := s.Store.CaseLog(ctx, c.ID)
	if err != nil {
		return err
	}
	missing := store.HandOf(c, log) != p.PR
	if missing {
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
		if !missing && n.ID == handNoteID(c.ID) && n.Detail == detail {
			return nil // the same proposal stays as its reader left it, resolved or not; a record written again raises it again
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
	return s.doneHandReadNotes(ctx, caseID)
}

// doneHandReadNotes resolves the notes of the case's hand landing reads that failed: a read that went through, or the
// record's end, is past them.
func (s *Ship) doneHandReadNotes(ctx context.Context, caseID string) error {
	notes, err := s.Store.Notes(ctx)
	if err != nil {
		return err
	}
	for _, n := range notes {
		if n.Done.IsZero() && strings.HasPrefix(n.ID, handNoteID(caseID)+":read:") {
			if _, _, err := s.Store.DoneNote(ctx, n.ID, s.now()); err != nil && !errors.Is(err, store.ErrNoNote) {
				return err
			}
		}
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
	if c.Into != "" {
		return p, nil
	}
	if p.State == Withdrawn && p.PR != "" && c.Status != store.CaseClosed {
		return s.withdrawHand(ctx, c, p) // a withdrawal that stopped partway, whatever the case did since
	}
	if c.Status != store.CaseTesting {
		return p, nil
	}
	switch p.State {
	case Landed:
		return p, s.finishHand(ctx, p)
	case Withdrawn:
		return p, nil // withdrawn and recorded; a new build prepares a new package
	case Prepared, Checked, Proposed, Revising: // a revision's build puts it back to Prepared, its request still open
	default:
		return p, nil
	}
	if p.PR == "" {
		return p, nil
	}
	raw, err := s.gh(ctx, p.Host, "pr", "view", p.PR, "--repo", p.Repo, "--json", "state,mergedAt,mergeCommit,headRefOid,baseRefName")
	if err != nil {
		return p, err
	}
	var pr struct {
		State       string    `json:"state"`
		MergedAt    time.Time `json:"mergedAt"`
		Head        string    `json:"headRefOid"`
		Base        string    `json:"baseRefName"`
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
			if err := s.handProposed(ctx, c, p); err != nil {
				return p, err
			}
		}
		if pr.Base != "" && pr.Base != p.Base {
			// retargeted by a person: the loop waits for it to be merged or closed, and its merge counts once on p.Base
			if err := s.caseNoteOnce(ctx, caseID, "hand landing retargeted", fmt.Sprintf("%s targets %s, not %s: a person has it", p.PR, pr.Base, p.Base)); err != nil {
				return p, err
			}
			return p, refused("the hand landing's pull request %s targets %s, not %s", p.PR, pr.Base, p.Base)
		}
		ours := firstOf(p.PublishedHead, p.Head)
		if pr.Head != "" && ours != "" && pr.Head != p.PublishedHead && pr.Head != p.Head {
			// a person works the request now (a rebase, a push of their own): the loop neither revises nor reviews it,
			// and reads it until it is merged or closed
			if err := s.caseNoteOnce(ctx, caseID, "hand landing taken over", fmt.Sprintf("%s holds %s, not the loop's %s: a person has it", p.PR, pr.Head, ours)); err != nil {
				return p, err
			}
			return p, refused("the hand landing's pull request %s is a person's now (its head moved)", p.PR)
		}
		return p, nil
	case "MERGED":
		if pr.Base != "" && pr.Base != p.Base {
			// merged into another branch: it counts once its merge commit reaches p.Base; until then a person has it
			if err := s.caseNoteOnce(ctx, caseID, "hand landing retargeted", fmt.Sprintf("%s was merged into %s, not %s: the loop measures it once it is on %s", p.PR, pr.Base, p.Base, p.Base)); err != nil {
				return p, err
			}
		}
		if pr.MergeCommit == nil || !commitRe.MatchString(pr.MergeCommit.OID) {
			return p, refused("the merged pull request %s names no merge commit", p.PR)
		}
		if _, err := s.git(ctx, p.Host, "fetch", "--quiet", "origin", p.Base); err != nil {
			return p, err
		}
		if _, err := s.git(ctx, p.Host, "merge-base", "--is-ancestor", pr.MergeCommit.OID, "origin/"+p.Base); err != nil {
			return p, refused("the merge commit of %s is not on %s yet", p.PR, p.Base)
		}
		if pr.Head != "" && pr.Head != firstOf(p.Head, p.PublishedHead) {
			// a person may change a proposal before landing it: what is measured is what landed
			if err := s.caseNoteOnce(ctx, caseID, "landed head differs",
				fmt.Sprintf("%s landed head %s, not the checked %s; the landing at %s is measured", p.PR, pr.Head, firstOf(p.Head, p.PublishedHead), pr.MergeCommit.OID)); err != nil {
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
		if commit, at, landed, err := s.landedByContent(ctx, p, pr.Head); err != nil {
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
		return s.withdrawHand(ctx, c, p)
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
func (s *Ship) withdrawHand(ctx context.Context, c store.Case, p Package) (Package, error) {
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
	// The case left testing since this request was first proposed (the reopen went in and it came back, a person
	// moved or closed it while the request was open, or a new episode began): what it holds now is newer than the
	// closed request, so it is not reopened again.
	first, episode := -1, int64(0)
	for i, e := range log {
		if pr0, ep, ok := store.HandRecordOf(e); ok && pr0 == pr {
			first, episode = i, ep
			break
		}
	}
	for i, e := range log { // a proposal whose record was never written still has its proposal line
		if first < 0 && e.Action == "proposed" && strings.HasPrefix(e.Detail, pr+" ") {
			first, episode = i, c.Episode
		}
	}
	left := c.Status != store.CaseTesting || first >= 0 && c.Episode > episode
	for i := first + 1; first >= 0 && i < len(log); i++ {
		left = left || log[i].Action == "set" && strings.Contains(log[i].Detail, "status testing -> ") || log[i].Action == "closed"
	}
	if left {
		// nothing to reopen
	} else if p.ClosedProposals < MaxClosedProposals {
		// the line goes first, so a reopen that stopped before its status change is finished, not lost
		if err := s.caseNoteOnce(ctx, p.Case, "hand landing withdrawn",
			"Its hand landing proposal "+pr+" was closed without merging. The case reopens and is investigated again."); err != nil {
			return p, err
		}
		if _, err := s.Store.UpdateCase(ctx, p.Case, store.CaseUpdate{Status: store.CaseOpen}, s.Actor); err != nil {
			return p, err
		}
	} else if err := s.caseNoteOnce(ctx, p.Case, "proposal closed",
		"Its hand landing proposal "+pr+" was closed without merging, its second closed proposal. The loop builds this case no more."); err != nil {
		return p, err
	}
	p.PR, p.Withdrew = "", pr
	return p, s.saveAuthorized(ctx, &p)
}

// caseNoteOnce writes a case log line unless the log holds the same one.
func (s *Ship) caseNoteOnce(ctx context.Context, id, action, detail string) error {
	clean, err := Redact(ctx, detail) // details quote what builds and people wrote
	if err != nil {
		return err
	}
	detail = store.CaseNoteText(clean)
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
		case errors.As(err, &wait) && s.now().Sub(at) < p.Plan.Window:
			return &shipHold{err: fmt.Errorf("the hand landing's plan is counted again at its landing, which waits: %w", err)}
		case errors.As(err, &wait):
			// a window after the landing the count still waits (an incomplete run inside its span never clears, and a
			// stopped reader may not run again): the change lands, to be measured by hand
			p.Plan.Units = ""
			if err := s.caseNoteOnce(ctx, c.ID, "measured by hand", "landed by hand more than a window after its plan, and the count at its landing still waits a window later: "+err.Error()); err != nil {
				return err
			}
		case err != nil && !errors.Is(err, ErrRefused):
			return err // a store that could not be read decides nothing: the landing is read again next pass
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
func (s *Ship) landedByContent(ctx context.Context, p Package, requestHead string) (string, time.Time, bool, error) {
	if firstOf(p.Head, p.PublishedHead) == "" || len(p.Changed) == 0 {
		return "", time.Time{}, false, nil
	}
	if _, err := s.git(ctx, p.Host, "fetch", "--quiet", "origin", p.Base); err != nil {
		return "", time.Time{}, false, err
	}
	heads := []string{firstOf(p.Head, p.PublishedHead)}
	if n := prNumber(p.PR); commitRe.MatchString(requestHead) && requestHead != heads[0] && n != "" {
		// a person's own version of the request, which they may have pushed instead
		if _, err := s.git(ctx, p.Host, "fetch", "--quiet", "origin", "refs/pull/"+n+"/head"); err == nil {
			heads = append(heads, requestHead)
		}
	}
	for _, head := range heads {
		commit, at, landed, err := s.holdsFiles(ctx, p, head)
		if err != nil || landed {
			return commit, at, landed, err
		}
	}
	return "", time.Time{}, false, nil
}

// prNumber is a pull request URL's number, or "".
func prNumber(url string) string {
	i := strings.LastIndex(url, "/pull/")
	if i < 0 {
		return ""
	}
	n := url[i+len("/pull/"):]
	for _, r := range n {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return n
}

// holdsFiles says whether the fetched default branch holds every file the change touches as head has it, and if so
// the newest commit of the branch that touched them and its time.
func (s *Ship) holdsFiles(ctx context.Context, p Package, head string) (string, time.Time, bool, error) {
	base := "origin/" + p.Base
	blob := func(rev, path string) (string, bool, error) {
		out, err := s.git(ctx, p.Host, "rev-parse", "--verify", "--quiet", rev+":"+path)
		var exit *exec.ExitError
		switch {
		case err == nil:
			return strings.TrimSpace(out), true, nil
		case errors.As(err, &exit) && exit.ExitCode() == 1:
			return "", false, nil // no such path there
		}
		return "", false, err // a read that failed decides nothing: a close is never withdrawn on it
	}
	for _, path := range p.Changed {
		want, wok, err := blob(head, path)
		if err != nil {
			return "", time.Time{}, false, err
		}
		got, gok, err := blob(base, path)
		if err != nil {
			return "", time.Time{}, false, err
		}
		if wok != gok || want != got {
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
