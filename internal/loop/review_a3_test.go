package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func TestRelayRefusesAnUnaffordableMaximumCharge(t *testing.T) {
	r := budgetRelay(t, 1, func(req *http.Request) (*http.Response, error) {
		t.Error("an unaffordable request reached the broker")
		return brokerResponse(req, usageFrames(0, 0)), nil
	})
	for _, body := range []string{`{"max_tokens":128000}`, `{}`, `{"max_tokens":1,"messages":"` + strings.Repeat("x", 400000) + `"}`} {
		rec := relayRequest(r, strings.NewReader(body), "invoke-with-response-stream")
		if rec.Code != 429 || !strings.Contains(rec.Body.String(), "allowance") {
			t.Fatalf("unaffordable request: %d %s", rec.Code, rec.Body.String())
		}
	}
	_, admitted, refused, _ := r.accounting()
	if admitted != 0 || refused != 3 {
		t.Fatalf("admitted %d, refused %d", admitted, refused)
	}
}

func TestRelayRefusesConcurrentReservationsAboveAllowance(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	r := budgetRelay(t, .02, func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
		}
		return brokerResponse(req, usageFrames(0, 1)), nil
	})
	done := make(chan int, 1)
	go func() {
		done <- relayRequest(r, strings.NewReader(`{"max_tokens":1000}`), "invoke-with-response-stream").Code
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("first request did not reach the broker")
	}
	rec := relayRequest(r, strings.NewReader(`{"max_tokens":1000}`), "invoke-with-response-stream")
	close(release)
	if code := <-done; code != 200 {
		t.Fatal(code)
	}
	if rec.Code != 429 {
		t.Fatalf("second reservation exceeded allowance: %d", rec.Code)
	}
}

func TestLandingPreservesAMovedProposalBranch(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	ctx := context.Background()
	c, p := recoveryReviewed(t, s, st, f)
	other := strings.TrimSpace(run(t, p.Host, "git", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
		"commit-tree", p.Head+"^{tree}", "-p", p.Head, "-m", "second writer"))
	realGit, _ := exec.LookPath("git")
	bin := filepath.Join(f.dir, "bin")
	script(t, bin, "git", `base=no
for arg do if [ "$arg" = "`+p.Head+`:refs/heads/main" ]; then base=yes; fi; done
'`+realGit+`' "$@"
code=$?
if [ "$code" = 0 ] && [ "$base" = yes ]; then
 '`+realGit+`' -C '`+p.Host+`' push --no-follow-tags origin '`+other+`:refs/heads/`+p.Branch+`' || exit 1
fi
exit "$code"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	landed, err := s.Land(ctx, c.ID)
	if landed.State != Landed || err == nil || !strings.Contains(err.Error(), "cleanup") {
		t.Fatalf("landing cleanup did not report refusal: %+v %v", landed, err)
	}
	if originHead(t, f, p.Branch) != other || originHead(t, f, "main") != p.Head {
		t.Fatal("landing deleted a second writer's branch or failed to land")
	}
	got, _ := st.CaseByID(ctx, c.ID)
	if got.Status != store.CaseDeployed {
		t.Fatalf("successful landing receipt was lost: %s", got.Status)
	}
	if _, err := s.Land(ctx, c.ID); err == nil || originHead(t, f, p.Branch) != other {
		t.Fatal("cleanup refusal allowed a repeated landing")
	}
}

func TestLandingKeepsAnUncertainPublishedCandidate(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	ctx := context.Background()
	c, p := recoveryReviewed(t, s, st, f)
	pushOther(t, f, "main", map[string]string{"README": "next base\n"})
	realGit, _ := exec.LookPath("git")
	bin, lost := filepath.Join(f.dir, "bin"), filepath.Join(f.dir, "lost-response")
	script(t, bin, "git", `proposal=no
probe=no
for arg do
 case "$arg" in *:refs/heads/`+p.Branch+`) proposal=yes ;; ls-remote) probe=yes ;; esac
done
if [ "$probe" = yes ] && [ -f '`+lost+`' ]; then exit 1; fi
'`+realGit+`' "$@"
code=$?
if [ "$code" = 0 ] && [ "$proposal" = yes ] && [ ! -f '`+lost+`' ]; then
 echo lost > '`+lost+`'
 exit 1
