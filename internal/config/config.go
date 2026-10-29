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
	return s, nil
}
