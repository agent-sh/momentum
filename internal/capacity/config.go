// Package capacity keeps the machine's disks from filling: it samples which paths processes use, records each
// disk's use at every run, and deletes what its rules find idle and rebuildable (models and caches idle for a
// month, Rust build output idle for two days, the uv and Docker caches, worktrees whose work is merged), and
// offloads tasks' idle scratch folders to restic. What it cannot handle it reports. Every path is the machine's
// configuration; nothing is compiled in.
package capacity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/config"
	"github.com/agent-sh/momentum/internal/paths"
)

// The rules, by name. Disk is the history of each disk's use and the writers behind a fast fill; it deletes
// nothing and always runs.
const (
	RuleModels    = "models"
	RuleTargets   = "targets"
	RuleUV        = "uv"
	RuleDocker    = "docker"
	RuleWorktrees = "worktrees"
	RuleScratch   = "scratch"
	RuleDisk      = "disk"
)

// Rules are the rules that delete, in the order a run takes them.
var Rules = []string{RuleModels, RuleTargets, RuleWorktrees, RuleScratch, RuleUV, RuleDocker}

// Config is the "capacity" section of sources.json.
type Config struct {
	// Disks are the mount points whose use each run records, e.g. "/" and "/data".
	Disks []string `json:"disks"`
	// Live names the rules that delete. A rule not named lists what it would delete and deletes nothing.
	Live []string `json:"live"`
	// Exclude names the projects whose paths every rule leaves alone and reports (but models: the owner's
	// ruling on models covers every project).
	Exclude Exclude `json:"exclude"`
	// Names are where the scheduled jobs that may name a path are kept, beside systemd's units and the crontab.
	Names NamesConfig `json:"names"`
	// MinMiB is the smallest item the models and targets rules delete: a smaller one frees nothing, and the small
	// entries at the top of a cache are its markers, locks and credentials.
	MinMiB float64 `json:"min_mib"`

	Models    ModelsConfig    `json:"models"`
	Targets   TargetsConfig   `json:"targets"`
	Worktrees WorktreesConfig `json:"worktrees"`
	Scratch   ScratchConfig   `json:"scratch"`
	UV        UVConfig        `json:"uv"`
	Docker    DockerConfig    `json:"docker"`
	Fill      FillConfig      `json:"fill"`
}

// Exclude names projects by their paths, by a glob any component of a path may match, and by their git remotes
// (owner/repo).
type Exclude struct {
	Paths   []string `json:"paths"`
	Names   []string `json:"names"`
	Remotes []string `json:"remotes"`
}

// NamesConfig locates Hermes's scheduled jobs (its jobs file and the directory its job scripts are in) and the
// directories of installed commands, whose symlinks name what they run.
type NamesConfig struct {
	HermesJobs    string   `json:"hermes_jobs"`
	HermesScripts string   `json:"hermes_scripts"`
	Links         []string `json:"links"`
}

// ModelsConfig is the models rule: models, datasets, model caches and run checkpoints. Each top-level item of a
// root that has had no write and was not seen in use for IdleDays is deleted. A root inside another root is not
// an item of it; its own entries are. An item in Keep is left.
type ModelsConfig struct {
	Roots      []string `json:"roots"`
	Keep       []string `json:"keep"`
	IdleDays   float64  `json:"idle_days"`
	EveryHours float64  `json:"every_hours"`
}

// TargetsConfig is the targets rule: Rust build output (a Cargo target directory) under Roots that has had no
// write for IdleHours.
type TargetsConfig struct {
	Roots      []string `json:"roots"`
	IdleHours  float64  `json:"idle_hours"`
	EveryHours float64  `json:"every_hours"`
}

// WorktreesConfig is the worktrees rule: linked git worktrees under Roots (globs) whose branch is merged or gone
// from its remote, and that are clean and pushed.
type WorktreesConfig struct {
	Roots      []string `json:"roots"`
	EveryHours float64  `json:"every_hours"`
}

