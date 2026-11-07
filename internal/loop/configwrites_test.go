package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
	"golang.org/x/sys/unix"
)

// The owner's content at a step of a configuration write: saved under the file's own name, as an editor saving in
// place does, so it goes to whichever file the name holds at that step.
const (
	ownerEdit  = "# Rules\n\nmarker-c0ffee the owner's edit\n"
	ownerEdit2 = "# Rules\n\nmarker-c0ffee the owner's later edit\n"
)

var errCrash = errors.New("marker-c0ffee crash")

// writeWorld is one configuration case on a fake home, the owner's last content of its target, and what the test
// does to the case and the file system around the loop's steps.
type writeWorld struct {
	t      *testing.T
	st     *store.Store
	s      *Ship
	r      *Runner
	c      store.Case
	home   string
	last   string // the owner's last content, "" while he wrote none
	passes int
	trail  []string // what happened, for a failure's message

	in        injection // the write's injection, while it runs
	acting    bool
	exchanges int
	fault     unix.Errno // what every write of the target's directory meets, while it persists
	persist   string     // a persistent condition in place: a permission, a fault or the directory moved away
	moved     bool
}

func newWriteWorld(t *testing.T) *writeWorld {
	t.Helper()
	st := testStore(t)
	home := fakeHome(t)
	dir := filepath.Join(home, filepath.Dir(ruleTarget))
	t.Cleanup(func() { os.Chmod(dir, 0o700) }) // before the temporary directory goes
	r := localRunner(t, st)
	r.Ship = configShip(t, st, home)
	r.Ship.Dir = r.Dir
	c := testingCase(t, st)
	raw, _ := json.Marshal(map[string]any{"repo": "", "config": ruleTarget, "episode": c.Episode})
	if err := st.CaseNote(context.Background(), c.ID, repoAction, string(raw), "fixture"); err != nil {
		t.Fatal(err)
	}
	w := &writeWorld{t: t, st: st, s: r.Ship, r: r, c: c, home: home}
	w.s.ConfigStage = w.stage
	return w
}

func (w *writeWorld) note(format string, args ...any) {
	w.trail = append(w.trail, fmt.Sprintf(format, args...))
}

func (w *writeWorld) fail(format string, args ...any) {
	w.t.Helper()
	w.t.Fatalf("%s\ntrail:\n  %s", fmt.Sprintf(format, args...), strings.Join(w.trail, "\n  "))
}

func (w *writeWorld) dir() string { return filepath.Join(w.home, filepath.Dir(ruleTarget)) }

// target is where the owner's file is: its path, or where its directory was moved.
func (w *writeWorld) target() string {
	if w.moved {
		return filepath.Join(w.dir()+"-away", filepath.Base(ruleTarget))
	}
	return filepath.Join(w.home, ruleTarget)
}

