// Package config reads where this machine's signal stores are. The locations are machine-specific, so they
// live in the configuration directory, never in the code: momentum ships no defaults for them.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/agent-sh/momentum/internal/paths"
)

// Sources lists the signal stores to ingest. An empty entry is not ingested.
type Sources struct {
	Mistakes struct {
		Path string `json:"path"`
	} `json:"mistakes"`
	Radar struct {
		Path       string `json:"path"`
		TestDevice string `json:"test_device"`
	} `json:"radar"`
	Incidents struct {
		Dir string `json:"dir"`
	} `json:"incidents"`
	// Transcripts are the agents' session histories, read with tacit's reader under each home: Claude Code,
	// Codex, pi and Hermes, where tacit finds them (the harnesses' own variables apply). Workers is how many
	// files an ingest reads at once; 0 is the default.
	Transcripts struct {
		Homes   []string `json:"homes"`
		Workers int      `json:"workers"`
	} `json:"transcripts"`
	// Sessions configures the live session board. Harness locations come from the harnesses' own variables
	// (CLAUDE_CONFIG_DIR, CODEX_HOME, XDG_DATA_HOME, HERMES_HOME, PI_CODING_AGENT_DIR), not from here.
	Sessions struct {
		Projects []string `json:"projects"` // directories whose subpaths name a session's project
	} `json:"sessions"`
	// Supervise configures `momentum supervise`. Live, when set, names the rules that send (empty: none), in
	// place of the ones momentum ships live; the others record what they would have sent. Notice is a command
	// run once for each case opened, with a line about the session as its last argument: a passive notice for
	// the owner, never one that wakes him. None by default. Off stops supervision.
	Supervise struct {
		Live   []string `json:"live"`
		Notice []string `json:"notice"`
		Off    bool     `json:"off"`
	} `json:"supervise"`
	// Detect configures the loop's detectors. ProgramPrefixes open the turns the machine's programs type into
	// sessions (a wake loop's, a brief's), which the transcripts may credit to the owner: none of them is his.
	Detect struct {
		ProgramPrefixes []string `json:"program_prefixes"`
	} `json:"detect"`
	// Loop configures the improvement loop (momentum loop, momentum ship). Nothing of it is compiled in but the
	// budgets' defaults: without a runner the loop investigates nothing, without owners the ship tool writes to no
	// repository, and without config_allow it changes no configuration in place.
	Loop Loop `json:"loop"`
}

// Loop is the loop's configuration. Runner is the command that starts the harness's main agent headless, with
// its first arguments (Claude Code on this machine's provider). Model and FallbackModel are the agent's models.
// Owners are the repository owners the ship tool may write to; every other repository is report-only, and
// Exclude names repositories (OWNER/REPO) it never touches. ConfigAllow are configuration directories it may
// change in place. Rules are the files of the machine's shared rules every run reads. RepoGuard is the
// read-only scope check every repository write runs first. MCP is an MCP configuration for the runs that build a
// change (a web search for prior art). Live, false by default, lets the ship tool land what it proposed. The
// budgets are in US dollars, the run's time in minutes; zero takes the default.
type Loop struct {
	Live          bool     `json:"live"`
	Runner        []string `json:"runner"`
	Model         string   `json:"model"`
	FallbackModel string   `json:"fallback_model"`
	Families      []string `json:"families"`
	Owners        []string `json:"owners"`
	Exclude       []string `json:"exclude"`
	ConfigAllow   []string `json:"config_allow"`
	Rules         []string `json:"rules"`
	RepoGuard     []string `json:"repo_guard"`
	MCP           string   `json:"mcp"`
	RunUSD        float64  `json:"run_usd"`
	CaseUSD       float64  `json:"case_usd"`
	DayUSD        float64  `json:"day_usd"`
	WeekUSD       float64  `json:"week_usd"`
	RunMinutes    int      `json:"run_minutes"`
	MaxTurns      int      `json:"max_turns"`
	// Constraints are the owner's standing constraints every run reads, in his words.
	Constraints []string `json:"constraints"`
	// CapacityGate is a command that waits until the machine has room for a check's test and exits 0, or fails.
	CapacityGate []string `json:"capacity_gate"`
}

// ErrNoSources means no sources file exists yet.
var ErrNoSources = errors.New("no sources configured")

// SourcesPath is the file Load reads.
func SourcesPath() string { return filepath.Join(paths.Config(), "sources.json") }

// Load reads the sources file and expands a leading ~/ in every location.
func Load() (Sources, error) {
	var s Sources
	raw, err := os.ReadFile(SourcesPath())
	if errors.Is(err, os.ErrNotExist) {
		return s, fmt.Errorf("%w: write %s (see README)", ErrNoSources, SourcesPath())
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("%s: %w", SourcesPath(), err)
	}
	s.Mistakes.Path = paths.Expand(s.Mistakes.Path)
	s.Radar.Path = paths.Expand(s.Radar.Path)
	s.Incidents.Dir = paths.Expand(s.Incidents.Dir)
	for i, p := range s.Transcripts.Homes {
		s.Transcripts.Homes[i] = paths.Expand(p)
	}
	for i, p := range s.Sessions.Projects {
		s.Sessions.Projects[i] = paths.Expand(p)
	}
	if len(s.Supervise.Notice) > 0 {
		s.Supervise.Notice[0] = paths.Expand(s.Supervise.Notice[0])
	}
	if len(s.Loop.Runner) > 0 {
		s.Loop.Runner[0] = paths.Expand(s.Loop.Runner[0])
	}
	if len(s.Loop.RepoGuard) > 0 {
		s.Loop.RepoGuard[0] = paths.Expand(s.Loop.RepoGuard[0])
	}
	if len(s.Loop.CapacityGate) > 0 {
		s.Loop.CapacityGate[0] = paths.Expand(s.Loop.CapacityGate[0])
	}
	for _, l := range [][]string{s.Loop.ConfigAllow, s.Loop.Rules} {
		for i, p := range l {
			l[i] = paths.Expand(p)
		}
	}
	s.Loop.MCP = paths.Expand(s.Loop.MCP)
	return s, nil
}
