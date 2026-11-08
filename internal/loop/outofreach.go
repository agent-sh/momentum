package loop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
)

// A build whose case's fix needs a path no change of the loop's may touch answers so instead of making a change: the
// path and the change a person makes there (out_of_reach). The host checks the path against BASE and sorts it: one no
// change may touch (PathScope refuses it, or the patch check would: Git's own directory, a path through a symlink or a
// submodule of BASE) holds the case for a person, with a "held for a person" line in its log and a raised problem note
// that names the path and the fix. The run keeps its own outcome (store.RunOutOfReach), which the stop rule neither
// counts nor stops at, and a held case takes no build slot. The hold stands until the case starts a new episode,
// leaves testing, or a person changes its status, mechanism or fix (store.PersonHoldOf) or resolves its note; new
// evidence alone does not end it. A path a change may touch, and one that is no path of the repository at BASE, are the build's refusals at
// its report.

// OutOfReach is a build report's answer that the case's fix is out of the loop's reach: the path, from the
// repository's root, and the change a person makes there.
type OutOfReach struct {
	Path string `json:"path"`
	Fix  string `json:"fix"`
}

const outOfReachSchema = `{"type":"object","additionalProperties":false,"required":["path","fix"],` +
	`"description":"Only when the case's fix needs a path no change may touch (the build prompt names them) and no change in reach fixes the case: make no change and give this instead. The host reads only summary and this answer, checks the path against BASE, and the case waits for a person. A path a change may touch, or one BASE does not hold, refuses the build.",` +
	`"properties":{"path":{"type":"string","description":"The path from the repository's root, such as .github/workflows/ci.yml: one BASE holds, or a new file in a directory BASE holds."},` +
	`"fix":{"type":"string","description":"The change a person makes there, in short sentences."}}}`

// outOfReachBrief tells a build it may answer that its fix is out of reach.
const outOfReachBrief = "When the case's fix needs a path no change may touch and nothing in reach fixes the case, make no change and answer out_of_reach: " +
	"the path from the repository's root, one BASE holds or a new file in a directory it holds, and the change a person makes there. " +
	"The host checks the path, and the case waits for a person. A path a change may touch, or one BASE does not hold, refuses the build."

// outOfReachPathMax bounds an answer's path, which the report refuses past it; outOfReachFixMax and
// outOfReachReasonMax clip the fix and the reason the hold keeps, so its record stays one readable case log line.
const (
	outOfReachPathMax   = 200
	outOfReachFixMax    = 350
	outOfReachReasonMax = 300
)

// personNotePrefix begins the ids of a case's notes of its holds for a person, one per path.
func personNotePrefix(caseID string) string { return "loop-build:" + store.IDPart(caseID) + ":person:" }

func personNoteID(caseID, where string) string {
	sum := sha256.Sum256([]byte(where))
	return personNotePrefix(caseID) + hex.EncodeToString(sum[:])
}

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

// outOfReach is a build's out-of-reach answer, after its authority check: a path no change may touch holds the case
// for a person (holdForPerson) and the run keeps its own outcome; one a change may touch, or one BASE does not hold, is
// the build's refusal at its report, as is any answer of a configuration build, which changes its one file. BASE that cannot be read is the host's error at the check, and a hold the store
// cannot record the host's error at the proposal.
func (r *Runner) outOfReach(ctx context.Context, c store.Case, run store.LoopRun, res Result, p Package, o OutOfReach) (Result, error) {
	if p.Kind == KindConfig {
		return stageFailureIn(ctx, res, "build report", refused("a configuration build changes its file and answers no out-of-reach path"))
	}
	why, refusal, err := r.unreachable(ctx, p, o.Path)
	switch {
	case err != nil:
		return stageFailureIn(ctx, res, "build check", err)
	case refusal != nil:
		return stageFailureIn(ctx, res, "build report", refusal)
	case why == nil:
		return stageFailureIn(ctx, res, "build report", refused("the out-of-reach path %s is one a change may touch: make the fix there", o.Path))
	}
	if err := r.holdForPerson(ctx, c, run.CaseEpisode, o, why); err != nil {
		return stageFailureIn(ctx, res, "build propose", err)
	}
	r.logf("build %s: its fix needs %s, which no change may touch: the case waits for a person", c.ID, o.Path)
	res.Status = store.RunOutOfReach
	return res, nil
}

