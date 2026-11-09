package loop

import (
	"context"
	"errors"
	"fmt"
	"github.com/agent-sh/momentum/internal/store"
	"net/url"
	"slices"
	"strings"
	"time"
)

func (r *Runner) reviews(ctx context.Context, only string, families ...string) []Done {
	if r.Ship == nil {
		return nil
	}
	var out []Done
	// each family's cases are read on their own, so a row of one that cannot be read keeps no other family's reviews
	var cs []store.Case
	reads := []string{""} // no families: every family's at once
	if len(families) > 0 {
		reads = families
	}
	for _, fam := range reads {
		fcs, err := r.Store.Cases(ctx, store.CaseFilter{Family: fam, Statuses: []store.CaseStatus{store.CaseTesting}})
		if err != nil {
			r.logf("reviews %s: %v", fam, err)
			out = append(out, Done{Family: fam, Step: StepReview, Skipped: "cannot read the cases in testing: " + err.Error()})
			continue
		}
		cs = append(cs, fcs...)
	}
	for _, c := range cs {
		if len(families) > 0 && !slices.Contains(families, c.Family) {
			continue
		}
		if only != "" && c.ID != only {
			continue
		}
		spent, running, err := r.caseRoom(ctx, c)
		if err != nil || running {
			why := "a run is in flight"
			if err != nil {
				why = "cannot read the case's reservations: " + err.Error()
			}
			out = append(out, Done{Family: c.Family, Case: c.ID, Step: StepReview, Skipped: why})
			continue
		}
		p, ok, err := r.Ship.Load(c.ID)
		if err != nil {
			out = append(out, Done{Family: c.Family, Case: c.ID, Step: StepReview, Skipped: "package: " + err.Error()})
			continue
		}
		if !ok || (p.State != Proposed && p.State != Landing) {
			continue
		}
		if r.handUnread[c.ID] {
			out = append(out, Done{Family: c.Family, Case: c.ID, Step: StepReview, Skipped: "its hand landing request could not be read in this pass"})
			continue
		}
		if p.Kind == KindConfig && !p.GoneSince.IsZero() {
			// its file is gone: it waits with its one note (configNote) until the file is back, then it is reviewed
			out = append(out, Done{Family: c.Family, Case: c.ID, Step: StepReview, Skipped: "its file is gone: it is reviewed once the file is back"})
			continue
		}
		if n := len(p.Reviews); n > 0 && p.Reviews[n-1].Head == p.Head && p.Reviews[n-1].Comment == "" {
			var err error
			p, err = r.postReview(ctx, p)
			if err != nil {
				out = append(out, Done{Family: c.Family, Case: c.ID, Step: StepReview, Skipped: err.Error()})
				continue
			}
		}
		if p.Clean() && p.Kind == KindConfig {
			// reviewed clean, a configuration proposal is filed for an agent to apply: the loop never writes its file
			filedP, filed, err := r.Ship.fileConfig(ctx, c.ID)
			switch {
			case err != nil:
				out = append(out, Done{Family: c.Family, Case: c.ID, Step: "file", Skipped: err.Error()})
			case filed:
				out = append(out, Done{Family: c.Family, Case: c.ID, Step: "file"})
			case !filedP.Filed.IsZero():
				out = append(out, Done{Family: c.Family, Case: c.ID, Step: "land", Skipped: "a configuration proposal: an agent applies it"})
			}
			continue
		}
		if p.Clean() && len(p.Hand) > 0 {
			// reviewed clean, it still waits for a person: the loop never lands a hand landing proposal
			out = append(out, Done{Family: c.Family, Case: c.ID, Step: "land", Skipped: store.HandStage + ": a session lands " + p.PR + " or closes it"})
			continue
		}
		if p.Clean() {
			if !r.Ship.Live {
				out = append(out, Done{Family: c.Family, Case: c.ID, Step: "land", Skipped: "the loop is not live"})
				continue
			}
			if landed, err := r.Ship.Land(ctx, c.ID); err != nil {
				out = append(out, Done{Family: c.Family, Case: c.ID, Step: "land", Skipped: err.Error()})
				action := "not landed"
				if landed.State == Landed && landed.Cleanup != "" {
					action = "cleanup refused"
				}
				if noteErr := r.caseNoteOnce(ctx, c.ID, action, err.Error()); noteErr != nil {
					r.logf("record landing refusal: %v", noteErr)
				}
			} else {
				out = append(out, Done{Family: c.Family, Case: c.ID, Step: "land"})
			}
			continue
		}
		if n := len(p.Reviews); n > 0 && p.Reviews[n-1].Head == p.Head || n >= MaxRounds {
			continue // reviewed on its head: its findings go back to the build, or its rounds are used
		}
		if spent+r.Budgets.Run > r.Budgets.Case {
			why := fmt.Sprintf("spent $%.2f of its $%.0f, no room for a review", spent, r.Budgets.Case)
			out = append(out, Done{Family: c.Family, Case: c.ID, Step: StepReview, Skipped: why})
			if err := r.noteReviewSkip(ctx, c, "budget", why); err != nil {
				r.logf("review waiting note: %v", err)
			}
			continue
		}
		if stop, err := r.stopped(ctx, c, false); err != nil || stop != "" {
			why := firstOf(stop, fmt.Sprint(err))
			out = append(out, Done{Family: c.Family, Case: c.ID, Step: StepReview, Skipped: why})
			if err := r.noteReviewSkip(ctx, c, "stopped", why); err != nil {
				r.logf("review waiting note: %v", err)
			}
			continue
		}
		if err := r.endStoppedNote(ctx, c); err != nil {
			r.logf("review stopped note: %v", err)
		}
		if err := r.prepareInputs(ctx); err != nil {
			out = append(out, Done{Family: c.Family, Case: c.ID, Step: StepReview, Skipped: "inputs: " + err.Error()})
			continue
		}
		run, prepErr := r.prepareRun(ctx, c, c.Family, StepReview)
		if prepErr != nil {
			out = append(out, Done{Family: c.Family, Case: c.ID, Step: StepReview, Skipped: "inputs: " + prepErr.Error()})
			continue
		}
		dir := run.Dir
		run, refusal := r.reservePrepared(ctx, run)
		if refusal != "" {
			if run.Dir == "" { // no row names the directory: what the preparation left goes
				r.removeRunDirs(context.WithoutCancel(ctx), c.ID, dir, false)
			}
			out = append(out, Done{Family: c.Family, Case: c.ID, Step: StepReview, Run: run, Skipped: refusal})
			continue
		}
		out = append(out, r.review(ctx, c, p, run))
	}
	return out
}

