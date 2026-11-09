package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

// relayPost defines post, which sends one model request to the run's relay. The sandbox starts the harness only once
// the relay's forwarder listens, so the first request needs no retry.
const relayPost = `post() { curl -s -o /dev/null -X POST -d '{}' "$1"; }
`

// A run the model's safeguards refuse falls back to the fallback model whatever its relay refused before: what the
// relay refused is a field of the result, and the failure the fallback compares stays "the model's safeguards".
func TestASafeguardsRefusalAfterARelayRefusalStillFallsBack(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	b := fakeBroker(t)
	dir := t.TempDir()
	refused := `{"type":"result","subtype":"success","is_error":true,"num_turns":1,"total_cost_usd":0.25,"result":"the safeguards blocked it"}`
	harness := script(t, dir, "harness", relayPost+`post "$ANTHROPIC_BEDROCK_BASE_URL/model/us.anthropic.claude-haiku-4-5-20251001-v1:0/invoke"
case "$*" in *"--model model-a"*) `+fixtureResult(refused)+` ;; *) `+fixtureResult(okResult)+` ;; esac`)
	a := testSandboxAgent(t, Agent{Runner: []string{harness}, Model: "model-a", Fallback: "model-b", Bedrock: b.Bedrock})
	res, err := a.RunOrFallback(context.Background(), Invocation{Dir: filepath.Join(shortScratch(t), "run"), Schema: investigationSchema, MaxTurns: 3, BudgetUSD: 1, Timeout: time.Minute})
	if err != nil || res.Status != store.RunOK || res.Model != "model-b" || res.USD != fixtureCharge(5) || res.Failure != "" {
		t.Fatalf("the run after a relay refusal and a safeguards stop: %+v %v", res, err)
	}
	if len(res.Refused) != 1 || !strings.Contains(res.Refused[0], "us.anthropic.claude-haiku-4-5") {
		t.Fatalf("the relay's refusals, once for the two attempts: %q", res.Refused)
	}
}

// Two runs of a case that ended alike stop it whatever their relays refused: the stop rule compares the failure, and
// the refusals are shown beside it in the run's summary, its case's log and the loop's printout.
func TestTwoRunsThatFailedAlikeStopACaseWhateverTheirRelayRefused(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	ctx := context.Background()
	st := testStore(t)
	b := fakeBroker(t)
	dir := t.TempDir()
	harness := script(t, dir, "harness", `n=$(cat '`+filepath.Join(dir, "count")+`' 2>/dev/null || echo 0); n=$((n+1)); echo $n > '`+filepath.Join(dir, "count")+`'
`+relayPost+`post "$ANTHROPIC_BEDROCK_BASE_URL/model/us.anthropic.claude-haiku-$n/invoke"
`+fixtureResult(`{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":91,"total_cost_usd":1.5}`)+``)
	r := runnerFor(t, st, []string{harness})
	r.Agent.Bedrock = b.Bedrock
	c := openCase(t, st, "failures", "a", 3)
	for i := 1; i <= 2; i++ {
		done, err := r.Once(ctx, Options{Families: []string{"failures"}})
		if err != nil || len(done) != 1 || done[0].Case != c.ID || done[0].Run.Status != store.RunTurns || done[0].Run.Failure != "turns" {
			t.Fatalf("run %d: %+v %v", i, done, err)
		}
		// the run's 91 requests run past the relay's allowance of three times its turns, which the relay refuses too
		want := "us.anthropic.claude-haiku-" + fmt.Sprint(i)
		var models int
		for _, refusal := range done[0].Refused {
			if strings.Contains(refusal, want) {
				models++
			}
		}
		if models != 1 || len(done[0].Refused) != 2 || !strings.Contains(done[0].Run.Summary, want) {
			t.Fatalf("run %d: refusals %q, summary %q", i, done[0].Refused, done[0].Run.Summary)
		}
	}
	log, err := st.CaseLog(ctx, c.ID)
	if err != nil || !strings.Contains(log[len(log)-1].Detail, "the relay refused") || !strings.Contains(log[len(log)-1].Detail, "claude-haiku-2") {
		t.Fatalf("the case's log: %+v %v", log, err)
	}
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 || !strings.Contains(done[0].Skipped, "failed the same way: turns") {
		t.Fatalf("the third run: %+v %v", done, err)
	}
	if runs, _ := st.LoopRuns(ctx, store.RunFilter{Case: c.ID}); len(runs) != 2 {
		t.Fatalf("%d runs", len(runs))
	}
}

