package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ErrRefused is a gate's refusal: the write is outside the loop's authority, now or ever. The agent's run ends its
// try there; nothing else may route around it.
var ErrRefused = errors.New("refused")

func refused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

// shipHold distinguishes authority and lock holds from invalid agent output.
// Both remain ErrRefused to callers of the ship tool.
type shipHold struct{ err error }

func (e *shipHold) Error() string { return e.err.Error() }
func (e *shipHold) Unwrap() error { return e.err }

func held(err error) bool {
	var hold *shipHold
	return errors.As(err, &hold)
}

// Gate holds the loop's authority, as the machine's configuration gives it. The loop never changes it: its
// configuration lives outside every repository it may write, and its code is outside what the ship tool may
// change (Protected).
type Gate struct {
	Owners    []string // repository owners it may write to
	Exclude   []string // OWNER/REPO it never touches
	RepoGuard []string // the machine's read-only scope check, run before any repository write
	// Busy names what works in a repository now (a live session, a process, a recent change), or "" when nothing
	// does. Only host repositories enter this check.
	Busy func(ctx context.Context, repoDir string) (string, error)
	Home string
	// Resolve names a checkout's repository (OWNER/REPO); RepoOf, by its GitHub origin, when nil.
	Resolve func(ctx context.Context, dir string) (string, error)
}

// Target is where a change goes: a repository, by its owner and name, and the checkout the loop starts from.
type Target struct {
	Repo  string // OWNER/REPO
	Dir   string // the repository's main checkout
	Owner string
}

var remoteRe = regexp.MustCompile(`^(?:https://github\.com/|git@github\.com:|ssh://git@github\.com/)([\w.-]+)/([\w.-]+?)(?:\.git)?/?$`)

