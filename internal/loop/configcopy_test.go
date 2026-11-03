package loop

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func configTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if info.Mode().Perm() != 0o555 {
				t.Errorf("directory mode %s: %o", path, info.Mode().Perm())
			}
			return nil
		}
		if info.Mode().Perm() != 0o444 {
			t.Errorf("file mode %s: %o", path, info.Mode().Perm())
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name, _ := filepath.Rel(dir, path)
		tree[name] = string(b)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestInputsConfigurationExactRootsAndGuards(t *testing.T) {
	goodInputScrubber(t)
	home := t.TempDir()
	allowed := []string{".claude/mods/source.js", ".claude/mods/flat/build", ".claude/hooks/check", ".claude/settings.json",
		".config/agents/rules.md", ".config/agents/notes.txt", ".config/agents/procedures/gpu.md", ".config/systemd/user/demo.service", ".config/systemd/user/demo.service.d/check.conf",
		".config/momentum/settings", ".local/bin/sh-script", ".local/bin/python-script", ".local/bin/node-script"}
	for _, name := range allowed {
		text := "marker-7f3a\n"
		switch filepath.Base(name) {
		case "sh-script":
			text = "#!/bin/sh\n" + text
		case "python-script":
			text = "#!/usr/bin/env python3\n" + text
		case "node-script":
			text = "#!/usr/bin/env -S node --no-warnings\n" + text
		}
		write(t, filepath.Join(home, name), text)
	}
	denied := []string{".claude.json", ".config/claude-code/bedrock.env", ".fireworks/state", ".ssh/config", ".aws/config",
		".config/agents/credential-store/file", ".config/agents/keyring/file", ".config/agents/a.TOKEN", ".config/agents/a.Secret",
		".config/agents/a.passwd", ".config/agents/a.pem", ".config/agents/a.key", ".config/agents/id_rsa", ".config/agents/a.env",
		".claude/mods/node_modules/pkg/index.js", ".claude/mods/dist/main.js", ".claude/mods/build/file", ".claude/mods/out/file",
		".claude/mods/target/file", ".claude/mods/.next/file", ".claude/mods/__pycache__/file", ".claude/mods/.git/file",
		".local/bin/data", ".local/bin/subdir/script", ".config/unrelated/file",
		".config/agents/backups/old.md", ".config/agents/receipts/r.md", ".config/agents/rules"}
	for _, name := range denied {
		write(t, filepath.Join(home, name), "do not copy\n")
	}
	// every harness's own configuration is taken by the allowlist, and what else its directory holds is not
	harnessAllowed := []string{".codex/config.toml", ".codex/work.config.toml", ".codex/AGENTS.md", ".codex/hooks.json", ".codex/rules/x.rules",
		".codex/skills/s/SKILL.md", ".codex/agents/a.toml", ".hermes/config.yaml", ".hermes/profile.yaml", ".hermes/SOUL.md",
		".hermes/skills/a/SKILL.md", ".hermes/scripts/x.py", ".hermes/plugins/p/index.js", ".hermes/hooks/h.sh", ".hermes/cron/jobs.json",
		".pi/agent/settings.json", ".pi/agent/AGENTS.md", ".pi/agent/extensions/e.ts", ".config/opencode/opencode.json",
		".config/opencode/cli.json", ".config/opencode/plugins/p.js", ".grok/config.toml", ".grok/AGENTS.md", ".grok/sandbox.toml",
		".grok/hooks/h.sh", ".grok/skills/s/SKILL.md"}
	harnessDenied := []string{".codex/auth.json", ".codex/auth.json.bak-1", ".codex/auth-profiles/a", ".codex/sessions/2026/rollout.jsonl",
		".codex/history.jsonl", ".codex/state_5.sqlite", ".codex/memories/m.md", ".codex/log/x", ".codex/config.toml.bak-1",
		".hermes/.env", ".hermes/auth.json", ".hermes/google_token.json", ".hermes/mcp-tokens/t", ".hermes/state.db", ".hermes/sessions/s.json",
		".hermes/plugins/p/node_modules/x.js", ".hermes/vault/v", ".hermes/mobile/m", ".pi/agent/auth.json", ".pi/agent/models.json",
		".pi/agent/sessions/s.jsonl", ".pi/agent/extensions/node_modules/x/index.js", ".config/opencode/auth.json",
		".config/opencode/node_modules/x/index.js", ".grok/auth.json", ".grok/sessions/s", ".grok/projects/p", ".grok/logs/l"}
	for _, name := range harnessAllowed {
		write(t, filepath.Join(home, name), "marker-7f3a\n")
	}
	for _, name := range harnessDenied {
		write(t, filepath.Join(home, name), "do not copy\n")
	}
	allowed = append(allowed, harnessAllowed...)
	denied = append(denied, harnessDenied...)
	write(t, filepath.Join(home, ".claude/hooks/binary"), "text\x00binary")
	write(t, filepath.Join(home, ".claude/hooks/invalid"), string([]byte{0xff, 0xfe}))
	write(t, filepath.Join(home, ".claude/hooks/oversized"), strings.Repeat("x", MaxConfigFileBytes+1))
	if err := syscall.Mkfifo(filepath.Join(home, ".claude/hooks/pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	write(t, filepath.Join(outside, "file"), "never read outside")
	if err := os.Symlink(filepath.Join(outside, "file"), filepath.Join(home, ".claude/hooks/link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, ".claude/mods/linked-dir")); err != nil {
		t.Fatal(err)
	}
	destRoot := t.TempDir()
	first := filepath.Join(destRoot, "first")
	second := filepath.Join(destRoot, "second")
	m, err := ConfigCopy(context.Background(), ConfigCopyOptions{Home: home, Destination: first})
	if err != nil {
		t.Fatal(err)
	}
	n, err := ConfigCopy(context.Background(), ConfigCopyOptions{Home: home, Destination: second})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m, n) || !reflect.DeepEqual(configTree(t, first), configTree(t, second)) {
		t.Fatal("configuration copies differ")
	}
	t.Cleanup(func() { removeInputTree(first); removeInputTree(second) })
	if len(m.Files) != len(allowed) || !m.Canary || m.Bytes > MaxConfigBytes {
		t.Fatalf("configuration manifest %+v", m)
	}
	tree := configTree(t, first)
	for _, name := range allowed {
		if !strings.Contains(tree[name], "[masked]") || strings.Contains(tree[name], "marker-7f3a") {
			t.Errorf("allowed file not scrubbed: %s", name)
		}
	}
	for _, name := range denied {
		if _, ok := tree[name]; ok {
			t.Errorf("denied file copied: %s", name)
		}
	}
	for reason, minimum := range map[string]int{"link": 2, "not text": 2, "not regular": 1, "file bytes": 1, "denied name": 9, "excluded directory": 8, "not a script": 1, "not a direct script": 1} {
		if m.SkippedByReason[reason] < minimum {
			t.Errorf("missing skip reason %s: %v", reason, m.SkippedByReason)
		}
	}
	total := 0
	for name, text := range tree {
		total += len(text)
		if name != "MANIFEST.txt" && strings.Contains(text, "never read outside") {
			t.Fatal("link followed outside Home")
		}
	}
	if m.Bytes != total {
		t.Fatal("configuration bytes omit manifest or files")
	}
}

func TestInputsConfigurationCapOrder(t *testing.T) {
	goodInputScrubber(t)
	home := t.TempDir()
	for i := range 34 {
		write(t, filepath.Join(home, ".claude/mods", "file"+strconv.FormatInt(int64(100+i), 10)), strings.Repeat("x", MaxConfigFileBytes))
	}
	write(t, filepath.Join(home, ".claude/hooks/later"), "small")
	dest := filepath.Join(t.TempDir(), "copy")
	m, err := ConfigCopy(context.Background(), ConfigCopyOptions{Home: home, Destination: dest})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeInputTree(dest) })
	// the later root keeps its small file; the big root keeps the sorted prefix that fits what is left
	if m.Bytes > MaxConfigBytes || len(m.Files) != 31 || m.CountsByRoot[".claude/hooks"] != 1 || m.CountsByRoot[".claude/mods"] != 30 || m.SkippedByReason["total bytes"] != 4 {
		t.Fatalf("cap/order manifest: files %d bytes %d counts %v skipped %v", len(m.Files), m.Bytes, m.CountsByRoot, m.SkippedByReason)
	}
	for i, file := range m.Files[:30] {
		if file.Path != filepath.Join(".claude/mods", "file"+strconv.Itoa(100+i)) {
			t.Fatal("configuration cap did not retain sorted prefix")
		}
	}
}

func TestInputsConfigurationCanaryFailureAndRootLink(t *testing.T) {
	inputScrubber(t, `cat`)
	home := t.TempDir()
	write(t, filepath.Join(home, ".claude/hooks/file"), "marker-7f3a")
	parent := t.TempDir()
	dest := filepath.Join(parent, "copy")
	if _, err := ConfigCopy(context.Background(), ConfigCopyOptions{Home: home, Destination: dest}); err == nil {
		t.Fatal("canary leak published")
	}
	if entries, err := os.ReadDir(parent); err != nil || len(entries) != 0 {
		t.Fatal("canary failure left input files")
	}
	goodInputScrubber(t)
	linkHome := t.TempDir()
	if err := os.Symlink(filepath.Join(home, ".claude"), filepath.Join(linkHome, ".claude")); err != nil {
		t.Fatal(err)
	}
	m, err := ConfigCopy(context.Background(), ConfigCopyOptions{Home: linkHome, Destination: dest})
	if err != nil || len(m.Files) != 0 || m.SkippedByReason["link"] != 3 {
		t.Fatalf("root link read: %+v %v", m, err)
	}
	t.Cleanup(func() { removeInputTree(dest) })
	homeLink := filepath.Join(parent, "home-link")
	if err := os.Symlink(home, homeLink); err != nil {
		t.Fatal(err)
	}
	if _, err := ConfigCopy(context.Background(), ConfigCopyOptions{Home: homeLink, Destination: filepath.Join(parent, "root-link-copy")}); err == nil {
		t.Fatal("Home link was followed")
	}
}

func TestInputsConfigurationAsOfTrackedFilesAndLeaks(t *testing.T) {
	goodInputScrubber(t)
	home := t.TempDir()
	repo := filepath.Join(home, ".config/agents")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	inputTestGit(t, repo, nil, "init", "-b", "main")
	file := filepath.Join(repo, "rules.md")
	write(t, file, "before marker-7f3a\n")
	write(t, filepath.Join(repo, "deleted.md"), "before deletion\n")
	inputTestGit(t, repo, nil, "add", ".")
	inputTestGit(t, repo, []string{"GIT_AUTHOR_DATE=2025-04-10T10:00:00Z", "GIT_COMMITTER_DATE=2025-04-10T10:00:00Z"}, "commit", "-m", "before")
	write(t, file, "after\n")
	if err := os.Remove(filepath.Join(repo, "deleted.md")); err != nil {
		t.Fatal(err)
	}
	inputTestGit(t, repo, nil, "add", ".")
	inputTestGit(t, repo, []string{"GIT_AUTHOR_DATE=2025-04-10T14:00:00Z", "GIT_COMMITTER_DATE=2025-04-10T14:00:00Z"}, "commit", "-m", "after")
	write(t, filepath.Join(repo, "untracked.md"), "not in the commit")
	write(t, filepath.Join(home, ".claude/hooks/live"), "live\n")
	dest := filepath.Join(t.TempDir(), "copy")
	m, err := ConfigCopy(context.Background(), ConfigCopyOptions{Home: home, Destination: dest, AsOf: time.Date(2025, 4, 10, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeInputTree(dest) })
	tree := configTree(t, dest)
	if tree[".config/agents/rules.md"] != "before [masked]\n" || tree[".config/agents/deleted.md"] != "before deletion\n" {
		t.Fatal("as-of copy read current files")
	}
	if _, ok := tree[".config/agents/untracked.md"]; ok {
		t.Fatal("as-of copy admitted untracked file")
	}
	if tree[".claude/hooks/live"] != "live\n" || !containsInputString(m.Leaks, ".claude/hooks") || containsInputString(m.Leaks, ".config/agents") {
		t.Fatalf("as-of leak accounting: %v", m.Leaks)
	}
}

func containsInputString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// The cap is shared by the roots: a big tree does not leave the later roots with nothing.
func TestInputsConfigurationCapIsSharedByTheRoots(t *testing.T) {
	goodInputScrubber(t)
	home := t.TempDir()
	for i := range 60 {
		write(t, filepath.Join(home, ".hermes/skills", "skill"+strconv.Itoa(100+i), "SKILL.md"), strings.Repeat("x", MaxConfigFileBytes-1))
	}
	write(t, filepath.Join(home, ".config/agents/rules.md"), "marker-7f3a rules")
	write(t, filepath.Join(home, ".config/systemd/user/demo.service"), "[Unit]\n")
	dest := filepath.Join(t.TempDir(), "copy")
	m, err := ConfigCopy(context.Background(), ConfigCopyOptions{Home: home, Destination: dest})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeInputTree(dest) })
	if m.CountsByRoot[".config/agents"] != 1 || m.CountsByRoot[".config/systemd/user"] != 1 || m.CountsByRoot[".hermes"] == 0 ||
		m.CountsByRoot[".hermes"] >= 60 || m.Bytes > MaxConfigBytes {
		t.Fatalf("the cap was not shared: %v bytes %d", m.CountsByRoot, m.Bytes)
	}
}