func (r *Runner) review(ctx context.Context, c store.Case, p Package, run store.LoopRun) Done {
	d := Done{Family: c.Family, Case: c.ID, Step: StepReview}
	res, err := r.invokeReview(ctx, c, p, run)
	if err == nil && exitedBeforeStart(res) {
		// a harness that does not start: kept as review run, which the stop rule counts. A review's other errors before
		// its harness stay run, which it does not count: a broker that cannot be reached is one of them
		res.Status, res.Failure, res.Text = store.RunFailed, "review run", neverStarted(res).Error()
	}
	if err == nil && res.Status == store.RunOK {
		current, e := r.Store.CaseByID(ctx, c.ID)
		if e != nil || current.Into != "" || current.Episode != run.CaseEpisode || current.Status != store.CaseTesting {
			res.Status, res.Failure = store.RunFailed, "review authority"
			r.finish(ctx, c, &run, res, nil)
			d.Run = run
			return d
		}
		rep, reportErr := readReviewReport(res.Report, p.Changed)
		if reportErr != nil {
			res.Status, res.Failure = store.RunFailed, "an unreadable review"
		} else if h, m, l, cerr := countFindings(*rep.Findings); cerr != nil {
			res.Status, res.Failure = store.RunFailed, "report"
		} else {
			text := reviewText(len(p.Reviews)+1, h, m, l, *rep.Summary, *rep.Findings)
			rev := Review{Head: p.Head, Findings: *rep.Findings, Text: text}
			saved, saveErr := r.Ship.Reviewed(ctx, c.ID, rev)
			if saveErr != nil && held(saveErr) {
				// a lock held by a person's step, or a package head that moved since the export, is a hold, not a
				// failure of the reviewer: the run is not counted by the stop rule and the next pass reviews the
				// current head again
				res.Status, res.Failure = store.RunFailed, "hold"
				r.logf("review of %s not recorded: %v", c.ID, saveErr)
			} else if saveErr != nil {
				res.Status, res.Failure = store.RunFailed, "review"
			} else if _, postErr := r.postReview(ctx, saved); postErr != nil {
				r.logf("post the review of %s: %v", c.ID, postErr)
			}

			run.Summary = fmt.Sprintf("%d high, %d medium, %d low: %s", h, m, l, *rep.Summary)
		}
	}
	r.finish(ctx, c, &run, res, err)
	d.Run, d.Refused = run, res.Refused
	return d
}

