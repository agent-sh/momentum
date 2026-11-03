package loop

import (
	"fmt"
	"strings"
	"time"
)

// InputsManifest is the caller's manifest.json next to the run's inputs. The
// caller keeps Summary in the run's record. Paths name host-created inputs.
type InputsManifest struct {
	Store         StoreManifest   `json:"store"`
	Records       RecordsManifest `json:"records"`
	Configuration ConfigManifest  `json:"configuration"`
	Repositories  []RepoClone     `json:"repositories"`
	Tacit         RepoClone       `json:"tacit"`
}

func (m InputsManifest) Summary() string {
	days := m.Store.Until.Sub(m.Store.Since) / (24 * time.Hour)
	repos, refused := 0, 0
	for _, repo := range m.Repositories {
		if repo.Error == "" && repo.Commit != "" {
			repos++
		} else if repo.Error != "" {
			refused++
		}
	}
	tacit := m.Tacit.Commit
	if tacit == "" || m.Tacit.Error != "" {
		tacit = "absent"
	} else if len(tacit) > 7 {
		tacit = tacit[:7]
	}
	// A manifest is host data, but keep even a malformed commit on one line.
	tacit = strings.Map(func(r rune) rune {
		if r <= ' ' || r == 127 {
			return -1
		}
		return r
	}, tacit)
	repositories := fmt.Sprintf("%d repositories", repos)
	if refused > 0 {
		repositories += fmt.Sprintf(" (%d refused)", refused)
	}
	return fmt.Sprintf("store copy %d days, %s events; %d records; configuration %d files, %.1f MiB; %s; tacit at %s",
		days, commaCount(m.Store.Events), m.Records.Written, len(m.Configuration.Files),
		float64(m.Configuration.Bytes)/(1<<20), repositories, tacit)
}

func commaCount(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
