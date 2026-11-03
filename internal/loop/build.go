package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/sandbox"
	"github.com/agent-sh/momentum/internal/store"
)

const buildSchema = `{"type":"object","additionalProperties":false,
"required":["summary","test","title","body","rollback","signal","window","lesson"],
"properties":{"summary":{"type":"string"},"test":{"type":"string"},"title":{"type":"string"},
"body":{"type":"string"},"rollback":{"type":"string"},"window":{"type":"string"},"lesson":{"type":"string"},"installed":{"type":"string"},
"signal":{"type":"object","additionalProperties":false,"required":["source","kinds","match"],
"properties":{"source":{"type":"string"},"kinds":{"type":"array","items":{"type":"string"}},
"tool":{"type":"string"},"harness":{"type":"string"},"match":{"type":"array","items":{"type":"string"}}}}}}`

type BuildReport struct {
	Summary   string `json:"summary"`
	Test      string `json:"test"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Rollback  string `json:"rollback"`
	Window    string `json:"window"`
	Lesson    string `json:"lesson"`
	Signal    Signal `json:"signal"`
	Installed string `json:"installed,omitempty"`
}

func buildReport(raw []byte) (BuildReport, error) {
	var report BuildReport
	if len(raw) > runFileMax {
		return report, errors.New("build report exceeds the report limit")
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
	for _, name := range []string{"summary", "test", "title", "body", "rollback", "signal", "window", "lesson"} {
		if value, ok := fields[name]; !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return report, errors.New("build report has a missing required field")
		}
	}
	for _, text := range []string{report.Summary, report.Test, report.Title, report.Body, report.Rollback, report.Window} {
		if strings.TrimSpace(text) == "" {
			return report, errors.New("build report has an empty required field")
		}
	}
	if len(report.Body) > 64<<10 {
		return report, errors.New("proposal body exceeds its limit")
	}
	if strings.ContainsAny(report.Lesson, "\r\n") || len(report.Lesson) > 300 {
		return report, errors.New("a lesson is one line of at most 300 characters")
	}
	return report, nil
}

func planWindow(text string) (time.Duration, error) {
	if strings.HasSuffix(text, "d") {
		n, err := time.ParseDuration(strings.TrimSuffix(text, "d") + "h")
		if err != nil || n > time.Duration(1<<63-1)/24 {
			return 0, errors.New("invalid measurement window")
		}
		return n * 24, nil
	}
	return time.ParseDuration(text)
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

func (r *Runner) waitingRepo(ctx context.Context, c store.Case) error {
	id := "loop-repo:" + store.IDPart(c.ID)
	notes, err := r.Store.Notes(ctx)
	if err != nil {
		return err
	}
	for _, note := range notes {
		if note.ID == id {
			return nil
		}
	}
	_, err = r.Store.PutNote(ctx, store.Note{ID: id, Kind: store.NoteProblem, Importance: store.ImportanceLow,
		Title:  "The loop's case " + c.ID + " waits for a repository",
		Detail: "The investigation named no code repository. The case stays in testing without a build run.", At: r.now()})
	return err
}

func (r *Runner) buildPackage(ctx context.Context, c store.Case, run store.LoopRun) (Package, error) {
	if r.Ship == nil {
		return Package{}, errors.New("build needs the host ship tool")
	}
	name, err := r.caseRepo(ctx, c)
	if err != nil {
		return Package{}, err
	}
	var source string
	for _, repo := range r.pass.manifest.Repositories {
		if repo.Name == name && repo.Error == "" {
			source = repo.Source
		}
	}
	if name == "" || source == "" {
		return Package{}, refused("the investigation's repository is not an admitted read_dirs repository")
	}
	p, had, err := r.Ship.Load(c.ID)
	if err != nil {
		return p, err
	}
	if !had || p.State == RolledBack || p.State == Kept {
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
	if p.RefreshBase {
		p, err = r.Ship.refreshBaseLocked(r.Ship.gitContext(shipContext(ctx, p.Case)), p)
	} else if _, err = r.Ship.Gate.Repository(ctx, p.Dir); err == nil {
		err = r.Ship.freshRunClone(shipContext(ctx, p.Case), p)
	}
	if err != nil {
		return p, err
	}
	// A run reads a repository as a masked snapshot, but a build gets this independent clone with its history, which no
	// scrubber has seen, and it may hold commits the source does not (a fetch of the remote brought them). Only a clone
	// whose objects the scrubber would not change is mounted.
	if err = checkCloneObjects(ctx, p.Work); err != nil {
		removeInputTree(p.Work) // unscanned, it stays out of storage a later step might read
		if errors.Is(err, errNeedsScrubbing) {
			return p, refused("the repository %s holds history the scrubber would mask, so a build clone of it cannot be mounted", name)
		}
		return p, err
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
	if _, err := r.Ship.Check(ctx, c.ID, report.Test, nil, snapshot); err != nil {
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
	if !regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`).MatchString(p.BaseCommit) {
		return sandbox.Spec{}, errors.New("patch base is not a commit id")
	}
	env := map[string]string{"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_ATTR_SOURCE": "4b825dc642cb6eb9a060e54bf8d69288fbee4904", "GIT_ATTR_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0"}
	pins := strings.Join(GitSafety(), " ")
	script := "git -C /work " + pins + " add -A && git -C /work " + pins +
		" diff --cached --binary --no-ext-diff --no-textconv --no-renames --src-prefix=a/ --dst-prefix=b/ " +
		p.BaseCommit + " -- > /out/change.patch"
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
