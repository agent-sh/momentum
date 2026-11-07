package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agent-sh/momentum/internal/sandbox"
	"github.com/agent-sh/momentum/internal/store"
)

const buildSchema = `{"type":"object","additionalProperties":false,
"required":["summary","test_command","title","body","rollback","signal","window","lesson"],
"properties":{"summary":{"type":"string"},
"test_command":{"type":"array","minItems":1,"items":{"type":"string"},"description":"The program and its arguments, one word each, such as [\"go\", \"test\", \"./internal/x\", \"-run\", \"TestY\"]. The host runs it in the sandbox without a shell, on BASE with only the change's test files (it must fail) and on the change (it must pass). Not a description of the test."},
"title":{"type":"string"},
"body":{"type":"string"},"rollback":{"type":"string"},"window":{"type":"string"},"lesson":{"type":"string","description":"One line of at most 300 characters: what a later agent should know from this case, or empty. A multi-line lesson is joined into one line; one longer than 300 characters is dropped."},"installed":{"type":"string"},
"signal":{"type":"object","additionalProperties":false,"required":["source","kinds","match"],
"properties":{"source":{"type":"string"},"kinds":{"type":"array","items":{"type":"string"}},
"tool":{"type":"string"},"harness":{"type":"string"},"match":{"type":"array","items":{"type":"string"}}}}}}`

type BuildReport struct {
	Summary   string   `json:"summary"`
	Test      []string `json:"test_command"` // the program and its arguments; the host runs them without a shell
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Rollback  string   `json:"rollback"`
	Window    string   `json:"window"`
	Lesson    string   `json:"lesson"`
	Signal    Signal   `json:"signal"`
	Installed string   `json:"installed,omitempty"`
}

func buildReport(raw []byte) (BuildReport, error) {
	var report BuildReport
	if len(raw) > runFileMax {
		return report, errors.New("build report exceeds the report limit")
	}
	var probe struct {
		Test json.RawMessage `json:"test_command"`
	}
	if json.Unmarshal(raw, &probe) == nil && len(probe.Test) > 0 && !bytes.HasPrefix(bytes.TrimSpace(probe.Test), []byte("[")) &&
		!bytes.Equal(bytes.TrimSpace(probe.Test), []byte("null")) {
		return report, refused(`the test command is no list of words: give the program and its arguments as a list, such as ["go", "test", "./..."], which the host runs in the sandbox`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		return report, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return report, err
	}
	for _, name := range []string{"summary", "test_command", "title", "body", "rollback", "signal", "window", "lesson"} {
		if value, ok := fields[name]; !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return report, errors.New("build report has a missing required field")
		}
	}
	for _, text := range []string{report.Summary, report.Title, report.Body, report.Rollback, report.Window} {
		if strings.TrimSpace(text) == "" {
			return report, errors.New("build report has an empty required field")
		}
	}
	if _, err := Argv(report.Test); err != nil {
		return report, err
	}
	if len(report.Body) > 64<<10 {
		return report, errors.New("proposal body exceeds its limit")
	}
	report.Lesson = lessonLine(report.Lesson)
	return report, nil
}

// lessonGiven reports whether a raw build report gave a lesson with text in it.
func lessonGiven(raw []byte) bool {
	var v struct {
		Lesson string `json:"lesson"`
	}
	return json.Unmarshal(raw, &v) == nil && strings.TrimSpace(v.Lesson) != ""
}

// lessonLine is a report's lesson as the proposal keeps it: its lines joined into one, or nothing when that is longer
// than 300 characters. The lesson is advice the case may publish when it closes as improved, not part of the change, so
// one that breaks its form drops alone and the build goes on.
func lessonLine(text string) string {
	line := strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(line) > 300 {
		return ""
	}
	return line
}

func planWindow(text string) (time.Duration, error) {
	if strings.HasSuffix(text, "d") {
		n, err := time.ParseDuration(strings.TrimSuffix(text, "d") + "h")
		if err != nil || n > time.Duration(1<<63-1)/24 {
			return 0, refused("invalid measurement window %q: give whole weeks, such as 7d or 14d", plainClip(text, 40))
		}
		return n * 24, nil
	}
	d, err := time.ParseDuration(text)
	if err != nil {
		return 0, refused("invalid measurement window %q: give whole weeks, such as 7d or 14d", plainClip(text, 40))
	}
	return d, nil
}

const repoAction = "build repository"

