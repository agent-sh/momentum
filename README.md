# momentum

One tool that keeps improving an agent system: how agents behave during work, and the infrastructure they run on between tasks.

It asks standing questions (what made the owner step in, what he keeps doing by hand, which sessions wait on him, what wastes load or money, what is running out of room, which component works poorly, which failures recur, what new work could replace a part of the setup), answers them with evidence, ships the improvements it is allowed to ship, and measures whether they helped.

Status: design. Evidence, cases, the sandboxed build loop and deterministic measurement are implemented. See [SPEC.md](SPEC.md).

## Sandbox

`internal/sandbox` runs a fixed bubblewrap layout in a transient systemd user service. It reuses bubblewrap and socat for isolation and the optional relay route. Programs run as the caller's UID and GID. There is no host shell.

The unit bounds memory, CPU, tasks and runtime. Defaults are 4G, 400%, 512 tasks and 1800 seconds. Cancellation stops the service and joins its client. The loop supplies its own budgets and calls `StopLeftover` with its own unit prefix before new runs. A broken user manager is a cleanup error; the unit's runtime limit remains the last bound.

Only the system's fixed read-only mounts, caller binds and an optional relay socket are visible. Bind destinations are `/work`, `/out`, `/run/input`, or under `/ro/` and `/opt/`, with no overlapping destinations. Sources are host-created, clean and free of sockets; the relay is checked separately. The environment starts with four fixed variables and the caller's additions. Without a relay, the program runs directly with only private loopback.

