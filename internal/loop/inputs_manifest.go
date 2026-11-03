package loop

import (
	"fmt"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/plain"
)

// InputsManifest is the caller's manifest.json next to the run's inputs. The
// caller keeps Summary in the run's record. Paths name host-created inputs.
type InputsManifest struct {
	Store         StoreManifest     `json:"store"`
	Records       RecordsManifest   `json:"records"`
	Configuration ConfigManifest    `json:"configuration"`
	Repositories  []RepoInput       `json:"repositories"`
	Tacit         RepoInput         `json:"tacit"`
	Diffs         DiffsManifest     `json:"diffs"`
	Programs      map[string]string `json:"programs"`
}

// Summary says on one line what the run got. Counts only: no path, no masked text.
func (m InputsManifest) Summary() string {
	days := m.Store.Until.Sub(m.Store.Since) / (24 * time.Hour)
	store := fmt.Sprintf("store copy %d days, %s events", days, commaCount(m.Store.Events))
	switch {
	case m.Store.CacheError != "":
		store += fmt.Sprintf(" (scrub cache off: %s)", plain.Line(m.Store.CacheError))
	case m.Store.Cached+m.Store.Scrubbed > 0:
		store += fmt.Sprintf(" (%s rows scrubbed, %s cached)", commaCount(m.Store.Scrubbed), commaCount(m.Store.Cached))
	}
	var repos []string
	for _, repo := range m.Repositories {
		repos = append(repos, repo.describe())
	}
	repositories := "repositories none"
	if len(repos) > 0 {
		repositories = fmt.Sprintf("%d repositories (%s)", len(repos), strings.Join(repos, "; "))
	}
	diffs := "no diffs"
	if m.Diffs.Commits > 0 {
		diffs = fmt.Sprintf("diffs of %d commits for %d paths", m.Diffs.Commits, m.Diffs.Paths)
		if m.Diffs.Masked > 0 {
			diffs += fmt.Sprintf(" (%d masked)", m.Diffs.Masked)
		}
	} else if m.Diffs.Error != "" {
		diffs = "diffs absent: " + plain.Line(m.Diffs.Error)
	}
	tacit := "tacit " + m.Tacit.describeFacts()
	return fmt.Sprintf("%s; %d records; configuration %d files, %.1f MiB; %s; %s; %s",
		store, m.Records.Written, len(m.Configuration.Files), float64(m.Configuration.Bytes)/(1<<20), repositories, tacit, diffs)
}

// describe is one repository in a summary: its snapshot, or why it is absent.
func (r RepoInput) describe() string {
	name := plain.Line(r.Name)
	if r.Error != "" || r.Commit == "" {
		reason := plain.Line(r.Error)
		if reason == "" {
			reason = "not read"
		}
		return fmt.Sprintf("%s absent: %s", name, reason)
	}
	return name + " " + r.snapshotText()
}

// describeFacts is tacit's store in a summary.
func (r RepoInput) describeFacts() string {
	if r.Error != "" || r.Commit == "" {
		reason := plain.Line(r.Error)
		if reason == "" {
			reason = "not read"
		}
		return "absent: " + reason
	}
	return r.snapshotText()
}

func (r RepoInput) snapshotText() string {
	commit := plain.Line(r.Commit)
	if len(commit) > 7 {
		commit = commit[:7]
	}
	text := fmt.Sprintf("%s files, %.1f MiB at %s", commaCount(r.Files), float64(r.Bytes)/(1<<20), commit)
	if r.Masked > 0 {
		text += fmt.Sprintf(", %d masked", r.Masked)
	}
	if n := r.skipped(); n > 0 {
		text += fmt.Sprintf(", %d left out", n)
	}
	switch {
	case r.History.Error != "":
		text += ", no log"
	default:
		text += fmt.Sprintf(", log %d commits", r.History.Commits)
	}
	return text
}

func (r RepoInput) skipped() (n int) {
	for _, v := range r.Skipped {
		n += v
	}
	return n
}

func commaCount(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
