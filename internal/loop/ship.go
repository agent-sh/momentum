package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/scanner"
	"time"

	"github.com/agent-sh/momentum/internal/measure"
	"github.com/agent-sh/momentum/internal/store"
)

// Ship is the one way a change of the loop's goes in: a change package per case, built in an independent run clone
// and imported as a patch into the host clone, checked by a test that fails without the change and passes with
// it, proposed as a pull request with a rollback and a measurement plan, reviewed in a clean context, revised for
// what a review finds (three rounds at most), and landed only when the loop is live and the last review found
// nothing high or medium. Every step checks the authority again (Gate). A configuration directory is no target:
// the loop changes only repositories, through pull requests.
type Ship struct {
	SourceLive     func(string) bool // nil means every recorded source is live
	InstalledRoots []string          // admitted host read directories for a build report's installed file
	Store          *store.Store
	Gate           Gate
	Sandbox        Sandbox
	Dir            string // the loop's directory: packages/<case>, host/<case> and work/<case> under it
	Live           bool
	Actor          string
	Now            func() time.Time
	// Identity is the git author and committer every commit of the loop's takes: the user's own, from his global
	// git configuration, never a repository's override or the environment's.
	Identity [2]string
	GH       string // the GitHub CLI
	// Env is what a check's test adds to the sandbox's own environment (a toolchain's settings); nothing of the
	// user's session.
	Env []string
	// Public makes a text the loop posts publishable (loop.Public: secrets scrubbed, mentions neutral).
	Public func(ctx context.Context, text string) (string, error)
	// BeforeCommit is a test hook for an edit after the proposal's gates.
	BeforeCommit func()
	// AfterPatchRead is a test hook after the one confined read.
	AfterPatchRead func()
}

// Package states.
const (
	Prepared    = "prepared"
	Checked     = "checked"
	Proposed    = "proposed"
	Landing     = "landing"
	Revising    = "revising" // a review found something: back to its build, on the same branch and pull request
	Landed      = "landed"
	RollingBack = "rolling back"
	RolledBack  = "rolled back"
	Kept        = "kept"
)

// MaxRounds is how many review rounds a package gets: prs.md's three.
const MaxRounds = 3

// MaxClosedProposals bounds automatic retries of closed, unmerged requests.
const MaxClosedProposals = 2

// Package is a case's change: where it goes, what checks it, how it is measured and undone, and where it stands.
type Package struct {
	Case            string           `json:"case"`
	Kind            string           `json:"kind"` // repo
	Repo            string           `json:"repo,omitempty"`
	Base            string           `json:"base,omitempty"`
	Branch          string           `json:"branch,omitempty"`
	Dir             string           `json:"dir"`         // the repository's main checkout
	Work            string           `json:"work"`        // independent run clone, never read by host steps
	Host            string           `json:"host"`        // host-only independent clone
	BaseCommit      string           `json:"base_commit"` // immutable patch base
	PublishedHead   string           `json:"published_head,omitempty"`
	RefreshBase     bool             `json:"refresh_base,omitempty"`
	State           string           `json:"state"`
	Check           *Check           `json:"check,omitempty"`
	Plan            *Plan            `json:"plan,omitempty"`
	Rollback        string           `json:"rollback,omitempty"`
	Title           string           `json:"title,omitempty"`
	Body            string           `json:"body,omitempty"`
	Lesson          string           `json:"lesson,omitempty"`
	PR              string           `json:"pr,omitempty"`
	LandingHead     string           `json:"landing_head,omitempty"`
	Cleanup         string           `json:"cleanup,omitempty"`
	Head            string           `json:"head,omitempty"` // the commit proposed
	Reviews         []Review         `json:"reviews,omitempty"`
	ClosedProposals int              `json:"closed_proposals,omitempty"`
	Landed          []string         `json:"landed,omitempty"` // the commits that went in
	LandedAt        time.Time        `json:"landed_at,omitempty"`
	Undo            *RollbackJournal `json:"undo,omitempty"`
	Changed         []string         `json:"changed,omitempty"` // the paths the change touches
	Created         time.Time        `json:"created"`
	Updated         time.Time        `json:"updated"`
	// What the measure step found and did: the measurement that kept the change, the problem note it stands for
	// now (measure:CASE), the rollback it holds while the loop is not live, and its rollbacks that failed in a row.
	Result           *measure.Result  `json:"result,omitempty"`
	Problem          string           `json:"problem,omitempty"`
	Held             bool             `json:"held,omitempty"`
	Omitted          []string         `json:"omitted,omitempty"`
	WatchSources     []string         `json:"watch_sources,omitempty"`
	InstalledAt      time.Time        `json:"installed_at,omitzero"`
	InstalledHash    string           `json:"installed_hash,omitempty"`
	InstalledOK      bool             `json:"installed_ok,omitempty"`
	InstalledGo      bool             `json:"installed_go,omitempty"`  // the decision for this file was read from its Go build information
	InstalledWhy     string           `json:"installed_why,omitempty"` // and, when it is no, why
	WatchRecent      []measure.Counts `json:"watch_recent,omitempty"`  // last three completed watch windows
	CheckoutAt       time.Time        `json:"checkout_at,omitzero"`    // first observed checkout time, kept for an installed build
	ProposalAt       time.Time        `json:"proposal_at,omitzero"`    // boundary of the latest waiting proposal
	ReadUntil        time.Time        `json:"read_until,omitzero"`     // end of the window whose sources are behind
	ReadWait         time.Time        `json:"read_wait,omitzero"`      // first run waiting for a complete source read
	WatchWindow      int              `json:"watch_window,omitempty"`  // last completed window read since the close
	OutOfEffect      time.Time        `json:"out_of_effect,omitzero"`  // first run that found the measuring change absent
	RollbackFailures int              `json:"rollback_failures,omitempty"`
	RollbackProblem  string           `json:"rollback_problem,omitempty"` // cleaned cause of the last failed rollback
}

// RollbackJournal keeps the exact revert candidate before any remote write. A retry checks this head on the
// remote and finishes its receipt rather than reverting a second time.
type RollbackJournal struct {
	Head    string   `json:"head"`
	Commits []string `json:"commits"`
	Why     string   `json:"why"`
}

// Check is the evidence that a test discriminates the change: it fails on the code without the change (the
// test's own files kept) and passes with it, both run on what the change commits (its tree), not on the files
// beside it in the work tree.
type Check struct {
	Test         []string  `json:"test"`
	TestFiles    []string  `json:"test_files"`
	Before       Outcome   `json:"before"`
	After        Outcome   `json:"after"`
	Tree         string    `json:"tree"` // the tree it checked: an edit after it needs a new check
	At           time.Time `json:"at"`
	CommandInput string    `json:"command_input,omitempty"` // host-only execution data; Test is display text
}

// Plan is how the change is measured: a signal over the store's events, counted in units of activity over
// the three windows before the gated window and one window after it is in effect (internal/measure).
type Plan struct {
	Sources    []string      `json:"sources,omitempty"` // signal and every source in the counted span
	Omitted    []string      `json:"omitted,omitempty"`
	Signal     Signal        `json:"signal"`
	Window     time.Duration `json:"window"`
	Units      string        `json:"units"`       // what it counts in: session hours, owner hours or machine hours
	Before     int           `json:"before"`      // the units the signal counted in the three windows before the gate
	BeforeBase int           `json:"before_base"` // the units of activity in it
	Gate       int           `json:"gate"`        // hits in the last window before the plan
	GateBase   int           `json:"gate_base"`   // activity in that window
	At         time.Time     `json:"at"`          // when it was counted: the gated window ends here
	// Installed is the built or installed file the machine runs (a binary, an installed configuration):
	// a recorded Go revision must hold the change; other files are checked by their hash and time.
	Installed string `json:"installed,omitempty"`
}

// Signal is a deterministic filter of the store's events: of a source and kinds, of a tool and a harness when
// named, whose summary holds every one of Match's texts (case blind). It counts the problem the case is about.
type Signal struct {
	Source  string   `json:"source"`
	Kinds   []string `json:"kinds"`
	Tool    string   `json:"tool,omitempty"`
	Harness string   `json:"harness,omitempty"`
	Match   []string `json:"match"`
}

// Finding is one finding of a review.
type Finding struct {
	Severity string `json:"severity"` // high, medium or low
	Where    string `json:"where"`
	Problem  string `json:"problem"`
	Fix      string `json:"fix"`
}

