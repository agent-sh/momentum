package loop

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// eventFrame builds AWS event-stream wire frames, including headers and both CRCs.
func eventFrame(event []byte) []byte {
	payload, _ := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString(event)})
	headers := []byte{13}
	headers = append(headers, []byte(":message-type")...)
	headers = append(headers, 7, 0, 5)
	headers = append(headers, []byte("event")...)
	out := make([]byte, 12+len(headers)+len(payload)+4)
	binary.BigEndian.PutUint32(out[:4], uint32(len(out)))
	binary.BigEndian.PutUint32(out[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(out[8:12], crc32.ChecksumIEEE(out[:8]))
	copy(out[12:], headers)
	copy(out[12+len(headers):], payload)
	binary.BigEndian.PutUint32(out[len(out)-4:], crc32.ChecksumIEEE(out[:len(out)-4]))
	return out
}

func usageFrames(input, output int64) []byte {
	a := eventFrame([]byte(fmt.Sprintf(`{"type":"message_start","message":{"usage":{"input_tokens":%d,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`, input)))
	return append(a, eventFrame([]byte(fmt.Sprintf(`{"type":"message_delta","usage":{"output_tokens":%d}}`, output)))...)
}

type brokerTransport func(*http.Request) (*http.Response, error)

func (f brokerTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The same production handler and proxy run with a local, in-memory fake broker.
// The socket integration tests use fakeBroker and never reach the real broker.
func budgetRelay(t *testing.T, budget float64, broker brokerTransport) *relay {
	t.Helper()
	target, _ := url.Parse("http://127.0.0.1:4171/fixture")
	r := newRelay(target, budget, broker)
	t.Cleanup(r.close)
	return r
}

func brokerResponse(req *http.Request, body []byte) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/vnd.amazon.eventstream"}},
		Body: io.NopCloser(bytes.NewReader(body)), Request: req}
}

func relayRequest(r *relay, body io.Reader, action string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", r.prefix+"/model/global.anthropic.claude-opus-5-5/"+action, body)
	rec := httptest.NewRecorder()
	r.srv.Handler.ServeHTTP(rec, req)
	return rec
}

func TestRelayChargesDirectRequestsAndStopsAtItsAllowance(t *testing.T) {
	wire := usageFrames(1000, 1000) // $0.024
	var calls atomic.Int64
	r := budgetRelay(t, .045, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return brokerResponse(req, wire), nil
	})
	for i := 0; i < 2; i++ {
		rec := relayRequest(r, strings.NewReader(textRequestBody(1000, 1000)), "invoke-with-response-stream")
		if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), wire) {
			t.Fatalf("under-budget request changed or refused: %d", rec.Code)
		}
	}
	if rec := relayRequest(r, strings.NewReader(textRequestBody(1000, 1000)), "invoke-with-response-stream"); rec.Code != 429 {
		t.Fatalf("extra direct request admitted: %d", rec.Code)
	}
	cost, admitted, refused, charged := r.accounting()
	if math.Abs(cost-.048) > 1e-10 || admitted != 2 || refused != 1 || charged != 2 || calls.Load() != 2 {
		t.Fatalf("accounting: %.8f %d %d %d", cost, admitted, refused, charged)
	}
}

func TestRelayNeverRefusesANormalRunUnderBudget(t *testing.T) {
	r := budgetRelay(t, 1, func(req *http.Request) (*http.Response, error) {
		return brokerResponse(req, usageFrames(1000, 100)), nil
	})
	for i := 0; i < 10; i++ {
		if rec := relayRequest(r, strings.NewReader(textRequestBody(1000, 1000)), "invoke-with-response-stream"); rec.Code != 200 {
			t.Fatalf("normal run refused: %d %s", rec.Code, rec.Body.String())
		}
	}
	cost, _, refused, charged := r.accounting()
	if math.Abs(cost-.06) > 1e-10 || refused != 0 || charged != 10 {
		t.Fatalf("accounting: %f %d %d", cost, refused, charged)
	}
}

