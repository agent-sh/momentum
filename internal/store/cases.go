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
// it, the suspected mechanism, the alternatives, the next test (the fix a build tests), the expected benefit, what it
// cost so far, and its status. Cases are kept: nothing derives them, so a schema change keeps them and Rebuild never
// touches them. A case is found by its signatures, so the same problem seen again lands in the same case: a
// detector's signature names a symptom, and a build that checked the findings adds the root cause's ("root:" and a
// key), which merges every case that shares it into one.

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

// caseIndexes find a case's signatures, its newest evidence, and the evidence of one reference in every case (the
// triggers that keep what evidence recorded, keptTriggers).
const caseIndexes = `
CREATE INDEX IF NOT EXISTS case_signatures_case ON case_signatures(case_id);
CREATE INDEX IF NOT EXISTS case_evidence_at ON case_evidence(case_id, at);
CREATE INDEX IF NOT EXISTS case_evidence_ref ON case_evidence(ref);
`

var caseIndexNames = []string{"case_signatures_case", "case_evidence_at", "case_evidence_ref"}

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
	Guarded      CloseReason = "guarded"               // a guard's watch ended with no recurrence: no measurement, only the measure tool closes so
	NotWorthIt   CloseReason = "not-worth-it"          // a fix would cost more than it saves, or the change did nothing
	Insufficient CloseReason = "insufficient-evidence" // the evidence cannot decide it; also a case unseen past its retention
	Solved       CloseReason = "already-solved"        // something else fixed it already
	Duplicate    CloseReason = "duplicate"             // another case has the same root cause: it went into that one
)

// CloseReasons are the reasons a case can close with.
var CloseReasons = []CloseReason{Improved, Guarded, NotWorthIt, Insufficient, Solved, Duplicate}

// CaseRetention is how long a case may go unseen before it closes by itself: about as long as Claude Code keeps a
// transcript, past which the evidence behind it is gone from its source.
const CaseRetention = 30 * 24 * time.Hour

// RootPrefix starts the signature of a root cause, which a build adds to its case once the findings hold: it links
// the cases of one cause, and never opens one, since a stated cause is a guess until a test discriminates it.
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
	Episode      int64 // increments whenever a closed case reopens
	ID           string
	Family       string
	Title        string
	Status       CaseStatus
	Reason       CloseReason // when closed
	Into         string      // when closed as a duplicate: the case it went into
	Mechanism    string      // the suspected mechanism
	Alternatives []string    // the other mechanisms that could explain the evidence
	NextTest     string      // the next test: the fix the investigation found, which a build tests
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
	// Exposure is a guard's, and marks one (Guarded): the units of exposure its incidents (Before) came in before the
	// change. A guard measures no rate: its watch asks whether the problem comes back, and After counts recurrences.
	Exposure int
	// Watched and Recur are a guard's at its close: the units of exposure its watch saw with no recurrence, and the
	// predictive probability that the problem, had it kept happening, would have recurred within them.
	Watched int
	Recur   float64
	// closedGuard marks the measurement of a case closed as guarded: its close may record no exposure, when its
	// history held none in the sources it kept at its watch's end.
	closedGuard bool
}

// Guarded says the measurement is a guard's watch, not a measured drop: one with exposure, or a guarded close's.
func (m Measurement) Guarded() bool { return m.Exposure > 0 || m.closedGuard }

