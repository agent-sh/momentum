package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The transcripts source's homes come from sources.json, ~ alone and ~/ expanded to the home directory.
func TestTranscriptsHomesAreExpanded(t *testing.T) {
	cfg, home := t.TempDir(), t.TempDir()
	t.Setenv("MOMENTUM_CONFIG", cfg)
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(cfg, "sources.json"), []byte(`{"transcripts": {"homes": ["~", "~/other", "/abs"], "workers": 3}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if h := s.Transcripts.Homes; len(h) != 3 || h[0] != home || h[1] != filepath.Join(home, "other") || h[2] != "/abs" || s.Transcripts.Workers != 3 {
		t.Fatalf("homes %v, workers %d", h, s.Transcripts.Workers)
	}
}