// ScratchConfig is the scratch rule: tasks' folders at the top of Roots (the cache directory agents keep their
// scratch in). An idle one's model data goes after the models rule's idle time and the rest is offloaded with
// Offload (evidence-offload, which backs a folder up to restic and leaves NAME.restic.txt in its place) after
// IdleDays, tagged Tag. A folder in Keep, or holding a path in it, stays. Offload empty finds evidence-offload on
// PATH.
type ScratchConfig struct {
	Roots      []string `json:"roots"`
	Keep       []string `json:"keep"`
	IdleDays   float64  `json:"idle_days"`
	Offload    string   `json:"offload"`
	Tag        string   `json:"tag"`
	EveryHours float64  `json:"every_hours"`
}

// UVConfig is the uv rule: `uv cache prune --ci` when the cache is over MaxGiB, or EveryDays after the last
// prune. Path is the uv binary; empty finds it on PATH.
type UVConfig struct {
	Path       string  `json:"path"`
	MaxGiB     float64 `json:"max_gib"`
	EveryDays  float64 `json:"every_days"`
	EveryHours float64 `json:"every_hours"`
}

// DockerConfig is the docker rule: the build cache older than Until, and dangling images.
type DockerConfig struct {
	Path       string  `json:"path"`
	Until      string  `json:"until"`
	EveryHours float64 `json:"every_hours"`
}

// FillConfig is when a disk fills fast: more than GiBPerHour over the last Window, or Full percent due within
// WithinHours at that rate when it is at least MinGiBPerHour (a creep that slow is the full note's, at the line).
type FillConfig struct {
	GiBPerHour    float64 `json:"gib_per_hour"`
	MinGiBPerHour float64 `json:"min_gib_per_hour"`
	Full          float64 `json:"full_pct"`
	WithinHours   float64 `json:"within_hours"`
	WindowMin     float64 `json:"window_min"`
}

// ErrNoCapacity means sources.json has no capacity section, and ErrBadConfig one this build cannot use.
var (
	ErrNoCapacity = errors.New("no capacity section in sources.json")
	ErrBadConfig  = errors.New("bad capacity configuration")
)

