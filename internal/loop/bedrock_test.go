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

	"github.com/agent-sh/momentum/internal/store"
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

// The loop's models are Bedrock's global inference profiles: a regional one, or an alias the harness may resolve to
// one, is not.
func TestTheLoopsModelsAreBedrocks(t *testing.T) {
	for m, want := range map[string]bool{"global.anthropic.claude-opus-5-5": true, "global.anthropic.claude-sonnet-5-5[1m]": true, "opus": false,
		"claude-opus-5-5": false, "us.anthropic.claude-opus-5-5": false, "anthropic.claude-opus-5-5": false, "gpt-6": false, "haiku": false, "fable": false} {
		if BedrockModel(m) != want {
			t.Errorf("%s: %v", m, !want)
		}
	}
}

// A run not on Bedrock does not count: its report goes into no case, and it is charged what its stream says it
// spent.
func TestALiveRunNotOnBedrockDoesNotCount(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	r := runnerFor(t, st, unrouted(t, investigatedResult))
	c := openCase(t, st, "failures", "a", 1)
	done, err := r.Once(ctx, Options{Families: []string{"failures"}})
	if err != nil || len(done) != 1 || !strings.Contains(done[0].Run.Failure, "no Bedrock route") || done[0].Run.USD != 0.75 {
		t.Fatalf("%+v %v", done, err)
	}
	if got, _ := st.CaseByID(ctx, c.ID); got.Status == store.CaseTesting || got.Mechanism != "" {
		t.Fatalf("its report went into its case: %s %q", got.Status, got.Mechanism)
	}
}

// A trial not on Bedrock does not count: no findings into its case, no judge, no grade.
func TestATrialNotOnBedrockIsNotGraded(t *testing.T) {
	e, task := evalFixture(t)
	e.Runner.Agent.Runner = unrouted(t, `{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":1,"structured_output":{"mechanism":"m","fix":"f","summary":"s"}}`)
	trs, err := e.Run(context.Background(), []GoldenTask{task})
	if err != nil || len(trs) != 2 {
		t.Fatalf("%+v %v", trs, err)
	}
	for _, tr := range trs {
		if !strings.Contains(tr.Err, "no Bedrock route") || tr.Grade.Verdict != "" || tr.JudgeDir != "" {
			t.Fatalf("%s: err %q verdict %q judge %q", tr.Arm, tr.Err, tr.Grade.Verdict, tr.JudgeDir)
		}
	}
	for _, s := range Summarize(trs, 1) {
		if s.Reached != 0 || s.AnyReached {
			t.Fatalf("counted: %+v", s)
		}
	}
}

// A run cut short with no result is checked too: a message of the model's, and no request through the route.
func TestARunCutShortOffTheRouteFails(t *testing.T) {
	b := fakeBroker(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "elsewhere")
	write(t, p, "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"assistant\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":1000,\"output_tokens\":500}}}'\nexec sleep 30\n")
	os.Chmod(p, 0o755)
	a := Agent{Runner: []string{p}, Model: "global.anthropic.claude-opus-5-5", Bedrock: b.Bedrock}
	res, err := a.Run(context.Background(), Invocation{Dir: filepath.Join(dir, "run"), Schema: runSchema, MaxTurns: 3, BudgetUSD: 1, Timeout: time.Second})
	if !errors.Is(err, ErrNoBedrock) || res.Failure != "not on Bedrock" {
		t.Fatalf("%+v %v", res, err)
	}
}