// Evidence is one reference behind a case: an event (ev:ID), a note (note:ID), a commit (git:OWNER/REPO@SHA), a
// file (file:PATH, with @OFFSET), or an address. At is when it happened. Tool and Cwd are an event's tool and
// working directory as it was filed (resolveEvidence), which judge the event once the store no longer holds it; any
// other reference has neither.
type Evidence struct {
	Ref     string
	At      time.Time
	Kind    string
	Summary string
	Tool    string
	Cwd     string
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
  after_n, effect, back, episode, guard_exposure, guard_watched, guard_recur`

func scanCase(row interface{ Scan(...any) error }) (Case, error) {
	var c Case
	var status, reason, alts string
	var costMS, opened, updated, seen, closed, deployed, window, back int64
	if err := row.Scan(&c.ID, &c.Family, &c.Title, &status, &reason, &c.Into, &c.Mechanism, &alts, &c.NextTest, &c.Benefit,
		&c.Cost.USD, &c.Cost.Tokens, &costMS, &c.Cost.Runs, &c.ClosedEvidence, &opened, &updated, &seen, &closed, &deployed,
		&c.Package, &window, &c.Measurement.Before, &c.Measurement.After, &c.Measurement.Effect, &back, &c.Episode, &c.Measurement.Exposure,
		&c.Measurement.Watched, &c.Measurement.Recur); err != nil {
		return Case{}, err
	}
	c.Status, c.Reason = CaseStatus(status), CloseReason(reason)
	c.Measurement.closedGuard = c.Status == CaseClosed && c.Reason == Guarded
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
	var err error
	if detail, err = cleanText(ctx, detail); err != nil {
		return err
	}
	return caseLogCleaned(ctx, q, id, actor, action, detail, at)
}

// caseLogCleaned writes text already cleaned before its transaction began.
func caseLogCleaned(ctx context.Context, q querier, id, actor, action, detail string, at time.Time) error {
	if actor == "" {
		actor = "unknown"
	}
	_, err := q.ExecContext(ctx, `INSERT INTO case_log (case_id, seq, at, actor, action, detail)
	  VALUES (?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM case_log WHERE case_id = ?), ?, ?, ?, ?)`,
		id, id, at.UnixMilli(), caseText(actor, 80), action, CaseNoteText(detail))
	return err
}

// CaseNoteText is the stored form of a case note after its caller cleans it.
func CaseNoteText(text string) string { return caseText(text, caseTextMax) }

// resolveEvidence checks a piece of evidence and fills it in. An event's reference (ev:ID) always takes the
// event's own time, kind, summary, tool and working directory, whatever the caller gave, and a note's (note:ID) the
// note's time, kind and title: an event or a note the store does not hold is an error. Another reference without a
// time is dated now. Only an event's has a tool and a directory.
func resolveEvidence(ctx context.Context, q querier, e Evidence, now time.Time) (Evidence, error) {
	if err := checkRef(e.Ref); err != nil {
		return Evidence{}, err
	}
	var row *sql.Row
	e.Tool, e.Cwd = "", ""
	switch {
	case strings.HasPrefix(e.Ref, "ev:"):
		row = q.QueryRowContext(ctx, `SELECT ts, kind, summary, COALESCE(tool, ''), COALESCE(cwd, '') FROM events WHERE id = ?`, e.Ref[len("ev:"):])
	case strings.HasPrefix(e.Ref, "note:"):
		row = q.QueryRowContext(ctx, `SELECT at, 'note:' || kind, title, '', '' FROM notes WHERE id = ?`, e.Ref[len("note:"):])
	}
	if row != nil {
		var ts int64
		var kind, summary, tool, cwd string
		err := row.Scan(&ts, &kind, &summary, &tool, &cwd)
		if errors.Is(err, sql.ErrNoRows) {
			return Evidence{}, badCase("the store holds nothing by %q", e.Ref)
		}
		if err != nil {
			return Evidence{}, err
		}
		e.At, e.Kind, e.Summary, e.Tool, e.Cwd = time.UnixMilli(ts), kind, summary, tool, cwd
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
// among them. An event's evidence it holds already takes the event's tool and directory as the store holds them now,
// which makes it no new evidence; a row they would not change is not written.
func addEvidence(ctx context.Context, q querier, id string, evs []Evidence, now time.Time) (int, time.Time, error) {
	added, newest := 0, time.Time{}
	for _, e := range evs {
		e, err := resolveEvidence(ctx, q, e, now)
		if err != nil {
			return 0, time.Time{}, err
		}
		res, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO case_evidence (case_id, ref, at, kind, summary, tool, cwd, added)
		  VALUES (?,?,?,?,?,?,?,?)`, id, e.Ref, e.At.UnixMilli(), e.Kind, e.Summary, e.Tool, e.Cwd, now.UnixMilli())
		if err != nil {
			return 0, time.Time{}, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return 0, time.Time{}, err
		} else if n == 0 && strings.HasPrefix(e.Ref, "ev:") {
			if _, err := q.ExecContext(ctx, `UPDATE case_evidence SET tool = ?1, cwd = ?2 WHERE case_id = ?3 AND ref = ?4
			  AND (tool != ?1 OR cwd != ?2)`, e.Tool, e.Cwd, id, e.Ref); err != nil {
				return 0, time.Time{}, err
			}
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
//   - A case closed as improved does not come back on its evidence: the measure step watches its signal, in the
//     units its measurement counted, and brings it back at its old rate (RegressCase).
//   - Nor does one closed as guarded: the measure step's watch reads its signal and its evidence from the watch's start
//     on every pass, by the rule the watch used, and brings it back (RegressCase). Only an event counts there: evidence
//     of another kind (a note, a commit, a file, an address) brings it back only by hand (ReopenCase).
//   - A case closed as already solved comes back on any evidence after the close, or after its fix was named when
//     that came after the close (CloseFixed on a case closed before a close named its fix).
//   - A dismissed case (not worth it, insufficient evidence) comes back only once the evidence after the close
//     reaches what it held when it closed: the problem has happened as often again.
func comesBack(ctx context.Context, q querier, c Case, now time.Time) (bool, string, error) {
	count := func(from time.Time) (int, error) {
		var n int
		err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM case_evidence WHERE case_id = ? AND at > ?`, c.ID, from.UnixMilli()).Scan(&n)
		return n, err
	}
	if c.Reason == Guarded {
		// one rule decides a guarded case's evidence: the measure step's watch after the close, which reads its events on
		// every pass from the watch's start and leaves the loop's own out, as the watch did; it reads no other evidence
		return false, "a guarded case's evidence is the measure step's watch to read: an event of it from the watch's start brings the case back, " +
			"and evidence that is no event does not (case reopen brings the case back by hand)", nil
	}
	from, since := c.Closed, "it closed as "+strings.ReplaceAll(string(c.Reason), "-", " ")
	if c.Reason == Solved {
		_, at, named, err := fixedOf(ctx, q, c)
		if err != nil {
			return false, "", err
		}
		if named && at.After(from) {
			from, since = at, "its fix was named"
		}
	}
	after, err := count(from)
	if err != nil {
		return false, "", err
	}
	switch c.Reason {
	case Improved:
		return false, fmt.Sprintf("%d new since it closed as improved; the measure step brings it back at its old rate", after), nil
	case Solved:
		return after > 0, fmt.Sprintf("%d new since %s", after, since), nil
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
	if _, err := q.ExecContext(ctx, `UPDATE cases SET episode = episode + 1, status = ?, reason = '', closed = 0, closed_evidence = 0, updated = ?, back = ?,
	  `+clearGuard+` WHERE id = ?`, CaseOpen, now.UnixMilli(), now.UnixMilli(), c.ID); err != nil {
		return false, err
	}
	action := "reopened"
	if c.Reason == Improved || c.Reason == Guarded || c.Reason == Solved {
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
		return Case{}, "", badCase("%q names a root cause: a build adds it to its case, and a symptom's signature opens one", f.Signature)
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
	Limit    int  // zero: all; callers inside a run bound materialization
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
	q += ` ORDER BY updated DESC, id`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
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

// heldOr is a merge's value of an evidence column (s, the duplicate's evidence, joined to its event e): the event's
// as the store holds it now, where it holds the event, else what the evidence recorded.
func heldOr(col string) string {
	return "CASE WHEN e.id IS NULL THEN s." + col + " ELSE COALESCE(e." + col + ", '') END"
}

// CaseEvidence lists a case's evidence, newest first, at most limit of it (0: all).
func (s *Store) CaseEvidence(ctx context.Context, id string, limit int) ([]Evidence, error) {
	q := `SELECT ref, at, kind, summary, tool, cwd, added FROM case_evidence WHERE case_id = ? ORDER BY at DESC, ref`
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
		if err := rows.Scan(&e.Ref, &at, &e.Kind, &e.Summary, &e.Tool, &e.Cwd, &added); err != nil {
			return nil, err
		}
		e.At, e.Added = time.UnixMilli(at), time.UnixMilli(added)
		out = append(out, e)
	}
	return out, rows.Err()
}

// CaseLogCount counts the entries without materializing them.
func (s *Store) CaseLogCount(ctx context.Context, id string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM case_log WHERE case_id = ?`, id).Scan(&n)
	return n, err
}

// CaseLog is a case's log, oldest first.
func (s *Store) CaseLog(ctx context.Context, id string) ([]CaseEntry, error) {
	return s.CaseLogLimit(ctx, id, 0)
}

// CaseLogLimit selects the newest limit entries before materializing their text.
// Zero retains the caller's unrestricted history interface.
func (s *Store) CaseLogLimit(ctx context.Context, id string, limit int) ([]CaseEntry, error) {
	q := `SELECT seq, at, actor, action, detail FROM case_log WHERE case_id = ? ORDER BY seq`
	args := []any{id}
	if limit > 0 {
		q = `SELECT seq, at, actor, action, detail FROM (SELECT seq, at, actor, action, detail FROM case_log WHERE case_id = ? ORDER BY seq DESC LIMIT ?) ORDER BY seq`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
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

// working are the statuses the loop moves a case among: the runner records an investigation into testing, and a
// build that finds the findings do not hold sends it back.
var working = []CaseStatus{CaseOpen, CaseInvestigating, CaseTesting}

// UpdateCase changes a case's investigation, as CaseUpdate says, and logs what changed. A case closed as a
// duplicate is changed where it went.
func (s *Store) UpdateCase(ctx context.Context, id string, u CaseUpdate, actor string) (Case, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Case{}, err
	}
	defer tx.Rollback()
	c, err := live(ctx, tx, id)
	if err != nil {
		return Case{}, err
	}
	if c, err = updateCase(ctx, tx, c, u, actor, time.Now()); err != nil {
		return Case{}, err
	}
	return c, tx.Commit()
}

// RecordFindings writes an investigation's findings into the case it investigated, the runner's bookkeeping for
// the run, in one transaction that reads the case by its own id without following a merge, only while that case
// has not gone into another and is still open or investigating (a case moved on since the run started keeps what
// it has). The bool says whether they were recorded.
func (s *Store) RecordFindings(ctx context.Context, id string, episode int64, u CaseUpdate, actor string, notes ...CaseEntry) (Case, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Case{}, false, err
	}
	defer tx.Rollback()
	c, err := getCase(ctx, tx, id)
	if err != nil {
		return Case{}, false, err
	}
	if c.Episode != episode || c.Into != "" || c.Status != CaseOpen && c.Status != CaseInvestigating {
		return c, false, nil
	}
	if c, err = updateCase(ctx, tx, c, u, actor, time.Now()); err != nil {
		return Case{}, false, err
	}
	for _, note := range notes {
		if err := caseLog(ctx, tx, c.ID, actor, note.Action, note.Detail, time.Now()); err != nil {
			return Case{}, false, err
		}
	}
	return c, true, tx.Commit()
}

// RecordUndecided is RecordFindings for an investigation that ended well without naming both a mechanism and a fix:
// the evidence did not decide. Its findings go into the case, which it then closes as insufficient evidence, why
// saying so, in the same transaction and under the same guard; Cases' rule brings the case back once as much
// evidence again comes in.
func (s *Store) RecordUndecided(ctx context.Context, id string, episode int64, u CaseUpdate, why, actor string) (Case, bool, error) {
	if strings.TrimSpace(why) == "" {
		why = "the investigation named no mechanism and fix"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Case{}, false, err
	}
	defer tx.Rollback()
	now := time.Now()
	c, err := getCase(ctx, tx, id)
	if err != nil {
		return Case{}, false, err
	}
	if c.Episode != episode || c.Into != "" || c.Status != CaseOpen && c.Status != CaseInvestigating {
		return c, false, nil
	}
	u.Status = "" // it closes
	if c, err = updateCase(ctx, tx, c, u, actor, now); err != nil {
		return Case{}, false, err
	}
	if err := closeAs(ctx, tx, c.ID, Insufficient, why, actor, now); err != nil {
		return Case{}, false, err
	}
	if c, err = getCase(ctx, tx, c.ID); err != nil {
		return Case{}, false, err
	}
	return c, true, tx.Commit()
}

// updateCase is UpdateCase's change of a case already read in the transaction.
func updateCase(ctx context.Context, tx *sql.Tx, c Case, u CaseUpdate, actor string, now time.Time) (Case, error) {
	var err error
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
		{"next_test", "fix", u.NextTest, caseTextMax}, {"benefit", "benefit", u.Benefit, caseTextMax}} { // next_test holds the investigation's fix
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
		return c, nil
	}
	if c.Status == CaseClosed {
		return Case{}, badCase("case %s is closed (%s): reopen it with new evidence before changing it", c.ID, c.Reason)
	}
	// A change is built only for a mechanism with a fix to test: an investigation's stated cause is a guess until a
	// build's test discriminates it. So a case in testing, after the update as before it, holds both.
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
		return Case{}, badCase("case %s is in testing only with a mechanism and a fix (its next test) on record", c.ID)
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
	return c, nil
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
	if _, err := tx.ExecContext(ctx, `UPDATE cases SET episode = episode + 1, status = ?, reason = '', closed = 0, closed_evidence = 0, updated = ?,
	  seen = MAX(seen, ?), back = ?, `+clearGuard+` WHERE id = ?`, CaseOpen, now.UnixMilli(), newest.UnixMilli(), now.UnixMilli(), c.ID); err != nil {
		return Case{}, err
	}
	action := "reopened"
	if c.Reason == Improved || c.Reason == Guarded || c.Reason == Solved {
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
	case reason == Guarded:
		return Case{}, badCase("only the measure tool closes a case as guarded, with its watch")
	case c.Status == CaseClosed && c.Family == HealthFamily && c.Reason == Solved && reason == Solved:
		hint := ""
		if _, _, named, err := fixedOf(ctx, tx, c); err != nil {
			return Case{}, err
		} else if !named {
			hint = fmt.Sprintf(" before its fix was named: case close %s --reason already-solved --fix REF --why WHY names it, which resumes the cases its stop held", c.ID)
		}
		return Case{}, badCase("case %s is closed already (%s)%s", c.ID, c.Reason, hint)
	case c.Status == CaseClosed:
		return Case{}, badCase("case %s is closed already (%s)", c.ID, c.Reason)
	case !slices.Contains(working, c.Status):
		return Case{}, badCase("case %s is %s: its change is in place, so only the ship tool's rollback or the measure tool closes it", c.ID, c.Status)
	case reason == Duplicate && into == "":
		return Case{}, badCase("a duplicate names the case it goes into")
	case reason != Duplicate && into != "":
		return Case{}, badCase("only a duplicate goes into another case")
	case reason == Solved && c.Family == HealthFamily:
		return Case{}, badCase("a %s case closes as already solved naming what fixed it (case close %s --reason already-solved --fix REF --why WHY), which resumes the cases its stop held", HealthFamily, c.ID)
	}
	if reason == Duplicate {
		if c, err = merge(ctx, tx, c, into, why, actor, now); err != nil {
			return Case{}, err
		}
		return c, tx.Commit()
	}
	if err := closeAs(ctx, tx, c.ID, reason, why, actor, now); err != nil {
		return Case{}, err
	}
	if c, err = getCase(ctx, tx, c.ID); err != nil {
		return Case{}, err
	}
	return c, tx.Commit()
}

