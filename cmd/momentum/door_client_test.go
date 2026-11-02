package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoopAndEvaluationRefuseAMissingDoorClient(t *testing.T) {
	for _, eval := range []bool{false, true} {
		t.Run(map[bool]string{false: "loop", true: "eval"}[eval], func(t *testing.T) {
			t.Setenv("MOMENTUM_HOME", t.TempDir())
			t.Setenv("MOMENTUM_CONFIG", t.TempDir())
			env := filepath.Join(t.TempDir(), "route.env")
			if err := os.WriteFile(env, []byte("CLAUDE_CODE_USE_BEDROCK=1\nCLAUDE_CODE_SKIP_BEDROCK_AUTH=1\nANTHROPIC_BEDROCK_BASE_URL=http://127.0.0.1:1/synthetic\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			configuration := `{"loop":{"bedrock_env":"` + env + `","door_client":"` + filepath.Join(t.TempDir(), "absent-client") + `"}}`
			if err := os.WriteFile(filepath.Join(os.Getenv("MOMENTUM_CONFIG"), "sources.json"), []byte(configuration), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"loop"}
			if eval {
				golden := filepath.Join(t.TempDir(), "golden.jsonl")
				if err := os.WriteFile(golden, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "eval", "--golden", golden, "--budget", "0")
			}
			code, _, diagnostic := momentum(args...)
			if code == 0 || !strings.Contains(diagnostic, "momentum-door") || !strings.Contains(diagnostic, "go install ./cmd/...") {
				t.Fatalf("missing client: %d %s", code, diagnostic)
			}
			if _, err := os.Stat(filepath.Join(loopDir(), "runs")); !os.IsNotExist(err) {
				t.Fatalf("a run was started: %v", err)
			}
		})
	}
}
