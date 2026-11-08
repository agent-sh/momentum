package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/momentum/internal/source"
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

// The loop's section holds its authority, so a key it does not know is an error, not a protection gone: a typo
// of exclude leaves nothing excluded. Its directories expand as the others do.
func TestTheLoopsSectionRefusesAKeyItDoesNotKnow(t *testing.T) {
	cfg, home := t.TempDir(), t.TempDir()
	t.Setenv("MOMENTUM_CONFIG", cfg)
	t.Setenv("HOME", home)
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(cfg, "sources.json"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"loop": {"owners": ["agent-sh"], "exlude": ["agent-sh/x"]}}`)
	if _, err := Load(); err == nil {
		t.Fatal("a misspelt key of the loop's was taken")
	}
	write(`{"loop": {"owners": ["agent-sh"], "exclude": ["agent-sh/x"], "read_dirs": ["~/projects"]}, "other": {"anything": 1}}`)
	s, err := Load()
	if err != nil || len(s.Loop.Exclude) != 1 || s.Loop.ReadDirs[0] != filepath.Join(home, "projects") {
		t.Fatalf("%+v %v", s.Loop, err)
	}
}

func TestLoopAcceptsWholeConfigurationAndExpandsTacitStore(t *testing.T) {
	cfg, home := t.TempDir(), t.TempDir()
	t.Setenv("MOMENTUM_CONFIG", cfg)
	t.Setenv("HOME", home)
	raw := `{"loop":{"runner":["/fixture/claude"],"owners":["fixture"],"exclude":["fixture/other"],"repo_guard":["/fixture/guard"],"sandbox_read":["~/runtime"],"capacity_gate":["/fixture/capacity"],"live":true,"fallback_model":"global.anthropic.claude-sonnet-5-5","tacit_store":"~/facts","read_dirs":["~/repo"]}}`
	if err := os.WriteFile(filepath.Join(cfg, "sources.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Loop.TacitStore != filepath.Join(home, "facts") || got.Loop.ReadDirs[0] != filepath.Join(home, "repo") || !got.Loop.Live || len(got.Loop.Owners) != 1 || len(got.Loop.Exclude) != 1 || len(got.Loop.RepoGuard) != 1 || len(got.Loop.CapacityGate) != 1 || len(got.Loop.SandboxRead) != 1 || got.Loop.FallbackModel == "" {
		t.Fatal("loop configuration was lost or not expanded")
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

func TestASubSourceIsLiveAsItsReaderIs(t *testing.T) {
	var s Sources
	for _, source := range []string{"radar", "radar-feedback", "mistakes", "incidents", "transcripts"} {
		if s.SourceLive(source) {
			t.Errorf("%s is live with nothing configured", source)
		}
	}
	s.Radar.Path = "/synthetic/radar.db"
	if !s.SourceLive("radar") || !s.SourceLive("radar-feedback") || s.SourceLive("mistakes") {
		t.Errorf("a configured radar: radar %v, its feedback %v, mistakes %v", s.SourceLive("radar"), s.SourceLive("radar-feedback"), s.SourceLive("mistakes"))
	}
	if !s.SourceLive("sessions") || !s.SourceLive("capacity") || !s.SourceLive("zz-reader") {
		t.Error("commands and unknown readers are live")
	}
}

func TestASourceIsLiveWhenItsReaderIsConfigured(t *testing.T) {
	var s Sources
	s.Mistakes.Path = "/synthetic/mistakes.jsonl"
	s.Incidents.Dir = "/synthetic/incidents"
	s.Transcripts.Homes = []string{"/synthetic"}
	for source, want := range map[string]bool{"mistakes": true, "incidents": true, "transcripts": true, "radar": false, "radar-feedback": false, "supervise": true} {
		if got := s.SourceLive(source); got != want {
			t.Errorf("%s: live %v, want %v", source, got, want)
		}
	}
	s.Supervise.Off = true
	if s.SourceLive("supervise") {
		t.Error("supervision is switched off")
	}
	var none Sources
	if none.SourceLive("mistakes") || none.SourceLive("incidents") || none.SourceLive("transcripts") {
		t.Error("readers nothing names are live")
	}
}

func TestANativeTranscriptReaderIsLiveWithoutAHarnessHome(t *testing.T) {
	var s Sources
	s.Transcripts.Sources = []source.TranscriptSource{{Harness: "kimi", Glob: "marker-7f3a/*.jsonl"}}
	if !s.SourceLive("transcripts") {
		t.Fatal("a configured native reader was omitted from measurement")
	}
}

// Configuration targets are off unless the owner turns them on, take only known keys, and an allow entry names one
// exact file under the home: no pattern, no directory form, nothing outside the home.
func TestConfigTargetsAreOffByDefaultAndExact(t *testing.T) {
	cfg, home := t.TempDir(), t.TempDir()
	t.Setenv("MOMENTUM_CONFIG", cfg)
	t.Setenv("HOME", home)
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(cfg, "sources.json"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"loop": {"live": true}}`)
	if s, err := Load(); err != nil || s.Loop.ConfigTargets.Enabled || len(s.Loop.ConfigTargets.Allow) != 0 {
		t.Fatalf("default: %+v %v", s.Loop.ConfigTargets, err)
	}
	write(`{"loop": {"config_targets": {"enabled": true, "alow": []}}}`)
	if _, err := Load(); err == nil {
		t.Fatal("a misspelt key of config_targets was taken")
	}
	for _, bad := range []string{`"/etc/marker-c0ffee"`, `"../x"`, `"~/../x"`, `".config/./agents/X.md"`, `""`, `".config/agents/"`,
		`".config/agents/*.md"`, `".config/agents/**"`, `".config/agents/zz-r?les.md"`, `".config/agents/zz-[r]ules.md"`,
		`".config/agents/MY zz-rules.md"`, `".config/agents/` + strings.Repeat("a", 430) + `.md"`} {
		write(`{"loop": {"config_targets": {"enabled": true, "allow": [` + bad + `]}}}`)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "config_targets.allow") {
			t.Fatalf("allow %s: %v", bad, err)
		}
	}
	write(`{"loop": {"config_targets": {"enabled": true, "allow": ["~/.config/agents/zz-rules.md", ".config/agents/zz-other.md"]}}}`)
	if s, err := Load(); err != nil || !s.Loop.ConfigTargets.Enabled || len(s.Loop.ConfigTargets.Allow) != 2 {
		t.Fatalf("%+v %v", s.Loop.ConfigTargets, err)
	}
}

