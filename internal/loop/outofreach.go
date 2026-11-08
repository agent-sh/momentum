package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
)

// A build whose case's fix needs a path no change of the loop's may touch answers so instead of making a change: the
// path and the change a person makes there (out_of_reach). The host checks the path with PathScope. A path it refuses
// holds the case for a person: a raised problem note names the path and the fix, and a "held for a person" line in the
// case's log keeps the hold with the case's episode and the evidence it held. The build's run ends as any answered run
// does, so the stop rule counts no failure and files no component-health case, and a held case takes no build slot. The
// hold ends when the case holds evidence it did not hold then, or leaves testing (a person closes it, or sends it back
// to its investigation): the case builds again. A path a change may touch is the build's own refusal at its report.

// OutOfReach is a build report's answer that the case's fix is out of the loop's reach: the path, from the
// repository's root, and the change a person makes there.
type OutOfReach struct {
	Path string `json:"path"`
	Fix  string `json:"fix"`
}

const outOfReachSchema = `{"type":"object","additionalProperties":false,"required":["path","fix"],` +
	`"description":"Only when the case's fix needs a path no change may touch (the build prompt names them) and no change in reach fixes the case: make no change and give this instead. The host reads only summary and this answer, checks the path, and the case waits for a person. A path a change may touch refuses the build.",` +
	`"properties":{"path":{"type":"string","description":"The path from the repository's root, such as .github/workflows/ci.yml."},` +
	`"fix":{"type":"string","description":"The change a person makes there, in short sentences."}}}`

// outOfReachBrief tells a build it may answer that its fix is out of reach.
const outOfReachBrief = "When the case's fix needs a path no change may touch and nothing in reach fixes the case, make no change and answer out_of_reach: " +
	"the path from the repository's root and the change a person makes there. The host checks the path, and the case waits for a person. " +
	"A path a change may touch refuses the build."

// The case log lines of a hold for a person: its start, with its record, and its end, with why.
const (
	actionPersonHeld  = "held for a person"
	actionPersonEnded = "person hold ended"
)

// outOfReachPathMax and outOfReachFixMax bound an answer's path, which the report refuses past it, and its fix, which
// the hold's note and record clip, so the record stays one readable case log line.
const (
	outOfReachPathMax = 200
	outOfReachFixMax  = 600
)

// personRecord is the detail of a "held for a person" line.
type personRecord struct {
	Path     string `json:"path"`
	Fix      string `json:"fix"`
	Episode  int64  `json:"episode"`
	Evidence int    `json:"evidence"` // how many evidence refs the case held at the build that answered
}

// personNoteID is the note of a case held for a person.
func personNoteID(caseID string) string { return "loop-build:" + store.IDPart(caseID) + ":person" }

