package loop

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agent-sh/momentum/internal/sandbox"
	"github.com/agent-sh/momentum/internal/store"
)

type taskInputs struct {
	once      sync.Once
	mu        sync.Mutex
	remaining int
	dir       string
	store     string
	end       time.Time
	binds     []sandbox.Bind
	manifest  InputsManifest
	hashes    map[string]string
	err       error
}

func (in *taskInputs) close() {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.dir != "" {
		removeInputTree(in.dir)
		in.dir = ""
	}
}

func (in *taskInputs) release() {
	in.mu.Lock()
	in.remaining--
	done := in.remaining == 0
	in.mu.Unlock()
	if done {
		in.close()
	}
}

func (e *Eval) makeTask(ctx context.Context, in *taskInputs, t GoldenTask, repositories []string) {
	// Preparation has its own deadline. No model or reservation runs during it.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	in.err = e.buildTask(ctx, in, t, repositories)
	if in.err != nil {
		why, err := Redact(ctx, in.err.Error())
		if err != nil {
			why = "task inputs failed; diagnostic withheld"
		}
		in.err = fmt.Errorf("the task's as-of inputs: %s", why)
	}
}

func (e *Eval) buildTask(ctx context.Context, in *taskInputs, t GoldenTask, repositories []string) error {
	if err := CheckScrubber(ctx); err != nil {
		return err
	}
	end, err := e.snapshotEnd(ctx, t)
	if err != nil {
		return err
	}
	in.end = end
	in.dir = filepath.Join(e.Dir, "inputs", digest(e.ID + "\x00" + t.ID)[:16])
	if err := os.MkdirAll(filepath.Dir(in.dir), 0700); err != nil {
		return err
	}
	if err := os.Mkdir(in.dir, 0700); err != nil {
		return err
	}
	in.store = filepath.Join(in.dir, "store.sqlite")
	in.manifest.Store, err = StoreCopy(ctx, StoreCopyOptions{Source: e.Source, Destination: in.store, ScratchDir: in.dir,
		Since: end.Add(-StoreWindow), Until: end, OmitInPlace: true})
	if err != nil {
		return err
	}
	config := filepath.Join(in.dir, "config")
	in.manifest.Configuration, err = ConfigCopy(ctx, ConfigCopyOptions{Home: e.Runner.Inputs.Home, Destination: config, AsOf: end})
	if err != nil {
		return err
	}
	repos := filepath.Join(in.dir, "repos")
	for _, source := range repositories {
		if err := cloneSource(ctx, source, []string{e.Dir, e.Runner.Dir}); err != nil {
			return err
		}
	}
	if err := os.Mkdir(repos, 0700); err != nil {
		return err
	}
	clones, err := cloneRepositories(ctx, repositories, end, repos)
	if err != nil {
		return err
	}
	for _, clone := range clones {
		head, err := hostInputGit(ctx, clone, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		in.manifest.Repositories = append(in.manifest.Repositories, RepoClone{Name: filepath.Base(clone), Commit: strings.TrimSpace(string(head)), Branch: "main"})
	}
	in.binds = []sandbox.Bind{{Src: in.store, Dst: "/ro/store.sqlite"}, {Src: config, Dst: "/ro/config"}, {Src: repos, Dst: "/ro/repos"}}
	tacit := e.Tacit
	if tacit == "" {
		tacit = e.Runner.Inputs.TacitStore
	}
	if tacit != "" {
		if err := cloneSource(ctx, tacit, []string{e.Dir, e.Runner.Dir}); err != nil {
			return err
		}
		clone := filepath.Join(in.dir, "clone")
		made, err := repositoryAsOf(ctx, tacit, end, clone)
		if err != nil {
			return fmt.Errorf("tacit clone: %w", err)
		}
		if made {
			head, err := hostInputGit(ctx, clone, "rev-parse", "HEAD")
			if err != nil {
				return err
			}
			in.manifest.Tacit = RepoClone{Name: "clone", Branch: "main", Commit: strings.TrimSpace(string(head))}
			in.binds = append(in.binds, sandbox.Bind{Src: clone, Dst: "/ro/clone"})
		} else {
			in.manifest.Tacit.Error = "tacit's store held no commit before the end"
		}
	} else {
		in.manifest.Tacit.Error = "no tacit store configured"
	}
	// Recreate the host command's schema on a private copy of the scrubbed
	// snapshot. Its events are identical. No child reads the live store.
	home := filepath.Join(in.dir, ".records-home")
	if err := os.Mkdir(home, 0700); err != nil {
		return err
	}
	defer removeInputTree(home)
	path := filepath.Join(home, "momentum.db")
	if err := copyStore(in.store, path); err != nil {
		return err
	}
	st, err := store.Open(path)
	if err != nil {
		return err
	}
	var texts []string
	texts = append(texts, t.CaseTitle)
	for _, id := range t.Symptom.Query.EventIDs {
		ev, found, err := st.EventByID(ctx, strings.TrimPrefix(id, "ev:"))
		if err != nil || !found {
			st.Close()
			return fmt.Errorf("symptom absent from the as-of snapshot: %s", id)
		}
		texts = append(texts, ev.Summary)
	}
	if err := st.Close(); err != nil {
		return err
	}
	records := filepath.Join(in.dir, "records")
	in.manifest.Records, err = Records(ctx, RecordsOptions{Store: in.store, IDs: t.Symptom.Query.EventIDs, Units: FindUnits(texts...),
		Destination: records, Binary: e.programs.momentum, Home: home, AsOf: end, Env: CleanEnv(nil)})
	if err != nil {
		return err
	}
	in.binds = append(in.binds, sandbox.Bind{Src: records, Dst: "/ro/records"})
	in.hashes, err = inputHashes(in.binds)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(in.manifest, "", "  ")
	if err != nil {
		return err
	}
	return writeExclusive(in.dir, "manifest.json", raw)
}

// inputHashes records the full published capture, including frozen unit output.
func inputHashes(binds []sandbox.Bind) (map[string]string, error) {
	out := map[string]string{}
	for _, bind := range binds {
		if err := filepath.WalkDir(bind.Src, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(bind.Src, path)
			if err != nil {
				return err
			}
			key := bind.Dst
			if rel != "." {
				key += "/" + filepath.ToSlash(rel)
			}
			if entry.Type()&os.ModeSymlink != 0 {
				target, err := os.Readlink(path)
				if err != nil {
					return err
				}
				out[key] = digest("link:" + target)
				return nil
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			sum := sha256.New()
			_, copyErr := io.Copy(sum, f)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			out[key] = fmt.Sprintf("%x", sum.Sum(nil))
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return out, nil
}
