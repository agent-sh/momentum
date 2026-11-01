package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/plain"
)

// A case is one answer in progress to one of the standing questions (SPEC, Cases): its family, the evidence behind
// it, the suspected mechanism, the alternatives, the next test that tells them apart, the expected benefit, what it
// cost so far, and its status. Cases are kept: nothing derives them, so a schema change keeps them and Rebuild never
// touches them. A case is found by its signatures, so the same problem seen again lands in the same case: a
// detector's signature names a symptom, and the investigation adds the root cause's ("root:" and a key), which
// merges every case that shares it into one.

// caseTables are the case store's kept tables. A new kept table takes no step: a store from before it gains it at its
// next open.
const caseTables = `
CREATE TABLE IF NOT EXISTS cases (id TEXT PRIMARY KEY, family TEXT NOT NULL, title TEXT NOT NULL, status TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '', merged_into TEXT NOT NULL DEFAULT '', mechanism TEXT NOT NULL DEFAULT '',
  alternatives TEXT NOT NULL DEFAULT '[]', next_test TEXT NOT NULL DEFAULT '', benefit TEXT NOT NULL DEFAULT '',
  cost_usd REAL NOT NULL DEFAULT 0, cost_tokens INTEGER NOT NULL DEFAULT 0, cost_ms INTEGER NOT NULL DEFAULT 0,
  runs INTEGER NOT NULL DEFAULT 0, closed_evidence INTEGER NOT NULL DEFAULT 0, opened INTEGER NOT NULL,
  updated INTEGER NOT NULL, seen INTEGER NOT NULL, closed INTEGER NOT NULL DEFAULT 0, deployed INTEGER NOT NULL DEFAULT 0,
  package TEXT NOT NULL DEFAULT '', window_ms INTEGER NOT NULL DEFAULT 0, before_n INTEGER NOT NULL DEFAULT 0,
  after_n INTEGER NOT NULL DEFAULT 0, effect TEXT NOT NULL DEFAULT '', back INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS case_signatures (signature TEXT PRIMARY KEY, case_id TEXT NOT NULL, at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS case_evidence (case_id TEXT NOT NULL, ref TEXT NOT NULL, at INTEGER NOT NULL,
  kind TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '', added INTEGER NOT NULL,
  PRIMARY KEY (case_id, ref)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS case_log (case_id TEXT NOT NULL, seq INTEGER NOT NULL, at INTEGER NOT NULL, actor TEXT NOT NULL,
  action TEXT NOT NULL, detail TEXT NOT NULL DEFAULT '', PRIMARY KEY (case_id, seq)) WITHOUT ROWID;
`

// caseTableNames are the tables caseTables makes.
var caseTableNames = []string{"cases", "case_signatures", "case_evidence", "case_log"}

// caseIndexes find a case's signatures and its newest evidence.
const caseIndexes = `
CREATE INDEX IF NOT EXISTS case_signatures_case ON case_signatures(case_id);
CREATE INDEX IF NOT EXISTS case_evidence_at ON case_evidence(case_id, at);
`

var caseIndexNames = []string{"case_signatures_case", "case_evidence_at"}

// Families are the standing questions a case answers (SPEC, The questions it keeps asking).
var Families = []string{"owner-burden", "blind-spots", "manual-work", "sessions", "pressure", "capacity",
	"component-health", "failures", "outside"}

// CaseStatus is where a case stands. It runs open, investigating, testing, deployed, measuring, then closed with a
// reason.
type CaseStatus string

const (
	CaseOpen          CaseStatus = "open"          // a detector or an agent saw it; nobody has looked yet
	CaseInvestigating CaseStatus = "investigating" // its mechanism is being found
	CaseTesting       CaseStatus = "testing"       // a change is being built and checked
	CaseDeployed      CaseStatus = "deployed"      // its change is in place; only the ship tool sets it
	CaseMeasuring     CaseStatus = "measuring"     // its change's effect is being measured; only the measure tool sets it
	CaseClosed        CaseStatus = "closed"        // done, with a reason
)

// CaseStatuses are the statuses, in their order.
var CaseStatuses = []CaseStatus{CaseOpen, CaseInvestigating, CaseTesting, CaseDeployed, CaseMeasuring, CaseClosed}

// CloseReason is why a case closed.
type CloseReason string

const (
	Improved     CloseReason = "improved"              // its change measurably helped: only the measure tool closes so
	NotWorthIt   CloseReason = "not-worth-it"          // a fix would cost more than it saves, or the change did nothing
	Insufficient CloseReason = "insufficient-evidence" // the evidence cannot decide it; also a case unseen past its retention
	Solved       CloseReason = "already-solved"        // something else fixed it already
	Duplicate    CloseReason = "duplicate"             // another case has the same root cause: it went into that one
)

// CloseReasons are the reasons a case can close with.
var CloseReasons = []CloseReason{Improved, NotWorthIt, Insufficient, Solved, Duplicate}

// CaseRetention is how long a case may go unseen before it closes by itself: about as long as Claude Code keeps a
// transcript, past which the evidence behind it is gone from its source.
const CaseRetention = 30 * 24 * time.Hour

// RootPrefix starts the signature of a root cause, which an investigation adds to its case: it links the cases of
// one cause, and never opens one, since a stated cause is a guess until a test discriminates it.
const RootPrefix = "root:"

// Lengths a case's texts are kept to.
const (
	caseTextMax     = 2000 // a mechanism, an alternative, the next test, the benefit
	signatureMax    = 300
	refMax          = 500
	evidenceTextMax = 240
	mergeHops       = 16 // the longest chain of merges followed; a longer one is a broken store
)

