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
// change may touch (PathScope refuses it, or the patch check would: Git's own directory, a symlink or a submodule of
// BASE) holds the case for a person, with a "held for a person" line in its log and a raised problem note
// that names the path and the fix. The run keeps its own outcome (store.RunOutOfReach), which the stop rule neither
// counts nor stops at, and a held case takes no build slot. The hold stands until the case starts a new episode,
// leaves testing, or a person changes its status (after the build started), mechanism or fix (store.PersonHoldOf) or
// resolves its note; new evidence alone does not end it. A path a change may touch, and one that is no path of the
// repository at BASE (a made-up path, a new file at the root, one under a submodule or a symlink out of the
// repository), are the build's refusals at its report; a path through a symlink inside the repository is read as
// its target.

// OutOfReach is a build report's answer that the case's fix is out of the loop's reach: the path, from the
// repository's root, and the change a person makes there.
type OutOfReach struct {
	Path string `json:"path"`
	Fix  string `json:"fix"`
}

const outOfReachSchema = `{"type":"object","additionalProperties":false,"required":["path","fix"],` +
	`"description":"Only when the case's fix needs a path no change may touch (the build prompt names them) and no change in reach fixes the case: make no change and give this instead. The host reads only summary and this answer, checks the path against BASE, and the case waits for a person. A path a change may touch, or one BASE does not hold, refuses the build.",` +
	`"properties":{"path":{"type":"string","description":"The path from the repository's root, such as .github/workflows/ci.yml: one BASE holds, or a new file in a directory below the root BASE holds."},` +
	`"fix":{"type":"string","description":"The change a person makes there, in short sentences."}}}`

// outOfReachBrief tells a build it may answer that its fix is out of reach.
const outOfReachBrief = "When the case's fix needs a path no change may touch and nothing in reach fixes the case, make no change and answer out_of_reach: " +
	"the path from the repository's root, one BASE holds or a new file in a directory below the root it holds, and the change a person makes there. " +
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
	if err := r.holdForPerson(ctx, c, run, o, why); err != nil {
		return stageFailureIn(ctx, res, "build propose", err)
	}
	r.logf("build %s: its fix needs %s, which no change may touch: the case waits for a person", c.ID, o.Path)
	res.Status = store.RunOutOfReach
	return res, nil
}

// unreachable sorts an out-of-reach answer's path, name, read through BASE's symlinks that point inside the
// repository (throughLinks). why says why no change of the loop's may touch it: PathScope refuses it, or the patch
// check would (a component that is Git's own directory, a symlink or a submodule of BASE, which the patch check
// refuses: checkBasePaths, patchPath); it is nil when a change may. refusal says name is no path of the repository at
// BASE (BASE holds no such file or directory, it is no new file in a directory below the root BASE holds, or it is
// under a submodule or a symlink out of the repository, whose content BASE does not hold), or that it reads through a
// symlink as a path a change may touch; a path in Git's own directory is read in the checkout's. err is the host's
// error reading BASE.
func (r *Runner) unreachable(ctx context.Context, p Package, name string) (why, refusal, err error) {
	if p.Host == "" || p.BaseCommit == "" {
		return nil, nil, errors.New("the package has no host clone and base commit; prepare it again")
	}
	gctx := r.Ship.gitContext(shipContext(ctx, p.Case))
	base, err := r.Ship.treeEntries(gctx, p, p.BaseCommit)
	if err != nil {
		return nil, nil, fmt.Errorf("the base tree cannot be read: %v", err) // the host's, never a refusal of the build
	}
	real, ok, err := r.throughLinks(gctx, p, base, name)
	if err != nil {
		return nil, nil, fmt.Errorf("the base tree cannot be read: %v", err)
	}
	if !ok {
		return nil, refused("the out-of-reach path %s is under a submodule or a symlink out of the repository at BASE, whose content BASE does not hold: name the submodule or the symlink itself", name), nil
	}
	gitDir := false
	for _, part := range strings.Split(real, "/") {
		gitDir = gitDir || strings.EqualFold(part, ".git")
	}
	held := inBase(base, real)
	if !held && strings.EqualFold(strings.Split(real, "/")[0], ".git") {
		if held, err = inGitDir(ctx, p.Dir, real); err != nil {
			return nil, nil, err
		}
	}
	if !held {
		return nil, refused("the out-of-reach path %s is no path of the repository at BASE, nor a new file in a directory below the root BASE holds: name the path the fix needs", name), nil
	}
	if _, scope := PathScope(Target{Repo: p.Repo}, []string{real}); scope != nil {
		return scope, nil, nil
	}
	if gitDir {
		return refused("%s is in Git's own directory, which no change touches", real), nil, nil
	}
	if checkBasePaths(base, []string{real}) != nil {
		return refused("%s is a symlink or a submodule of the repository, which no change touches", real), nil, nil
	}
	if real != name {
		return nil, refused("the out-of-reach path %s is %s through a symlink of BASE, which a change may touch: make the fix there", name, real), nil
	}
	return nil, nil, nil
}

// linkHops bounds the symlinks throughLinks follows for one path, as the kernel bounds a lookup's.
const linkHops = 40