// Review is one round of the clean-context review.
type Review struct {
	Round    int       `json:"round"`
	High     int       `json:"high"`
	Medium   int       `json:"medium"`
	Low      int       `json:"low"`
	Findings []Finding `json:"findings,omitempty"`
	Head     string    `json:"head"`
	Comment  string    `json:"comment,omitempty"` // where it was posted
	Text     string    `json:"text"`
	At       time.Time `json:"at"`
}

func (s *Ship) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Ship) pkgDir(c string) string  { return filepath.Join(s.Dir, "packages", c) }
func (s *Ship) workDir(c string) string { return filepath.Join(s.Dir, "work", c) }

// Load reads a case's package; ok is false when it has none.
func (s *Ship) Load(caseID string) (Package, bool, error) {
	if caseID == "" || caseID != filepath.Base(caseID) || strings.HasPrefix(caseID, ".") {
		return Package{}, false, refused("%q is no case id", caseID)
	}
	raw, err := os.ReadFile(filepath.Join(s.pkgDir(caseID), "package.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return Package{}, false, nil
	}
	if err != nil {
		return Package{}, false, err
	}
	var p Package
	if err := json.Unmarshal(raw, &p); err != nil {
		return Package{}, false, fmt.Errorf("%s's package: %w", caseID, err)
	}
	return p, true, nil
}

func (s *Ship) save(p *Package) error {
	p.Updated = s.now()
	dir := s.pkgDir(p.Case)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".package.json.new")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "package.json"))
}

type shipCaseKey struct{}

func shipContext(ctx context.Context, id string) context.Context {
	return store.WithTextCleaner(context.WithValue(ctx, shipCaseKey{}, id), Redact)
}

func (s *Ship) authority(ctx context.Context, id string) error {
	c, err := s.Store.CaseByID(ctx, id)
	if err != nil {
		return err
	}
	if c.Into != "" {
		return refused("the assigned case was merged; no ship side effect is authorized")
	}
	return nil
}

func (s *Ship) assignedAuthority(ctx context.Context) error {
	if id, ok := ctx.Value(shipCaseKey{}).(string); ok {
		return s.authority(ctx, id)
	}
	return nil
}

func cleanedNote(ctx context.Context) context.Context {
	return store.WithTextCleaner(ctx, func(_ context.Context, text string) (string, error) { return text, nil })
}

func (s *Ship) saveAuthorized(ctx context.Context, p *Package) error {
	if err := s.authority(ctx, p.Case); err != nil {
		return err
	}
	if p.Cleanup != "" {
		clean, err := Redact(ctx, p.Cleanup)
		if err != nil {
			return err
		}
		p.Cleanup = clean
	}
	return s.save(p)
}

// working reads a case that a change may be built for: in testing, as the investigation left it.
func (s *Ship) working(ctx context.Context, caseID string) (store.Case, error) {
	c, err := s.Store.CaseByID(ctx, caseID)
	if err != nil {
		return store.Case{}, err
	}
	if c.Status != store.CaseTesting {
		return store.Case{}, refused("case %s is %s: a change is built only for a case in testing (a mechanism and a discriminating test on record)", c.ID, c.Status)
	}
	return c, nil
}

// public is a text as the loop may post it.
func (s *Ship) public(ctx context.Context, text string) (string, error) {
	if s.Public == nil {
		return Public(ctx, text)
	}
	return s.Public(ctx, text)
}

// Prepare creates a host-only clone and an independent run clone before any writable mount uses it.
func (s *Ship) Prepare(ctx context.Context, caseID, repoDir string) (Package, error) {
	ctx = s.gitContext(shipContext(ctx, caseID))
	unlock, err := s.lock(ctx, caseID)
	if err != nil {
		return Package{}, err
	}
	defer unlock()
	c, err := s.working(ctx, caseID)
	if err != nil {
		return Package{}, err
	}
	old, had, err := s.Load(c.ID)
	if err != nil {
		return Package{}, err
	}
	if had && old.State != RolledBack && old.State != Kept {
		return old, refused("case %s has a package already (in %s)", c.ID, old.State)
	}
	if repoDir == "" {
		return Package{}, refused("prepare needs --repo, a host repository")
	}
	abs, err := filepath.Abs(repoDir)
	if err != nil {
		return Package{}, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Package{}, err
	}
	if under(abs, s.Dir) || under(real, s.Dir) || s.Sandbox.Cache != "" && under(real, s.Sandbox.Cache) {
		return Package{}, refused("prepare reads host repositories, never the loop's clones, run directories or writable caches")
	}
	abs = real
	t, err := s.Gate.Repository(ctx, abs)
	if err != nil {
		return Package{}, err
	}
	if had {
		if err := s.archive(old); err != nil {
			return Package{}, err
		}
	}
	host, work := filepath.Join(s.Dir, "host", c.ID), s.workDir(c.ID)
	if err := s.authority(ctx, c.ID); err != nil {
		return Package{}, err
	}
	for _, d := range []string{host, work} {
		if err := os.MkdirAll(filepath.Dir(d), 0700); err != nil {
			return Package{}, err
		}
		if err := removeAll(d); err != nil {
			return Package{}, err
		}
	}
	if err := s.cloneConfined(ctx, t.Dir, host); err != nil {
		return Package{}, err
	}
	origin, err := gitOutput(ctx, t.Dir, "remote", "get-url", "origin")
	if err != nil {
		return Package{}, err
	}
	if _, err := s.git(ctx, host, "remote", "set-url", "origin", strings.TrimSpace(origin)); err != nil {
		return Package{}, err
	}
	if _, err := s.git(ctx, host, "fetch", "--quiet", "origin", t.Base); err != nil {
		return Package{}, err
	}
	branch := "momentum/" + c.ID
	if _, err := s.git(ctx, host, "checkout", "--quiet", "-B", branch, "origin/"+t.Base); err != nil {
		return Package{}, err
	}
	base, err := s.git(ctx, host, "rev-parse", "HEAD")
	if err != nil {
		return Package{}, err
	}
	p := Package{Case: c.ID, Kind: "repo", Repo: t.Repo, Base: t.Base, BaseCommit: strings.TrimSpace(base), Dir: t.Dir, Branch: branch,
		Work: work, Host: host, State: Prepared, Created: s.now()}
	// A rolled-back package may still have a proposal branch after refused cleanup.
	p.PublishedHead, err = s.remoteBranch(ctx, p)
	if err != nil {
		return Package{}, err
	}
	if err := s.freshRunClone(ctx, p); err != nil {
		return Package{}, err
	}
	note, err := Redact(ctx, fmt.Sprintf("%s at %s", p.Repo, p.BaseCommit))
	if err != nil {
		return Package{}, err
	}
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return Package{}, err
	}
	return p, s.Store.CaseNote(cleanedNote(ctx), c.ID, "prepared", note, s.Actor)
}

// freshRunClone is used only before a run starts. No later host operation inspects this clone.
func (s *Ship) freshRunClone(ctx context.Context, p Package) error {
	if err := s.assignedAuthority(ctx); err != nil {
		return err
	}
	if err := removeAll(p.Work); err != nil {
		return err
	}
	// Build a fresh clone in host-only storage, before exposing it to a run.
	staging := filepath.Join(s.Dir, "host", "."+p.Case+"-build")
	if err := removeAll(staging); err != nil {
		return err
	}
	defer func() { _ = removeAll(staging) }()
	if err := s.cloneConfined(ctx, p.Host, staging); err != nil {
		return err
	}
	if _, err := hostGit(ctx, staging, nil, "remote", "remove", "origin"); err != nil {
		return err
	}
	if _, err := hostGit(ctx, staging, nil, "checkout", "--quiet", "--detach", p.BaseCommit); err != nil {
		return err
	}
	// the reflog records this host's paths and generated identity; a run has no use for it
	if err := removeAll(filepath.Join(staging, ".git", "logs")); err != nil {
		return err
	}
	return os.Rename(staging, p.Work)
}

// refreshBase runs before a replacement build is mounted. The old patch is kept in host package data.
func (s *Ship) refreshBase(ctx context.Context, p Package) (Package, error) {
	ctx = s.gitContext(shipContext(ctx, p.Case))
	unlock, err := s.lock(ctx, p.Case)
	if err != nil {
		return p, err
	}
	defer unlock()
	return s.refreshBaseLocked(ctx, p)
}

