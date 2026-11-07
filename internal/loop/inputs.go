package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/sandbox"
	"github.com/agent-sh/momentum/internal/store"
)

// InputOptions names trusted host sources. The zero window uses the last 30 days.
// Home is the configuration home. StoreHome is the records child's MOMENTUM_HOME.
type InputOptions struct {
	Momentum, Home, StoreHome, TacitStore string
	ReadDirs                              []string
	Window                                time.Duration
	// ScrubCache is the host file of the persistent scrub cache (empty: none). It must lie outside the loop storage.
	ScrubCache string
}

type passInputs struct {
	dir       string
	manifest  InputsManifest
	binds     []sandbox.Bind
	err       error
	attempted bool
	momentum  string
	cache     *ScrubCache
	since     time.Time
}

func (p *passInputs) close() {
	if p == nil {
		return
	}
	p.cache.Close(context.Background(), time.Now())
	p.cache = nil
	if p.dir != "" {
		removeInputTree(p.dir)
	}
}

// prepareInputs is lazy and runs once per pass, before any reservation.
func (r *Runner) prepareInputs(ctx context.Context) error {
	p := r.pass
	if p == nil {
		return fmt.Errorf("investigation needs a loop pass")
	}
	if p.attempted {
		return p.err
	}
	p.attempted = true
	p.err = r.buildInputs(ctx, p)
	return p.err
}