// Hundreds of thousands of skipped files do not make the manifest larger than the cap: the listing is bounded and the
// counts by reason cover them all.
func TestInputsConfigurationSkipListIsBounded(t *testing.T) {
	goodInputScrubber(t)
	home := t.TempDir()
	for i := range 3 * MaxConfigSkips {
		write(t, filepath.Join(home, ".claude/hooks", "log"+strconv.Itoa(i)+".log"), "x")
	}
	write(t, filepath.Join(home, ".claude/hooks/keep"), "marker-7f3a")
	dest := filepath.Join(t.TempDir(), "copy")
	m, err := ConfigCopy(context.Background(), ConfigCopyOptions{Home: home, Destination: dest})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeInputTree(dest) })
	if len(m.Skipped) != MaxConfigSkips || m.SkippedOmitted != 2*MaxConfigSkips || m.SkippedByReason["denied name"] != 3*MaxConfigSkips || len(m.Files) != 1 {
		t.Fatalf("skip list: %d listed, %d omitted, %v, %d files", len(m.Skipped), m.SkippedOmitted, m.SkippedByReason, len(m.Files))
	}
}

// A file that cannot be read does not end the copy.
func TestInputsConfigurationAnUnreadableFileIsSkipped(t *testing.T) {
	goodInputScrubber(t)
	home := t.TempDir()
	write(t, filepath.Join(home, ".claude/hooks/readable"), "marker-7f3a")
	write(t, filepath.Join(home, ".claude/hooks/locked"), "marker-7f3a")
	if err := os.Chmod(filepath.Join(home, ".claude/hooks/locked"), 0); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, ".claude/hooks/dir/inner"), "marker-7f3a")
	if err := os.Chmod(filepath.Join(home, ".claude/hooks/dir"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(home, ".claude/hooks/dir"), 0o700) })
	dest := filepath.Join(t.TempDir(), "copy")
	m, err := ConfigCopy(context.Background(), ConfigCopyOptions{Home: home, Destination: dest})
	if err != nil {
		t.Fatalf("an unreadable file ended the copy: %v", err)
	}
	t.Cleanup(func() { removeInputTree(dest) })
	if len(m.Files) != 1 || m.SkippedByReason["unreadable"] < 2 {
		t.Fatalf("unreadable files: %v, %d files", m.SkippedByReason, len(m.Files))
	}
}
