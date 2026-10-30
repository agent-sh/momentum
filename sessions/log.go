package sessions

// Role says what an activity entry is.
type Role string

const (
	RoleUser      Role = "USER"      // text the harness recorded in the user role; not attribution to the owner
	RoleAssistant Role = "ASSISTANT" // the agent's text
	RoleTool      Role = "TOOL"      // a tool call
	RoleOut       Role = "OUT"       // a tool result
	RolePeer      Role = "PEER"      // a message another session sent
	RoleCompacted Role = "COMPACTED" // the summary a compaction wrote
)

// Entry is one line of a session's condensed activity.
type Entry struct {
	Role   Role
	Name   string // the tool, for a tool call
	Origin string // for user-role text: the harness's own label for where it came from, when it records one
	Text   string
}

// String renders the entry as one activity line: "ROLE: text", "TOOL name: text", or "USER [origin]: text".
func (e Entry) String() string {
	switch {
	case e.Role == RoleTool:
		return "TOOL " + e.Name + ": " + e.Text
	case e.Role == RoleUser && e.Origin != "":
		return "USER [" + e.Origin + "]: " + e.Text
	}
	return string(e.Role) + ": " + e.Text
}

// LogKeep is how many entries a Log holds before it drops the oldest quarter of them.
const LogKeep = 6000

// Log is a session's condensed activity with a running sequence number, so a review can read only what happened
// since the previous one.
//
// A store keeps a log entry by entry (see Checkpoint): Unsaved says what it does not hold yet, and Saved records
// that it holds everything.
type Log struct {
	base  int // sequence number of items[0]
	items []Entry
	kept  int // the entries before this sequence number are in a store
}

// Kept returns the log a store held: its oldest entry has sequence number base, and all of them count as saved.
func Kept(base int, items []Entry) Log {
	return Log{base: base, items: items, kept: base + len(items)}
}

// Add appends an entry.
func (l *Log) Add(e Entry) {
	l.items = append(l.items, e)
	if len(l.items) > LogKeep {
		drop := len(l.items) - LogKeep*3/4
		l.items = append([]Entry(nil), l.items[drop:]...)
		l.base += drop
	}
}

// Seq is how many entries the log has ever had.
func (l *Log) Seq() int {
	if l == nil {
		return 0
	}
	return l.base + len(l.items)
}

// Base is the sequence number of the oldest entry the log holds.
func (l *Log) Base() int {
	if l == nil {
		return 0
	}
	return l.base
}

// Since returns a copy of the entries after sequence number from. gap reports that some of them were already
// dropped, so the copy starts later than asked.
func (l *Log) Since(from int) (items []Entry, gap bool) {
	if l == nil {
		return nil, false
	}
	if from < l.base {
		return append([]Entry(nil), l.items...), from > 0
	}
	if from >= l.Seq() {
		return nil, false
	}
	return append([]Entry(nil), l.items[from-l.base:]...), false
}

// Unsaved returns the entries a store does not hold yet, and the sequence number of the first of them. To keep
// the log, a store drops what it holds below Base and from that number on, then adds these. A log that started
// over (a rewritten transcript) holds nothing saved, so the store drops everything it had.
func (l *Log) Unsaved() (from int, items []Entry) {
	from = max(l.kept, l.base)
	return from, append([]Entry(nil), l.items[from-l.base:]...)
}

// Saved records that a store holds every entry.
func (l *Log) Saved() { l.kept = l.Seq() }
