# momentum

One tool that keeps improving an agent system: how agents behave during work, and the infrastructure they run on between tasks.

It asks standing questions (what made the owner step in, what he keeps doing by hand, which sessions wait on him, what wastes load or money, what is running out of room, which component works poorly, which failures recur, what new work could replace a part of the setup), answers them with evidence, ships the improvements it is allowed to ship, and measures whether they helped.

Status: milestone 1 (the evidence layer) in progress. See [SPEC.md](SPEC.md).

## Evidence

```
momentum evidence ingest              read what is new in every configured source
momentum evidence events --since 1d   events, newest first (--kind, --source, --json)
momentum evidence status              events per source and kind, and unparsed records
momentum evidence rebuild SOURCE      drop a source's events; the next ingest reads it again
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

Knowledge stays in [tacit](https://github.com/agent-sh/tacit). Cases, evidence and state stay on the machine, never in this repository.

MIT licensed.
