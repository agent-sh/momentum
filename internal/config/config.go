// Package config reads where this machine's signal stores are. The locations are machine-specific, so they
// live in the configuration directory. Opted-in transcript homes use harness
// directory conventions; additional locations remain in sources.json.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/source"
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
	// Transcripts read tacit's four harnesses and the native OpenCode, Grok and
	// Kimi adapters. Sources adds native locations; Workers controls concurrency.
	Transcripts struct {
		Homes   []string                  `json:"homes"`
		Workers int                       `json:"workers"`
		Sources []source.TranscriptSource `json:"sources"`
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
	// Loop configures investigations, builds and measurement.
	Loop Loop `json:"loop"`
}

// Loop configures the native agent, read sources, rules, constraints and budgets.
// Model and FallbackModel name one model each.
type Loop struct {
	Live          bool     `json:"live"`
	Runner        []string `json:"runner"`
	Model         string   `json:"model"`
	FallbackModel string   `json:"fallback_model"`
	Families      []string `json:"families"`
	Owners        []string `json:"owners"`
	Exclude       []string `json:"exclude"`
	// ReadDirs are repositories cloned read-only for investigations and host directories that admit a build report's installed file.
	ReadDirs   []string `json:"read_dirs"`
	TacitStore string   `json:"tacit_store"`
	// SandboxRead adds read-only inputs for build checks.
	SandboxRead []string `json:"sandbox_read"`
	Rules       []string `json:"rules"`
	RepoGuard   []string `json:"repo_guard"`
	BedrockEnv  string   `json:"bedrock_env"`
	RunUSD      float64  `json:"run_usd"`
	CaseUSD     float64  `json:"case_usd"`
	DayUSD      float64  `json:"day_usd"`
	WeekUSD     float64  `json:"week_usd"`
	RunMinutes  int      `json:"run_minutes"`
	MaxTurns    int      `json:"max_turns"`
	// Constraints are the owner's standing constraints every run reads, in his words.
	Constraints []string `json:"constraints"`
	// CapacityGate is a command that waits until the machine has room for a check's test and exits 0, or fails.
	CapacityGate []string `json:"capacity_gate"`
	// ConfigTargets is the loop's authority to change configuration files in place: off unless Enabled, and then
	// only the existing files Allow names exactly (home-relative or under ~/); an empty Allow admits none. Landing
	// also needs Live.
	ConfigTargets ConfigTargets `json:"config_targets"`
}

// ConfigTargets is loop.config_targets.
type ConfigTargets struct {
	Enabled bool     `json:"enabled"`
	Allow   []string `json:"allow"`
}

// Own are the loop's own files as configured: its rules files, the programs of its scope check, capacity gate and
// runner with every file they name as an argument, its relay environment and tacit store. No configuration target is
// one of them.
func (l Loop) Own() []string {
	own := append([]string(nil), l.Rules...)
	for _, cmd := range [][]string{l.RepoGuard, l.CapacityGate, l.Runner} {
		for i, arg := range cmd {
			// the program, and every argument that names a file (a script it runs, a configuration it reads)
			if i == 0 && arg != "" || filepath.IsAbs(arg) || strings.HasPrefix(arg, "~/") {
				own = append(own, arg)
			}
		}
	}
	for _, p := range []string{l.BedrockEnv, l.TacitStore} {
		if p != "" {
			own = append(own, p)
		}
	}
	return own
}

// checkAllow refuses an allow entry that is not one exact file: a clean path under the home, relative or under ~/,
// with no pattern and no trailing slash.
func checkAllow(allow []string) error {
	for _, entry := range allow {
		n := strings.TrimSpace(entry)
		if rest, ok := strings.CutPrefix(n, "~/"); ok {
			n = rest
		}
		if n == "" || strings.ContainsAny(n, "\\\x00\r\n*?[") || filepath.IsAbs(n) || !filepath.IsLocal(n) || filepath.Clean(n) != n {
			return fmt.Errorf("loop.config_targets.allow entry %q is not one file under the home (an exact path, no pattern or directory)", entry)
		}
	}
	return nil
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
	// the loop's section holds its authority: a key it does not know (a typo of exclude) is an error, not a
	// protection silently gone
	var sections struct {
		Loop json.RawMessage `json:"loop"`
	}
	json.Unmarshal(raw, &sections)
	if len(sections.Loop) > 0 {
		d := json.NewDecoder(bytes.NewReader(sections.Loop))
		d.DisallowUnknownFields()
		var l Loop
		if err := d.Decode(&l); err != nil {
			return s, fmt.Errorf("%s: loop: %w", SourcesPath(), err)
		}
		if err := checkAllow(l.ConfigTargets.Allow); err != nil {
			return s, fmt.Errorf("%s: %w", SourcesPath(), err)
		}
	}
	s.Mistakes.Path = paths.Expand(s.Mistakes.Path)
	s.Radar.Path = paths.Expand(s.Radar.Path)
	s.Incidents.Dir = paths.Expand(s.Incidents.Dir)
	for i, p := range s.Transcripts.Homes {
		s.Transcripts.Homes[i] = paths.Expand(p)
	}
	for i := range s.Transcripts.Sources {
		src := &s.Transcripts.Sources[i]
		switch src.Harness {
		case "opencode":
			if src.DB == "" || src.Glob != "" {
				return s, fmt.Errorf("transcripts.sources[%d]: opencode requires db only; known source harnesses: opencode, grok, kimi", i)
			}
		case "grok", "kimi":
			if src.Glob == "" || src.DB != "" {
				return s, fmt.Errorf("transcripts.sources[%d]: %s requires glob only; known source harnesses: opencode, grok, kimi", i, src.Harness)
			}
		default:
			return s, fmt.Errorf("transcripts.sources[%d]: unsupported harness; known source harnesses: opencode, grok, kimi", i)
		}
		src.Glob, src.DB = paths.Expand(src.Glob), paths.Expand(src.DB)
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
	for _, l := range [][]string{s.Loop.ReadDirs, s.Loop.SandboxRead, s.Loop.Rules} {
		for i, p := range l {
			l[i] = paths.Expand(p)
		}
	}
	s.Loop.BedrockEnv = paths.Expand(s.Loop.BedrockEnv)
	s.Loop.TacitStore = paths.Expand(s.Loop.TacitStore)
	return s, nil
}

// SourceLive reports which configured readers can still contribute to a measurement. A source that carries a reader's
// name and a dash (radar-feedback, which Rebuild drops with the radar) is that reader's.
func (s Sources) SourceLive(source string) bool {
	for _, reader := range []string{"mistakes", "radar", "incidents", "transcripts"} {
		if strings.HasPrefix(source, reader+"-") {
			source = reader
			break
		}
	}
	switch source {
	case "mistakes":
		return s.Mistakes.Path != ""
	case "radar":
		return s.Radar.Path != ""
	case "incidents":
		return s.Incidents.Dir != ""
	case "transcripts":
		return len(s.Transcripts.Homes) > 0 || len(s.Transcripts.Sources) > 0
	case "supervise":
		return !s.Supervise.Off
	default:
		return true // sessions and capacity are commands, not configured ingest paths
	}
}
