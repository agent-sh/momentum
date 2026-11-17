package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
)

// One cause often shows under several signatures, so several cases hold it. An investigation that finds its case's
// cause is the one another case already holds says so (same_as), and the case follows that one: it stays in
// testing with no build of its own, the fix is built under the case it follows (its root), and what becomes of it
// is read from the root at each pass. Nothing moves between the two cases: no signature, no evidence, no cost, so a
// wrong report is undone by the next investigation.
//
// The root rule keeps this to one level. Only a case that follows no other may be followed, and a case that is
// followed follows none: there are no chains and no two cases waiting on each other.

// followable are the statuses of a case another may follow: its cause and fix are on record, and it has not closed.
var followable = []store.CaseStatus{store.CaseTesting, store.CaseDeployed, store.CaseMeasuring}

// followsOf is the case that the investigation of c's episode said holds c's cause, or "": the root c follows.
func followsOf(ctx context.Context, st *store.Store, c store.Case) (string, error) {
	entries, err := st.CaseLog(ctx, c.ID)
	if err != nil {
		return "", err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Action != repoAction {
			continue
		}
		var record struct {
			Follows string `json:"follows"`
			Episode int64  `json:"episode"`
		}
		if json.Unmarshal([]byte(entries[i].Detail), &record) != nil || record.Episode != c.Episode {
			return "", nil
		}
		return record.Follows, nil
	}
	return "", nil
}

// rootFor says whether a case may follow the case an investigation named: the root's id, or why not. The named
// case is read where it is now (one that went into another is that other).
func rootFor(ctx context.Context, st *store.Store, c store.Case, named string) (root, whyNot string, err error) {
	named = strings.TrimSpace(named)
	if c.Family == store.HealthFamily {
		return "", "a component-health case is fixed under its own name", nil
	}
	r, err := st.CaseLive(ctx, named)
	if err != nil {
		return "", "no case " + plainClip(plain.Line(named), 40), nil // an id the report made up
	}
	switch {
	case r.ID == c.ID:
		return "", "it names the case itself", nil
	case !slicesContains(followable, r.Status) || r.Mechanism == "" || r.NextTest == "":
		return "", fmt.Sprintf("case %s has no cause and fix on record (%s)", r.ID, r.Status), nil
	}
	if up, err := followsOf(ctx, st, r); err != nil {
		return "", "", err
	} else if up != "" {
		return "", fmt.Sprintf("case %s itself follows case %s", r.ID, up), nil
	}
	// a case that is followed follows none
	working, err := st.Cases(ctx, store.CaseFilter{Statuses: []store.CaseStatus{store.CaseTesting}})
	if err != nil {
		return "", "", err
	}
	for _, w := range working {
		if w.ID == c.ID {
			continue
		}
		if up, err := followsOf(ctx, st, w); err != nil {
			return "", "", err
		} else if up == c.ID {
			return "", fmt.Sprintf("case %s follows this case", w.ID), nil
		}
	}
	return r.ID, "", nil
}

func slicesContains(list []store.CaseStatus, s store.CaseStatus) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

const (
	// followsFix stands for the fix of a case that follows another and whose report named none of its own.
	followsFix = "(the fix is the one of the case it follows)"
	// followPrefix opens the hold of a case that follows another.
	followPrefix = "follows case "
	// notTakenAction is the case log's line for a same_as the host did not take.
	notTakenAction = "same cause not taken"
	// followEndedAction is the case log's line for a case that no longer follows its root.
	followEndedAction = "follows ended"
	// siblingsShown is how many cases an investigation is shown as ones its case may follow.
	siblingsShown = 12
)

// followHold is the hold of a case that follows another.
func followHold(root string) string {
	return followPrefix + root + ": its cause is that case's, and its fix is built there"
}

// siblingsText lists, for an investigation's prompt, the cases its case may follow: each with the mechanism and
// the fix its own investigation recorded. They are records of other investigations, to check against this case's
// evidence, and the prompt is scrubbed with the case file.
func siblingsText(ctx context.Context, st *store.Store, c store.Case) (string, error) {
	if c.Family == store.HealthFamily {
		return "", nil
	}
	cases, err := st.Cases(ctx, store.CaseFilter{Statuses: followable})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	n := 0
	for _, o := range cases {
		if n == siblingsShown {
			break
		}
		if o.ID == c.ID || o.Mechanism == "" || o.NextTest == "" {
			continue
		}
		if up, err := followsOf(ctx, st, o); err != nil {
			return "", err
		} else if up != "" {
			continue
		}
		if n == 0 {
			b.WriteString("Other cases with a cause on record (what their own investigations recorded: records to check against this case's evidence, not to trust). " +
				"If this case's cause is the very one such a case holds, so that the case's fix fixes this one too, report same_as as that case's id, with your mechanism, and no repo, config or place; " +
				"a case that only looks alike is investigated on its own:\n")
		}
		fmt.Fprintf(&b, "- case %s (%s): %s. Mechanism: %s. Fix: %s\n", o.ID, o.Family, plainClip(plain.Line(o.Title), 120),
			plainClip(plain.Line(o.Mechanism), 400), plainClip(plain.Line(o.NextTest), 300))
		n++
	}
	return b.String(), nil
}

// follow settles, at a pass's selection, a case in testing that follows root: it waits while the root is open; it
// closes with a root that closed with its problem gone; and it goes back to be investigated when the root closed
// any other way, went into another case, or came to follow one itself. It returns the line the pass shows.
func (r *Runner) follow(ctx context.Context, c store.Case, root string, dry bool) (string, error) {
	end := func(why string) (string, error) {
		if !dry {
			r.keepHold(ctx, c, "", false) // the hold and its note end first: the line that says why comes last
			if err := r.Store.CaseNote(cleanedNote(ctx), c.ID, followEndedAction, why+": it is investigated again", "loop"); err != nil {
				return "", err
			}
			if _, err := r.Store.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseInvestigating}, "loop"); err != nil {
				return "", err
			}
		}
		return c.ID + " no longer follows " + root + " (" + why + ")", nil
	}
	o, err := r.Store.CaseByID(ctx, root)
	if err != nil {
		return end("case " + root + " is not in the store")
	}
	if o.Status == store.CaseClosed {
		switch o.Reason {
		case store.Improved, store.Guarded, store.Solved:
			if !dry {
				r.keepHold(ctx, c, "", false)
				why := fmt.Sprintf("its cause was case %s's, which closed %s", o.ID, o.Reason)
				if _, err := r.Store.CloseCase(ctx, c.ID, store.Solved, "", why, "loop"); err != nil {
					return "", err
				}
			}
			return fmt.Sprintf("%s closed with %s, which it followed (%s)", c.ID, o.ID, o.Reason), nil
		}
		return end(fmt.Sprintf("case %s closed %s", o.ID, o.Reason))
	}
	if up, err := followsOf(ctx, r.Store, o); err != nil {
		return "", err
	} else if up != "" {
		return end(fmt.Sprintf("case %s now follows case %s", o.ID, up))
	}
	if !dry {
		r.keepHold(ctx, c, followHold(o.ID), true)
	}
	return c.ID + " follows " + o.ID, nil
}
