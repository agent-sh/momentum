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
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/measure"
	"github.com/agent-sh/momentum/internal/store"
)

// Ship is the one way a change of the loop's goes in: a change package per case, built in a work tree of the
// loop's own on a branch of an owned repository, checked by a test that fails without the change and passes with
// it, proposed as a pull request with a rollback and a measurement plan, reviewed in a clean context, revised for
// what a review finds (three rounds at most), and landed only when the loop is live and the last review found
// nothing high or medium. Every step checks the authority again (Gate). A configuration directory is no target:
// the loop changes only repositories, through pull requests.
type Ship struct {
	Store   *store.Store
	Gate    Gate
	Sandbox Sandbox
	Dir     string // the loop's directory: packages/<case> and work/<case> under it
	Live    bool
	Actor   string
	Now     func() time.Time
	// Identity is the git author and committer every commit of the loop's takes: the user's own, from his global
	// git configuration, never a repository's override or the environment's.
	Identity [2]string
	GH       string // the GitHub CLI
	// Env is what a check's test adds to the sandbox's own environment (a toolchain's settings); nothing of the
	// user's session.
	Env []string
	// Public makes a text the loop posts publishable (loop.Public: secrets scrubbed, mentions neutral).
	Public func(ctx context.Context, text string) (string, error)
}

// Package states.
const (
	Prepared   = "prepared"
	Checked    = "checked"
	Proposed   = "proposed"
	Landing    = "landing"
	Revising   = "revising" // a review found something: back to its build, on the same branch and pull request
	Landed     = "landed"
	RolledBack = "rolled back"
)

// MaxRounds is how many review rounds a package gets: prs.md's three.
const MaxRounds = 3

// Package is a case's change: where it goes, what checks it, how it is measured and undone, and where it stands.
type Package struct {
	Case        string    `json:"case"`
	Kind        string    `json:"kind"` // repo
	Repo        string    `json:"repo,omitempty"`
	Base        string    `json:"base,omitempty"`
	Branch      string    `json:"branch,omitempty"`
	Dir         string    `json:"dir"`  // the repository's main checkout
	Work        string    `json:"work"` // the loop's work tree
	State       string    `json:"state"`
	Check       *Check    `json:"check,omitempty"`
	Plan        *Plan     `json:"plan,omitempty"`
	Rollback    string    `json:"rollback,omitempty"`
	Title       string    `json:"title,omitempty"`
	Body        string    `json:"body,omitempty"`
	Lesson      string    `json:"lesson,omitempty"`
	PR          string    `json:"pr,omitempty"`
	LandingHead string    `json:"landing_head,omitempty"`
	Head        string    `json:"head,omitempty"` // the commit proposed
	Reviews     []Review  `json:"reviews,omitempty"`
	Landed      []string  `json:"landed,omitempty"` // the commits that went in
	LandedAt    time.Time `json:"landed_at,omitempty"`
	Changed     []string  `json:"changed,omitempty"` // the paths the change touches
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
}

// Check is the evidence that a test discriminates the change: it fails on the code without the change (the
// test's own files kept) and passes with it, both run on what the change commits (its tree), not on the files
// beside it in the work tree.
type Check struct {
	Test      []string  `json:"test"`
	TestFiles []string  `json:"test_files"`
	Before    Outcome   `json:"before"`
	After     Outcome   `json:"after"`
	Tree      string    `json:"tree"` // the tree it checked: an edit after it needs a new check
	At        time.Time `json:"at"`
}

