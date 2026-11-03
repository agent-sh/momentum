package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// broker is a Bedrock broker for a test: a loopback server under a path of its own (the real one's nonce), which
// answers every request and keeps its paths, and its env file, written as the real broker writes it.
type broker struct {
	*Bedrock
	url   string
	mu    sync.Mutex
	paths []string
	reply []byte
}

func fakeBroker(t *testing.T) *broker {
	t.Helper()
	b := &broker{}
	nonce := "/n" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("fake broker cannot listen in this sandbox: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b.mu.Lock()
		b.paths = append(b.paths, req.Method+" "+req.URL.Path)
		reply := append([]byte{}, b.reply...)
		b.mu.Unlock()
		if !strings.HasPrefix(req.URL.Path, nonce+"/") {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		var fixture struct {
			Text    string          `json:"fixture_report"`
			Report  json.RawMessage `json:"-"`
			Refusal bool            `json:"fixture_refusal"`
			Output  int             `json:"fixture_output_tokens"`
			NoUsage bool            `json:"fixture_no_usage"`
		}
		json.NewDecoder(req.Body).Decode(&fixture)
		fixture.Report = json.RawMessage(fixture.Text)
		if len(fixture.Report) == 0 {
			var result struct {
				Report json.RawMessage `json:"structured_output"`
			}
			json.Unmarshal([]byte(okResult), &result)
			fixture.Report = result.Report
		}
		if strings.HasSuffix(req.URL.Path, "/invoke-with-response-stream") {
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			if len(reply) > 0 {
				w.Write(reply)
				return
			}
			start, outputTokens := `{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":0}}}`, 100
			if fixture.NoUsage {
				start = `{"type":"message_start","message":{}}`
			}
			if fixture.Output > 0 {
				outputTokens = fixture.Output
			}
			w.Write(eventFrame([]byte(start)))
			if string(fixture.Report) != "null" {
				event, _ := json.Marshal(map[string]any{"type": "content_block_start", "index": 0,
					"content_block": map[string]any{"type": "tool_use", "name": "StructuredOutput", "input": fixture.Report}})
				w.Write(eventFrame(event))
				w.Write(eventFrame([]byte(`{"type":"content_block_stop","index":0}`)))
			}
			reason := "tool_use"
			if fixture.Refusal {
				reason = "refusal"
			}
			delta := `{"type":"message_delta","delta":{"stop_reason":"` + reason + `"},"usage":{"output_tokens":` + fmt.Sprint(outputTokens) + `}}`
			if fixture.NoUsage {
				delta = `{"type":"message_delta","delta":{"stop_reason":"` + reason + `"}}`
			}
			w.Write(eventFrame([]byte(delta)))
		} else {
			content := []any{}
			if string(fixture.Report) != "null" {
				content = append(content, map[string]any{"type": "tool_use", "name": "StructuredOutput", "input": fixture.Report})
			}
			reply := map[string]any{"content": content}
			if !fixture.NoUsage {
				reply["usage"] = map[string]int{"input_tokens": 10, "output_tokens": max(100, fixture.Output)}
			}
			json.NewEncoder(w).Encode(reply)
		}
	}))
	srv.Listener.Close()
	srv.Listener = listener
	srv.Start()
	t.Cleanup(srv.Close)
	b.url = srv.URL + nonce
	file := filepath.Join(t.TempDir(), "bedrock.env")
	write(t, file, `# Claude Code Bedrock routing
CLAUDE_CODE_USE_BEDROCK="1"
ANTHROPIC_DEFAULT_MODEL="global.anthropic.claude-opus-5-5[1m]"
ANTHROPIC_DEFAULT_OPUS_MODEL="global.anthropic.claude-opus-5-5[1m]"
ANTHROPIC_DEFAULT_SONNET_MODEL="global.anthropic.claude-sonnet-5-5[1m]"
ANTHROPIC_BEDROCK_BASE_URL="`+b.url+`"
CLAUDE_CODE_SKIP_BEDROCK_AUTH="1"
`)
	b.Bedrock = &Bedrock{File: file}
	return b
}

// testBedrock is a route for a test whose fake harness sends nothing to it.
func testBedrock(t *testing.T) *Bedrock { return fakeBroker(t).Bedrock }

func (b *broker) seen() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string{}, b.paths...)
}