func reviewText(round, high, medium, low int, summary string, findings []Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Clean-context review, round %d\n\n", round)
	verdict := "Ready to merge"
	if high > 0 || medium > 0 {
		verdict = "Not ready to merge"
	}
	fmt.Fprintf(&b, "%s: %d High, %d Medium, %d Low.\n\n%s\n", verdict, high, medium, low, strings.TrimSpace(summary))
	n := map[string]int{}
	for _, f := range findings {
		tag := strings.ToUpper(f.Severity[:1])
		n[tag]++
		fmt.Fprintf(&b, "\n**%s%d.** `%s`: %s Fix: %s\n", tag, n[tag], f.Where, strings.TrimSpace(f.Problem), strings.TrimSpace(f.Fix))
	}
	b.WriteString("\nPosted by momentum's improvement loop.\n")
	return b.String()
}

func (r *Runner) postReview(ctx context.Context, p Package) (Package, error) {
	unlock, err := r.Ship.lock(ctx, p.Case)
	if err != nil {
		return p, err
	}
	defer unlock()
	current, ok, err := r.Ship.Load(p.Case)
	if err != nil {
		return p, err
	}
	if !ok {
		return p, refused("the review package is missing")
	}
	p = current
	if err := r.Ship.authority(ctx, p.Case); err != nil {
		return p, err
	}
	n := len(p.Reviews) - 1
	if n < 0 || p.Reviews[n].Comment != "" {
		return p, nil
	}
	if p.Kind == KindConfig {
		// a configuration proposal has no pull request: its review goes to its case's log, once its file's authority
		// is checked again as a repository's is before a post, and whole beside its patch, since the log cuts a line
		if err := r.Ship.packageAuthority(ctx, p); err != nil {
			return p, err
		}
		text, err := Redact(ctx, p.Reviews[n].Text)
		if err != nil {
			return p, err
		}
		if err := writeFileSafe(r.Ship.proposalFile(p, ".review.md"), []byte(text+"\n")); err != nil {
			return p, err
		}
		// once per round of the package: a post that stopped before its save posts again
		line := fmt.Sprintf("round %d of the package made %s: %s", p.Reviews[n].Round, p.Created.UTC().Format(time.RFC3339Nano), text)
		if err := r.caseNoteOnce(ctx, p.Case, "review posted", line); err != nil {
			return p, err
		}
		if err := r.Ship.wrote("review: posted"); err != nil {
			return p, err
		}
		p.Reviews[n].Comment = fmt.Sprintf("case log: review round %d", p.Reviews[n].Round)
		if (p.Reviews[n].High > 0 || p.Reviews[n].Medium > 0) && len(p.Reviews) < MaxRounds {
			p.State = Revising
		}
		err = r.Ship.saveAuthorized(ctx, &p)
		return p, err
	}
	if r.Post == nil || p.PR == "" {
		return p, fmt.Errorf("review posting is unavailable")
	}
	t, err := r.Ship.Gate.Repository(ctx, p.Dir)
	if err != nil {
		return p, err
	}
	if !strings.EqualFold(t.Repo, p.Repo) {
		return p, refused("the review destination is not the package's repository")
	}
	private, err := r.Ship.repositoryPrivate(ctx, p)
	if err != nil {
		return p, err
	}
	text := p.Reviews[n].Text
	if !private {
		rev := p.Reviews[n]
		text = fmt.Sprintf("Case: %s\nBase: %s\nHead: %s\nReview round %d: %d high, %d medium, %d low.",
			p.Case, p.BaseCommit, p.Head, rev.Round, rev.High, rev.Medium, rev.Low)
	}
	pub, err := r.Ship.public(ctx, text)
	if err != nil {
		return p, err
	}
	url, err := r.Post(ctx, p.Repo, p.PR, pub)
	if err != nil {
		return p, err
	}
	if url == "" {
		return p, fmt.Errorf("review post returned no receipt")
	}
	p.Reviews[n].Comment = url
	if (p.Reviews[n].High > 0 || p.Reviews[n].Medium > 0) && len(p.Reviews) < MaxRounds {
		p.State = Revising
	}
	err = r.Ship.saveAuthorized(ctx, &p)
	return p, err
}

