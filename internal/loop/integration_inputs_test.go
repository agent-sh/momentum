package loop

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/sandbox"
	"github.com/agent-sh/momentum/internal/store"
)

// fakeManager tests the pass's startup ordering without running a model or unit.
// The real end-to-end test below uses the maintained launcher instead.
func fakeManager(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script(t, dir, "systemd-run", `printf 'probe\n' >> '`+log+`'`)
	script(t, dir, "systemctl", `case "$2" in
 list-units) printf 'list\n' >> '`+log+`'; printf '`+UnitPrefix+`old.service loaded failed failed fixture\n';;
 stop) printf 'stop %s\n' "$3" >> '`+log+`';;
 show) printf 'not-found\n';;
 *) exit 7;;
esac`)
	script(t, dir, "bwrap", "exit 7")
	script(t, dir, "socat", "exit 7")
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return log
}

func inputRunner(t *testing.T) (*Runner, store.Case) {
	t.Helper()
	goodInputScrubber(t)
	st := testStore(t)
	r := localRunner(t, st)
	r.Inputs = InputOptions{Home: t.TempDir(), TacitStore: inputRepo(t, t.TempDir(), "facts"), Momentum: script(t, t.TempDir(), "records", "printf 'marker-7f3a record\\n'")}
	c := openCase(t, st, "failures", "marker-7f3a", 1)
	return r, c
}

func TestPassStopsLeftoversBeforePreparingInputs(t *testing.T) {
	log := fakeManager(t)
	r, c := inputRunner(t)
	// Fail the input canary. There must be no reservation or agent launch.
	Scrubber = []string{"cat"}
	done, err := r.Once(context.Background(), Options{})
	if err != nil || len(done) != 1 || !strings.Contains(done[0].Skipped, "canary") {
		t.Fatalf("pass: %+v %v", done, err)
	}
	calls, err := os.ReadFile(log)
	if err != nil || string(calls) != "probe\nlist\nstop "+UnitPrefix+"old.service\n" {
		t.Fatalf("startup order or scope: %q %v", calls, err)
	}
	runs, err := r.Store.LoopRuns(context.Background(), store.RunFilter{})
	got, _ := r.Store.CaseByID(context.Background(), c.ID)
	if err != nil || len(runs) != 0 || got.Cost.Runs != 0 || got.Cost.USD != 0 {
		t.Fatalf("failed inputs charged a case: %+v %+v %v", runs, got.Cost, err)
	}
}

func TestInputFailuresStartNoRun(t *testing.T) {
	for _, kind := range []string{"missing", "failed", "canary"} {
		t.Run(kind, func(t *testing.T) {
			fakeManager(t)
			r, c := inputRunner(t)
			switch kind {
			case "missing":
				Scrubber = []string{filepath.Join(t.TempDir(), "absent")}
			case "failed":
				Scrubber = []string{"false"}
			case "canary":
				Scrubber = []string{"cat"}
			}
			done, err := r.Once(context.Background(), Options{})
			if err != nil || len(done) != 1 || done[0].Run.ID != "" || !strings.Contains(done[0].Skipped, "inputs:") {
				t.Fatalf("failure was not skipped: %+v %v", done, err)
			}
			runs, err := r.Store.LoopRuns(context.Background(), store.RunFilter{})
			after, _ := r.Store.CaseByID(context.Background(), c.ID)
			if err != nil || len(runs) != 0 || after.Cost.Runs != 0 || after.Cost.USD != 0 {
				t.Fatalf("failure left accounting: %+v %+v %v", runs, after.Cost, err)
			}
		})
	}
}

