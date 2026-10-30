# momentum

One tool that keeps improving an agent system: how agents behave during work, and the infrastructure they run on between tasks.

It asks standing questions (what made the owner step in, what he keeps doing by hand, which sessions wait on him, what wastes load or money, what is running out of room, which component works poorly, which failures recur, what new work could replace a part of the setup), answers them with evidence, ships the improvements it is allowed to ship, and measures whether they helped.

Status: milestone 1 (the evidence layer) in progress. See [SPEC.md](SPEC.md).

## Evidence

```
momentum evidence ingest              read what is new in every configured source
momentum evidence events --since 1d   events, newest first (--kind, --source, --harness, --until, --json)
momentum evidence events --session ID one session's events, its subagents' among them, oldest first
momentum evidence status              events per source and kind, and unparsed records
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

The packages under `sessions/` are exported for boards that show the same sessions.

## Report

momentum has no app or dashboard of its own. Its state shows inside surfaces the owner already looks at: a view there calls `momentum report --json` every minute or so, keeps its last good copy, and draws a short section in its own style.

```
momentum report [--json]       what needs you, what was decided and shipped in the last 7 days, what runs now,
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

Four sections are notes: needs you (each title is the question), decided (the detail says how to override it), shipped, and problems. A note is kept state: a schema change keeps it and `evidence rebuild` never touches it. `note add` prints the note's id; the same `--id` again replaces that note and makes it stand again. A needs or problem note stays until `note done`; a decided or shipped one shows for 7 days.

`running` is computed live, by the scan `momentum sessions` makes: the sessions that are working, and the workflows, agents, background jobs and monitors in any session. The report resumes that scan from the checkpoints the last `momentum sessions` run saved, so with the sessions timer running it costs about what that run costs. It opens the store read-only and writes nothing; with no store, or one that is not this build's schema (the next `momentum sessions` of this build migrates it), it prints nothing and exits 3. Its scan has 3 seconds and reads the harnesses at once: a harness that fails is named on stderr and running keeps what it read, one not done by then is named and left out, and the report still prints.

Knowledge stays in [tacit](https://github.com/agent-sh/tacit). Cases, evidence and state stay on the machine, never in this repository.

MIT licensed.
