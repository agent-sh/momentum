package loop

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// skills are the loop's procedure and its families' skills, one file each, built into the binary so a run reads
// the skills of the build that runs it. Each family's is small and distinct, and the runner loads only the one a
// case's family names: no run chooses among them.
//
//go:embed skills/*/SKILL.md
var skills embed.FS

// Skill is a skill's text without its front matter.
func Skill(name string) (string, error) {
	raw, err := fs.ReadFile(skills, "skills/"+name+"/SKILL.md")
	if err != nil {
		return "", fmt.Errorf("no skill %q", name)
	}
	s := string(raw)
	if strings.HasPrefix(s, "---\n") {
		if i := strings.Index(s[4:], "\n---\n"); i >= 0 {
			s = s[4+i+5:]
		}
	}
	return strings.TrimSpace(s), nil
}

// Families are the families the loop has a skill for.
func Families() []string {
	var out []string
	entries, _ := fs.ReadDir(skills, "skills")
	for _, e := range entries {
		if n := e.Name(); n != "loop" && n != "review" {
			out = append(out, n)
		}
	}
	return out
}

// systemPrompt is what a run's agent reads before its prompt: the loop's procedure, the skill of its step, the
// standing constraints, and the machine's rules, read from their files at each run.
func systemPrompt(step, family string, rules []string, constraints []string) (string, error) {
	var b strings.Builder
	name := family
	if step == StepReview {
		name = "review"
	}
	if step != StepReview {
		loop, err := Skill("loop")
		if err != nil {
			return "", err
		}
		b.WriteString(loop + "\n\n")
	}
	sk, err := Skill(name)
	if err != nil {
		return "", err
	}
	b.WriteString(sk + "\n")
	if len(constraints) > 0 {
		b.WriteString("\n# Standing constraints from the owner\n\n")
		for _, c := range constraints {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	}
	for _, r := range rules {
		raw, err := os.ReadFile(r)
		if err != nil {
			return "", fmt.Errorf("a rules file the loop reads: %w", err)
		}
		fmt.Fprintf(&b, "\n# The machine's rules: %s\n\n%s\n", r, strings.TrimSpace(string(raw)))
	}
	return b.String(), nil
}
