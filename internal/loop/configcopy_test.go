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
		".config/agents/rules", ".config/systemd/user/demo.service", ".config/systemd/user/demo.service.d/check.conf",
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
		".local/bin/data", ".local/bin/subdir/script", ".config/unrelated/file"}
	for _, name := range denied {
		write(t, filepath.Join(home, name), "do not copy\n")
	}
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
	if m.Bytes > MaxConfigBytes || len(m.Files) != 31 || m.CountsByRoot[".claude/hooks"] != 0 || m.SkippedByReason["total bytes"] != 4 {
		t.Fatalf("cap/order manifest: files %d bytes %d skipped %v", len(m.Files), m.Bytes, m.SkippedByReason)
	}
	for i, file := range m.Files {
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
	file := filepath.Join(repo, "rules")
	write(t, file, "before marker-7f3a\n")
	write(t, filepath.Join(repo, "deleted"), "before deletion\n")
	inputTestGit(t, repo, nil, "add", ".")
	inputTestGit(t, repo, []string{"GIT_AUTHOR_DATE=2025-04-10T10:00:00Z", "GIT_COMMITTER_DATE=2025-04-10T10:00:00Z"}, "commit", "-m", "before")
	write(t, file, "after\n")
	if err := os.Remove(filepath.Join(repo, "deleted")); err != nil {
		t.Fatal(err)
	}
	inputTestGit(t, repo, nil, "add", ".")
	inputTestGit(t, repo, []string{"GIT_AUTHOR_DATE=2025-04-10T14:00:00Z", "GIT_COMMITTER_DATE=2025-04-10T14:00:00Z"}, "commit", "-m", "after")
	write(t, filepath.Join(repo, "untracked"), "not in the commit")
	write(t, filepath.Join(home, ".claude/hooks/live"), "live\n")
	dest := filepath.Join(t.TempDir(), "copy")
	m, err := ConfigCopy(context.Background(), ConfigCopyOptions{Home: home, Destination: dest, AsOf: time.Date(2025, 4, 10, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeInputTree(dest) })
	tree := configTree(t, dest)
	if tree[".config/agents/rules"] != "before [masked]\n" || tree[".config/agents/deleted"] != "before deletion\n" {
		t.Fatal("as-of copy read current files")
	}
	if _, ok := tree[".config/agents/untracked"]; ok {
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
