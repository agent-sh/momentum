package loop

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCustomGoModuleCacheHasAnExplicitEnvironmentName(t *testing.T) {
	root := filepath.Join(t.TempDir(), "compiler")
	modules := filepath.Join(t.TempDir(), "packages")
	write(t, filepath.Join(root, "bin", "go"), "marker-7f3a")
	write(t, filepath.Join(modules, "placeholder"), "marker-7f3a")
	binds, env, err := (Toolchains{ReadOnly: []string{root, modules}, Path: []string{filepath.Join(root, "bin")},
		GoRoot: root, GoModules: modules}).inputs()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"GOROOT", "GOMODCACHE", "GOCACHE", "PATH"} {
		if _, ok := env[name]; !ok {
			t.Fatalf("missing environment name: %s", name)
		}
	}
	admitted := false
	for _, bind := range binds {
		if bind.Write || !strings.HasPrefix(bind.Dst, "/opt/") {
			t.Fatal("toolchain bind is writable or outside its mount roots")
		}
		admitted = admitted || bind.Src == modules
	}
	if !admitted {
		t.Fatal("custom module cache is not mounted")
	}
}
