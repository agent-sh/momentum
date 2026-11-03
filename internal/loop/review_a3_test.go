package loop

import (
	"encoding/json"
	"fmt"
	"github.com/agent-sh/momentum/internal/store"

	"net/http"

	"strings"
	"sync/atomic"

	"testing"
	"time"
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
		{"two-first", []string{first, last}, first, false},
		{"two-last", []string{first, last}, last, false},
		{"earlier-clean", []string{last, first}, last, false},
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
