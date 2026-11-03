package loop

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/sandbox"
	"github.com/agent-sh/momentum/internal/store"
)

const roDestination = "/opt/momentum/momentum-ro"

func readerBind(t *testing.T, binds []sandbox.Bind, source, hash string, programs map[string]string) {
	t.Helper()
	count := 0
	for _, bind := range binds {
		if bind.Dst == roDestination {
			count++
			if bind.Src != source || bind.Write || hash == "" || programs[roDestination] != hash {
				t.Fatalf("reader bind or hash differs: %+v", bind)
			}
		}
	}
	if count != 1 {
		t.Fatal("run needs exactly one pinned read-only reader")
	}
}

func TestInputsReadOnlyProgramPinnedBoundAndRecorded(t *testing.T) {
	r, c := inputRunner(t)
	r.pass = &passInputs{}
	defer r.pass.close()
	before := readText(t, r.Inputs.Momentum)
	if err := r.prepareInputs(context.Background()); err != nil {
		t.Fatal(err)
	}
	write(t, r.Inputs.Momentum, "#!/bin/sh\nprintf 'changed source\\n'\n")
	run, err := r.prepareRun(context.Background(), c, c.Family, StepInvestigate)
	if err != nil {
		t.Fatal(err)
	}
	binds, manifest, err := r.runInputs(run)
	if err != nil {
		t.Fatal(err)
	}
	readerBind(t, binds, r.pass.momentum, digest(before), manifest.Programs)
	if readText(t, r.pass.momentum) != before {
		t.Fatal("replacing the source changed the pinned run reader")
	}
}

func TestEvaluationReadOnlyProgramSamePinBothArmsAndHashes(t *testing.T) {
	e, _, in := preparedEval(t)
	programs := e.provenance().Programs
	readerBind(t, in.binds, e.programs.momentum, programs["momentum"], in.manifest.Programs)
	if programs["momentum-ro"] != programs["momentum"] || in.hashes[roDestination] != programs["momentum"] {
		t.Fatal("records child, reader and published hash differ")
	}
}

func TestInvestigationPromptIncludesReadOnlyFormsAndPython(t *testing.T) {
	for _, text := range []string{"momentum-ro evidence events --session SESSION", "--since 3d, --kind tool_result", "momentum-ro evidence show ev:ID",
		"momentum-ro case show", "changes nothing", "python3's sqlite3 on /ro/store.sqlite for what these do not answer"} {
		if !strings.Contains(inputReference(), text) {
			t.Fatalf("shared reference omits %s", text)
		}
	}
}

func buildReadOnlyFixture(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "momentum")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/momentum")
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build run reader: %v\n%s", err, out)
	}
	return binary
}

func TestMomentumRORealSandboxReadsRunInputsAndRefusesWrites(t *testing.T) {
	requireEvalSandbox(t)
	r, c := inputRunner(t)
	r.Dir = shortScratch(t)
	r.Inputs.Momentum = buildReadOnlyFixture(t)
	bin := t.TempDir()
	scrump := script(t, bin, "scrump", "sed -e 's/AKIA[A-Z0-9]*EXAMPLE/[masked]/g' -e 's/password=marker-7f3a/password=[masked]/g'")
	Scrubber = []string{scrump, "--profile", "text"}
	body := `set -eu
test "$(command -v momentum-ro)" = /opt/momentum/momentum-ro
momentum-ro evidence events --since 30d > /out/events.txt
momentum-ro case show > /out/case.txt
momentum-ro case show --json > /out/case.json
if momentum-ro evidence ingest > /out/refused.out 2> /out/refused.err; then exit 7; else test "$?" = 2; fi
python3 - <<'PY'
import pathlib, json
assert 'tool_result' in pathlib.Path('/out/events.txt').read_text()
case=json.loads(pathlib.Path('/out/case.json').read_text())['case']
assert case['ID'] in pathlib.Path('/out/case.txt').read_text()
assert pathlib.Path('/out/refused.out').read_bytes() == b''
assert pathlib.Path('/out/refused.err').read_text().strip() == "momentum-ro reads the run's copies and changes nothing"
for path in ['/ro/store.sqlite-wal','/ro/store.sqlite-shm','/ro/store.sqlite-journal','/home/agent/.config/momentum','/home/agent/.local/share/momentum']:
    assert not pathlib.Path(path).exists()
PY
` + fixtureResult(investigatedResult)
	harness := script(t, t.TempDir(), "harness", body)
	r.Agent = testSandboxAgent(t, Agent{Runner: []string{harness}, Model: "global.anthropic.claude-opus-5-5"})
	if _, err := r.Store.Put(context.Background(), []store.Event{{Source: "fixture", SourceKey: "read", TS: time.Now().Add(-time.Minute),
		Actor: store.ActorTool, Kind: "tool_result", Summary: "marker-7f3a result"}}); err != nil {
		t.Fatal(err)
	}
	done, err := r.Once(context.Background(), Options{})
	if err != nil || len(done) != 1 || done[0].Run.Status != store.RunOK {
		t.Fatalf("read-only investigation: %+v %v", done, err)
	}
	got, err := r.Store.CaseByID(context.Background(), c.ID)
	if err != nil || got.Status != store.CaseTesting {
		t.Fatal("fake harness did not complete")
	}
}

func TestEvaluationMomentumRORealSandboxBothArmsAndNoJudgeReader(t *testing.T) {
	body := `set -eu
momentum-ro evidence events --since 2000-01-01 > /out/events.txt
momentum-ro case show > /out/case.txt
test -s /out/events.txt && test -s /out/case.txt
if momentum-ro evidence ingest; then exit 7; else test "$?" = 2; fi`
	judge := `test ! -e /opt/momentum/momentum-ro || exit 8
` + fixtureResult(evalGrade)
	e, task := sandboxEval(t, body, judge)
	e.Runner.Inputs.Momentum = buildReadOnlyFixture(t)
	bin := t.TempDir()
	scrump := script(t, bin, "scrump", "sed -e 's/AKIA[A-Z0-9]*EXAMPLE/[masked]/g' -e 's/password=marker-7f3a/password=[masked]/g'")
	Scrubber = []string{scrump, "--profile", "text"}
	trials, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trials) != 2 {
		t.Fatalf("read-only evaluation: %d %v", len(trials), err)
	}
	for _, tr := range trials {
		if tr.Err != "" || tr.Run.Status != store.RunOK || tr.Grade.Verdict != "reached" {
			t.Fatalf("read-only trial: %+v", tr)
		}
	}
}