// originRepository resolves the fetch URL and every expanded push URL. A custom resolver may name a
// local test repository, but its destinations must still agree.
func originRepository(ctx context.Context, dir string) (string, error) {
	fetch, err := gitOutput(ctx, dir, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	pushes, err := gitOutput(ctx, dir, "remote", "get-url", "--push", "--all", "origin")
	if err != nil {
		return "", err
	}
	repo := remoteIdentity(fetch)
	urls := strings.Split(strings.TrimSpace(pushes), "\n")
	for _, url := range urls {
		if url == "" || remoteIdentity(url) != repo {
			return "", refused("origin's fetch and push destinations differ")
		}
	}
	return repo, nil
}

// remoteIdentity is the repository a remote URL names: OWNER/REPO, in lower case, for GitHub's https and ssh forms,
// and url: with the URL for any other.
func remoteIdentity(url string) string {
	url = strings.TrimSpace(url)
	if m := remoteRe.FindStringSubmatch(url); m != nil {
		return strings.ToLower(m[1] + "/" + m[2])
	}
	return "url:" + url
}

// repo names the checkout's repository (OWNER/REPO) with the gate's resolver.
func (g Gate) repo(ctx context.Context, dir string) (string, error) {
	if g.Resolve != nil {
		return g.Resolve(ctx, dir)
	}
	return RepoOf(ctx, dir)
}

// RepoOf is a checkout's repository on GitHub, shared by its fetch and all push destinations.
func RepoOf(ctx context.Context, dir string) (string, error) {
	repo, err := originRepository(ctx, dir)
	if err != nil {
		return "", err
	}
	if !regexp.MustCompile(`^[\w.-]+/[\w.-]+$`).MatchString(repo) {
		return "", refused("origin is not a GitHub repository")
	}
	return repo, nil
}

// policy is the part of Repository that only the configuration decides: an owner the loop may write to, a
// repository it does not exclude, a scope check to run. What it refuses stays refused until the configuration
// changes, so the runner checks it before it picks a build (heldBuild).
func (g Gate) policy(repo string) error {
	owner := strings.SplitN(repo, "/", 2)[0]
	switch {
	case !slices.ContainsFunc(g.Owners, func(o string) bool { return strings.EqualFold(o, owner) }):
		return refused("%s is not of an owner the loop may write to (loop.owners): report only", repo)
	case slices.ContainsFunc(g.Exclude, func(x string) bool { return strings.EqualFold(x, repo) }):
		return refused("%s is excluded (loop.exclude): report only", repo)
	case len(g.RepoGuard) == 0:
		return refused("no scope check is configured (loop.repo_guard): no repository write")
	}
	return nil
}

// Repository checks that the loop may change a repository now, and returns it as a target: an owner it may write
// to, a repository it does not exclude, the machine's scope check allows it, and nothing else works in it (the
// scheduled-AI policy leaves a repository another agent or a human works on alone).
func (g Gate) Repository(ctx context.Context, dir string) (target Target, err error) {
	defer func() {
		if errors.Is(err, ErrRefused) && !held(err) {
			err = &shipHold{err: err}
		}
	}()
	if err := CheckHostStorage(ctx, dir); err != nil {
		return Target{}, err
	}
	if err := CheckGitConfig(ctx, dir); err != nil {
		return Target{}, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return Target{}, err
	}
	main, err := mainCheckout(ctx, dir)
	if err != nil {
		return Target{}, refused("%s is not a git checkout", dir)
	}
	if _, err := originRepository(ctx, dir); err != nil {
		return Target{}, err
	}
	repo, err := g.repo(ctx, dir)
	if err != nil {
		return Target{}, err
	}
	owner := strings.SplitN(repo, "/", 2)[0]
	if err := g.policy(repo); err != nil {
		return Target{}, err
	}
	if err := g.scope(ctx, repo, main); err != nil {
		return Target{}, err
	}
	if g.Busy != nil {
		what, err := g.Busy(ctx, main)
		if err != nil {
			return Target{}, refused("cannot tell whether someone works in %s (%v): report only", repo, err)
		}
		if what != "" {
			return Target{}, refused("%s is in use: %s; the loop leaves it alone", repo, what)
		}
	}
	return Target{Repo: repo, Dir: main, Owner: owner}, nil
}

// undecidedRe is a scope check's reason that says it could not decide: what it needed to know about the repository
// (its activity, its provenance, its identity, who works in it) is unknown, unavailable, uncertain or inaccessible, or
// could not be verified or established.
var undecidedRe = regexp.MustCompile(`(?i)\b(?:unknown|unavailable|uncertain|inaccessible)\b|\b(?:could not|cannot|can't|unable to) (?:verify|determine|establish|tell)\b`)

// scope runs the machine's read-only scope check for scheduled repository work. Its decision is the write's: an
// exit of 0 that allows it, or a refusal, which a disposition of report only is. An answer that decides nothing holds
// the build for this pass, as a repository in use does (Repository): no decision to read (an output that is none, or
// no allowed in it), an allow with an exit other than 0, or report only for reasons that each say the check could not
// tell (undecidedRe), such as activity it could not read. That is the host's condition, which the next pass asks
// again; a report only for any other reason (excluded, inactive, another's) is decided, and refuses the write.
func (g Gate) scope(ctx context.Context, repo, dir string) error {
	if err := CheckGitConfig(ctx, dir); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, g.RepoGuard[0], append(append([]string{}, g.RepoGuard[1:]...), "--repo", repo, "--path", dir)...)
	cmd.Env = gitChildEnv()
	out, diag := Capture{Limit: MaxStream}, Capture{Limit: MaxStream}
	cmd.Stdout, cmd.Stderr = &out, &diag
	err := cmd.Run()
	if out.Over() || diag.Over() {
		return refused("scope output exceeds the whole-stream limit")
	}
	var v struct {
		Allowed     *bool    `json:"allowed"`
		Disposition string   `json:"disposition"`
		Reasons     []string `json:"reasons"`
	}
	jerr := json.Unmarshal(out.Bytes(), &v)
	why := strings.Join(v.Reasons, "; ")
	switch {
	case jerr != nil || v.Allowed == nil || *v.Allowed && err != nil:
		if why == "" && err != nil {
			why = err.Error()
		}
		if why == "" {
			why = "no decision in its output"
		}
		return &shipHold{err: refused("the scope check gave no decision for %s (%s): the build waits for the next pass", repo, why)}
	case *v.Allowed && v.Disposition != "report_only":
		return nil
	case len(v.Reasons) > 0 && !slices.ContainsFunc(v.Reasons, func(r string) bool { return !undecidedRe.MatchString(r) }):
		return &shipHold{err: refused("the scope check could not decide for %s (%s): the build waits for the next pass", repo, why)}
	}
	return refused("the scope check does not allow %s (%s %s): report only", repo, v.Disposition, why)
}

// Protected are the paths of momentum's own repository that its judgment and its authority rest on (ProposalProtected):
// CheckPaths names a hand path among them as the loop's own. Each call returns a list of its own: nothing a change
// compiles in can empty it.
func Protected() []string { return ProposalProtected() }

// selfLand are the paths of the loop's own repository it lands itself, behind the gates every change passes (its
// check, the full suite of each package it touches, its review, CI) and the reads of its content (selfKept) and, for a
// detector, of what it does to the stored history (selfReplay): a directory, with its final slash, or a file. Each
// file's test file lands with it. The parts of them selfHand names are a person's. What is not here is a person's to
// land (Hand landing), and ProposalProtected no change touches at all.
var selfLand = []string{
	"internal/detect/",    // the detectors: a change is replayed over the stored history before it lands
	"internal/supervise/", // supervision's rules and engine
	"internal/request/",   // the owner's requests and their follow-ups
	"sessions/",           // the live session layer's readers, by harness
	// the capacity rules a capacity run applies, once a person has made the file: the loop adds no file to the
	// package that deletes files (selfScope)
	"internal/capacity/rules.go",
	"README.md",
}