// Plan is how the change is measured: a signal over the store's events, counted over a window before the change
// and as long after it (internal/measure).
type Plan struct {
	Signal Signal        `json:"signal"`
	Window time.Duration `json:"window"`
	Before int           `json:"before"` // the signal's count over the window before the proposal
	At     time.Time     `json:"at"`     // when it was counted: the window before ends here
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

// Prepare makes the case's work tree: a branch of the repository's default branch in a worktree of the loop's
// own, once the gate allows the repository. A case has one package at a time; one rolled back can be prepared
// again, its old work tree and branch gone first.
func (s *Ship) Prepare(ctx context.Context, caseID, repoDir string) (Package, error) {
	c, err := s.working(ctx, caseID)
	if err != nil {
		return Package{}, err
	}
	old, had, err := s.Load(c.ID)
	if err != nil {
		return Package{}, err
	}
	if had && old.State != RolledBack {
		return old, refused("case %s has a package %s already (in %s)", c.ID, old.State, old.Work)
	}
	if repoDir == "" {
		return Package{}, refused("a package goes to one repository: --repo, a checkout of it")
	}
	t, err := s.Gate.Repository(ctx, repoDir)
	if err != nil {
		return Package{}, err
	}
	work := s.workDir(c.ID)
	branch := "momentum/" + c.ID
	if had { // its last tree's registration, so the branch can be checked out again
		s.git(ctx, old.Dir, "worktree", "remove", "--force", old.Work)
		s.git(ctx, old.Dir, "worktree", "prune")
	}
	if err := os.RemoveAll(work); err != nil {
		return Package{}, err
	}
	s.git(ctx, t.Dir, "worktree", "prune")
	if err := os.MkdirAll(filepath.Dir(work), 0o700); err != nil {
		return Package{}, err
	}
	if _, err := s.git(ctx, t.Dir, "fetch", "--quiet", "origin", t.Base); err != nil {
		return Package{}, err
	}
	if _, err := s.git(ctx, t.Dir, "worktree", "add", "--no-checkout", "-B", branch, work, "origin/"+t.Base); err != nil {
		return Package{}, err
	}
	if err := CheckGitConfig(ctx, work); err != nil {
		return Package{}, err
	}
	if _, err := s.git(ctx, work, "reset", "--hard", "HEAD"); err != nil {
		return Package{}, err
	}
	p := Package{Case: c.ID, Kind: "repo", Repo: t.Repo, Base: t.Base, Dir: t.Dir, Branch: branch, Work: work, State: Prepared, Created: s.now()}
	if err := s.save(&p); err != nil {
		return Package{}, err
	}
	return p, s.Store.CaseNote(ctx, c.ID, "prepared", fmt.Sprintf("%s in %s", p.Repo, p.Work), s.Actor)
}

// staged stages every change of the work tree and returns its tree, by its hash, and the paths it changes against
// the default branch: both sides of a rename (git's renames are off), each name as git keeps it (NUL-separated).
func (s *Ship) staged(ctx context.Context, p Package) (string, []string, error) {
	if err := s.ownTree(p); err != nil {
		return "", nil, err
	}
	if _, err := s.git(ctx, p.Work, "add", "-A"); err != nil {
		return "", nil, err
	}
	tree, err := s.git(ctx, p.Work, "write-tree")
	if err != nil {
		return "", nil, err
	}
	out, err := s.git(ctx, p.Work, "diff", "--cached", "--name-status", "--no-renames", "-z", "origin/"+p.Base)
	if err != nil {
		return "", nil, err
	}
	var paths []string
	f := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		paths = append(paths, f[i+1])
	}
	sort.Strings(paths)
	return strings.TrimSpace(tree), slices.Compact(paths), nil
}

// ownTree refuses a work tree whose link to its repository is no longer the one git made: a .git that points
// elsewhere would make the loop's own git calls read another repository's configuration.
func (s *Ship) ownTree(p Package) error {
	b, err := os.ReadFile(filepath.Join(p.Work, ".git"))
	if err != nil {
		return refused("%s's .git is not the link git made: %v", p.Work, err)
	}
	link := strings.TrimSpace(strings.TrimPrefix(string(b), "gitdir: "))
	common := filepath.Join(p.Dir, ".git", "worktrees")
	if !strings.HasPrefix(string(b), "gitdir: ") || !under(link, common) {
		return refused("%s's .git points to %q, not to its repository's work trees", p.Work, link)
	}
	return nil
}

