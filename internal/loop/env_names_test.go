package loop

import (
	"os/exec"
	"strings"
	"testing"
)

func maskedEnv() string { return `python3 -c 'import os; print("\n".join(sorted(os.environ.keys())))'` }
func TestEnvironmentProbePrintsOnlyNames(t *testing.T) {
	cmd := exec.Command("sh", "-c", maskedEnv())
	cmd.Env = []string{"PATH=/usr/bin:/bin", "ZZ_HIDDEN=marker-7f3a", "ZZ_MULTI=first\nsecond"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"marker-7f3a", "usr/bin", "first", "second", "="} {
		if strings.Contains(string(out), value) {
			t.Fatal("environment probe exposed a value")
		}
	}
	for _, name := range []string{"PATH", "ZZ_HIDDEN", "ZZ_MULTI"} {
		if !strings.Contains(string(out), name) {
			t.Fatalf("missing variable name %s", name)
		}
	}
}
