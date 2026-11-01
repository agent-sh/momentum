package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/capacity"
	"github.com/agent-sh/momentum/sessions"
)

func TestCapacityWithoutItsSectionOrWithABadRuleIsAUsageError(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", cfg)
	for _, c := range []struct {
		conf string
		args []string
		want string
	}{
		{"", []string{"capacity"}, "no capacity section"},
		{`{"radar": {"path": "/x"}}`, []string{"capacity"}, "no capacity section"},
		{`{"capacity": {"live": ["model"]}}`, []string{"capacity"}, `"model", which is not a rule`},
		{`{"capacity": {}}`, []string{"capacity", "--rule", "disks"}, `"disks", which is not a rule`},
	} {
		if c.conf != "" {
			if err := os.WriteFile(filepath.Join(cfg, "sources.json"), []byte(c.conf), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		var out, errOut bytes.Buffer
		if code := run(context.Background(), c.args, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), c.want) {
			t.Errorf("%s %v: exit %d, stderr %q; want 2 and %q", c.conf, c.args, code, errOut.String(), c.want)
		}
	}
}

// noJobs is a machine with no scheduled jobs, for a test that must not read this machine's.
type noJobs struct{}

func (noJobs) Units(context.Context) ([]capacity.JobText, []string, error) { return nil, nil, nil }
func (noJobs) Crontab(context.Context) (string, error)                     { return "", nil }

// noTools is a machine without uv or Docker.
type noTools struct{}

func (noTools) UV(context.Context, time.Duration, ...string) (string, error) {
	return "", errors.New("uv: not installed")
}
func (noTools) Docker(context.Context, time.Duration, ...string) (string, error) {
	return "", errors.New("docker: not installed")
}

func TestACapacityDryRunRecordsNothing(t *testing.T) {
	home, cfg, data := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("MOMENTUM_HOME", home)
	t.Setenv("MOMENTUM_CONFIG", cfg)
	capacityJobs, capacityProc, capacityTools = noJobs{}, t.TempDir(), noTools{} // nothing of this machine's
	t.Cleanup(func() { capacityJobs, capacityProc, capacityTools = nil, "", nil })
	conf := `{"capacity": {"disks": ["` + data + `"], "live": ["models"], "models": {"roots": ["` + data + `"]}}}`
	if err := os.WriteFile(filepath.Join(cfg, "sources.json"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"capacity", "--dry-run", "--rule", "models", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var res struct {
		DryRun bool `json:"dry_run"`
		Disks  []struct {
			Disk string `json:"disk"`
			Size int64  `json:"size"`
		} `json:"disks"`
		Rules []struct {
			Rule string `json:"rule"`
			Live bool   `json:"live"`
			Ran  bool   `json:"ran"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	if !res.DryRun || len(res.Disks) != 1 || res.Disks[0].Size == 0 || len(res.Rules) != 1 || res.Rules[0].Live || !res.Rules[0].Ran {
		t.Fatalf("a dry run of one rule, not live: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(home, "momentum.db")); !os.IsNotExist(err) {
		t.Fatal("a dry run makes no store")
	}
}

func TestASessionLookThatIsNotWholeProtectsNothing(t *testing.T) {
	w := &sessions.World{Now: time.Now()}
	live := []*sessions.Session{{Key: "k1", SessionID: "id1", PID: 5, Tool: "claude", Cwd: "/w", Phase: sessions.Working},
		{Key: "k2", PID: 6, Tool: "codex", Phase: sessions.Done}, {Key: "hermes:h", SessionID: "h", Tool: "hermes"}}
	for name, c := range map[string]struct {
		w               *sessions.World
		snap, scan, err error
	}{
		"no snapshot":  {nil, errors.New("no proc"), nil, nil},
		"its error":    {w, errors.New("partial"), nil, nil},
		"a scan error": {w, nil, errors.New("pi: not read in time"), nil},
		"no stamps":    {w, nil, nil, errors.New("store")},
	} {
		if got, err := capacitySessions(c.w, c.snap, live, c.scan, c.err); err == nil {
			t.Errorf("%s: a look that is not whole is an error, got %v", name, got)
		}
	}
	got, err := capacitySessions(w, nil, live, nil, nil)
	if err != nil || len(got) != 2 || got[5].ID != "id1" || got[5].Cwd != "/w" || got[-3].Key != "hermes:h" {
		t.Errorf("a whole look keeps each session but the done ones, by its process: %+v %v", got, err)
	}
}

func TestTheCapacityListingSaysWhatEachItemFreedAndWhoseFigureItIs(t *testing.T) {
	const M = int64(1 << 20)
	zero, six := int64(0), 6*M
	res := &capacity.Result{At: time.Now(), Rules: []capacity.RuleResult{
		{Rule: capacity.RuleTargets, Live: true, Ran: true, Findings: []capacity.Finding{
			{Rule: capacity.RuleTargets, Path: "/p/a/target", Bytes: 9 * M, Action: capacity.ActDelete, Done: true, Freed: 4 * M},
			{Rule: capacity.RuleTargets, Path: "/p/b/target", Bytes: 9 * M, Action: capacity.ActDelete, Freed: 2 * M, Err: "stopped"},
		}},
		{Rule: capacity.RuleWorktrees, Ran: true, Findings: []capacity.Finding{
			{Rule: capacity.RuleWorktrees, Path: "/c/wt", Bytes: 7 * M, Action: capacity.ActDelete, WouldFree: &six},
			{Rule: capacity.RuleWorktrees, Path: "/c/linked", Bytes: 7 * M, Action: capacity.ActDelete, WouldFree: &zero},
		}},
		{Rule: capacity.RuleUV, Live: true, Ran: true, Findings: []capacity.Finding{
			{Rule: capacity.RuleUV, Path: "/c/uv", Bytes: 40 * M, Action: capacity.ActDelete, Done: true, Freed: 3 * M, By: "uv",
				AtLeast: true, Out: 30 * M},
		}},
		{Rule: capacity.RuleDocker, Live: true, Ran: true, Findings: []capacity.Finding{
			{Rule: capacity.RuleDocker, Path: "docker build cache", Action: capacity.ActDelete, Done: true, By: "Docker", Unsaid: true},
		}},
	}}
	var out bytes.Buffer
	printCapacity(&out, res, false)
	for _, want := range []string{
		"/p/a/target; freed 4 MiB\n",
		"/p/b/target; stopped; freed 2 MiB\n", // a deletion that stopped midway freed what went
		"/c/wt; would free 6 MiB\n",
		"/c/linked; would free 0 MiB\n",
		"/c/uv; freed at least 3 MiB by uv's count, 30 MiB out of it\n",
		"docker build cache; freed an amount Docker did not print\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the listing has %q:\n%s", want, out.String())
		}
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"path":"/p/a/target","bytes":9437184,"action":"delete","done":true,"freed":4194304`) ||
		!strings.Contains(string(b), `"path":"docker build cache","bytes":0,"action":"delete","done":true,"freed":0,"freed_by":"Docker","unsaid":true`) {
		t.Errorf("the JSON says what each freed, nothing among it: %s", b)
	}
}