// Case is one case.
type Case struct {
	ID           string
	Family       string
	Title        string
	Status       CaseStatus
	Reason       CloseReason // when closed
	Into         string      // when closed as a duplicate: the case it went into
	Mechanism    string      // the suspected mechanism
	Alternatives []string    // the other mechanisms that could explain the evidence
	NextTest     string      // the next discriminating test
	Benefit      string      // the expected benefit
	Cost         Cost        // what investigating and changing it cost so far
	Evidence     int         // how many evidence refs it holds
	// ClosedEvidence is how many it held when it closed: a dismissed case comes back only once the evidence that
	// came after the close reaches it.
	ClosedEvidence int
	Opened         time.Time
	Updated        time.Time
	Seen           time.Time // its newest evidence
	Closed         time.Time
	Deployed       time.Time // when its change went in, which its measurement counts from
	Back           time.Time // when it last came back after a close: its retention counts from the later of this and Opened
	Package        string    // its change package's directory
	Measurement    Measurement
	Signatures     []string
}

// Cost is what a case's runs spent: the model's money and tokens, and the time they took.
type Cost struct {
	USD      float64
	Tokens   int64
	Duration time.Duration
	Runs     int
}

// Measurement is a deployed change's effect on its family's signal: the signal's count over a window before the
// change and over as long after it, and the effect in words.
type Measurement struct {
	Window        time.Duration
	Before, After int
	Effect        string
}

// Evidence is one reference behind a case: an event (ev:ID), a note (note:ID), a commit (git:OWNER/REPO@SHA), a
// file (file:PATH, with @OFFSET), or an address. At is when it happened.
type Evidence struct {
	Ref     string
	At      time.Time
	Kind    string
	Summary string
	Added   time.Time
}

// CaseEntry is one line of a case's log: who changed it, how, and why.
type CaseEntry struct {
	Seq    int
	At     time.Time
	Actor  string
	Action string
	Detail string
}

// Finding is a report of a problem by its signature: a detector's, or an agent's. Observing it opens the case its
// signature names, or adds the evidence to the case that has it.
type Finding struct {
	Family    string
	Signature string
	Title     string
	Evidence  []Evidence
}

// Observed is what observing a finding did.
type Observed string

const (
	Opened    Observed = "opened"    // a new case
	Updated   Observed = "updated"   // new evidence on a case that is not closed
	Reopened  Observed = "reopened"  // a closed case came back on its new evidence
	KeptShut  Observed = "kept"      // a closed case took the new evidence and stays closed
	Unchanged Observed = "unchanged" // nothing new
)

// ErrBadCase means a case or a change to it cannot be stored as it is: a family, status, reason, signature or
// reference that is wrong, or a change its status does not allow.
var ErrBadCase = errors.New("bad case")

// ErrNoCase means no case has the id.
var ErrNoCase = errors.New("no case has this id")

func badCase(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBadCase, fmt.Sprintf(format, args...))
}

// caseText is a text of a case as it is kept: one plain line, clipped.
func caseText(s string, n int) string { return plain.Clip(plain.Line(s), n) }

func checkSignature(sig string) error {
	switch {
	case sig == "":
		return badCase("a finding needs a signature")
	case plain.Line(sig) != sig:
		return badCase("signature %q is not one plain line", sig)
	case len(sig) > signatureMax:
		return badCase("signature is %d bytes, at most %d", len(sig), signatureMax)
	case sig == RootPrefix || strings.HasPrefix(sig, RootPrefix) && strings.TrimSpace(sig[len(RootPrefix):]) == "":
		return badCase("a root cause's signature names it: root:KEY")
	}
	return nil
}

// refForms are the forms a reference takes: an event, a note, a commit, a file, an address.
var refForms = regexp.MustCompile(`^(?:ev:[0-9a-f]{24}|note:[^\s]+|git:[\w.-]+/[\w.-]+@[0-9a-f]{7,40}|file:(?:/|~/).+|https?://\S+)$`)

func checkRef(ref string) error {
	switch {
	case ref == "":
		return badCase("evidence needs a reference")
	case plain.Line(ref) != ref:
		return badCase("reference %q is not one plain line", ref)
	case len(ref) > refMax:
		return badCase("reference is %d bytes, at most %d", len(ref), refMax)
	case !refForms.MatchString(ref):
		return badCase("reference %q is none of ev:EVENT, note:ID, git:OWNER/REPO@SHA, file:PATH[@OFFSET] (absolute or ~/), an http or https address", ref)
	}
	return nil
}

// newCaseID is a short random id: c and 8 hex digits.
func newCaseID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return "c" + hex.EncodeToString(b)
}

const caseColumns = `id, family, title, status, reason, merged_into, mechanism, alternatives, next_test, benefit, cost_usd,
  cost_tokens, cost_ms, runs, closed_evidence, opened, updated, seen, closed, deployed, package, window_ms, before_n,
  after_n, effect, back`

func scanCase(row interface{ Scan(...any) error }) (Case, error) {
	var c Case
	var status, reason, alts string
	var costMS, opened, updated, seen, closed, deployed, window, back int64
	if err := row.Scan(&c.ID, &c.Family, &c.Title, &status, &reason, &c.Into, &c.Mechanism, &alts, &c.NextTest, &c.Benefit,
		&c.Cost.USD, &c.Cost.Tokens, &costMS, &c.Cost.Runs, &c.ClosedEvidence, &opened, &updated, &seen, &closed, &deployed,
		&c.Package, &window, &c.Measurement.Before, &c.Measurement.After, &c.Measurement.Effect, &back); err != nil {
		return Case{}, err
	}
	c.Status, c.Reason = CaseStatus(status), CloseReason(reason)
	if err := json.Unmarshal([]byte(alts), &c.Alternatives); err != nil {
		return Case{}, fmt.Errorf("case %s: its alternatives: %w", c.ID, err)
	}
	c.Cost.Duration, c.Measurement.Window = time.Duration(costMS)*time.Millisecond, time.Duration(window)*time.Millisecond
	c.Opened, c.Updated, c.Seen = time.UnixMilli(opened), time.UnixMilli(updated), time.UnixMilli(seen)
	if closed != 0 {
		c.Closed = time.UnixMilli(closed)
	}
	if deployed != 0 {
		c.Deployed = time.UnixMilli(deployed)
	}
	if back != 0 {
		c.Back = time.UnixMilli(back)
	}
	return c, nil
}