// The loop's own files, as configured, are what Own names: rules, programs and the files they name as arguments, the
// relay environment and the tacit store.
func TestTheLoopsOwnFilesAreItsConfiguredOnes(t *testing.T) {
	l := Loop{Rules: []string{"/r/zz-rules.md"}, RepoGuard: []string{"python3", "~/g/guard.py", "x", "--policy=/p/policy.json"},
		CapacityGate: []string{"/b/gate", "--config", "/c/gate.toml", "--rules=~/c/rules.toml", "--quiet=1"},
		Runner:       []string{"/b/run"}, BedrockEnv: "/e/relay.env", TacitStore: "/s/store"}
	if got := strings.Join(l.Own(), " "); got != "/r/zz-rules.md python3 ~/g/guard.py /p/policy.json /b/gate /c/gate.toml ~/c/rules.toml /b/run /e/relay.env /s/store" {
		t.Fatalf("own: %s", got)
	}
	// a relative file, and one under $HOME, are the loop's own too: a word that names no file is not
	l = Loop{RepoGuard: []string{"/b/guard", "scripts/zz-check.sh", "zz-conf.toml", "$HOME/.config/agents/zz-a.md", "${HOME}/zz-b.md",
		"--rules=$HOME/zz-c.md", "--from=./zz-d.md", "plain", "--count=2"}}
	if got := strings.Join(l.Own(), " "); got != "/b/guard scripts/zz-check.sh zz-conf.toml ~/.config/agents/zz-a.md ~/zz-b.md ~/zz-c.md ./zz-d.md" {
		t.Fatalf("own, relative and under $HOME: %s", got)
	}
	// a shell's line names its files too
	l = Loop{Runner: []string{"/bin/sh", "-c", "cd ~/zz-work && ./zz-run.sh --conf \"$HOME/zz-e.md\" | tee zz-log.txt; exit"}}
	if got := strings.Join(l.Own(), " "); got != "/bin/sh ~/zz-work ./zz-run.sh ~/zz-e.md zz-log.txt" {
		t.Fatalf("own, in a shell's line: %s", got)
	}
}
