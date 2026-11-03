package loop

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed skills/*/SKILL.md
var skills embed.FS

func Skill(name string) (string, error) {
	raw, err := skills.ReadFile("skills/" + name + "/SKILL.md")
	if err != nil {
		return "", fmt.Errorf("no skill %q", name)
	}
	return strings.TrimSpace(string(raw)), nil
}

func stepSystem(step string, rules, constraints []string) (string, error) {
	base, err := investigationSystem(rules, constraints)
	if err != nil {
		return "", err
	}
	if step == StepInvestigate {
		return base, nil
	}
	name := "loop"
	if step == StepReview {
		name = "review"
	}
	skill, err := Skill(name)
	if err != nil {
		return "", err
	}
	return skill + "\n\n" + base, nil
}
