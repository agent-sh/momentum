package capacity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A harness starts an MCP server, and the server starts a program for each call. The program writes nothing to its
// target dir and lives for one call, so neither the writes nor the sampler see the use: the server's configuration,
// and the script it runs, name it.
func TestATargetOnlyAnMCPServersScriptNamesIsKept(t *testing.T) {
	gitEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	projects := filepath.Join(home, "projects")
	repo := func(name string) string {
		d := filepath.Join(projects, name)
		write(t, filepath.Join(d, ".gitignore"), "target/\n")
		write(t, filepath.Join(d, "src", "main.rs"), "fn main() {}\n")
		gitT(t, d, "init", "-q")
		gitT(t, d, "add", ".")
		gitT(t, d, "commit", "-qm", "init")
		cargoTarget(t, filepath.Join(d, "target"))
		return filepath.Join(d, "target")
	}
	used, idle := repo("tools"), repo("other")
	blob(t, filepath.Join(used, "release", "web-cli"), 1<<20)
	server := filepath.Join(home, ".claude", "mcp-servers", "web", "server.mjs")
	write(t, server, "import { spawn } from \"node:child_process\";\nconst HOME = process.env.HOME;\n"+
		"const BIN = process.env.WEB_BIN ||\n  `${HOME}/projects/tools/target/release/web-cli`;\n")
	claude := filepath.Join(home, ".claude.json")
	write(t, claude, `{"mcpServers":{"web":{"type":"stdio","command":"node","args":["~/.claude/mcp-servers/web/server.mjs"]}}}`)

	// names.mcp is not set: the harnesses' own configurations are read by default
	cfg := Config{Targets: TargetsConfig{Roots: []string{projects}}}.Normal()
	if !strings.Contains(strings.Join(cfg.Names.MCP, " "), claude) {
		t.Fatalf("Claude Code's configuration is read by default: %q", cfg.Names.MCP)
	}
	cfg.Live = []string{RuleTargets}
	res, err := Run(context.Background(), cfg, openStore(t), options(t, procRoot(t), time.Now().Add(72*time.Hour), nil))
	must(t, err)
	got := findings(res, RuleTargets)
	if f := got[used]; f.Action != ActKeep || !strings.Contains(f.Why, "named by mcp web") {
		t.Errorf("the target dir an MCP server's script runs a program from is kept: %q (%s)", f.Action, f.Why)
	}
	if _, err := os.Stat(filepath.Join(used, "release", "web-cli")); err != nil {
		t.Errorf("the program the server runs is still there: %v", err)
	}
	if f := got[idle]; f.Action != ActDelete {
		t.Errorf("a target dir no server names still goes: %q (%s)", f.Action, f.Why)
	}
}

func TestMCPConfigsNameTheirServers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := func(parts ...string) string { return filepath.Join(append([]string{home}, parts...)...) }
	proj := p("projects", "app")
	write(t, p("claude.json"), `{"mcpServers":{"g":{"command":"`+p("bin", "global srv")+`"}},`+
		`"projects":{"`+proj+`":{"mcpServers":{"pr":{"command":"node","args":["`+p("srv", "proj.js")+`"],"env":{"DATA":"`+p("data", "pr")+`"}}}},`+
		`"`+p("projects", "gone")+`":{"mcpServers":{}}}}`)
	write(t, filepath.Join(proj, ".mcp.json"), `{"mcpServers":{"local":{"command":"./tools/run.sh"}}}`)
	write(t, filepath.Join(proj, "tools", "run.sh"), "#!/bin/sh\nexec $HOME/models/local-model/serve\n")
	write(t, p("codex.toml"), "model = \"x\"\n[mcp_servers.cx]\ncommand = \"/usr/bin/env\"\nargs = [\n  \"$HOME/cx/server\",\n]\n"+
		"[mcp_servers.cx.tools.a]\napproval_mode = \"approve\"\n[profiles.other]\nmodel_dir = \""+p("not-mcp", "toml")+"\"\n")
	write(t, p("hermes.yaml"), "model_dir: "+p("not-mcp", "yaml")+"\nmcp_servers:\n  hx:\n    command: ~/hx/bin/srv\n    args:\n      - mcp\n"+
		"    env:\n      X: '1'\nother:\n  path: "+p("not-mcp", "after")+"\n")
	n, err := LoadNamed(context.Background(), nil, NamesConfig{MCP: []string{p("claude.json"), p("codex.toml"), p("hermes.yaml"), p("missing.json")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for path, job := range map[string]string{
		p("bin", "global srv"):       "mcp g",
		p("srv", "proj.js"):          "mcp pr",
		p("data", "pr"):              "mcp pr",
		filepath.Join(proj, "tools"): "mcp local",
		p("models", "local-model"):   "mcp local (via run.sh)",
		p("cx", "server"):            "mcp cx",
		p("hx", "bin", "srv"):        "mcp hx",
	} {
		if j, ok := n.Protects(path); !ok || j != job {
			t.Errorf("%s is named by %q: %q %v", path, job, j, ok)
		}
	}
	for _, path := range []string{p("not-mcp")} {
		if j, ok := n.Protects(path); ok {
			t.Errorf("%s is no server's: named by %s", path, j)
		}
	}
}

func TestAHomeTemplateIsExpandedBeforeTheSplit(t *testing.T) {
	t.Setenv("HOME", "/h")
	got := strings.Join(words("const a = `${HOME}/x/y`; b = \"$HOME/z\"; c = `${process.env.HOME}/w`; d = $HOMEDIR/q"), " ")
	for _, w := range []string{"/h/x/y", "/h/z", "/h/w"} {
		if !strings.Contains(" "+got+" ", " "+w+" ") {
			t.Errorf("%s is a word of %q", w, got)
		}
	}
	if strings.Contains(got, "/hDIR") || strings.Contains(got, "/h/q") {
		t.Errorf("$HOMEDIR is another variable: %q", got)
	}
}