func (r *Runner) buildInputs(ctx context.Context, p *passInputs) error {
	if err := CheckScrubber(ctx); err != nil {
		return err
	}
	if r.Inputs.Home == "" || r.Inputs.Momentum == "" || r.Inputs.TacitStore == "" {
		return fmt.Errorf("inputs need a host binary, home and tacit store")
	}
	if err := hostDir(r.Dir); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(r.Dir, ".pass-")
	if err != nil {
		return err
	}
	p.dir = dir
	// Freeze the same program used by host records children and run readers.
	var hash string
	p.momentum, hash, err = copyRecordsProgram(r.Inputs.Momentum, dir, "momentum")
	if err != nil {
		return err
	}
	p.manifest.Programs = map[string]string{"/opt/momentum/momentum-ro": hash}
	p.binds = append(p.binds, sandbox.Bind{Src: p.momentum, Dst: "/opt/momentum/momentum-ro"})
	until, window := r.now(), r.Inputs.Window
	if window == 0 {
		window = StoreWindow
	}
	if window != StoreWindow {
		return fmt.Errorf("investigations read the last 30 days")
	}
	// The persistent scrub cache is host state outside every run: a cache that cannot be opened is not a reason to
	// stop (the pass scrubs without it); a scrubber that does not run is.
	cacheErr := ""
	if r.Inputs.ScrubCache != "" {
		if inside(r.Dir, r.Inputs.ScrubCache) || inside(dir, r.Inputs.ScrubCache) {
			return fmt.Errorf("the scrub cache must lie outside the loop storage")
		}
		p.cache, err = OpenScrubCache(r.Inputs.ScrubCache)
		if errors.Is(err, ErrNoScrubber) {
			return err
		}
		if err != nil {
			cacheErr = reason(err)
		}
	}
	p.since = until.Add(-HistoryWindow)
	logs := filepath.Join(dir, "history")
	if err = os.Mkdir(logs, 0o700); err != nil {
		return err
	}
	// The store copy, the configuration copy and the snapshots read different sources into different directories, so
	// they run at the same time: the pass waits for the longest, not for their sum. The first that fails stops the
	// others, and its error is the pass's.
	storeDst, configDst, tacitDst, reposDst := filepath.Join(dir, "store.sqlite"), filepath.Join(dir, "config"), filepath.Join(dir, "clone"), filepath.Join(dir, "repos")
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		cause   error
		tacitOK bool
	)
	fail := func(err error) {
		mu.Lock()
		if cause == nil {
			cause = err
		}
		mu.Unlock()
		stop()
	}
	steps := []func(){
		func() {
			m, err := StoreCopy(sctx, StoreCopyOptions{Source: r.Store.Path(), Destination: storeDst, ScratchDir: dir, Since: until.Add(-window), Until: until, Cache: p.cache})
			p.manifest.Store = m
			if err != nil {
				fail(err)
			}
		},
		func() {
			m, err := ConfigCopy(sctx, ConfigCopyOptions{Home: r.Inputs.Home, Destination: configDst})
			p.manifest.Configuration = m
			if err != nil {
				fail(err)
			}
		},
		func() {
			m, err := SnapshotFacts(sctx, r.Inputs.TacitStore, tacitDst, filepath.Join(logs, "facts.log"), p.cache, p.since)
			p.manifest.Tacit = m
			switch {
			case errors.Is(err, ErrNoScrubber):
				fail(err)
			case sctx.Err() != nil:
				fail(sctx.Err()) // stopped: the step that failed, or the pass, gives the reason
			case err != nil:
				// tacit's facts are context, not a precondition: without them the run goes on, and the manifest says why
				p.manifest.Tacit.Error = reason(err)
			default:
				tacitOK = true
			}
		},
		func() {
			// A repository that cannot be read (missing, not a work tree, no default branch) is left out and counted in the
			// manifest's summary; the others and the investigation go on. What the scrubber changes in one is masked.
			// Each is read at its origin's default branch, fetched into a mirror of the loop's own, never into the checkout.
			gh := ""
			if r.Ship != nil {
				gh = r.Ship.GH
			}
			m, err := SnapshotRepos(sctx, SnapshotOptions{Sources: r.Inputs.ReadDirs, Destination: reposDst, LogDir: filepath.Join(logs, "repos"),
				LoopDirs: []string{r.Dir}, Mirrors: filepath.Join(r.Dir, "mirrors"), GH: gh, Cache: p.cache, Since: p.since, Logf: r.logf})
			p.manifest.Repositories = m
			if err != nil {
				fail(err)
			}
		},
	}
	took := make([]time.Duration, len(steps))
	start := time.Now()
	for i, step := range steps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			begin := time.Now()
			step()
			took[i] = time.Since(begin)
		}()
	}
	wg.Wait()
	r.logf("pass inputs: %s (store %s, configuration %s, tacit %s, repositories %s)", roundSecond(time.Since(start)), roundSecond(took[0]),
		roundSecond(took[1]), roundSecond(took[2]), roundSecond(took[3]))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if cause != nil {
		return cause
	}
	p.manifest.Store.CacheError = cacheErr
	p.binds = append(p.binds, sandbox.Bind{Src: storeDst, Dst: "/ro/store.sqlite"}, sandbox.Bind{Src: configDst, Dst: "/ro/config"})
	if tacitOK {
		p.binds = append(p.binds, sandbox.Bind{Src: tacitDst, Dst: "/ro/clone"})
	}
	p.binds = append(p.binds, sandbox.Bind{Src: reposDst, Dst: "/ro/repos"}, sandbox.Bind{Src: logs, Dst: "/ro/history"})
	if f := p.cache.Failure(); f != "" && p.manifest.Store.CacheError == "" {
		p.manifest.Store.CacheError = f // the cache failed during the pass: the rows after it were scrubbed without it
	}
	return nil
}

func roundSecond(d time.Duration) time.Duration { return d.Round(time.Second) }

// inside reports whether path lies in dir or is it.
func inside(dir, path string) bool {
	a, err := filepath.Abs(dir)
	if err != nil {
		return true
	}
	b, err := filepath.Abs(path)
	if err != nil {
		return true
	}
	rel, err := filepath.Rel(a, b)
	return err != nil || rel == "." || filepath.IsLocal(rel)
}