// testFileRe are the names a test file has in the languages the owned repositories use.
var testFileRe = regexp.MustCompile(`(_test\.go|\.test\.[cm]?[jt]sx?|\.spec\.[cm]?[jt]sx?|(^|/)test_[^/]*\.py|_test\.py|(^|/)tests?/.*|\.bats)$`)

// Check runs the case's test twice in the sandbox, each time in a tree of the change's commit as it would be (no
// ignored file of the work tree in it): the default branch with only the test's own files of the change (it must
// fail), and the change (it must pass). The test's files are those the change touches that are named as tests, or
// the ones named. A check that does not come out so is recorded as it came out, and the package stays unchecked.
func (s *Ship) Check(ctx context.Context, caseID, test string, testFiles []string) (Package, error) {
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
	words, err := Words(test)
	if err != nil {
		return p, err
	}
	tree, paths, err := s.staged(ctx, p)
	if err != nil {
		return p, err
	}
	if len(paths) == 0 {
		return p, refused("the change is empty: edit the files in %s first", p.Work)
	}
	if err := CheckPaths(Target{Repo: p.Repo}, paths); err != nil {
		return p, err
	}
	// a clean review has read the whole change: the review's diff leaves credential files out, and the loop never
	// changes one
	for _, c := range paths {
		if CredentialPath(c) {
			return p, refused("%s is a credential file or lies where credentials are kept: the loop never changes one", c)
		}
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
	before, err := s.export(ctx, p, "before", "origin/"+p.Base)
	if err != nil {
		return p, err
	}
	defer s.unexport(p, before)
	for _, f := range testFiles {
		b, err := s.git(ctx, p.Work, "show", tree+":"+f)
		if err != nil {
			return p, err
		}
		if err := writeTest(before, f, []byte(b)); err != nil {
			return p, err
		}
	}
	after, err := s.export(ctx, p, "after", tree)
	if err != nil {
		return p, err
	}
	defer s.unexport(p, after)
	b, err := s.Sandbox.Run(ctx, before, words, s.Env)
	if err != nil {
		return p, err
	}
	a, err := s.Sandbox.Run(ctx, after, words, s.Env)
	if err != nil {
		return p, err
	}
	ck := &Check{Test: words, TestFiles: testFiles, Before: b, After: a, Tree: tree, At: s.now()}
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
	if err := s.save(&p); err != nil {
		return p, err
	}
	return p, s.Store.CaseNote(ctx, caseID, "checked", fmt.Sprintf("%s: fails without the change (exit %d), passes with it", strings.Join(words, " "), b.Exit), s.Actor)
}

// export makes a tree of a commit or a tree object, in a detached work tree of the loop's own: what a test runs on.
func (s *Ship) export(ctx context.Context, p Package, name, treeish string) (string, error) {
	dir := filepath.Join(s.Dir, "work", "."+name+"-"+p.Case)
	s.unexport(p, dir)
	if _, err := s.git(ctx, p.Dir, "worktree", "add", "--detach", dir, "origin/"+p.Base); err != nil {
		return "", err
	}
	if _, err := s.git(ctx, dir, "read-tree", "-u", "--reset", treeish); err != nil {
		s.unexport(p, dir)
		return "", err
	}
	// the test runs on the tree without its credential files: what a test prints reaches the build and the review
	listed, err := s.git(ctx, dir, append([]string{"ls-files", "-z", "--"}, credentialIncludes()...)...)
	if err != nil {
		s.unexport(p, dir)
		return "", err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		s.unexport(p, dir)
		return "", err
	}
	defer root.Close()
	for _, f := range strings.Split(listed, "\x00") {
		if f == "" {
			continue
		}
		if err := root.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.unexport(p, dir)
			return "", fmt.Errorf("leave %s out of the test's tree: %w", f, err)
		}
	}
	return dir, nil
}

func (s *Ship) unexport(p Package, dir string) {
	s.git(context.Background(), p.Dir, "worktree", "remove", "--force", dir)
	os.RemoveAll(dir)
	s.git(context.Background(), p.Dir, "worktree", "prune")
}