// outOfReachReport checks a report that answers its fix is out of reach: its summary and the answer are what the
// host reads, and the change's fields are not.
func outOfReachReport(report BuildReport) (BuildReport, error) {
	if strings.TrimSpace(report.Summary) == "" {
		return report, errors.New("build report has an empty required field")
	}
	o := report.OutOfReach
	name := strings.TrimSuffix(o.Path, "/")
	if name == "" || plain.Line(name) != name || utf8.RuneCountInString(name) > outOfReachPathMax || !filepath.IsLocal(name) ||
		path.Clean(name) != name || strings.Contains(name, `\`) {
		return report, refused("the out-of-reach path %q is no path of the repository: name it from the repository's root, such as .github/workflows/ci.yml", plainClip(o.Path, 120))
	}
	if strings.TrimSpace(o.Fix) == "" {
		return report, refused("the out-of-reach answer has no fix: say the change a person makes in %s", name)
	}
	o.Path = name
	report.Installed, report.Lesson = "", lessonLine(report.Lesson)
	return report, nil
}

// outOfReach is a build's out-of-reach answer, after its authority check: a path PathScope refuses holds the case for
// a person (holdForPerson) and the run ends as answered; one a change may touch is the build's refusal at its report.
// A hold the store cannot record is the host's error at the proposal.
func (r *Runner) outOfReach(ctx context.Context, c store.Case, run store.LoopRun, res Result, p Package, o OutOfReach) (Result, error) {
	_, scope := PathScope(Target{Repo: p.Repo}, []string{o.Path})
	if scope == nil {
		return stageFailureIn(ctx, res, "build report", refused("the out-of-reach path %s is one a change may touch: make the fix there", o.Path))
	}
	if err := r.holdForPerson(ctx, c, run.CaseEpisode, o, scope); err != nil {
		return stageFailureIn(ctx, res, "build propose", err)
	}
	r.logf("build %s: its fix needs %s, which no change may touch: the case waits for a person", c.ID, o.Path)
	return res, nil
}

// holdForPerson holds a case for a person: its raised problem note first, naming the path and the fix, then the case
// log's "held for a person" line, which stands as the hold. c is the case as the build was picked, so evidence that came
// in during the build ends the hold.
func (r *Runner) holdForPerson(ctx context.Context, c store.Case, episode int64, o OutOfReach, why error) error {
	texts, err := RedactAll(ctx, []string{o.Path, o.Fix, strings.TrimPrefix(why.Error(), ErrRefused.Error()+": ")})
	if err != nil {
		return err
	}
	where, fix, reason := plain.Line(texts[0]), plainClip(plain.Line(texts[1]), outOfReachFixMax), plain.Line(texts[2])
	n := store.Note{ID: personNoteID(c.ID), Kind: store.NoteProblem, Importance: store.ImportanceNormal, At: r.now(),
		Title: "The loop's build for case " + c.ID + " needs a person: " + where,
		Detail: "Its fix is out of the loop's reach: " + reason + ". The change to make in " + where + ": " + sentence(fix) +
			" Make it and close the case. New evidence, or sending the case back to its investigation, ends the hold."}
	if _, err := r.Store.PutNote(ctx, n); err != nil {
		return err
	}
	var detail bytes.Buffer
	enc := json.NewEncoder(&detail)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(personRecord{Path: where, Fix: fix, Episode: episode, Evidence: c.Evidence}); err != nil {
		return err
	}
	return r.Store.CaseNote(cleanedNote(ctx), c.ID, actionPersonHeld, strings.TrimSpace(detail.String()), "loop")
}

// personHoldOf is the hold for a person of a case in testing, from its log: its newest "held for a person" line of the
// case's current episode with no end after it.
func personHoldOf(c store.Case, log []store.CaseEntry) (personRecord, bool) {
	if c.Status != store.CaseTesting {
		return personRecord{}, false
	}
	for i := len(log) - 1; i >= 0; i-- {
		switch log[i].Action {
		case actionPersonEnded:
			return personRecord{}, false
		case actionPersonHeld:
			var h personRecord
			if json.Unmarshal([]byte(log[i].Detail), &h) != nil || h.Episode != c.Episode || h.Path == "" {
				return personRecord{}, false
			}
			return h, true
		}
	}
	return personRecord{}, false
}

// heldForPerson is the path a case in testing waits on a person for, or "", and the error of a log it cannot read. A
// hold the case's new evidence ended gets its end, and its note is resolved; one whose end cannot be written is logged
// and ends all the same, and the next pass writes it. A dry pass writes nothing.
func (r *Runner) heldForPerson(ctx context.Context, c store.Case, dry bool) (string, error) {
	log, err := r.Store.CaseLog(ctx, c.ID)
	if err != nil {
		return "", err
	}
	h, ok := personHoldOf(c, log)
	switch {
	case !ok:
		return "", nil
	case c.Evidence <= h.Evidence:
		return h.Path, nil
	case dry:
		return "", nil
	}
	// the note goes first: the hold's line stands until its end is written
	if err := r.endPersonHold(ctx, c.ID, "new evidence came in"); err != nil {
		r.logf("the person hold of %s: %v", c.ID, err)
	}
	return "", nil
}

// endPersonHold resolves a case's note of its hold for a person, then writes the hold's end with why.
func (r *Runner) endPersonHold(ctx context.Context, caseID, why string) error {
	if _, _, err := r.Store.DoneNote(ctx, personNoteID(caseID), r.now()); err != nil && !errors.Is(err, store.ErrNoNote) {
		return err
	}
	return r.Store.CaseNote(cleanedNote(ctx), caseID, actionPersonEnded, why, "loop")
}

// sentence is text that ends as a sentence does.
func sentence(text string) string {
	if strings.HasSuffix(text, ".") || strings.HasSuffix(text, "!") || strings.HasSuffix(text, "?") || strings.HasSuffix(text, "…") {
		return text
	}
	return text + "."
}