func (s *Ship) refreshBaseLocked(ctx context.Context, p Package) (Package, error) {
	if roundsUsed(p) {
		return p, refused("review rounds used; the change waits for a person")
	}
	if _, err := s.Gate.Repository(ctx, p.Dir); err != nil {
		return p, err
	}
	if _, err := s.git(ctx, p.Host, "fetch", "--quiet", "origin", p.Base); err != nil {
		return p, err
	}
	base, err := s.git(ctx, p.Host, "rev-parse", "origin/"+p.Base)
	if err != nil {
		return p, err
	}
	p.BaseCommit = strings.TrimSpace(base)
	if _, err := s.git(ctx, p.Host, "reset", "--hard", p.BaseCommit); err != nil {
		return p, err
	}
	if err := s.freshRunClone(ctx, p); err != nil {
		return p, err
	}
	p.Check, p.Head, p.RefreshBase, p.State = nil, "", false, Prepared
	return p, s.saveAuthorized(ctx, &p)
}

// testFileRe are the names a test file has in the languages the owned repositories use.
var testFileRe = regexp.MustCompile(`(_test\.go|\.test\.[cm]?[jt]sx?|\.spec\.[cm]?[jt]sx?|(^|/)test_[^/]*\.py|_test\.py|(^|/)tests?/.*|\.bats)$`)

// Check runs the case's test twice in the sandbox, each time in a tree of the change's commit as it would be (no
// ignored file of the work tree in it): the default branch with only the test's own files of the change (it must
// fail), and the change (it must pass). The test's files are those the change touches that are named as tests, or
// the ones named. A check that does not come out so is recorded as it came out, and the package stays unchecked.
func (s *Ship) Check(ctx context.Context, caseID, test string, testFiles []string, patch ...Patch) (pkg Package, stepErr error) {
	ctx = s.gitContext(shipContext(ctx, caseID))
	unlock, err := s.lock(ctx, caseID)
	if err != nil {
		return Package{}, err
	}
	defer unlock()
	if _, err := s.working(ctx, caseID); err != nil {
		return Package{}, err
	}
	p, ok, err := s.Load(caseID)
	if err != nil {
		return Package{}, err
	}
	if !ok || (p.State != Prepared && p.State != Checked && p.State != Revising) {
		return p, refused("case %s has no package to check (prepare it first; a proposed one is reviewed, not checked again)", caseID)
	}
	if p.Host == "" || p.BaseCommit == "" {
		return p, refused("legacy package has no host clone and patch base; prepare a fresh package")
	}
	if _, err := s.Gate.Repository(ctx, p.Dir); err != nil {
		return p, err
	}
	words, err := Words(test)
	if err != nil {
		return p, err
	}
	tree, paths, err := s.applyPatch(ctx, p, patch)
	if err != nil {
		return p, err
	}
	if len(paths) == 0 {
		return p, refused("the patch is empty: produce a cumulative diff against %s", p.BaseCommit)
	}
	if err := CheckPaths(Target{Repo: p.Repo}, paths); err != nil {
		return p, err
	}
	if len(testFiles) == 0 {
		for _, c := range paths {
			if testFileRe.MatchString(c) {
				testFiles = append(testFiles, c)
			}
		}
	}
	if len(testFiles) == 0 {
		return p, refused("the change holds no test file (by its name, or --test-files): a change ships with a test that fails without it")
	}
	for _, f := range testFiles {
		if !slices.Contains(paths, f) {
			return p, refused("%s is not a file the change touches", f)
		}
	}
	before, err := s.export(ctx, p, "before", p.BaseCommit)
	if err != nil {
		return p, err
	}
	exports := []string{before}
	cleanup := func() error {
		pending := exports
		exports = nil
		var err error
		for _, dir := range pending {
			err = errors.Join(err, s.unexport(p, dir))
		}
		return err
	}
	defer func() { stepErr = errors.Join(stepErr, cleanup()) }()
	for _, f := range testFiles {
		b, err := s.git(ctx, p.Host, "show", tree+":"+f)
		if err != nil {
			return p, err
		}
		entry, err := s.git(ctx, p.Host, "ls-tree", tree, "--", f)
		if err != nil {
			return p, err
		}
		mode := os.FileMode(0o644)
		switch fields := strings.Fields(entry); {
		case len(fields) < 3 || fields[1] != "blob":
			return p, refused("test file is not a regular blob: %s", f)
		case fields[0] == "100755":
			mode = 0o755
		case fields[0] != "100644":
			return p, refused("test file has no accepted mode: %s", f)
		}
		if err := writeTest(before, f, []byte(b), mode); err != nil {
			return p, err
		}
	}
	after, err := s.export(ctx, p, "after", tree)
	if err != nil {
		return p, err
	}
	exports = append(exports, after)
	b, err := s.Sandbox.Run(ctx, before, words, s.Env)
	if err != nil {
		return p, err
	}
	if err := s.authority(ctx, caseID); err != nil {
		return p, err
	}
	a, err := s.Sandbox.Run(ctx, after, words, s.Env)
	if err != nil {
		return p, err
	}
	if err := cleanup(); err != nil {
		return p, err
	}
	// Execute the original words. Persist only their cleaned display form.
	displayText, err := Redact(ctx, test)
	if err != nil {
		return p, err
	}
	display, err := commandWords(displayText, false)
	if err != nil {
		return p, err
	}
	note, err := Redact(ctx, fmt.Sprintf("%s: fails without the change (exit %d), passes with it", strings.Join(words, " "), b.Exit))
	if err != nil {
		return p, err
	}
	if err := s.authority(ctx, caseID); err != nil {
		return p, err
	}
	const commandInput = "test-command.json"
	ck := &Check{Test: display, TestFiles: testFiles, Before: b, After: a, Tree: tree, At: s.now(), CommandInput: commandInput}
	if err := writeJSON(filepath.Join(s.pkgDir(caseID), commandInput), checkCommand{Tree: tree, Args: words}); err != nil {
		return p, err
	}
	if err := writeJSON(filepath.Join(s.pkgDir(caseID), "check.json"), ck); err != nil {
		return p, err
	}
	switch {
	case b.Exit == 0:
		return p, refused("the test passes without the change too (exit 0 on the base with the test's files): it does not discriminate the change; the check is in check.json")
	case a.Exit != 0:
		return p, refused("the test fails with the change (exit %d): the change does not pass its own test; the check is in check.json", a.Exit)
	}
	p.Check, p.State, p.Changed = ck, Checked, paths
	if err := s.keepPatch(p); err != nil {
		return p, err
	}
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, err
	}
	return p, s.Store.CaseNote(cleanedNote(ctx), caseID, "checked", note, s.Actor)
}

// Proposal is what a change is proposed with.
type Proposal struct {
	Installed                     string // built or installed file the machine runs
	Title, Body, Rollback, Lesson string
	Signal                        Signal
	Window                        time.Duration
}

// MinSignal is the least the measurement's signal must count over the window before the change: below it, no
// window could show the change helped.
const MinSignal = measure.MinBefore