// throughLinks reads name as BASE does: a directory above it that is a symlink of BASE pointing inside the
// repository is read as its target, link after link. ok is false when a directory above it is a submodule, or a
// symlink pointing out of the repository or past linkHops links, whose content BASE does not hold. A symlink or a
// submodule that is name itself stays, as the entry. err is the host's error reading a symlink's target.
func (r *Runner) throughLinks(ctx context.Context, p Package, base map[string]treeEntry, name string) (string, bool, error) {
	for hops := 0; hops <= linkHops; hops++ {
		parts := strings.Split(name, "/")
		link := ""
		for i := 1; i < len(parts) && link == ""; i++ {
			switch above := strings.Join(parts[:i], "/"); base[above].mode {
			case "160000":
				return "", false, nil
			case "120000":
				link = above
			}
		}
		if link == "" {
			return name, true, nil
		}
		target, err := r.Ship.git(ctx, p.Host, "cat-file", "blob", base[link].oid)
		if err != nil {
			return "", false, err
		}
		next := path.Join(path.Dir(link), target, strings.TrimPrefix(name, link+"/"))
		if target == "" || path.IsAbs(target) || !filepath.IsLocal(next) || strings.Contains(next, `\`) {
			return "", false, nil
		}
		name = next
	}
	return "", false, nil
}

// inBase says whether BASE's entries (Ship.treeEntries: its files, symlinks and submodules) hold name, a path with no
// symlink or submodule above it (throughLinks): as an entry, as a directory, or as a new file in a directory below
// the root they hold. A new file at the root is no path of BASE: no directory there tells a fix's file from a made-up one.
func inBase(base map[string]treeEntry, name string) bool {
	if _, ok := base[name]; ok {
		return true
	}
	parent := path.Dir(name)
	for entry := range base {
		if strings.HasPrefix(entry, name+"/") || parent != "." && strings.HasPrefix(entry, parent+"/") {
			return true
		}
	}
	return false
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
// then its raised problem note, naming the path and the fix. The hold keeps the episode and start of the build that
// answered, run, so a person's change of the case's status while it ran ends it (store.PersonHoldOf). A note that
// cannot be put is logged: the next pass puts a standing hold's note when it has none (heldForPerson).
func (r *Runner) holdForPerson(ctx context.Context, c store.Case, run store.LoopRun, o OutOfReach, why error) error {
	texts, err := RedactAll(ctx, []string{o.Path, o.Fix, strings.TrimPrefix(why.Error(), ErrRefused.Error()+": ")})
	if err != nil {
		return err
	}
	h := store.NewPersonHold(c, plainClip(plain.Line(texts[0]), outOfReachPathMax), plainClip(plain.Line(texts[1]), outOfReachFixMax),
		plainClip(plain.Line(texts[2]), outOfReachReasonMax))
	h.Episode, h.Started = run.CaseEpisode, run.Started.UnixMilli()
	if err := r.Store.CaseNote(cleanedNote(ctx), c.ID, store.ActionPersonHeld, store.PersonDetail(h), "loop"); err != nil {
		return err
	}
	if err := r.putPersonNote(ctx, c.ID, h); err != nil {
		r.logf("the person hold note of %s: %v; the next pass puts it", c.ID, err)
	}
	return nil
}

// putPersonNote puts the raised problem note of a case's hold for a person; an unread hold's says its line cannot be
// read.
func (r *Runner) putPersonNote(ctx context.Context, caseID string, h store.PersonHold) error {
	title := "The loop's build for case " + caseID + " needs a person: " + h.Path
	detail := "Its fix is out of the loop's reach: " + sentence(h.Reason) + " The change to make in " + h.Path + ": " + sentence(h.Fix) +
		" Make it and close the case. Resolving this note, changing the case's mechanism or fix, or sending it back to its investigation also ends the hold, and the case builds again; new evidence alone does not."
	if h.Unread {
		title = "The loop's build for case " + caseID + " needs a person: its hold line cannot be read"
		detail = "The case's log holds a hold for a person that the loop cannot read, so the case does not build. Read the build's answer in the case's log, make the change it names and close the case. " +
			"Resolving this note, changing the case, or sending it back to its investigation also ends the hold, and the case builds again; new evidence alone does not."
	}
	_, err := r.Store.PutNote(ctx, store.Note{ID: personNoteID(caseID, h.Path), Kind: store.NoteProblem, Importance: store.ImportanceNormal,
		At: r.now(), Title: title, Detail: detail})
	return err
}

// heldForPerson is why a case in testing waits on a person (store.PersonHoldOf, PersonHold.Why), or "", and the error
// of a log it cannot read. A standing hold whose note was never put gets it. A hold that no longer stands (a new
// episode, a person's change of the case), and one whose note a person resolved, get their end, and their notes are
// resolved, before the case builds again; one whose end cannot be written is logged and ends all the same, and the
// next pass writes it. A dry pass writes nothing.
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
		return h.Why(), nil
	}
	if dry {
		return "", nil
	}
	why := "a person changed the case's status, mechanism or fix"
	if !last.Unread && last.Episode != c.Episode {
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