// HealthFamily is the family of the cases the loop files about itself: its stop rule opens one when a case's last two
// runs failed the same way, and a fix of it resumes the cases it stopped (CloseFixed).
const HealthFamily = "component-health"

// ActionResumed is the line a case the stop rule held gets when the component-health case of its stop closes as fixed:
// the stop rule counts only the case's runs after the newest such line. ActionFixed is the line that names the fix
// on the component-health case, as it was given. ActionFixNamed is the line a case closed as already solved before a
// close named its fix gets when its fix is named after the close, with the cases it resumed and why.
const (
	ActionResumed  = "resumed"
	ActionFixed    = "fixed"
	ActionFixNamed = "fix named"
)

// ReviewNoteID is the id of the note the loop keeps when a case's review waits for a cause (budget, stopped).
func ReviewNoteID(caseID, cause string) string { return "loop-review:" + IDPart(caseID) + ":" + cause }

// CloseFixed closes a component-health case as already solved by what fixed it (fix: its commit, git:OWNER/REPO@SHA,
// or another reference), and resumes the cases its stop held: each case of stopped still open, investigating or
// testing gets a resumed line naming the fix, in the same transaction, and keeps its episode, status and
// investigation; its review's stopped note is done. A case closed, deployed or measuring, or one that went into
// another, is not resumed. It returns the closed case, the cases it resumed, and whether it only named the fix. A
// case whose stop comes after the close resumes then (ResumeLate). A case closed as already solved before a close
// named its fix gets its fix named the same way and stays closed: no reopen, no new episode, no evidence; the
// naming counts as its close for a stop that comes after it.
func (s *Store) CloseFixed(ctx context.Context, id, fix string, stopped []string, why, actor string) (Case, []string, bool, error) {
	if strings.TrimSpace(why) == "" {
		return Case{}, nil, false, badCase("a close says why")
	}
	if err := checkRef(fix); err != nil {
		return Case{}, nil, false, badCase("what fixed it: %v", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Case{}, nil, false, err
	}
	defer tx.Rollback()
	now := time.Now()
	c, err := getCase(ctx, tx, id)
	if err != nil {
		return Case{}, nil, false, err
	}
	if err := fixable(ctx, tx, c); err != nil {
		return Case{}, nil, false, err
	}
	var resumed []string
	for _, sid := range stopped {
		sc, err := getCase(ctx, tx, sid)
		if errors.Is(err, ErrNoCase) {
			continue
		} else if err != nil {
			return Case{}, nil, false, err
		}
		if sc.ID == c.ID || sc.Into != "" || !slices.Contains(working, sc.Status) || slices.Contains(resumed, sc.ID) {
			continue
		}
		if err := resume(ctx, tx, sc.ID, c.ID, fix, "", actor, now); err != nil {
			return Case{}, nil, false, err
		}
		resumed = append(resumed, sc.ID)
	}
	if err := caseLog(ctx, tx, c.ID, actor, ActionFixed, fix, now); err != nil {
		return Case{}, nil, false, err
	}
	what := "fixed by " + fix
	if len(resumed) > 0 {
		what += "; resumes " + strings.Join(resumed, ", ")
	}
	named := c.Status == CaseClosed
	if named {
		// closed as already solved before a close named its fix: the close stands, and only its fix is named
		if _, err := tx.ExecContext(ctx, `UPDATE cases SET updated = ? WHERE id = ?`, now.UnixMilli(), c.ID); err != nil {
			return Case{}, nil, false, err
		}
		if err := caseLog(ctx, tx, c.ID, actor, ActionFixNamed, "closed as already-solved before its fix was named; "+what+": "+why, now); err != nil {
			return Case{}, nil, false, err
		}
	} else if err := closeAs(ctx, tx, c.ID, Solved, what+": "+why, actor, now); err != nil {
		return Case{}, nil, false, err
	}
	if c, err = getCase(ctx, tx, c.ID); err != nil {
		return Case{}, nil, false, err
	}
	return c, resumed, named, tx.Commit()
}

// Fixable refuses a case CloseFixed would refuse: one of another family, one closed for another reason or closed
// with its fix named, or one deployed or measuring.
func (s *Store) Fixable(ctx context.Context, id string) error {
	c, err := getCase(ctx, s.db, id)
	if err != nil {
		return err
	}
	return fixable(ctx, s.db, c)
}

func fixable(ctx context.Context, q querier, c Case) error {
	switch {
	case c.Status == CaseClosed && c.Reason == Duplicate:
		return badCase("case %s is closed already: a duplicate of %s", c.ID, c.Into)
	case c.Family != HealthFamily:
		return badCase("case %s is %s: only a %s case names what fixed it, to resume the cases its stop held", c.ID, c.Family, HealthFamily)
	case c.Status == CaseClosed && c.Reason != Solved:
		return badCase("case %s is closed already (%s), not as already solved: only a case closed as already-solved names its fix after its close", c.ID, c.Reason)
	case c.Status == CaseClosed:
		fix, _, named, err := fixedOf(ctx, q, c)
		if err != nil {
			return err
		}
		if named {
			return badCase("case %s is closed already (already-solved), and its fix is named already: %s", c.ID, fix)
		}
	case !slices.Contains(working, c.Status):
		return badCase("case %s is %s: its change is in place, so only the ship tool's rollback or the measure tool closes it", c.ID, c.Status)
	}
	return nil
}

// fixedOf reads the fix named for a component-health case closed as already solved, and when it was named: its
// newest fixed line from its close on, written by the close or, for a case closed before a close named its fix, by
// the naming after it. The naming is the close a late stop is measured against (ResumeLate).
func fixedOf(ctx context.Context, q querier, h Case) (string, time.Time, bool, error) {
	var fix string
	var at int64
	err := q.QueryRowContext(ctx, `SELECT detail, at FROM case_log WHERE case_id = ? AND action = ? AND at >= ? ORDER BY seq DESC LIMIT 1`,
		h.ID, ActionFixed, h.Closed.UnixMilli()).Scan(&fix, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, false, nil
	} else if err != nil {
		return "", time.Time{}, false, err
	}
	return fix, time.UnixMilli(at), true, nil
}

// resume writes a case's resumed line at a time, naming the component-health case and its fix, and marks its
// review's stopped note done: a stop after the resume puts it again (the runner's noteReviewSkip).
func resume(ctx context.Context, q querier, id, health, fix, late, actor string, now time.Time) error {
	detail := fmt.Sprintf("the stop rule's hold ended: %s, the loop's case of its stop, closed as fixed by %s%s", health, fix, late)
	if err := caseLog(ctx, q, id, actor, ActionResumed, detail, now); err != nil {
		return err
	}
	_, err := q.ExecContext(ctx, `UPDATE notes SET done = ? WHERE id = ? AND done = 0`, now.UnixMilli(), ReviewNoteID(id, "stopped"))
	return err
}

// FixedStop finds the component-health case that holds a stop's signature when it closed as fixed after first, the
// start of the stop's older run: the close would have resumed the case had its stop come in time. For a case closed
// as already solved before a close named its fix, the naming counts as its close. It returns that case and what
// fixed it.
func (s *Store) FixedStop(ctx context.Context, signature string, first time.Time) (Case, string, bool, error) {
	h, fix, _, ok, err := fixedStop(ctx, s.db, signature, first)
	return h, fix, ok, err
}

// fixedStop is FixedStop, with the time of the close that named the fix.
func fixedStop(ctx context.Context, q querier, signature string, first time.Time) (Case, string, time.Time, bool, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT case_id FROM case_signatures WHERE signature = ?`, signature).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Case{}, "", time.Time{}, false, nil
	} else if err != nil {
		return Case{}, "", time.Time{}, false, err
	}
	h, err := getCase(ctx, q, id)
	if err != nil {
		return Case{}, "", time.Time{}, false, err
	}
	if h.Family != HealthFamily || h.Status != CaseClosed || h.Reason != Solved {
		return Case{}, "", time.Time{}, false, nil
	}
	fix, at, named, err := fixedOf(ctx, q, h)
	if err != nil || !named || !at.After(first) {
		return Case{}, "", time.Time{}, false, err // no fix named yet, or named before the stop's older run began
	}
	return h, fix, at, true, nil
}

// ResumeLate resumes a case the stop rule would hold now when the component-health case of its stop closed as fixed
// after the stop's older run started (FixedStop): its second failure was still running at the close, or ended after
// the close read which cases were held. The resumed line is dated at the close (for a case closed before a close
// named its fix, at the naming), as the close would have written it, so a run that started after the fix counts
// toward the case's next stop. It reports whether it resumed the case.
func (s *Store) ResumeLate(ctx context.Context, id, signature string, first time.Time, actor string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	h, fix, closed, ok, err := fixedStop(ctx, tx, signature, first)
	if err != nil || !ok {
		return false, err
	}
	c, err := getCase(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if c.Into != "" || !slices.Contains(working, c.Status) {
		return false, nil
	}
	// as of the close, as the close would have resumed it: a run started after the fix still counts toward the next stop
	if err := resume(ctx, tx, c.ID, h.ID, fix, ", after this stop's first run started", actor, closed); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ResumedAt is when a case last resumed (its newest resumed line), or zero when it never did.
func (s *Store) ResumedAt(ctx context.Context, id string) (time.Time, error) {
	var at sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(at) FROM case_log WHERE case_id = ? AND action = ?`, id, ActionResumed).Scan(&at); err != nil {
		return time.Time{}, err
	}
	if !at.Valid {
		return time.Time{}, nil
	}
	return time.UnixMilli(at.Int64), nil
}