// prepareRun creates the case and records before charging a reservation. These
// files are host-owned. A failed preparation returns a skipped step.
func (r *Runner) prepareRun(ctx context.Context, c store.Case, family, step string) (run store.LoopRun, err error) {
	defer func() {
		if err != nil && step == StepBuild {
			var staged *buildStageError
			if !errors.As(err, &staged) {
				err = &buildStageError{key: "build inputs", err: err}
			}
		}
	}()
	run = store.LoopRun{ID: newRunID(r.now()), Case: c.ID, Family: family, Step: step, Model: r.Agent.Model, Started: r.now(), CaseEpisode: c.Episode}
	run.Dir = filepath.Join(r.Dir, "runs", run.ID)
	host := HostDir(run.Dir)
	if err = hostDir(host); err != nil {
		return run, err
	}
	complete := false
	defer func() {
		if !complete {
			removeInputTree(host)
		}
	}()
	input := filepath.Join(host, "input")
	if err = hostDir(input); err != nil {
		return run, err
	}
	evidence, err := r.Store.CaseEvidence(ctx, c.ID, 0)
	if err != nil {
		return run, err
	}
	log, err := r.Store.CaseLog(ctx, c.ID)
	if err != nil {
		return run, err
	}
	raw, err := json.Marshal(struct {
		Case     store.Case        `json:"case"`
		Evidence []store.Evidence  `json:"evidence"`
		Log      []store.CaseEntry `json:"log"`
	}{c, evidence, log})
	if err != nil {
		return run, err
	}
	system, err := stepSystem(step, r.Rules, r.Constraints)
	if err != nil {
		return run, err
	}
	evidenceText, err := caseEvidence(ctx, r.Store, c)
	if err != nil {
		return run, err
	}
	prompt := fmt.Sprintf("%s\nProblem: %s\n%s%s%s%s", liveQuestion, plain.Line(c.Title), evidenceText, caseHolds(c), findingsInstruction, inputReference(true))
	schema := investigationSchema
	extra := map[string]string{}
	if step == StepBuild {
		p, prepErr := r.buildPackage(ctx, c, run)
		if prepErr != nil {
			return run, &buildStageError{key: "build prepare", err: prepErr}
		}
		schema = buildSchema
		history := ""
		for _, repo := range r.pass.manifest.Repositories {
			if repo.Error == "" && repo.Source == p.Dir && repo.Commit != p.BaseCommit {
				// the branch no longer holds the snapshot's commit, or the pass could not fetch it
				history = fmt.Sprintf(" The snapshot in /ro/repos and the history in /ro/history and /ro/diffs stop at commit %s, not at BASE's commit %s: what differs between the two is not in them.", repo.Commit, p.BaseCommit)
			}
		}
		prompt = fmt.Sprintf("Build case %s: %s. BASE is %s. /work is your build tree at BASE, writable with its Git metadata: one commit that holds the tree of the default branch's commit %s, without its history (that is in /ro/history and /ro/diffs).%s /out is writable. Read /run/input/case.json for the investigation and evidence. Make one cause's change and its discriminating test. End with the build report. The host produces the patch.\n%s\n%s", c.ID, c.Title, p.WorkBase, p.BaseCommit, history, buildScope(p.Repo), inputReference(true))
		prompt = strings.ReplaceAll(prompt, "/work is empty scratch and the working directory.", "/work is the build tree at BASE and the working directory.")
		prompt = strings.ReplaceAll(prompt, "Return a report with mechanism, alternatives ruled out, fix and summary. Build no change.", "Return summary, test_command, title, body, rollback, signal, window and lesson. test_command is the program and its arguments, such as [\"go\", \"test\", \"./...\"], which the host runs in the sandbox. window is a duration, such as 7d. signal names source, kinds, match and optional tool and harness.")
		if n := len(p.Reviews); n > 0 {
			extra["review.md"] = p.Reviews[n-1].Text
		}
		refusal, e := r.lastBuildRefusal(ctx, c)
		if e != nil {
			return run, e
		}
		if refusal != "" {
			extra["refusal.md"] = refusal
		}
		if f, e := OpenFile(r.Ship.pkgDir(c.ID), "change.patch"); e == nil {
			b, e := ReadFile(f, PatchMax)
			f.Close()
			if e != nil {
				return run, e
			}
			extra["previous.patch"] = string(b)
		} else if !os.IsNotExist(e) {
			return run, e
		}

	} else if step == StepReview {
		p, ok, e := r.Ship.Load(c.ID)
		if e != nil || !ok {
			return run, firstError(e, "review package is absent")
		}
		schema = reviewSchema
		prompt = fmt.Sprintf("Review case %s (%s): %s. Read /run/input/case.json, /run/input/change.patch and /run/input/package.json. The proposed tree is read-only at /ro/change with no .git. /ro holds the shared pass inputs. End with a nonempty summary, findings and reviewed_files naming every changed file once.", c.ID, c.Family, p.Title)
		diff, e := r.reviewDiff(ctx, p)
		if e != nil {
			return run, e
		}
		extra["change.patch"] = diff
		b, e := json.Marshal(freshReviewPackage(p))
		if e != nil {
			return run, e
		}
		extra["package.json"] = string(b)
	}
	// every run, investigation, build or review, is told which of its repositories are not origin's branch fetched now
	prompt += r.pass.manifest.Stale()
	clean, err := RedactAll(ctx, []string{string(raw), system, prompt})
	if err != nil {
		return run, err
	}
	if !json.Valid([]byte(clean[0])) {
		return run, fmt.Errorf("scrubbed case is not JSON")
	}
	for name, text := range map[string]string{"case.json": clean[0], "system.md": clean[1], "schema.json": schema} {
		if err = writeExclusive(input, name, []byte(text)); err != nil {
			return run, err
		}
	}
	for name, text := range extra {
		cleaned, e := Redact(ctx, text)
		if e != nil {
			return run, e
		}
		if name == "package.json" && !json.Valid([]byte(cleaned)) {
			return run, fmt.Errorf("scrubbed package is not JSON")
		}
		if err = writeExclusive(input, name, []byte(cleaned)); err != nil {
			return run, err
		}
	}
	promptJSON, err := json.Marshal(clean[2])
	if err != nil {
		return run, err
	}
	if err = writeExclusive(host, "prompt.json", promptJSON); err != nil {
		return run, err
	}
	var ids, texts []string
	texts = append(texts, c.Title, c.Mechanism, c.NextTest)
	for _, ev := range evidence {
		if id, ok := strings.CutPrefix(ev.Ref, "ev:"); ok {
			ids = append(ids, id)
		}
		texts = append(texts, ev.Summary)
	}
	for _, entry := range log {
		texts = append(texts, entry.Detail)
	}
	home := r.Inputs.StoreHome
	if home == "" {
		home = filepath.Dir(r.Store.Path())
	}
	manifest := r.pass.manifest
	manifest.Records, err = Records(ctx, RecordsOptions{Store: filepath.Join(r.pass.dir, "store.sqlite"), IDs: ids, Units: UnionUnits(FindUnits(texts...), ActiveUnits(ctx, r.Inputs.Home, r.now().Add(-ActiveUnitWindow), r.now())),
		Destination: filepath.Join(host, "records"), Binary: r.pass.momentum, Home: home, Env: CleanEnv(nil, scrubberDir())})
	if err != nil {
		return run, err
	}
	// The diffs of the commits that touched the paths this case's evidence names: host-read, scrubbed, text only.
	manifest.Diffs, err = SnapshotDiffs(ctx, manifest.Repositories, texts, filepath.Join(host, "diffs"), r.pass.since, r.pass.cache)
	if errors.Is(err, ErrNoScrubber) {
		return run, err
	}
	if err != nil {
		manifest.Diffs.Error = reason(err)
	}
	if f := r.pass.cache.Failure(); f != "" && manifest.Store.CacheError == "" {
		manifest.Store.CacheError = f
	}
	raw, err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return run, err
	}
	if err = writeExclusive(host, "manifest.json", raw); err != nil {
		return run, err
	}
	run.Summary = manifest.Summary()
	complete = true
	return run, nil
}

