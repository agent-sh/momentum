package loop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/sandbox"
	"github.com/agent-sh/momentum/internal/store"
)

// A response that carries two reports has no single terminal call: it voids the report an earlier response gave, so
// that earlier report cannot be authenticated afterwards.
func TestTwoReportsInOneResponseVoidAnEarlierReport(t *testing.T) {
	first := `{"findings":[],"summary":"first"}`
	responses := [][]byte{structuredFrames(first), structuredFrames(`{"findings":[],"summary":"second"}`, `{"findings":[],"summary":"third"}`)}
	var n atomic.Int64
	r := budgetRelay(t, 1, func(req *http.Request) (*http.Response, error) {
		return brokerResponse(req, responses[n.Add(1)-1]), nil
	})
	for i := range responses {
		if rec := relayRequest(r, strings.NewReader(`{"max_tokens":1000}`), "invoke-with-response-stream"); rec.Code != 200 {
			t.Fatalf("response %d: %d", i, rec.Code)
		}
	}
	res := Result{Status: store.RunOK, Report: json.RawMessage(first)}
	reports, refused := r.reportsObserved()
	authenticateReport(&res, reports, refused)
	if res.Status == store.RunOK || len(res.Report) != 0 {
		t.Fatalf("the earlier report outlived a response of two: %+v", res)
	}
}

// An error answer releases its reservation: a run whose allowance holds one request's maximum charge can make its
// next request after the broker refused one.
func TestAnErrorAnswerReleasesItsReservation(t *testing.T) {
	body := textRequestBody(10, 1000)
	rate := relayRates["global.anthropic.claude-opus-5-5"]
	reserve := (1000*rate.output + float64(len(body))*max(rate.input, rate.write1)) / 1e6
	var n atomic.Int64
	r := budgetRelay(t, 1.2*reserve/1.5, func(req *http.Request) (*http.Response, error) {
		if n.Add(1) == 1 {
			resp := brokerResponse(req, []byte(`{"message":"Too many requests"}`))
			resp.StatusCode = 429
			return resp, nil
		}
		return brokerResponse(req, usageFrames(10, 10)), nil
	})
	if rec := relayRequest(r, strings.NewReader(body), "invoke-with-response-stream"); rec.Code != 429 {
		t.Fatalf("the error answer: %d", rec.Code)
	}
	if rec := relayRequest(r, strings.NewReader(body), "invoke-with-response-stream"); rec.Code != 200 || n.Load() != 2 {
		t.Fatalf("the request after an error answer: %d, %d broker calls", rec.Code, n.Load())
	}
}

// The harness's one GET is the inference-profile list itself: a longer path under it is no model request and never
// reaches the broker.
func TestTheRelayAdmitsOnlyTheProfileListItself(t *testing.T) {
	var calls atomic.Int64
	r := budgetRelay(t, 1, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return brokerResponse(req, []byte(`{}`)), nil
	})
	for _, path := range []string{"/inference-profiles/extra", "/inference-profiles/a/b"} {
		rec := httptest.NewRecorder()
		r.srv.Handler.ServeHTTP(rec, httptest.NewRequest("GET", r.prefix+path, nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET %s answered %d", path, rec.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("the broker was asked %d times", calls.Load())
	}
}

// Output tokens only grow in a stream: a delta that reports fewer than the start is a corrupt answer, priced at its
// reservation rather than at what it claims.
func TestUsageThatShrinksIsRefused(t *testing.T) {
	wire := append(eventFrame([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":50}}}`)),
		eventFrame([]byte(`{"type":"message_delta","usage":{"output_tokens":10}}`))...)
	p := usageParser{stream: true}
	p.feed(wire)
	p.eof = true
	if _, ok := p.cost(relayRates["global.anthropic.claude-opus-5-5"]); ok {
		t.Fatal("a stream whose output tokens shrank was priced")
	}
}

// A run bind may not reach the run's host-only directory (its relay socket, archives and trusted inputs), except the
// fixed read-only inputs the host puts there for it.
func TestARunBindIntoItsHostDirectoryIsRefused(t *testing.T) {
	a := testSandboxAgent(t, Agent{Runner: []string{script(t, t.TempDir(), "harness", fixtureResult(okResult))}, Model: "global.anthropic.claude-opus-5-5"})
	dir := filepath.Join(shortScratch(t), "run")
	if err := os.MkdirAll(HostDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(HostDir(dir), "archive.txt")
	if err := os.WriteFile(secret, []byte("marker-7f3a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := a.Run(context.Background(), Invocation{Dir: dir, System: "marker-7f3a", Schema: investigationSchema, MaxTurns: 3, BudgetUSD: 1,
		Timeout: time.Minute, Binds: []sandbox.Bind{{Src: secret, Dst: "/ro/archive.txt"}}})
	if err == nil || !strings.Contains(err.Error(), "host-only directory") {
		t.Fatalf("a bind into the host-only directory: %v", err)
	}
}

// With eight requests in flight a ninth waits for a slot and is refused when none frees: it never reaches the broker.
func TestANinthRequestInFlightIsRefused(t *testing.T) {
	started, release := make(chan struct{}, relayInFlight+1), make(chan struct{})
	var calls atomic.Int64
	r := budgetRelay(t, 100, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
		case <-req.Context().Done():
		}
		return brokerResponse(req, usageFrames(0, 0)), nil
	})
	defer close(release)
	for i := 0; i < relayInFlight; i++ {
		go relayRequest(r, strings.NewReader(`{"max_tokens":1}`), "invoke-with-response-stream")
	}
	for i := 0; i < relayInFlight; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("requests did not enter")
		}
	}
	late := make(chan int, 1)
	go func() {
		late <- relayRequest(r, strings.NewReader(`{"max_tokens":1}`), "invoke-with-response-stream").Code
	}()
	select {
	case code := <-late:
		if code != 429 || calls.Load() != relayInFlight {
			t.Fatalf("the ninth request: %d, %d broker calls", code, calls.Load())
		}
	case <-time.After(relayWait + 2*time.Second):
		t.Fatalf("the ninth request was admitted: %d broker calls", calls.Load())
	}
}

// A run's case.json, system prompt and prompt are scrubbed before the run gets them: the case's own title, the
// standing constraints and the brief can hold what the scrubber masks.
func TestARunsCaseSystemAndPromptAreScrubbed(t *testing.T) {
	goodInputScrubber(t)
	st := testStore(t)
	ctx := context.Background()
	r := runnerFor(t, st, fakeAgent(t, `{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":1}`))
	r.Constraints = []string{"never print marker-7f3a"}
	c := openCase(t, st, "failures", "marker-7f3a", 1)
	r.pass = &passInputs{}
	defer r.pass.close()
	if err := r.prepareInputs(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := r.prepareRun(ctx, c, c.Family, StepInvestigate)
	if err != nil {
		t.Fatal(err)
	}
	host := HostDir(run.Dir)
	for _, path := range []string{filepath.Join(host, "input", "case.json"), filepath.Join(host, "input", "system.md"), filepath.Join(host, "prompt.json")} {
		raw, err := os.ReadFile(path)
		if err != nil || strings.Contains(string(raw), "marker-7f3a") || !strings.Contains(string(raw), "[masked]") {
			t.Errorf("%s is not scrubbed: %v", filepath.Base(path), err)
		}
	}
}
