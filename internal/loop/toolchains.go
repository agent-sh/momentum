package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-sh/momentum/internal/sandbox"
)

// Toolchains supplies read-only compilers, module caches and test read roots.
// Build caches live in the unit's tmpfs, never in a shared writable mount.
type Toolchains struct {
	ReadOnly          []string
	Cache             string // reserved host storage, excluded from host repository reads
	Path              []string
	GoRoot, GoModules string
}

func testEnvironment() map[string]string {
	return map[string]string{
		"HOME": "/home/agent", "PATH": "/usr/bin:/bin", "TMPDIR": "/tmp", "LANG": "C.UTF-8",
		"GIT_TERMINAL_PROMPT": "0", "GOTOOLCHAIN": "local", "GOTELEMETRY": "off",
		"GOPROXY": "off", "GOSUMDB": "off", "GOFLAGS": "-buildvcs=false",
		"CARGO_NET_OFFLINE": "true", "GOCACHE": "/tmp/go-build", "GOPATH": "/tmp/go",
		"UV_CACHE_DIR": "/tmp/uv", "npm_config_cache": "/tmp/npm",
	}
}

func (t Toolchains) roots() ([]string, error) {
	var roots []string
	for _, p := range t.ReadOnly {
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			return nil, err
		}
		real, err = filepath.Abs(real)
		if err != nil {
			return nil, err
		}
		covered := false
		for _, r := range roots {
			covered = covered || under(real, r)
		}
		if covered || under(real, "/usr") {
			continue
		}
		for i := 0; i < len(roots); {
			if under(roots[i], real) {
				roots = append(roots[:i], roots[i+1:]...)
			} else {
				i++
			}
		}
		roots = append(roots, real)
	}
	return roots, nil
}

func rewriteToolPath(roots []string, p string) string {
	for i, root := range roots {
		if under(p, root) {
			return filepath.Join(fmt.Sprintf("/opt/toolchain-%d", i), strings.TrimPrefix(p, root))
		}
	}
	return p
}

func (t Toolchains) rewrite(p string) string {
	roots, err := t.roots()
	if err != nil {
		return p
	}
	return rewriteToolPath(roots, p)
}

func (t Toolchains) inputs() ([]sandbox.Bind, map[string]string, error) {
	roots, err := t.roots()
	if err != nil {
		return nil, nil, err
	}
	env := testEnvironment()
	var binds []sandbox.Bind
	for i, root := range roots {
		dst := fmt.Sprintf("/opt/toolchain-%d", i)
		binds = append(binds, sandbox.Bind{Src: root, Dst: dst})
		if st, err := os.Stat(filepath.Join(root, "bin", "go")); err == nil && st.Mode().IsRegular() {
			env["GOROOT"] = dst
		}
		if filepath.Base(root) == "mod" {
			env["GOMODCACHE"] = dst
		}
	}
	for name, source := range map[string]string{"GOROOT": t.GoRoot, "GOMODCACHE": t.GoModules} {
		if source == "" {
			continue
		}
		real, err := filepath.EvalSymlinks(source)
		if err != nil {
			return nil, nil, err
		}
		env[name] = rewriteToolPath(roots, real)
	}
	var paths []string
	for _, p := range t.Path {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		paths = append(paths, rewriteToolPath(roots, p))
	}
	env["PATH"] = strings.Join(append(paths, "/usr/bin", "/bin"), ":")
	return binds, env, nil
}
