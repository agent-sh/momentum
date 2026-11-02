# momentum

One tool that keeps improving an agent system: how agents behave during work, and the infrastructure they run on between tasks.

It asks standing questions (what made the owner step in, what he keeps doing by hand, which sessions wait on him, what wastes load or money, what is running out of room, which component works poorly, which failures recur, what new work could replace a part of the setup), answers them with evidence, ships the improvements it is allowed to ship, and measures whether they helped.

Status: milestone 1 (the evidence layer) in progress. See [SPEC.md](SPEC.md).

## Evidence

```
momentum evidence ingest              read what is new in every configured source
momentum evidence events --since 1d   events, newest first, each with its ev:ID (--kind, --source, --harness,
                                      --actor, --until, --json)
momentum evidence events --session ID one session's events, its subagents' among them, oldest first
momentum evidence status              events per source and kind, and unparsed records
momentum evidence search TEXT         stored summaries of every source holding TEXT, newest first (--source, --kind,
                                      --actor, --session, --since, --until, --limit)
momentum evidence show ID             an event, with the record of the transcript or log it names
momentum evidence rebuild SOURCE      drop a source's events; the next ingest reads it again
                                      (not sessions; transcripts only with --force)
```

Each event records who produced it (owner, agent, tool, harness, detector), and whether an experiment or a test produced it. Every source names a harness the same way: claude, codex, pi, opencode, hermes. The store lives in `~/.local/share/momentum`, mode 0600. A schema change keeps every event. Only a change in how the sources read drops what they derive (their cursors), and then the next ingest reads them again.

Source locations are machine-specific, so momentum ships none. List them in `~/.config/momentum/sources.json`; leave out any you do not have:

```json
{
  "mistakes":    {"path": "~/path/to/claude-mistakes/events.jsonl"},
  "radar":       {"path": "~/path/to/radar.db", "test_device": "name-of-its-verification-device"},
  "incidents":   {"dir": "~/path/to/incidents"},
  "transcripts": {"homes": ["~"]}
}
```