// harnessOnRoute is a fake harness that sends its requests through its route, as the real one does: each line of
// requests is a method and a path under the route's URL; it records each answer's status and its environment in
// its own directory, then prints its result.
func harnessOnRoute(t *testing.T, result string, requests ...string) []string {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	dir := t.TempDir()
	body := maskedEnv() + " > /opt/fixture/env\n"
	for _, r := range requests {
		method, path, _ := strings.Cut(r, " ")
		body += `curl -s -o /dev/null -w '%{http_code}\n' -X ` + method + ` -d '{"max_tokens":1000}' "$ANTHROPIC_BEDROCK_BASE_URL` + path + `" >> ` + "/opt/fixture/codes" + "\n"
	}
	body += "echo '" + result + "'"
	return []string{script(t, dir, "harness", body)}
}

func codes(t *testing.T, runner []string) []string {
	raw, _ := os.ReadFile(filepath.Join(filepath.Dir(runner[0]), "codes"))
	return strings.Fields(string(raw))
}

// A run reaches the broker only through its relay, from inside its sandbox: the harness has the file's switch, auth
// and model pins, and the relay's port and path as the broker's URL, never the broker's own.
func TestARunGoesThroughItsRelayToTheBroker(t *testing.T) {
	b := fakeBroker(t)
	harness := harnessOnRoute(t, okResult, "POST /model/global.anthropic.claude-opus-5-5/invoke-with-response-stream", "GET /inference-profiles")
	a := testSandboxAgent(t, Agent{Runner: harness, Model: "global.anthropic.claude-opus-5-5", Bedrock: b.Bedrock})
	res, err := a.Run(context.Background(), Invocation{Dir: filepath.Join(shortScratch(t), "run"), Schema: investigationSchema, MaxTurns: 3, BudgetUSD: 1, Timeout: time.Minute})
	if err != nil || res.Status != "ok" {
		t.Fatalf("%+v %v", res, err)
	}
	if got := codes(t, harness); strings.Join(got, " ") != "200 200" {
		t.Fatalf("the relay answered %v", got)
	}
	seen := b.seen()
	if len(seen) != 2 || !strings.HasSuffix(seen[0], "/model/global.anthropic.claude-opus-5-5/invoke-with-response-stream") ||
		!strings.HasSuffix(seen[1], "/inference-profiles") || !strings.HasPrefix(seen[0], "POST /n") {
		t.Fatalf("the broker saw %q", seen)
	}
	raw, _ := os.ReadFile(filepath.Join(filepath.Dir(harness[0]), "env"))
	env := string(raw)
	for _, want := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_SKIP_BEDROCK_AUTH", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_BEDROCK_BASE_URL"} {
		if !strings.Contains(env, want) {
			t.Errorf("the harness lacks variable name %s", want)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(env), "\n") {
		if strings.Contains(line, "=") {
			t.Fatal("environment probe exposed a value")
		}
	}

}