func (r *Runner) runInputs(run store.LoopRun) ([]sandbox.Bind, InputsManifest, error) {
	if r.pass == nil || r.pass.err != nil {
		return nil, InputsManifest{}, fmt.Errorf("pass inputs are unavailable")
	}
	f, err := OpenFile(HostDir(run.Dir), "manifest.json")
	if err != nil {
		return nil, InputsManifest{}, err
	}
	defer f.Close()
	raw, err := ReadFile(f, MaxStream)
	if err != nil {
		return nil, InputsManifest{}, err
	}
	var manifest InputsManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, manifest, err
	}
	binds := append([]sandbox.Bind(nil), r.pass.binds...)
	binds = append(binds, sandbox.Bind{Src: filepath.Join(HostDir(run.Dir), "records"), Dst: "/ro/records"},
		sandbox.Bind{Src: filepath.Join(HostDir(run.Dir), "diffs"), Dst: "/ro/diffs"})
	return binds, manifest, nil
}

// sweepPasses removes the input directories of passes that never finished (a killed process leaves its own): the loop
// runs one pass at a time, so any that exists when a pass starts is stale.
func (r *Runner) sweepPasses() {
	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), ".pass-") {
			removeInputTree(filepath.Join(r.Dir, e.Name()))
		}
	}
}
