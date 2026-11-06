package loop

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agent-sh/momentum/internal/store"
)

const LessonRetry = 24 * time.Hour

func (r *Runner) lessonState(ctx context.Context, id string) (published, cooling bool, err error) {
	entries, err := r.Store.CaseLog(ctx, id)
	if err != nil {
		return false, false, err
	}
	for _, entry := range entries {
		published = published || entry.Action == store.LessonPublished
		cooling = cooling || entry.Action == store.LessonReserved && r.now().Sub(entry.At) < LessonRetry
	}
	return published, cooling, nil
}

func (r *Runner) published(ctx context.Context, id string) bool {
	published, _, err := r.lessonState(ctx, id)
	if err != nil {
		r.logf("read lesson receipt of %s: %v", id, err)
	}
	return published
}

// PublishLesson reserves the cleaned lesson before writing it under the case's
// stable name. A receipt follows a successful write, never the reservation.
func PublishLesson(ctx context.Context, st *store.Store, ship *Ship, id, text, tacit, actor string) error {
	c, err := st.CaseByID(ctx, id)
	if err != nil {
		return err
	}
	if c.Into != "" || c.Status != store.CaseClosed || c.Reason != store.Improved && c.Reason != store.Guarded {
		return refused("a lesson is published only for a case closed as improved or guarded")
	}
	line := strings.TrimSpace(text)
	if line == "" || strings.ContainsAny(line, "\r\n") || utf8.RuneCountInString(line) > 300 {
		return refused("a lesson is one line of at most 300 characters")
	}
	clean, err := Public(ctx, line)
	if err != nil {
		return err
	}
	reserved, start, err := st.BeginLesson(ctx, c.ID, clean)
	if err != nil || !start {
		return err
	}
	scope := "projects/momentum"
	if p, ok, err := ship.Load(c.ID); err != nil {
		return err
	} else if ok && p.Repo != "" {
		scope = "projects/" + p.Repo[strings.IndexByte(p.Repo, '/')+1:]
	}
	// Reclean an older reservation too. Measurement validates the outcome,
	// not the model's explanation or instructions. A guard validates neither: its
	// lesson says so, with the probability its watch reports.
	prefix := "unverified lesson from a loop run: "
	if c.Reason == store.Guarded {
		prefix = fmt.Sprintf("unverified lesson from a loop run, guarded, not measured (P=%.0f%% that the problem, had it kept happening, would have recurred in its watch): ",
			100*c.Measurement.Recur)
	}
	desc, err := Public(ctx, prefix+reserved)
	if err != nil {
		return err
	}
	kind := ""
	if c.Reason == store.Guarded {
		kind = ", guarded, not measured: its problem did not come back in its watch, which a problem that stopped by itself shows too"
	}
	body, err := Public(ctx, fmt.Sprintf("Unverified lesson from momentum's improvement loop on case %s (%s)%s: %s", c.ID, c.Family, kind, c.Title))
	if err != nil {
		return err
	}
	current, err := st.CaseByID(ctx, id)
	if err != nil || current.Into != "" || current.Episode != c.Episode || current.Status != store.CaseClosed || current.Reason != c.Reason {
		return firstError(err, "case changed before lesson publication")
	}
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, firstOf(tacit, "tacit"), "add", "--harness", "claude", "--kind", "observed",
		"--scope", scope, "--desc", desc, "--body", body, "momentum-lesson-"+strings.ToLower(c.ID))
	cmd.Env = CleanEnv(nil)
	out, diag := Capture{Limit: MaxStream}, Capture{Limit: MaxStream}
	cmd.Stdout, cmd.Stderr = &out, &diag
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tacit add failed: %w", err)
	}
	if out.Over() || diag.Over() {
		return errors.New("tacit output exceeds its limit")
	}
	return st.CaseNote(store.WithTextCleaner(ctx, Redact), c.ID, store.LessonPublished, scope+": "+desc, actor)
}

func (r *Runner) lessons(ctx context.Context, only string, families []string) error {
	if r.Ship == nil {
		return nil
	}
	cases, err := r.Store.Cases(ctx, store.CaseFilter{Statuses: []store.CaseStatus{store.CaseClosed}})
	if err != nil {
		return err
	}
	for _, c := range cases {
		if c.Reason != store.Improved && c.Reason != store.Guarded || only != "" && only != c.ID || !slices.Contains(families, c.Family) {
			continue
		}
		done, cooling, err := r.lessonState(ctx, c.ID)
		if err != nil {
			return err
		}
		if done || cooling {
			continue
		}
		p, ok, err := r.Ship.Load(c.ID)
		if err != nil {
			if noteErr := r.packageProblem(ctx, c, err, false); noteErr != nil {
				return noteErr
			}
			r.logf("lesson package of %s: %v", c.ID, err)
			continue
		}
		if !ok || p.Lesson == "" {
			continue
		}
		if err := PublishLesson(ctx, r.Store, r.Ship, c.ID, p.Lesson, r.Tacit, "loop"); err != nil {
			r.logf("publish lesson of %s: %v", c.ID, err)
		}
	}
	return nil
}