// The relay answers only under its own path, and counts only the model requests the broker answered: a request
// without its path reaches no broker, and a run whose only requests were a listing or ones the broker refused is
// not on Bedrock.
func TestTheRelayCountsOnlyAnsweredModelRequests(t *testing.T) {
	b := fakeBroker(t)
	route, env, err := b.open()
	if err != nil {
		t.Fatal(err)
	}
	defer route.close()
	base := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "ANTHROPIC_BEDROCK_BASE_URL="); ok {
			base = v
		}
	}
	host := base[:strings.LastIndex(base, "/")]
	for _, req := range []struct{ method, url string }{
		{http.MethodPost, host + "/model/global.anthropic.claude-opus-5-5/invoke"}, // no relay path
		{http.MethodGet, base + "/inference-profiles"},                             // no model request
	} {
		r, _ := http.NewRequest(req.method, req.url, strings.NewReader("{}"))
		if resp, err := http.DefaultClient.Do(r); err == nil {
			resp.Body.Close()
		}
	}
	if n := route.models.Load(); n != 0 {
		t.Fatalf("counted %d", n)
	}
	for _, p := range b.seen() {
		if strings.Contains(p, "/model/") {
			t.Fatalf("a request without the relay's path reached the broker: %q", b.seen())
		}
	}
	// the broker refuses a request under another nonce: the relay forwards it, and does not count it
	refused := &Bedrock{File: rewrite(t, t.TempDir(), "other.env", strings.Replace(mustRead(t, b.File), b.url, b.url+"-stale", 1))}
	stale, env2, err := refused.open()
	if err != nil {
		t.Fatal(err)
	}
	defer stale.close()
	for _, kv := range env2 {
		if v, ok := strings.CutPrefix(kv, "ANTHROPIC_BEDROCK_BASE_URL="); ok {
			if resp, err := http.Post(v+"/model/global.anthropic.claude-opus-5-5/invoke", "application/json", strings.NewReader("{}")); err == nil {
				resp.Body.Close()
			}
		}
	}
	if n := stale.models.Load(); n != 0 {
		t.Fatalf("a refused request counted: %d", n)
	}
}

func mustRead(t *testing.T, p string) string {
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// An escaped slash in a model id, as an ARN pin has, reaches the broker escaped.
func TestTheRelayKeepsAnEscapedPath(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got = req.URL.EscapedPath()
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	file := rewrite(t, t.TempDir(), "bedrock.env", "CLAUDE_CODE_USE_BEDROCK=\"1\"\nCLAUDE_CODE_SKIP_BEDROCK_AUTH=\"1\"\nANTHROPIC_BEDROCK_BASE_URL=\""+srv.URL+"/n0nce\"\n")
	route, env, err := (&Bedrock{File: file}).open()
	if err != nil {
		t.Fatal(err)
	}
	defer route.close()
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "ANTHROPIC_BEDROCK_BASE_URL="); ok {
			resp, err := http.Post(v+"/model/arn%3Aaws%3Abedrock%3Aus-east-1%3A1%3Aapplication-inference-profile%2Fabc/invoke", "application/json", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
		}
	}
	if !strings.Contains(got, "profile%2Fabc/invoke") || !strings.HasPrefix(got, "/n0nce/model/") {
		t.Fatalf("the broker saw %q", got)
	}
}

// An evaluation whose trials or judge have no route does not start.
func TestAnEvaluationWithNoRouteForItsJudgeDoesNotStart(t *testing.T) {
	e, task := evalFixture(t)
	e.Judge.Bedrock = nil
	if _, err := e.Run(context.Background(), []GoldenTask{task}); !errors.Is(err, ErrNoBedrock) {
		t.Fatalf("%v", err)
	}
	e, task = evalFixture(t)
	e.Runner.Agent.Bedrock = &Bedrock{File: filepath.Join(t.TempDir(), "absent.env")}
	if _, err := e.Run(context.Background(), []GoldenTask{task}); !errors.Is(err, ErrNoBedrock) {
		t.Fatalf("%v", err)
	}
}

// A process started outside a run has no route: the tests' environment holds none of a broker's variables.
func TestNoRouteOutsideARun(t *testing.T) {
	for _, k := range bedrockKeys {
		if _, ok := os.LookupEnv(k); ok {
			t.Fatalf("%s is set outside a run", k)
		}
	}
}