func TestDryPassBuildsNoInputs(t *testing.T) {
	r, c := inputRunner(t)
	// Even discovery of a sandbox or scrubber would fail. Selection must remain pure.
	t.Setenv("PATH", t.TempDir())
	Scrubber = []string{"absent"}
	done, err := r.Once(context.Background(), Options{Dry: true})
	if err != nil || len(done) != 1 || done[0].Case != c.ID || !strings.HasPrefix(done[0].Skipped, "dry:") {
		t.Fatalf("dry pass: %+v %v", done, err)
	}
	if r.pass != nil {
		t.Fatal("dry pass made input state")
	}
	entries, err := os.ReadDir(r.Dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("dry pass wrote files: %v %v", entries, err)
	}
	runs, err := r.Store.LoopRuns(context.Background(), store.RunFilter{})
	if err != nil || len(runs) != 0 {
		t.Fatalf("dry pass reserved a run: %v %v", runs, err)
	}
}

func TestPassInputsAreSharedAndRemoved(t *testing.T) {
	r, _ := inputRunner(t)
	ctx := context.Background()
	r.pass = &passInputs{}
	if err := r.prepareInputs(ctx); err != nil {
		t.Fatal(err)
	}
	p := r.pass
	first := p.manifest.Store
	// A second preparation must not rebuild the snapshot, even if the store changed.
	_, err := r.Store.Put(ctx, []store.Event{{Source: "fixture", SourceKey: "later", TS: time.Now().Add(-time.Minute), Actor: store.ActorOwner, Kind: "owner_text", Summary: "marker-7f3a"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.prepareInputs(ctx); err != nil {
		t.Fatal(err)
	}
	if r.pass != p || p.manifest.Store != first {
		t.Fatal("pass rebuilt its shared inputs")
	}
	p.close()
	if _, err := os.Lstat(p.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pass inputs remain: %v", err)
	}
}

func TestPassRemovesInputsAndKeepsManifestSummary(t *testing.T) {
	fakeManager(t)
	r, c := inputRunner(t)
	// The agent cannot start. Its prepared manifest must still explain this run.
	done, err := r.Once(context.Background(), Options{})
	if err != nil || len(done) != 1 || done[0].Run.ID == "" {
		t.Fatalf("pass: %+v %v", done, err)
	}
	run := done[0].Run
	raw, err := os.ReadFile(filepath.Join(HostDir(run.Dir), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest InputsManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(run.Summary, manifest.Summary()) {
		t.Fatalf("manifest missing from summary: %s", run.Summary)
	}
	if r.pass != nil {
		t.Fatal("pass retained input state")
	}
	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pass-") {
			t.Fatal("pass inputs were not removed")
		}
	}
	got, _ := r.Store.CaseByID(context.Background(), c.ID)
	if got.Cost.USD != 0 || !run.Started.IsZero() {
		t.Fatalf("never-started agent was charged: %+v %+v", got.Cost, run)
	}
}

func TestInvestigationOnSandboxReadsOnlyPreparedInputs(t *testing.T) {
	if err := sandbox.Available(true); err != nil {
		t.Skip(err)
	}
	r, c := inputRunner(t)
	r.Dir = shortScratch(t)
	repo := inputRepo(t, t.TempDir(), "example")
	r.Inputs.ReadDirs = []string{repo}
	write(t, filepath.Join(r.Inputs.Home, ".config", "agents", "rules.md"), "marker-7f3a rule")
	body := `python3 - <<'PYCHECK'
import sqlite3, pathlib, os
for name in ['/ro/store.sqlite','/ro/records/index.txt','/ro/config/MANIFEST.txt','/ro/clone','/ro/repos/example','/run/input/case.json','/run/input/system.md','/run/input/schema.json']:
    assert pathlib.Path(name).exists(), name
for name in ['/ro/store.sqlite','/ro/records/index.txt','/ro/config/MANIFEST.txt','/ro/clone/file','/ro/repos/example/file']:
    try:
        with open(name,'a') as output: output.write('changed')
    except OSError: pass
    else: raise AssertionError('writable input')
connection=sqlite3.connect('file:/ro/store.sqlite?mode=ro',uri=True)
try: connection.execute("INSERT INTO events(id) VALUES ('changed')")
except sqlite3.OperationalError: pass
else: raise AssertionError('writable store')
assert os.getcwd() == '/work'
assert not list(pathlib.Path('/work').iterdir())
assert not pathlib.Path('/usr/bin/momentum').exists()
print('\n'.join(sorted(os.environ.keys())))
PYCHECK
` + fixtureResult(investigatedResult)
	harness := script(t, t.TempDir(), "harness", body)
	r.Agent = testSandboxAgent(t, Agent{Runner: []string{harness}, Model: "global.anthropic.claude-opus-5-5"})
	done, err := r.Once(context.Background(), Options{})
	if err != nil || len(done) != 1 || done[0].Run.Status != store.RunOK {
		t.Fatalf("investigation: %+v %v", done, err)
	}
	run := done[0].Run
	if !strings.Contains(run.Summary, "store copy 30 days") || !strings.Contains(run.Summary, "1 repositories") {
		t.Fatalf("manifest summary absent: %s", run.Summary)
	}
	got, _ := r.Store.CaseByID(context.Background(), c.ID)
	if got.Status != store.CaseTesting || got.NextTest != "take the lock first" {
		t.Fatalf("findings not recorded: %+v", got)
	}
	if RoutePort != sandbox.RelayPort || sandbox.RelayPort != 8787 || sandbox.DefaultMemory != "4G" || sandbox.DefaultCPUQuota != "400%" || sandbox.DefaultTasks != 512 || sandbox.DefaultRuntimeSec != 1800 {
		t.Fatal("sandbox constants changed")
	}
}

func TestFindingsFileRefusesLinksPipesAndOversize(t *testing.T) {
	for _, kind := range []string{"link", "parent-link", "pipe", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "out", "findings.md")
			if kind == "oversize" {
				write(t, path, "## Mechanism\n"+strings.Repeat("x", runFileMax)+"\n## Fix\nfix\n")
			} else if kind == "pipe" {
				os.MkdirAll(filepath.Dir(path), 0700)
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				target := filepath.Join(t.TempDir(), "findings.md")
				write(t, target, "## Mechanism\nmarker-7f3a\n## Fix\nfix\n")
				if kind == "parent-link" {
					os.Symlink(filepath.Dir(target), filepath.Join(dir, "out"))
				} else {
					os.MkdirAll(filepath.Dir(path), 0700)
					os.Symlink(target, path)
				}
			}
			if _, _, ok := investigationFindings(Result{}, dir); ok {
				t.Fatal("accepted unsafe findings file")
			}
		})
	}
}