var repoName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func (r *Runner) caseRepo(ctx context.Context, c store.Case) (string, error) {
	entries, err := r.Store.CaseLog(ctx, c.ID)
	if err != nil {
		return "", err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Action != repoAction {
			continue
		}
		var record struct {
			Repo    string `json:"repo"`
			Episode int64  `json:"episode"`
		}
		if err := json.Unmarshal([]byte(entries[i].Detail), &record); err != nil {
			return "", err
		}
		if record.Episode != c.Episode {
			return "", nil
		}
		if record.Repo != "" && !repoName.MatchString(record.Repo) {
			return "", errors.New("invalid investigation repository name")
		}
		return record.Repo, nil
	}
	return "", nil
}

// waitingRepoNote is the report's note of a case in testing whose investigation named no repository.
func waitingRepoNote(c store.Case, at time.Time) store.Note {
	return store.Note{ID: repoNoteID(c.ID), Kind: store.NoteProblem, Importance: store.ImportanceLow,
		Title:  "The loop's case " + c.ID + " waits for a repository",
		Detail: "The investigation named no code repository. The case stays in testing without a build run.", At: at}
}

func (r *Runner) buildPackage(ctx context.Context, c store.Case, run store.LoopRun) (Package, error) {
	if r.Ship == nil {
		return Package{}, errors.New("build needs the host ship tool")
	}
	name, err := r.caseRepo(ctx, c)
	if err != nil {
		return Package{}, err
	}
	var source, snapshot string
	for _, repo := range r.pass.manifest.Repositories {
		if repo.Name == name && repo.Error == "" {
			source = repo.Source
			if repo.Fallback == "" {
				snapshot = repo.Commit // origin's branch as this pass fetched it
			}
		}
	}
	if name == "" || source == "" {
		return Package{}, refused("the investigation's repository is not an admitted read_dirs repository")
	}
	p, had, err := r.Ship.Load(c.ID)
	if err != nil {
		return p, err
	}
	if !had || p.State == RolledBack || p.State == Kept || p.State == Withdrawn {
		p, err = r.Ship.Prepare(ctx, c.ID, source)
	}
	if err != nil {
		return p, err
	}
	unlock, err := r.Ship.lock(ctx, c.ID)
	if err != nil {
		return p, err
	}
	defer unlock()
	p, had, err = r.Ship.Load(c.ID)
	if err != nil {
		return p, err
	}
	if !had || (p.State != Prepared && p.State != Checked && p.State != Revising) {
		return p, refused("the package is not ready for a build")
	}
	if filepath.Clean(source) != p.Dir {
		return p, refused("the case's repository changed after preparation")
	}
	// The build's base is the default branch as the host fetches it now, at the pass's snapshot while the branch holds
	// it, not the one the package was prepared at.
	if p, err = r.Ship.buildBase(r.Ship.gitContext(shipContext(ctx, p.Case)), p, snapshot); err != nil {
		return p, err
	}
	if err := r.doneBaseNotes(ctx, c.ID); err != nil { // the base was fetched: what held it is over
		r.logf("base notes of %s: %v", c.ID, err)
	}
	// The build tree holds the base commit's tree and one commit, no history. A public repository's base commit is
	// the default branch its remote serves to anyone, so the scrubber has nothing to keep from the run there, and
	// masking would break the build. Any other tree (its objects, refs and configuration) is scanned before it is
	// mounted: one the scrubber would change is never mounted.
	public, err := r.Ship.publicBase(r.Ship.gitContext(shipContext(ctx, p.Case)), p)
	if err != nil {
		removeInputTree(p.Work)
		return p, err
	}
	if !public {
		if err = checkCloneObjects(ctx, p.Work); err != nil {
			removeInputTree(p.Work) // unscanned, it stays out of storage a later step might read
			if errors.Is(err, errNeedsScrubbing) {
				return p, refused("the repository %s is not public and its base holds text the scrubber would mask, so its build tree cannot be mounted", name)
			}
			return p, err
		}
	}
	if err = os.MkdirAll(run.Dir, 0700); err != nil {
		return p, err
	}
	work := filepath.Join(run.Dir, "work")
	// run.Dir is newly generated host storage and has not been mounted.
	if err = os.Rename(p.Work, work); err != nil {
		return p, err
	}
	p.Work, p.Check, p.State = work, nil, Prepared
	if err = r.Ship.saveAuthorized(ctx, &p); err != nil {
		return p, err
	}
	_, _, err = r.Store.DoneNote(ctx, "loop-repo:"+store.IDPart(c.ID), r.now())
	if errors.Is(err, store.ErrNoNote) {
		err = nil
	}
	return p, err
}