func (r *Runner) reviewDiff(ctx context.Context, p Package) (string, error) {
	diff, err := r.Ship.git(ctx, p.Host, "diff", "--binary", "--no-ext-diff", "--no-textconv", "--no-renames", p.BaseCommit, p.Head)
	if err != nil {
		return "", err
	}
	return Redact(ctx, diff)
}

func roundsUsed(p Package) bool {
	n := len(p.Reviews)
	if n < MaxRounds {
		return false
	}
	switch p.State {
	case Prepared, Checked, Revising:
		return true
	case Proposed, Landing:
		last := p.Reviews[n-1]
		// Posting a clean review is retryable and needs no new model round.
		return last.Head != p.Head || last.High > 0 || last.Medium > 0
	}
	return false
}

func proposalsUsed(p Package) bool {
	return p.ClosedProposals >= MaxClosedProposals && (p.State == Prepared || p.State == Checked || p.State == Revising || p.State == Withdrawn)
}

func (r *Runner) noteReviewSkip(ctx context.Context, c store.Case, cause, why string) error {
	detail, err := Redact(ctx, why)
	if err != nil {
		return err
	}
	detail = store.CaseNoteText(detail)
	id := store.ReviewNoteID(c.ID, cause)
	notes, err := r.Store.Notes(ctx)
	if err != nil {
		return err
	}
	resumed, err := r.Store.ResumedAt(ctx, c.ID)
	if err != nil {
		return err
	}
	for _, note := range notes {
		// the same cause stays resolved if its reader resolved it; a stopped note, until the case resumes past it. A note in
		// the resume's millisecond came before it: the stop rule counts runs from the millisecond after the resume.
		if note.ID == id && note.Detail == detail && (note.Done.IsZero() || cause != "stopped" || note.At.After(resumed)) {
			return nil
		}
	}
	_, err = r.Store.PutNote(ctx, store.Note{ID: id, Kind: store.NoteProblem, Importance: store.ImportanceLow,
		At: r.now(), Title: "The loop's review for case " + c.ID + " waits: " + cause, Detail: detail})
	return err
}

// stopEnded is the detail of a review's stopped note whose stop the rule holds no more (endStoppedNote).
const stopEnded = "the stop rule no longer holds the case"

