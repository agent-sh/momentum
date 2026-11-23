package detect

import (
	"context"
	"errors"
	"sort"

	"github.com/agent-sh/momentum/internal/store"
)

// Keyed is one event a detector keys, with the signature it keys it by and how many times it counts (an incident
// sighting's repeats; 1 otherwise): what a run of the detector counts, before any threshold or evidence bound.
type Keyed struct {
	Event     store.Event
	Signature string
	Weight    int
}

// Replayable are the families whose detectors read the store alone, so a run over a copy of the store keys what a
// run over the store keys. The sessions detector also reads the transcripts and Hermes's database, which a copy
// does not hold.
var Replayable = []string{"failures", "owner-burden"}

// KeyedEvents are the events the replayable detectors key over a window, each with its signature, ordered by time
// and then by event id. Programs is the owner-burden detector's program prefixes (OwnerBurden.Programs). A detector
// that fails partway returns what it keyed with its error, and the other detector's events all the same.
func KeyedEvents(ctx context.Context, st *store.Store, w Window, programs Programs) ([]Keyed, error) {
	occ, inc, ferr := Failures{}.sightings(ctx, st, w)
	owner, oerr := OwnerBurden{Programs: programs}.sightings(ctx, st, w)
	var out []Keyed
	for _, o := range append(append(occ, inc...), owner...) {
		out = append(out, Keyed{Event: o.ev, Signature: o.sig, Weight: max(o.weight, 1)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Event.TS.Equal(out[j].Event.TS) {
			return out[i].Event.TS.Before(out[j].Event.TS)
		}
		return out[i].Event.ID() < out[j].Event.ID()
	})
	return out, errors.Join(ferr, oerr)
}