// Proposal is what a change is proposed with.
type Proposal struct {
	Title, Body, Rollback, Lesson string
	Signal                        Signal
	Window                        time.Duration
}

// MinSignal is the least the measurement's signal must count over the window before the change: below it, no
// window could show the change helped (measure.MinBefore).
const MinSignal = measure.MinBefore

// Propose makes the checked change a proposal: it checks the measurement plan against the case's own evidence,
// commits the change as the user, checks the gate on the committed tree, pushes the branch and opens its pull
// request, or, for a revision after a review, updates the one it has. The change must be the one the check passed.
// A proposal that failed after its commit is taken up again where it stopped.
func (s *Ship) Propose(ctx context.Context, caseID string, pr Proposal) (Package, error) {
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
	tree, paths, err := s.staged(ctx, p)
	if err != nil {
		return p, err
	}
	if tree != p.Check.Tree {
		return p, refused("the change differs from the one the check passed: check it again")
	}
	if err := CheckPaths(Target{Repo: p.Repo}, paths); err != nil {
		return p, err
	}
	switch {
	case strings.TrimSpace(pr.Title) == "" || strings.TrimSpace(pr.Body) == "":
		return p, refused("a proposal needs a title and a body (what changes, why, the evidence, the prior art)")
	case strings.TrimSpace(pr.Rollback) == "":
		return p, refused("a proposal needs its rollback: how the change is undone")
	}
	before, err := s.validPlan(ctx, c, pr.Signal, pr.Window)
	if err != nil {
		return p, err
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
	p.Plan, p.Rollback, p.Title, p.Body, p.Lesson, p.Changed = &Plan{Signal: pr.Signal, Window: pr.Window, Before: before, At: s.now()}, pr.Rollback,
		title, body, pr.Lesson, paths
	// the commit first, so the work tree is clean when the scope check looks at every work tree of the repository
	if head, err := s.git(ctx, p.Work, "rev-parse", "HEAD^{tree}"); err != nil || strings.TrimSpace(head) != tree {
		if err := s.commit(ctx, p.Work, fmt.Sprintf("%s\n\n%s\n\nCase %s.", title, wrap(body), c.ID)); err != nil {
			return p, err
		}
	}
	head, err := s.git(ctx, p.Work, "rev-parse", "HEAD")
	if err != nil {
		return p, err
	}
	p.Head = strings.TrimSpace(head)
	if n := len(p.Reviews); n > 0 && p.Reviews[n-1].Head == p.Head {
		return p, refused("the revision changes nothing of the commit the review read: change what its findings name")
	}
	if _, err := s.Gate.Repository(ctx, p.Dir); err != nil {
		return p, err
	}
	if _, err := s.push(ctx, p, p.Work, "--no-verify", "--force-with-lease", "origin", "HEAD:refs/heads/"+p.Branch); err != nil {
		return p, err
	}
	text, err := s.public(ctx, s.packageText(c, p))
	if err != nil {
		return p, err
	}
	if p.PR == "" {
		url, err := s.gh(ctx, p.Work, "pr", "create", "--repo", p.Repo, "--base", p.Base, "--head", p.Branch, "--title", title, "--body", body+"\n\n"+text)
		if err != nil {
			s.save(&p) // its commit and head: a retry takes it from there
			return p, err
		}
		p.PR = strings.TrimSpace(lastOf(strings.Split(strings.TrimSpace(url), "\n")))
	} else if _, err := s.gh(ctx, p.Work, "pr", "edit", p.PR, "--repo", p.Repo, "--title", title, "--body", body+"\n\n"+text); err != nil {
		s.save(&p)
		return p, err
	}
	p.State = Proposed
	if err := s.save(&p); err != nil {
		return p, err
	}
	return p, s.Store.CaseNote(ctx, caseID, "proposed", fmt.Sprintf("%s %s; measured by %d events in the %s before", p.PR, title, before, pr.Window), s.Actor)
}

// validPlan checks a measurement plan: a signal that names a source, kinds and at least one text, a window of at
// least a day, a count over the window before of at least MinSignal, and a signal that counts the case's own
// problem: the case holds events, and at least half of those in that window are among what it counts. It returns
// the count before.
func (s *Ship) validPlan(ctx context.Context, c store.Case, sig Signal, window time.Duration) (int, error) {
	switch {
	case sig.Source == "" || len(sig.Kinds) == 0 || len(sig.Match) == 0:
		return 0, refused("a measurement signal names a source, kinds and at least one text its events hold")
	case window < 24*time.Hour:
		return 0, refused("a measurement window is at least a day")
	}
	for _, m := range sig.Match {
		if len(strings.TrimSpace(m)) < 4 {
			return 0, refused("the signal's text %q is too short to name a problem", m)
		}
	}
	now := s.now()
	got, err := Count(ctx, s.Store, sig, now.Add(-window), now)
	if err != nil {
		return 0, err
	}
	if len(got) < MinSignal {
		return 0, refused("the signal counts %d events in the %s before, fewer than %d: no window could show an effect", len(got), window, MinSignal)
	}
	evs, err := s.Store.CaseEvidence(ctx, c.ID, 0)
	if err != nil {
		return 0, err
	}
	counted := map[string]bool{}
	for _, e := range got {
		counted["ev:"+e.ID()] = true
	}
	events, in, hit := 0, 0, 0
	for _, e := range evs {
		if !strings.HasPrefix(e.Ref, "ev:") {
			continue
		}
		events++
		if e.At.Before(now.Add(-window)) {
			continue
		}
		in++
		if counted[e.Ref] {
			hit++
		}
	}
	switch {
	case events == 0:
		return 0, refused("the case holds no event of the store: its measurement has nothing of its own problem to count")
	case in == 0:
		return 0, refused("the case has no evidence in the window: measure over a window that holds it")
	case hit*2 < in:
		return 0, refused("the signal counts %d of the case's %d events in the window: it does not measure the case's problem", hit, in)
	}
	return len(got), nil
}

// editsSignal refuses a change that edits the texts its own measurement counts: a change of words that only stops
// the signal from matching would measure as a fix. It reads the lines the change removes, hunk by hunk.
func (s *Ship) editsSignal(ctx context.Context, p Package, sig Signal) error {
	diff, err := s.git(ctx, p.Work, "diff", "--cached", "--no-renames", "--no-ext-diff", "--no-textconv", "--text", "-U0", "origin/"+p.Base)
	if err != nil {
		return err
	}
	var removed []string
	inHunk := false
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			inHunk = false
		case strings.HasPrefix(l, "@@"):
			inHunk = true
		case inHunk && strings.HasPrefix(l, "-"):
			removed = append(removed, strings.ToLower(l[1:]))
		}
	}
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

