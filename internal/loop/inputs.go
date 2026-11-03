package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
}

type passInputs struct {
	dir       string
	manifest  InputsManifest
	binds     []sandbox.Bind
	err       error
	attempted bool
}

func (p *passInputs) close() {
	if p != nil && p.dir != "" {
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
	until, window := r.now(), r.Inputs.Window
	if window == 0 {
		window = StoreWindow
	}
	if window != StoreWindow {
		return fmt.Errorf("investigations read the last 30 days")
	}
	dst := filepath.Join(dir, "store.sqlite")
	p.manifest.Store, err = StoreCopy(ctx, StoreCopyOptions{Source: r.Store.Path(), Destination: dst, ScratchDir: dir, Since: until.Add(-window), Until: until})
	if err != nil {
		return err
	}
	p.binds = append(p.binds, sandbox.Bind{Src: dst, Dst: "/ro/store.sqlite"})
	dst = filepath.Join(dir, "config")
	p.manifest.Configuration, err = ConfigCopy(ctx, ConfigCopyOptions{Home: r.Inputs.Home, Destination: dst})
	if err != nil {
		return err
	}
	p.binds = append(p.binds, sandbox.Bind{Src: dst, Dst: "/ro/config"})
	dst = filepath.Join(dir, "clone")
	p.manifest.Tacit, err = CloneTacit(ctx, r.Inputs.TacitStore, dst)
	switch {
	case errors.Is(err, ErrNoScrubber):
		return err
	case err != nil:
		// tacit's facts are context, not a precondition: without them the run goes on, and the manifest says "tacit absent"
		p.manifest.Tacit.Error = err.Error()
	default:
		p.binds = append(p.binds, sandbox.Bind{Src: dst, Dst: "/ro/clone"})
	}
	dst = filepath.Join(dir, "repos")
	if err = os.Mkdir(dst, 0700); err != nil {
		return err
	}
	p.manifest.Repositories, err = CloneRepos(ctx, RepoCloneOptions{Sources: r.Inputs.ReadDirs, Destination: dst, LoopDirs: []string{r.Dir}})
	if err != nil {
		return err
	}
	// A repository that cannot be cloned (missing, not a work tree, a secret in its history the scrubber refuses) is
	// left out and counted in the manifest's summary; the other repositories and the investigation go on.
	p.binds = append(p.binds, sandbox.Bind{Src: dst, Dst: "/ro/repos"})
	return nil
}

// prepareRun creates the case and records before charging a reservation. These
// files are host-owned. A failed preparation returns a skipped step.
func (r *Runner) prepareRun(ctx context.Context, c store.Case, family, step string) (run store.LoopRun, err error) {
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
	system, err := investigationSystem(r.Rules, r.Constraints)
	if err != nil {
		return run, err
	}
	evidenceText, err := caseEvidence(ctx, r.Store, c)
	if err != nil {
		return run, err
	}
	prompt := fmt.Sprintf("%s\nProblem: %s\n%s%s%s%s", liveQuestion, plain.Line(c.Title), evidenceText, caseHolds(c), findingsInstruction, inputReference())
	clean, err := RedactAll(ctx, []string{string(raw), system, prompt})
	if err != nil {
		return run, err
	}
	if !json.Valid([]byte(clean[0])) {
		return run, fmt.Errorf("scrubbed case is not JSON")
	}
	for name, text := range map[string]string{"case.json": clean[0], "system.md": clean[1], "schema.json": investigationSchema} {
		if err = writeExclusive(input, name, []byte(text)); err != nil {
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
		Destination: filepath.Join(host, "records"), Binary: r.Inputs.Momentum, Home: home, Env: CleanEnv(nil, scrubberDir())})
	if err != nil {
		return run, err
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
	binds = append(binds, sandbox.Bind{Src: filepath.Join(HostDir(run.Dir), "records"), Dst: "/ro/records"})
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
