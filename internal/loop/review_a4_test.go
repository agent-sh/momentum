package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/store"
)

func TestHarnessNumbersAreOnlyDiagnostics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stream")
	write(t, path, `{"type":"result","subtype":"success","total_cost_usd":1000,"num_turns":999,"session_id":"forged","permission_denials":[{}],"usage":{"input_tokens":99999},"structured_output":{"summary":"marker-7f3a"}}`)
	res, err := readStream(path)
	if err != nil || res.USD != 0 || res.CostKnown || res.Tokens != 0 || res.Turns != 0 || res.TurnsKnown || res.Status == store.RunOK ||
		res.Harness.Session != "forged" || res.Harness.Denials != 1 {
		t.Fatalf("stdout installed host observations: %+v %v", res, err)
	}
}

func TestRelayEnforcesHostTurnsForSequentialAndConcurrentRequests(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		t.Run(map[bool]string{false: "sequential", true: "concurrent"}[concurrent], func(t *testing.T) {
			var calls atomic.Int64
			r := budgetRelay(t, 1, func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				return brokerResponse(req, structuredFrames(`{"findings":[],"summary":"terminal"}`)), nil
			})
			r.turnLimit = requestAllowance(Invocation{MaxTurns: 1})
			var success, denied atomic.Int64
			request := func() {
				rec := relayRequest(r, strings.NewReader(textRequestBody(10, 1000)), "invoke-with-response-stream")
				switch rec.Code {
				case 200:
					success.Add(1)
				case 429:
					denied.Add(1)
				default:
					t.Errorf("unexpected request status %d", rec.Code)
				}
			}
			var wg sync.WaitGroup
			for i := 0; i < 4; i++ {
				if concurrent {
					wg.Add(1)
					go func() { defer wg.Done(); request() }()
				} else {
					request()
				}
			}
			wg.Wait()
			_, over := r.observedUsage()
			if success.Load() != 3 || denied.Load() != 1 || calls.Load() != 3 || !over {
				t.Fatalf("host turns: %d accepted, %d refused, %d broker calls, over %v", success.Load(), denied.Load(), calls.Load(), over)
			}
		})
	}
}

func TestFallbackGetsOnlyTheRemainingHostTurnAllowance(t *testing.T) {
	inv := fallbackInvocation(Invocation{MaxTurns: 1, BudgetUSD: 1, Timeout: time.Minute}, Result{Turns: 1, USD: .1}, time.Second)
	if inv.MaxTurns != 1 || requestAllowance(inv) != 2 || inv.BudgetUSD != .9 || inv.Timeout != 59*time.Second {
		t.Fatalf("fallback widened or lost its allowance: %+v", inv)
	}
	r := budgetRelay(t, 1, func(req *http.Request) (*http.Response, error) { return brokerResponse(req, usageFrames(10, 100)), nil })
	r.turnLimit = requestAllowance(inv)
	for _, want := range []int{200, 200, 429} {
		if rec := relayRequest(r, strings.NewReader(textRequestBody(10, 1000)), "invoke-with-response-stream"); rec.Code != want {
			t.Fatalf("fallback got fresh turns: %d, want %d", rec.Code, want)
		}
	}
}

func TestTerminalReportFollowsResponseCompletionOrder(t *testing.T) {
	early := `{"findings":[],"summary":"initial"}`
	late := `{"findings":[{"severity":"high","where":"fixture.go","problem":"gate fails","fix":"repair it"}],"summary":"final"}`
	blocked, release := make(chan struct{}), make(chan struct{})
	r := budgetRelay(t, 1, func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		report := early
		if bytes.Contains(body, []byte("delayed")) {
			close(blocked)
			select {
			case <-release:
			case <-req.Context().Done():
			}
			report = late
		}
		return brokerResponse(req, structuredFrames(report)), nil
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		relayRequest(r, strings.NewReader(`{"max_tokens":1000,"messages":[{"role":"user","content":"delayed"}]}`), "invoke-with-response-stream")
	}()
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("request did not start")
	}
	relayRequest(r, strings.NewReader(textRequestBody(10, 1000)), "invoke-with-response-stream")
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not complete")
	}
	reports, refused := r.reportsObserved()
	for _, tc := range []struct {
		report string
		ok     bool
	}{{early, false}, {late, true}} {
		res := Result{Report: json.RawMessage(tc.report)}
		authenticateReport(&res, reports, refused)
		if (res.Status == store.RunOK) != tc.ok {
			t.Fatalf("stdout selected report out of completion order: %+v", res)
		}
	}
}