// What a result's relay refused goes into the run's summary, and so into its case's log; the failure, which the
// stop rule compares, is the result's own.
func TestTheRelayRefusalsAreInTheSummaryNotTheFailure(t *testing.T) {
	res := Result{Status: store.RunFailed, Failure: "the model's safeguards", Refused: []string{`model "x" is no Claude model`}}
	st := testStore(t)
	r := runnerFor(t, st, nil)
	c := openCase(t, st, "failures", "a", 1)
	run, why := r.reserve(context.Background(), c.Family, c.ID, StepInvestigate)
	if why != "" {
		t.Fatal(why)
	}
	r.finish(context.Background(), c, &run, res, nil)
	got, err := st.LoopRunByID(context.Background(), run.ID)
	if err != nil || got.Failure != "the model's safeguards" || !strings.Contains(got.Summary, `the relay refused model "x" is no Claude model`) {
		t.Fatalf("%+v %v", got, err)
	}
}

// The broker's file is read at each launch and never cached: the broker writes it again when it restarts.
func TestTheBrokersFileIsReadAtEachLaunch(t *testing.T) {
	first, second := fakeBroker(t), fakeBroker(t)
	harness := harnessOnRoute(t, okResult, "POST /model/global.anthropic.claude-opus-5-5/invoke")
	a := testSandboxAgent(t, Agent{Runner: harness, Model: "global.anthropic.claude-opus-5-5", Bedrock: first.Bedrock})
	launch := func(name string) {
		t.Helper()
		res, err := a.Run(context.Background(), Invocation{Dir: filepath.Join(shortScratch(t), name), Schema: investigationSchema, MaxTurns: 3, BudgetUSD: 1, Timeout: time.Minute})
		if err != nil || res.Status != store.RunOK {
			t.Fatalf("%s: %+v %v", name, res, err)
		}
	}
	launch("first")
	// the broker restarts: its file names the new broker
	raw, err := os.ReadFile(second.File)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first.File, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	launch("second")
	if got := len(first.seen()); got != 1 {
		t.Errorf("the first broker saw %d requests, want the first launch's alone", got)
	}
	if got := len(second.seen()); got != 1 {
		t.Errorf("the second broker saw %d requests, want the second launch's: the file was read once", got)
	}
}

// A broker is a loopback one: another address, a name that is not local, a scheme that is not http and a URL with no
// port are no route.
func TestTheBrokerMustBeOnLoopback(t *testing.T) {
	b := fakeBroker(t)
	raw, err := os.ReadFile(b.File)
	if err != nil {
		t.Fatal(err)
	}
	for url, ok := range map[string]bool{
		"http://203.0.113.5:8080/n": false, "http://broker.example.invalid:8080/n": false, "https://127.0.0.1:8080/n": false, "http://127.0.0.1/n": false,
		"http://127.0.0.1:8080/n": true, "http://localhost:8080/n": true, "http://[::1]:8080/n": true,
	} {
		err := (&Bedrock{File: rewritten(t, raw, b.url, url)}).Check()
		if (err == nil) != ok || err != nil && !errors.Is(err, ErrNoBedrock) {
			t.Errorf("%s: %v, want a route: %v", url, err, ok)
		}
	}
}

// The relay answers the harness's listing of the profiles only as a GET, only under the run's own path, and no model
// id with a bracket; each is refused at the relay and the broker never sees it.
func TestTheRelayAdmitsOnlyWhatItIsFor(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	b := fakeBroker(t)
	dir := t.TempDir()
	harness := script(t, dir, "harness", `base="$ANTHROPIC_BEDROCK_BASE_URL"; root="${base%/*}"
code() { curl -s -o /dev/null -w '%{http_code}\n' "$@" >> '`+filepath.Join(dir, "codes")+`'; }
code -X GET "$base/inference-profiles"
code -X POST -d '{}' "$base/inference-profiles"
code -X POST -d '{}' "$root/model/global.anthropic.claude-opus-5-5/invoke"
code -X POST -d '{}' "$base/model/global.anthropic.claude-opus-5-5%5B1m%5D/invoke"
code -X POST -d '{"max_tokens":1000}' "$base/model/global.anthropic.claude-opus-5-5/invoke"
echo '`+okResult+`'`)
	a := testSandboxAgent(t, Agent{Runner: []string{harness}, Model: "global.anthropic.claude-opus-5-5", Bedrock: b.Bedrock})
	if _, err := a.Run(context.Background(), Invocation{Dir: filepath.Join(shortScratch(t), "run"), Schema: investigationSchema, MaxTurns: 3, BudgetUSD: 1, Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(codes(t, []string{harness}), " "); got != "200 403 403 403 200" {
		t.Fatalf("the relay answered %s", got)
	}
	seen := b.seen()
	if len(seen) != 2 || !strings.HasPrefix(seen[0], "GET ") || !strings.HasSuffix(seen[0], "/inference-profiles") ||
		!strings.HasPrefix(seen[1], "POST ") || !strings.HasSuffix(seen[1], "/model/global.anthropic.claude-opus-5-5/invoke") {
		t.Fatalf("the broker saw %q", seen)
	}
}