// Propose makes the checked change a proposal: it checks the measurement plan against the case's own evidence,
// commits the change as the user, checks the gate on the committed tree, pushes the branch and opens its pull
// request, or, for a revision after a review, updates the one it has. The change must be the one the check passed.
// A proposal that failed after its commit is taken up again where it stopped.
func (s *Ship) Propose(ctx context.Context, caseID string, pr Proposal, patch ...Patch) (Package, error) {
	ctx = s.gitContext(shipContext(ctx, caseID))
	unlock, err := s.lock(ctx, caseID)
	if err != nil {
		return Package{}, err
	}
	defer unlock()
	c, err := s.working(ctx, caseID)
	if err != nil {
		return Package{}, err
	}
	p, ok, err := s.Load(caseID)
	if err != nil {
		return Package{}, err
	}
	if !ok || p.State != Checked || p.Check == nil {
		return p, refused("case %s has no checked package: a change is proposed only after its check passed", caseID)
	}
	tree, paths, err := s.applyPatch(ctx, p, patch)
	if err != nil {
		return p, err
	}
	if tree != p.Check.Tree {
		return p, refused("the change differs from the one the check passed: check it again")
	}
	parent, err := s.git(ctx, p.Host, "rev-parse", firstOf(p.Head, p.BaseCommit))
	if err != nil {
		return p, err
	}
	parent = strings.TrimSpace(parent)
	if err := CheckPaths(Target{Repo: p.Repo}, paths); err != nil {
		return p, err
	}
	switch {
	case strings.TrimSpace(pr.Title) == "" || strings.TrimSpace(pr.Body) == "":
		return p, refused("a proposal needs a title and a body (what changes, why, the evidence, the prior art)")
	case strings.TrimSpace(pr.Rollback) == "":
		return p, refused("a proposal needs its rollback: how the change is undone")
	}
	plan, err := s.validPlan(ctx, c, pr.Signal, pr.Window, pr.Installed, s.now())
	if err != nil {
		var wait *waitingPlan
		if errors.As(err, &wait) {
			if waitErr := s.waitProposal(ctx, &p, plan, wait); waitErr != nil {
				return p, waitErr
			}
		}
		return p, err
	}
	p.ProposalAt = time.Time{}
	if !p.ReadWait.IsZero() {
		p.ReadWait, p.ReadUntil = time.Time{}, time.Time{}
		s.runner().resolve(ctx, &p)
	}
	if err := s.editsSignal(ctx, p, pr.Signal); err != nil {
		return p, err
	}
	title, err := s.public(ctx, strings.TrimSpace(pr.Title))
	if err != nil {
		return p, err
	}
	body, err := s.public(ctx, strings.TrimSpace(pr.Body))
	if err != nil {
		return p, err
	}
	private, err := s.repositoryPrivate(ctx, p)
	if err != nil {
		return p, err
	}
	commitTitle, commitBody := title, body
	if !private {
		commitTitle = fmt.Sprintf("Change for case %s", p.Case)
		commitBody = fmt.Sprintf("Base: %s\nChecked tree: %s", p.BaseCommit, tree)
	}
	p.Plan, p.Rollback, p.Title, p.Body, p.Lesson, p.Changed = &plan, pr.Rollback,
		title, body, pr.Lesson, paths
	for _, field := range []*string{&p.Rollback, &p.Lesson} {
		*field, err = Redact(ctx, *field)
		if err != nil {
			return p, err
		}
	}
	// A review cannot be satisfied by a new commit with the same tree.
	if n := len(p.Reviews); n > 0 {
		reviewed, err := s.git(ctx, p.Host, "rev-parse", p.Reviews[n-1].Head+"^{tree}")
		if err != nil {
			return p, err
		}
		if strings.TrimSpace(reviewed) == tree {
			return p, refused("the revision changes nothing of the tree the review read")
		}
	}
	if got, err := s.git(ctx, p.Host, "rev-parse", parent+"^{tree}"); err != nil || strings.TrimSpace(got) != tree {
		if s.BeforeCommit != nil {
			s.BeforeCommit()
		}
		if err := s.commitTree(ctx, p, tree, parent, fmt.Sprintf("%s\n\n%s\n\nCase %s.", commitTitle, wrap(commitBody), c.ID)); err != nil {
			return p, err
		}
	} else {
		if _, err := s.git(ctx, p.Host, "update-ref", "refs/heads/"+p.Branch, parent); err != nil {
			return p, err
		}
	}
	head, err := s.git(ctx, p.Host, "rev-parse", "HEAD")
	if err != nil {
		return p, err
	}
	p.Head = strings.TrimSpace(head)
	committedTree, err := s.git(ctx, p.Host, "rev-parse", p.Head+"^{tree}")
	if err != nil || strings.TrimSpace(committedTree) != tree {
		return p, refused("the commit is not the checked tree")
	}
	if n := len(p.Reviews); n > 0 && p.Reviews[n-1].Head == p.Head {
		return p, refused("the revision changes nothing of the commit the review read: change what its findings name")
	}
	if err := s.keepPatch(p); err != nil {
		return p, err
	}
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, err
	}
	if _, err := s.Gate.Repository(ctx, p.Dir); err != nil {
		return p, err
	}
	if err := s.publish(ctx, &p, p.Head); err != nil {
		return p, err
	}
	text, err := s.public(ctx, s.packageText(c, p))
	if err != nil {
		return p, err
	}
	private, err = s.repositoryPrivate(ctx, p)
	if err != nil {
		return p, err
	}
	postTitle, postBody := title, body+"\n\n"+text
	if !private {
		postTitle = fmt.Sprintf("Change for case %s", p.Case)
		postBody, err = s.hostPackageText(ctx, c, p)
		if err != nil {
			return p, err
		}
	}
	if p.PR == "" {
		if err := s.saveAuthorized(ctx, &p); err != nil {
			return p, err
		}
		// Creation can succeed while its response is lost. Match the package's exact branch, base and commit
		// before issuing another create. A closed request does not reclaim a new proposal.
		list, err := s.gh(ctx, p.Host, "pr", "list", "--repo", p.Repo, "--head", p.Branch, "--base", p.Base, "--state", "all",
			"--json", "number,url,headRefOid,state")
		if err != nil {
			return p, err
		}
		var prs []struct {
			Number int    `json:"number"`
			URL    string `json:"url"`
			Head   string `json:"headRefOid"`
			State  string `json:"state"`
		}
		if err := json.Unmarshal([]byte(list), &prs); err != nil {
			return p, fmt.Errorf("read existing pull requests: %w", err)
		}
		for _, pr := range prs {
			if pr.State != "CLOSED" && pr.Head == p.Head && pr.Number > 0 && pr.URL != "" {
				if p.PR != "" {
					return p, refused("multiple pull requests match the package's commit")
				}
				p.PR = pr.URL
			}
		}
		if p.PR == "" {
			url, err := s.gh(ctx, p.Host, "pr", "create", "--repo", p.Repo, "--base", p.Base, "--head", p.Branch, "--title", postTitle, "--body", postBody)
			if err != nil {
				return p, err
			}
			p.PR = strings.TrimSpace(lastOf(strings.Split(strings.TrimSpace(url), "\n")))
		}
	} else if _, err := s.gh(ctx, p.Host, "pr", "edit", p.PR, "--repo", p.Repo, "--title", postTitle, "--body", postBody); err != nil {
		s.saveAuthorized(ctx, &p)
		return p, err
	}
	p.State = Proposed
	note, err := Redact(ctx, fmt.Sprintf("%s %s; measured in %s: %d of %d in the baseline; gate %d of %d", p.PR, title, plan.Units, plan.Before, plan.BeforeBase, plan.Gate, plan.GateBase))
	if err != nil {
		return p, err
	}
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, err
	}
	return p, s.Store.CaseNote(cleanedNote(ctx), caseID, "proposed", note, s.Actor)
}

// editsSignal refuses a change that edits the texts its own measurement counts: a change of words that only stops
// the signal from matching would measure as a fix. It also reads neighboring
// literals, since a removed fragment can belong to a split signal.
func (s *Ship) editsSignal(ctx context.Context, p Package, sig Signal) error {
	// Git's maximum context includes the whole old file, even when only one
	// fragment changes. The host's normal output bound still applies.
	diff, err := s.git(ctx, p.Host, "diff", "--no-renames", "--no-ext-diff", "--no-textconv", "--text", "-U2147483647", p.BaseCommit, p.Check.Tree)
	if err != nil {
		return err
	}
	var removed []string
	var old []string
	var changed []bool
	flush := func() {
		removed = append(removed, changedLiterals(old, changed)...)
		old, changed = nil, nil
	}
	inHunk := false
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			flush()
			inHunk = false
		case strings.HasPrefix(l, "@@"):
			flush()
			inHunk = true
		case inHunk && strings.HasPrefix(l, "-"):
			removed = append(removed, strings.ToLower(l[1:]))
			old, changed = append(old, l[1:]), append(changed, true)
		case inHunk && strings.HasPrefix(l, " "):
			old, changed = append(old, l[1:]), append(changed, false)
		}
	}
	flush()
	for _, m := range sig.Match {
		m = strings.ToLower(m)
		for _, l := range removed {
			if strings.Contains(l, m) {
				return refused("the change edits %q, a text its measurement counts: measure what the agents do, not the words the change rewrites", m)
			}
		}
	}
	return nil
}

