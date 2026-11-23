package scorecard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
)

// A system is off while its off-file exists. tacit honors its own (its hooks do nothing, its timers' jobs exit at
// once, its MCP server answers that it is off); momentum's loop and supervision honor momentum's. A shutdown writes
// the file, stops the system's model-spending timers, and says how to turn it back on. momentum's ingest and this
// weekly run stay: they are cheap, and they are what judges tacit.

// TacitOffFile is the file that turns tacit off: $XDG_CONFIG_HOME/tacit/off, default ~/.config/tacit/off.
func TacitOffFile() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" || !filepath.IsAbs(dir) {
		dir = filepath.Join(paths.Home(), ".config")
	}
	return filepath.Join(dir, "tacit", "off")
}

// MomentumOffFile is the file that turns momentum's loop and supervision off: off in momentum's configuration
// directory.
func MomentumOffFile() string { return filepath.Join(paths.Config(), "off") }

// MomentumOff says whether momentum's loop and supervision are off, and why (the file's first line).
func MomentumOff() (bool, string) { return isOff(MomentumOffFile()) }

func isOff(path string) (bool, string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return errors.Is(err, os.ErrPermission), ""
	}
	first, _, _ := strings.Cut(string(b), "\n")
	return true, strings.TrimSpace(first)
}

// timerPatterns are the timers a shutdown stops, by system: tacit's dreamer and curator, which spend on models, and
// momentum's loop with any supervision timer of its own. The rest stays.
var timerPatterns = map[string][]string{
	Tacit:    {"tacit-dream*.timer", "tacit-curate*.timer"},
	Momentum: {"momentum-loop*.timer", "momentum-supervise*.timer", "momentum-investigat*.timer"},
}

// Switch turns the systems off.
type Switch struct {
	TacitOff, MomentumOff string
	// Systemctl runs systemctl --user with the arguments and returns its output.
	Systemctl func(ctx context.Context, args ...string) ([]byte, error)
}

// DefaultSwitch is the machine's: the systems' own off-files and the user manager.
func DefaultSwitch() Switch {
	return Switch{TacitOff: TacitOffFile(), MomentumOff: MomentumOffFile(), Systemctl: func(ctx context.Context, args ...string) ([]byte, error) {
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		return exec.CommandContext(cctx, "systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	}}
}

// OffFile is a system's off-file.
func (s Switch) OffFile(system string) string {
	if system == Tacit {
		return s.TacitOff
	}
	return s.MomentumOff
}

// Off says whether a system is off now.
func (s Switch) Off(system string) bool {
	off, _ := isOff(s.OffFile(system))
	return off
}

// Shutdown is what turning a system off did.
type Shutdown struct {
	System  string
	OffFile string
	Units   []string // the timers stopped and disabled
	Failed  []string // what could not be done, each with why
	Restore string   // the commands that turn it back on
}

// Shut turns a system off: its off-file, with why, when and the command that turns it back on, then its timers.
// The off-file comes first: it alone stops the system, the timers only spare their empty runs.
func (s Switch) Shut(ctx context.Context, system, week, why string, now time.Time) (Shutdown, error) {
	sd := Shutdown{System: system, OffFile: s.OffFile(system)}
	units, err := s.units(ctx, system)
	if err != nil {
		sd.Failed = append(sd.Failed, "listing its timers: "+err.Error())
	}
	sd.Restore = "rm " + sd.OffFile
	if len(units) > 0 {
		sd.Restore += " && systemctl --user enable --now " + strings.Join(units, " ")
	}
	text := fmt.Sprintf("%s is off: the momentum scorecard shut it down after %d failing weeks in a row.\nwhy: %s\ndate: %s\nweek: %s\nturn it back on: %s\n",
		system, FailingWeeks, why, now.Format(time.RFC3339), week, sd.Restore)
	if err := os.MkdirAll(filepath.Dir(sd.OffFile), 0o755); err != nil {
		return sd, err
	}
	if err := os.WriteFile(sd.OffFile, []byte(text), 0o644); err != nil {
		return sd, err
	}
	for _, u := range units {
		if out, err := s.Systemctl(ctx, "disable", "--now", u); err != nil {
			sd.Failed = append(sd.Failed, fmt.Sprintf("disable %s: %v %s", u, err, strings.TrimSpace(string(out))))
			continue
		}
		sd.Units = append(sd.Units, u)
	}
	return sd, nil
}

// units are the system's installed timers that a shutdown stops.
func (s Switch) units(ctx context.Context, system string) ([]string, error) {
	out, err := s.Systemctl(ctx, append([]string{"list-unit-files", "--no-legend", "--plain", "--type=timer"}, timerPatterns[system]...)...)
	if err != nil && len(bytes.TrimSpace(out)) == 0 {
		return nil, nil // none matches: systemctl exits 1 on an empty list
	}
	var units []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && strings.HasSuffix(f[0], ".timer") && !slices.Contains(units, f[0]) {
			units = append(units, f[0])
		}
	}
	return units, nil
}