// selfHand are the parts of selfLand that stay a person's to land: the detectors' frame (detect.go: the run that files
// findings and sweeps cases, the signatures' versions and the text normalization the stop rule keys refusals by, the
// evidence bound; keyed.go: what the measure step keys and which families it decides), which the replay does not
// read; the sessions detector, which reads the transcripts
// and Hermes's database, so no copy of the store replays it; supervision's history and replay, which the sessions
// detector reads and which measure the rules; the one part of the session layer that types into a session, whose
// checks keep a message from answering a dialog or a permission; and what the store, which no change touches, builds
// on: the session layer's own package (its files directly under sessions/, sessionsRoot), its stamps, its process and
// terminal readers and its internal text.
var selfHand = []string{"internal/detect/detect.go", "internal/detect/keyed.go", "internal/detect/sessions.go",
	"internal/supervise/history.go", "internal/supervise/replay.go",
	"sessions/act/", "sessions/state/", "sessions/proc/", "sessions/term/", "sessions/internal/"}

// sessionsRoot is the session layer's own package, which the store imports: its files directly under sessions/.
func sessionsRoot(c string) bool {
	rest, ok := strings.CutPrefix(strings.ToLower(c), "sessions/")
	return ok && !strings.Contains(rest, "/")
}

// replayed are the landable paths whose change the host replays over the stored history (selfReplay).
var replayed = []string{"internal/detect/"}

// selfLandable says whether the loop lands a change to a path of its own repository itself, by the path: in selfLand,
// not in selfHand nor the session layer's own package, or the test file of a file selfLand names. selfLand is read
// with the path's case, selfHand without it, so a path that only differs in case is never the loop's to land where
// the path it folds to is a person's.
func selfLandable(c string) bool {
	if sessionsRoot(c) {
		return false
	}
	for _, p := range selfHand {
		// without case: a checkout on a case-insensitive disk writes sessions/Act/x.go into sessions/act/
		if inPathFold(c, p) || strings.HasSuffix(p, ".go") && strings.EqualFold(c, strings.TrimSuffix(p, ".go")+"_test.go") {
			return false
		}
	}
	if base, ok := strings.CutSuffix(c, "_test.go"); ok && slices.Contains(selfLand, base+".go") {
		return true
	}
	for _, p := range selfLand {
		if inPath(c, p) {
			return true
		}
	}
	return false
}

// isReplayed says whether a change to the path is replayed over the stored history before it lands.
func isReplayed(c string) bool {
	if !selfLandable(c) {
		return false
	}
	for _, p := range replayed {
		if inPath(c, p) {
			return true
		}
	}
	return false
}

// ciPaths are the CI configurations of the hosts the loop could push to.
var ciPaths = []string{".github/", ".circleci/", ".buildkite/", ".gitlab-ci.yml", "Jenkinsfile", "azure-pipelines.yml", ".travis.yml"}

// SelfRepo is momentum's own repository, where Protected applies.
const SelfRepo = "agent-sh/momentum"

// CheckPaths refuses a change to a repository that touches what the loop may never change there: changed holds
// every path a change adds, removes or edits (both sides of a move).
func CheckPaths(t Target, changed []string) error {
	hand, err := PathScope(t, changed)
	if err != nil {
		return err
	}
	for _, c := range hand {
		if !strings.Contains(c, "/") {
			return refused("%s is a root file of the loop's repository: never its to land", c)
		}
		for _, p := range Protected() {
			if inPath(c, p) {
				return refused("%s is the loop's own (what its judgment or its authority rests on): never its to land", c)
			}
		}
		return refused("%s is outside the loop's explicit self-change allow-list", c)
	}
	return nil
}

// PathScope sorts a change's paths by what the loop may do with them. A CI configuration, in any repository, and in
// its own repository what its measurement and the scanners that write its events rest on (ProposalProtected), refuse
// the change outright. In its own repository a path outside the paths it may land (SelfLandable) is returned: such a
// change may still be checked, reviewed and proposed, but only for a person to land by hand. In any other repository
// every path but CI configuration lands.
func PathScope(t Target, changed []string) (hand []string, err error) {
	// A repository's CI runs from the tree a pushed branch holds, with that repository's secrets, before anyone has
	// reviewed the change: no repository's CI configuration is the loop's to change.
	for _, c := range changed {
		for _, p := range ciPaths {
			// by prefix, without case: a Jenkinsfile.release or a .gitlab-ci.yml.example may be what a host runs
			if inPathFold(c, p) || strings.HasPrefix(strings.ToLower(c), strings.ToLower(p)) {
				return nil, refused("%s is CI configuration: it runs on a pushed branch before review, never the loop's to change", c)
			}
		}
	}
	if !strings.EqualFold(t.Repo, SelfRepo) {
		return nil, nil
	}
	for _, c := range changed {
		if proposalProtected(c) {
			return nil, refused("%s is what the loop's measurement or its event scanners rest on: never its to change, not even for a hand landing", c)
		}
		if !selfLandable(c) {
			hand = append(hand, c)
		}
	}
	return hand, nil
}

