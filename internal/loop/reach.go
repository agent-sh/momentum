package loop

import (
	"context"
	"encoding/json"
	"strings"
	"unicode"

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
var selfTops = []string{"internal", "sessions", "cmd"}

// pathExts mark a word with no slash as a file: a root file of the repository.
var pathExts = []string{".go", ".md", ".mod", ".sum", ".work", ".json", ".yml", ".yaml", ".toml", ".sh", ".ts", ".tsx", ".py"}

// ownPlaceInReach says whether a place an investigation named for a fix is in the loop's own repository and every
// path it names there is one a build may change now: one at least, under the repository's own directories (or
// README.md), and none that no change may touch (proposalProtected). A path outside the repository (a home or an
// absolute path) or under any other first part (which may be another repository) is not.
func ownPlaceInReach(place string) bool {
	if !strings.Contains(strings.ToLower(place), strings.ToLower(SelfRepo)) {
		return false
	}
	paths := 0
	for _, w := range strings.FieldsFunc(place, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune("(),;`'\"[]{}<>", r) }) {
		w = strings.TrimRight(w, ".,:;")
		if w == "" || strings.EqualFold(w, SelfRepo) {
			continue
		}
		file := false
		for _, ext := range pathExts {
			file = file || strings.HasSuffix(strings.ToLower(w), ext)
		}
		if !strings.Contains(w, "/") && !file {
			continue // a word, not a path
		}
		if strings.HasPrefix(w, "~") || strings.HasPrefix(w, "/") || strings.HasPrefix(w, "$") || strings.HasPrefix(w, ".") {
			return false // a path outside the repository
		}
		top, _, nested := strings.Cut(w, "/")
		switch {
		case !nested && w == "README.md":
		case !nested:
			return false // another root file: no change creates or edits one but README.md
		default:
			known := false
			for _, t := range selfTops {
				known = known || top == t
			}
			if !known {
				return false
			}
		}
		if proposalProtected(w) {
			return false
		}
		paths++
	}
	return paths > 0
}

// reachAgain sends back to be investigated a case in testing whose investigation named no repo and placed the fix in
// the loop's own repository, in paths a build may change now (ownPlaceInReach), once in the case's episode. It reports
// whether it did; a dry pass writes nothing.
func (r *Runner) reachAgain(ctx context.Context, c store.Case, place string, dry bool) (bool, error) {
	if !ownPlaceInReach(place) {
		return false, nil
	}
	log, err := r.Store.CaseLog(ctx, c.ID)
	if err != nil {
		return false, err
	}
	for _, e := range log {
		var rec reachRecord
		if e.Action == reachAgainAction && json.Unmarshal([]byte(e.Detail), &rec) == nil && rec.Episode == c.Episode {
			return false, nil // its investigation read today's reach already, and named no repo
		}
	}
	if dry {
		return true, nil
	}
	raw, _ := json.Marshal(reachRecord{Place: place, Episode: c.Episode})
	// the case moves first: a hold lifted over a case that then stayed would show a wait with no reason
	if err := r.Store.CaseNote(cleanedNote(ctx), c.ID, reachAgainAction, string(raw), "loop"); err != nil {
		return false, err
	}
	if _, err := r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseInvestigating}, "loop"); err != nil {
		return false, err
	}
	r.keepHold(ctx, c, "", false)
	return true, nil
}
