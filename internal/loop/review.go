package loop

import (
	"context"
	"encoding/json"
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
	cs, err := r.Store.Cases(ctx, store.CaseFilter{Statuses: []store.CaseStatus{store.CaseTesting}})
	if err != nil {
		r.logf("reviews: %v", err)
		return nil
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
		if err != nil || !ok || (p.State != Proposed && p.State != Landing) {
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
		if p.Clean() {
			if landed, err := r.Ship.Land(ctx, c.ID); err != nil {
				out = append(out, Done{Family: c.Family, Case: c.ID, Step: "land", Skipped: err.Error()})
				action := "not landed"
				if landed.State == Landed && landed.Cleanup != "" {
					action = "cleanup refused"
				}
				r.Store.CaseNote(store.WithTextCleaner(ctx, Redact), c.ID, action, err.Error(), "loop")
			} else {
				out = append(out, Done{Family: c.Family, Case: c.ID, Step: "land"})
			}
			continue
		}
		if n := len(p.Reviews); n > 0 && p.Reviews[n-1].Head == p.Head || n >= MaxRounds {
			continue // reviewed on its head: its findings go back to the build, or its rounds are used
		}
		if spent+r.Budgets.Run > r.Budgets.Case {
			out = append(out, Done{Family: c.Family, Case: c.ID, Step: StepReview, Skipped: fmt.Sprintf("spent $%.2f of its $%.0f, no room for a review", spent, r.Budgets.Case)})
			continue
		}
		if stop, err := r.stopped(ctx, c, false); err != nil || stop != "" {
			out = append(out, Done{Family: c.Family, Case: c.ID, Step: StepReview, Skipped: firstOf(stop, fmt.Sprint(err))})
			continue
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
		run, refusal := r.reservePrepared(ctx, run)
		if refusal != "" {
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
	if err == nil && res.Status == store.RunOK {
		current, e := r.Store.CaseByID(ctx, c.ID)
		if e != nil || current.Into != "" || current.Episode != run.CaseEpisode || current.Status != store.CaseTesting {
			res.Status, res.Failure = store.RunFailed, "review authority"
			r.finish(ctx, c, &run, res, nil)
			d.Run = run
			return d
		}
		var rep struct {
			Summary  *string    `json:"summary"`
			Findings *[]Finding `json:"findings"`
		}
		if json.Unmarshal(res.Report, &rep) != nil || rep.Summary == nil || rep.Findings == nil {
			res.Status, res.Failure = store.RunFailed, "an unreadable review"
		} else if h, m, l, cerr := countFindings(*rep.Findings); cerr != nil {
			res.Status, res.Failure = store.RunFailed, "report"
		} else {
			text := reviewText(len(p.Reviews)+1, h, m, l, *rep.Summary, *rep.Findings)
			rev := Review{Findings: *rep.Findings, Text: text}
			saved, saveErr := r.Ship.Reviewed(ctx, c.ID, rev)
			if saveErr != nil {
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
	if err := r.Ship.authority(ctx, p.Case); err != nil {
		return p, err
	}
	n := len(p.Reviews) - 1
	if n < 0 || p.Reviews[n].Comment != "" {
		return p, nil
	}
	if r.Post == nil || p.PR == "" {
		return p, fmt.Errorf("review posting is unavailable")
	}
	pub, err := r.Ship.public(ctx, p.Reviews[n].Text)
	if err != nil {
		return p, err
	}
	t, err := r.Ship.Gate.Repository(ctx, p.Dir)
	if err != nil {
		return p, err
	}
	if !strings.EqualFold(t.Repo, p.Repo) {
		return p, refused("the review destination is not the package's repository")
	}
	url, err := r.Post(ctx, p.Repo, p.PR, pub)
	if err != nil {
		return p, err
	}
	if url == "" {
		return p, fmt.Errorf("review post returned no receipt")
	}
	p.Reviews[n].Text, p.Reviews[n].Comment = pub, url
	if (p.Reviews[n].High > 0 || p.Reviews[n].Medium > 0) && len(p.Reviews) < MaxRounds {
		p.State = Revising
	}
	return p, r.Ship.saveAuthorized(ctx, &p)
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
		if !ok || !roundsUsed(p) {
			continue
		}
		waiting[id] = true
		at := p.Reviews[len(p.Reviews)-1].At
		if n, ok := have[id]; ok && (n.Done.IsZero() || !n.At.Before(at.Truncate(time.Millisecond))) {
			continue // it stands, or it was resolved while this change waited
		}
		note := store.Note{ID: id, Kind: store.NoteProblem, Importance: store.ImportanceLow, At: at,
			Title: fmt.Sprintf("The loop's change for case %s used its %d review rounds and waits for a person", c.ID, MaxRounds),
			Detail: fmt.Sprintf("No review round remains for the current change in %s. The loop builds and reviews this change no more. "+
				"Take it over in its pull request, or close the case.", firstOf(p.PR, "its change"))}
		if u, err := url.Parse(p.PR); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
			note.Links = []store.Link{{Label: "pull request", URL: p.PR}}
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
