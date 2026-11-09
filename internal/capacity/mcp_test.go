package capacity

import (
	"context"
	"encoding/json"
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

// A server's environment and request headers hold tokens. A value with a slash is no path to read, and a read's
// error names the word it read: a value too long for a path ended in the run's problems, its events and its notes.
func TestAnMCPServersSecretsReachNoOutput(t *testing.T) {
	gitEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	projects := filepath.Join(home, "projects")
	proj := filepath.Join(projects, "app")
	must(t, os.MkdirAll(proj, 0o755))
	env := "env-secret/" + strings.Repeat("e", 4096)
	bearer := "Bearer hdr-secret/" + strings.Repeat("h", 600)
	claude := filepath.Join(home, ".claude.json")
	write(t, claude, `{"mcpServers":{"web":{"type":"http","url":"https://example.com/mcp","headers":{"Authorization":"`+bearer+`"}}},`+
		`"projects":{"`+proj+`":{"mcpServers":{"db":{"command":"node","args":["srv.js"],"env":{"TOKEN":"`+env+`"}}}}}}`)
	codex := filepath.Join(home, ".codex", "config.toml")
	write(t, codex, "[mcp_servers.cx]\nurl = \"https://example.com/mcp\"\ncwd = \""+proj+"\"\nhttp_headers = { Authorization = \""+bearer+"\" }\n"+
		"env = { TOKEN = \""+env+"\" }\n")
	hermes := filepath.Join(home, ".hermes", "config.yaml")
	write(t, hermes, "mcp_servers:\n  hx:\n    url: https://example.com/mcp\n    cwd: "+proj+"\n    headers:\n      Authorization: "+bearer+
		"\n    env:\n      TOKEN: "+env+"\n")
	leaks := func(s string) bool { return strings.Contains(s, "env-secret") || strings.Contains(s, "hdr-secret") }

	names := NamesConfig{MCP: []string{claude, codex, hermes}}
	n, err := LoadNamed(context.Background(), nil, names, nil)
	if err != nil {
		t.Errorf("a server's environment and headers are no files to read: an error of %d bytes, a secret in it: %v", len(err.Error()), leaks(err.Error()))
	}
	if n != nil && leaks(strings.Join(n.Warnings, " ")) {
		t.Errorf("no warning holds a secret")
	}

	cfg := Config{Targets: TargetsConfig{Roots: []string{projects}}, Names: names}.Normal()
	cfg.Live = []string{RuleTargets}
	opt := options(t, procRoot(t), time.Now().Add(72*time.Hour), nil)
	opt.DryRun = true
	res, err := Run(context.Background(), cfg, openStore(t), opt)
	must(t, err)
	b, err := json.Marshal(res)
	must(t, err)
	if leaks(string(b)) {
		t.Errorf("the run's output holds a secret: problems %d bytes", len(strings.Join(res.Problems, " ")))
	}
}

// A file a server's argument names that cannot be read names itself: only the server's command and the script it
// runs hold the run, and their error names the server and the errno, once.
func TestOnlyAnMCPServersCommandAndScriptHoldTheRun(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	locked := func(name string) string {
		p := filepath.Join(home, name)
		write(t, p, "#!/bin/sh\n")
		must(t, os.Chmod(p, 0))
		return p
	}
	data, script := locked("data.txt"), locked("locked-script.js")
	srv := filepath.Join(home, "srv.js")
	write(t, srv, "// a server\n")
	cfg := filepath.Join(home, "claude.json")
	write(t, cfg, `{"mcpServers":{"a":{"command":"node","args":["`+srv+`","--data","`+data+`"]}}}`)
	n, err := LoadNamed(context.Background(), nil, NamesConfig{MCP: []string{cfg}}, nil)
	if err != nil {
		t.Errorf("an argument that cannot be read holds nothing: %v", err)
	}
	if j, ok := n.Protects(data); !ok || j != "mcp a" {
		t.Errorf("the argument still names itself: %q %v", j, ok)
	}

	write(t, cfg, `{"mcpServers":{"b":{"command":"node","args":["`+script+`"]}}}`)
	_, err = LoadNamed(context.Background(), nil, NamesConfig{MCP: []string{cfg}}, nil)
	switch {
	case err == nil:
		t.Fatal("a script the server runs that cannot be read holds the run")
	case strings.Count(err.Error(), "mcp b") != 1 || !strings.Contains(err.Error(), "permission denied"):
		t.Errorf("the error names the server once, and the errno: %v", err)
	case strings.Contains(err.Error(), "locked-script"):
		t.Errorf("the error never names the word: %v", err)
	}
}

// Claude Code's file lists its projects, and each project's .mcp.json its servers. A .mcp.json that lists projects
// is read for its servers only, so two that list each other are read once; the home directory is a project too.
func TestMCPJsonFilesThatListEachOtherAreReadOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a, b := filepath.Join(home, "a"), filepath.Join(home, "b")
	write(t, filepath.Join(home, ".claude.json"), `{"projects":{"`+home+`":{},"`+a+`":{}}}`)
	write(t, filepath.Join(home, ".mcp.json"), `{"mcpServers":{"h":{"command":"`+filepath.Join(home, "h", "srv")+`"}}}`)
	write(t, filepath.Join(a, ".mcp.json"), `{"mcpServers":{"a":{"command":"`+filepath.Join(a, "srv")+`"}},"projects":{"`+b+`":{}}}`)
	write(t, filepath.Join(b, ".mcp.json"), `{"mcpServers":{"b":{"command":"`+filepath.Join(b, "srv")+`"}},"projects":{"`+a+`":{}}}`)
	n, err := LoadNamed(context.Background(), nil, NamesConfig{MCP: []string{filepath.Join(home, ".claude.json")}}, nil)
	must(t, err)
	for path, job := range map[string]string{filepath.Join(home, "h", "srv"): "mcp h", filepath.Join(a, "srv"): "mcp a"} {
		if j, ok := n.Protects(path); !ok || j != job {
			t.Errorf("%s is named by %q: %q %v", path, job, j, ok)
		}
	}
	if j, ok := n.Protects(filepath.Join(b, "srv")); ok {
		t.Errorf("a .mcp.json's projects are not read: named by %s", j)
	}
}

// Each way TOML and YAML write a server's table names it.
func TestEveryFormOfAnMCPServerTableIsRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, c := range []struct{ name, file, text string }{
		{"inline", "config.toml", "[mcp_servers]\nweb = { command = \"/h/web/srv\", args = [\"/h/web/a.js\"] }\n"},
		{"dotted", "config.toml", "model = \"x\"\nmcp_servers.web.command = \"/h/web/srv\"\nmcp_servers.web.args = [\"/h/web/a.js\"]\n"},
		{"flow", "config.yaml", "model: x\nmcp_servers: {web: {command: /h/web/srv, args: [/h/web/a.js], env: {K: v}}}\nother: {path: /h/not}\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), c.file)
			write(t, f, c.text)
			n, err := LoadNamed(context.Background(), nil, NamesConfig{MCP: []string{f}}, nil)
			must(t, err)
			for _, path := range []string{"/h/web/srv", "/h/web/a.js"} {
				if j, ok := n.Protects(path); !ok || j != "mcp web" {
					t.Errorf("%s is named by mcp web: %q %v", path, j, ok)
				}
			}
			if j, ok := n.Protects("/h/not"); ok {
				t.Errorf("/h/not is no server's: named by %s", j)
			}
		})
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
