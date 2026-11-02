package loop

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
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
}

func fakeBroker(t *testing.T) *broker {
	t.Helper()
	b := &broker{}
	nonce := "/n" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b.mu.Lock()
		b.paths = append(b.paths, req.Method+" "+req.URL.Path)
		b.mu.Unlock()
		if !strings.HasPrefix(req.URL.Path, nonce+"/") {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		w.Write([]byte("{}"))
	}))
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

// testBedrock is a route for a test's fake harness, which reaches its broker by the line script puts first in it.
func testBedrock(t *testing.T) *Bedrock { return fakeBroker(t).Bedrock }

func (b *broker) seen() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string{}, b.paths...)
}

// unrouted is a fake harness that answers without sending anything to the route it was given, as one on another
// provider would: script would put the request first, so it is written by hand.
func unrouted(t *testing.T, result string) []string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "elsewhere")
	write(t, p, "#!/bin/sh\necho '"+result+"'\n")
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{p}
}

// A run goes to the broker the file names, through a relay of its own, with the file's switch, auth and model pins,
// and nothing of the caller's that would send it elsewhere.
func TestARunGoesThroughTheBedrockBroker(t *testing.T) {
	b := fakeBroker(t)
	dir := t.TempDir()
	envOut := filepath.Join(dir, "env")
	p := script(t, dir, "harness", `env > `+envOut+`
echo '`+okResult+`'`)
	a := Agent{Runner: []string{p}, Model: "global.anthropic.claude-opus-5-5", Bedrock: b.Bedrock,
		Env: []string{"HOME=" + dir, "ANTHROPIC_API_KEY=sk-direct", "ANTHROPIC_BASE_URL=https://api.example.invalid", "CLAUDE_CODE_USE_BEDROCK=0"}}
	res, err := a.Run(context.Background(), Invocation{Dir: filepath.Join(dir, "run"), Schema: runSchema, MaxTurns: 3, BudgetUSD: 1, Timeout: time.Minute})
	if err != nil || res.Status != "ok" {
		t.Fatalf("%+v %v", res, err)
	}
	seen := b.seen()
	if len(seen) != 1 || !strings.HasPrefix(seen[0], "POST /n") || !strings.HasSuffix(seen[0], "/model/global.anthropic.claude-opus-5-5/invoke") {
		t.Fatalf("the broker saw %q", seen)
	}
	raw, _ := os.ReadFile(envOut)
	env := string(raw)
	for _, want := range []string{"CLAUDE_CODE_USE_BEDROCK=1\n", "CLAUDE_CODE_SKIP_BEDROCK_AUTH=1\n", "ANTHROPIC_DEFAULT_OPUS_MODEL=global.anthropic.claude-opus-5-5[1m]\n", "ANTHROPIC_BEDROCK_BASE_URL=http://127.0.0.1:"} {
		if !strings.Contains(env, want) {
			t.Errorf("the harness lacks %q", want)
		}
	}
	for _, not := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL=", "CLAUDE_CODE_USE_BEDROCK=0", b.url} {
		if strings.Contains(env, not) {
			t.Errorf("the harness has %q", not)
		}
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
		"bedrock off": {File: rewrite(t, dir, "off.env", strings.Replace(string(raw), `CLAUDE_CODE_USE_BEDROCK="1"`, `CLAUDE_CODE_USE_BEDROCK="0"`, 1))},
		"own auth":    {File: rewrite(t, dir, "auth.env", strings.Replace(string(raw), `CLAUDE_CODE_SKIP_BEDROCK_AUTH="1"`, ``, 1))},
		"remote URL":  {File: rewrite(t, dir, "remote.env", strings.Replace(string(raw), "http://127.0.0.1:", "http://broker.example.invalid:", 1))},
		"no URL":      {File: rewrite(t, dir, "nourl.env", strings.Replace(string(raw), "ANTHROPIC_BEDROCK_BASE_URL", "OTHER_URL", 1))},
	} {
		a := Agent{Runner: []string{p}, Model: "global.anthropic.claude-opus-5-5", Bedrock: route}
		_, err := a.Run(context.Background(), Invocation{Dir: filepath.Join(dir, "run"), Schema: runSchema, MaxTurns: 3, BudgetUSD: 1, Timeout: time.Minute})
		if !errors.Is(err, ErrNoBedrock) {
			t.Errorf("%s: %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), b.url[strings.LastIndex(b.url, "/"):]) {
			t.Errorf("%s: the refusal names the broker's nonce: %v", name, err)
		}
		if _, statErr := os.Stat(started); statErr == nil {
			t.Fatalf("%s: the harness started", name)
		}
	}
}

func rewrite(t *testing.T, dir, name, text string) string {
	p := filepath.Join(dir, name)
	write(t, p, text)
	return p
}

// A run whose model answered while no request reached the broker was not on Bedrock: it fails, and says so.
func TestARunThatSkipsTheBrokerFails(t *testing.T) {
	b := fakeBroker(t)
	dir := t.TempDir()
	a := Agent{Runner: unrouted(t, okResult), Model: "global.anthropic.claude-opus-5-5", Bedrock: b.Bedrock}
	res, err := a.Run(context.Background(), Invocation{Dir: filepath.Join(dir, "run"), Schema: runSchema, MaxTurns: 3, BudgetUSD: 1, Timeout: time.Minute})
	if !errors.Is(err, ErrNoBedrock) || res.Status != "failed" || res.Failure != "not on Bedrock" {
		t.Fatalf("%+v %v", res, err)
	}
	if len(b.seen()) != 0 {
		t.Fatalf("the broker saw %q", b.seen())
	}
}

// The broker's file is read at each launch: when the broker restarts, with a new nonce, the next run goes to the
// new one.
func TestEachRunReadsTheBrokersFileAgain(t *testing.T) {
	first, second := fakeBroker(t), fakeBroker(t)
	dir := t.TempDir()
	p := script(t, dir, "harness", `echo '`+okResult+`'`)
	route := &Bedrock{File: filepath.Join(dir, "bedrock.env")}
	a := Agent{Runner: []string{p}, Model: "global.anthropic.claude-opus-5-5", Bedrock: route}
	for _, b := range []*broker{first, second} {
		raw, _ := os.ReadFile(b.File)
		write(t, route.File, string(raw))
		if _, err := a.Run(context.Background(), Invocation{Dir: filepath.Join(dir, "run"), Schema: runSchema, MaxTurns: 3, BudgetUSD: 1, Timeout: time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	if len(first.seen()) != 1 || len(second.seen()) != 1 {
		t.Fatalf("first %q second %q", first.seen(), second.seen())
	}
}

// The loop's models are Bedrock's: a global inference profile, or an alias the broker's file pins.
func TestTheLoopsModelsAreBedrocks(t *testing.T) {
	for m, want := range map[string]bool{"global.anthropic.claude-opus-5-5": true, "global.anthropic.claude-sonnet-5-5[1m]": true, "opus": true,
		"claude-opus-5-5": false, "us.anthropic.claude-opus-5-5": false, "anthropic.claude-opus-5-5": false, "gpt-6": false} {
		if BedrockModel(m) != want {
			t.Errorf("%s: %v", m, !want)
		}
	}
}
