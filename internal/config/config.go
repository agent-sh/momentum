// Package config reads where this machine's signal stores are. The locations are machine-specific, so they
// live in the configuration directory. Opted-in transcript homes use harness
// directory conventions; additional locations remain in sources.json.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
	return s, nil
}