// changedLiterals joins adjacent quoted literals, with an optional + between
// them. It returns only sequences with an edited literal or joining operator.
// This is a source-text guard, not evaluation of arbitrary program expressions.
func changedLiterals(lines []string, changed []bool) []string {
	var scan scanner.Scanner
	scan.Init(strings.NewReader(strings.Join(lines, "\n")))
	scan.Mode = scanner.ScanStrings | scanner.ScanRawStrings | scanner.ScanComments | scanner.SkipComments
	scan.Error = func(*scanner.Scanner, string) {} // arbitrary repository text is not necessarily Go
	var joined strings.Builder
	var out []string
	touched := false
	flush := func() {
		if touched {
			out = append(out, strings.ToLower(joined.String()))
		}
		joined.Reset()
		touched = false
	}
	mark := func(first, last int) {
		for line := first; line <= last && line <= len(changed); line++ {
			if line > 0 && changed[line-1] {
				touched = true
			}
		}
	}
	for token := scan.Scan(); token != scanner.EOF; token = scan.Scan() {
		switch token {
		case scanner.String, scanner.RawString:
			text, err := strconv.Unquote(scan.TokenText())
			if err != nil {
				flush()
				continue
			}
			joined.WriteString(text)
			mark(scan.Position.Line, scan.Pos().Line)
		case '+':
			if joined.Len() > 0 {
				mark(scan.Position.Line, scan.Position.Line)
			}
		default:
			flush()
		}
	}
	flush()
	return out
}

// Reviewed records a round of the clean-context review on the proposed head: its findings, counted by severity
// from the findings themselves, and where it was posted. A review that finds something high or medium sends the
// package back to its build (revising), until its rounds are used.
func (s *Ship) Reviewed(ctx context.Context, caseID string, r Review) (Package, error) {
	ctx = s.gitContext(shipContext(ctx, caseID))
	unlock, err := s.lock(ctx, caseID)
	if err != nil {
		return Package{}, err
	}
	defer unlock()
	p, ok, err := s.Load(caseID)
	if err != nil {
		return Package{}, err
	}
	if !ok || p.State != Proposed {
		return p, refused("case %s has no proposed package to review", caseID)
	}
	if r.Head == "" {
		return p, refused("the review head differs from the proposed head")
	}
	if r.Head != p.Head {
		// the package moved on while the review ran: no fault of the reviewer's, and the next pass reviews the new head
		return p, &shipHold{err: refused("the review head differs from the proposed head")}
	}
	if len(p.Reviews) >= MaxRounds {
		return p, refused("review rounds used; the change waits for a person")
	}
	for _, previous := range p.Reviews {
		if previous.Head == p.Head {
			return p, refused("this head has already been reviewed")
		}
	}
	if strings.TrimSpace(r.Text) == "" {
		return p, refused("a review needs a nonempty summary")
	}
	// Persist only cleaned model text. Posting later must not be the first scrub.
	clean, err := Redact(ctx, r.Text)
	if err != nil {
		return p, err
	}
	r.Text = clean
	r.Findings = append([]Finding(nil), r.Findings...)
	for i := range r.Findings {
		for _, field := range []*string{&r.Findings[i].Where, &r.Findings[i].Problem, &r.Findings[i].Fix} {
			clean, err := Redact(ctx, *field)
			if err != nil {
				return p, err
			}
			*field = clean
		}
	}
	if len(r.Findings) > 0 || (r.High == 0 && r.Medium == 0 && r.Low == 0) {
		h, m, l, err := countFindings(r.Findings)
		if err != nil {
			return p, err
		}
		if (r.High != 0 || r.Medium != 0 || r.Low != 0) && (h != r.High || m != r.Medium || l != r.Low) {
			return p, refused("the review counts %d high, %d medium, %d low, and its findings %d, %d, %d: the counts are the findings'",
				r.High, r.Medium, r.Low, h, m, l)
		}
		r.High, r.Medium, r.Low = h, m, l
	}
	r.Round, r.At = len(p.Reviews)+1, s.now()
	note, err := Redact(ctx, fmt.Sprintf("round %d: %d high, %d medium, %d low %s", r.Round, r.High, r.Medium, r.Low, r.Comment))
	if err != nil {
		return p, err
	}
	p.Reviews = append(p.Reviews, r)
	if r.Comment != "" && (r.High > 0 || r.Medium > 0) && len(p.Reviews) < MaxRounds {
		p.State = Revising
	}
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, err
	}
	return p, s.Store.CaseNote(cleanedNote(ctx), caseID, "reviewed", note, s.Actor)
}

func countFindings(fs []Finding) (h, m, l int, err error) {
	for _, f := range fs {
		switch strings.ToLower(f.Severity) {
		case "high":
			h++
		case "medium":
			m++
		case "low":
			l++
		default:
			return 0, 0, 0, refused("a finding of severity %q: high, medium or low", f.Severity)
		}
	}
	return h, m, l, nil
}

// Clean reports whether a package's last review found nothing high or medium on the commit it proposes now, and
// was posted on its pull request.
func (p Package) Clean() bool {
	if len(p.Reviews) == 0 {
		return false
	}
	r := p.Reviews[len(p.Reviews)-1]
	return r.High == 0 && r.Medium == 0 && r.Head == p.Head && r.Comment != ""
}

// Land puts a proposed change in: only when the loop is live, its last review is clean on the work tree's head,
// and the gate allows the repository again. Its branch is rebased on the default branch, checked again on its
// tree, pushed to its pull request when the rebase moved it, and pushed to the default branch as a fast-forward by
// the user's own identity (prs.md), so GitHub marks the pull request merged. The case is then deployed.
func (s *Ship) Land(ctx context.Context, caseID string) (Package, error) {
	ctx = s.gitContext(shipContext(ctx, caseID))
	unlock, err := s.lock(ctx, caseID)
	if err != nil {
		return Package{}, err
	}
	defer unlock()
	p, ok, err := s.Load(caseID)
	if err == nil && ok && p.State == Landing {
		if !s.Live {
			return p, refused("the loop is not live")
		}
		// a landing resumed after a crash is still a landing of a case in testing, as a new one is
		if _, err := s.working(ctx, caseID); err != nil {
			return p, err
		}
		// Restore only this package's interrupted operation before the clean-tree scope check.
		_, _ = s.git(ctx, p.Host, "rebase", "--abort")
		if _, err := s.git(ctx, p.Host, "reset", "--hard", p.Head); err != nil {
			return p, err
		}
		if _, err := s.Gate.Repository(ctx, p.Dir); err != nil {
			return p, err
		}
		if _, err := s.git(ctx, p.Host, "fetch", "--quiet", "origin", p.Base); err != nil {
			return p, err
		}
		if p.LandingHead != "" {
			if _, err := s.git(ctx, p.Host, "merge-base", "--is-ancestor", p.LandingHead, "origin/"+p.Base); err == nil {
				return s.finishLanding(ctx, p)
			}
			// A lost publication response can leave the remote on this exact head.
			// Keep and reconcile it before another rebase can produce a different one.
			if p.PublishedHead != p.LandingHead {
				if err := s.publish(ctx, &p, p.LandingHead); err != nil {
					return p, err
				}
			}
			_, relationErr := s.git(ctx, p.Host, "merge-base", "--is-ancestor", "origin/"+p.Base, p.LandingHead)
			var exit *exec.ExitError
			if relationErr != nil && (!errors.As(relationErr, &exit) || exit.ExitCode() != 1) {
				return p, relationErr // an uncertain read must retain the journal
			}
			if relationErr == nil {
				if err := s.openPR(ctx, p); err != nil {
					if errors.Is(err, errPRClosed) {
						return s.closedProposal(ctx, p)
					}
					return p, err
				}
				if _, err := s.push(ctx, p, p.Host, "--no-verify", "origin", p.LandingHead+":refs/heads/"+p.Base); err != nil {
					return p, refused("the journaled push to %s did not go as a fast-forward: %v", p.Base, err)
				}
				return s.finishLanding(ctx, p)
			}
			// Publication is known, but the default branch moved. Its receipt
			// now leases the next checked rebase; the reviewed head stays p.Head.
		}
		if _, err := s.git(ctx, p.Host, "reset", "--hard", p.Head); err != nil {
			return p, err
		}
		p.State, p.LandingHead = Proposed, ""
		if err := s.saveAuthorized(ctx, &p); err != nil {
			return p, err
		}
	}
	switch {
	case err != nil:
		return Package{}, err
	case !ok || p.State != Proposed:
		return p, refused("case %s has no proposed package to land", caseID)
	case !s.Live:
		return p, refused("the loop is not live (loop.live): it proposes and does not land")
	case !p.Clean():
		return p, refused("case %s's last review is not clean on its head, or was not posted: no landing", caseID)
	case p.Check == nil:
		return p, refused("case %s's package has no check", caseID)
	}
	if _, err := s.working(ctx, caseID); err != nil {
		return p, err
	}
	if head, err := s.git(ctx, p.Host, "rev-parse", "HEAD"); err != nil || strings.TrimSpace(head) != p.Head {
		return p, refused("the work tree's head is not the reviewed %s: no landing", p.Head)
	}
	if st, err := s.git(ctx, p.Host, "status", "--porcelain"); err != nil || strings.TrimSpace(st) != "" {
		return p, refused("the work tree holds changes besides the reviewed commit: no landing")
	}
	if _, err := s.Gate.Repository(ctx, p.Dir); err != nil {
		return p, err
	}
	if _, err := s.git(ctx, p.Host, "fetch", "--quiet", "origin", p.Base); err != nil {
		return p, err
	}
	p.State = Landing
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, err
	}
	basePush := false
	complete := false
	defer func() {
		if complete || basePush || p.LandingHead != "" {
			return
		}
		if _, err := s.git(context.Background(), p.Host, "reset", "--hard", p.Head); err != nil {
			return
		}
		p.State, p.LandingHead = Proposed, ""
		_ = s.saveAuthorized(ctx, &p)
	}()
	if _, err := s.git(ctx, p.Host, "rebase", "origin/"+p.Base); err != nil {
		s.git(context.Background(), p.Host, "rebase", "--abort")
		p.State, p.RefreshBase = Revising, true
		complete = true
		if err := s.saveAuthorized(ctx, &p); err != nil {
			return p, err
		}
		return p, refused("the change does not rebase on %s: %v", p.Base, err)
	}
	head, err := s.git(ctx, p.Host, "rev-parse", "HEAD")
	if err != nil {
		return p, err
	}
	head = strings.TrimSpace(head)
	tree, err := s.export(ctx, p, "land", head)
	if err != nil {
		return p, err
	}
	command, err := s.checkCommand(p)
	if err != nil {
		return p, errors.Join(err, s.unexport(p, tree))
	}
	a, err := s.Sandbox.Run(ctx, tree, command, s.Env)
	err = errors.Join(err, s.unexport(p, tree))
	if err != nil {
		return p, err
	}
	if a.Exit != 0 {
		return p, refused("after the rebase the test fails (exit %d): no landing", a.Exit)
	}
	base, err := s.git(ctx, p.Host, "rev-parse", "origin/"+p.Base)
	if err != nil {
		return p, err
	}
	commits, err := s.git(ctx, p.Host, "rev-list", "--reverse", strings.TrimSpace(base)+"..HEAD")
	if err != nil {
		return p, err
	}
	p.LandingHead, p.Landed = head, strings.Fields(commits)
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, err
	}
	if head != p.Head { // the pull request's head is what reaches the base, so GitHub marks it merged
		if err := s.publish(ctx, &p, head); err != nil {
			if !errors.Is(err, errPublicationUncertain) {
				p.LandingHead, p.Landed = "", nil // the remote was read and the push was not accepted: the reviewed head comes back
			}
			return p, err
		}
	}
	if err := s.openPR(ctx, p); err != nil {
		if errors.Is(err, errPRClosed) {
			complete = true
			return s.closedProposal(ctx, p)
		}
		return p, err
	}
	basePush = true
	if _, err := s.push(ctx, p, p.Host, "--no-verify", "origin", head+":refs/heads/"+p.Base); err != nil {
		return p, refused("the push to %s did not go as a fast-forward: %v", p.Base, err)
	}
	complete = true
	return s.finishLanding(ctx, p)
}