fi
exit "$code"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if _, err := s.Land(ctx, c.ID); err == nil {
		t.Fatal("fixture did not lose the push response and its reconciliation")
	}
	accepted := originHead(t, f, p.Branch)
	saved, _, err := s.Load(c.ID)
	if err != nil || saved.State != Landing || saved.LandingHead != accepted || saved.PublishedHead != p.Head {
		t.Fatalf("uncertain candidate was discarded: %+v %v", saved, err)
	}
	// A new committer time must not produce another rebase candidate on retry.
	t.Setenv("GIT_COMMITTER_DATE", "2030-01-01T00:00:00Z")
	os.Remove(lost)
	// Restore the normal Git executable, retaining the exact uncertain journal.
	t.Setenv("PATH", filepath.Dir(realGit)+":"+os.Getenv("PATH"))
	landed, err := s.Land(ctx, c.ID)
	if err != nil || landed.State != Landed || landed.Head != accepted || landed.PublishedHead != accepted || originHead(t, f, "main") != accepted {
		t.Fatalf("retry did not adopt the exact journaled candidate: %+v %v", landed, err)
	}
}

func TestLandingCanRebaseAgainAfterItsPublicationIsKnown(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	ctx := context.Background()
	c, _ := recoveryReviewed(t, s, st, f)
	pushOther(t, f, "main", map[string]string{"README": "first base\n"})
	realGit, _ := exec.LookPath("git")
	bin := filepath.Join(f.dir, "bin")
	script(t, bin, "git", `for arg do
 case "$arg" in *:refs/heads/main) echo 'fixture refuses the default push' >&2; exit 1 ;; esac
done
exec '`+realGit+`' "$@"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if _, err := s.Land(ctx, c.ID); err == nil {
		t.Fatal("fixture did not refuse the default push")
	}
	journal, _, _ := s.Load(c.ID)
	if journal.State != Landing || journal.LandingHead == "" || journal.PublishedHead != journal.LandingHead {
		t.Fatalf("no known publication: %+v", journal)
	}
	t.Setenv("PATH", filepath.Dir(realGit)+":"+os.Getenv("PATH"))
	pushOther(t, f, "main", map[string]string{"README": "second base\n"})
	landed, err := s.Land(ctx, c.ID)
	if err != nil || landed.State != Landed || landed.Head == journal.LandingHead || originHead(t, f, "main") != landed.Head {
		t.Fatalf("known publication prevented a necessary rebase: %+v %v", landed, err)
	}
}

func TestExecutableTestKeepsItsModeInTheBaseline(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "existing"}[existing], func(t *testing.T) {
			st := testStore(t)
			files := map[string]string{"Makefile": "test:\n\t./tests/probe\n"}
			if existing {
				files["tests/probe"] = "#!/bin/sh\nexit 0\n"
			}
			f := newRepo(t, files)
			s := recoveryShip(t, st, f)
			c := testingCase(t, st)
			p, err := s.Prepare(context.Background(), c.ID, f.main)
			if err != nil {
				t.Fatal(err)
			}
			probe := filepath.Join(p.Work, "tests", "probe")
			write(t, probe, "#!/bin/sh\nexit 0\n")
			os.Chmod(probe, 0755)
			_, err = checkPatch(t, s, context.Background(), c.ID, "make test", []string{"tests/probe"})
			if err == nil || !strings.Contains(err.Error(), "passes without") {
				t.Fatalf("executable bit supplied false discrimination: %v", err)
			}
			var ck Check
			raw, _ := os.ReadFile(filepath.Join(s.pkgDir(c.ID), "check.json"))
			if json.Unmarshal(raw, &ck) != nil || ck.Before.Exit != 0 || ck.After.Exit != 0 {
				t.Fatalf("test modes differed between exports: %s", raw)
			}
		})
	}
}

func TestExecutableExportsKeepTheCandidateModeUnderARestrictiveUmask(t *testing.T) {
	mask := syscall.Umask(0077)
	t.Cleanup(func() { syscall.Umask(mask) })
	st := testStore(t)
	f := newRepo(t, map[string]string{"Makefile": "test:\n\t./tests/probe\n"})
	s := recoveryShip(t, st, f)
	c := testingCase(t, st)
	p, err := s.Prepare(context.Background(), c.ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(p.Work, "tests", "probe")
	write(t, probe, "#!/bin/sh\ntest \"$(stat -c %a tests/probe)\" = 755\n")
	os.Chmod(probe, 0755)
	_, err = checkPatch(t, s, context.Background(), c.ID, "make test", []string{"tests/probe"})
	if err == nil || !strings.Contains(err.Error(), "passes without") {
		t.Fatalf("mode supplied false discrimination: %v", err)
	}
	var ck Check
	raw, _ := os.ReadFile(filepath.Join(s.pkgDir(c.ID), "check.json"))
	if json.Unmarshal(raw, &ck) != nil || ck.Before.Exit != 0 || ck.After.Exit != 0 {
		t.Fatalf("candidate and baseline modes differ: %s", raw)
	}
}

func TestRunSummariesAreCleanedBeforePersistence(t *testing.T) {
	old := Scrubber
	t.Cleanup(func() { Scrubber = old })
	Scrubber = []string{script(t, t.TempDir(), "cleaner", "sed -e 's/marker-7f3a/[masked]/g' -e 's/safeguards/[masked]/g'")}
	ctx := context.Background()
	st := testStore(t)
	r := Runner{Store: st, Budgets: Budgets{Run: 1}}
	c := openCase(t, st, "failures", "fixture", 1)
	for i, res := range []Result{
		{Started: true, Status: store.RunOK, CostKnown: true, Report: json.RawMessage(`{"outcome":"investigated","summary":"marker-7f3a","root":"root:marker-7f3a"}`)},
		{Started: true, Status: store.RunFailed, Failure: "the model's safeguards", CostKnown: true, Text: "marker-7f3a"},
		{Started: true, Status: store.RunOK, CostKnown: true},
	} {
		run := store.LoopRun{ID: fmt.Sprint(i), Case: c.ID, Family: c.Family, Step: StepBuild,
			Started: time.Now(), Status: store.RunRunning, USD: 1}
		if i == 2 {
			run.Step, run.Summary = StepReview, "0 high, 0 medium, 0 low: marker-7f3a"
		}
		if err := st.PutLoopRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		r.finish(ctx, c, &run, res, nil)
		saved, _ := st.LoopRunByID(ctx, run.ID)
		if strings.Contains(saved.Summary, "marker-7f3a") || !strings.Contains(saved.Summary, "[masked]") || saved.Failure != res.Failure {
			t.Fatalf("unclean summary or changed failure key: %+v", saved)
		}
	}
	log, _ := st.CaseLog(ctx, c.ID)
	for _, entry := range log {
		if strings.Contains(entry.Detail, "marker-7f3a") {
			t.Fatalf("unclean case log: %+v", entry)
		}
	}
	savedCase, _ := st.CaseByID(ctx, c.ID)
	if !slices.Contains(savedCase.Signatures, "root:[masked]") {
		t.Fatalf("root text was not cleaned: %v", savedCase.Signatures)
	}
	Scrubber = []string{filepath.Join(t.TempDir(), "missing")}
	run := store.LoopRun{ID: "failed-cleaner", Case: c.ID, Step: StepInvestigate, Status: store.RunRunning, Started: time.Now(), USD: 1}
	st.PutLoopRun(ctx, run)
	r.finish(ctx, c, &run, Result{Started: true, Status: store.RunOK, CostKnown: true, Text: "marker-7f3a"}, nil)
	saved, _ := st.LoopRunByID(ctx, run.ID)
	if saved.Status != store.RunFailed || strings.Contains(saved.Summary, "marker-7f3a") || strings.Contains(run.Summary, "marker-7f3a") {
		t.Fatalf("failed cleaner leaked a summary: %+v", saved)
	}
}

func TestReviewTextIsCleanedBeforePackageAndCaseLog(t *testing.T) {
	st := testStore(t)
	f := newRepo(t, nil)
	s := recoveryShip(t, st, f)
	ctx := context.Background()
	c, _, pr := recoveryChecked(t, s, st, f)
	if _, err := proposePatch(t, s, ctx, c.ID, pr); err != nil {
		t.Fatal(err)
	}
	old := Scrubber
	t.Cleanup(func() { Scrubber = old })
	Scrubber = []string{script(t, t.TempDir(), "cleaner", "sed 's/marker-7f3a/[masked]/g'")}
	rev := Review{Text: "marker-7f3a", Findings: []Finding{{Severity: "medium", Where: "marker-7f3a", Problem: "marker-7f3a", Fix: "marker-7f3a"}}}
	p, err := s.Reviewed(ctx, c.ID, rev)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(s.pkgDir(c.ID), "package.json"))
	if strings.Contains(string(raw), "marker-7f3a") || !strings.Contains(string(raw), "[masked]") || p.Reviews[0].Medium != 1 {
		t.Fatalf("unclean review or changed counts: %s", raw)
	}
	cleanCtx := store.WithTextCleaner(ctx, Redact)
	if err := st.CaseNote(cleanCtx, c.ID, "fixture", "marker-7f3a", "test"); err != nil {
		t.Fatal(err)
	}
	log, _ := st.CaseLog(ctx, c.ID)
	if log[len(log)-1].Detail != "[masked]" {
		t.Fatalf("unclean case note: %+v", log[len(log)-1])
	}
	Scrubber = []string{filepath.Join(t.TempDir(), "missing")}
	if _, err := s.Reviewed(ctx, c.ID, rev); err == nil {
		t.Fatal("failed cleaner accepted a review")
	}
	saved, _, _ := s.Load(c.ID)
	if len(saved.Reviews) != 1 {
		t.Fatal("failed cleaner persisted a review")
	}
	if err := st.CaseNote(cleanCtx, c.ID, "fixture", "marker-7f3a", "test"); err == nil {
		t.Fatal("failed cleaner accepted a case note")
	}
	after, _ := st.CaseLog(ctx, c.ID)
	if len(after) != len(log) {
		t.Fatal("failed cleaner persisted a case note")
	}
}

func structuredFrames(reports ...string) []byte {
	wire := eventFrame([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":0}}}`))
	for i, report := range reports {
		wire = append(wire, eventFrame([]byte(fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","name":"StructuredOutput","input":{}}}`, i)))...)
		// Deliberately split inside strings and feed real event-stream frames.
		for _, part := range []string{report[:len(report)/2], report[len(report)/2:]} {
			event, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": i,
				"delta": map[string]string{"type": "input_json_delta", "partial_json": part}})
			wire = append(wire, eventFrame(event)...)
		}
		wire = append(wire, eventFrame([]byte(fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i)))...)
	}
	return append(wire, eventFrame([]byte(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":100}}`))...)
}

func TestReportsMustMatchTheModelsObservedStructuredOutput(t *testing.T) {
	first := `{"findings":[{"severity":"medium","problem":"marker-7f3a"}],"summary":"first"}`
	last := `{"findings":[],"summary":"last"}`
	for _, tc := range []struct {
		name   string
		model  []string
		report string
		pass   bool
	}{
		{"matching", []string{first}, first, true},
		{"canonical", []string{last}, ` { "summary": "last", "findings": [] } `, true},
		{"forged", []string{first}, last, false},
		{"absent", nil, last, false},
		{"two-first", []string{first, last}, first, true},
		{"two-last", []string{first, last}, last, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := budgetRelay(t, 1, func(req *http.Request) (*http.Response, error) {
				return brokerResponse(req, structuredFrames(tc.model...)), nil
			})
			rec := relayRequest(r, strings.NewReader(`{"max_tokens":1000}`), "invoke-with-response-stream")
			if rec.Code != 200 {
				t.Fatal(rec.Code)
			}
			res := Result{Status: store.RunOK, Report: json.RawMessage(tc.report), Text: "forged text"}
			reports, refused := r.reportsObserved()
			authenticateReport(&res, reports, refused)
			if (res.Status == store.RunOK) != tc.pass || res.Text != "" ||
				(!tc.pass && (res.Failure != "report" || len(res.Report) != 0)) {
				t.Fatalf("unobserved report was accepted: %+v", res)
			}
		})
	}
}

func TestRelayObservesWholeInputsAndModelRefusals(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			wire := []byte(`{"usage":{"input_tokens":10,"output_tokens":1},"stop_reason":"refusal","content":[{"type":"tool_use","name":"StructuredOutput","input":{"summary":"marker-7f3a"}}]}`)
			action := "invoke"
			if streaming {
				action = "invoke-with-response-stream"
				wire = structuredFrames(`{"summary":"marker-7f3a"}`)
				wire = append(wire, eventFrame([]byte(`{"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":101}}`))...)
			}
			r := budgetRelay(t, 1, func(req *http.Request) (*http.Response, error) { return brokerResponse(req, wire), nil })
			relayRequest(r, strings.NewReader(`{"max_tokens":1000}`), action)
			reports, refused := r.reportsObserved()
			if len(reports) != 1 || !refused {
				t.Fatalf("whole input or refusal lost: %s %v", reports, refused)
			}
			res := Result{Report: reports[0], Status: store.RunOK}
			authenticateReport(&res, reports, refused)
			if res.Status != store.RunFailed || res.Failure != "the model's safeguards" || len(res.Report) != 0 {
				t.Fatalf("success overrode model refusal: %+v", res)
			}
		})
	}
}