func TestRelayChargesReservationWhenUsageIsUnparseable(t *testing.T) {
	body := textRequestBody(10, 1000)
	for name, wire := range map[string][]byte{"invalid": []byte("marker-7f3a"), "partial": eventFrame([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":0,"output_tokens":0}}}`))} {
		t.Run(name, func(t *testing.T) {
			r := budgetRelay(t, 1, func(req *http.Request) (*http.Response, error) { return brokerResponse(req, wire), nil })
			relayRequest(r, strings.NewReader(body), "invoke-with-response-stream")
			cost, _, _, charged := r.accounting()
			want := .02 + float64(len(body))*8/1e6
			if math.Abs(cost-want) > 1e-10 || charged != 1 {
				t.Fatalf("uncertain request charged %f, want %f", cost, want)
			}
		})
	}
}

func TestRelayBoundsBodiesAndRequestsInFlight(t *testing.T) {
	started := make(chan struct{}, relayInFlight)
	release := make(chan struct{})
	var active, highest atomic.Int64
	r := budgetRelay(t, 100, func(req *http.Request) (*http.Response, error) {
		n := active.Add(1)
		for old := highest.Load(); n > old && !highest.CompareAndSwap(old, n); old = highest.Load() {
		}
		defer active.Add(-1)
		started <- struct{}{}
		select {
		case <-release:
		case <-req.Context().Done():
		}
		return brokerResponse(req, usageFrames(0, 0)), nil
	})
	if rec := relayRequest(r, strings.NewReader(strings.Repeat("x", relayBodyLimit+1)), "invoke"); rec.Code != 413 {
		t.Fatalf("oversized body admitted: %d", rec.Code)
	}
	var wg sync.WaitGroup
	for i := 0; i < relayInFlight; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			relayRequest(r, strings.NewReader(`{"max_tokens":1}`), "invoke-with-response-stream")
		}()
	}
	for i := 0; i < relayInFlight; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			close(release)
			wg.Wait()
			t.Fatal("requests did not enter")
		}
	}
	late := relayRequest(r, strings.NewReader(`{"max_tokens":1}`), "invoke-with-response-stream")
	close(release)
	wg.Wait()
	if late.Code != 429 || highest.Load() != relayInFlight {
		t.Fatalf("in-flight bound: code %d, maximum %d", late.Code, highest.Load())
	}
}

