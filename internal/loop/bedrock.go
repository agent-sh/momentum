package loop

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
)

// Bedrock is the route every headless run takes: the machine's loopback Bedrock broker, which holds the
// credential and whose env file names its URL. The file is read at each launch, never cached: the broker writes it
// again when it restarts, with a new nonce in its URL's path. A run's sandbox has no network but its relay, a
// server of the run's own on a Unix socket that forwards to the broker only the requests of a Claude model on
// Bedrock's global inference profiles, so a run reaches no other provider, route or region from its first request.
type Bedrock struct {
	File string // the broker's env file; empty: ~/.config/claude-code/bedrock.env
}

// ErrNoBedrock is a run that did not start because it had no Bedrock route.
var ErrNoBedrock = errors.New("no Bedrock route")

// bedrockKeys are the variables a run takes from the broker's file: the switch to Bedrock, the broker's URL, the
// broker's own auth, and the model pins.
var bedrockKeys = []string{"CLAUDE_CODE_USE_BEDROCK", "ANTHROPIC_BEDROCK_BASE_URL", "CLAUDE_CODE_SKIP_BEDROCK_AUTH",
	"ANTHROPIC_DEFAULT_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL"}

// RoutePort is the loopback port a run's harness reaches its relay by, inside its sandbox, whose loopback is its own.
const RoutePort = 4170

func (b *Bedrock) path() string {
	if b.File != "" {
		return paths.Expand(b.File)
	}
	return filepath.Join(paths.Home(), ".config", "claude-code", "bedrock.env")
}

// read reads the broker's file now: its variables, checked. What it says never names the broker's URL, whose path
// carries the nonce.
func (b *Bedrock) read() (map[string]string, error) {
	f, err := os.Open(b.path())
	if err != nil {
		return nil, fmt.Errorf("%w: the broker's env file %s cannot be read (%s)", ErrNoBedrock, b.path(), errors.Unwrap(err))
	}
	defer f.Close()
	vars := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimPrefix(strings.TrimSpace(sc.Text()), "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") || !slices.Contains(bedrockKeys, k) {
			continue
		}
		if u, err := strconv.Unquote(v); err == nil {
			v = u
		} else if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
			v = v[1 : len(v)-1]
		}
		vars[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: the broker's env file cannot be read", ErrNoBedrock)
	}
	switch {
	case vars["CLAUDE_CODE_USE_BEDROCK"] != "1":
		return nil, fmt.Errorf("%w: the broker's env file does not turn Bedrock on", ErrNoBedrock)
	case vars["CLAUDE_CODE_SKIP_BEDROCK_AUTH"] != "1":
		return nil, fmt.Errorf("%w: the broker's env file does not leave auth to the broker", ErrNoBedrock)
	}
	u, err := url.Parse(vars["ANTHROPIC_BEDROCK_BASE_URL"])
	if err != nil || u.Scheme != "http" || !loopback(u.Hostname()) || u.Port() == "" {
		return nil, fmt.Errorf("%w: the broker's env file names no loopback broker", ErrNoBedrock)
	}
	return vars, nil
}

// Check says whether a run could start on the route now, as each run checks again at its launch.
func (b *Bedrock) Check() error {
	if b == nil {
		return fmt.Errorf("%w: every headless run is on Bedrock, and none falls back to the direct API", ErrNoBedrock)
	}
	_, err := b.read()
	return err
}

func loopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}

// relay is one run's way to the broker: a server of its own on a Unix socket, under a path of its own, that
// forwards to the broker a model request of a Claude model on a global inference profile and the harness's listing of
// the inference profiles, and refuses anything else.
type relay struct {
	srv                           *http.Server
	prefix                        string
	mu                            sync.Mutex
	refused                       []string // what it refused, each kind once: shown with the run
	allowance, spent, reserved    float64
	admittedN, refusedN, chargedN int
	slots                         chan struct{}
	wg                            sync.WaitGroup
	closed                        bool
	stop                          sync.Once
}

const relayBodyLimit = 8 << 20
const relayInFlight = 8
const relayWait = 3 * time.Second

type relayRate struct {
	input, output, read, write5, write1 float64
	maxOutput                           int64
}