// inPath says whether a changed path is p, or, for a directory p names with its final slash, the directory or a path
// under it.
func inPath(c, p string) bool {
	if !strings.HasSuffix(p, "/") {
		return c == p
	}
	return c == strings.TrimSuffix(p, "/") || strings.HasPrefix(c, p)
}

// inPathFold is inPath without case: a checkout on a case-insensitive disk writes Internal/loop/x.go into
// internal/loop/.
func inPathFold(c, p string) bool { return inPath(strings.ToLower(c), strings.ToLower(p)) }

// SelfLandable are the paths of the loop's own repository it may land itself (selfLand), and SelfHand the parts of
// them that stay a person's.
func SelfLandable() []string { return slices.Clone(selfLand) }

// SelfHand are the parts of SelfLandable that stay a person's to land.
func SelfHand() []string { return slices.Clone(selfHand) }

// ProposalProtected are the paths of momentum's own repository that no change of the loop's touches, not even a
// proposal a person lands by hand: what its judgment and its authority rest on. The loop (its gates, ship tool,
// runner, budgets, relay and measure step), its measurement statistics and the replay a detector change is read by,
// the sandbox, the run's read-only reader, the check CI builds from the tree, its configuration and its paths; the
// store, whose schema and migrations hold every event and case and whose case store decides which close a case may
// take; the weekly scorecard that judges momentum and tacit, and internal/weekly/ reserved beside it; the capacity run's events and read records and the busy check the
// repository gate uses; every command, since the commands' package holds the gates,
// the kill switches (supervise.off, loop.live) and the measure and ship steps; and its build and supply chain (go.mod,
// go.sum, go.work, go.work.sum, vendor/). CI configuration is refused in every repository besides (ciPaths). A
// path is compared without case: a checkout on a case-insensitive disk writes Internal/loop/x.go into internal/loop/.
// Each call returns a list of its own.
func ProposalProtected() []string {
	return []string{"cmd/", "internal/loop/", "internal/measure/", "internal/sandbox/", "internal/ro/", "internal/tracked/",
		"internal/config/", "internal/paths/", "internal/store/", "internal/scorecard/", "internal/weekly/",
		"internal/capacity/run.go", "internal/capacity/record.go", "internal/capacity/acts.go", "internal/capacity/scratch_made.go", "internal/capacity/fsid.go",
		"go.mod", "go.sum", "go.work", "go.work.sum", "vendor/"}
}

// proposalProtected says whether a path of momentum's own repository is in ProposalProtected, and not one the loop
// lands.
func proposalProtected(c string) bool {
	if selfLandable(c) {
		return false
	}
	for _, p := range ProposalProtected() {
		if inPathFold(c, p) {
			return true
		}
	}
	return false
}

// under reports whether p is dir or inside it.
func under(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// mainCheckout is the main checkout of the checkout at dir, a linked worktree's too: the place a repository package
// records (Package.Dir).
func mainCheckout(ctx context.Context, dir string) (string, error) {
	top, err := gitOutput(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.Dir(strings.TrimSpace(top)), nil
}

// gitOutput runs git in a directory and returns what it printed. Git runs without optional locks, hooks or a
// file-system monitor, never prompts, and reads none of git's own variables (a GIT_DIR would point it elsewhere).
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	if err := CheckHostStorage(ctx, dir); err != nil {
		return "", err
	}
	if err := CheckGitConfig(ctx, dir); err != nil {
		return "", err
	}
	return hostGit(ctx, dir, nil, args...)
}

// HostGitRead is a scope or busy probe, of host repositories.
// Its repository and configuration reads use only checked, pinned sandbox directories.
func HostGitRead(ctx context.Context, dir string, args ...string) (string, error) {
	return gitOutput(ctx, dir, args...)
}

// A trusted helper may start Git itself. Give its children the same pins as a direct host Git call.
func gitChildEnv() []string {
	pins := append(GitSafety(), "-c", "diff.external=", "-c", "push.followTags=false")
	env := append(GitEnv(), fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(pins)/2))
	for i := 0; i < len(pins); i += 2 {
		key, value, _ := strings.Cut(pins[i+1], "=")
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i/2, key), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i/2, value))
	}
	return env
}