`transcripts` reads the agents' session history with [tacit](https://github.com/agent-sh/tacit)'s reader: Claude Code with its subagents, Codex with its compressed rollouts, pi with its tasks, and the Hermes store, where tacit finds them under each home (pi's variables and `TACIT_HERMES_DB` apply). Each item is one event, credited by tacit's attribution. Events hold only redacted text, so [scrump](https://github.com/avifenesh/scrump) with its text profile must be on `PATH`; without it the source stores nothing and fails. scrump 0.2.0 or later masks each text on its own as a record; with an older one each call is padded with about 1 KB of boundary per text, which a first ingest pays for. The reader's saved position in each transcript keeps some of its lines unredacted, in the same private store. `"workers"` sets how many files an ingest reads at once (default: a third of the cores). Claude Code deletes a transcript after about 30 days, so its events can be all that is left of it: `evidence rebuild transcripts` needs `--force`, and then says how many events have no source left.

## Sessions

```
momentum sessions          the live agent sessions and their phases (--json, --all)
```

It finds Claude Code, Codex, pi, opencode and Hermes sessions from processes, tmux panes, transcripts and the harnesses' own state, with no model call, and keeps when each session entered its phase in the store. Each run resumes every Claude Code, Codex and pi transcript where the previous run of the same build stopped, so a run on a timer reads only what was appended. Runs on one store take turns: a run waits up to 10 seconds for the one before, then fails and names it. Harness locations follow their own variables (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, `XDG_DATA_HOME`, `HERMES_HOME`, `PI_CODING_AGENT_DIR`). To name projects by their path under a directory, add it to `sources.json`:

```json
{"sessions": {"projects": ["~/path/to/code"]}}
```

Each run also records every phase change as an event of source `sessions`: a session appeared, entered another phase, began a new stretch of the same phase after it moved between two runs, or vanished. Its Meta holds `from`, `to`, `name`, the time in the phase it left, and for needs_you `ask_kind` and `ask` entering it, `from_ask_kind` and `from_ask` leaving it. The time comes as two bounds: `for_ms` runs until the run that saw the change, so it grows with the time between runs, and `seen_ms` runs until the last run that still saw the old phase. A wait counts in the window where it ended. These events are observations of live sessions, so `evidence rebuild` refuses them. How long sessions waited on a permission prompt this week, and which ones:

```
momentum evidence events --source sessions --since 7d --limit 100000 --json | jq -s 'map(.Meta |= fromjson
  | select(.Meta.from == "needs_you" and .Meta.from_ask_kind == "permission")) | group_by(.Session)
  | map({session: .[0].Session, name: .[0].Meta.name, waits: length,
         minutes: (map(.Meta.for_ms // 0) | add / 60000 | floor), at_least: (map(.Meta.seen_ms // 0) | add / 60000 | floor)})'
```

The packages under `sessions/` are exported for boards that show the same sessions. One of them, `sessions/act`, types a message into a session's tmux pane or kitty window, or queues it for a Codex thread, and only when the session's process owns the terminal, no dialog or menu shows, and its input line holds nothing typed.

## Supervise

```
momentum supervise [--dry] [--json]   scan the sessions, then continue the ones that stopped where a rule says it may
momentum supervise replay [--since WHEN] [--until WHEN] [--labels FILE] [--anchor] [--points FILE]
                                      run the rules over the store's history, against the owner's own steps
```

Run after each `momentum sessions` scan (a second `ExecStart` of the sessions unit, the first marked `-` so a failed scan does not skip it), with `momentum evidence ingest` on its own timer every few minutes, it continues sessions that stopped without their owner: a turn that ended on a transient API error (tried again with a backoff, then a case), on an expired login, a usage limit or an organization block (resumed once access works again, or at the reset time the error states), a Hermes reply cut at the model's output limit, a session reopened after it ended mid-task, a Claude Code turn that waits on a model request that never got its reply (interrupted the way its owner would, with the message queued and then Escape, or typed as the next prompt when its screen shows no turn running), a wakeup or a scheduled job's run of its own that never came, and a turn that ended on a promise nothing will wake it for. Deterministic rules decide; no model takes part. It never answers a question, a plan or a permission, never acts on an automated run, a session on another machine or a Hermes conversation on a messaging platform, holds while the owner was at the session recently, and sends nothing that reaches his phone. What it leaves to him shows in the report: a needs note for a session that waits on his answer or a stalled turn momentum could not resume, a problem note for one it could not continue, each marked done when the session moves on, and a decided note for each stalled turn it resumed. Every message opens with `[momentum]`, and every decision, message and outcome is an event of source `supervise`. Rules that are not live record what they would have sent, tagged experiment `supervise-dry`. `transient`, `access` and `stalled` ship live; to choose otherwise, or to turn it off:

```json
{"supervise": {"live": ["transient", "access", "stalled"], "off": false}}
```

## Report

momentum has no app or dashboard of its own. Its state shows inside surfaces the owner already looks at: a view there calls `momentum report --json` every minute or so, keeps its last good copy, and draws a short section in its own style.

```
momentum report [--json]       what needs you, what was decided and shipped in the last 12 hours, what runs now,
                               and what momentum could not fix; --json prints one JSON object on one line
momentum note add --kind needs|decided|shipped|problem --title T [--detail D] [--effect E]
                  [--link LABEL=URL]... [--id ID] [--source S] [--importance high|normal|low]
momentum note done ID          resolve a needs or problem note, withdraw a decided or shipped one
momentum note list [--json]    every note, newest first, and whether the report shows it
```

```json
{"version": 1, "generated_at": "2026-10-30T14:00:00+02:00",
 "needs_you": [{"id": "n3fa9c2e1", "title": "Rent a GPU for the eval?", "at": "2026-10-30T13:12:00+02:00", "links": [], "source": "momentum"}],
 "decided": [], "shipped": [], "running": [], "problems": [],
 "counts": {"needs_you": 1, "decided": 0, "shipped": 0, "running": 0, "problems": 0}}
```

Every section is an array, sorted by importance and then newest first, of at most 20 items; `counts` counts them all. An item has an `id`, a one-line `title` of at most 120 characters, an optional `detail` and `effect` (what a shipped change measurably did), an `at` in RFC 3339, `links` and a `source`. All of it is plain text: no HTML or markdown, no control characters (escape sequences, bells) and no invisible format characters (bidi overrides).

Four sections are notes: needs you (each title is the question, or what momentum could not do), decided (the detail says how to override it), shipped, and problems. A note is kept state: a schema change keeps it and `evidence rebuild` never touches it. `note add` prints the note's id; the same `--id` again replaces that note and makes it stand again. A needs or problem note stays until `note done`; a decided or shipped one shows for 12 hours.

`running` is computed live, by the scan `momentum sessions` makes: the sessions that are working, and the workflows, agents, background jobs and monitors in any session. The report resumes that scan from the checkpoints the last `momentum sessions` run saved, so with the sessions timer running it costs about what that run costs. It opens the store read-only and writes nothing; with no store, or one that is not this build's schema (the next `momentum sessions` of this build migrates it), it prints nothing and exits 3. Its scan has 3 seconds and reads the harnesses at once: a harness that fails is named on stderr and running keeps what it read, one not done by then is named and left out, and the report still prints.

## Capacity

```
momentum capacity [--dry-run] [--now] [--rule R,R] [--json] [--all]
```

It keeps the disks from filling. Each run samples which paths processes use (open files, memory maps, working directories, programs, command lines, Docker containers' bind mounts), since a noatime mount records no read, and records each disk's use. Then each rule that is due scans:

- **models:** a top-level item of a models, datasets or model-cache root with no write and no use seen for 30 days of watching. A root's items wait until the sampler has watched it for 30 days, and time it did not run does not count. A Hugging Face cache's shared Xet blobs go with the last repository that links them, under Hugging Face's own locks.
- **targets:** a Cargo target directory with no write for 48 hours, that no process uses, that git does not track, and whose project is known and not excluded. What Cargo does not build again (saved benchmark baselines) stays.
- **worktrees:** a linked worktree whose branch is merged, landed or gone from its remote, clean (ignored files only in rebuildable caches), with every commit merged or on a branch its remote still has: `git worktree remove`, then its branch if it has not moved. One with uncommitted, ignored or unpushed work is reported; one whose repository someone works in (a process or session in one of its work trees, or a change to one in the last hour that capacity did not make) waits.
- **scratch:** a task's idle folder at the top of the cache directory, one an agent's call created within minutes of its birth (a tool's cache is tagged, named like a command, or created by no agent's call), with no write and no use seen for 21 days of watching: offloaded to restic with `evidence-offload`, which leaves `NAME.restic.txt` in its place. Its model weights, checkpoints and datasets are deleted instead, after the models rule's 30 days. A folder a process uses, a job names, a live session works in or made, or a repository references stays; an excluded project's is reported, all but its model data.
- **uv:** `uv cache prune --ci` over 30 GiB or weekly. Every prune deletes the environments uv keeps in its cache, so while processes run from some (found by their files, the program they were started as, or the PATH, VIRTUAL_ENV or PYTHONPATH they run with), or a scheduled job names one, it prunes around them instead: it keeps each environment they use, and what a scheduled job names, with what those reach in the cache (their links, the paths their `.pth` files name, an overlay's layer), and moves the rest of `archive-v0` and `environments-v2` aside under the cache's lock, then deletes it. `builds-v0` stays whole, and an overlay there that a command whose uv was killed may still run in unseen (a process with no PATH that runs the overlay's interpreter and started after it was made) holds uv's own prune off, with what it names. It waits for, and reports, a process whose use of the cache it cannot tell. With only holders of uv's lock that have started their command and run outside the cache (a project's `.venv`, an installed tool), and no clean or prune waiting, it prunes with `--force` under a lock of its own put in the lock file's place, which a new uv process waits for. Around running environments the old lock file goes back once it is done, so uv's own clean or prune waits for its holders again.
- **docker:** the build cache older than 168 hours and dangling images; unused volumes are reported. Its size counts only what the prune removes: a record an image shares stays and frees nothing, and so does a parent of a record that stays.
- **disk:** a fill faster than 20 GiB an hour, or one that reaches 94% within 6 hours, names its writers in a problem note (by what each wrote at a look and since the run before, the fastest of every writer the fill's runs saw). When the fill ends, the problem note goes and a decided note says what the disk gained and who wrote it, for 12 hours. A disk above 94% has a problem note of its own until it drops below.

Only the rules listed in `live` delete or offload; the others list what they would do. Whatever a systemd unit, timer, crontab line, Hermes job or installed command's symlink names stays, and so does what a process uses: each item is judged again just before it goes. Excluded projects are report-only for every rule but models. A run that cannot read the user's jobs, or that sees another PID namespace's processes, deletes nothing. Each deletion, offload and prune is an event, recorded as it happens, and a run's are one decided note in the report, rewritten as each goes: what the run freed, by rule, its largest items and how to keep one next time. What a rule leaves by its own policy (an excluded project's item, one of no known project, Docker's volumes) is in the run's events and its output, with no note. What it would take and something outside the rule keeps (a prune held off for a day, whatever holds it, an offload out of its time, model data it may not remove, a directory or a repository it cannot read), a deletion that failed, and a rule that could not run are problem notes. Every path is configuration, under `capacity` in `sources.json`, and an unknown key is refused:

```json
{"capacity": {
  "disks": ["/", "/data"],
  "live": ["targets", "worktrees"],
  "exclude": {"paths": ["~/code/other-team"], "names": ["*other-team*"], "remotes": ["org/other-team"]},
  "names": {"hermes_jobs": "~/.hermes/cron/jobs.json", "hermes_scripts": "~/.hermes/scripts", "links": ["~/.local/bin"]},
  "models": {"roots": ["/srv/models", "~/.cache/huggingface", "~/.cache/huggingface/hub"], "keep": ["/srv/models/token"]},
  "targets": {"roots": ["~/code", "~/.cache"]},
  "worktrees": {"roots": ["~/.cache", "~/code/*-worktrees"]},
  "scratch": {"roots": ["~/.cache"], "offload": "~/.local/bin/evidence-offload", "keep": ["~/.cache/keep-me"]}
}}
```

Thresholds have defaults (`idle_days`, `idle_hours`, `max_gib`, `every_days`, `until`, `fill`, `min_mib`, `every_hours`, scratch's `tag`). A dry run deletes and records nothing and runs every rule now.

Run it with `momentum evidence ingest`'s timer: a oneshot unit of its own that the ingest unit `Wants=`, ordered `After=` it, so a failed ingest does not skip it and an hourly scan has its own timeout.

## Cases

```
momentum case detect [--since WHEN] [--family F,F] [--dry] [--json]
momentum case list [--family F] [--status S,S] [--all] [--json]
momentum case show ID [--evidence N] [--log N] [--json]
momentum case open --family F --signature S --title T [--evidence REF]...
momentum case set ID [--status open|investigating|testing] [--title T] [--mechanism M] [--alternative A]... [--next-test T]
                     [--benefit B]
momentum case evidence ID [--at WHEN] [--kind K] [--summary S] REF...
                                        ev:EVENT, note:ID, git:OWNER/REPO@SHA, file:PATH[@OFFSET], or an address
momentum case signature ID root:KEY     the root cause; a case that names one another open case holds goes into it
                                        (a closed one comes back first: case reopen)
momentum case close ID --reason not-worth-it|insufficient-evidence|already-solved|duplicate [--into ID] --why W
momentum case reopen ID --evidence REF... [--at WHEN] --why W
```

`case detect` runs the detectors, deterministic reads with no model (of the store, and for sessions of what the replay of supervision reads): recurring tool and infrastructure failures (guard refusals, harness and tool errors, commands that fail the setup's way, model refusals, incidents), the owner's own turns that push back or repeat with the agent's last words before them, and what keeps sessions waiting on him (supervision's give-ups, nudges no rule covers, long waits). Each finding is a signature; a case opens for a new one, and the case that holds it takes the evidence.

A case is one answer in progress to a standing question: its family, its evidence, the suspected mechanism and the alternatives, the next test that tells them apart, the expected benefit, what it cost so far, and its status (open, investigating, testing, deployed, measuring, closed with a reason). A symptom's signature opens a case, and the same signature finds it again; the root cause's signature merges the cases it explains. A closed case comes back on new evidence only: a recurrence after it was solved, three consecutive read windows at its old share after it improved, or as much again as it was dismissed on. Only the ship tool deploys a case and only the measure tool closes one as improved. A case unseen for 30 days closes by itself.

## Loop

```
momentum loop [--dry] [--family F,F] [--case ID] [--json]
momentum loop runs [--since WHEN] [--case ID] [--json]
momentum loop lesson --case ID --text T
momentum loop eval [--golden FILE] [--task ID,ID] [--trials K] [--arms loop,native] [--budget USD] [--parallel N] [--json]
momentum measure [--dry] [--family F,F] [--case ID] [--json]
momentum ship prepare|check|propose|status|land|rollback ...
```

The improvement loop takes each family's case one step a run: investigate it, build its change (or revise it for its review), or publish its lesson. A step is one run of the harness's own agent (Claude Code in print mode, bare and restricted, Opus 5.5 with Sonnet 5.5 when Opus's safeguards refuse), with the loop's procedure and the family's skill in its system prompt (an investigation is the native path's instead: the native brief and the case's evidence, and the runner writes its report into the case), the harness's permissions in dontAsk mode (its file tools reach only its directories; it reads the rest of the machine through momentum's commands, which scrub secrets; it writes only in its run's directory and its case's work tree), and a budget per run, per case, per day and per week. A case whose runs fail the same way twice stops, and the loop opens a case about itself.

`loop eval` is the gate before the loop goes live: it runs the loop's investigation and the native path on a golden set kept on the machine, real past problems with verified root causes, each as of the moment its case could have opened (the store and the repositories as they stood then, no reading of the machine as it is now), several times, and a clean-context judge grades whether each reached the cause. It prints, per arm, the causes reached, pass@k and pass^k, the cost and time per cause reached, the tasks that flipped, and a paired bootstrap over tasks of the difference; `results.json` keeps every trial.

Inside a run, everything momentum prints is scrubbed whole before the run reads it, including replay, whose file flags are refused. File evidence is scrubbed whole before line selection; files over 64 MiB are refused. Git outside a sandbox reads no attributes from the tree and refuses repository settings that name programs; ship authenticates through its own GitHub credential helper, and every push and review post checks its repository again, which goes only where origin's fetch and push addresses agree. A case with a run in flight waits for it.

A change goes in only through `momentum ship`, and only to a repository: a work tree of the loop's own, a test that fails without the change and passes with it in a bubblewrap sandbox that reaches nothing of the user's, a measurement plan whose signal counts the case's own problem, a rollback, a pull request committed as the user with its text scrubbed and its mentions inert, a clean-context review posted on it (three rounds at most), and a fast-forward of the default branch, only when the loop is live. Each landed change is measured from when it is in effect (an installed Go binary whose revision holds it, or its checkout held it and its named installed file changed after that, from a checkout that held it then), over its window, by its signal's share of activity against the three windows before the gated window (Fisher's exact test, at 5%): a change that helped closes its case as improved with a shipped note of its effect, one that did not is rolled back, while the loop is live, with a decided note, and three consecutive read watch windows at its old share bring its case back, with at least five hit units among them. Choose a week, or two for a rarer problem, in whole weeks; every counted source must have read past a window before it can decide. Tool results count only failures; units are session hours for session signals, owner hours for owner words alone, and machine hours for events without sessions, with the loop's own activity left out by both its raw and resolved directory. Its authority is configuration the loop never writes, and a key of it momentum does not know is an error:

```json
{"loop": {
  "runner": ["claude"],
  "live": false,
  "owners": ["your-github-user"],
  "exclude": ["your-github-user/do-not-touch"],
  "repo_guard": ["~/bin/repo-guard"],
  "rules": ["~/.config/agent-rules.md"],
  "read_dirs": ["~/projects"],
  "constraints": ["never wake me"],
  "capacity_gate": ["~/bin/capacity-gate"]
}}
```

Run it from a oneshot user unit on a timer of its own, after the ingest.

Knowledge stays in [tacit](https://github.com/agent-sh/tacit). Cases, evidence and state stay on the machine, never in this repository.

MIT licensed.