A golden argv, the plain PASS/FAIL script and five test-only widenings check the boundary. Namespace tests skip with a reason when the real unit probe cannot run. The launch, exit-status ambiguity, Ubuntu mount restriction, vendor guidance, pinned `srt` fallback, Podman alternative and primary sources are in [Sandbox](SPEC.md#sandbox).

## Evidence

```
momentum evidence ingest              read what is new in every configured source
momentum evidence events --since 1d   events, newest first (--kind, --source, --harness, --until, --json)
momentum evidence events --session ID one session's events, its subagents' among them, oldest first
momentum evidence status              events per source and kind, and unparsed records
momentum evidence show [--bytes N] ID...  one to ten events with their records, scrubbed (what a loop run reads under /ro/records)
momentum evidence unit NAME [--lines N] [--bytes N]  a user service's state and the end of its log, scrubbed
momentum evidence rebuild SOURCE      drop a source's events; the next ingest reads it again
                                      (not sessions; transcripts only with --force)
```

Each event records who produced it (owner, agent, tool, harness, detector), and whether an experiment or a test produced it. Every source names a harness the same way: claude, codex, pi, opencode, hermes, grok, kimi. The store lives in `~/.local/share/momentum`, mode 0600. A schema change keeps every event. Only a change in how the sources read drops what they derive (their cursors), and then the next ingest reads them again.

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

Native readers also discover OpenCode's `opencode/opencode.db` under `XDG_DATA_HOME` (else `~/.local/share`), Grok's session `updates.jsonl` under `GROK_HOME` (else `~/.grok`), and Kimi's main and child `wire.jsonl` under `KIMI_CODE_HOME` (else `~/.kimi-code`). Additional locations use `transcripts.sources`, with or without `homes`:

```json
{
  "transcripts": {
    "sources": [
      {"harness": "opencode", "db": "~/history/opencode.db"},
      {"harness": "grok", "glob": "~/history/grok/*/updates.jsonl"},
      {"harness": "kimi", "glob": "~/history/kimi/*/agents/*/wire.jsonl"}
    ]
  }
}
```

Source harnesses are `opencode`, `grok` and `kimi`. OpenCode requires `db` only; Grok and Kimi require `glob` only. Configuration load refuses other harnesses and missing or wrong location fields. A leading `~` is expanded. OpenCode prefers V2 over its migrated legacy copy and rereads changed sessions. Grok and Kimi resume by offset and count malformed complete lines as skipped. Unknown input provenance stays unknown.

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
momentum case close ID --reason not-worth-it|insufficient-evidence|already-solved|duplicate [--into ID] [--fix REF] --why W
momentum case reopen ID --evidence REF... [--at WHEN] --why W
```

`case detect` runs the detectors, deterministic reads with no model (of the store, and for sessions of what the replay of supervision reads): recurring tool and infrastructure failures (guard refusals, harness and tool errors, commands that fail the setup's way, model refusals, incidents), the owner's own turns that push back or repeat with the agent's last words before them, and what keeps sessions waiting on him (supervision's give-ups, nudges no rule covers, long waits). Each finding is a signature; a case opens for a new one, and the case that holds it takes the evidence.

A case is one answer in progress to a standing question: its family, its evidence, the suspected mechanism and the alternatives, the next test that tells them apart, the expected benefit, what it cost so far, and its status (open, investigating, testing, deployed, measuring, closed with a reason). A symptom's signature opens a case, and the same signature finds it again; the root cause's signature merges the cases it explains. A closed case comes back on new evidence only: a recurrence after it was solved, its old rate after it improved, or as much again as it was dismissed on. The host deploys checked and reviewed packages. The measure step starts once the change is in effect and closes an improved case with its measured effect. A case unseen for 30 days closes by itself.

## Loop

```
momentum loop [--dry] [--family F,F] [--case ID] [--json]
momentum loop runs [--since WHEN] [--case ID] [--json]
```

The loop investigates, builds and reviews through Claude Code's native agent in print mode. The host records investigation findings and an optional repository NAME under `/ro/repos`. A complete report moves the case to testing. A code fix needs an admitted repository; a case without one waits with a passive note. Two runs of the same step that fail the same way stop the case and open a component-health case. Once its cause is fixed, `case close ID --reason already-solved --fix git:OWNER/REPO@SHA --why W` on that component-health case resumes the cases it stopped: the stop rule counts only their runs after the close.

Run the loop tests with `go test -timeout 50m ./internal/loop`. Use `-run` for focused checks. The sandbox checks can exceed Go's default timeout.

The host gates the source repository and prepares an independent host clone at the base commit and a build tree of it: one commit of its tree, without history, scanned by the scrubber unless the repository is public. A build edits `/work`, runs its discriminating test and returns summary, test_command (the program and its arguments, which the host runs in the sandbox before and after the change), title, body, rollback, signal, window and lesson. In momentum's own repository the loop lands only its two rules files; a fix that needs other paths is checked and reviewed the same way and proposed as a pull request labeled `momentum-hand-landing`, which a person lands or closes, and which the loop then measures or reopens. Toolchains and module caches are read-only, with build caches in tmpfs. A second sandbox job without a relay or harness exports the cumulative patch. The host reads that one confined file, validates it, applies it in its clone and checks that the test fails on BASE and passes on the change. It proposes exactly the checked tree.

A review reads the proposed tree at `/ro/change` read-only and the patch at `/run/input/change.patch`. The host keeps the findings locally. A private repository receives the cleaned findings; a repository that is not private receives host-built text only. Before publishing a patch there, the host scans its added lines and file names for personal content and secrets and refuses a hit. High and medium findings return to a build with `review.md`. A moved base supplies `previous.patch` for adaptation. Three review rounds are allowed. Two closed, unmerged proposals stop automatic builds. Exhausted changes and reviews held by budget or the stop rule wait with passive notes. The owner can still propose a checked package. A clean posted review can land when `loop.live` is true. Every push rechecks the repository gate. Landing and rollback use host journals and fast-forward pushes. Rollback restores already published content and skips the public-patch scan. An improved case's lesson is published by the host without another model run, marked unverified. Measurement validates the outcome, not the model-written advice.

The measure step runs on each loop pass, or with `momentum measure [--dry] [--family F,F] [--case ID] [--json]`. It uses no model. A plan counts the signal's share of active hours over whole weeks. Choose a week, or two for a rarer problem. At least five hit hours admit the gate. The three windows before that gate supply the independent baseline. Every counted source must have read past a window before it decides.

Measurement starts once the repository checkout holds the landed change. For a binary or installed configuration, the build report names `installed` in an admitted `loop.read_dirs` directory. The owner can also use `momentum ship propose --installed FILE`. A Go revision must hold the change. Another installed file must be rebuilt after its checkout held it. Host code checks and records the installed file. No momentum command runs inside the sandbox.

A lower share beyond chance closes the case as improved and leaves a shipped note with its measured effect. No measured effect rolls the change back while the loop is live. Otherwise a decided note holds the rollback and gives the command to do it by hand. A quiet window waits one more window. Three consecutive read watch windows at the old share, with five hits among them, reopen the case. Waiting and decision notes stay passive. SPEC's Measurement section states the assumptions and measured test rates.

Read gaps do not count as a fix: a window in which a scan failed is never read whole. Its window after starts again once the source reads past the failure. A Go build from a modified checkout is judged once by the tree it was built from, while its hash and time stand. A build report names a file only where the loop reads, in loop.read_dirs.

The host prepares scrubbed inputs before any model starts: the last 30 days of events and notes, evidence records and neighbours, unit logs, text configuration, tacit's facts and full-history repository clones. Pass-level copies are made once, shared read-only and removed at the end. Each run gets its own records and case file. A missing or failed scrubber skips the step without charging the case. A dry pass prepares nothing and writes nothing.

The agent has `momentum-ro` on PATH at `/opt/momentum/momentum-ro`, bound read-only from the same pinned binary used by the host records child. It also reads `/ro/store.sqlite` with python3's sqlite3, and reads `/ro/records`, `/ro/config`, `/ro/clone`, `/ro/repos` and `/run/input`. `/work` is scratch for investigations; `/out/findings.md` preserves partial findings. The host reads only that bounded regular file if the run returns no report. It treats all run output as untrusted, scrubs findings and never runs programs or Git on the host in writable run directories. The separate patch unit runs Git inside the fixed sandbox.

`momentum-ro evidence events` uses the host's filters, ordering and limits; `evidence show ev:ID [ID...] [--bytes N]` reads up to ten events with their frozen records and lists indexed neighbours from the same session. `evidence status [--since DUR]` counts sources and kinds. `evidence unit NAME` reads a frozen unit log. `case show` prints the case, evidence and log; `case show --json` prints the input file. `--store`, `--records` and `--case` override the run paths for tests only.

The basename from `os.Executable()` selects this mode, never argv[0]. All other commands exit 2: "momentum-ro reads the run's copies and changes nothing". It opens only the run's SQLite copy in read-only immutable mode, reads no configuration, writes nothing, uses no network and runs no other program. Renaming a copy to `momentum` restores the host parser; the sandbox still has no host store, host configuration or host network. This reader is no door.

Host `evidence show` scrubs all headers and whole records in one batch, then clips records. `evidence unit` scrubs once too. The records pass publishes those captures without another scrub. The sandbox reads already-scrubbed records without starting a scrubber.


`internal/sandbox` runs the whole harness under a transient systemd user service. The Bedrock relay is its only network route. The relay enforces model policy, spend reservations and request turns, observes token usage and authenticates the terminal report. Model refusal can use the fallback within the primary's remaining budget and time. Session persistence is disabled; the host archive is the transcript.

```json
{"loop": {
  "runner": ["~/.local/bin/claude"],
  "bedrock_env": "~/.config/claude-code/bedrock.env",
  "rules": ["~/.config/agent-rules.md"],
  "read_dirs": ["~/projects/example"],
  "tacit_store": "~/.local/share/tacit/store",
  "constraints": ["never wake me"]
}}
```

`loop.owners`, `loop.exclude`, `loop.repo_guard`, `loop.sandbox_read`, `loop.capacity_gate` and `loop.live` control host builds and landing. A missing repository guard refuses writes. Run the pass from its own oneshot user unit after ingest.

`loop.config_targets` lets the loop change the content of a configuration file in place. It is off by default and admits only the existing files `allow` names exactly; an empty list admits none. Landing also needs `loop.live`. The loop's own rules, policy and configuration, credential files, `bin` directories, databases, programs and links are refused in code, whatever the list says:

```json
{"loop": {"config_targets": {"enabled": true, "allow": ["~/.config/agents/procedures/example.md"]}}}
```

## The ship tool

The owner's CLI controls the same host package lifecycle:

```
momentum ship prepare --case ID --repo DIR
momentum ship check --case ID --test "go test ./..." --patch FILE
momentum ship propose --case ID --title TITLE --body-file FILE --rollback TEXT --signal JSON --window 7d --lesson TEXT --patch FILE [--installed FILE]
momentum ship status --case ID [--json]
momentum ship land --case ID
momentum ship rollback --case ID --why TEXT
momentum case rollback ID --why TEXT
momentum loop lesson --case ID --text TEXT
```

Agent runs return reports. The host calls the ship tool's methods. Patch validation, export, publication leases, landing and rollback are specified in [The ship tool](SPEC.md#the-ship-tool).

## Evaluation

`momentum loop eval` compares the loop's investigation and the harness's native agent against verified past root causes. The golden JSONL and its answers stay on the machine. Tests use synthetic tasks.

```text
momentum loop eval --golden FILE --trials 3 --arms loop,native --budget 120 --parallel 3
momentum loop eval --golden FILE --held-out ID1,ID2 --split held-out --task ID1 --json
```

The host prepares one as-of input set per task. Both arms and all its trials share the same read-only `/ro` store, records, configuration, repository histories and tacit clone. The end is the earlier of the task cutoff and one minute after its newest symptom. Later events, commits, refs, reflogs and unreachable objects are removed. Both arms also share the pinned `momentum-ro` binary and its commands. The judge has no reader or `/ro` inputs. No tacit program is mounted. Both prompts end with the live investigation's paths reference. The inputs go when the task ends.

Historical Hermes message rows are withheld; their stored summaries remain. Configuration roots outside a repository are copied as they are now and listed as leaks. Missing transcripts and histories cannot be reconstructed. Commit dates approximate historical branch positions when a reflog is unavailable. See [SPEC.md](SPEC.md#evaluation) for exact keys and limits.

Each slot reserves both arms, each investigation and two judge attempts, with the relay's 1.5 times budget allowance included. The default pair reserves $30 (a $3 judge: the relay reserves the largest charge a request could cost); `--budget` must hold that for every parallel slot plus what was spent. A failed start costs nothing and its pair is excluded. The clean-context judge has only its input directory and no tools or `/ro` mounts. Accounting comes from the relay's observed cost, tokens and turns.

`results.json` records the build, exact golden-file hash, every prompt and system hash, pinned records and read-only binary hashes, the reader's fixed bind destination, models, budgets, input manifests and hashes, selected and held-out tasks, errors and historical limitations. The summary reports reach, cost and time per reached cause, paired task bootstrap intervals, pass@k, pass^k and flips. Undefined rates print as undefined and serialize as null. The operator keeps the held-out split and checks a judge sample by hand before using the comparison to turn the loop on.

Knowledge stays in [tacit](https://github.com/agent-sh/tacit). Cases, evidence and state stay on the machine, never in this repository.

MIT licensed.