// reviewed brings the case's package to a clean posted review on its proposed head. It skips the sandbox check: the
// steps under test are the landing's and the rollback's.
func (w *writeWorld) reviewed() {
	t, ctx, s := w.t, context.Background(), w.s
	t.Helper()
	p, err := s.PrepareConfig(ctx, w.c.ID, ruleTarget, "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Work, ruleTarget), newRule)
	write(t, filepath.Join(p.Work, "tests/test_rules.py"), ruleTest)
	p = fixtureCandidate(t, s, p)
	p.Check = &Check{Tree: strings.TrimSpace(run(t, p.Host, "git", "rev-parse", p.Head+"^{tree}")), TestFiles: []string{"tests/test_rules.py"}, At: time.Now()}
	p.Plan, p.Title, p.Rollback = &Plan{Window: 7 * 24 * time.Hour}, configProposal.Title, configProposal.Rollback
	if err := s.save(&p); err != nil {
		t.Fatal(err)
	}
	if p, err = s.Reviewed(ctx, w.c.ID, Review{Head: p.Head, Text: "Checked every changed file."}); err != nil {
		t.Fatal(err)
	}
	if p, err = w.r.postReview(ctx, p); err != nil || !p.Clean() {
		t.Fatalf("post: %+v %v", p, err)
	}
}

func (w *writeWorld) edit(text string) {
	if err := os.WriteFile(w.target(), []byte(text), 0o600); err != nil {
		w.t.Fatal(err)
	}
	w.last = text
	w.note("the owner saved %q", text)
}

// injection is what happens during one write: the owner saves the file at a step (and again at a later one), the
// write stops at a step as a crash would, the exchange that would put an edit back fails, and at a step something
// happens to the case or to the file system (what), which a file system condition keeps until the test clears it.
type injection struct {
	edit, edit2, crash string
	failBack           bool
	at, what           string
}

func (in injection) String() string {
	var parts []string
	if in.edit != "" {
		parts = append(parts, "edit@"+in.edit)
	}
	if in.edit2 != "" {
		parts = append(parts, "edit2@"+in.edit2)
	}
	if in.failBack {
		parts = append(parts, "failed exchange back")
	}
	if in.what != "" {
		parts = append(parts, in.what+"@"+in.at)
	}
	if in.crash != "" {
		parts = append(parts, "crash@"+in.crash)
	}
	return strings.Join(parts, ",")
}

// stage is the ship's hook between a write's steps and before each write of the target's directory ("op NAME").
func (w *writeWorld) stage(step string) error {
	if strings.HasPrefix(step, "op ") {
		if w.acting && w.in.failBack && step == "op exchange" {
			if w.exchanges++; w.exchanges == 2 {
				return unix.EIO
			}
		}
		if w.fault != 0 {
			return w.fault
		}
		return nil
	}
	if !w.acting {
		return nil
	}
	if step == w.in.edit {
		w.edit(ownerEdit)
	}
	if step == w.in.edit2 {
		w.edit(ownerEdit2)
	}
	if step == w.in.at {
		w.happen(w.in.what)
	}
	if step == w.in.crash {
		w.note("crash at %s", step)
		return errCrash
	}
	return nil
}

// happen is what an injection does to the case or to the file system.
func (w *writeWorld) happen(what string) {
	ctx := context.Background()
	var err error
	switch what {
	case "closed":
		_, err = w.st.CloseCase(ctx, w.c.ID, store.NotWorthIt, "", "the owner closes it", "test")
	case "reopened":
		if _, err = w.st.CloseCase(ctx, w.c.ID, store.NotWorthIt, "", "the owner closes it", "test"); err == nil {
			_, err = w.st.ReopenCase(ctx, w.c.ID, nil, "the owner reopens it", "test")
		}
	case "duplicate":
		_, err = w.st.CloseCase(ctx, w.c.ID, store.Duplicate, w.other().ID, "the same cause", "test")
	case "merged":
		// a signature another case holds, added to this one, merges this one into it
		_, _, err = w.st.AddCaseSignature(ctx, w.c.ID, w.other().Signatures[0], "test")
	case "EACCES":
		err = os.Chmod(w.dir(), 0o555)
		w.persist = what
	case "EROFS", "EPERM":
		w.fault = map[string]unix.Errno{"EROFS": unix.EROFS, "EPERM": unix.EPERM}[what]
		w.persist = what
	case "removed":
		err = os.Rename(w.dir(), w.dir()+"-away")
		w.moved, w.persist = err == nil, what
	}
	w.note("%s: %v", what, err)
}

// clear ends a persistent condition: the directory's permission back, the fault gone, the directory back where it was.
func (w *writeWorld) clear() {
	switch w.persist {
	case "EACCES":
		os.Chmod(w.dir(), 0o700)
	case "EROFS", "EPERM":
		w.fault = 0
	case "removed":
		if _, err := os.Lstat(w.dir()); !errors.Is(err, fs.ErrNotExist) {
			w.fail("the target's directory was made again while it was away: %v", err)
		}
		if err := os.Rename(w.dir()+"-away", w.dir()); err != nil {
			w.fail("restore: %v", err)
		}
		w.moved = false
	}
	w.note("%s cleared", w.persist)
	w.persist = ""
}

// other is the case this one goes into when it is merged.
func (w *writeWorld) other() store.Case {
	ctx := context.Background()
	c, _, err := w.st.ObserveCase(ctx, store.Finding{Family: "failures", Signature: "failures/v1:zz-merge", Title: "the same cause",
		Evidence: []store.Evidence{{Ref: "file:/t/zz-merge", At: time.Now().Add(-time.Hour)}}}, "test")
	if err != nil {
		w.fail("the other case: %v", err)
	}
	if c.Status == store.CaseOpen {
		mech, next := "the same cause", "the same fix"
		if c, err = w.st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
			w.fail("the other case: %v", err)
		}
	}
	return c
}

