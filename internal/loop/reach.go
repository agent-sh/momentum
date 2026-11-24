package loop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/agent-sh/momentum/internal/store"
)

// An investigation that places a case's fix in paths of the loop's own repository that no build may change names no
// repo, only the place, and the case waits for a person (repoHold). The paths a build may change grow: a path that was
// out of reach when the investigation ran can be in reach now. Such a case's record still names no repo, so it would
// wait for good. At a pass's selection, a case that waits on a place in the loop's own repository whose every path a
// build may change now goes back to be investigated, once an episode: the investigation reads today's reach and names
// the repository. A place that names anything else (another repository, a path outside the repository, a path no
// build may change) keeps the wait.

// reachAgainAction is the case log's line for such a case, with the place and the episode (reachRecord).
const reachAgainAction = "in reach again"

type reachRecord struct {
	Place   string `json:"place"`
	Episode int64  `json:"episode"`
}

// selfTops are the first parts of the paths of the loop's own repository a place may name for a build to change.
var selfTops = []string{"internal", "sessions"}

// pathExts mark a word as a file, with no slash a root file of the repository.
var pathExts = []string{".go", ".md", ".mod", ".sum", ".work", ".json", ".yml", ".yaml", ".toml", ".sh", ".ts", ".tsx", ".py", ".txt"}

// reachDigest names the paths no build may change (ProposalProtected), as an investigation was told them: a record
// made under the same list is not read again.
func reachDigest() string {
	sum := sha256.Sum256([]byte(strings.Join(ProposalProtected(), "\n")))
	return hex.EncodeToString(sum[:8])
}

// pathWord is a path as a place may name it: plain path characters, and a line or a line range after it.
var pathWord = regexp.MustCompile(`^([A-Za-z0-9._/-]+?)(?::\d+(?:[:-]\d+)?|#L\d+(?:-L?\d+)?)?$`)

// ownPlaceInReach says whether a place an investigation named for a fix is in the loop's own repository and every
// path it names there is one a build may change now: one at least, under the repository's own directories (or
// README.md), and none that no change may touch (proposalProtected), nor a directory that holds one. A path may have a
// line after it (a colon and its number, a #L anchor). A place cut short (placeMax), a word with anything else in
// it (a URL, a glob, a home, a pipe), an absolute path or a step up, a path under any other first part (which may be
// another repository), another root file, and a bare name of what no change may touch (cmd, vendor, go.mod) are not.
func ownPlaceInReach(place string) bool {
	if !strings.Contains(strings.ToLower(place), strings.ToLower(SelfRepo)) || strings.HasSuffix(place, "…") || utf8.RuneCountInString(place) >= placeMax {
		return false
	}
	bare := map[string]bool{}
	for _, p := range ProposalProtected() {
		top, _, _ := strings.Cut(strings.ToLower(p), "/")
		bare[top] = true
	}
	paths := 0
	for _, w := range strings.FieldsFunc(place, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune("(),;`'\"[]{}<>", r) }) {
		w = strings.TrimRight(w, ".,:")
		if w == "" || strings.EqualFold(w, SelfRepo) {
			continue
		}
		if bare[strings.ToLower(strings.TrimSuffix(w, "/"))] {
			return false // a bare name of a directory or root file no change may touch, or one that holds such paths
		}
		if !strings.ContainsAny(w, "/:#*?~$\\|+&=@%") && !hasPathExt(w) {
			continue // a word, not a path
		}
		m := pathWord.FindStringSubmatch(w)
		if m == nil {
			return false // a URL, a glob, a home, two paths in one word
		}
		w = m[1]
		if strings.HasPrefix(w, "/") || strings.HasPrefix(w, ".") || strings.Contains(w, "..") || strings.Contains(w, "//") {
			return false // outside the repository
		}
		top, _, nested := strings.Cut(w, "/")
		if !nested {
			if w != "README.md" {
				return false // another root file: no change edits one but README.md
			}
		} else if !slices.Contains(selfTops, top) {
			return false
		}
		if proposalProtected(w) || proposalProtected(w+".go") {
			return false
		}
		if !hasPathExt(w) { // a directory: none of what it holds may be out of reach
			dir := strings.TrimSuffix(w, "/") + "/"
			for _, p := range ProposalProtected() {
				if strings.HasPrefix(strings.ToLower(p), strings.ToLower(dir)) {
					return false
				}
			}
		}
		paths++
	}
	return paths > 0
}

// hasPathExt says whether a word's last part ends as a file's name does.
func hasPathExt(w string) bool {
	last := strings.ToLower(w[strings.LastIndex(w, "/")+1:])
	for _, ext := range pathExts {
		if strings.HasSuffix(last, ext) {
			return true
		}
	}
	return false
}

// reachAgain sends back to be investigated a case in testing whose investigation named no repo and placed the fix in
// the loop's own repository, in paths a build may change now (ownPlaceInReach), when the investigation was told
// another reach (reachDigest) and the case has room to run (not out of budget, not stopped): once in the case's
// episode. It reports whether it did; a dry pass writes nothing.
func (r *Runner) reachAgain(ctx context.Context, c store.Case, place string, room, dry bool) (bool, error) {
	if !room || !ownPlaceInReach(place) {
		return false, nil
	}
	log, err := r.Store.CaseLog(ctx, c.ID)
	if err != nil {
		return false, err
	}
	for i := len(log) - 1; i >= 0; i-- { // the record the place came from
		if log[i].Action != repoAction {
			continue
		}
		var rec struct {
			Reach string `json:"reach"`
		}
		if json.Unmarshal([]byte(log[i].Detail), &rec) == nil && rec.Reach == reachDigest() {
			return false, nil // its investigation was told today's reach
		}
		break
	}
	for _, e := range log {
		var rec reachRecord
		if e.Action == reachAgainAction && json.Unmarshal([]byte(e.Detail), &rec) == nil && rec.Episode == c.Episode {
			return false, nil // it went back once this episode already
		}
	}
	if dry {
		return true, nil
	}
	// the case moves first: its line is what keeps it from going back twice, so a move that failed leaves none
	if _, err := r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseInvestigating}, "loop"); err != nil {
		return false, err
	}
	raw, _ := json.Marshal(reachRecord{Place: place, Episode: c.Episode})
	if err := r.Store.CaseNote(cleanedNote(ctx), c.ID, reachAgainAction, string(raw), "loop"); err != nil {
		return false, err
	}
	// the hold ends with a line that says why, and its notes with it: the case waits for nothing now
	if err := r.doneHoldNotes(ctx, c.ID); err != nil {
		return false, err
	}
	if err := r.Store.CaseNote(cleanedNote(ctx), c.ID, store.ActionUnheld, "its fix's place is in the loop's reach now: it is investigated again", "loop"); err != nil {
		return false, err
	}
	return true, nil
}
