package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// A fix a person made, outside the loop's packages, is recorded on its case and measured like a change the loop
// landed: the measure step compares its signature's recurrence in the window before the fix with the window after it,
// and closes the case as improved only when the rate measurably fell (CloseHandFixed). ActionHandFix records one:
// named by a person (case fixed), or seen landing at the place the case's investigation names (a commit there, or a
// file's change). ActionHandMeasured records what its measurement found when it did not close the case.
const (
	ActionHandFix      = "fixed by hand"
	ActionHandMeasured = "hand fix measured"
)

// HandFix is a fix of a case made by hand: what it is (a reference: git:OWNER/REPO@SHA, file:PATH, ...), when it went
// in, how it was learned (named by a person, or seen landing), and whether the measure step has measured it yet.
type HandFix struct {
	Ref      string    `json:"ref"`
	At       time.Time `json:"-"`
	AtMS     int64     `json:"at"`
	How      string    `json:"how"` // named or seen
	Episode  int64     `json:"episode"`
	Measured bool      `json:"-"`
}

// HowNamed and HowSeen are how a hand fix was learned.
const (
	HowNamed = "named"
	HowSeen  = "seen"
)

// RecordHandFix records a fix made by hand on a case that is open, investigating or testing, in its current episode:
// ref is what it is, at when it went in (not after now). The same reference twice in an episode records nothing more.
// It reports whether it recorded one.
func (s *Store) RecordHandFix(ctx context.Context, id, ref string, at time.Time, how, actor string) (bool, error) {
	if err := checkRef(ref); err != nil {
		return false, badCase("what fixed it: %v", err)
	}
	if how != HowNamed && how != HowSeen {
		return false, badCase("a hand fix is %s or %s", HowNamed, HowSeen)
	}
	now := time.Now()
	if at.IsZero() || at.After(now) {
		return false, badCase("a hand fix went in by now")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	c, err := live(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if !slices.Contains(working, c.Status) {
		return false, badCase("case %s is %s: a fix by hand is measured on a case that is open, investigating or testing", c.ID, c.Status)
	}
	fixes, err := handFixes(ctx, tx, c)
	if err != nil {
		return false, err
	}
	if slices.ContainsFunc(fixes, func(f HandFix) bool { return f.Ref == ref }) {
		return false, nil
	}
	b, err := json.Marshal(HandFix{Ref: ref, AtMS: at.UnixMilli(), How: how, Episode: c.Episode})
	if err != nil {
		return false, err
	}
	if err := caseLog(ctx, tx, c.ID, actor, ActionHandFix, string(b), now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// HandFixes are a case's fixes by hand in its current episode, oldest first, each marked measured once the measure
// step recorded what its measurement found.
func (s *Store) HandFixes(ctx context.Context, id string) ([]HandFix, error) {
	c, err := s.CaseByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return handFixes(ctx, s.db, c)
}

func handFixes(ctx context.Context, q querier, c Case) ([]HandFix, error) {
	rows, err := q.QueryContext(ctx, `SELECT action, detail FROM case_log WHERE case_id = ? AND action IN (?, ?) ORDER BY seq`,
		c.ID, ActionHandFix, ActionHandMeasured)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HandFix
	for rows.Next() {
		var action, detail string
		if err := rows.Scan(&action, &detail); err != nil {
			return nil, err
		}
		var f HandFix
		if json.Unmarshal([]byte(detail), &f) != nil || f.Episode != c.Episode || f.Ref == "" {
			continue
		}
		f.At = time.UnixMilli(f.AtMS)
		switch action {
		case ActionHandFix:
			if !slices.ContainsFunc(out, func(o HandFix) bool { return o.Ref == f.Ref }) {
				out = append(out, f)
			}
		case ActionHandMeasured:
			for i := range out {
				if out[i].Ref == f.Ref {
					out[i].Measured = true
				}
			}
		}
	}
	return out, rows.Err()
}

// HandFixMeasured records that a case's fix by hand was measured and did not close it: effect says what the
// measurement found.
func (s *Store) HandFixMeasured(ctx context.Context, id string, f HandFix, effect, actor string) error {
	c, err := s.CaseByID(ctx, id)
	if err != nil {
		return err
	}
	b, err := json.Marshal(struct {
		HandFix
		Effect string `json:"effect"`
	}{HandFix{Ref: f.Ref, AtMS: f.At.UnixMilli(), How: f.How, Episode: c.Episode}, effect})
	if err != nil {
		return err
	}
	return caseLog(ctx, s.db, c.ID, actor, ActionHandMeasured, string(b), time.Now())
}

// CloseHandFixed is the measure tool's: a case open, investigating or testing whose fix by hand measurably helped
// closes as improved, with its counts before and after the fix over window, the fix, and the effect in words. Its
// signature's recurrence brings it back (comesBack): no package of the loop's is there to watch.
func (s *Store) CloseHandFixed(ctx context.Context, id string, f HandFix, window time.Duration, before, after int, effect, actor string) (Case, error) {
	if strings.TrimSpace(effect) == "" || window <= 0 || before <= 0 || after < 0 {
		return Case{}, badCase("an improvement closes with its window, its counts before and after the fix and its effect")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Case{}, err
	}
	defer tx.Rollback()
	c, err := live(ctx, tx, id)
	if err != nil {
		return Case{}, err
	}
	fixes, err := handFixes(ctx, tx, c)
	if err != nil {
		return Case{}, err
	}
	switch {
	case !slices.Contains(working, c.Status):
		return Case{}, badCase("case %s is %s: a fix by hand closes a case that is open, investigating or testing", c.ID, c.Status)
	case c.Package != "":
		return Case{}, badCase("case %s has a package of the loop's: its measurement is the package's", c.ID)
	case !slices.ContainsFunc(fixes, func(h HandFix) bool { return h.Ref == f.Ref }):
		return Case{}, badCase("case %s holds no fix by hand %s in its episode", c.ID, f.Ref)
	}
	now := time.Now()
	text := fmt.Sprintf("%s (fixed by hand: %s, %s)", effect, f.Ref, f.At.UTC().Format(time.DateTime))
	if _, err := tx.ExecContext(ctx, `UPDATE cases SET status = ?, reason = ?, closed = ?, updated = ?, window_ms = ?, before_n = ?, after_n = ?,
	  effect = ?, guard_exposure = 0, guard_watched = 0, guard_recur = 0, closed_evidence = (SELECT COUNT(*) FROM case_evidence WHERE case_id = ?) WHERE id = ?`,
		CaseClosed, string(Improved), now.UnixMilli(), now.UnixMilli(), window.Milliseconds(), before, after, caseText(text, caseTextMax), c.ID, c.ID); err != nil {
		return Case{}, err
	}
	if err := caseLog(ctx, tx, c.ID, actor, "closed", string(Improved)+": "+text, now); err != nil {
		return Case{}, err
	}
	if c, err = getCase(ctx, tx, c.ID); err != nil {
		return Case{}, err
	}
	return c, tx.Commit()
}

// CloseDetectorFix is the measure tool's: a case whose problem is no longer seen because momentum's own detector keys
// its events otherwise now, or no longer as failures, closes as a detector fix, never as improved; effect says which.
// A case open, investigating or testing closes so, and so does one deployed or measuring whose own change was the
// detector's.
func (s *Store) CloseDetectorFix(ctx context.Context, id, effect, actor string) (Case, error) {
	if strings.TrimSpace(effect) == "" {
		return Case{}, badCase("a detector fix closes with what the detector keys its events as now")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Case{}, err
	}
	defer tx.Rollback()
	c, err := live(ctx, tx, id)
	if err != nil {
		return Case{}, err
	}
	if c.Status == CaseClosed {
		return Case{}, badCase("case %s is closed already (%s)", c.ID, c.Reason)
	}
	now := time.Now()
	if _, err := tx.ExecContext(ctx, `UPDATE cases SET status = ?, reason = ?, closed = ?, updated = ?, effect = ?,
	  closed_evidence = (SELECT COUNT(*) FROM case_evidence WHERE case_id = ?) WHERE id = ?`,
		CaseClosed, string(DetectorFix), now.UnixMilli(), now.UnixMilli(), caseText(effect, caseTextMax), c.ID, c.ID); err != nil {
		return Case{}, err
	}
	if err := caseLog(ctx, tx, c.ID, actor, "closed", string(DetectorFix)+": "+effect, now); err != nil {
		return Case{}, err
	}
	if c, err = getCase(ctx, tx, c.ID); err != nil {
		return Case{}, err
	}
	return c, tx.Commit()
}

// EvidenceEvents are the ids of the events a case's evidence holds, filed at or after since.
func (s *Store) EvidenceEvents(ctx context.Context, id string, since time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ref FROM case_evidence WHERE case_id = ? AND ref LIKE 'ev:%' AND at >= ?`, id, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		out = append(out, strings.TrimPrefix(ref, "ev:"))
	}
	return out, rows.Err()
}
