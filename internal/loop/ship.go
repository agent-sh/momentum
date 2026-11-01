package loop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/agent-sh/momentum/internal/store"
)

// Ship is the one way a change of the loop's goes in: a change package per case, built in a work tree of the
// loop's own, checked by a test that fails without the change and passes with it, proposed with a rollback and a
// measurement plan, reviewed in a clean context, and landed only when the loop is live and the review found
// nothing high or medium. Every step checks the authority again (Gate).
type Ship struct {
	Store   *store.Store
	Gate    Gate
	Sandbox Sandbox
	Dir     string // the loop's directory: packages/<case> and work/<case> under it
	Live    bool
	Actor   string
	Now     func() time.Time
	// Identity is the git author and committer every commit of the loop's takes: the user's own, from his global
	// git configuration, never a repository's override.
	Identity [2]string
	GH       string // the GitHub CLI
	Window   time.Duration
	// Env is the environment a check's test runs with.
	Env []string
}

// Package states.
const (
	Prepared   = "prepared"
	Checked    = "checked"
	Proposed   = "proposed"
	Landed     = "landed"
	RolledBack = "rolled back"
)

// MaxRounds is how many review rounds a package gets: prs.md's three.
const MaxRounds = 3

// Package is a case's change: where it goes, what checks it, how it is measured and undone, and where it stands.
type Package struct {
	Case     string    `json:"case"`
	Kind     string    `json:"kind"` // repo or config
	Repo     string    `json:"repo,omitempty"`
	Base     string    `json:"base,omitempty"`
	Branch   string    `json:"branch,omitempty"`
	Dir      string    `json:"dir"`  // the repository's main checkout, or the configuration directory
	Work     string    `json:"work"` // the loop's work tree, or its copy of the configuration
	State    string    `json:"state"`
	Check    *Check    `json:"check,omitempty"`
	Plan     *Plan     `json:"plan,omitempty"`
	Rollback string    `json:"rollback,omitempty"`
	Title    string    `json:"title,omitempty"`
	Body     string    `json:"body,omitempty"`
	Lesson   string    `json:"lesson,omitempty"`
	PR       string    `json:"pr,omitempty"`
	Head     string    `json:"head,omitempty"` // the commit proposed
	Reviews  []Review  `json:"reviews,omitempty"`
	Landed   []string  `json:"landed,omitempty"` // the commits that went in
	LandedAt time.Time `json:"landed_at,omitempty"`
	Changed  []string  `json:"changed,omitempty"` // the paths the change touches
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
}

// Check is the evidence that a test discriminates the change: it fails on the code without the change (the
// test's own files kept) and passes with it.
type Check struct {
	Test      []string  `json:"test"`
	TestFiles []string  `json:"test_files"`
	Before    Outcome   `json:"before"`
	After     Outcome   `json:"after"`
	Tree      string    `json:"tree"` // the change it checked, by its hash: an edit after it needs a new check
	At        time.Time `json:"at"`
}