// Load reads the capacity section of sources.json, fills the defaults and expands a leading ~/ in every path.
func Load() (Config, error) {
	raw, err := os.ReadFile(config.SourcesPath())
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("%w: write %s", ErrNoCapacity, config.SourcesPath())
	}
	if err != nil {
		return Config{}, err
	}
	var f struct {
		Capacity json.RawMessage `json:"capacity"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return Config{}, fmt.Errorf("%w: %s: %v", ErrBadConfig, config.SourcesPath(), err)
	}
	if len(f.Capacity) == 0 || string(f.Capacity) == "null" {
		return Config{}, fmt.Errorf("%w (%s)", ErrNoCapacity, config.SourcesPath())
	}
	// A key this build does not know is refused: a typo in "exclude" or "keep" would drop a protection silently.
	var c Config
	dec := json.NewDecoder(bytes.NewReader(f.Capacity))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%w: %s: capacity: %v", ErrBadConfig, config.SourcesPath(), err)
	}
	if err := c.check(); err != nil {
		return Config{}, fmt.Errorf("%w: %s: capacity: %v", ErrBadConfig, config.SourcesPath(), err)
	}
	return c.Normal(), nil
}

// check refuses a live rule momentum does not know: a typo would leave a rule off without a word.
func (c Config) check() error {
	for _, r := range c.Live {
		if !slices.Contains(Rules, r) {
			return fmt.Errorf("live names %q, which is not a rule (%s)", r, strings.Join(Rules, ", "))
		}
	}
	if c.Docker.Until != "" {
		if _, err := time.ParseDuration(c.Docker.Until); err != nil {
			return fmt.Errorf("docker.until: %w", err)
		}
	}
	return nil
}

// The defaults are the owner's and the disk study's: models idle for 30 days, Rust build output idle for 48 hours,
// tasks' folders idle for 21 days (longer than the pauses after which a task comes back to its folder),
// the uv cache over 30 GiB or weekly, Docker's build cache older than 168 hours, and a fill faster than 20 GiB an
// hour or one that reaches 94% within 6 hours. MinMiB leaves the small entries at the top of a cache, its markers,
// locks and credentials, which free nothing.
const (
	defaultIdleDays    = 30
	defaultMinMiB      = 1
	defaultTargetHours = 48
	defaultScratchDays = 21
	defaultScratchTag  = "scratch-evidence"
	defaultUVMaxGiB    = 30
	defaultUVEveryDays = 7
	defaultDockerUntil = "168h"
	defaultFillGiBHour = 20
	defaultFillFull    = 94
	defaultFillWithin  = 6
	defaultFillWindow  = 30
	defaultScanHours   = 1
	defaultDockerHours = 24
)

// Normal is the configuration with its defaults filled and every path expanded.
func (c Config) Normal() Config {
	set := func(v *float64, d float64) {
		if *v <= 0 {
			*v = d
		}
	}
	set(&c.Models.IdleDays, defaultIdleDays)
	set(&c.MinMiB, defaultMinMiB)
	set(&c.Models.EveryHours, defaultScanHours)
	set(&c.Targets.IdleHours, defaultTargetHours)
	set(&c.Targets.EveryHours, defaultScanHours)
	set(&c.Worktrees.EveryHours, defaultScanHours)
	set(&c.Scratch.IdleDays, defaultScratchDays)
	set(&c.Scratch.EveryHours, defaultScanHours)
	if c.Scratch.Tag == "" {
		c.Scratch.Tag = defaultScratchTag
	}
	set(&c.UV.MaxGiB, defaultUVMaxGiB)
	set(&c.UV.EveryDays, defaultUVEveryDays)
	set(&c.UV.EveryHours, defaultScanHours)
	set(&c.Docker.EveryHours, defaultDockerHours)
	set(&c.Fill.GiBPerHour, defaultFillGiBHour)
	set(&c.Fill.MinGiBPerHour, c.Fill.GiBPerHour/20)
	set(&c.Fill.Full, defaultFillFull)
	set(&c.Fill.WithinHours, defaultFillWithin)
	set(&c.Fill.WindowMin, defaultFillWindow)
	if c.Docker.Until == "" {
		c.Docker.Until = defaultDockerUntil
	}
	exp := func(ps []string) []string {
		out := make([]string, 0, len(ps))
		for _, p := range ps {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, paths.Expand(p))
			}
		}
		return out
	}
	c.Disks = exp(c.Disks)
	c.Exclude.Paths = exp(c.Exclude.Paths)
	c.Models.Roots = exp(c.Models.Roots)
	c.Models.Keep = exp(c.Models.Keep)
	c.Targets.Roots = exp(c.Targets.Roots)
	c.Worktrees.Roots = exp(c.Worktrees.Roots)
	c.Scratch.Roots = exp(c.Scratch.Roots)
	c.Scratch.Keep = exp(c.Scratch.Keep)
	c.Scratch.Offload = paths.Expand(c.Scratch.Offload)
	c.Names.HermesJobs = paths.Expand(c.Names.HermesJobs)
	c.Names.HermesScripts = paths.Expand(c.Names.HermesScripts)
	c.Names.Links = exp(c.Names.Links)
	c.UV.Path = paths.Expand(c.UV.Path)
	c.Docker.Path = paths.Expand(c.Docker.Path)
	return c
}

// IsLive reports whether a rule deletes.
func (c Config) IsLive(rule string) bool { return slices.Contains(c.Live, rule) }

// every is a rule's cadence.
func (c Config) every(rule string) time.Duration {
	h := map[string]float64{RuleModels: c.Models.EveryHours, RuleTargets: c.Targets.EveryHours,
		RuleWorktrees: c.Worktrees.EveryHours, RuleScratch: c.Scratch.EveryHours, RuleUV: c.UV.EveryHours,
		RuleDocker: c.Docker.EveryHours}[rule]
	return time.Duration(h * float64(time.Hour))
}

// configured reports whether a rule has what it needs to run on this machine.
func (c Config) configured(rule string) bool {
	switch rule {
	case RuleModels:
		return len(c.Models.Roots) > 0
	case RuleTargets:
		return len(c.Targets.Roots) > 0
	case RuleWorktrees:
		return len(c.Worktrees.Roots) > 0
	case RuleScratch:
		return len(c.Scratch.Roots) > 0
	}
	return true
}