// Count is the events a signal counts over a span.
func Count(ctx context.Context, st *store.Store, sig Signal, since, until time.Time) ([]store.Event, error) {
	evs, err := st.Events(ctx, store.Query{Since: since, Until: until, Source: sig.Source, Kinds: sig.Kinds, Harness: sig.Harness, Limit: 1_000_000})
	if err != nil {
		return nil, err
	}
	var out []store.Event
	for _, e := range evs {
		if sig.Tool != "" && e.Tool != sig.Tool {
			continue
		}
		low := strings.ToLower(e.Summary)
		all := true
		for _, m := range sig.Match {
			all = all && strings.Contains(low, strings.ToLower(m))
		}
		if all {
			out = append(out, e)
		}
	}
	return out, nil
}

// Reviewed records a round of the clean-context review on the proposed head: its findings, counted by severity
// from the findings themselves, and where it was posted. A review that finds something high or medium sends the
// package back to its build (revising), until its rounds are used.
func (s *Ship) Reviewed(ctx context.Context, caseID string, r Review) (Package, error) {
	p, ok, err := s.Load(caseID)
	if err != nil {
		return Package{}, err
	}
	if !ok || p.State != Proposed {
		return p, refused("case %s has no proposed package to review", caseID)
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
	r.Round, r.At, r.Head = len(p.Reviews)+1, s.now(), p.Head
	p.Reviews = append(p.Reviews, r)
	if r.Comment != "" && (r.High > 0 || r.Medium > 0) && len(p.Reviews) < MaxRounds {
		p.State = Revising
	}
	if err := s.save(&p); err != nil {
		return p, err
	}
	return p, s.Store.CaseNote(ctx, caseID, "reviewed", fmt.Sprintf("round %d: %d high, %d medium, %d low %s", r.Round, r.High, r.Medium,
		r.Low, r.Comment), s.Actor)
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
	p, ok, err := s.Load(caseID)
	if err == nil && ok && p.State == Landing {
		if !s.Live {
			return p, refused("the loop is not live")
		}
		if err := s.ownTree(p); err != nil {
			return p, err
		}
		// Restore only this package's interrupted operation before the clean-tree scope check.
		_, _ = s.git(ctx, p.Work, "rebase", "--abort")
		if _, err := s.git(ctx, p.Work, "reset", "--hard", p.Head); err != nil {
			return p, err
		}
		if _, err := s.Gate.Repository(ctx, p.Dir); err != nil {
			return p, err
		}
		if _, err := s.git(ctx, p.Work, "fetch", "--quiet", "origin", p.Base); err != nil {
			return p, err
		}
		if p.LandingHead != "" {
			if _, err := s.git(ctx, p.Work, "merge-base", "--is-ancestor", p.LandingHead, "origin/"+p.Base); err == nil {
				p.Head, p.State, p.LandedAt = p.LandingHead, Landed, s.now()
				if err := s.save(&p); err != nil {
					return p, err
				}
				_, err := s.Store.DeployCase(ctx, p.Case, s.pkgDir(p.Case), p.LandedAt, p.PR+": "+p.Title, s.Actor)
				return p, err
			}
		}
		if _, err := s.git(ctx, p.Work, "reset", "--hard", p.Head); err != nil {
			return p, err
		}
		p.State, p.LandingHead = Proposed, ""
		if err := s.save(&p); err != nil {
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
	c, err := s.working(ctx, caseID)
	if err != nil {
		return p, err
	}
	if err := s.ownTree(p); err != nil {
		return p, err
	}
	if head, err := s.git(ctx, p.Work, "rev-parse", "HEAD"); err != nil || strings.TrimSpace(head) != p.Head {
		return p, refused("the work tree's head is not the reviewed %s: no landing", p.Head)
	}
	if st, err := s.git(ctx, p.Work, "status", "--porcelain"); err != nil || strings.TrimSpace(st) != "" {
		return p, refused("the work tree holds changes besides the reviewed commit: no landing")
	}
	if _, err := s.Gate.Repository(ctx, p.Dir); err != nil {
		return p, err
	}
	if _, err := s.git(ctx, p.Work, "fetch", "--quiet", "origin", p.Base); err != nil {
		return p, err
	}
	p.State = Landing
	if err := s.save(&p); err != nil {
		return p, err
	}
	basePush := false
	complete := false
	defer func() {
		if complete || basePush {
			return
		}
		if _, err := s.git(context.Background(), p.Work, "reset", "--hard", p.Head); err != nil {
			return
		}
		p.State, p.LandingHead = Proposed, ""
		_ = s.save(&p)
	}()
	if _, err := s.git(ctx, p.Work, "rebase", "origin/"+p.Base); err != nil {
		s.git(context.Background(), p.Work, "rebase", "--abort")
		return p, refused("the change does not rebase on %s: %v", p.Base, err)
	}
	head, err := s.git(ctx, p.Work, "rev-parse", "HEAD")
	if err != nil {
		return p, err
	}
	head = strings.TrimSpace(head)
	tree, err := s.export(ctx, p, "land", head)
	if err != nil {
		return p, err
	}
	a, err := s.Sandbox.Run(ctx, tree, p.Check.Test, s.Env)
	s.unexport(p, tree)
	if err != nil {
		return p, err
	}
	if a.Exit != 0 {
		return p, refused("after the rebase the test fails (exit %d): no landing", a.Exit)
	}
	base, err := s.git(ctx, p.Work, "rev-parse", "origin/"+p.Base)
	if err != nil {
		return p, err
	}
	commits, err := s.git(ctx, p.Work, "rev-list", "--reverse", strings.TrimSpace(base)+"..HEAD")
	if err != nil {
		return p, err
	}
	p.LandingHead, p.Landed = head, strings.Fields(commits)
	if err := s.save(&p); err != nil {
		return p, err
	}
	if head != p.Head { // the pull request's head is what reaches the base, so GitHub marks it merged
		if _, err := s.push(ctx, p, p.Work, "--no-verify", "--force-with-lease", "origin", "HEAD:refs/heads/"+p.Branch); err != nil {
			return p, err
		}
	}
	basePush = true
	if _, err := s.push(ctx, p, p.Work, "--no-verify", "origin", "HEAD:refs/heads/"+p.Base); err != nil {
		return p, refused("the push to %s did not go as a fast-forward: %v", p.Base, err)
	}
	s.push(ctx, p, p.Work, "--no-verify", "--quiet", "origin", "--delete", p.Branch)
	p.Landed, p.Head = strings.Fields(commits), head
	p.State, p.LandedAt = Landed, s.now()
	complete = true
	if err := s.save(&p); err != nil {
		return p, err
	}
	what := fmt.Sprintf("%s: %s", p.PR, p.Title)
	if _, err := s.Store.DeployCase(ctx, c.ID, s.pkgDir(caseID), p.LandedAt, what, s.Actor); err != nil {
		return p, err
	}
	return p, nil
}

// RollBack undoes a landed change: a revert of its commits, by the user's identity, pushed to the default branch
// as a fast-forward; the case goes back to testing.
func (s *Ship) RollBack(ctx context.Context, caseID, why string) (Package, error) {
	p, ok, err := s.Load(caseID)
	if err != nil {
		return Package{}, err
	}
	if !ok || p.State != Landed {
		return p, refused("case %s has no landed package to roll back", caseID)
	}
	if _, err := s.Gate.Repository(ctx, p.Dir); err != nil {
		return p, err
	}
	dir := filepath.Join(s.Dir, "work", ".revert-"+caseID)
	s.unexport(p, dir)
	if _, err := s.git(ctx, p.Dir, "fetch", "--quiet", "origin", p.Base); err != nil {
		return p, err
	}
	if _, err := s.git(ctx, p.Dir, "worktree", "add", "--detach", dir, "origin/"+p.Base); err != nil {
		return p, err
	}
	defer s.unexport(p, dir)
	shas := append([]string{}, p.Landed...)
	slices.Reverse(shas)
	if _, err := s.git(ctx, dir, append([]string{"revert", "--no-edit"}, shas...)...); err != nil {
		return p, err
	}
	if _, err := s.push(ctx, p, dir, "--no-verify", "origin", "HEAD:refs/heads/"+p.Base); err != nil {
		return p, err
	}
	p.State = RolledBack
	if err := s.save(&p); err != nil {
		return p, err
	}
	_, err = s.Store.RollBackCase(ctx, caseID, why, s.Actor)
	return p, err
}

// commit commits the work tree's staged change as the user.
func (s *Ship) commit(ctx context.Context, dir, msg string) error {
	if s.Identity[0] == "" || s.Identity[1] == "" {
		return refused("no git identity: set user.name and user.email in the global git configuration")
	}
	if _, err := s.git(ctx, dir, "add", "-A"); err != nil {
		return err
	}
	_, err := s.git(ctx, dir, "commit", "--quiet", "--no-verify", "-m", msg)
	return err
}

// git runs git as the ship tool does: no hooks (a tracked hooks directory is the change's own), no file-system
// monitor, no external diff, and the user's identity for every commit it writes (a commit, a rebase, a revert),
// whatever the repository's configuration or the environment say.
func (s *Ship) git(ctx context.Context, dir string, args ...string) (string, error) {
	if err := CheckGitConfig(ctx, dir); err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	full := append([]string{"-C", dir, "-c", "diff.external=", "-c", "commit.gpgsign=false",
		"-c", "user.name=" + s.Identity[0], "-c", "user.email=" + s.Identity[1]}, GitSafety()...)
	full = append(full, "-c", "credential.helper=",
		"-c", "credential.https://github.com.helper=!"+shellQuote(firstOf(s.GH, "gh"))+" auth git-credential",
		"-c", "protocol.ext.allow=never")
	full = append(full, args...)
	cmd := exec.CommandContext(cctx, "git", full...)
	cmd.Env = append(noGitEnv(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME="+s.Identity[0], "GIT_AUTHOR_EMAIL="+s.Identity[1],
		"GIT_COMMITTER_NAME="+s.Identity[0], "GIT_COMMITTER_EMAIL="+s.Identity[1])
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}
	return string(out), nil
}

func (s *Ship) gh(ctx context.Context, dir string, args ...string) (string, error) {
	if err := CheckGitConfig(ctx, dir); err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, firstOf(s.GH, "gh"), args...)
	cmd.Dir = dir
	cmd.Env = append(noGitEnv(), "GIT_OPTIONAL_LOCKS=0", "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null", "GIT_CONFIG_KEY_1=core.fsmonitor", "GIT_CONFIG_VALUE_1=false")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("gh %s: %w: %s", args[0], err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// packageText is what a proposal adds to its body: the case, its check, its measurement and its rollback.
func (s *Ship) packageText(c store.Case, p Package) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Change package\n\n- Case: `%s` (%s), %d evidence. Mechanism: %s\n", c.ID, c.Family, c.Evidence, c.Mechanism)
	fmt.Fprintf(&b, "- Check: `%s` fails without the change (exit %d) and passes with it (%.0f s).\n", strings.Join(p.Check.Test, " "),
		p.Check.Before.Exit, p.Check.After.Seconds)
	sig, _ := json.Marshal(p.Plan.Signal)
	fmt.Fprintf(&b, "- Measurement: the signal `%s` counted %d events in the %s before; the same window after the change is compared with it.\n",
		sig, p.Plan.Before, p.Plan.Window)
	fmt.Fprintf(&b, "- Rollback: %s\n", p.Rollback)
	b.WriteString("\nOpened by momentum's improvement loop. It lands by fast-forward only after a clean-context review finds nothing high or medium.\n")
	return b.String()
}

func writeFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
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

func firstOf(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
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
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	config := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(cctx, "git", append([]string{"-C", dir, "config"}, args...)...)
		cmd.Env = append(noGitEnv(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0")
		return cmd.Output()
	}
	out, err := config("--null", "--name-only", "--list")
	if err != nil {
		return fmt.Errorf("inspect Git configuration names: %w", err)
	}
	for _, name := range strings.Split(string(out), "\x00") {
		key := strings.ToLower(name)
		if key == "core.fsmonitor" {
			value, err := config("--type=bool", "--get", key)
			if err == nil && strings.TrimSpace(string(value)) == "false" {
				continue
			}
			return refused("repository Git setting %s names a program", key)
		}
		blocked := false
		switch key {
		case "core.sshcommand", "core.askpass", "core.gitproxy", "core.alternaterefscommand", "core.pager", "core.editor", "sequence.editor",
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
			value, err := config("--get-all", key)
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
func writeTest(dir, name string, data []byte) error {
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
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}

// push checks the actual work tree's destinations and current authority immediately before every push.
func (s *Ship) push(ctx context.Context, p Package, dir string, args ...string) (string, error) {
	t, err := s.Gate.Repository(ctx, dir)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(t.Repo, p.Repo) {
		return "", refused("the push destination is not the package's repository")
	}
	return s.git(ctx, dir, append([]string{"push"}, args...)...)
}

// shellQuote keeps a configured executable path one word in Git's credential helper.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