// act runs one write with an injection. The step "before" is the state itself, before the write begins, and "after"
// is the moment the write returns.
func (w *writeWorld) act(name string, in injection, action func() error) {
	w.in, w.acting, w.exchanges = in, true, 0
	defer func() { w.in, w.acting = injection{}, false }()
	if w.stage("before") != nil {
		return
	}
	w.note("%s: %v", name, action())
	w.stage("after")
}

// land is the loop's landing, or its resume.
func (w *writeWorld) land() error {
	_, err := w.s.Land(context.Background(), w.c.ID)
	return err
}

// ownerRollBack is momentum case rollback.
func (w *writeWorld) ownerRollBack() error {
	_, err := w.s.RollBack(context.Background(), w.c.ID, "the owner undoes it")
	return err
}

// loopRollBack is the measure step's rollback, or a resumed one.
func (w *writeWorld) loopRollBack() error {
	_, err := w.s.rollBack(context.Background(), w.c.ID, "no measured effect", false)
	return err
}

// nextPass is what a live pass does for the case: the selection (a resumed rollback, a deployment, the build's
// package, prepared or brought to the file as it is, with no agent run) and then the reviews' landings.
func (w *writeWorld) nextPass() {
	ctx := context.Background()
	w.passes++
	w.r.pass = &passInputs{}
	if b, err := os.ReadFile(w.target()); err == nil && !w.moved {
		w.r.pass.manifest.Configuration.Files = []ConfigFile{{Path: ruleTarget, Root: ".config/agents", SHA256: hashOf(b)}}
	}
	c, step, _, err := w.r.pick(ctx, "failures", w.c.ID, false)
	if err != nil {
		w.fail("pass %d: %v", w.passes, err)
	}
	if step == StepBuild && c.ID == w.c.ID {
		id := fmt.Sprintf("fixture-build-%d", w.passes)
		_, err := w.r.buildPackage(ctx, c, store.LoopRun{ID: id, Case: c.ID, Dir: filepath.Join(w.r.Dir, "runs", id), CaseEpisode: c.Episode})
		w.note("pass %d build: %v", w.passes, err)
	}
	for _, d := range w.r.reviews(ctx, w.c.ID) {
		w.note("pass %d %s: %s", w.passes, d.Step, d.Skipped)
	}
	w.state(fmt.Sprintf("after pass %d", w.passes))
}

// state notes the package's state and checks the owner's last content.
func (w *writeWorld) state(when string) {
	p, _, _ := w.s.Load(w.c.ID)
	c, _ := w.st.CaseByID(context.Background(), w.c.ID)
	w.note("%s: package %s, case %s", when, p.State, c.Status)
	w.content(when)
}

// content: the file holds the owner's last content, or, while he wrote none, its old or new content, in its mode.
// Between steps his content may sit at the swap name an exchange displaced it to, for the next step to put back.
func (w *writeWorld) content(when string) {
	w.contentIn(when, false)
}