// closeAs closes a case with a reason other than a duplicate's, keeping how much evidence it held, and logs why.
func closeAs(ctx context.Context, q querier, id string, reason CloseReason, why, actor string, now time.Time) error {
	if _, err := q.ExecContext(ctx, `UPDATE cases SET status = ?, reason = ?, closed = ?, updated = ?, `+clearGuard+`,
	  closed_evidence = (SELECT COUNT(*) FROM case_evidence WHERE case_id = ?) WHERE id = ?`,
		CaseClosed, string(reason), now.UnixMilli(), now.UnixMilli(), id, id); err != nil {
		return err
	}
	return caseLog(ctx, q, id, actor, "closed", string(reason)+": "+why, now)
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
		// The duplicate's evidence moves with its event's tool and directory as the store holds them now, where it holds
		// the event, else as the duplicate recorded them: what an older build's merge stores too (case_evidence_filed,
		// then case_evidence_deleted). Evidence the target holds already keeps what it recorded, and where it recorded
		// neither the delete below gives it the duplicate's record (case_evidence_deleted), for both builds alike.
		`INSERT OR IGNORE INTO case_evidence (case_id, ref, at, kind, summary, tool, cwd, added)
		   SELECT ?, s.ref, s.at, s.kind, s.summary, ` + heldOr("tool") + `, ` + heldOr("cwd") + `, s.added
		   FROM case_evidence AS s LEFT JOIN events AS e ON substr(s.ref, 1, 3) = 'ev:' AND e.id = substr(s.ref, 4)
		   WHERE s.case_id = ?`,
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
		{"next_test", "fix", c.NextTest, dst.NextTest}, {"benefit", "benefit", c.Benefit, dst.Benefit}} {
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

// AddCaseSignature gives a case another signature, so what a detector sees by it lands there; a build names the
// root cause so ("root:" and a key). A signature another case holds merges this case into that one:
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
	c, err := live(ctx, tx, id)
	if err != nil {
		return Case{}, false, err
	}
	c, merged, err := addCaseSignature(ctx, tx, c, sig, actor)
	if err != nil {
		return Case{}, false, err
	}
	return c, merged, tx.Commit()
}