type checkCommand struct {
	Tree string   `json:"tree"`
	Args []string `json:"args"`
}

func (s *Ship) checkCommand(p Package) ([]string, error) {
	if p.Check.CommandInput == "" {
		return append([]string(nil), p.Check.Test...), nil
	} // legacy checks stored execution words
	if p.Check.CommandInput != "test-command.json" {
		return nil, refused("invalid check command input")
	}
	f, err := OpenFile(s.pkgDir(p.Case), p.Check.CommandInput)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := ReadFile(f, runFileMax)
	if err != nil {
		return nil, err
	}
	var input checkCommand
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, err
	}
	if input.Tree != p.Check.Tree || len(input.Args) == 0 || !testPrograms[input.Args[0]] {
		return nil, refused("check execution input does not match its checked tree")
	}
	return input.Args, nil
}

func (s *Ship) finishLanding(ctx context.Context, p Package) (Package, error) {
	p.Head, p.State = p.LandingHead, Landed
	if p.LandedAt.IsZero() {
		p.LandedAt = s.now()
	}
	// Deployment can fail or be interrupted. Keep deletion intent with the landing.
	p.Cleanup = "proposal branch cleanup pending"
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, err
	}
	if err := s.deploymentReceipt(ctx, p); err != nil {
		return p, err
	}
	what := fmt.Sprintf("%s: %s", p.PR, p.Title)
	if _, err := s.Store.DeployCase(ctx, p.Case, s.pkgDir(p.Case), p.LandedAt, what, s.Actor); err != nil {
		return p, err
	}
	return s.cleanupBranch(ctx, p)
}

func (s *Ship) cleanupBranch(ctx context.Context, p Package) (Package, error) {
	remote, err := s.remoteBranch(ctx, p)
	if err != nil {
		return p, err
	}
	if remote == "" {
		p.Cleanup = ""
		return p, s.saveAuthorized(ctx, &p)
	}
	_, cleanupErr := s.push(ctx, p, p.Host, "--no-verify", "--quiet",
		"--force-with-lease=refs/heads/"+p.Branch+":"+p.LandingHead, "origin", "--delete", p.Branch)
	if cleanupErr != nil {
		p.Cleanup = "proposal branch cleanup refused: " + cleanupErr.Error()
		if err := s.saveAuthorized(ctx, &p); err != nil {
			return p, err
		}
		return p, refused("%s", p.Cleanup)
	}
	p.Cleanup = ""
	return p, s.saveAuthorized(ctx, &p)
}

func (s *Ship) retryCleanup(ctx context.Context, caseID string) (Package, error) {
	ctx = s.gitContext(shipContext(ctx, caseID))
	unlock, err := s.lock(ctx, caseID)
	if err != nil {
		return Package{}, err
	}
	defer unlock()
	p, ok, err := s.Load(caseID)
	if err != nil || !ok || p.Cleanup == "" {
		return p, err
	}
	if !s.Live {
		return p, refused("resuming cleanup needs loop.live")
	}
	return s.cleanupBranch(ctx, p)
}

// RollBack undoes a landed change: a revert of its commits, by the user's identity, pushed to the default branch
// as a fast-forward; the case goes back to testing.
func (s *Ship) RollBack(ctx context.Context, caseID, why string) (pkg Package, stepErr error) {
	unlock, err := s.lock(ctx, caseID)
	if err != nil {
		return Package{}, err
	}
	defer unlock()
	return s.rollBackLocked(ctx, caseID, why)
}

