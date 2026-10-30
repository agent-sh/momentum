# momentum

One tool that keeps improving an agent system: how agents behave during work, and the infrastructure they run on between tasks.

It asks standing questions (what made the owner step in, what he keeps doing by hand, which sessions wait on him, what wastes load or money, what is running out of room, which component works poorly, which failures recur, what new work could replace a part of the setup), answers them with evidence, ships the improvements it is allowed to ship, and measures whether they helped.

Status: milestone 1 (the evidence layer) in progress. See [SPEC.md](SPEC.md).

## Evidence

```
momentum evidence ingest              read what is new in every configured source
momentum evidence events --since 1d   events, newest first (--kind, --source, --json)
momentum evidence status              events per source and kind, and unparsed records
momentum evidence rebuild SOURCE      drop a source's events; the next ingest reads it again (not sessions)
```

Each event records who produced it (owner, agent, tool, harness, detector), and whether an experiment or a test produced it. The store lives in `~/.local/share/momentum`, mode 0600.

Source locations are machine-specific, so momentum ships none. List them in `~/.config/momentum/sources.json`; leave out any you do not have:

```json
{
  "mistakes":  {"path": "~/path/to/claude-mistakes/events.jsonl"},
  "radar":     {"path": "~/path/to/radar.db", "test_device": "name-of-its-verification-device"},
  "incidents": {"dir": "~/path/to/incidents"}
}
```

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

Knowledge stays in [tacit](https://github.com/agent-sh/tacit). Cases, evidence and state stay on the machine, never in this repository.

MIT licensed.
