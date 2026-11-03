package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostGitRejectsRedirectedStorageBeforeAnyProcess(t *testing.T) {
	for _, kind := range []string{"objects", "refs", "config", "alternates", "run-pointer"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			f := newRepo(t, nil)
			s := shipWithSandbox(st, f, Sandbox{})
			outside := t.TempDir()
			meta := filepath.Join(f.main, ".git")
			switch kind {
			case "objects", "refs", "config":
				if err := os.RemoveAll(filepath.Join(meta, kind)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(meta, kind)); err != nil {
					t.Fatal(err)
				}
			case "alternates":
				write(t, filepath.Join(meta, "objects", "info", "alternates"), outside+"\n")
			case "run-pointer":
				if err := os.RemoveAll(meta); err != nil {
					t.Fatal(err)
				}
				write(t, meta, "gitdir: "+filepath.Join(s.Dir, "runs", "fixture", "work", ".git")+"\n")
			}
			bin, marker := t.TempDir(), filepath.Join(t.TempDir(), "git-ran")
			script(t, bin, "git", "touch '"+marker+"'; exit 7")
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			if _, err := s.git(ctx, f.main, "cat-file", "-t", "HEAD"); err == nil {
				t.Fatal("host Git accepted redirected storage")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("Git started before storage was refused")
			}
		})
	}
}

func TestBuildCloneIsInitializedBeforeExposure(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := newRepo(t, nil)
	s := shipWithSandbox(st, f, Sandbox{})
	bin, record := t.TempDir(), filepath.Join(t.TempDir(), "git-calls")
	script(t, bin, "git", `printf '%s\n' "$@" >> '`+record+`'
exec /usr/bin/git "$@"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	p, err := s.Prepare(ctx, testingCase(t, st).ID, f.main)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "\n"+p.Work+"\n") {
		t.Fatal("host Git initialized the exposed run clone")
	}
	if _, err := s.git(ctx, p.Work, "status"); err == nil {
		t.Fatal("host Git entered an exposed run clone")
	}
}