func (w *writeWorld) contentIn(when string, final bool) {
	b, err := os.ReadFile(w.target())
	if err != nil {
		w.fail("%s: %v", when, err)
	}
	displaced, _ := os.ReadFile(filepath.Join(filepath.Dir(w.target()), swapName(w.c.ID)))
	switch got := string(b); {
	case w.last != "" && got != w.last && (final || string(displaced) != w.last):
		w.fail("%s: the file holds %q, not the owner's last content %q", when, got, w.last)
	case w.last == "" && got != oldRule && got != newRule:
		w.fail("%s: the file holds %q", when, got)
	}
	if info, err := os.Stat(w.target()); err != nil || info.Mode() != 0o640 {
		w.fail("%s: the file's mode %v %v", when, info.Mode(), err)
	}
}

// clean: nothing but the target is under the home.
func (w *writeWorld) clean(when string) {
	var files []string
	filepath.WalkDir(w.home, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(w.home, path)
			files = append(files, rel)
		}
		return nil
	})
	if !slices.Equal(files, []string{ruleTarget}) {
		w.fail("%s: the home holds %v", when, files)
	}
}

// loggedPaths are the files the case log names.
var loggedPaths = regexp.MustCompile(`/[^\s,;:()"]+\.json`)

// settled: the package is where an existing step takes it on (a build on the file as it is, a landed change being
// measured, or an inert package of a case that is not in testing), every file the case log names is there, and
// nothing but the target is under the home.
func (w *writeWorld) settled(when string) {
	ctx := context.Background()
	w.contentIn(when, true)
	p, ok, err := w.s.Load(w.c.ID)
	c, cerr := w.st.CaseByID(ctx, w.c.ID)
	if err != nil || !ok || cerr != nil {
		w.fail("%s: %v %v %v", when, ok, err, cerr)
	}
	b, _ := os.ReadFile(w.target())
	switch {
	case p.State == Landing || p.State == RollingBack:
		w.fail("%s: the package is still %s with its case %s", when, p.State, c.Status)
	case c.Status == store.CaseDeployed || c.Status == store.CaseMeasuring:
		if p.State != Landed {
			w.fail("%s: the package is %s with its case %s", when, p.State, c.Status)
		}
	case p.State == Landed:
		w.fail("%s: a landed change with its case %s", when, c.Status)
	case c.Status == store.CaseTesting:
		if p.State != Prepared || p.TargetBase == nil || p.TargetBase.SHA256 != hashOf(b) {
			w.fail("%s: the package is %s, not a build on the file as it is", when, p.State)
		}
	}
	ids := []string{w.c.ID}
	if c.Into != "" {
		ids = append(ids, c.Into)
	}
	for _, id := range ids {
		log, err := w.st.CaseLog(ctx, id)
		if err != nil {
			w.fail("%s: %v", when, err)
		}
		for _, e := range log {
			for _, path := range loggedPaths.FindAllString(e.Detail, -1) {
				if _, err := os.Stat(path); err != nil {
					w.fail("%s: the case log's %q names %s, which is gone: %v", when, e.Action, path, err)
				}
			}
		}
	}
	w.clean(when)
}

