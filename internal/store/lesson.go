package store

import (
	"context"
	"time"
)

// Lesson states on a case's log: the text reserved before tacit's write, and the receipt after it.
const (
	LessonReserved  = "publishing"
	LessonPublished = "published"
)

// BeginLesson reserves a case's lesson before tacit's write, and returns the text to write: the one reserved first,
// so a write tried again after one that failed partway writes the same lesson (tacit takes the same name and text
// as the same fact). ok is false when the lesson is published already.
func (s *Store) BeginLesson(ctx context.Context, id, text string) (string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT action, detail FROM case_log WHERE case_id = ? AND action IN (?, ?) ORDER BY seq`, id,
		LessonReserved, LessonPublished)
	if err != nil {
		return "", false, err
	}
	reserved := ""
	for rows.Next() {
		var action, detail string
		if err := rows.Scan(&action, &detail); err != nil {
			rows.Close()
			return "", false, err
		}
		if action == LessonPublished {
			rows.Close()
			return "", false, nil
		}
		if reserved == "" {
			reserved = detail
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	if reserved != "" {
		return reserved, true, nil
	}
	if err := caseLog(ctx, tx, id, "loop", LessonReserved, text, time.Now()); err != nil {
		return "", false, err
	}
	return caseText(text, caseTextMax), true, tx.Commit()
}