// Plan is how the change is measured: a signal over the store's events, counted over a window before the change
// and as long after it (internal/measure).
type Plan struct {
	Signal Signal        `json:"signal"`
	Window time.Duration `json:"window"`
	Before int           `json:"before"` // the signal's count over the window before the proposal
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

// Review is one round of the clean-context review.
type Review struct {
	Round   int       `json:"round"`
	High    int       `json:"high"`
	Medium  int       `json:"medium"`
	Low     int       `json:"low"`
	Head    string    `json:"head"`
	Comment string    `json:"comment,omitempty"` // where it was posted
	Text    string    `json:"text"`
	At      time.Time `json:"at"`
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

// Prepare makes the case's work tree: a branch of the repository's default branch in a worktree of the loop's
// own, or a copy of the configuration directory, once the gate allows the target. A case has one package at a
// time; one rolled back can be prepared again.
func (s *Ship) Prepare(ctx context.Context, caseID, repoDir, configDir string) (Package, error) {
	c, err := s.working(ctx, caseID)
	if err != nil {
		return Package{}, err
	}
	if p, ok, err := s.Load(c.ID); err != nil {
		return Package{}, err
	} else if ok && p.State != RolledBack {
		return p, refused("case %s has a package %s already (%s, in %s)", c.ID, p.State, p.Kind, p.Work)
	}
	work := s.workDir(c.ID)
	if err := os.RemoveAll(work); err != nil {
		return Package{}, err
	}
	if err := os.MkdirAll(filepath.Dir(work), 0o700); err != nil {
		return Package{}, err
	}
	p := Package{Case: c.ID, Work: work, State: Prepared, Created: s.now()}
	switch {
	case repoDir != "" && configDir == "":
		t, err := s.Gate.Repository(ctx, repoDir)
		if err != nil {
			return Package{}, err
		}
		if _, err := gitOutput(ctx, t.Dir, "fetch", "--quiet", "origin", t.Base); err != nil {
			return Package{}, err
		}
		p.Kind, p.Repo, p.Base, p.Dir, p.Branch = "repo", t.Repo, t.Base, t.Dir, "momentum/"+c.ID
		if _, err := gitOutput(ctx, t.Dir, "worktree", "add", "-B", p.Branch, work, "origin/"+t.Base); err != nil {
			return Package{}, err
		}
	case configDir != "" && repoDir == "":
		t, err := s.Gate.Config(configDir)
		if err != nil {
			return Package{}, err
		}
		p.Kind, p.Dir = "config", t.Dir
		if err := copyTree(t.Dir, work); err != nil {
			return Package{}, err
		}
	default:
		return Package{}, refused("a package goes to one repository (--repo) or one configuration directory (--config)")
	}
	if err := s.save(&p); err != nil {
		return Package{}, err
	}
	return p, s.Store.CaseNote(ctx, c.ID, "prepared", fmt.Sprintf("%s %s in %s", p.Kind, firstOf(p.Repo, p.Dir), p.Work), s.Actor)
}

// changed lists the paths the change touches, relative to its tree, and the hash of the change.
func (s *Ship) changed(ctx context.Context, p Package) ([]string, string, error) {
	h := sha256.New()
	var paths []string
	switch p.Kind {
	case "repo":
		if _, err := gitOutput(ctx, p.Work, "add", "-A"); err != nil {
			return nil, "", err
		}
		names, err := gitOutput(ctx, p.Work, "diff", "--cached", "--name-only", "origin/"+p.Base)
		if err != nil {
			return nil, "", err
		}
		diff, err := gitOutput(ctx, p.Work, "diff", "--cached", "--binary", "origin/"+p.Base)
		if err != nil {
			return nil, "", err
		}
		h.Write([]byte(diff))
		for _, n := range strings.Split(strings.TrimSpace(names), "\n") {
			if n != "" {
				paths = append(paths, n)
			}
		}
	case "config":
		var err error
		if paths, err = treeDiff(p.Dir, p.Work); err != nil {
			return nil, "", err
		}
		for _, rel := range paths {
			b, _ := os.ReadFile(filepath.Join(p.Work, rel))
			h.Write([]byte(rel + "\x00"))
			h.Write(b)
		}
	}
	sort.Strings(paths)
	return paths, hex.EncodeToString(h.Sum(nil)), nil
}

// testFileRe are the names a test file has in the languages the owned repositories use.
var testFileRe = regexp.MustCompile(`(_test\.go|\.test\.[cm]?[jt]sx?|\.spec\.[cm]?[jt]sx?|(^|/)test_[^/]*\.py|_test\.py|(^|/)tests?/.*|\.bats)$`)

// Check runs the case's test twice in the sandbox: on the base with only the test's own files of the change (it
// must fail), and on the change (it must pass). The test's files are those the change touches that are named as
// tests, or the ones named. A check that does not come out so is recorded as it came out, and the package stays
// unchecked.
func (s *Ship) Check(ctx context.Context, caseID, test string, testFiles []string) (Package, error) {
	if _, err := s.working(ctx, caseID); err != nil {
		return Package{}, err
	}
	p, ok, err := s.Load(caseID)
	if err != nil {
		return Package{}, err
	}
	if !ok || (p.State != Prepared && p.State != Checked) {
		return p, refused("case %s has no package to check (prepare it first; a proposed one is reviewed, not checked again)", caseID)
	}
	words, err := Words(test)
	if err != nil {
		return p, err
	}
	paths, tree, err := s.changed(ctx, p)
	if err != nil {
		return p, err
	}
	if len(paths) == 0 {
		return p, refused("the change is empty: edit the files in %s first", p.Work)
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
	if err := CheckPaths(Target{Repo: p.Repo}, paths); err != nil {
		return p, err
	}
	before := filepath.Join(s.Dir, "work", ".before-"+caseID)
	os.RemoveAll(before)
	defer func() {
		if p.Kind == "repo" {
			gitOutput(context.Background(), p.Dir, "worktree", "remove", "--force", before)
		}
		os.RemoveAll(before)
	}()
	switch p.Kind {
	case "repo":
		if _, err := gitOutput(ctx, p.Dir, "worktree", "add", "--detach", before, "origin/"+p.Base); err != nil {
			return p, err
		}
	case "config":
		if err := copyTree(p.Dir, before); err != nil {
			return p, err
		}
	}
	for _, f := range testFiles {
		if err := copyFile(filepath.Join(p.Work, f), filepath.Join(before, f)); err != nil {
			return p, err
		}
	}
	b, err := s.Sandbox.Run(ctx, before, words, s.Env)
	if err != nil {
		return p, err
	}
	a, err := s.Sandbox.Run(ctx, p.Work, words, s.Env)
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

// Proposal is what a change is proposed with.
type Proposal struct {
	Title, Body, Rollback, Lesson string
	Signal                        Signal
	Window                        time.Duration
}

// MinSignal is the least the measurement's signal must count over the window before the change: below it, no
// window could show the change helped.
const MinSignal = 3

// Propose makes the checked change a proposal: it checks the measurement plan against the case's own evidence,
// commits the change as the user, pushes its branch and opens a pull request (a repository), or keeps the diff (a
// configuration). The change must be the one the check passed.
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
	paths, tree, err := s.changed(ctx, p)
	if err != nil {
		return p, err
	}
	if tree != p.Check.Tree {
		return p, refused("the change differs from the one the check passed: check it again")
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
	p.Plan, p.Rollback, p.Title, p.Body, p.Lesson, p.Changed = &Plan{Signal: pr.Signal, Window: pr.Window, Before: before}, pr.Rollback,
		pr.Title, pr.Body, pr.Lesson, paths
	switch p.Kind {
	case "repo":
		if _, err := s.Gate.Repository(ctx, p.Dir); err != nil {
			return p, err
		}
		if err := s.commit(ctx, p.Work, fmt.Sprintf("%s\n\n%s\n\nCase %s.", pr.Title, wrap(pr.Body), c.ID)); err != nil {
			return p, err
		}
		head, err := gitOutput(ctx, p.Work, "rev-parse", "HEAD")
		if err != nil {
			return p, err
		}
		p.Head = strings.TrimSpace(head)
		if _, err := gitOutput(ctx, p.Work, "push", "--force-with-lease", "origin", "HEAD:refs/heads/"+p.Branch); err != nil {
			return p, err
		}
		body := pr.Body + "\n\n" + s.packageText(c, p)
		url, err := s.gh(ctx, p.Work, "pr", "create", "--repo", p.Repo, "--base", p.Base, "--head", p.Branch, "--title", pr.Title, "--body", body)
		if err != nil {
			return p, err
		}
		p.PR = strings.TrimSpace(lastOf(strings.Split(strings.TrimSpace(url), "\n")))
	case "config":
		if err := s.writeDiff(p); err != nil {
			return p, err
		}
	}
	p.State = Proposed
	if err := s.save(&p); err != nil {
		return p, err
	}
	return p, s.Store.CaseNote(ctx, caseID, "proposed", fmt.Sprintf("%s %s; measured by %d events in the %s before", firstOf(p.PR, p.Dir),
		pr.Title, before, pr.Window), s.Actor)
}

// validPlan checks a measurement plan: a signal that names a source, kinds and at least one text, a window of at
// least a day, a count over the window before of at least MinSignal, and a signal that counts the case's own
// problem: at least half of the case's evidence events in that window are among what it counts. It returns the
// count before.
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
	in, hit := 0, 0
	for _, e := range evs {
		if !strings.HasPrefix(e.Ref, "ev:") || e.At.Before(now.Add(-window)) {
			continue
		}
		in++
		if counted[e.Ref] {
			hit++
		}
	}
	if in > 0 && hit*2 < in {
		return 0, refused("the signal counts %d of the case's %d events in the window: it does not measure the case's problem", hit, in)
	}
	if in == 0 && len(evs) > 0 {
		return 0, refused("the case has no evidence in the window: measure over a window that holds it")
	}
	return len(got), nil
}

// editsSignal refuses a change that edits the texts its own measurement counts: a change of words that only stops
// the signal from matching would measure as a fix.
func (s *Ship) editsSignal(ctx context.Context, p Package, sig Signal) error {
	var removed []string
	switch p.Kind {
	case "repo":
		diff, err := gitOutput(ctx, p.Work, "diff", "--cached", "-U0", "origin/"+p.Base)
		if err != nil {
			return err
		}
		for _, l := range strings.Split(diff, "\n") {
			if strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "---") {
				removed = append(removed, strings.ToLower(l))
			}
		}
	case "config":
		paths, err := treeDiff(p.Dir, p.Work)
		if err != nil {
			return err
		}
		for _, rel := range paths {
			old, _ := os.ReadFile(filepath.Join(p.Dir, rel))
			now, _ := os.ReadFile(filepath.Join(p.Work, rel))
			kept := map[string]bool{}
			for _, l := range strings.Split(string(now), "\n") {
				kept[l] = true
			}
			for _, l := range strings.Split(string(old), "\n") {
				if !kept[l] {
					removed = append(removed, strings.ToLower(l))
				}
			}
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

// Reviewed records a round of the clean-context review: its findings by severity, and where it was posted.
func (s *Ship) Reviewed(ctx context.Context, caseID string, r Review) (Package, error) {
	p, ok, err := s.Load(caseID)
	if err != nil {
		return Package{}, err
	}
	if !ok || p.State != Proposed {
		return p, refused("case %s has no proposed package to review", caseID)
	}
	r.Round, r.At, r.Head = len(p.Reviews)+1, s.now(), p.Head
	p.Reviews = append(p.Reviews, r)
	if err := s.save(&p); err != nil {
		return p, err
	}
	return p, s.Store.CaseNote(ctx, caseID, "reviewed", fmt.Sprintf("round %d: %d high, %d medium, %d low %s", r.Round, r.High, r.Medium,
		r.Low, r.Comment), s.Actor)
}

// Clean reports whether a package's last review found nothing high or medium on the commit it proposes now.
func (p Package) Clean() bool {
	if len(p.Reviews) == 0 {
		return false
	}
	r := p.Reviews[len(p.Reviews)-1]
	return r.High == 0 && r.Medium == 0 && r.Head == p.Head
}

// Land puts a proposed change in: only when the loop is live, its last review is clean, and the gate allows the
// target again. A repository's branch is rebased on its default branch, checked again, and pushed to it as a
// fast-forward by the user's own identity (prs.md); a configuration's files are replaced, its originals kept for
// the rollback, and its test run in place, which undoes the change when it fails. The case is then deployed.
func (s *Ship) Land(ctx context.Context, caseID string) (Package, error) {
	p, ok, err := s.Load(caseID)
	switch {
	case err != nil:
		return Package{}, err
	case !ok || p.State != Proposed:
		return p, refused("case %s has no proposed package to land", caseID)
	case !s.Live:
		return p, refused("the loop is not live (loop.live): it proposes and does not land")
	case !p.Clean():
		return p, refused("case %s's last review is not clean on its head: no landing", caseID)
	case p.Check == nil:
		return p, refused("case %s's package has no check", caseID)
	}
	c, err := s.working(ctx, caseID)
	if err != nil {
		return p, err
	}
	switch p.Kind {
	case "repo":
		if _, err := s.Gate.Repository(ctx, p.Dir); err != nil {
			return p, err
		}
		if _, err := gitOutput(ctx, p.Work, "fetch", "--quiet", "origin", p.Base); err != nil {
			return p, err
		}
		// the rebase writes its commits anew: as the user, never a repository's override of his identity
		if _, err := gitOutput(ctx, p.Work, "-c", "user.name="+s.Identity[0], "-c", "user.email="+s.Identity[1], "-c", "commit.gpgsign=false",
			"rebase", "origin/"+p.Base); err != nil {
			gitOutput(context.Background(), p.Work, "rebase", "--abort")
			return p, refused("the change does not rebase on %s: %v", p.Base, err)
		}
		a, err := s.Sandbox.Run(ctx, p.Work, p.Check.Test, s.Env)
		if err != nil {
			return p, err
		}
		if a.Exit != 0 {
			return p, refused("after the rebase the test fails (exit %d): no landing", a.Exit)
		}
		base, err := gitOutput(ctx, p.Work, "rev-parse", "origin/"+p.Base)
		if err != nil {
			return p, err
		}
		commits, err := gitOutput(ctx, p.Work, "rev-list", "--reverse", strings.TrimSpace(base)+"..HEAD")
		if err != nil {
			return p, err
		}
		if _, err := gitOutput(ctx, p.Work, "push", "origin", "HEAD:refs/heads/"+p.Base); err != nil {
			return p, refused("the push to %s did not go as a fast-forward: %v", p.Base, err)
		}
		gitOutput(ctx, p.Work, "push", "--quiet", "origin", "--delete", p.Branch)
		p.Landed = strings.Fields(commits)
	case "config":
		if _, err := s.Gate.Config(p.Dir); err != nil {
			return p, err
		}
		backup := filepath.Join(s.pkgDir(caseID), "backup")
		os.RemoveAll(backup)
		for _, rel := range p.Changed {
			src := filepath.Join(p.Dir, rel)
			if _, err := os.Stat(src); err == nil {
				if err := copyFile(src, filepath.Join(backup, rel)); err != nil {
					return p, err
				}
			}
		}
		for _, rel := range p.Changed {
			if err := replaceFile(filepath.Join(p.Work, rel), filepath.Join(p.Dir, rel)); err != nil {
				s.restore(p, backup)
				return p, err
			}
		}
		a, err := s.Sandbox.Run(ctx, p.Dir, p.Check.Test, s.Env)
		if err != nil || a.Exit != 0 {
			s.restore(p, backup)
			return p, refused("the test failed in place (exit %d, %v): the change was undone", a.Exit, err)
		}
	}
	p.State, p.LandedAt = Landed, s.now()
	if err := s.save(&p); err != nil {
		return p, err
	}
	what := fmt.Sprintf("%s: %s", firstOf(p.PR, p.Dir), p.Title)
	if _, err := s.Store.DeployCase(ctx, c.ID, s.pkgDir(caseID), p.LandedAt, what, s.Actor); err != nil {
		return p, err
	}
	return p, nil
}

// restore puts a configuration's originals back from its backup, and removes the files the change added.
func (s *Ship) restore(p Package, backup string) {
	for _, rel := range p.Changed {
		dst := filepath.Join(p.Dir, rel)
		if _, err := os.Stat(filepath.Join(backup, rel)); err == nil {
			replaceFile(filepath.Join(backup, rel), dst)
		} else {
			os.Remove(dst)
		}
	}
}

// RollBack undoes a landed change: a revert of its commits pushed to the default branch as a fast-forward, or the
// configuration's originals put back; the case goes back to testing.
func (s *Ship) RollBack(ctx context.Context, caseID, why string) (Package, error) {
	p, ok, err := s.Load(caseID)
	if err != nil {
		return Package{}, err
	}
	if !ok || p.State != Landed {
		return p, refused("case %s has no landed package to roll back", caseID)
	}
	switch p.Kind {
	case "repo":
		if _, err := s.Gate.Repository(ctx, p.Dir); err != nil {
			return p, err
		}
		dir := filepath.Join(s.Dir, "work", ".revert-"+caseID)
		os.RemoveAll(dir)
		if _, err := gitOutput(ctx, p.Dir, "fetch", "--quiet", "origin", p.Base); err != nil {
			return p, err
		}
		if _, err := gitOutput(ctx, p.Dir, "worktree", "add", "--detach", dir, "origin/"+p.Base); err != nil {
			return p, err
		}
		defer func() {
			gitOutput(context.Background(), p.Dir, "worktree", "remove", "--force", dir)
		}()
		shas := append([]string{}, p.Landed...)
		slices.Reverse(shas)
		args := append([]string{"-c", "user.name=" + s.Identity[0], "-c", "user.email=" + s.Identity[1], "revert", "--no-edit"}, shas...)
		if _, err := gitOutput(ctx, dir, args...); err != nil {
			return p, err
		}
		if _, err := gitOutput(ctx, dir, "push", "origin", "HEAD:refs/heads/"+p.Base); err != nil {
			return p, err
		}
	case "config":
		s.restore(p, filepath.Join(s.pkgDir(caseID), "backup"))
	}
	p.State = RolledBack
	if err := s.save(&p); err != nil {
		return p, err
	}
	_, err = s.Store.RollBackCase(ctx, caseID, why, s.Actor)
	return p, err
}

// commit commits the work tree's change as the user: his global identity, set on the command, so a repository's
// override never takes it.
func (s *Ship) commit(ctx context.Context, dir, msg string) error {
	if s.Identity[0] == "" || s.Identity[1] == "" {
		return refused("no git identity: set user.name and user.email in the global git configuration")
	}
	if _, err := gitOutput(ctx, dir, "add", "-A"); err != nil {
		return err
	}
	_, err := gitOutput(ctx, dir, "-c", "user.name="+s.Identity[0], "-c", "user.email="+s.Identity[1], "-c", "commit.gpgsign=false",
		"commit", "--quiet", "--author", s.Identity[0]+" <"+s.Identity[1]+">", "-m", msg)
	return err
}

func (s *Ship) gh(ctx context.Context, dir string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, firstOf(s.GH, "gh"), args...)
	cmd.Dir = dir
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

func (s *Ship) writeDiff(p Package) error {
	paths, err := treeDiff(p.Dir, p.Work)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, rel := range paths {
		fmt.Fprintf(&b, "=== %s\n", rel)
		out, _ := exec.Command("diff", "-u", filepath.Join(p.Dir, rel), filepath.Join(p.Work, rel)).Output()
		b.Write(out)
	}
	return os.WriteFile(filepath.Join(s.pkgDir(p.Case), "diff.patch"), []byte(b.String()), 0o600)
}

// treeDiff lists the files of b that differ from a's (new ones too), relative to b.
func treeDiff(a, b string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(b, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "node_modules" || d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(b, p)
		nb, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		na, err := os.ReadFile(filepath.Join(a, rel))
		if err != nil || string(na) != string(nb) {
			out = append(out, rel)
		}
		return nil
	})
	return out, err
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git") && rel != ".":
			return filepath.SkipDir
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case d.Type()&fs.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, target)
		}
		return copyFile(p, target)
	})
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, b, st.Mode().Perm())
}

// replaceFile puts src's content at dst in one rename, so a reader sees the old file or the new one.
func replaceFile(src, dst string) error {
	tmp := dst + ".momentum-new"
	if err := copyFile(src, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
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