// Bedrock global rates, per million tokens. Source: Amazon Bedrock pricing,
// global cross-region inference, 2026-11-02. The evaluation's bedrockRates
// table uses these same values and will replace this small relay table.
// The output ceiling is the models' 128K maximum:
// https://platform.claude.com/docs/en/models/overview
var relayRates = map[string]relayRate{
	"global.anthropic.claude-opus-5-5":   {4, 20, .2, 5, 8, 128000},
	"global.anthropic.claude-sonnet-5-5": {2, 10, .1, 2.5, 4, 128000},
}

type relayCharge struct {
	reservation float64
	rate        relayRate
	parser      *usageParser
}
type relayChargeKey struct{}

// open reads the broker's file and starts the run's relay on the socket; env is what the run's harness takes
// inside its sandbox: the file's switch, auth and pins, with the relay's port and path as the broker's URL.
func (b *Bedrock) open(socket string, budget float64) (*relay, []string, error) {
	vars, err := b.read()
	if err != nil {
		return nil, nil, err
	}
	target, _ := url.Parse(vars["ANTHROPIC_BEDROCK_BASE_URL"])
	// A socket directory belongs to the host, never a run's writable mount.
	if _, err := os.Lstat(socket); err == nil {
		return nil, nil, fmt.Errorf("the relay socket already exists")
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: the run's relay cannot listen: %v", ErrNoBedrock, err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		ln.Close()
		return nil, nil, err
	}
	if budget <= 0 || budget > math.MaxFloat64/1.5 || math.IsNaN(budget) || math.IsInf(budget, 0) {
		ln.Close()
		return nil, nil, fmt.Errorf("invalid relay budget")
	}
	r := newRelay(target, budget, nil)
	go r.srv.Serve(ln)
	var env []string
	for _, k := range bedrockKeys {
		switch v, ok := vars[k]; {
		case k == "ANTHROPIC_BEDROCK_BASE_URL":
			env = append(env, k+"="+"http://127.0.0.1:"+strconv.Itoa(RoutePort)+r.prefix)
		case ok:
			env = append(env, k+"="+v)
		}
	}
	return r, env, nil
}

func newRelay(target *url.URL, budget float64, transport http.RoundTripper) *relay {
	r := &relay{prefix: "/" + strings.ToLower(rand.Text()), allowance: 1.5 * budget, slots: make(chan struct{}, relayInFlight)}
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			// the path as it came, escapes and all: a model id may hold an escaped character
			rest := strings.TrimPrefix(pr.In.URL.EscapedPath(), r.prefix)
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = target.Scheme, target.Host, target.Host
			pr.Out.URL.RawPath = strings.TrimSuffix(target.EscapedPath(), "/") + rest
			pr.Out.URL.Path, _ = url.PathUnescape(pr.Out.URL.RawPath)
		},
		FlushInterval: -1, // a streamed answer streams on
		ModifyResponse: func(resp *http.Response) error {
			if c, ok := resp.Request.Context().Value(relayChargeKey{}).(*relayCharge); ok {
				if resp.StatusCode < 200 || resp.StatusCode >= 300 {
					c.parser.bad = true
				}
				resp.Body = &usageBody{ReadCloser: resp.Body, parser: c.parser}
			}
			return nil
		},
		ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "the Bedrock broker did not answer", http.StatusBadGateway)
		},
	}
	r.srv = &http.Server{ErrorLog: log.New(io.Discard, "", 0), Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			http.Error(w, "relay closed", 429)
			return
		}
		r.wg.Add(1)
		r.mu.Unlock()
		defer r.wg.Done()
		if why := r.admit(req); why != "" {
			r.refuse(w, why, http.StatusForbidden)
			return
		}
		if req.Method == http.MethodPost {
			// Waiting peers retain no request body. Only these eight slots capture one.
			timer := time.NewTimer(relayWait)
			defer timer.Stop()
			select {
			case r.slots <- struct{}{}:
				defer func() { <-r.slots }()
			case <-timer.C:
				r.refuse(w, "eight model requests are already in flight", 429)
				return
			case <-req.Context().Done():
				r.refuse(w, "request ended while waiting", 429)
				return
			}
			req.Body = http.MaxBytesReader(w, req.Body, relayBodyLimit)
			body, err := io.ReadAll(req.Body)
			req.Body.Close()
			if err != nil {
				r.refuse(w, "request body exceeds 8 MiB or could not be read", 413)
				return
			}
			parts := strings.Split(req.URL.EscapedPath(), "/")
			model, _ := url.PathUnescape(parts[len(parts)-2])
			rate, ok := relayRates[model]
			if !ok {
				r.refuse(w, "no bounded price for this model", 403)
				return
			}
			var input struct {
				MaxTokens *int64 `json:"max_tokens"`
			}
			if json.Unmarshal(body, &input) != nil {
				r.refuse(w, "invalid model request", 400)
				return
			}
			output := rate.maxOutput
			if input.MaxTokens != nil {
				if *input.MaxTokens <= 0 || *input.MaxTokens > output {
					r.refuse(w, "invalid max_tokens", 400)
					return
				}
				output = *input.MaxTokens
			}
			reserve := (float64(output)*rate.output + float64(len(body))*rate.input/4) / 1e6
			r.mu.Lock()
			left := r.allowance - r.spent - r.reserved
			if left <= 1e-12 {
				r.mu.Unlock()
				r.refuse(w, "run dollar allowance exhausted", 429)
				return
			}
			reserve = min(reserve, left)
			r.reserved += reserve
			r.admittedN++
			r.mu.Unlock()
			c := &relayCharge{reservation: reserve, rate: rate, parser: &usageParser{stream: strings.HasSuffix(req.URL.Path, "/invoke-with-response-stream")}}
			defer func() {
				cost, ok := c.parser.cost(rate)
				if !ok {
					cost = reserve
				}
				r.mu.Lock()
				r.reserved -= reserve
				r.spent += cost
				r.chargedN++
				r.mu.Unlock()
			}()
			req = req.WithContext(context.WithValue(req.Context(), relayChargeKey{}, c))
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
		}
		proxy.ServeHTTP(w, req)
	}), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second}
	return r
}

