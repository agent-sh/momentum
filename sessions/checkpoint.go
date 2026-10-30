package sessions

import "context"

// Checkpoint is one tracker's place, kept so that a run in another process resumes it instead of reading the
// transcript again. State is where the tracker stands and what it derived from what it read, in the tracker's
// own encoding; Log is its activity log, which a store keeps entry by entry (Log.Unsaved, Log.Saved).
//
// A checkpoint holds only what the transcript decided, never what a scan derived from the clock or from other
// files: a tracker resumed from one and fed the rest of the transcript ends where a full re-read ends. A tracker
// checks that its transcript is still the one it read before it resumes, and starts over when it is not. A
// checkpoint is only valid for the code that wrote it, so a caller that keeps them across upgrades drops them when
// its build changes, as momentum does.
type Checkpoint struct {
	Tool  string // the Source's tool
	Key   string // what the tracker follows: a transcript's path, or a session's key for a database log
	State []byte
	Log   *Log // the tracker's live log: a store calls Saved once it holds every entry
}

// Saved is where a Source finds the checkpoints an earlier run kept.
type Saved interface {
	// Checkpoint returns the checkpoint kept for a tracker, or nil when there is none.
	Checkpoint(ctx context.Context, tool, key string) (*Checkpoint, error)
}

// Resumable is a Source whose trackers resume from checkpoints.
type Resumable interface {
	Source
	// Resume makes later scans look up a tracker's checkpoint in saved the first time they need the tracker.
	Resume(saved Saved)
	// Checkpoints returns the checkpoint of every tracker the last Scan read.
	Checkpoints() []Checkpoint
}