func stageFailure(res Result, key string, err error) (Result, error) {
	var wait *waitingPlan
	if errors.As(err, &wait) {
		err = &shipHold{err: err}
	}
	if held(err) {
		return res, &buildStageError{key: key, err: err}
	}
	res.Status, res.Failure = store.RunFailed, key
	res.Text = key + ": " + err.Error()
	return res, nil
}

type buildStageError struct {
	key string
	err error
}

func (e *buildStageError) Error() string { return e.key + ": " + e.err.Error() }
func (e *buildStageError) Unwrap() error { return e.err }

func (r *Runner) build(ctx context.Context, c store.Case, run store.LoopRun) (Result, error) {
	inv, manifest, err := r.preparedInvocation(run, buildSchema)
	if err != nil {
		return stageFailure(Result{}, "build inputs", err)
	}
	binds, env, err := r.Toolchains.inputs()
	if err != nil {
		return stageFailure(Result{}, "build toolchains", err)
	}
	inv.Binds, inv.Env = append(inv.Binds, binds...), env
	res, err := r.Agent.RunOrFallback(ctx, inv)
	res.InputSummary = manifest.Summary()
	if err != nil {
		return stageFailure(res, "build run", err)
	}
	if res.Status != store.RunOK {
		return res, nil
	}
	report, err := buildReport(res.Report)
	if err != nil {
		return stageFailure(res, "build report", err)
	}
	if report.Lesson == "" && lessonGiven(res.Report) {
		r.logf("build %s: its lesson is over 300 characters, so the proposal has none", c.ID)
	}
	if err := r.Ship.installedReadable(report.Installed); err != nil {
		return stageFailure(res, "build report", err)
	}
	p, ok, err := r.Ship.Load(c.ID)
	if err != nil || !ok {
		return stageFailure(res, "build package", firstError(err, "package is absent"))
	}
	// Reject stale or merged case reports before any host side effect.
	current, err := r.Store.CaseByID(ctx, c.ID)
	if err != nil || current.Episode != run.CaseEpisode || current.Into != "" || current.Status != store.CaseTesting {
		return stageFailure(res, "build authority", firstError(err, "case changed during the build"))
	}
	patch, err := r.patchUnit(ctx, run, p)
	if err != nil {
		return stageFailure(res, "build patch", err)
	}
	// One confined read. Check and Propose share its immutable byte snapshot.
	data, err := readPatch(patch)
	if err != nil {
		return stageFailure(res, "build patch", err)
	}
	snapshot := Patch{data: data}
	if _, err := r.Ship.CheckArgs(ctx, c.ID, report.Test, nil, snapshot); err != nil {
		return stageFailure(res, "build check", err)
	}
	window, err := planWindow(report.Window)
	if err != nil {
		return stageFailure(res, "build propose", err)
	}
	_, err = r.Ship.Propose(ctx, c.ID, Proposal{Title: report.Title, Body: report.Body, Rollback: report.Rollback,
		Lesson: report.Lesson, Signal: report.Signal, Window: window, Installed: report.Installed}, snapshot)
	if err != nil {
		return stageFailure(res, "build propose", err)
	}
	return res, nil
}

func firstError(err error, message string) error {
	if err != nil {
		return err
	}
	return errors.New(message)
}