func TestSandboxRunUnitsAndConstants(t *testing.T) {
	if got := runUnit("CASE_7.X", "", "primary"); got != UnitPrefix+"case-7-x" {
		t.Fatalf("unit: %s", got)
	}
	if got := runUnit("CASE_7.X", "", "fallback"); got != UnitPrefix+"case-7-x-fallback" {
		t.Fatalf("fallback unit: %s", got)
	}
	if RoutePort != sandbox.RelayPort {
		t.Fatal("relay port is not the sandbox port")
	}
	if sandbox.RelayPort != 8787 || MaxStream != 8<<20 {
		t.Fatal("loop launcher constants changed")
	}
}

func TestTestingWithoutShipAndImprovedCasesReserveNoAgent(t *testing.T) {
	r, c := inputRunner(t)
	mechanism, fix := "marker-7f3a mechanism", "fix"
	if _, err := r.Store.UpdateCase(context.Background(), c.ID, store.CaseUpdate{Status: store.CaseTesting, Mechanism: &mechanism, NextTest: &fix}, "fixture"); err != nil {
		t.Fatal(err)
	}
	picked, _, why, err := r.pick(context.Background(), c.Family, c.ID, true)
	if err != nil || picked.ID != "" || !strings.Contains(why, "has no ship tool") {
		t.Fatalf("testing selection: %+v %s %v", picked, why, err)
	}
	// Seed a retained improved state as a synthetic fixture.
	writer, err := sql.Open("sqlite", r.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err = writer.Exec("UPDATE cases SET status='closed', reason='improved' WHERE id=?", c.ID); err != nil {
		t.Fatal(err)
	}
	picked, _, why, err = r.pick(context.Background(), c.Family, c.ID, true)
	if err != nil || picked.ID != "" {
		t.Fatalf("improved selection: %+v %s %v", picked, why, err)
	}
}

// A repository the inputs cannot clone is left out and counted in the manifest; the pass and the other inputs go on.
func TestARefusedRepositoryIsLeftOutAndNoted(t *testing.T) {
	r, _ := inputRunner(t)
	r.pass = &passInputs{}
	t.Cleanup(r.pass.close)
	r.Inputs.ReadDirs = []string{filepath.Join(t.TempDir(), "missing")}
	if err := r.prepareInputs(context.Background()); err != nil {
		t.Fatalf("a missing repository stopped the inputs: %v", err)
	}
	m := r.pass.manifest
	if len(m.Repositories) != 1 || m.Repositories[0].Error == "" || !strings.Contains(m.Summary(), "(1 refused)") {
		t.Fatalf("the refusal is not in the manifest: %+v %s", m.Repositories, m.Summary())
	}
}

func TestReservationKeepsManifestBeforeTheAgentStarts(t *testing.T) {
	r, c := inputRunner(t)
	ctx := context.Background()
	r.pass = &passInputs{}
	defer r.pass.close()
	if err := r.prepareInputs(ctx); err != nil {
		t.Fatal(err)
	}
	prepared, err := r.prepareRun(ctx, c, c.Family, StepInvestigate)
	if err != nil {
		t.Fatal(err)
	}
	run, why := r.reservePrepared(ctx, prepared)
	if why != "" {
		t.Fatal(why)
	}
	stored, err := r.Store.LoopRunByID(ctx, run.ID)
	if err != nil || !strings.Contains(stored.Summary, "store copy 30 days") || stored.Status != store.RunRunning {
		t.Fatalf("reservation has no manifest: %+v %v", stored, err)
	}
	// If the loop dies before finish, stale recovery retains the same line.
	r.Now = func() time.Time { return run.Started.Add(r.Budgets.RunTime + time.Minute) }
	if err := r.endStale(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err = r.Store.LoopRunByID(ctx, run.ID)
	if err != nil || !strings.Contains(stored.Summary, "store copy 30 days") {
		t.Fatalf("stale recovery lost the manifest: %+v %v", stored, err)
	}
}

func TestPreparedInputsKeepTheirOriginalCaseEpisode(t *testing.T) {
	r, c := inputRunner(t)
	ctx := context.Background()
	r.pass = &passInputs{}
	defer r.pass.close()
	if err := r.prepareInputs(ctx); err != nil {
		t.Fatal(err)
	}
	prepared, err := r.prepareRun(ctx, c, c.Family, StepInvestigate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.CloseCase(ctx, c.ID, store.NotWorthIt, "", "fixture", "fixture"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	evidence := []store.Evidence{{Ref: "file:/fixture/new", At: time.Now()}}
	if _, err := r.Store.ReopenCase(ctx, c.ID, evidence, "fixture", "fixture"); err != nil {
		t.Fatal(err)
	}
	run, why := r.reservePrepared(ctx, prepared)
	if why != "" {
		t.Fatal(why)
	}
	if run.CaseEpisode != c.Episode {
		t.Fatal("prepared brief adopted a different case episode")
	}
	r.finish(ctx, c, &run, Result{Started: true, Status: store.RunOK, CostKnown: true, Report: nativeReport(investigatedResult)}, nil)
	got, err := r.Store.CaseByID(ctx, c.ID)
	if err != nil || got.Mechanism != "" || got.Status != store.CaseOpen {
		t.Fatalf("old brief changed reopened case: %+v %v", got, err)
	}
}

// A pass told to stop (a signal ends the command's context) stops its runs' units, records the run as stopped, removes
// its inputs and starts no other run.
func TestAPassToldToStopStopsItsRunsAndCleansUp(t *testing.T) {
	if err := sandbox.Available(true); err != nil {
		t.Skip(err)
	}
	r, c := inputRunner(t)
	r.Dir = shortScratch(t)
	readyDir := t.TempDir()
	ready := filepath.Join(readyDir, "ready")
	harness := script(t, t.TempDir(), "harness", "echo started > /opt/ready/ready\nsleep 60\n")
	r.Agent = testSandboxAgent(t, Agent{Runner: []string{harness}, Model: "global.anthropic.claude-opus-5-5"})
	r.Agent.Binds = append(r.Agent.Binds, sandbox.Bind{Src: readyDir, Dst: "/opt/ready", Write: true})
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(ready); err == nil {
				stop()
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		stop()
	}()
	start := time.Now()
	done, err := r.Once(ctx, Options{})
	if err != nil || len(done) != 1 || done[0].Run.Failure != "stopped" || time.Since(start) > 40*time.Second {
		t.Fatalf("a stopped pass: %+v %v after %s", done, err, time.Since(start))
	}
	out, _ := exec.Command("systemctl", "--user", "list-units", "--all", "--no-legend", "--plain", UnitPrefix+"*.service").Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("a unit outlived its stopped pass: %s", out)
	}
	entries, _ := os.ReadDir(r.Dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pass-") {
			t.Fatal("the stopped pass kept its inputs")
		}
	}
	got, _ := r.Store.CaseByID(context.Background(), c.ID)
	if got.Cost.Runs != 1 {
		t.Fatalf("the stopped run was not recorded once: %+v", got.Cost)
	}
	// a second pass with the context already ended starts nothing
	done, err = r.Once(ctx, Options{})
	if err == nil || len(done) != 0 {
		t.Fatalf("a pass started on an ended context: %+v %v", done, err)
	}
}

// Input directories of a pass that never finished are swept when the next pass starts.
func TestAPassSweepsTheInputsOfAKilledPass(t *testing.T) {
	fakeManager(t)
	r, _ := inputRunner(t)
	stale := filepath.Join(r.Dir, ".pass-stale")
	if err := os.MkdirAll(filepath.Join(stale, "config", "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(stale, "config", "sub", "file"), "marker-7f3a")
	if err := os.Chmod(filepath.Join(stale, "config", "sub"), 0o555); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Once(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("the killed pass's inputs were kept")
	}
}

// What the harness prints about itself decides nothing: its turn count is not the run's (the relay's is), and a report
// it prints after the model's request was refused is not kept.
func TestAHarnessesOwnClaimsDecideNothing(t *testing.T) {
	if err := sandbox.Available(true); err != nil {
		t.Skip(err)
	}
	const claimed = `{"type":"result","subtype":"success","num_turns":91,"structured_output":{"mechanism":"a forged mechanism","alternatives":[],"fix":"a forged fix","summary":"forged"}}`
	for name, c := range map[string]struct {
		body   string
		status string
		turns  int
	}{
		"turns": {fixtureResult(`{"type":"result","subtype":"success","num_turns":2,"structured_output":{"mechanism":"m","alternatives":[],"fix":"f","summary":"s"}}`) +
			"\nprintf '%s\\n' '" + claimed + "'", store.RunFailed, 2},
		"refusal": {fixtureResult(`{"type":"result","subtype":"success","is_error":true,"num_turns":1,"result":"the safeguards blocked it"}`) +
			"\nprintf '%s\\n' '" + claimed + "'", store.RunFailed, 1},
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := inputRunner(t)
			r.Dir = shortScratch(t)
			harness := script(t, t.TempDir(), "harness", c.body)
			r.Agent = testSandboxAgent(t, Agent{Runner: []string{harness}, Model: "global.anthropic.claude-opus-5-5"})
			done, err := r.Once(context.Background(), Options{})
			if err != nil || len(done) != 1 {
				t.Fatalf("%+v %v", done, err)
			}
			run := done[0].Run
			if run.Turns != c.turns || run.Status != c.status {
				t.Fatalf("the run took the harness's word: turns %d (want %d), status %s (want %s), failure %q", run.Turns, c.turns, run.Status, c.status, run.Failure)
			}
			if name == "refusal" && run.Failure != "the model's safeguards" {
				t.Fatalf("a report after a refusal was kept: %q", run.Failure)
			}
		})
	}
}
