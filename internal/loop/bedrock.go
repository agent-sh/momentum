package loop

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/agent-sh/momentum/internal/paths"
)

// Bedrock is the route every headless run takes: the loopback Bedrock broker, whose env file names its URL. The
// file is read at each launch, never cached: the broker writes it again when it restarts, with a new nonce in its
// URL's path. A run with no route does not start, and nothing falls back to the direct API. A run goes through a
// relay of its own in front of the broker, which counts the model requests it forwards: a run whose stream shows the
// model ran while no request reached the broker did not run on Bedrock, and fails.
type Bedrock struct {
	File string // the broker's env file; empty: ~/.config/claude-code/bedrock.env
}

// ErrNoBedrock is a run that did not start, or does not count, because it had no Bedrock route.
var ErrNoBedrock = errors.New("no Bedrock route")

// bedrockKeys are the variables a run takes from the broker's file: the switch to Bedrock, the broker's URL, the
// broker's own auth, and the model pins.
var bedrockKeys = []string{"CLAUDE_CODE_USE_BEDROCK", "ANTHROPIC_BEDROCK_BASE_URL", "CLAUDE_CODE_SKIP_BEDROCK_AUTH",
	"ANTHROPIC_DEFAULT_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL"}

// otherRoutes are the variables that would send a run elsewhere, which a run never takes from its caller.
var otherRoutes = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_OAUTH_TOKEN", "AWS_BEARER_TOKEN_BEDROCK", "ANTHROPIC_BEDROCK_BASE_URL",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_SKIP_BEDROCK_AUTH"}

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
		return nil, fmt.Errorf("%w: the broker's env file cannot be read (%s)", ErrNoBedrock, errors.Unwrap(err))
	}
	defer f.Close()
	vars := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimPrefix(line, "export ")
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

// relay is one run's way to the broker: a loopback server of its own, under a path of its own, that forwards to
// the broker and counts the model requests it forwards.
type relay struct {
	srv    *http.Server
	url    string
	models atomic.Int64
}

// open reads the broker's file and starts the run's relay; env is what the run's harness takes: the file's
// variables, with the relay as the broker's URL.
func (b *Bedrock) open() (*relay, []string, error) {
	vars, err := b.read()
	if err != nil {
		return nil, nil, err
	}
	target, _ := url.Parse(vars["ANTHROPIC_BEDROCK_BASE_URL"])
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("%w: the run's relay cannot listen: %v", ErrNoBedrock, err)
	}
	prefix := "/" + strings.ToLower(rand.Text())
	r := &relay{url: "http://" + ln.Addr().String() + prefix}
	base := strings.TrimSuffix(target.Path, "/")
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			rest := strings.TrimPrefix(pr.In.URL.Path, prefix)
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = target.Scheme, target.Host, target.Host
			pr.Out.URL.Path, pr.Out.URL.RawPath = base+rest, ""
		},
		FlushInterval: -1, // a streamed answer streams on
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "the Bedrock broker did not answer", http.StatusBadGateway)
		},
	}
	r.srv = &http.Server{ErrorLog: log.New(io.Discard, "", 0), Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rest, ok := strings.CutPrefix(req.URL.Path, prefix)
		if !ok || rest != "" && rest[0] != '/' {
			http.NotFound(w, req)
			return
		}
		if req.Method == http.MethodPost && strings.HasPrefix(rest, "/model/") {
			r.models.Add(1)
		}
		proxy.ServeHTTP(w, req)
	})}
	go r.srv.Serve(ln)
	var env []string
	for _, k := range bedrockKeys {
		switch v, ok := vars[k]; {
		case k == "ANTHROPIC_BEDROCK_BASE_URL":
			env = append(env, k+"="+r.url)
		case ok:
			env = append(env, k+"="+v)
		}
	}
	return r, env, nil
}

func (r *relay) close() { r.srv.Close() }

// routed is a run's environment on its route: the caller's, without any variable that would send it elsewhere,
// and the route's.
func routed(env, route []string) []string {
	if env == nil {
		env = CleanEnv(nil)
	}
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(otherRoutes, k) && !slices.Contains(bedrockKeys, k) {
			out = append(out, kv)
		}
	}
	return append(out, route...)
}

// BedrockModel says whether a model names a Claude model on Bedrock: a global inference profile, or an alias the
// broker's file pins to one.
func BedrockModel(m string) bool {
	m = strings.TrimSuffix(m, "[1m]")
	return strings.HasPrefix(m, "global.anthropic.claude-") || slices.Contains([]string{"opus", "sonnet", "haiku", "fable"}, m)
}

// modelRan says whether a run's stream shows the model answered: a turn, a token or a cost.
func modelRan(res Result) bool {
	return res.TurnsKnown && res.Turns > 0 || res.Tokens > 0 || res.CostKnown && res.USD > 0
}
