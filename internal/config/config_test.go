package config

import (
	"os"
	"path/filepath"
	"strings"
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

func TestTranscriptSourceHarnessesValidatedAtLoad(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("MOMENTUM_CONFIG", cfg)
	for _, tc := range []struct {
		harness, field string
		valid          bool
	}{
		{"opencode", `"db":"neutral.db"`, true},
		{"grok", `"glob":"neutral/*.jsonl"`, true},
		{"kimi", `"glob":"neutral/*.jsonl"`, true},
		{"codex", `"glob":"neutral/*.jsonl"`, false},
		{"marker-7f3a", `"glob":"neutral/*.jsonl"`, false},
		{"", `"glob":"neutral/*.jsonl"`, false},
		{"kimi", `"db":"neutral.db"`, false},
		{"opencode", `"glob":"neutral/*.jsonl"`, false},
	} {
		raw := `{"transcripts":{"sources":[{"harness":"` + tc.harness + `",` + tc.field + `}]}}`
		if err := os.WriteFile(filepath.Join(cfg, "sources.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := Load()
		if (err == nil) != tc.valid {
			t.Errorf("%q validation: %v", tc.harness, err)
		}
		if err != nil && !strings.Contains(err.Error(), "opencode, grok, kimi") {
			t.Errorf("known source harnesses missing from error: %v", err)
		}
	}
}

func TestExtraTranscriptLocationsAreExpanded(t *testing.T) {
	cfg, home := t.TempDir(), t.TempDir()
	t.Setenv("MOMENTUM_CONFIG", cfg)
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(cfg, "sources.json"), []byte(`{"transcripts":{"sources":[{"harness":"opencode","db":"~/neutral.db"},{"harness":"kimi","glob":"~/neutral/*/wire.jsonl"}]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Transcripts.Sources) != 2 || s.Transcripts.Sources[0].DB != filepath.Join(home, "neutral.db") || s.Transcripts.Sources[1].Glob != filepath.Join(home, "neutral", "*", "wire.jsonl") {
		t.Fatalf("locations %+v", s.Transcripts.Sources)
	}
}