// getCase reads a case with its evidence count and signatures. A missing case is ErrNoCase.
func getCase(ctx context.Context, q querier, id string) (Case, error) {
	if own, ok := ctx.Value(caseAuthority{}).(string); ok && id != own {
		return Case{}, badCase("this run changes only its own canonical case")
	}
	c, err := scanCase(q.QueryRowContext(ctx, `SELECT `+caseColumns+` FROM cases WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Case{}, fmt.Errorf("%w: %q", ErrNoCase, id)
	}
	if err != nil {
		return Case{}, err
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM case_evidence WHERE case_id = ?`, id).Scan(&c.Evidence); err != nil {
		return Case{}, err
	}
	rows, err := q.QueryContext(ctx, `SELECT signature FROM case_signatures WHERE case_id = ? ORDER BY rowid`, id)
	if err != nil {
		return Case{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return Case{}, err
		}
		c.Signatures = append(c.Signatures, s)
	}
	return c, rows.Err()
}

// live follows a case's merges to the case that holds its evidence now.
func live(ctx context.Context, q querier, id string) (Case, error) {
	c, err := getCase(ctx, q, id)
	for hops := 0; err == nil && c.Reason == Duplicate && c.Into != ""; hops++ {
		if hops == mergeHops {
			return Case{}, fmt.Errorf("case %s: more than %d merges deep", id, mergeHops)
		}
		c, err = getCase(ctx, q, c.Into)
	}
	return c, err
}

// caseLog adds a line to a case's log.
func caseLog(ctx context.Context, q querier, id, actor, action, detail string, at time.Time) error {
	if actor == "" {
		actor = "unknown"
	}
	_, err := q.ExecContext(ctx, `INSERT INTO case_log (case_id, seq, at, actor, action, detail)
	  VALUES (?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM case_log WHERE case_id = ?), ?, ?, ?, ?)`,
		id, id, at.UnixMilli(), caseText(actor, 80), action, caseText(detail, caseTextMax))
	return err
}

// resolveEvidence checks a piece of evidence and fills it in. An event's reference (ev:ID) always takes the
// event's own time, kind and summary, whatever the caller gave, and a note's (note:ID) the note's time, kind and
// title: an event or a note the store does not hold is an error. Another reference without a time is dated now.
func resolveEvidence(ctx context.Context, q querier, e Evidence, now time.Time) (Evidence, error) {
	if err := checkRef(e.Ref); err != nil {
		return Evidence{}, err
	}
	var row *sql.Row
	switch {
	case strings.HasPrefix(e.Ref, "ev:"):
		row = q.QueryRowContext(ctx, `SELECT ts, kind, summary FROM events WHERE id = ?`, e.Ref[len("ev:"):])
	case strings.HasPrefix(e.Ref, "note:"):
		row = q.QueryRowContext(ctx, `SELECT at, 'note:' || kind, title FROM notes WHERE id = ?`, e.Ref[len("note:"):])
	}
	if row != nil {
		var ts int64
		var kind, summary string
		err := row.Scan(&ts, &kind, &summary)
		if errors.Is(err, sql.ErrNoRows) {
			return Evidence{}, badCase("the store holds nothing by %q", e.Ref)
		}
		if err != nil {
			return Evidence{}, err
		}
		e.At, e.Kind, e.Summary = time.UnixMilli(ts), kind, summary
	}
	if e.At.IsZero() {
		e.At = now
	}
	if e.At.After(now.Add(clockSkew)) {
		return Evidence{}, badCase("%q is dated %s, after now: evidence is of what happened", e.Ref, e.At.Format(time.RFC3339))
	}
	e.Kind, e.Summary = caseText(e.Kind, 80), caseText(e.Summary, evidenceTextMax)
	return e, nil
}

// clockSkew is how far after now a piece of evidence may be dated: two clocks of one machine read a moment apart.
const clockSkew = time.Minute

// addEvidence stores the evidence a case does not hold yet, and returns how many were new and the newest time
// among them.
func addEvidence(ctx context.Context, q querier, id string, evs []Evidence, now time.Time) (int, time.Time, error) {
	added, newest := 0, time.Time{}
	for _, e := range evs {
		e, err := resolveEvidence(ctx, q, e, now)
		if err != nil {
			return 0, time.Time{}, err
		}
		res, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO case_evidence (case_id, ref, at, kind, summary, added)
		  VALUES (?,?,?,?,?,?)`, id, e.Ref, e.At.UnixMilli(), e.Kind, e.Summary, now.UnixMilli())
		if err != nil {
			return 0, time.Time{}, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return 0, time.Time{}, err
		} else if n > 0 {
			added++
			if e.At.After(newest) {
				newest = e.At
			}
		}
	}
	return added, newest, nil
}

// comesBack decides whether a closed case comes back on the evidence it holds now, and says why either way.
//   - A case closed as improved comes back when the evidence of its last window (as long as its measurement's)
//     reaches what the window before its change held: the problem is back at its old rate. Without a measurement,
//     any evidence after the close brings it back.
//   - A case closed as already solved comes back on any evidence after the close.
//   - A dismissed case (not worth it, insufficient evidence) comes back only once the evidence after the close
//     reaches what it held when it closed: the problem has happened as often again.
func comesBack(ctx context.Context, q querier, c Case, now time.Time) (bool, string, error) {
	count := func(from time.Time) (int, error) {
		var n int
		err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM case_evidence WHERE case_id = ? AND at > ?`, c.ID, from.UnixMilli()).Scan(&n)
		return n, err
	}
	after, err := count(c.Closed)
	if err != nil {
		return false, "", err
	}
	switch c.Reason {
	case Improved:
		if c.Measurement.Window <= 0 {
			return after > 0, fmt.Sprintf("%d new since it closed as improved", after), nil
		}
		from := now.Add(-c.Measurement.Window)
		if from.Before(c.Closed) {
			from = c.Closed
		}
		recent, err := count(from)
		if err != nil {
			return false, "", err
		}
		need := max(c.Measurement.Before, 1)
		return recent >= need, fmt.Sprintf("%d in its last %s, %d before its change", recent, days(c.Measurement.Window), c.Measurement.Before), nil
	case Solved:
		return after > 0, fmt.Sprintf("%d new since it closed as already solved", after), nil
	case NotWorthIt, Insufficient:
		need := max(c.ClosedEvidence, 1)
		return after >= need, fmt.Sprintf("%d new since it closed as %s; it comes back at %d", after, c.Reason, need), nil
	}
	return false, "", nil
}