// unreachable sorts an out-of-reach answer's path, name. why says why no change of the loop's may touch it: PathScope
// refuses it, or the patch check would (a component that is Git's own directory, a path through a symlink or a
// submodule of BASE, which the patch check refuses: checkBasePaths, patchPath); it is nil when a change may. refusal
// says name is no path of the repository at BASE: BASE holds no such file or directory, it is no new file in a
// directory BASE holds, and it is under no symlink or submodule there; a path in Git's own directory is read in the
// checkout's. err is the host's error reading BASE.
func (r *Runner) unreachable(ctx context.Context, p Package, name string) (why, refusal, err error) {
	if p.Host == "" || p.BaseCommit == "" {
		return nil, nil, errors.New("the package has no host clone and base commit; prepare it again")
	}
	base, err := r.Ship.treeEntries(r.Ship.gitContext(shipContext(ctx, p.Case)), p, p.BaseCommit)
	if err != nil {
		return nil, nil, fmt.Errorf("the base tree cannot be read: %v", err) // the host's, never a refusal of the build
	}
	parts := strings.Split(name, "/")
	gitDir := false
	for _, part := range parts {
		gitDir = gitDir || strings.EqualFold(part, ".git")
	}
	held := inBase(base, name)
	if !held && strings.EqualFold(parts[0], ".git") {
		if held, err = inGitDir(ctx, p.Dir, name); err != nil {
			return nil, nil, err
		}
	}
	if !held {
		return nil, refused("the out-of-reach path %s is no path of the repository at BASE, nor a new file in a directory BASE holds: name the path the fix needs", name), nil
	}
	if _, scope := PathScope(Target{Repo: p.Repo}, []string{name}); scope != nil {
		return scope, nil, nil
	}
	if gitDir {
		return refused("%s is in Git's own directory, which no change touches", name), nil, nil
	}
	if checkBasePaths(base, []string{name}) != nil {
		return refused("%s is a symlink or a submodule of the repository, or a path through one, which no change touches", name), nil, nil
	}
	return nil, nil, nil
}

// inBase says whether BASE's entries (Ship.treeEntries: its files, symlinks and submodules) hold name: as an entry, as
// a directory, as a new file in a directory they hold (the root among them), or under a symlink or a submodule, whose
// content BASE does not list.
func inBase(base map[string]treeEntry, name string) bool {
	if _, ok := base[name]; ok {
		return true
	}
	parts := strings.Split(name, "/")
	for i := 1; i < len(parts); i++ {
		if e, ok := base[strings.Join(parts[:i], "/")]; ok && (e.mode == "120000" || e.mode == "160000") {
			return true
		}
	}
	parent := path.Dir(name)
	for entry := range base {
		if strings.HasPrefix(entry, name+"/") || parent != "." && strings.HasPrefix(entry, parent+"/") {
			return true
		}
	}
	return parent == "."
}

// inGitDir says whether the checkout at dir holds name, a path in Git's own directory (its first part is .git), as
// BASE's rule reads it (inBase): every directory above it is a directory there, not a link, so name is an entry of that
// directory or a new file in it. Git's directory is the one git rev-parse names for the checkout (--git-dir), or the
// repository's common one (--git-common-dir), which a linked worktree's .git file points past. err is the host's
// error asking Git.
func inGitDir(ctx context.Context, dir, name string) (bool, error) {
	if dir == "" {
		return false, nil
	}
	out, err := hostInputGit(ctx, dir, "rev-parse", "--git-dir", "--git-common-dir")
	if err != nil {
		return false, fmt.Errorf("the checkout's git directory cannot be read: %v", err)
	}
	_, rest, inside := strings.Cut(name, "/")
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		gitDir := strings.TrimSpace(line)
		if gitDir == "" {
			continue
		}
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(dir, gitDir)
		}
		if fi, err := os.Lstat(gitDir); err != nil || !fi.IsDir() {
			continue
		}
		if !inside || dirsAbove(gitDir, rest) {
			return true, nil
		}
	}
	return false, nil
}

// dirsAbove says whether every directory above name, under root, is a directory there and not a link.
func dirsAbove(root, name string) bool {
	parts := strings.Split(name, "/")
	cur := root
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		if fi, err := os.Lstat(cur); err != nil || !fi.IsDir() {
			return false
		}
	}
	return true
}