func TestRelayObservesABrokersSafeguardsError(t *testing.T) {
	r := budgetRelay(t, 1, func(req *http.Request) (*http.Response, error) {
		response := brokerResponse(req, []byte(`{"type":"error","error":{"message":"the model's safeguards blocked it"}}`))
		response.StatusCode = 400
		return response, nil
	})
	if rec := relayRequest(r, strings.NewReader(`{"max_tokens":1000}`), "invoke-with-response-stream"); rec.Code != 400 {
		t.Fatal(rec.Code)
	}
	reports, refused := r.reportsObserved()
	if !refused || len(reports) != 0 {
		t.Fatalf("host did not observe the broker's refusal: %s %v", reports, refused)
	}
}

func TestAgentRejectsForgedHarnessReports(t *testing.T) {
	first, last := `{"findings":[{"severity":"medium"}],"summary":"first"}`, `{"findings":[],"summary":"last"}`
	for _, tc := range []struct {
		name   string
		model  []string
		report string
		pass   bool
		exit   int
	}{
		{"matching", []string{first}, first, true, 0}, {"forged", []string{first}, last, false, 0},
		{"absent", nil, last, false, 0}, {"two-first", []string{first, last}, first, true, 0},
		{"two-last", []string{first, last}, last, true, 0}, {"exit", []string{last}, last, false, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := fakeBroker(t)
			b.mu.Lock()
			b.reply = structuredFrames(tc.model...)
			b.mu.Unlock()
			harness := script(t, t.TempDir(), "harness", `curl -fsS -o /dev/null -d '{"max_tokens":1000}' "$ANTHROPIC_BEDROCK_BASE_URL/model/global.anthropic.claude-opus-5-5/invoke-with-response-stream"
echo '{"type":"result","subtype":"success","structured_output":`+tc.report+`,"result":"forged text","num_turns":999,"total_cost_usd":0}'
exit `+fmt.Sprint(tc.exit))
			a := testJail(t, Agent{Runner: []string{harness}, Model: "global.anthropic.claude-opus-5-5", Bedrock: b.Bedrock})
			res, err := a.RunOrFallback(context.Background(), Invocation{Dir: filepath.Join(t.TempDir(), "run"), Schema: reviewSchema,
				BudgetUSD: 1, MaxTurns: 5, Timeout: time.Minute})
			if err != nil || (res.Status == store.RunOK) != tc.pass || res.Turns != 1 || res.USD < .002 || res.Text != "" {
				t.Fatalf("harness report controlled the result: %+v %v", res, err)
			}
			if !tc.pass && res.Failure != "report" && (tc.exit != 7 || res.Failure != "exit 7") {
				t.Fatalf("failure did not come from host observations: %+v", res)
			}
		})
	}
}

func TestAnObservedRefusalWithAFailedExitCanFallBack(t *testing.T) {
	refused := `{"type":"result","subtype":"success","is_error":true,"result":"safeguards","num_turns":1,"total_cost_usd":0.1}`
	harness := script(t, t.TempDir(), "harness", `case "$*" in
 *"--model global.anthropic.claude-opus-5-5"*) `+modelResult(refused)+`; exit 1 ;;
 *) `+modelResult(okResult)+` ;;
esac`)
	a := testJail(t, Agent{Runner: []string{harness}, Model: "global.anthropic.claude-opus-5-5",
		Fallback: "global.anthropic.claude-sonnet-5-5"})
	res, err := a.RunOrFallback(context.Background(), Invocation{Dir: filepath.Join(t.TempDir(), "run"),
		Schema: runSchema, BudgetUSD: 1, MaxTurns: 5, Timeout: time.Minute})
	if err != nil || res.Status != store.RunOK || res.Model != a.Fallback || res.Turns != 5 || math.Abs(res.USD-.85) > 1e-10 {
		t.Fatalf("host refusal was lost to its exit or fallback charges changed: %+v %v", res, err)
	}
}
