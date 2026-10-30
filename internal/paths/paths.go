// Package paths resolves where momentum keeps its state on the machine. Nothing personal lives in the
// repository; cases, evidence and the event store live under the XDG directories, never under /tmp.
package paths

import (
	"os"
	"path/filepath"
)

// Home is the user's home directory, or "." when it cannot be found.
func Home() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "."
}

func xdg(env, fallback string) string {
	if v := os.Getenv(env); v != "" && filepath.IsAbs(v) {
		return v
	}
	return filepath.Join(Home(), fallback)
}

// Data is momentum's data directory: $XDG_DATA_HOME/momentum, default ~/.local/share/momentum.
// MOMENTUM_HOME overrides it, which tests use.
func Data() string {
	if v := os.Getenv("MOMENTUM_HOME"); v != "" {
		return v
	}
	return filepath.Join(xdg("XDG_DATA_HOME", ".local/share"), "momentum")
}

// DB is the event and case store.
func DB() string { return filepath.Join(Data(), "momentum.db") }

// Config is momentum's configuration directory: $XDG_CONFIG_HOME/momentum, default ~/.config/momentum.
// MOMENTUM_CONFIG overrides it, which tests use.
func Config() string {
	if v := os.Getenv("MOMENTUM_CONFIG"); v != "" {
		return v
	}
	return filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "momentum")
}

// Expand turns a leading ~/, or ~ alone, into the home directory.
func Expand(p string) string {
	if p == "~" {
		return Home()
	}
	if len(p) >= 2 && p[:2] == "~/" {
		return filepath.Join(Home(), p[2:])
	}
	return p
}