// holdForPerson holds a case for a person: the case log's "held for a person" line first, which stands as the hold,
// then its raised problem note, naming the path and the fix. A note that cannot be put is logged: the next pass puts
// a standing hold's note when it has none (heldForPerson).
func (r *Runner) holdForPerson(ctx context.Context, c store.Case, episode int64, o OutOfReach, why error) error {
	texts, err := RedactAll(ctx, []string{o.Path, o.Fix, strings.TrimPrefix(why.Error(), ErrRefused.Error()+": ")})
	if err != nil {
		return err
	}
	h := store.NewPersonHold(c, plainClip(plain.Line(texts[0]), outOfReachPathMax), plainClip(plain.Line(texts[1]), outOfReachFixMax),
		plainClip(plain.Line(texts[2]), outOfReachReasonMax))
	h.Episode = episode
	if err := r.Store.CaseNote(cleanedNote(ctx), c.ID, store.ActionPersonHeld, store.PersonDetail(h), "loop"); err != nil {
		return err
	}
	if err := r.putPersonNote(ctx, c.ID, h); err != nil {
		r.logf("the person hold note of %s: %v; the next pass puts it", c.ID, err)
	}
	return nil
}

// putPersonNote puts the raised problem note of a case's hold for a person.
func (r *Runner) putPersonNote(ctx context.Context, caseID string, h store.PersonHold) error {
	_, err := r.Store.PutNote(ctx, store.Note{ID: personNoteID(caseID, h.Path), Kind: store.NoteProblem, Importance: store.ImportanceNormal,
		At: r.now(), Title: "The loop's build for case " + caseID + " needs a person: " + h.Path,
		Detail: "Its fix is out of the loop's reach: " + sentence(h.Reason) + " The change to make in " + h.Path + ": " + sentence(h.Fix) +
			" Make it and close the case. Resolving this note, changing the case's mechanism or fix, or sending it back to its investigation also ends the hold, and the case builds again; new evidence alone does not."})
	return err
}

// heldForPerson is the path a case in testing waits on a person for (store.PersonHoldOf), or "", and the error of a
// log it cannot read. A standing hold whose note was never put gets it. A hold that no longer stands (a new episode, a
// person's change of the case), and one whose note a person resolved, get their end, and their notes are resolved,
// before the case builds again; one whose end cannot be written is logged and ends all the same, and the next pass
// writes it. A dry pass writes nothing.
func (r *Runner) heldForPerson(ctx context.Context, c store.Case, dry bool) (string, error) {
	log, err := r.Store.CaseLog(ctx, c.ID)
	if err != nil {
		return "", err
	}
	last, open := store.LastPersonHold(log)
	if !open {
		return "", nil
	}
	if h, ok := store.PersonHoldOf(c, log); ok {
		n, had, err := r.Store.Note(ctx, personNoteID(c.ID, h.Path))
		switch {
		case err != nil:
			r.logf("the person hold note of %s: %v", c.ID, err)
		case had && !n.Done.IsZero():
			// a person who resolves the note is done waiting: the hold ends, and the case builds again
			if !dry {
				if err := r.endPersonHold(ctx, c.ID, "a person resolved its note"); err != nil {
					r.logf("the person hold of %s: %v", c.ID, err)
				}
			}
			return "", nil
		case !had && !dry:
			if err := r.putPersonNote(ctx, c.ID, h); err != nil {
				r.logf("the person hold note of %s: %v", c.ID, err)
			}
		}
		return h.Path, nil
	}
	if dry {
		return "", nil
	}
	why := "a person changed the case's status, mechanism or fix"
	if last.Episode != c.Episode {
		why = "the case is in a new episode"
	}
	if err := r.endPersonHold(ctx, c.ID, why); err != nil {
		r.logf("the person hold of %s: %v", c.ID, err)
	}
	return "", nil
}

// endPersonHold resolves a case's standing notes of its holds for a person, then writes the hold's end with why.
func (r *Runner) endPersonHold(ctx context.Context, caseID, why string) error {
	notes, err := r.Store.Notes(ctx)
	if err != nil {
		return err
	}
	for _, n := range notes {
		if n.Done.IsZero() && strings.HasPrefix(n.ID, personNotePrefix(caseID)) {
			if _, _, err := r.Store.DoneNote(ctx, n.ID, r.now()); err != nil {
				return err
			}
		}
	}
	return r.Store.CaseNote(cleanedNote(ctx), caseID, store.ActionPersonEnded, why, "loop")
}

// sentence is text that ends as a sentence does.
func sentence(text string) string {
	if strings.HasSuffix(text, ".") || strings.HasSuffix(text, "!") || strings.HasSuffix(text, "?") || strings.HasSuffix(text, "…") {
		return text
	}
	return text + "."
}