// patchSpec is a fixed data-export job. It has no harness, relay or host input.
func patchSpec(run store.LoopRun, p Package, host string) (sandbox.Spec, error) {
	// The run clone is a build tree: its one commit, WorkBase, holds BaseCommit's tree, so a diff against it applies
	// to BaseCommit on the host.
	if !regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`).MatchString(p.WorkBase) {
		return sandbox.Spec{}, errors.New("patch base is not a commit id")
	}
	env := map[string]string{"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_ATTR_SOURCE": "4b825dc642cb6eb9a060e54bf8d69288fbee4904", "GIT_ATTR_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0"}
	pins := strings.Join(GitSafety(), " ")
	script := "git -C /work " + pins + " add -A && git -C /work " + pins +
		" diff --cached --binary --no-ext-diff --no-textconv --no-renames --src-prefix=a/ --dst-prefix=b/ " +
		p.WorkBase + " -- > /out/change.patch"
	return sandbox.Spec{Unit: "momentum-run-loop-patch-" + strings.TrimPrefix(runUnit(run.ID, run.Dir, ""), "momentum-run-loop-"),
		Host: host, Binds: []sandbox.Bind{{Src: p.Work, Dst: "/work", Write: true},
			{Src: filepath.Join(run.Dir, "out"), Dst: "/out", Write: true}},
		Dir: "/work", Env: env, Argv: []string{"/bin/sh", "-c", script}}, nil
}

func (r *Runner) patchUnit(ctx context.Context, run store.LoopRun, p Package) (Patch, error) {
	host := filepath.Join(HostDir(run.Dir), "patch")
	if err := hostDir(host); err != nil {
		return Patch{}, err
	}
	spec, err := patchSpec(run, p, host)
	if err != nil {
		return Patch{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, r.Budgets.RunTime)
	defer cancel()
	out, diag := Capture{Limit: MaxStream}, Capture{Limit: MaxStream}
	result, err := spec.Run(cctx, &out, &diag)
	if err != nil || result.Code != 0 || result.Failed || out.Over() || diag.Over() {
		return Patch{}, firstError(err, "patch unit did not export a patch")
	}
	return Patch{Dir: run.Dir, File: "out/change.patch"}, nil
}

// buildScope tells a build, before it starts, which paths of its repository the loop may land, which it may only
// propose for a person to land by hand, and which no change may touch (PathScope).
func buildScope(repo string) string {
	ci := strings.Join(ciPaths, ", ")
	if !strings.EqualFold(repo, SelfRepo) {
		return "Paths: the loop lands a change to any path of this repository except CI configuration (" + ci + "), which no change touches."
	}
	return "Paths: this is the loop's own repository. The loop lands a change only when every path it touches is one of " +
		strings.Join(SelfLandable(), ", ") + ": prefer a fix inside them. A fix that needs other paths is still checked and reviewed, " +
		"then proposed for a person to land by hand; the loop never lands it. No change touches CI configuration (" + ci +
		") or these, which the loop's measurement and its event scanners rest on: " + strings.Join(ProposalProtected(), ", ") +
		" (of internal/supervise/, rules.go and test files may change)."
}

// refusalKeys are the build stages whose failure is the run's own output: its report, its patch, its check or its
// proposal. Preparation, inputs, toolchains and authority are the host's and tell a build nothing to change.
var refusalKeys = []string{"build report", "build patch", "build check", "build propose"}

// hostStages are the build stages whose failure is the host's or the harness's, not the build's output: the next
// build reads past them to the last refusal of its output.
var hostStages = []string{"build prepare", "build inputs", "build toolchains", "build run", "build package", "build authority"}

// lastBuildRefusal is why the host refused the case's newest build of this episode, for the next build's inputs
// (refusal.md), or "" when the newest build was not refused for its own output.
func (r *Runner) lastBuildRefusal(ctx context.Context, c store.Case) (string, error) {
	// only runs that ran and ended on their own: a reservation the budgets refused, or a run the host ended, says
	// nothing of a build's output
	runs, err := r.Store.LoopRuns(ctx, store.RunFilter{Case: c.ID, Ran: true, Judged: true})
	if err != nil {
		return "", err
	}
	var since time.Time // a refusal of a package this one replaced (a withdrawn hand landing) tells it nothing
	if r.Ship != nil {
		if p, ok, err := r.Ship.Load(c.ID); err == nil && ok {
			since = p.Created
		}
	}
	for _, run := range runs { // newest first
		if run.Step != StepBuild || run.Status == store.RunRunning {
			continue
		}
		if run.CaseEpisode == c.Episode && !run.Started.Before(since) && slices.Contains(hostStages, run.Failure) {
			continue // the host's own stage failed: it says nothing of what the build wrote
		}
		if run.CaseEpisode != c.Episode || run.Started.Before(since) || run.Status != store.RunFailed || !slices.Contains(refusalKeys, run.Failure) {
			return "", nil
		}
		text := run.Summary
		i := strings.Index(text, run.Failure+": ")
		if i < 0 {
			return "", nil
		}
		text = text[i:]
		if run.Failure != "build report" && !strings.HasPrefix(text[len(run.Failure)+2:], "refused") {
			return "", nil // a host error of the stage (a network, a git or a gh failure) is no refusal of the build's output
		}
		for _, tail := range []string{" (relay: ", " (the relay refused ", " (untrusted harness diagnostics"} {
			if i := strings.Index(text, tail); i >= 0 {
				text = text[:i]
			}
		}
		return "# Why the host refused this case's last build\n\nRun " + run.ID + " failed at " + run.Failure + ":\n\n" +
			strings.TrimSpace(text) + "\n\nFix that cause in this build.\n", nil
	}
	return "", nil
}