// rollBackLocked reloads the package under the caller's case lock.
func (s *Ship) rollBackLocked(ctx context.Context, caseID, why string) (pkg Package, stepErr error) {
	ctx = s.gitContext(shipContext(ctx, caseID))
	cleanWhy, cleanErr := Redact(ctx, why)
	if cleanErr != nil {
		return Package{}, cleanErr
	}
	why = cleanWhy
	p, ok, err := s.Load(caseID)
	if err != nil {
		return Package{}, err
	}
	if !ok || (p.State != Landed && p.State != RollingBack && p.State != RolledBack) {
		return p, refused("case %s has no landed package to roll back", caseID)
	}
	if p.State == RolledBack {
		return p, s.rollbackReceipt(ctx, p, why)
	}
	if p.State == RollingBack && !s.Live {
		return p, refused("resuming rollback needs loop.live")
	}
	c, err := s.Store.CaseByID(ctx, caseID)
	if err != nil {
		return p, err
	}
	if c.Status != store.CaseDeployed && c.Status != store.CaseMeasuring {
		return p, refused("only a deployed or measuring case has a change to roll back")
	}
	if p.Host == "" || p.BaseCommit == "" {
		return p, refused("legacy package has no host clone; migrate its receipt before rollback")
	}
	if _, err := s.Gate.Repository(ctx, p.Dir); err != nil {
		return p, err
	}
	dir := filepath.Join(s.Dir, "rollback", caseID)
	if err := s.dropRollback(p, dir); err != nil {
		return p, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return p, err
	}
	if _, err := s.git(ctx, p.Host, "fetch", "--quiet", "origin", p.Base); err != nil {
		return p, err
	}
	start := "origin/" + p.Base
	if p.Undo != nil {
		_, ancestorErr := s.git(ctx, p.Host, "merge-base", "--is-ancestor", p.Undo.Head, start)
		if ancestorErr == nil {
			p.State = RolledBack
			if err := s.saveAuthorized(ctx, &p); err != nil {
				return p, err
			}
			return p, s.rollbackReceipt(ctx, p, why)
		}
		var exit *exec.ExitError
		if !errors.As(ancestorErr, &exit) || exit.ExitCode() != 1 {
			return p, ancestorErr
		}
		_, relationErr := s.git(ctx, p.Host, "merge-base", "--is-ancestor", start, p.Undo.Head)
		switch {
		case relationErr == nil:
			start = p.Undo.Head
		case !errors.As(relationErr, &exit) || exit.ExitCode() != 1:
			return p, relationErr
		default:
			// The default branch moved without accepting this revert. Keep the
			// old journal on disk until its replacement is ready.
			why, p.Undo = p.Undo.Why, nil
		}
	}
	if _, err := s.git(ctx, p.Host, "worktree", "add", "--detach", dir, start); err != nil {
		return p, err
	}
	defer func() { stepErr = errors.Join(stepErr, s.dropRollback(p, dir)) }()
	if p.Undo == nil {
		shas := append([]string{}, p.Landed...)
		if len(shas) == 0 {
			return p, refused("the landed package has no commits to revert")
		}
		slices.Reverse(shas)
		if _, err := s.git(ctx, dir, append([]string{"revert", "--no-edit"}, shas...)...); err != nil {
			return p, err
		}
		head, err := s.git(ctx, dir, "rev-parse", "HEAD")
		if err != nil {
			return p, err
		}
		commits, err := s.git(ctx, dir, "rev-list", "--reverse", "origin/"+p.Base+"..HEAD")
		if err != nil {
			return p, err
		}
		p.State, p.Undo = RollingBack, &RollbackJournal{Head: strings.TrimSpace(head), Commits: strings.Fields(commits), Why: why}
		if err := s.saveAuthorized(ctx, &p); err != nil {
			return p, err
		}
	}
	if _, err := s.push(ctx, p, dir, "--no-verify", "origin", p.Undo.Head+":refs/heads/"+p.Base); err != nil {
		return p, err
	}
	p.State = RolledBack
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, err
	}
	return p, s.rollbackReceipt(ctx, p, why)
}

// rollbackReceipt finishes only a pending case transition. A receipt already written is idempotent.
func (s *Ship) rollbackReceipt(ctx context.Context, p Package, why string) error {
	c, err := s.Store.CaseByID(ctx, p.Case)
	if err != nil {
		return err
	}
	if c.Status == store.CaseTesting {
		return nil
	}
	if p.Undo != nil {
		why = p.Undo.Why
	}
	if err := s.runner().noteOnce(ctx, store.Note{ID: noteID("rolled-back", p), Kind: store.NoteDecided,
		Title: plainClip("momentum rolled back its change for: "+c.Title, store.TitleMax),
		Detail: why + ". To keep the change after all: revert the revert on the default branch, then momentum case close " + c.ID +
			" --reason already-solved --why ..., or the loop builds another change for the problem.", Links: prLinks(p.PR)}); err != nil {
		return err
	}
	_, err = s.Store.RollBackCase(ctx, p.Case, why, s.Actor)
	return err
}

// commitTree commits the exact validated object and advances only the expected branch head.
func (s *Ship) commitTree(ctx context.Context, p Package, tree, parent, msg string) error {
	if s.Identity[0] == "" || s.Identity[1] == "" {
		return refused("no git identity: set user.name and user.email in the global git configuration")
	}
	head, err := s.git(ctx, p.Host, "commit-tree", tree, "-p", parent, "-m", msg)
	if err != nil {
		return err
	}
	head = strings.TrimSpace(head)
	got, err := s.git(ctx, p.Host, "rev-parse", head+"^{tree}")
	if err != nil || strings.TrimSpace(got) != tree {
		return refused("commit-tree did not produce the checked tree")
	}
	_, err = s.git(ctx, p.Host, "update-ref", "refs/heads/"+p.Branch, head, parent)
	return err
}

// git runs git as the ship tool does: no hooks (a tracked hooks directory is the change's own), no file-system
// monitor, no external diff, and the user's identity for every commit it writes (a commit, a rebase, a revert),
// whatever the repository's configuration or the environment say.
func (s *Ship) git(ctx context.Context, dir string, args ...string) (string, error) {
	if dir == "" {
		return "", refused("host Git requires an explicit host repository")
	}
	real := gitResolved(dir)
	if under(real, filepath.Join(s.Dir, "work")) || under(real, filepath.Join(s.Dir, "runs")) || s.Sandbox.Cache != "" && under(real, s.Sandbox.Cache) {
		return "", refused("host Git never reads a run clone, run directory or writable build cache")
	}
	return s.gitInput(ctx, dir, nil, args...)
}

func (s *Ship) gitInput(ctx context.Context, dir string, input []byte, args ...string) (string, error) {
	if err := CheckHostStorage(s.gitContext(ctx), dir); err != nil {
		return "", err
	}
	if id, ok := ctx.Value(shipCaseKey{}).(string); ok {
		if err := s.authority(ctx, id); err != nil {
			return "", err
		}
	}
	if err := CheckGitConfig(ctx, dir); err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	full := append([]string{"-C", dir}, []string{"-c", "diff.external=", "-c", "commit.gpgsign=false",
		"-c", "user.name=" + s.Identity[0], "-c", "user.email=" + s.Identity[1]}...)
	full = append(full, GitSafety()...)
	full = append(full, "-c", "credential.helper=",
		"-c", "credential.https://github.com.helper=!"+shellQuote(firstOf(s.GH, "gh"))+" auth git-credential",
		"-c", "protocol.ext.allow=never", "-c", "push.followTags=false")
	full = append(full, args...)
	cmd := exec.CommandContext(cctx, "git", full...)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Env = append(noGitEnv(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME="+s.Identity[0], "GIT_AUTHOR_EMAIL="+s.Identity[1],
		"GIT_COMMITTER_NAME="+s.Identity[0], "GIT_COMMITTER_EMAIL="+s.Identity[1])
	out, errOut := Capture{Limit: MaxStream}, Capture{Limit: MaxStream}
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := s.assignedAuthority(ctx); err != nil {
		return "", err
	}
	err := cmd.Run()
	if out.Over() || errOut.Over() {
		return "", refused("Git output exceeds the whole-stream limit")
	}
	if err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}
	return out.String(), nil
}

func (s *Ship) gh(ctx context.Context, dir string, args ...string) (string, error) {
	if err := CheckHostStorage(s.gitContext(ctx), dir); err != nil {
		return "", err
	}
	if id, ok := ctx.Value(shipCaseKey{}).(string); ok {
		if err := s.authority(ctx, id); err != nil {
			return "", err
		}
	}
	if err := CheckGitConfig(ctx, dir); err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, firstOf(s.GH, "gh"), args...)
	cmd.Dir = dir
	cmd.Env = gitChildEnv()
	out, diag := Capture{Limit: MaxStream}, Capture{Limit: MaxStream}
	cmd.Stdout, cmd.Stderr = &out, &diag
	if err := s.assignedAuthority(ctx); err != nil {
		return "", err
	}
	err := cmd.Run()
	if out.Over() || diag.Over() {
		return "", refused("GitHub CLI output exceeds the whole-stream limit")
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("gh %s: %w: %s", args[0], err, strings.TrimSpace(diag.String()))
		}
		return "", err
	}
	return out.String(), nil
}

// packageText is what a proposal adds to its body: the case, its check, its measurement and its rollback.
func (s *Ship) packageText(c store.Case, p Package) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Change package\n\n- Case: `%s` (%s), %d evidence.\n", c.ID, c.Family, c.Evidence)
	fmt.Fprintf(&b, "- Check: `%s` fails without the change (exit %d) and passes with it (%.0f s).\n", strings.Join(p.Check.Test, " "),
		p.Check.Before.Exit, p.Check.After.Seconds)
	sig, _ := json.Marshal(p.Plan.Signal)
	fmt.Fprintf(&b, "- Measurement: the signal `%s` counted events in %d of %d %s in the %s baseline before the gated window; the gate counted %d of %d in its last %s. The %s after the change is in effect is compared with the baseline (Fisher's exact test, one-sided, p below 0.05).\n",
		sig, p.Plan.Before, p.Plan.BeforeBase, p.Plan.Units, days(measure.BaselineWindows*p.Plan.Window), p.Plan.Gate, p.Plan.GateBase, days(p.Plan.Window), days(p.Plan.Window))
	if p.Plan.Installed != "" {
		fmt.Fprintf(&b, "- In effect once `%s`, where it is installed, changed after the checkout took the change.\n", filepath.Base(p.Plan.Installed))
	}
	fmt.Fprintf(&b, "- Rollback: %s\n", p.Rollback)
	b.WriteString("\nOpened by momentum's improvement loop. It lands by fast-forward only after a clean-context review finds nothing high or medium.\n")
	return b.String()
}