// days is a duration in whole days, or hours under two days.
func days(d time.Duration) string {
	if d < 48*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// reopenIfBack reopens a closed case its evidence brings back, and logs what came in and what it did, in one line:
// added says what came in. A case closed as improved or already solved that comes back regressed: its fix did not
// hold.
func reopenIfBack(ctx context.Context, q querier, c Case, added, actor string, now time.Time) (bool, error) {
	back, why, err := comesBack(ctx, q, c, now)
	if err != nil {
		return false, err
	}
	if !back {
		return false, caseLog(ctx, q, c.ID, actor, "kept closed", added+"; "+why, now)
	}
	if _, err := q.ExecContext(ctx, `UPDATE cases SET status = ?, reason = '', closed = 0, closed_evidence = 0, updated = ?, back = ?
	  WHERE id = ?`, CaseOpen, now.UnixMilli(), now.UnixMilli(), c.ID); err != nil {
		return false, err
	}
	action := "reopened"
	if c.Reason == Improved || c.Reason == Solved {
		action = "regressed"
	}
	return true, caseLog(ctx, q, c.ID, actor, action, fmt.Sprintf("%s; closed as %s; %s", added, c.Reason, why), now)
}

// takeEvidence gives a case the evidence it does not hold yet, moves its seen to the newest of it, and logs it: a
// closed case comes back as comesBack says.
func takeEvidence(ctx context.Context, q querier, c Case, evs []Evidence, by, actor string, now time.Time) (Case, Observed, error) {
	added, newest, err := addEvidence(ctx, q, c.ID, evs, now)
	if err != nil {
		return Case{}, "", err
	}
	if added == 0 {
		return c, Unchanged, nil
	}
	if _, err := q.ExecContext(ctx, `UPDATE cases SET seen = MAX(seen, ?), updated = ? WHERE id = ?`, newest.UnixMilli(),
		now.UnixMilli(), c.ID); err != nil {
		return Case{}, "", err
	}
	what := fmt.Sprintf("%d new", added)
	if by != "" {
		what += " by " + by
	}
	obs := Updated
	if c.Status == CaseClosed {
		back, err := reopenIfBack(ctx, q, c, what, actor, now)
		if err != nil {
			return Case{}, "", err
		}
		obs = KeptShut
		if back {
			obs = Reopened
		}
	} else if err := caseLog(ctx, q, c.ID, actor, "evidence", what, now); err != nil {
		return Case{}, "", err
	}
	c, err = getCase(ctx, q, c.ID)
	return c, obs, err
}

func checkFamily(f string) error {
	if !slices.Contains(Families, f) {
		return badCase("family %q is not one of %s", f, strings.Join(Families, ", "))
	}
	return nil
}

// ObserveCase files a finding: the case its signature names takes the evidence it does not hold yet, or a new case
// opens with it. A case closed as a duplicate passes it on to the case it went into. A closed case keeps the
// evidence and comes back only as comesBack says. A new case's title is the finding's; a case's own title stays.
func (s *Store) ObserveCase(ctx context.Context, f Finding, actor string) (Case, Observed, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Case{}, "", err
	}
	defer tx.Rollback()
	c, obs, err := observe(ctx, tx, f, actor, time.Now())
	if err != nil {
		return Case{}, "", err
	}
	return c, obs, tx.Commit()
}

