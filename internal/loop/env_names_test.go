package loop

import (
	"os/exec"
	"strings"
	"testing"
)

// maskedEnv is a shell command that lists the environment of the shell that runs it with every value hidden but
// those of the variables named. Each line is NAME= for the others and NAME=value for a named variable that is set
// (an empty one included). A test reads an environment through it and never through env: a failure message then
// names the variable that leaked and never carries its content, whoever's environment the shell inherited.
func maskedEnv(shown ...string) string {
	hide := `sed -n -E 's/^([A-Za-z_][A-Za-z0-9_]*)=.*/\1=/p'`
	if len(shown) == 0 {
		return "env | " + hide
	}
	return "{ env | " + hide + " | grep -v -E '^(" + strings.Join(shown, "|") + ")=$'; for v in " + strings.Join(shown, " ") +
		`; do if printenv "$v" > /dev/null; then printf '%s=%s\n' "$v" "$(printenv "$v")"; fi; done; }`
}

// maskedEnv hides every value it was not asked to show, keeps a shown variable set to nothing, and leaves out a shown
// variable that is not set. The environment here is the test's own, with synthetic values.
func TestMaskedEnvHidesEveryValueButTheShownOnes(t *testing.T) {
	cmd := exec.Command("sh", "-c", maskedEnv("ZZ_SHOWN", "ZZ_EMPTY", "ZZ_UNSET"))
	cmd.Env = []string{"PATH=/usr/bin:/bin", "ZZ_HIDDEN=marker-7f3a", "ZZ_SHOWN=shown-value", "ZZ_EMPTY=", "ZZ_MULTI=first\nmarker-7f3a second line"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if strings.Contains(got, "marker-7f3a") || strings.Contains(got, "usr/bin") || strings.Contains(got, "second line") {
		t.Errorf("a value was printed:\n%s", got)
	}
	lines := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(got), "\n") {
		lines[l] = true
	}
	for _, want := range []string{"ZZ_HIDDEN=", "ZZ_SHOWN=shown-value", "ZZ_EMPTY=", "PATH=", "ZZ_MULTI="} {
		if !lines[want] {
			t.Errorf("the listing lacks the line %q:\n%s", want, got)
		}
	}
	if lines["ZZ_SHOWN="] || lines["ZZ_UNSET="] {
		t.Errorf("a shown variable is listed twice, or one that is not set is listed:\n%s", got)
	}
	cmd = exec.Command("sh", "-c", maskedEnv())
	cmd.Env = []string{"ZZ_HIDDEN=marker-7f3a"}
	out, err = cmd.Output()
	if err != nil || !strings.Contains(string(out), "ZZ_HIDDEN=\n") {
		t.Fatalf("with nothing shown the listing is still there: %q %v", out, err)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if !strings.HasSuffix(l, "=") {
			t.Errorf("with nothing shown a line holds a value: %q", l)
		}
	}
}
