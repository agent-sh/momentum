# momentum

One tool that keeps improving an agent system: how agents behave during work, and the infrastructure they run on between tasks.

It asks standing questions (what made the owner step in, what he keeps doing by hand, which sessions wait on him, what wastes load or money, what is running out of room, which component works poorly, which failures recur, what new work could replace a part of the setup), answers them with evidence, ships the improvements it is allowed to ship, and measures whether they helped.

Status: design. See [SPEC.md](SPEC.md).

Knowledge stays in [tacit](https://github.com/agent-sh/tacit). Cases, evidence and state stay on the machine, never in this repository.

MIT licensed.