// The relay forwards only a model request of a Claude model on a global inference profile, and the harness's
// listing of the profiles: a regional profile, an alias, an ARN, a path outside the run's own and any other request
// are refused at the relay and never reach the broker; the run says what was refused.
func TestTheRelayForwardsOnlyGlobalProfileModelRequests(t *testing.T) {
	b := fakeBroker(t)
	harness := harnessOnRoute(t, `{"type":"result","subtype":"success","is_error":true,"result":"API Error: 403"}`,
		"POST /model/us.anthropic.claude-haiku-4-5-20251001-v1:0/invoke",
		"POST /model/claude-sonnet-5-5/invoke",
		"POST /model/arn%3Aaws%3Abedrock%3Aus-east-1%3A1%3Ainference-profile%2Fus.anthropic.claude-opus-5-5/invoke",
		"POST /model/global.anthropic.claude-opus-5-5/invoke/extra",
		"GET /model/global.anthropic.claude-opus-5-5/invoke",
		"POST /foundation-models",
		"POST /model/global.anthropic.claude-sonnet-5-5/invoke")
	a := testSandboxAgent(t, Agent{Runner: harness, Model: "global.anthropic.claude-opus-5-5", Bedrock: b.Bedrock})
	res, err := a.Run(context.Background(), Invocation{Dir: filepath.Join(shortScratch(t), "run"), Schema: investigationSchema, MaxTurns: 3, BudgetUSD: 1, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(codes(t, harness), " "); got != "403 403 403 403 403 403 200" {
		t.Fatalf("the relay answered %s", got)
	}
	if seen := b.seen(); len(seen) != 1 || !strings.HasSuffix(seen[0], "/model/global.anthropic.claude-sonnet-5-5/invoke") {
		t.Fatalf("the broker saw %q", seen)
	}
	// what the relay refused is the result's own field, never part of the failure the fallback and the stop rule compare
	if res.Status == "ok" || !strings.Contains(strings.Join(res.Refused, "\n"), "us.anthropic.claude-haiku") || strings.Contains(res.Failure, "relay") ||
		strings.Contains(res.Failure, "haiku") {
		t.Fatalf("the run's failure and refusals: %+v", res)
	}
}

// A run's harness reaches nothing but its relay: not the broker itself, by the URL its file names, and not any other
// listener of the machine's.
func TestARunReachesNoListenerButItsRelay(t *testing.T) {
	b := fakeBroker(t)
	dir := t.TempDir()
	p := script(t, dir, "harness", `curl -s -m 5 -o /dev/null -w '%{http_code}\n' -X POST "`+b.url+`/model/global.anthropic.claude-opus-5-5/invoke" > `+"/opt/fixture/codes"+` || echo refused >> `+"/opt/fixture/codes"+`
echo '`+okResult+`'`)
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	outside, _ := exec.Command("curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "-X", "POST", b.url+"/model/global.anthropic.claude-opus-5-5/invoke").Output()
	if string(outside) != "200" {
		t.Fatalf("outside the sandbox the broker answers: %q", outside)
	}
	before := len(b.seen())
	a := testSandboxAgent(t, Agent{Runner: []string{p}, Model: "global.anthropic.claude-opus-5-5", Bedrock: b.Bedrock})
	if _, err := a.Run(context.Background(), Invocation{Dir: filepath.Join(shortScratch(t), "run"), Schema: investigationSchema, MaxTurns: 3, BudgetUSD: 1, Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(codes(t, []string{p}), " "); !strings.Contains(got, "refused") && !strings.HasPrefix(got, "000") {
		t.Fatalf("inside, the broker's own URL answered: %s", got)
	}
	if len(b.seen()) != before {
		t.Fatalf("the broker saw the sandbox: %q", b.seen())
	}
}

// No route, no run: no Bedrock settings, a file that does not turn Bedrock on, leaves auth to the run, or names no
// loopback broker, and the harness never starts. What the refusal says never names the broker's URL.
func TestARunWithNoBedrockRouteDoesNotStart(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	p := script(t, dir, "harness", `touch `+started+`
echo '`+okResult+`'`)
	b := fakeBroker(t)
	raw, _ := os.ReadFile(b.File)
	for name, route := range map[string]*Bedrock{
		"none":        nil,
		"no file":     {File: filepath.Join(dir, "absent.env")},
		"not bedrock": {File: rewritten(t, raw, `CLAUDE_CODE_USE_BEDROCK="1"`, `CLAUDE_CODE_USE_BEDROCK="0"`)},
		"own auth":    {File: rewritten(t, raw, `CLAUDE_CODE_SKIP_BEDROCK_AUTH="1"`, ``)},
		"not local":   {File: rewritten(t, raw, b.url, "https://bedrock-runtime.example.invalid")},
	} {
		a := testSandboxAgent(t, Agent{Runner: []string{p}, Model: "global.anthropic.claude-opus-5-5", Bedrock: route})
		if route == nil {
			a.Bedrock = nil
		}
		_, err := a.Run(context.Background(), Invocation{Dir: filepath.Join(shortScratch(t), name), Schema: investigationSchema, MaxTurns: 3, BudgetUSD: 1, Timeout: time.Minute})
		if !errors.Is(err, ErrNoBedrock) {
			t.Errorf("%s: %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), b.url) {
			t.Errorf("%s: the refusal names the broker's URL: %v", name, err)
		}
	}
	if _, err := os.Stat(started); err == nil {
		t.Fatal("the harness started with no route")
	}
}

func rewritten(t *testing.T, raw []byte, from, to string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bedrock.env")
	write(t, p, strings.Replace(string(raw), from, to, 1))
	return p
}