func TestUsageParserChecksFramesAndCumulativeCacheUsage(t *testing.T) {
	wire := append(eventFrame([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":2,"cache_read_input_tokens":300,"cache_creation_input_tokens":500,"cache_creation":{"ephemeral_5m_input_tokens":200,"ephemeral_1h_input_tokens":300}}}}`)),
		eventFrame([]byte(`{"type":"message_delta","usage":{"output_tokens":10}}`))...)
	p := usageParser{stream: true}
	for _, b := range wire {
		p.feed([]byte{b})
	}
	p.eof = true
	cost, ok := p.cost(relayRates["global.anthropic.claude-opus-5-5"])
	if !ok || math.Abs(cost-.00406) > 1e-10 {
		t.Fatalf("cache usage: %f %v", cost, ok)
	}
	wire[len(wire)-1] ^= 1
	p = usageParser{stream: true}
	p.feed(wire)
	p.eof = true
	if _, ok := p.cost(relayRates["global.anthropic.claude-opus-5-5"]); ok {
		t.Fatal("accepted a corrupt frame")
	}
	p = usageParser{stream: true}
	header := make([]byte, 12)
	binary.BigEndian.PutUint32(header[:4], maxEventFrame+1)
	binary.BigEndian.PutUint32(header[8:], crc32.ChecksumIEEE(header[:8]))
	p.feed(header)
	if !p.bad || len(p.buf) != 0 {
		t.Fatal("oversized frame retained")
	}
}

func TestRelayPricesANonStreamingResponse(t *testing.T) {
	r := budgetRelay(t, 1, func(req *http.Request) (*http.Response, error) {
		return brokerResponse(req, []byte(`{"usage":{"input_tokens":1000,"output_tokens":100}}`)), nil
	})
	if rec := relayRequest(r, strings.NewReader(textRequestBody(1000, 1000)), "invoke"); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	cost, _, _, _ := r.accounting()
	if math.Abs(cost-.006) > 1e-10 {
		t.Fatal(cost)
	}
}

func TestRelayEndsWaitingRequestsOnCancellation(t *testing.T) {
	r := budgetRelay(t, 1, func(req *http.Request) (*http.Response, error) { return brokerResponse(req, nil), nil })
	for i := 0; i < relayInFlight; i++ {
		r.slots <- struct{}{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("POST", r.prefix+"/model/global.anthropic.claude-opus-5-5/invoke", strings.NewReader(`{"max_tokens":1}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	r.srv.Handler.ServeHTTP(rec, req)
	if rec.Code != 429 {
		t.Fatal(rec.Code)
	}
	for i := 0; i < relayInFlight; i++ {
		<-r.slots
	}
}

func TestSocketRelayAccountsForDirectRequestsToAFakeBroker(t *testing.T) {
	b := fakeBroker(t)
	b.mu.Lock()
	b.reply = usageFrames(1000, 1000)
	wire := append([]byte{}, b.reply...)
	b.mu.Unlock()
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		t.Setenv("XDG_RUNTIME_DIR", tmp)
	}
	socket := filepath.Join(shortScratch(t), "route.sock")
	r, _, err := b.Bedrock.open(socket, .045)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer tr.CloseIdleConnections()
	client := http.Client{Transport: tr, Timeout: 5 * time.Second}
	for i, want := range []int{200, 200, 429} {
		resp, err := client.Post("http://127.0.0.1"+r.prefix+"/model/global.anthropic.claude-opus-5-5/invoke-with-response-stream",
			"application/json", strings.NewReader(textRequestBody(1000, 1000)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != want || (i < 2 && !bytes.Equal(got, wire)) {
			t.Fatalf("direct request %d: code %d, %v", i, resp.StatusCode, err)
		}
	}
	r.close()
	cost, admitted, refused, charged := r.accounting()
	if math.Abs(cost-.048) > 1e-10 || admitted != 2 || refused != 1 || charged != 2 {
		t.Fatalf("%f %d %d %d", cost, admitted, refused, charged)
	}
}

// Repeated text gives the synthetic provider's reported input a possible source.
func textRequestBody(input, output int) string {
	body, _ := json.Marshal(map[string]any{"anthropic_version": "bedrock-2023-05-31", "max_tokens": output,
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "text", "text": strings.Repeat("x ", input)}}}}})
	return string(body)
}

// An error answer of the broker (throttling) is neither charged nor a turn of the run's; a storm of them is bounded by
// their count.
func TestBrokerErrorAnswersAreNeitherChargedNorTurns(t *testing.T) {
	var calls atomic.Int64
	r := budgetRelay(t, 1, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		resp := brokerResponse(req, []byte(`{"message":"Too many requests"}`))
		resp.StatusCode = 429
		return resp, nil
	})
	r.turnLimit = 3
	for i := 0; i < relayErrorLimit; i++ {
		if rec := relayRequest(r, strings.NewReader(textRequestBody(10, 1000)), "invoke-with-response-stream"); rec.Code != 429 {
			t.Fatalf("answer %d: %d", i, rec.Code)
		}
	}
	spent, admitted, _, _ := r.accounting()
	if spent != 0 || admitted != 0 || calls.Load() != relayErrorLimit {
		t.Fatalf("error answers were charged or counted as turns: spent %v, admitted %d, calls %d", spent, admitted, calls.Load())
	}
	if rec := relayRequest(r, strings.NewReader(textRequestBody(10, 1000)), "invoke-with-response-stream"); rec.Code != 429 || calls.Load() != relayErrorLimit {
		t.Fatalf("a storm of errors went on to the broker: %d, %d calls", rec.Code, calls.Load())
	}
}

func TestABrokerThatDoesNotAnswerIsAnErrorAnswerToo(t *testing.T) {
	var calls atomic.Int64
	r := budgetRelay(t, 1, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	})
	r.turnLimit = 3
	for i := 0; i < relayErrorLimit; i++ {
		if rec := relayRequest(r, strings.NewReader(textRequestBody(10, 1000)), "invoke-with-response-stream"); rec.Code != http.StatusBadGateway {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	spent, admitted, _, _ := r.accounting()
	if spent != 0 || admitted != 0 || calls.Load() != relayErrorLimit {
		t.Fatalf("failed transports were charged or counted as turns: spent %v, admitted %d, calls %d", spent, admitted, calls.Load())
	}
	if rec := relayRequest(r, strings.NewReader(textRequestBody(10, 1000)), "invoke-with-response-stream"); rec.Code != 429 || calls.Load() != relayErrorLimit {
		t.Fatalf("a storm of failed transports went on to the broker: %d, %d calls", rec.Code, calls.Load())
	}
}

// A reset after the request was sent may have been billed: the reservation is charged, and it is no free error answer.
func TestAResetAfterTheRequestWasSentIsCharged(t *testing.T) {
	r := budgetRelay(t, 1, func(req *http.Request) (*http.Response, error) {
		return nil, &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	})
	r.turnLimit = 3
	if rec := relayRequest(r, strings.NewReader(textRequestBody(10, 1000)), "invoke-with-response-stream"); rec.Code != http.StatusBadGateway {
		t.Fatalf("a reset answered %d", rec.Code)
	}
	spent, admitted, _, _ := r.accounting()
	if spent <= 0 || admitted != 1 || r.errorAnswers() != 0 {
		t.Fatalf("a reset was free: spent %v, admitted %d, error answers %d", spent, admitted, r.errorAnswers())
	}
}

// The reservation reads max_tokens by its exact key, once: a body that holds it twice, or in another case, would be
// priced at one value and forwarded with another, so it is refused.
func TestARequestWithAmbiguousMaxTokensIsRefused(t *testing.T) {
	var calls atomic.Int64
	r := budgetRelay(t, 1, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return brokerResponse(req, []byte(`{}`)), nil
	})
	for _, body := range []string{
		`{"max_tokens":128000,"Max_Tokens":1,"messages":[]}`,
		`{"max_tokens":128000,"max_tokens":1,"messages":[]}`,
		`{"MAX_TOKENS":1,"messages":[]}`,
		`{"max_tokens":"1","messages":[]}`,
		`{"max_tokens":1,"messages":[]} {"x":1}`,
	} {
		if rec := relayRequest(r, strings.NewReader(body), "invoke-with-response-stream"); rec.Code != 400 {
			t.Errorf("%s answered %d", body, rec.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("an ambiguous body reached the broker %d times", calls.Load())
	}
	if rec := relayRequest(r, strings.NewReader(textRequestBody(10, 1000)), "invoke-with-response-stream"); rec.Code == 400 {
		t.Fatalf("an honest body was refused: %d", rec.Code)
	}
}