// endStoppedNote marks done a stopped note of a case's review once the stop rule holds the case no more without a
// resume (a new episode, or a stop the rule no longer counts, such as two refusals a build kept before causes were).
// Its detail says so first, so a later stop, whatever its why, puts the note again (noteReviewSkip). A note done
// already is skipped only when its detail says so: one its reader dismissed while the stop held keeps the stop's why,
// which would hide a later stop for the same why.
func (r *Runner) endStoppedNote(ctx context.Context, c store.Case) error {
	id := store.ReviewNoteID(c.ID, "stopped")
	n, ok, err := r.Store.Note(ctx, id)
	if err != nil || !ok || !n.Done.IsZero() && n.Detail == stopEnded {
		return err
	}
	n.Detail = stopEnded
	if _, err := r.Store.PutNote(ctx, n); err != nil {
		return err
	}
	_, _, err = r.Store.DoneNote(ctx, id, r.now())
	return err
}

const waitingPrefix = "loop-rounds:"

func (r *Runner) noteWaiting(ctx context.Context) error {
	if r.Ship == nil {
		return nil
	}
	cs, err := r.Store.Cases(ctx, store.CaseFilter{Statuses: []store.CaseStatus{store.CaseTesting}})
	if err != nil {
		return err
	}
	notes, err := r.Store.Notes(ctx)
	if err != nil {
		return err
	}
	have := map[string]store.Note{}
	for _, n := range notes {
		have[n.ID] = n
	}
	waiting := map[string]bool{}
	var unread []error
	for _, c := range cs {
		id := waitingPrefix + store.IDPart(c.ID)
		p, ok, err := r.Ship.Load(c.ID)
		if err != nil {
			waiting[id] = true // unread is not proof that the case left its waiting state
			unread = append(unread, fmt.Errorf("case %s package: %w", c.ID, err))
			continue
		}
		if !ok || !proposalsUsed(p) && (!roundsUsed(p) || p.Kind == KindConfig) {
			continue // a configuration change whose rounds are used waits with its one note (configNote)
		}
		waiting[id] = true
		at := p.Updated
		if !proposalsUsed(p) {
			at = p.Reviews[len(p.Reviews)-1].At
		}
		if n, ok := have[id]; ok && (n.Done.IsZero() || !n.At.Before(at.Truncate(time.Millisecond))) {
			continue // it stands, or it was resolved while this change waited
		}
		note := store.Note{ID: id, Kind: store.NoteProblem, Importance: store.ImportanceLow, At: at,
			Title: fmt.Sprintf("The loop's change for case %s used its %d review rounds and waits for a person", c.ID, MaxRounds),
			Detail: fmt.Sprintf("No review round remains for the current change in %s. The loop builds and reviews this change no more. "+
				"Take it over in its pull request, or close the case.", firstOf(p.PR, "its change"))}
		if proposalsUsed(p) {
			note.Title = fmt.Sprintf("The loop's change for case %s used its %d closed proposals and waits for a person", c.ID, MaxClosedProposals)
			note.Detail = "Two pull requests were closed without merging. The loop builds this case no more. Propose the checked package with ship propose, or close the case."
			if p.Kind == KindConfig {
				note.Detail = "Its configuration proposals were withdrawn three times: its file kept changing under them. The loop builds this case no more. Change the file by hand, or close the case."
			} else if p.State == Withdrawn {
				note.Detail = "Two pull requests were closed without merging, the last a hand landing proposal. The loop builds this case no more. Take the change over by hand, or close the case."
			}
		}
		if u, err := url.Parse(firstOf(p.PR, p.Withdrew)); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
			note.Links = []store.Link{{Label: "pull request", URL: firstOf(p.PR, p.Withdrew)}}
		}
		if _, err := r.Store.PutNote(ctx, note); err != nil {
			return err
		}
	}
	for id, n := range have {
		if strings.HasPrefix(id, waitingPrefix) && n.Done.IsZero() && !waiting[id] {
			if _, _, err := r.Store.DoneNote(ctx, id, r.now()); err != nil {
				return err
			}
		}
	}
	return errors.Join(unread...)
}
