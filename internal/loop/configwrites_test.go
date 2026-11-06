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

// writeWorld is one configuration case on a fake home, and the owner's last content of its target.
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
}

func newWriteWorld(t *testing.T) *writeWorld {
	t.Helper()
	st := testStore(t)
	home := fakeHome(t)
	r := localRunner(t, st)
	r.Ship = configShip(t, st, home)
	r.Ship.Dir = r.Dir
	c := testingCase(t, st)
	raw, _ := json.Marshal(map[string]any{"repo": "", "config": ruleTarget, "episode": c.Episode})
	if err := st.CaseNote(context.Background(), c.ID, repoAction, string(raw), "fixture"); err != nil {
		t.Fatal(err)
	}
	return &writeWorld{t: t, st: st, s: r.Ship, r: r, c: c, home: home}
}

func (w *writeWorld) note(format string, args ...any) {
	w.trail = append(w.trail, fmt.Sprintf(format, args...))
}

func (w *writeWorld) fail(format string, args ...any) {
	w.t.Helper()
	w.t.Fatalf("%s\ntrail:\n  %s", fmt.Sprintf(format, args...), strings.Join(w.trail, "\n  "))
}

func (w *writeWorld) target() string { return filepath.Join(w.home, ruleTarget) }

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
// write stops at a step as a crash would, and the exchange that would put an edit back fails.
type injection struct {
	edit, edit2, crash string
	failBack           bool
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
	if in.crash != "" {
		parts = append(parts, "crash@"+in.crash)
	}
	return strings.Join(parts, ",")
}