func TestRelayRefusesBinaryAndNonJSONBodiesBeforeForwarding(t *testing.T) {
	var calls atomic.Int64
	r := budgetRelay(t, 1, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return brokerResponse(req, usageFrames(10, 100)), nil
	})
	for _, body := range []string{"marker-7f3a", "[]", `{"max_tokens":1000,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"eA=="}}]}]}`,
		`{"max_tokens":1000,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","data":"marker-7f3a"}}]}]}`} {
		if rec := relayRequest(r, strings.NewReader(body), "invoke"); rec.Code != 400 {
			t.Fatalf("unbounded input admitted: %d %s", rec.Code, body)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid input reached broker")
	}
}

func TestReservationRejectsTextWhoseMaximumDoesNotFit(t *testing.T) {
	r := budgetRelay(t, .022, func(req *http.Request) (*http.Response, error) {
		t.Error("over-budget request forwarded")
		return brokerResponse(req, usageFrames(1000, 1000)), nil
	})
	if rec := relayRequest(r, strings.NewReader(textRequestBody(1000, 1000)), "invoke-with-response-stream"); rec.Code != 429 {
		t.Fatalf("valid input exceeded the reservation: %d", rec.Code)
	}
}

func TestAgentIgnoresForgedChargesAndTokenClaims(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-route-observation", true: "observed"}[routed], func(t *testing.T) {
			b := fakeBroker(t)
			b.mu.Lock()
			b.reply = structuredFrames(`{"findings":[],"summary":"terminal"}`)
			b.mu.Unlock()
			record := `{"type":"result","subtype":"success","num_turns":999,"total_cost_usd":1000,"usage":{"input_tokens":99999},"structured_output":{"findings":[],"summary":"terminal"}}`
			body := "echo " + shellQuote(record)
			if routed {
				body = `curl -s -o /dev/null -d '{"max_tokens":1000,"messages":[{"role":"user","content":"marker-7f3a"}]}' "$ANTHROPIC_BEDROCK_BASE_URL/model/global.anthropic.claude-opus-5-5/invoke-with-response-stream"` + "\n" + body
			}
			a := testSandboxAgent(t, Agent{Runner: []string{script(t, t.TempDir(), "harness", body)}, Model: "global.anthropic.claude-opus-5-5", Bedrock: b.Bedrock})
			res, err := a.RunOrFallback(context.Background(), Invocation{Dir: filepath.Join(shortScratch(t), "run"), MaxTurns: 1, BudgetUSD: 1, Timeout: time.Minute, Schema: investigationSchema})
			if err != nil {
				t.Fatal(err)
			}
			want := 1.0
			if routed {
				want = .00204
			}
			if res.USD != want || (res.Status == store.RunOK) != routed || res.Harness.USD == nil || *res.Harness.USD != 1000 || res.Tokens == 99999 {
				t.Fatalf("stdout governed accounting: %+v", res)
			}
		})
	}
}

func TestMatchingReportCannotOverrideRefusedTurns(t *testing.T) {
	b := fakeBroker(t)
	harness := script(t, t.TempDir(), "harness", `for i in 1 2 3 4; do
curl -s -o /dev/null -d '{"max_tokens":1000,"messages":[{"role":"user","content":"marker-7f3a"}]}' "$ANTHROPIC_BEDROCK_BASE_URL/model/global.anthropic.claude-opus-5-5/invoke-with-response-stream"
done
echo '`+okResult+`'`)
	a := testSandboxAgent(t, Agent{Runner: []string{harness}, Model: "global.anthropic.claude-opus-5-5", Bedrock: b.Bedrock})
	res, err := a.RunOrFallback(context.Background(), Invocation{Dir: filepath.Join(shortScratch(t), "run"), MaxTurns: 1, BudgetUSD: 1, Timeout: time.Minute, Schema: investigationSchema})
	if err != nil || res.Status != store.RunTurns || res.Failure != "turns" || res.RelayAdmitted != 3 || res.RelayRefused != 1 || len(res.Report) != 0 {
		t.Fatalf("matching report bypassed host turn limit: %+v %v", res, err)
	}
}

func TestUnknownFallbackReservationSurvivesRunnerAccounting(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	c := openCase(t, st, "failures", "fixture", 1)
	r := localRunner(t, st)
	row, why := r.reserve(ctx, c.Family, c.ID, StepInvestigate)
	if why != "" {
		t.Fatal(why)
	}
	primary := Result{USD: .00204, RelayUSD: .00204, CostKnown: true, RelayAdmitted: 1, RelayCharged: 1}
	combined := finishFallback(primary, Result{Status: store.RunTimeout, Failure: "timeout"}, r.Budgets.Run-primary.USD)
	r.finish(ctx, c, &row, combined, nil)
	saved, err := st.LoopRunByID(ctx, row.ID)
	if err != nil || saved.USD != r.Budgets.Run || combined.ReservationUSD != r.Budgets.Run-primary.USD {
		t.Fatalf("unknown fallback reservation was lost: %+v %+v %v", combined, saved, err)
	}
}
