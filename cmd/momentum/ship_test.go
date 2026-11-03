package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShipRequiresAnExplicitCase(t *testing.T) {
	t.Setenv("MOMENTUM_HOME", t.TempDir())
	t.Setenv("MOMENTUM_CONFIG", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	if code, _, text := momentum("ship", "status"); code != 2 || !strings.Contains(text, "--case") {
		t.Fatalf("owner ship command accepted no case: exit %d", code)
	}
}

func TestGlobalGitIdentityUsesNoRepositoryEnvironment(t *testing.T) {
	bin := t.TempDir()
	record := filepath.Join(bin, "names")
	body := `#!/bin/sh
pwd > '` + record + `'
python3 -c 'import os; print("\n".join(sorted(os.environ.keys())))' >> '` + record + `'
printf 'Synthetic Name\n'
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "untrusted"))
	if gitGlobal("user.name") != "Synthetic Name" {
		t.Fatal("global identity lookup did not use the fixture")
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) == 0 || lines[0] != "/" {
		t.Fatal("global identity lookup could discover a repository")
	}
	for _, name := range lines[1:] {
		if strings.HasPrefix(name, "GIT_") {
			t.Fatalf("global identity lookup inherited repository environment name: %s", name)
		}
	}
}