func addCaseSignature(ctx context.Context, tx *sql.Tx, c Case, sig, actor string) (Case, bool, error) {
	now := time.Now()
	var holder string
	err := tx.QueryRowContext(ctx, `SELECT case_id FROM case_signatures WHERE signature = ?`, sig).Scan(&holder)
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
		return c, false, nil
	case err != nil:
		return Case{}, false, err
	}
	h, err := live(ctx, tx, holder)
	if err != nil {
		return Case{}, false, err
	}
	if h.ID == c.ID {
		return c, false, nil
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
	return c, true, nil
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
	what, err := cleanText(ctx, what)
	if err != nil {
		return Case{}, err
	}
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
	if err := caseLogCleaned(ctx, tx, c.ID, actor, "deployed", what, time.Now()); err != nil {
		return Case{}, err
	}
	if c, err = getCase(ctx, tx, c.ID); err != nil {
		return Case{}, err
	}
	return c, tx.Commit()
}

// RollBackCase is the ship tool's: a deployed or measuring case whose change was undone goes back to testing. A
// measuring one loses every measurement column, since they are its change's and its change is gone. A deployed one's
// change was never measured, so its columns hold the last measurement of a change before it, which it keeps
// (clearGuard), as a case that comes back does.
func (s *Store) RollBackCase(ctx context.Context, id, why, actor string) (Case, error) {
	why, err := cleanText(ctx, why)
	if err != nil {
		return Case{}, err
	}
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
	columns := clearMeasurement
	if c.Status == CaseDeployed {
		columns = clearGuard
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cases SET status = ?, deployed = 0, `+columns+`, updated = ? WHERE id = ?`,
		CaseTesting, time.Now().UnixMilli(), c.ID); err != nil {
		return Case{}, err
	}
	if err := caseLogCleaned(ctx, tx, c.ID, actor, "rolled back", why, time.Now()); err != nil {
		return Case{}, err
	}
	if c, err = getCase(ctx, tx, c.ID); err != nil {
		return Case{}, err
	}
	return c, tx.Commit()
}

// StartMeasuring is the measure tool's: a deployed case goes to measuring once its change is in effect, from, with
// its measurement's window and the signal's count over the window before its change, which the window after it is
// compared with. The window after counts from then: the case's deployed time becomes it.
func (s *Store) StartMeasuring(ctx context.Context, id string, window time.Duration, before int, from time.Time, actor string) (Case, error) {
	return s.startMeasuring(ctx, id, window, before, 0, from, actor, "measuring", func() string {
		return fmt.Sprintf("in effect from %s; %d in its baseline; the %s after it is due %s",
			from.Format(time.DateTime), before, days(window), from.Add(window).Format(time.DateTime))
	})
}

// StartGuarding is StartMeasuring for a change whose plan is a guard: it keeps the guard's incidents and the units of
// exposure they came in (Measurement.Exposure marks the case guarded), and compares no rates; its log line says it is
// guarded, with what the guard watches for (detail), and when its watch is due.
func (s *Store) StartGuarding(ctx context.Context, id string, watch time.Duration, incidents, exposure int, from time.Time, detail, actor string) (Case, error) {
	if exposure <= 0 {
		return Case{}, badCase("a guard keeps the exposure its incidents came in")
	}
	return s.startMeasuring(ctx, id, watch, incidents, exposure, from, actor, "guarding", func() string {
		return fmt.Sprintf("in effect from %s; guarded, not measured: %s; its watch is due %s", from.Format(time.DateTime), detail,
			from.Add(watch).Format(time.DateTime))
	})
}

func (s *Store) startMeasuring(ctx context.Context, id string, window time.Duration, before, exposure int, from time.Time, actor, action string, line func() string) (Case, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Case{}, err
	}
	defer tx.Rollback()
	c, err := getCase(ctx, tx, id)
	if err != nil {
		return Case{}, err
	}
	switch {
	case c.Status != CaseDeployed:
		return Case{}, badCase("case %s is %s: only a deployed case is measured", c.ID, c.Status)
	case window <= 0 || before < 0:
		return Case{}, badCase("a measurement needs its window and the count before its change")
	case from.Before(c.Deployed):
		return Case{}, badCase("case %s's change went in %s: its measurement counts from then or later", c.ID, c.Deployed.Format(time.DateTime))
	}
	now := time.Now()
	// a measurement of its own: what an earlier episode's measurement counted after its change, and said of it, a case
	// that came back keeps until then (clearGuard), and a guard counts its recurrences in after_n
	if _, err := tx.ExecContext(ctx, `UPDATE cases SET status = ?, window_ms = ?, before_n = ?, guard_exposure = ?, after_n = 0, effect = '', guard_watched = 0,
	  guard_recur = 0, deployed = ?, updated = ? WHERE id = ?`,
		CaseMeasuring, window.Milliseconds(), before, exposure, from.UnixMilli(), now.UnixMilli(), c.ID); err != nil {
		return Case{}, err
	}
	if err := caseLog(ctx, tx, c.ID, actor, action, line(), now); err != nil {
		return Case{}, err
	}
	if c, err = getCase(ctx, tx, c.ID); err != nil {
		return Case{}, err
	}
	return c, tx.Commit()
}

// RestartMeasuring starts a fresh window after a measuring change, from a later time: it is in effect again, or a
// source of its measurement did not read what its window holds. The old window decides nothing; the deployed time and
// the log, whose line is why and the time, record the restart.
func (s *Store) RestartMeasuring(ctx context.Context, id string, from time.Time, why, actor string) (Case, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Case{}, err
	}
	defer tx.Rollback()
	c, err := getCase(ctx, tx, id)
	if err != nil {
		return Case{}, err
	}
	if c.Status != CaseMeasuring || !from.After(c.Deployed) {
		return Case{}, badCase("case %s needs a later in-effect time while measuring", id)
	}
	now := time.Now()
	if _, err := tx.ExecContext(ctx, `UPDATE cases SET deployed = ?, updated = ? WHERE id = ?`, from.UnixMilli(), now.UnixMilli(), id); err != nil {
		return Case{}, err
	}
	if err := caseLog(ctx, tx, id, actor, "measuring restarted", why+" "+from.Format(time.DateTime), now); err != nil {
		return Case{}, err
	}
	c, err = getCase(ctx, tx, id)
	if err != nil {
		return Case{}, err
	}
	return c, tx.Commit()
}

// CloseMeasured is the measure tool's: a measuring case whose change measurably helped closes as improved, with
// its count after the change and the effect in words. Its old rate brings it back (RegressCase).
func (s *Store) CloseMeasured(ctx context.Context, id string, after int, effect, actor string) (Case, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Case{}, err
	}
	defer tx.Rollback()
	c, err := getCase(ctx, tx, id)
	if err != nil {
		return Case{}, err
	}
	switch {
	case c.Status != CaseMeasuring:
		return Case{}, badCase("case %s is %s: only a measuring case closes as improved", c.ID, c.Status)
	case c.Measurement.Guarded():
		return Case{}, badCase("case %s is guarded: a guard's watch closes as guarded, never as improved", c.ID)
	case strings.TrimSpace(effect) == "" || after < 0:
		return Case{}, badCase("an improvement closes with its count after the change and its effect")
	}
	now := time.Now()
	if _, err := tx.ExecContext(ctx, `UPDATE cases SET status = ?, reason = ?, closed = ?, updated = ?, after_n = ?, effect = ?,
	  closed_evidence = (SELECT COUNT(*) FROM case_evidence WHERE case_id = ?) WHERE id = ?`,
		CaseClosed, string(Improved), now.UnixMilli(), now.UnixMilli(), after, caseText(effect, caseTextMax), c.ID, c.ID); err != nil {
		return Case{}, err
	}
	if err := caseLog(ctx, tx, c.ID, actor, "closed", string(Improved)+": "+effect, now); err != nil {
		return Case{}, err
	}
	if c, err = getCase(ctx, tx, c.ID); err != nil {
		return Case{}, err
	}
	return c, tx.Commit()
}

// clearGuard empties a case's guard columns: a case that comes back, by regression, reopening or a close by hand,
// keeps no watch of its last episode to show as a measurement. A guard keeps its incidents, recurrences, watch and
// words in the measurement's columns too (Measurement.Guarded marks them a guard's: its exposure, or its close as
// guarded, which may record none), so those go with it; a measured case keeps its last measurement, which case show
// still shows. SQLite reads every SET expression from the row as it was, so the test sees the guard's exposure and
// reason.
const clearGuard = `window_ms = CASE WHEN ` + guardRow + ` THEN 0 ELSE window_ms END, before_n = CASE WHEN ` + guardRow + ` THEN 0 ELSE before_n END,
  after_n = CASE WHEN ` + guardRow + ` THEN 0 ELSE after_n END, effect = CASE WHEN ` + guardRow + ` THEN '' ELSE effect END,
  guard_exposure = 0, guard_watched = 0, guard_recur = 0`

// guardRow says in SQL that a case's measurement columns are a guard's (Measurement.Guarded).
const guardRow = "(guard_exposure > 0 OR reason = '" + string(Guarded) + "')"

// clearMeasurement empties every measurement and guard column: a rollback of a measuring change takes the change
// away, and with it what was measured of it.
const clearMeasurement = `guard_exposure = 0, guard_watched = 0, guard_recur = 0, window_ms = 0, before_n = 0, after_n = 0, effect = ''`

// CloseGuarded is the measure tool's: a measuring case whose guard's watch ended with no recurrence closes as guarded,
// never as improved, however much of its unit the watch saw (none, when it was not used or only while the change was
// out of effect), with its history's incidents and exposure as counted through the watch's start, the units of
// exposure watched, the predictive probability that the problem, had it kept happening, would have recurred within
// them (0 when the history gives no rate or nothing was watched), and the watch in words. Its log line adds sources,
// when given: the sources it left out of its counts, which no other record names. The case store leaves its evidence
// to the measure step: the watch after the close brings it back (RegressCase) at the first event of its signal, or of
// its own evidence, from the watch's start.
func (s *Store) CloseGuarded(ctx context.Context, id string, incidents, exposure, watched int, recur float64, effect, sources, actor string) (Case, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Case{}, err
	}
	defer tx.Rollback()
	c, err := getCase(ctx, tx, id)
	if err != nil {
		return Case{}, err
	}
	switch {
	case c.Status != CaseMeasuring || !c.Measurement.Guarded():
		return Case{}, badCase("case %s is %s: only a guarded measuring case closes as guarded", c.ID, c.Status)
	case strings.TrimSpace(effect) == "" || incidents < 0 || exposure < 0 || watched < 0 || recur < 0 || recur > 1:
		return Case{}, badCase("a guarded close records its history, its exposure watched, its probability and its watch")
	}
	now := time.Now()
	if _, err := tx.ExecContext(ctx, `UPDATE cases SET status = ?, reason = ?, closed = ?, updated = ?, after_n = 0, effect = ?, before_n = ?,
	  guard_exposure = ?, guard_watched = ?, guard_recur = ?, closed_evidence = (SELECT COUNT(*) FROM case_evidence WHERE case_id = ?) WHERE id = ?`,
		CaseClosed, string(Guarded), now.UnixMilli(), now.UnixMilli(), caseText(effect, caseTextMax), incidents, exposure, watched, recur, c.ID, c.ID); err != nil {
		return Case{}, err
	}
	line := string(Guarded) + ": " + effect
	if sources != "" {
		line += "; " + sources
	}
	if err := caseLog(ctx, tx, c.ID, actor, "closed", line, now); err != nil {
		return Case{}, err
	}
	if c, err = getCase(ctx, tx, c.ID); err != nil {
		return Case{}, err
	}
	return c, tx.Commit()
}

// GuardRecurred is the measure tool's: a guarded measuring case whose problem came back while its rollback is held
// keeps how many units of it came back, so its status and view say it recurred.
func (s *Store) GuardRecurred(ctx context.Context, id string, units int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE cases SET after_n = ?, updated = ? WHERE id = ? AND status = ? AND guard_exposure > 0`,
		units, time.Now().UnixMilli(), id, CaseMeasuring)
	return err
}

// RegressCase is the measure tool's: a case closed as improved whose problem is back at its old rate comes back,
// open, the same case with its history, its log saying it regressed and why.
func (s *Store) RegressCase(ctx context.Context, id, why, actor string) (Case, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Case{}, err
	}
	defer tx.Rollback()
	c, err := getCase(ctx, tx, id)
	if err != nil {
		return Case{}, err
	}
	if c.Status != CaseClosed || c.Reason != Improved && c.Reason != Guarded {
		return Case{}, badCase("case %s is %s %s: only a case closed as improved or guarded regresses", c.ID, c.Status, c.Reason)
	}
	now := time.Now()
	if _, err := tx.ExecContext(ctx, `UPDATE cases SET episode = episode + 1, status = ?, reason = '', closed = 0, closed_evidence = 0, updated = ?, back = ?,
	  `+clearGuard+` WHERE id = ?`, CaseOpen, now.UnixMilli(), now.UnixMilli(), c.ID); err != nil {
		return Case{}, err
	}
	if err := caseLog(ctx, tx, c.ID, actor, "regressed", "closed as "+string(c.Reason)+"; "+why, now); err != nil {
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