// admit says why the relay refuses a request, or "": under the relay's own path, a POST to a Claude model on a
// global inference profile (/model/ID/ACTION), or the harness's GET of the inference profiles. A model id is read
// unescaped, as Bedrock reads it.
func (r *relay) admit(req *http.Request) string {
	rest, ok := strings.CutPrefix(req.URL.EscapedPath(), r.prefix)
	if !ok || rest == "" || rest[0] != '/' {
		return "not under the run's own path"
	}
	parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	switch {
	case req.Method == http.MethodGet && len(parts) == 1 && parts[0] == "inference-profiles":
		return ""
	case req.Method == http.MethodPost && len(parts) == 3 && parts[0] == "model":
		id, err := url.PathUnescape(parts[1])
		if err != nil || !BedrockModel(id) || strings.Contains(id, "[") {
			return fmt.Sprintf("model %q is no Claude model on a global inference profile", plainClip(id, 80))
		}
		if parts[2] == "invoke" || parts[2] == "invoke-with-response-stream" {
			return ""
		}
	}
	return fmt.Sprintf("%s %s is no model request", req.Method, plainClip("/"+strings.Join(parts, "/"), 80))
}

// refusals is what the relay refused: shown with the run, never part of its failure.
func (r *relay) refusals() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.refused...)
}

func (r *relay) refuse(w http.ResponseWriter, why string, code int) {
	r.mu.Lock()
	r.refusedN++
	if len(r.refused) < 8 && !slices.Contains(r.refused, why) {
		r.refused = append(r.refused, why)
	}
	r.mu.Unlock()
	http.Error(w, "momentum's relay refused the request: "+why, code)
}

func (r *relay) accounting() (float64, int, int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.spent, r.admittedN, r.refusedN, r.chargedN
}

func (r *relay) close() {
	r.stop.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		r.srv.Close()
		r.wg.Wait()
	})
}

// BedrockModel says whether a model names a Claude model on Bedrock as the owner's runs take it: a global inference
// profile. A regional one, or an alias the harness may resolve to one, is not. The relay holds every request to it;
// the loop's configuration is checked by it too, so a model that could never run says so before any run starts.
func BedrockModel(m string) bool {
	return strings.HasPrefix(strings.TrimSuffix(m, "[1m]"), "global.anthropic.claude-")
}