// act runs one write with an injection. The step "before" is the state itself, before the write begins, and "after"
// is the moment the write returns.
func (w *writeWorld) act(name string, in injection, action func() error) {
	stage := func(step string) error {
		if step == in.edit {
			w.edit(ownerEdit)
		}
		if step == in.edit2 {
			w.edit(ownerEdit2)
		}
		if step == in.crash {
			w.note("crash at %s", step)
			return errCrash
		}
		return nil
	}
	if stage("before") != nil {
		return
	}
	w.s.ConfigStage = stage
	defer func() { w.s.ConfigStage = nil }()
	if in.failBack {
		calls, real := 0, exchange
		exchange = func(fromDir int, from string, toDir int, to string, flags uint) error {
			if calls++; calls == 2 {
				return unix.EIO
			}
			return real(fromDir, from, toDir, to, flags)
		}
		defer func() { exchange = real }()
	}
	w.note("%s: %v", name, action())
	stage("after")
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
	if b, err := os.ReadFile(w.target()); err == nil {
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

// settled: the package is where an existing step takes it on (a build on the file as it is, or a landed change being
// measured), every file the case log names is there, and nothing but the target is under the home.
func (w *writeWorld) settled(when string) {
	ctx := context.Background()
	p, ok, err := w.s.Load(w.c.ID)
	c, cerr := w.st.CaseByID(ctx, w.c.ID)
	if err != nil || !ok || cerr != nil {
		w.fail("%s: %v %v %v", when, ok, err, cerr)
	}
	w.contentIn(when, true)
	b, _ := os.ReadFile(w.target())
	switch {
	case p.State == Prepared && c.Status == store.CaseTesting:
		if p.TargetBase == nil || p.TargetBase.SHA256 != hashOf(b) {
			w.fail("%s: the next build is not on the file as it is", when)
		}
	case p.State == Landed && c.Status == store.CaseDeployed:
	default:
		w.fail("%s: the package is %s with its case %s", when, p.State, c.Status)
	}
	log, err := w.st.CaseLog(ctx, w.c.ID)
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
	w.clean(when)
}

// done: the first case ends (a change in place is rolled back by the owner, then the case is closed), and a second
// case prepares on the file as it is.
func (w *writeWorld) done() {
	ctx := context.Background()
	c, _ := w.st.CaseByID(ctx, w.c.ID)
	if c.Status == store.CaseDeployed || c.Status == store.CaseMeasuring {
		if p, err := w.s.RollBack(ctx, w.c.ID, "the owner ends it"); err != nil || p.State != RolledBack {
			w.fail("the owner's rollback at the end: %s %v", p.State, err)
		}
		w.contentIn("after the owner's last rollback", true)
	}
	if _, err := w.st.CloseCase(ctx, w.c.ID, store.NotWorthIt, "", "the owner ends it", "test"); err != nil {
		w.fail("close: %v", err)
	}
	second := sameEvidenceCase(w.t, w.st, w.c)
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

func writeInjections(steps []string) []injection {
	var out []injection
	for _, step := range append([]string{"before"}, steps...) {
		out = append(out, injection{crash: step}, injection{edit: step}, injection{edit: step, crash: step})
	}
	if slices.Contains(steps, "swap written") {
		// an edit before the exchange: the displaced file is not what was read, and the exchange is undone
		const back = "exchanged back"
		out = append(out, injection{edit: "swap written", crash: back}, injection{edit: "swap written", edit2: back},
			injection{edit: "swap written", edit2: back, crash: back}, injection{edit: "swap written", failBack: true},
			// his edit sits at the swap name when he saves the file again: the file's is the newer, and the other is kept
			injection{edit: "swap written", failBack: true, edit2: "after"})
	}
	return out
}

// Every write of a configuration package, from every state, meets a crash at each of its steps, the owner's edit at
// each, and both; then the loop's next passes and the owner's case rollback run, in both orders. The owner's last
// content is never lost, the package ends where an existing step takes it on, no step needs a copy by hand, every
// file the case log names is there, nothing but the target is written under the home, and once the case is done a
// second case prepares on the file.
func TestConfigurationWritesSurviveEveryCrashAndEdit(t *testing.T) {
	for _, a := range writeActions() {
		for _, in := range writeInjections(a.steps) {
			for _, ownerFirst := range []bool{false, true} {
				order := "then the next pass"
				if ownerFirst {
					order = "then the owner's rollback"
				}
				t.Run(fmt.Sprintf("%s/%s/%s/%s", a.start, a.name, in, order), func(t *testing.T) {
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

// An edit an interrupted write displaced that is newer than what the file holds goes back into the file, and the
// older one is kept in the package's records and named in the case log: nothing of the owner's is removed unkept.
func TestADisplacedEditNewerThanTheFileGoesBack(t *testing.T) {
	w := newWriteWorld(t)
	ctx := context.Background()
	w.reviewed()
	w.act("land", injection{crash: "written"}, w.land)
	w.edit(ownerEdit)
	swap := filepath.Join(filepath.Dir(w.target()), swapName(w.c.ID))
	write(t, swap, ownerEdit2) // his later edit, displaced to the swap name in the file's mode
	if err := os.Chmod(swap, 0o640); err != nil {
		t.Fatal(err)
	}
	// written after the file's edit, beyond the file system's clock tick
	if err := os.Chtimes(swap, time.Now().Add(time.Second), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	w.last = ownerEdit2
	w.note("land: %v", w.land())
	w.content("after the landing")
	if _, err := os.Lstat(swap); !errors.Is(err, os.ErrNotExist) {
		w.fail("left at the swap name: %v", err)
	}
	var kept ConfigState
	if ok, err := readRecord(recordsOf(t, w.s, w.c.ID), "displaced-"+hashOf([]byte(ownerEdit))[:16]+".json", &kept); !ok || err != nil || string(kept.Content) != ownerEdit {
		w.fail("the older edit is not kept: %v %v", ok, err)
	}
	log, _ := w.st.CaseLog(ctx, w.c.ID)
	found := false
	for _, e := range log {
		found = found || e.Action == "displaced content kept" && strings.Contains(e.Detail, "displaced-"+hashOf([]byte(ownerEdit))[:16])
	}
	if !found {
		w.fail("the case log does not name the kept edit")
	}
	w.nextPass()
	w.nextPass()
	w.settled("settled")
	w.done()
}