func observe(ctx context.Context, q querier, f Finding, actor string, now time.Time) (Case, Observed, error) {
	if err := checkFamily(f.Family); err != nil {
		return Case{}, "", err
	}
	if err := checkSignature(f.Signature); err != nil {
		return Case{}, "", err
	}
	if strings.HasPrefix(f.Signature, RootPrefix) {
		return Case{}, "", badCase("%q names a root cause: an investigation adds it to its case, and a symptom's signature opens one", f.Signature)
	}
	var id string
	err := q.QueryRowContext(ctx, `SELECT case_id FROM case_signatures WHERE signature = ?`, f.Signature).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		title := caseText(f.Title, TitleMax)
		if title == "" {
			return Case{}, "", badCase("a new case needs a title")
		}
		for id == "" {
			cand := newCaseID()
			var taken int
			if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM cases WHERE id = ?`, cand).Scan(&taken); err != nil {
				return Case{}, "", err
			}
			if taken == 0 {
				id = cand
			}
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO cases (id, family, title, status, opened, updated, seen) VALUES (?,?,?,?,?,?,?)`,
			id, f.Family, title, CaseOpen, now.UnixMilli(), now.UnixMilli(), now.UnixMilli()); err != nil {
			return Case{}, "", err
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO case_signatures (signature, case_id, at) VALUES (?,?,?)`,
			f.Signature, id, now.UnixMilli()); err != nil {
			return Case{}, "", err
		}
		added, newest, err := addEvidence(ctx, q, id, f.Evidence, now)
		if err != nil {
			return Case{}, "", err
		}
		if added > 0 {
			if _, err := q.ExecContext(ctx, `UPDATE cases SET seen = ? WHERE id = ?`, newest.UnixMilli(), id); err != nil {
				return Case{}, "", err
			}
		}
		if err := caseLog(ctx, q, id, actor, "opened", fmt.Sprintf("%s, %d evidence, by %s", f.Family, added, f.Signature), now); err != nil {
			return Case{}, "", err
		}
		c, err := getCase(ctx, q, id)
		return c, Opened, err
	case err != nil:
		return Case{}, "", err
	}
	c, err := live(ctx, q, id)
	if err != nil {
		return Case{}, "", err
	}
	return takeEvidence(ctx, q, c, f.Evidence, f.Signature, actor, now)
}

// CaseByID reads a case. A case closed as a duplicate is read as it is: CaseLive follows it.
func (s *Store) CaseByID(ctx context.Context, id string) (Case, error) { return getCase(ctx, s.db, id) }

// CaseLive reads the case that holds a case's evidence now: the case itself, or the one it went into.
func (s *Store) CaseLive(ctx context.Context, id string) (Case, error) { return live(ctx, s.db, id) }

// CaseFilter selects cases: one family, some statuses (none: every status but closed).
type CaseFilter struct {
	Family   string
	Statuses []CaseStatus
	All      bool // every status, closed ones too
}

// Cases lists cases, the most recently changed first.
func (s *Store) Cases(ctx context.Context, f CaseFilter) ([]Case, error) {
	var where []string
	var args []any
	if f.Family != "" {
		where, args = append(where, "family = ?"), append(args, f.Family)
	}
	switch {
	case len(f.Statuses) > 0:
		where = append(where, "status IN (?"+strings.Repeat(",?", len(f.Statuses)-1)+")")
		for _, st := range f.Statuses {
			args = append(args, string(st))
		}
	case !f.All:
		where, args = append(where, "status != ?"), append(args, string(CaseClosed))
	}
	q := `SELECT id FROM cases`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY updated DESC, id`, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Case, 0, len(ids))
	for _, id := range ids {
		c, err := getCase(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// CaseEvidence lists a case's evidence, newest first, at most limit of it (0: all).
func (s *Store) CaseEvidence(ctx context.Context, id string, limit int) ([]Evidence, error) {
	q := `SELECT ref, at, kind, summary, added FROM case_evidence WHERE case_id = ? ORDER BY at DESC, ref`
	args := []any{id}
	if limit > 0 {
		q, args = q+` LIMIT ?`, append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Evidence
	for rows.Next() {
		var e Evidence
		var at, added int64
		if err := rows.Scan(&e.Ref, &at, &e.Kind, &e.Summary, &added); err != nil {
			return nil, err
		}
		e.At, e.Added = time.UnixMilli(at), time.UnixMilli(added)
		out = append(out, e)
	}
	return out, rows.Err()
}

// CaseLog is a case's log, oldest first.
func (s *Store) CaseLog(ctx context.Context, id string) ([]CaseEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, at, actor, action, detail FROM case_log WHERE case_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CaseEntry
	for rows.Next() {
		var e CaseEntry
		var at int64
		if err := rows.Scan(&e.Seq, &at, &e.Actor, &e.Action, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// CaseUpdate is a change to a case's investigation: a field set is replaced, one left nil stays. Status moves among
// open, investigating and testing: a deployed or measuring case is the ship and measure tools' to move, and
// closing and reopening have their own calls.
type CaseUpdate struct {
	Status       CaseStatus
	Title        *string
	Mechanism    *string
	Alternatives []string // set: replaces them; nil keeps them
	NextTest     *string
	Benefit      *string
}

// working are the statuses an investigation moves a case among.
var working = []CaseStatus{CaseOpen, CaseInvestigating, CaseTesting}

// UpdateCase changes a case's investigation, as CaseUpdate says, and logs what changed. A case closed as a
// duplicate is changed where it went.
func (s *Store) UpdateCase(ctx context.Context, id string, u CaseUpdate, actor string) (Case, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Case{}, err
	}
	defer tx.Rollback()
	now := time.Now()
	c, err := live(ctx, tx, id)
	if err != nil {
		return Case{}, err
	}
	var sets []string
	var args []any
	var changed []string
	if u.Status != "" && u.Status != c.Status {
		switch {
		case !slices.Contains(CaseStatuses, u.Status):
			return Case{}, badCase("status %q is not one of open, investigating, testing", u.Status)
		case c.Status == CaseClosed:
			return Case{}, badCase("case %s is closed (%s): reopen it with new evidence", c.ID, c.Reason)
		case !slices.Contains(working, c.Status):
			return Case{}, badCase("case %s is %s: only the ship and measure tools move it", c.ID, c.Status)
		case u.Status == CaseClosed:
			return Case{}, badCase("close a case with its own call, which takes a reason")
		case !slices.Contains(working, u.Status):
			return Case{}, badCase("only the ship tool deploys and only the measure tool measures; a case moves among open, investigating and testing")
		}
		sets, args = append(sets, "status = ?"), append(args, string(u.Status))
		changed = append(changed, fmt.Sprintf("status %s -> %s", c.Status, u.Status))
	}
	text := func(col, name string, v *string, n int) error {
		if v == nil {
			return nil
		}
		t := caseText(*v, n)
		if col == "title" && t == "" {
			return badCase("a case's title cannot be empty")
		}
		sets, args = append(sets, col+" = ?"), append(args, t)
		changed = append(changed, name+": "+plain.Clip(t, 160))
		return nil
	}
	for _, f := range []struct {
		col, name string
		v         *string
		n         int
	}{{"title", "title", u.Title, TitleMax}, {"mechanism", "mechanism", u.Mechanism, caseTextMax},
		{"next_test", "next test", u.NextTest, caseTextMax}, {"benefit", "benefit", u.Benefit, caseTextMax}} {
		if err := text(f.col, f.name, f.v, f.n); err != nil {
			return Case{}, err
		}
	}
	if u.Alternatives != nil {
		alts := make([]string, 0, len(u.Alternatives))
		for _, a := range u.Alternatives {
			if a = caseText(a, caseTextMax); a != "" {
				alts = append(alts, a)
			}
		}
		raw, err := json.Marshal(alts)
		if err != nil {
			return Case{}, err
		}
		sets, args = append(sets, "alternatives = ?"), append(args, string(raw))
		changed = append(changed, fmt.Sprintf("%d alternatives", len(alts)))
	}
	if len(sets) == 0 {
		return c, tx.Commit()
	}
	if c.Status == CaseClosed {
		return Case{}, badCase("case %s is closed (%s): reopen it with new evidence before changing it", c.ID, c.Reason)
	}
	// A change is built only for a mechanism with a test that tells it from the alternatives: an investigation's
	// stated cause is a guess until a test discriminates it. So a case in testing, after the update as before it,
	// holds both.
	after, mech, next := c.Status, c.Mechanism, c.NextTest
	if u.Status != "" {
		after = u.Status
	}
	if u.Mechanism != nil {
		mech = caseText(*u.Mechanism, caseTextMax)
	}
	if u.NextTest != nil {
		next = caseText(*u.NextTest, caseTextMax)
	}
	if after == CaseTesting && (mech == "" || next == "") {
		return Case{}, badCase("case %s is in testing only with a mechanism and the next discriminating test on record", c.ID)
	}
	sets, args = append(sets, "updated = ?"), append(args, now.UnixMilli(), c.ID)
	if _, err := tx.ExecContext(ctx, `UPDATE cases SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return Case{}, err
	}
	if err := caseLog(ctx, tx, c.ID, actor, "set", strings.Join(changed, "; "), now); err != nil {
		return Case{}, err
	}
	if c, err = getCase(ctx, tx, c.ID); err != nil {
		return Case{}, err
	}
	return c, tx.Commit()
}

// AddCaseEvidence gives a case more evidence, or the case it went into a duplicate's. A closed case keeps it, and
// comes back only as comesBack says.
func (s *Store) AddCaseEvidence(ctx context.Context, id string, evs []Evidence, actor string) (Case, Observed, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Case{}, "", err
	}
	defer tx.Rollback()
	now := time.Now()
	c, err := live(ctx, tx, id)
	if err != nil {
		return Case{}, "", err
	}
	c, obs, err := takeEvidence(ctx, tx, c, evs, "", actor, now)
	if err != nil {
		return Case{}, "", err
	}
	return c, obs, tx.Commit()
}

// ReopenCase brings a closed case back on evidence it did not hold: someone judged that the evidence changed. A
// reopening with nothing new is refused, so a dismissed case does not come back on the evidence it was dismissed
// on, and so is a duplicate's: the case it went into holds its evidence.
func (s *Store) ReopenCase(ctx context.Context, id string, evs []Evidence, why, actor string) (Case, error) {
	if strings.TrimSpace(why) == "" {
		return Case{}, badCase("a reopening says why the evidence changed")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Case{}, err
	}
	defer tx.Rollback()
	now := time.Now()
	c, err := getCase(ctx, tx, id)
	if err != nil {
		return Case{}, err
	}
	switch {
	case c.Status != CaseClosed:
		return Case{}, badCase("case %s is %s, not closed", c.ID, c.Status)
	case c.Reason == Duplicate:
		return Case{}, badCase("case %s went into %s, which holds its evidence: reopen that one", c.ID, c.Into)
	}
	added, newest, err := addEvidence(ctx, tx, c.ID, evs, now)
	if err != nil {
		return Case{}, err
	}
	if added == 0 {
		return Case{}, badCase("case %s comes back only on evidence it does not hold", c.ID)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cases SET status = ?, reason = '', closed = 0, closed_evidence = 0, updated = ?,
	  seen = MAX(seen, ?), back = ? WHERE id = ?`, CaseOpen, now.UnixMilli(), newest.UnixMilli(), now.UnixMilli(), c.ID); err != nil {
		return Case{}, err
	}
	action := "reopened"
	if c.Reason == Improved || c.Reason == Solved {
		action = "regressed" // its fix did not hold
	}
	if err := caseLog(ctx, tx, c.ID, actor, action, fmt.Sprintf("closed as %s; %d new evidence: %s", c.Reason, added, why), now); err != nil {
		return Case{}, err
	}
	if c, err = getCase(ctx, tx, c.ID); err != nil {
		return Case{}, err
	}
	return c, tx.Commit()
}

// CloseCase closes the case it names when it is open, investigating or testing, with a reason other than improved,
// which only the measure tool gives. A duplicate goes into the case into names, followed to where it went: its
// evidence, signatures and cost move there, and so does what its investigation found where that case has none.
func (s *Store) CloseCase(ctx context.Context, id string, reason CloseReason, into, why, actor string) (Case, error) {
	if strings.TrimSpace(why) == "" {
		return Case{}, badCase("a close says why")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Case{}, err
	}
	defer tx.Rollback()
	now := time.Now()
	c, err := getCase(ctx, tx, id)
	if err != nil {
		return Case{}, err
	}
	switch {
	case c.Status == CaseClosed && c.Reason == Duplicate:
		return Case{}, badCase("case %s is closed already: a duplicate of %s", c.ID, c.Into)
	case !slices.Contains(CloseReasons, reason):
		return Case{}, badCase("reason %q is not one of not-worth-it, insufficient-evidence, already-solved, duplicate", reason)
	case reason == Improved:
		return Case{}, badCase("only the measure tool closes a case as improved, with its measurement")
	case c.Status == CaseClosed:
		return Case{}, badCase("case %s is closed already (%s)", c.ID, c.Reason)
	case !slices.Contains(working, c.Status):
		return Case{}, badCase("case %s is %s: its change is in place, so only the ship tool's rollback or the measure tool closes it", c.ID, c.Status)
	case reason == Duplicate && into == "":
		return Case{}, badCase("a duplicate names the case it goes into")
	case reason != Duplicate && into != "":
		return Case{}, badCase("only a duplicate goes into another case")
	}
	if reason == Duplicate {
		if c, err = merge(ctx, tx, c, into, why, actor, now); err != nil {
			return Case{}, err
		}
		return c, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cases SET status = ?, reason = ?, closed = ?, updated = ?,
	  closed_evidence = (SELECT COUNT(*) FROM case_evidence WHERE case_id = ?) WHERE id = ?`,
		CaseClosed, string(reason), now.UnixMilli(), now.UnixMilli(), c.ID, c.ID); err != nil {
		return Case{}, err
	}
	if err := caseLog(ctx, tx, c.ID, actor, "closed", string(reason)+": "+why, now); err != nil {
		return Case{}, err
	}
	if c, err = getCase(ctx, tx, c.ID); err != nil {
		return Case{}, err
	}
	return c, tx.Commit()
}

// merge closes c as a duplicate of the case into names (followed to where it went), and moves c's evidence,
// signatures and cost there. A closed case that takes it in comes back as comesBack says. It returns the case
// that holds the evidence now.
func merge(ctx context.Context, q querier, c Case, into, why, actor string, now time.Time) (Case, error) {
	dst, err := live(ctx, q, into)
	if err != nil {
		return Case{}, err
	}
	if dst.ID == c.ID {
		return Case{}, badCase("case %s cannot go into itself", c.ID)
	}
	var moved int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM case_evidence WHERE case_id = ? AND ref NOT IN
	  (SELECT ref FROM case_evidence WHERE case_id = ?)`, c.ID, dst.ID).Scan(&moved); err != nil {
		return Case{}, err
	}
	var newest int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(at), 0) FROM case_evidence WHERE case_id = ?`, c.ID).Scan(&newest); err != nil {
		return Case{}, err
	}
	for _, stmt := range []string{
		`INSERT OR IGNORE INTO case_evidence (case_id, ref, at, kind, summary, added)
		   SELECT ?, ref, at, kind, summary, added FROM case_evidence WHERE case_id = ?`,
		`UPDATE case_signatures SET case_id = ? WHERE case_id = ?`,
	} {
		if _, err := q.ExecContext(ctx, stmt, dst.ID, c.ID); err != nil {
			return Case{}, err
		}
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM case_evidence WHERE case_id = ?`, c.ID); err != nil {
		return Case{}, err
	}
	// What the duplicate's investigation found goes where the target has none.
	var took []string
	sets, args := []string{}, []any{}
	for _, f := range []struct{ col, name, from, to string }{{"mechanism", "mechanism", c.Mechanism, dst.Mechanism},
		{"next_test", "next test", c.NextTest, dst.NextTest}, {"benefit", "benefit", c.Benefit, dst.Benefit}} {
		if f.from != "" && f.to == "" {
			sets, args, took = append(sets, f.col+" = ?"), append(args, f.from), append(took, f.name)
		}
	}
	if len(c.Alternatives) > 0 && len(dst.Alternatives) == 0 {
		raw, err := json.Marshal(c.Alternatives)
		if err != nil {
			return Case{}, err
		}
		sets, args, took = append(sets, "alternatives = ?"), append(args, string(raw)), append(took, "alternatives")
	}
	sets = append(sets, "cost_usd = cost_usd + ?", "cost_tokens = cost_tokens + ?", "cost_ms = cost_ms + ?", "runs = runs + ?",
		"seen = MAX(seen, ?)", "updated = ?")
	args = append(args, c.Cost.USD, c.Cost.Tokens, c.Cost.Duration.Milliseconds(), c.Cost.Runs, newest, now.UnixMilli(), dst.ID)
	if _, err := q.ExecContext(ctx, `UPDATE cases SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return Case{}, err
	}
	// Each case that went into c goes where c goes: one hop from its live case, however many merges come.
	if _, err := q.ExecContext(ctx, `UPDATE cases SET merged_into = ? WHERE merged_into = ?`, dst.ID, c.ID); err != nil {
		return Case{}, err
	}
	// The cost moves: the duplicate keeps none, so a sum over cases counts each run once. Its log keeps its lines.
	if _, err := q.ExecContext(ctx, `UPDATE cases SET status = ?, reason = ?, merged_into = ?, closed = ?, updated = ?,
	  closed_evidence = 0, cost_usd = 0, cost_tokens = 0, cost_ms = 0, runs = 0 WHERE id = ?`, CaseClosed, string(Duplicate),
		dst.ID, now.UnixMilli(), now.UnixMilli(), c.ID); err != nil {
		return Case{}, err
	}
	if err := caseLog(ctx, q, c.ID, actor, "closed", fmt.Sprintf("duplicate of %s: %s", dst.ID, why), now); err != nil {
		return Case{}, err
	}
	what := fmt.Sprintf("took in %s (%d new evidence, %d signatures", c.ID, moved, len(c.Signatures))
	if len(took) > 0 {
		what += ", its " + strings.Join(took, ", ")
	}
	if err := caseLog(ctx, q, dst.ID, actor, "merged", what+"): "+why, now); err != nil {
		return Case{}, err
	}
	if dst.Status == CaseClosed && moved > 0 {
		if _, err := reopenIfBack(ctx, q, dst, fmt.Sprintf("%d new from %s", moved, c.ID), actor, now); err != nil {
			return Case{}, err
		}
	}
	return getCase(ctx, q, dst.ID)
}

// AddCaseSignature gives a case another signature, so what a detector sees by it lands there; the investigation
// names the root cause so ("root:" and a key). A signature another case holds merges this case into that one:
// cases dedupe by root cause. It returns the case that holds the evidence now, and whether a merge happened.
func (s *Store) AddCaseSignature(ctx context.Context, id, sig, actor string) (Case, bool, error) {
	if err := checkSignature(sig); err != nil {
		return Case{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Case{}, false, err
	}
	defer tx.Rollback()
	now := time.Now()
	c, err := live(ctx, tx, id)
	if err != nil {
		return Case{}, false, err
	}
	var holder string
	err = tx.QueryRowContext(ctx, `SELECT case_id FROM case_signatures WHERE signature = ?`, sig).Scan(&holder)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `INSERT INTO case_signatures (signature, case_id, at) VALUES (?,?,?)`, sig, c.ID,
			now.UnixMilli()); err != nil {
			return Case{}, false, err
		}
		if err := caseLog(ctx, tx, c.ID, actor, "signature", sig, now); err != nil {
			return Case{}, false, err
		}
		if c, err = getCase(ctx, tx, c.ID); err != nil {
			return Case{}, false, err
		}
		return c, false, tx.Commit()
	case err != nil:
		return Case{}, false, err
	}
	h, err := live(ctx, tx, holder)
	if err != nil {
		return Case{}, false, err
	}
	if h.ID == c.ID {
		return c, false, tx.Commit()
	}
	if !slices.Contains(working, c.Status) {
		return Case{}, false, badCase("case %s holds %q, and case %s is %s: only an open, investigating or testing case goes into another",
			h.ID, sig, c.ID, c.Status)
	}
	if h.Status == CaseClosed {
		return Case{}, false, badCase("case %s holds %q and is closed (%s): a case does not go into a closed one by its signature. If this "+
			"case's evidence is new to it, bring it back first (case reopen %s --evidence REF... --why WHY), then add the signature", h.ID, sig,
			h.Reason, h.ID)
	}
	if c, err = merge(ctx, tx, c, h.ID, "the same signature "+sig, actor, now); err != nil {
		return Case{}, false, err
	}
	return c, true, tx.Commit()
}

// AddCaseCost adds what a run spent on a case.
func (s *Store) AddCaseCost(ctx context.Context, id string, c Cost, actor string) (Case, error) {
	if c.USD < 0 || c.Tokens < 0 || c.Duration < 0 || c.Runs < 0 {
		return Case{}, badCase("a cost is never negative")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Case{}, err
	}
	defer tx.Rollback()
	now := time.Now()
	cs, err := live(ctx, tx, id)
	if err != nil {
		return Case{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cases SET cost_usd = cost_usd + ?, cost_tokens = cost_tokens + ?,
	  cost_ms = cost_ms + ?, runs = runs + ?, updated = ? WHERE id = ?`, c.USD, c.Tokens, c.Duration.Milliseconds(), c.Runs,
		now.UnixMilli(), cs.ID); err != nil {
		return Case{}, err
	}
	if err := caseLog(ctx, tx, cs.ID, actor, "cost", fmt.Sprintf("$%.2f, %d tokens, %s, %d runs", c.USD, c.Tokens,
		c.Duration.Round(time.Second), c.Runs), now); err != nil {
		return Case{}, err
	}
	if cs, err = getCase(ctx, tx, cs.ID); err != nil {
		return Case{}, err
	}
	return cs, tx.Commit()
}

// SweepCases closes, as insufficient evidence, every case open, investigating or testing whose newest evidence is
// older than the retention and that has been open as long since it opened or last came back: the problem stopped,
// and what it rests on is gone from its source. Any evidence after the close brings it back. A deployed or
// measuring case is its measurement's. Only the families named are swept (none named: every family), since a
// family nothing looked at says nothing stopped. It returns the cases it closed.
func (s *Store) SweepCases(ctx context.Context, now time.Time, retention time.Duration, actor string, families ...string) ([]Case, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	cut := now.Add(-retention).UnixMilli()
	q, args := `SELECT id FROM cases WHERE status IN (?,?,?) AND seen < ? AND MAX(opened, back) < ?`, []any{CaseOpen, CaseInvestigating, CaseTesting, cut, cut}
	if len(families) > 0 {
		q += ` AND family IN (?` + strings.Repeat(",?", len(families)-1) + `)`
		for _, f := range families {
			args = append(args, f)
		}
	}
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []Case
	for _, id := range ids {
		// The problem stopped, which judges nothing about its evidence: any evidence after the close brings it back.
		if _, err := tx.ExecContext(ctx, `UPDATE cases SET status = ?, reason = ?, closed = ?, updated = ?, closed_evidence = 0
		  WHERE id = ?`, CaseClosed, string(Insufficient), now.UnixMilli(), now.UnixMilli(), id); err != nil {
			return nil, err
		}
		if err := caseLog(ctx, tx, id, actor, "closed", fmt.Sprintf("%s: unseen for %s, past its evidence's retention", Insufficient,
			days(retention)), now); err != nil {
			return nil, err
		}
		c, err := getCase(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, tx.Commit()
}

// DeployCase is the ship tool's: a case in testing goes to deployed, with its change package and the time its
// change went in, from which its measurement counts. Nothing else deploys a case.
func (s *Store) DeployCase(ctx context.Context, id, pkg string, at time.Time, what, actor string) (Case, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Case{}, err
	}
	defer tx.Rollback()
	c, err := getCase(ctx, tx, id)
	if err != nil {
		return Case{}, err
	}
	if c.Status != CaseTesting {
		return Case{}, badCase("case %s is %s: only a case in testing deploys", c.ID, c.Status)
	}
	if pkg == "" || at.IsZero() {
		return Case{}, badCase("a deployed case names its package and when its change went in")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cases SET status = ?, package = ?, deployed = ?, updated = ? WHERE id = ?`,
		CaseDeployed, pkg, at.UnixMilli(), time.Now().UnixMilli(), c.ID); err != nil {
		return Case{}, err
	}
	if err := caseLog(ctx, tx, c.ID, actor, "deployed", what, time.Now()); err != nil {
		return Case{}, err
	}
	if c, err = getCase(ctx, tx, c.ID); err != nil {
		return Case{}, err
	}
	return c, tx.Commit()
}

// RollBackCase is the ship tool's: a deployed or measuring case whose change was undone goes back to testing,
// with no measurement, since its change is gone.
func (s *Store) RollBackCase(ctx context.Context, id, why, actor string) (Case, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Case{}, err
	}
	defer tx.Rollback()
	c, err := getCase(ctx, tx, id)
	if err != nil {
		return Case{}, err
	}
	if c.Status != CaseDeployed && c.Status != CaseMeasuring {
		return Case{}, badCase("case %s is %s: only a deployed or measuring case rolls back", c.ID, c.Status)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cases SET status = ?, deployed = 0, window_ms = 0, before_n = 0, after_n = 0,
	  effect = '', updated = ? WHERE id = ?`, CaseTesting, time.Now().UnixMilli(), c.ID); err != nil {
		return Case{}, err
	}
	if err := caseLog(ctx, tx, c.ID, actor, "rolled back", why, time.Now()); err != nil {
		return Case{}, err
	}
	if c, err = getCase(ctx, tx, c.ID); err != nil {
		return Case{}, err
	}
	return c, tx.Commit()
}

// CaseNote logs a line on a case for a tool that changes nothing of its own: the ship tool's steps before it
// deploys (prepared, checked, proposed, reviewed), the runner's runs.
func (s *Store) CaseNote(ctx context.Context, id, action, detail, actor string) error {
	c, err := live(ctx, s.db, id)
	if err != nil {
		return err
	}
	return caseLog(ctx, s.db, c.ID, actor, action, detail, time.Now())
}

type caseAuthority struct{}

// WithCaseAuthority confines mutations to one canonical case. Alias resolution checks it in the transaction.
func WithCaseAuthority(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, caseAuthority{}, id)
}