func (s *Ship) repositoryPrivate(ctx context.Context, p Package) (bool, error) {
	raw, err := s.gh(ctx, p.Host, "repo", "view", p.Repo, "--json", "isPrivate")
	if err != nil {
		return false, err
	}
	var visibility struct {
		Private *bool `json:"isPrivate"`
	}
	if json.Unmarshal([]byte(raw), &visibility) != nil || visibility.Private == nil {
		return false, refused("repository visibility is unknown")
	}
	return *visibility.Private, nil
}

func (s *Ship) hostPackageText(ctx context.Context, c store.Case, p Package) (string, error) {
	stat, err := s.git(ctx, p.Host, "diff", "--shortstat", p.BaseCommit, p.Head, "--")
	if err != nil {
		return "", err
	}
	return s.public(ctx, fmt.Sprintf("Case: %s\nEvidence count: %d\nBase: %s\nHead: %s\nDiff: %s\nCheck: before exit %d; after exit %d.",
		c.ID, c.Evidence, p.BaseCommit, p.Head, strings.TrimSpace(stat), p.Check.Before.Exit, p.Check.After.Exit))
}

func (s *Ship) openPR(ctx context.Context, p Package) error {
	raw, err := s.gh(ctx, p.Host, "pr", "view", p.PR, "--repo", p.Repo, "--json", "state")
	if err != nil {
		return err
	}
	var pr struct {
		State string `json:"state"`
	}
	if json.Unmarshal([]byte(raw), &pr) != nil {
		return refused("the pull request is not open")
	}
	if pr.State == "CLOSED" {
		return errors.Join(refused("the pull request is closed"), errPRClosed)
	}
	if pr.State != "OPEN" {
		return refused("the pull request is not open")
	}
	return nil
}

var errPRClosed = errors.New("pull request closed")

func (s *Ship) closedProposal(ctx context.Context, p Package) (Package, error) {
	if _, err := s.git(ctx, p.Host, "reset", "--hard", p.Head); err != nil {
		return p, err
	}
	p.ClosedProposals++
	p.State, p.PR, p.LandingHead, p.Landed, p.Reviews = Checked, "", "", nil, nil
	if err := s.saveAuthorized(ctx, &p); err != nil {
		return p, err
	}
	if err := s.Store.CaseNote(ctx, p.Case, "proposal closed",
		"The pull request was closed without merging. The checked package can be proposed again.", s.Actor); err != nil {
		return p, err
	}
	return p, refused("the pull request was closed; the checked package can be proposed again")
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func lastOf(xs []string) string {
	if len(xs) == 0 {
		return ""
	}
	return xs[len(xs)-1]
}

// wrap breaks a text's paragraphs at 72 characters, for a commit message.
func wrap(s string) string {
	var out []string
	for _, para := range strings.Split(strings.TrimSpace(s), "\n\n") {
		var line []string
		n := 0
		var lines []string
		for _, w := range strings.Fields(para) {
			if n+len(w)+len(line) > 72 && len(line) > 0 {
				lines, line, n = append(lines, strings.Join(line, " ")), nil, 0
			}
			line, n = append(line, w), n+len(w)
		}
		if len(line) > 0 {
			lines = append(lines, strings.Join(line, " "))
		}
		out = append(out, strings.Join(lines, "\n"))
	}
	return strings.Join(out, "\n\n")
}

// CheckGitConfig refuses repository settings that name programs before an unsandboxed operation.
// The directory's own configuration and includes are read without our pins, system or global settings.
func CheckGitConfig(ctx context.Context, dir string) error {
	if err := CheckHostStorage(ctx, dir); err != nil {
		return err
	}
	return checkPinnedGitConfig(ctx, dir)
}

func checkPinnedGitConfig(ctx context.Context, dir string) error {
	return CheckGitConfigWith(func(args ...string) ([]byte, error) {
		out, err := hostConfig(ctx, dir, args...)
		return []byte(out), err
	})
}

// CheckGitConfigWith inspects configuration through the caller's filesystem boundary.
// The host checks the repository storage before these reads.
func CheckGitConfigWith(config func(...string) ([]byte, error)) error {
	out, err := config("--no-includes", "--null", "--name-only", "--list")
	if err != nil {
		return fmt.Errorf("inspect Git configuration names: %w", err)
	}
	for _, name := range strings.Split(string(out), "\x00") {
		// git lists a name with its section and variable in lower case and its subsection as written, and a subsection is
		// case sensitive: a value is read by the name as git listed it
		key := strings.ToLower(name)
		if key == "core.fsmonitor" {
			value, err := config("--type=bool", "--get", name)
			if err == nil && strings.TrimSpace(string(value)) == "false" {
				continue
			}
			return refused("repository Git setting %s names a program", key)
		}
		blocked := false
		switch key {
		case "core.sshcommand", "core.askpass", "core.gitproxy", "core.alternaterefscommand", "core.worktree", "core.pager", "core.editor", "sequence.editor",
			"interactive.difffilter", "gc.recentobjectshook":
			blocked = true
		}
		for _, prefix := range []string{"credential.", "url.", "include.", "includeif.", "filter.", "gpg.", "uploadpack.", "pager.", "hook.", "trailer."} {
			blocked = blocked || strings.HasPrefix(key, prefix)
		}
		for _, pair := range [][2]string{{"merge.", ".driver"}, {"diff.", ".textconv"}, {"diff.", ".command"}, {"remote.", ".receivepack"},
			{"remote.", ".uploadpack"}, {"remote.", ".vcs"}} {
			blocked = blocked || strings.HasPrefix(key, pair[0]) && strings.HasSuffix(key, pair[1])
		}
		if strings.HasPrefix(key, "submodule.") && strings.HasSuffix(key, ".update") {
			// checkout, rebase, merge and none are the modes git submodule init writes; any other value (! and a
			// command) runs a program
			value, err := config("--get-all", name)
			for _, v := range strings.Split(strings.TrimSpace(string(value)), "\n") {
				blocked = blocked || err != nil || !slices.Contains([]string{"checkout", "rebase", "merge", "none"}, strings.ToLower(strings.TrimSpace(v)))
			}
		}
		if blocked {
			return refused("repository Git setting %s names a program", key)
		}
	}
	return nil
}

// writeTest refuses links in the base export. Root confines the write even if a component changes.
func writeTest(dir, name string, data []byte, mode os.FileMode) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	parts := strings.Split(filepath.ToSlash(name), "/")
	for i := range parts {
		part := filepath.Join(parts[:i+1]...)
		st, err := root.Lstat(part)
		if err == nil && st.Mode()&os.ModeSymlink != 0 {
			return refused("base test path contains a symlink: %s", name)
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if i < len(parts)-1 && errors.Is(err, fs.ErrNotExist) {
			if err := root.Mkdir(part, 0o700); err != nil {
				return err
			}
		}
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Chmod(mode), f.Close())
}

// push checks the actual work tree's destinations and current authority immediately before every push.
func (s *Ship) push(ctx context.Context, p Package, dir string, args ...string) (string, error) {
	t, err := s.Gate.Repository(ctx, p.Dir)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(t.Repo, p.Repo) {
		return "", refused("the push destination is not the package's repository")
	}
	actual, err := originRepository(ctx, dir)
	if err != nil {
		return "", err
	}
	source, err := originRepository(ctx, p.Dir)
	if err != nil || actual != source {
		return "", refused("the host clone's destination changed")
	}
	for _, arg := range args {
		candidate, ref, ok := strings.Cut(arg, ":")
		// A journaled rollback restores commits already published by this
		// repository. Its push still passes every repository gate above.
		if p.State != RollingBack && ok && (ref == "refs/heads/"+p.Branch || ref == "refs/heads/"+p.Base) {
			if err := s.checkPublicPatch(ctx, p, candidate); err != nil {
				return "", err
			}
		}
	}
	return s.git(ctx, dir, append([]string{"push", "--no-follow-tags"}, args...)...)
}

// shellQuote keeps a configured executable path one word in Git's credential helper.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