// secondCase is another case in testing.
func secondCase(t *testing.T, st *store.Store) store.Case {
	t.Helper()
	ctx := context.Background()
	c, _, err := st.ObserveCase(ctx, store.Finding{Family: "failures", Signature: "failures/v1:zz-later", Title: "a later case",
		Evidence: []store.Evidence{{Ref: "file:/t/zz-later", At: time.Now().Add(-time.Hour)}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	mech, next := "Answer is off by one", "assert Answer() == 42"
	if c, err = st.UpdateCase(ctx, c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mech, NextTest: &next}, "test"); err != nil {
		t.Fatal(err)
	}
	return c
}

// done: the first case ends (a change in place is rolled back by the owner, then a case still open is closed), and a
// second case prepares on the file as it is.
func (w *writeWorld) done() {
	ctx := context.Background()
	c, _ := w.st.CaseByID(ctx, w.c.ID)
	if c.Status == store.CaseDeployed || c.Status == store.CaseMeasuring {
		if p, err := w.s.RollBack(ctx, w.c.ID, "the owner ends it"); err != nil || p.State != RolledBack {
			w.fail("the owner's rollback at the end: %s %v", p.State, err)
		}
		w.contentIn("after the owner's last rollback", true)
	}
	if c, _ = w.st.CaseByID(ctx, w.c.ID); c.Status != store.CaseClosed {
		if _, err := w.st.CloseCase(ctx, w.c.ID, store.NotWorthIt, "", "the owner ends it", "test"); err != nil {
			w.fail("close: %v", err)
		}
	}
	second := secondCase(w.t, w.st)
	q, err := w.s.PrepareConfig(ctx, second.ID, ruleTarget, "")
	b, _ := os.ReadFile(w.target())
	if err != nil || q.TargetBase == nil || q.TargetBase.SHA256 != hashOf(b) {
		w.fail("a second case on the file once the first is done: %v", err)
	}
	w.contentIn("at the end", true)
	w.clean("at the end")
}

// writeAction is a write from a package state, and its steps in order.
type writeAction struct {
	start, name string
	setup       func(w *writeWorld)
	do          func(w *writeWorld) error
	steps       []string
}

func writeActions() []writeAction {
	crashed := func(w *writeWorld, name, step string, do func() error) {
		w.act(name, injection{crash: step}, do)
	}
	proposed := func(w *writeWorld) { w.reviewed() }
	landing := func(w *writeWorld) { w.reviewed(); crashed(w, "land", "journaled", w.land) }
	written := func(w *writeWorld) { w.reviewed(); crashed(w, "land", "written", w.land) }
	landed := func(w *writeWorld) {
		w.reviewed()
		if err := w.land(); err != nil {
			w.fail("land: %v", err)
		}
	}
	rollingBack := func(w *writeWorld) { landed(w); crashed(w, "rollback", "rollback journaled", w.loopRollBack) }
	rolledBack := func(w *writeWorld) { landed(w); crashed(w, "rollback", "rollback saved", w.loopRollBack) }
	ownersLanding := func(w *writeWorld) { written(w); crashed(w, "owner's rollback", "rollback journaled", w.ownerRollBack) }
	land := []string{"journal written", "journaled", "swap writing", "swap written", "exchanged", "written", "landed"}
	resume := []string{"swap writing", "swap written", "exchanged", "written", "landed"}
	rollback := []string{"rollback record written", "rollback journaled", "swap writing", "swap written", "exchanged", "rolled back", "rollback saved"}
	resumeRollback := []string{"swap writing", "swap written", "exchanged", "rolled back", "rollback saved"}
	abandon := []string{"rollback record written", "rollback journaled", "swap writing", "swap written", "exchanged", "rolled back"}
	ownerRB := (*writeWorld).ownerRollBack
	loopLand := (*writeWorld).land
	loopRollBack := (*writeWorld).loopRollBack
	return []writeAction{
		{"proposed", "the loop lands", proposed, loopLand, land},
		{"landing", "the loop resumes the landing", landing, loopLand, resume},
		{"landing", "the owner rolls it back", landing, ownerRB, nil},
		{"landing with the change written", "the loop resumes the landing", written, loopLand, []string{"landed"}},
		{"landing with the change written", "the owner rolls it back", written, ownerRB, abandon},
		{"landed", "the owner rolls it back", landed, ownerRB, rollback},
		{"landed", "the loop rolls it back", landed, loopRollBack, rollback},
		{"rolling back", "the owner rolls it back", rollingBack, ownerRB, resumeRollback},
		{"rolling back", "the loop resumes the rollback", rollingBack, loopRollBack, resumeRollback},
		{"rolled back", "the owner rolls it back", rolledBack, ownerRB, nil},
		{"rolled back", "the loop resumes the rollback", rolledBack, loopRollBack, nil},
		{"the owner rolling back a landing", "the owner rolls it back", ownersLanding, ownerRB, resumeRollback[:4]},
		{"the owner rolling back a landing", "the loop resumes the landing", ownersLanding, loopLand, nil},
	}
}

// writeHappenings are what may happen to the case or the file system at a step: the case closed by hand, reopened,
// closed as a duplicate or merged into another; a permission, a read-only file system or a refused operation that
// stays; the target's directory moved away and later back.
var writeHappenings = []string{"closed", "reopened", "duplicate", "merged", "EACCES", "EROFS", "EPERM", "removed"}

func writeInjections(steps []string) []injection {
	var out []injection
	all := append([]string{"before"}, steps...)
	for _, step := range all {
		out = append(out, injection{crash: step}, injection{edit: step}, injection{edit: step, crash: step})
	}
	if slices.Contains(steps, "swap written") {
		// an edit before the exchange: the displaced file is not what was read, and the exchange is undone
		const back = "exchanged back"
		out = append(out, injection{edit: "swap written", crash: back}, injection{edit: "swap written", edit2: back},
			injection{edit: "swap written", edit2: back, crash: back}, injection{edit: "swap written", failBack: true},
			// his edit sits at the swap name when he saves the file again
			injection{edit: "swap written", failBack: true, edit2: "after"})
	}
	for _, step := range all {
		for _, what := range writeHappenings {
			out = append(out, injection{at: step, what: what})
		}
	}
	return out
}

// writeSlots bounds the scenarios that run at once.
var writeSlots = make(chan struct{}, 8)

// Every write of a configuration package, from every state, meets at each of its steps a crash, the owner's edit,
// both, the case closed, reopened, merged or closed as a duplicate, a write error that stays, and the target's
// directory moved away; then the loop's next passes and the owner's case rollback run, in both orders, and a condition
// that stays is cleared before the case is taken on again. The owner's last content is never lost, the package ends
// where an existing step takes it on, no step needs a copy by hand, every file the case log names is there, nothing
// but the target is written under the home, and once the case is done a second case prepares on the file.
func TestConfigurationWritesSurviveEveryCrashAndEdit(t *testing.T) {
	for _, a := range writeActions() {
		for _, in := range writeInjections(a.steps) {
			for _, ownerFirst := range []bool{false, true} {
				order := "then the next pass"
				if ownerFirst {
					order = "then the owner's rollback"
				}
				t.Run(fmt.Sprintf("%s/%s/%s/%s", a.start, a.name, in, order), func(t *testing.T) {
					t.Parallel()
					writeSlots <- struct{}{}
					defer func() { <-writeSlots }()
					w := newWriteWorld(t)
					a.setup(w)
					w.state("at the start")
					w.act(a.name, in, func() error { return a.do(w) })
					w.state("after the write")
					if ownerFirst {
						w.note("the owner's rollback: %v", w.ownerRollBack())
						w.state("after the owner's rollback")
						w.nextPass()
					} else {
						w.nextPass()
						w.note("the owner's rollback: %v", w.ownerRollBack())
						w.state("after the owner's rollback")
					}
					if w.persist != "" {
						w.clear()
						w.note("the owner's rollback: %v", w.ownerRollBack())
						w.state("after the owner's rollback")
					}
					w.nextPass()
					w.nextPass()
					w.settled("settled")
					w.done()
				})
			}
		}
	}
}

// The journal a case log names stays at that path when a later package of the case replaces this one.
func TestTheJournalTheCaseLogNamesStaysWhereItSays(t *testing.T) {
	w := newWriteWorld(t)
	w.reviewed()
	w.act("land", injection{crash: "written"}, w.land)
	w.edit(ownerEdit)
	w.note("land: %v", w.land())
	w.note("the owner's rollback: %v", w.ownerRollBack())
	w.nextPass() // the next build prepares a new package
	if p, _, _ := w.s.Load(w.c.ID); p.State != Prepared {
		w.fail("the next build: %s", p.State)
	}
	log, _ := w.st.CaseLog(context.Background(), w.c.ID)
	named := 0
	for _, e := range log {
		for _, path := range loggedPaths.FindAllString(e.Detail, -1) {
			named++
			var j ConfigJournal
			if ok, err := readRecord(filepath.Dir(path), filepath.Base(path), &j); !ok || err != nil || string(j.Old.Content) != oldRule {
				w.fail("the case log's %q names %s: %v %v", e.Action, path, ok, err)
			}
		}
	}
	if named == 0 {
		w.fail("the case log names no journal")
	}
	w.content("at the end")
}

// What an interrupted write left at the swap name, when the file holds anything but the step's own content, stays
// out of the file: the file's content came after the exchange, whatever its time says (a restore from a backup keeps
// an old one). The other content is kept in the package's records and named in the case log.
func TestTheFileWinsOverWhatAnExchangeDisplaced(t *testing.T) {
	for name, back := range map[string]time.Duration{"a later time": time.Second, "an earlier time": -24 * time.Hour} {
		t.Run(name, func(t *testing.T) {
			w := newWriteWorld(t)
			ctx := context.Background()
			w.reviewed()
			w.act("land", injection{edit: "swap written", failBack: true}, w.land)
			w.edit(ownerEdit2) // he saves the file again, over the change, or restores it from a backup
			if err := os.Chtimes(w.target(), time.Now().Add(back), time.Now().Add(back)); err != nil {
				t.Fatal(err)
			}
			w.note("land: %v", w.land())
			w.contentIn("after the landing", true)
			swap := filepath.Join(filepath.Dir(w.target()), swapName(w.c.ID))
			if _, err := os.Lstat(swap); !errors.Is(err, os.ErrNotExist) {
				w.fail("left at the swap name: %v", err)
			}
			kept := "displaced-" + hashOf([]byte(ownerEdit))[:16] + ".json"
			var c ConfigState
			if ok, err := readRecord(recordsOf(t, w.s, w.c.ID), kept, &c); !ok || err != nil || string(c.Content) != ownerEdit {
				w.fail("the other content is not kept: %v %v", ok, err)
			}
			log, _ := w.st.CaseLog(ctx, w.c.ID)
			found := false
			for _, e := range log {
				found = found || e.Action == "displaced content kept" && strings.Contains(e.Detail, kept) && strings.Contains(e.Detail, "another content")
			}
			if !found {
				w.fail("the case log does not name the kept content")
			}
			w.nextPass()
			w.nextPass()
			w.settled("settled")
			w.done()
		})
	}
}

// A case merged into another while its landing is interrupted: the owner's case rollback finishes the landing, and the
// file is free for the case it went into.
func TestTheOwnersRollbackFinishesTheLandingOfAMergedCase(t *testing.T) {
	for _, what := range []string{"duplicate", "merged"} {
		t.Run(what, func(t *testing.T) {
			w := newWriteWorld(t)
			ctx := context.Background()
			w.reviewed()
			w.act("land", injection{crash: "written"}, w.land)
			w.happen(what)
			if c, _ := w.st.CaseByID(ctx, w.c.ID); c.Into == "" {
				w.fail("the case was not merged")
			}
			if err := w.ownerRollBack(); err != nil {
				w.fail("the owner's rollback of a merged case's landing: %v", err)
			}
			if p, _, _ := w.s.Load(w.c.ID); p.State != RolledBack {
				w.fail("the package: %s", p.State)
			}
			w.contentIn("after the rollback", true)
			if b, _ := os.ReadFile(w.target()); string(b) != oldRule {
				w.fail("the rollback left %q", b)
			}
			if _, err := w.s.PrepareConfig(ctx, w.other().ID, ruleTarget, ""); err != nil {
				w.fail("the case it went into, on the file: %v", err)
			}
		})
	}
}

// A landed change whose case never deployed it, then was closed: the owner's case rollback restores the file, and the
// case log says so.
func TestTheOwnerRollsBackALandedChangeWhoseCaseNeverDeployedIt(t *testing.T) {
	w := newWriteWorld(t)
	ctx := context.Background()
	w.reviewed()
	w.act("land", injection{crash: "landed"}, w.land)
	w.happen("closed")
	if err := w.ownerRollBack(); err != nil {
		w.fail("the owner's rollback: %v", err)
	}
	if b, _ := os.ReadFile(w.target()); string(b) != oldRule {
		w.fail("the rollback left %q", b)
	}
	c, _ := w.st.CaseByID(ctx, w.c.ID)
	log, _ := w.st.CaseLog(ctx, w.c.ID)
	if last := log[len(log)-1]; c.Status != store.CaseClosed || last.Action != "rolled back" {
		w.fail("the case %s, its last line %+v", c.Status, last)
	}
}

// A landing the file system refuses to write holds before it journals anything: the package stays proposed and lands
// once the owner gives the directory back.
func TestALandingTheFileSystemRefusesHoldsProposed(t *testing.T) {
	for _, what := range []string{"EACCES", "EROFS"} {
		t.Run(what, func(t *testing.T) {
			w := newWriteWorld(t)
			w.reviewed()
			w.happen(what)
			for i := 0; i < 2; i++ {
				if err := w.land(); !held(err) || !strings.Contains(err.Error(), "cannot be written") {
					w.fail("land %d: %v", i, err)
				}
				if p, _, _ := w.s.Load(w.c.ID); p.State != Proposed {
					w.fail("land %d: the package is %s", i, p.State)
				}
			}
			if _, err := os.Stat(filepath.Join(recordsOf(t, w.s, w.c.ID), configJournal)); !errors.Is(err, os.ErrNotExist) {
				w.fail("journaled: %v", err)
			}
			w.clear()
			if err := w.land(); err != nil {
				w.fail("land once the directory takes a file: %v", err)
			}
			w.contentIn("landed", true)
		})
	}
}

// An interrupted landing whose target's directory the owner moved away is set aside by the loop, and the owner's case
// rollback of one is finished, with the journal named.
func TestALandingWhoseDirectoryIsGoneIsSetAside(t *testing.T) {
	for _, owner := range []bool{false, true} {
		t.Run(fmt.Sprint("owner ", owner), func(t *testing.T) {
			w := newWriteWorld(t)
			w.reviewed()
			w.act("land", injection{crash: "written"}, w.land)
			w.happen("removed")
			step := w.land
			if owner {
				step = w.ownerRollBack
			}
			w.note("step: %v", step())
			if p, _, _ := w.s.Load(w.c.ID); p.State != RolledBack {
				w.fail("the package: %s", p.State)
			}
			w.clear()
			w.nextPass()
			w.nextPass()
			w.settled("settled")
			w.done()
		})
	}
}

// A busy file's hold names the command that frees it.
func TestABusyFilesHoldNamesCaseRollback(t *testing.T) {
	w := newWriteWorld(t)
	w.reviewed()
	w.act("land", injection{crash: "written"}, w.land)
	second := secondCase(t, w.st)
	if _, err := w.s.PrepareConfig(context.Background(), second.ID, ruleTarget, ""); !held(err) || !strings.Contains(err.Error(), "momentum case rollback "+w.c.ID) {
		w.fail("the busy hold: %v", err)
	}
}

// A landing whose journal is gone while its file holds the change is held for the owner, not built again over the
// change, and the owner's case rollback restores the file from what the package's host repository holds.
func TestALandingWithoutItsJournalIsHeld(t *testing.T) {
	w := newWriteWorld(t)
	w.reviewed()
	w.act("land", injection{crash: "written"}, w.land)
	if err := os.Remove(filepath.Join(recordsOf(t, w.s, w.c.ID), configJournal)); err != nil {
		t.Fatal(err)
	}
	if err := w.land(); !held(err) || !strings.Contains(err.Error(), "momentum case rollback") {
		w.fail("the landing without its journal: %v", err)
	}
	if p, _, _ := w.s.Load(w.c.ID); p.State != Landing {
		w.fail("the package: %s", p.State)
	}
	if err := w.ownerRollBack(); err != nil {
		w.fail("the owner's rollback: %v", err)
	}
	if b, _ := os.ReadFile(w.target()); string(b) != oldRule {
		w.fail("the rollback left %q", b)
	}
	w.nextPass()
	w.nextPass()
	w.settled("settled")
	w.done()
}
