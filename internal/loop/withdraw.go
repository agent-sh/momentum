package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/agent-sh/momentum/internal/store"
)

// Withdraw ends a case's package that has not ended, at its owner's word (ship withdraw): a change the case no longer
// wants, such as one whose fix moved to another place (unfinishedHold). The loop closes no pull request, so a
// repository's package is withdrawn only while no request of it is open (requestEnded): a person closes it first,
// which counts as a closed proposal, and a hand landing's record ends with it. A configuration proposal is withdrawn
// as its case's close withdraws it, with its note resolved. A package landing, landed or rolling back ends with its
// measurement or its rollback instead. The withdrawn package stays until the next preparation archives it: the
// next pass prepares the fix where the investigation names it.
func (s *Ship) Withdraw(ctx context.Context, caseID, why string) (Package, error) {
	why = strings.TrimSpace(why)
	if why == "" {
		return Package{}, refused("a withdrawal says why")
	}
	ctx = s.gitContext(shipContext(ctx, caseID))
	unlock, err := s.lock(ctx, caseID)
	if err != nil {
		return Package{}, err
	}
	defer unlock()
	p, ok, err := s.Load(caseID)
	if err != nil {
		return p, err
	}
	if !ok {
		return p, refused("case %s has no package", caseID)
	}
	c, err := s.Store.CaseByID(ctx, caseID)
	if err != nil {
		return p, err
	}
	switch {
	case ended(p):
		return p, refused("case %s's package has ended already (%s)", c.ID, p.State)
	case p.State == Landing || p.State == Landed || p.State == RollingBack:
		return p, refused("case %s's package is %s: it ends with its landing and measurement, or its rollback (momentum ship rollback %s)", c.ID, p.State, c.ID)
	}
	if p.Kind == KindConfig {
		w, err := s.withdrawConfig(ctx, c, p, "withdrawn by its owner: "+why, false)
		if err != nil {
			return w, err
		}
		return w, s.configNote(ctx, c, &w)
	}
	closed, merged, err := s.requestEnded(ctx, c, p)
	if err != nil {
		return p, err
	}
	what := fmt.Sprintf("repository package %.12s for %s (%s) withdrawn by its owner: %s", firstOf(p.Head, p.BaseCommit), p.Repo, p.State, why)
	switch {
	case closed != "":
		what += "; its pull request " + closed + " was closed without merging"
		p.ClosedProposals++ // a closed proposal of the case's, as a close the landing reads counts one
	case merged != "":
		what += "; its pull request " + merged + " was merged by a person, outside the loop's landing: the loop does not measure a change it did not land"
	}
	// the lines first: a withdrawal that stops before its save is run again, and writes its lines once. A merged
	// case's lines would go to the case it went into, so it writes none.
	if c.Into == "" {
		if len(p.Hand) > 0 && closed != "" {
			// the hand landing's record ends, as a close of its request ends it (withdrawHand), and its notes resolve
			if err := s.handEnded(ctx, c.ID, "closed without merging: "+closed+"; withdrawn by its owner"); err != nil {
				return p, err
			}
		}
		if err := s.caseNoteOnce(ctx, c.ID, ActionWithdrawn, what); err != nil {
			return p, err
		}
	} else if err := s.doneHandNotes(ctx, c.ID); err != nil {
		return p, err
	}
	if err := s.wrote("owner withdraw: line"); err != nil {
		return p, err
	}
	if len(p.Hand) > 0 && closed != "" {
		p.Withdrew = closed
	}
	p.State, p.PR = Withdrawn, ""
	return p, s.save(&p)
}

// requestEnded reads a repository package's pull request before its owner's withdrawal: the one it names, or, when it
// names none but published its branch, the ones at the branch (a create whose response or save was lost, found as
// Propose finds it), of any state and base, since a person may have retargeted, merged or closed it. Any request open
// at the branch refuses; a closed or merged one is the package's when it holds the package's commit, as Propose adopts
// one, and is read as the saved one is (requestState). Any failure of the read is the read's.
func (s *Ship) requestEnded(ctx context.Context, c store.Case, p Package) (closed, merged string, err error) {
	if p.PR != "" {
		raw, err := s.gh(ctx, p.Host, "pr", "view", p.PR, "--repo", p.Repo, "--json", "state,headRefOid")
		if err != nil {
			return "", "", err
		}
		var pr struct {
			State string `json:"state"`
			Head  string `json:"headRefOid"`
		}
		if err := json.Unmarshal([]byte(raw), &pr); err != nil {
			return "", "", fmt.Errorf("read the pull request %s: %w", p.PR, err)
		}
		return s.requestState(ctx, c, p, p.PR, pr.State, pr.Head)
	}
	if p.PublishedHead == "" || p.Branch == "" || p.Repo == "" {
		return "", "", nil // nothing of the package's was ever pushed to its branch: no request is at it
	}
	out, err := s.gh(ctx, p.Host, "pr", "list", "--repo", p.Repo, "--head", p.Branch, "--state", "all", "--json", "url,state,headRefOid")
	if err != nil {
		return "", "", err
	}
	var prs []struct {
		URL   string `json:"url"`
		State string `json:"state"`
		Head  string `json:"headRefOid"`
	}
	if err := json.Unmarshal([]byte(out), &prs); err != nil {
		return "", "", fmt.Errorf("read the pull requests of %s: %w", p.Branch, err)
	}
	for _, pr := range prs {
		if pr.State == "OPEN" {
			return "", "", refused("case %s's branch %s has an open pull request, %s: the loop closes no pull request, so close it, then withdraw the package", c.ID, p.Branch, pr.URL)
		}
	}
	for _, pr := range prs {
		if p.Head == "" || pr.Head != p.Head {
			continue // an earlier package's request at the case's branch
		}
		cl, mg, err := s.requestState(ctx, c, p, pr.URL, pr.State, pr.Head)
		if err != nil {
			return "", "", err
		}
		closed, merged = firstOf(closed, cl), firstOf(merged, mg)
	}
	if merged != "" {
		return "", merged, nil // a merge is how the change ended, whatever request was closed before it
	}
	return closed, "", nil
}

// requestState reads a package's request (url) in state, at head. closed is a request a person closed without merging;
// merged one a person merged, which is a hand landing's landing only. An open request refuses, since the loop closes
// none, and so does a hand landing's merged one, or its closed one whose change the default branch holds as proposed (a
// landing by push), which the next pass records as its landing.
func (s *Ship) requestState(ctx context.Context, c store.Case, p Package, url, state, head string) (closed, merged string, err error) {
	switch {
	case state == "CLOSED" && len(p.Hand) > 0:
		// a person who lands a hand landing's head by a push and then closes its request landed it all the same, as
		// reconcileHand reads it
		if _, _, landed, err := s.landedByContent(ctx, p, head); err != nil {
			return "", "", err
		} else if landed {
			return "", "", refused("case %s's pull request %s was closed, and %s holds its change as proposed: a hand landing, whose landing the next pass records", c.ID, url, p.Base)
		}
		return url, "", nil
	case state == "CLOSED":
		return url, "", nil
	case state == "OPEN":
		return "", "", refused("case %s's pull request %s is open: the loop closes no pull request, so close it, then withdraw the package", c.ID, url)
	case state == "MERGED" && len(p.Hand) > 0:
		return "", "", refused("case %s's pull request %s was merged: a hand landing, whose landing the next pass records", c.ID, url)
	case state == "MERGED":
		return "", url, nil
	}
	return "", "", refused("case %s's pull request %s is %q", c.ID, url, state)
}

// doneHandNotes resolves a case's hand landing notes with no case log line: a merged case's.
func (s *Ship) doneHandNotes(ctx context.Context, caseID string) error {
	if _, _, err := s.Store.DoneNote(ctx, handNoteID(caseID), s.now()); err != nil && !errors.Is(err, store.ErrNoNote) {
		return err
	}
	return s.doneHandReadNotes(ctx, caseID)
}

// ActionWithdrawn is the case log action of a package its owner withdrew (Withdraw).
const ActionWithdrawn = "package withdrawn"
