# momentum: one tool that keeps improving an agent system

Status: design. Phase 0 (the questions answered by hand, to set the baseline) is under way.

## Mandate

Reduce the work the owner spends supervising his agents. Improve, not only fix: how agents behave during a task, and the infrastructure they run on between tasks. Keep the owner's explicit instructions and constraints. Investigate with evidence, finish improvements within the authority already granted, measure whether each one helped, and bring the owner only the decisions that are his, with the evidence.

"I noticed something and wrote it down" is not an improvement. A change that shipped and measurably helped is.

## The questions it keeps asking

Each family is a standing question. A case is one answer in progress.

| Family | Standing question |
|---|---|
| Owner burden | What made the owner step in, correct an agent or lose patience, and what did the agent do just before? |
| Owner blind spots | What does the owner miss when he is tired or busy, and how can agents guard against it? |
| Manual work | What does the owner keep doing by hand, and what would do it for him? |
| Sessions | Which sessions stopped, stalled or wait on him, and which of those can move on without him? |
| Pressure | What load, memory, IO or money goes to waste, and what would cut it? |
| Capacity | What is trending to the edge (disk, quotas), what does every cleanup do by hand, and what did it miss? |
| Component health | Which installed component (memory, supervisors, guards, this tool) works poorly, measured? |
| Failures | Which tool or infrastructure failures recur, what is the root cause, and what fixes it at the source? |
| Outside | Which new system, model or result could replace a part of this setup, and does it win when tried here? |

## What it replaces

One tool, one case store, one loop, in place of separate scouts, boards, detectors and periodic review prompts: a findings scout with its curator and phone page, an incident detector with its investigator and escalation, the periodic tool-failure review, the "things I can pick up" scan, the session board and its watchdogs, and supervisors with nothing to supervise. During migration the existing phone page keeps working by reading this tool's cases.

Two existing Go codebases are the starting point, not something to rewrite: a session board's per-harness scanners (live state of Claude Code, Codex, pi, opencode and Hermes sessions every 15 s, with no model in the loop) become the live session layer, and tacit's transcript readers serve history.

Session supervision is core, not a later extra. In Phase 0, most of the owner's manual work was asking sessions for status, answering approvals the agent had already recommended, running steps that need his identity, and nudging stopped sessions back to work.

## Boundaries

- **Knowledge stays in tacit.** The tool reads facts with tacit's search and fact interface and publishes a validated result with `tacit_add`, with its conditions and evidence. It keeps no second fact store, and a hypothesis is never written as a fact.
- **Personal content never enters this repository.** Cases, evidence and state live on the machine under the XDG directories (`~/.local/share/momentum`, `~/.config/momentum`). Never under `/tmp`.
- **Authority is inherited, not grown.** It follows the machine's shared agent rules and, for unattended runs, the scheduled-AI policy: external repositories and other people's issues are report-only, paid compute goes through the spend gate, and it never widens its own authority or changes its own evaluator to make a candidate pass.
- **Evidence keeps three lines visible:** the owner's own words versus generated text (subagents, compaction summaries, harness text, experiment prompts); a claimed completion versus an observed one; production work versus experiments, with experiments tagged so they never feed back as evidence of the owner's preferences.
  - On top of tacit's attribution rules, these do not count as owner text: the same text landing in two sessions within 2 s (a program sent it), wake-loop messages, and agent briefs that open sessions.
  - Prompts a tool pre-filled for the owner are kept but labeled: submitting them is still manual work.
  - Each owner turn also records its input kind (typed, queued, shell, slash command, answer to an agent question, accepted suggestion) and the system event just before it.

## Architecture

- **One agent loop with skills and tools, not an orchestrator of roles.** The harness's own main agent runs the loop.
  - Each question family is a skill: its procedure, rules and judgment calls.
  - Hard limits live in the tools, not in prompts: authority and write gates, the spend gate, budgets, validation.
  - Subagents are used only where they earn their own context: parallel investigations that need fresh, disposable context, and clean-context verification.
  - Splitting work by problem type turns handoffs into a telephone game. An earlier orchestrator in this setup, split by role, cost hundreds of times the tokens of the native path per merged task.
  - Prior art: Anthropic, ["When to fold subagents into your main agent loop"](https://claude.com/resources/articles/building-agents-when-to-fold-subagents-into-your-main-agent-loop) (2026-10-22), and [`anthropics/single-agent-loop-kit`](https://github.com/anthropics/single-agent-loop-kit). [arXiv 2601.04748](https://arxiv.org/abs/2601.04748) finds that skill selection holds up to a critical library size and then drops sharply, driven by how alike the skills are. So the skill set stays small and distinct, and selection is measured.
- **Go.** tacit's client is Go, and the readers this tool needs are Go packages there: transcript discovery, the Hermes database reader, owner attribution, redaction, the model runner and the note reviewer. Milestone 1 moves them out of `internal/` so both tools import them. The CLI exposes a few tools grouped by intent (evidence, case, measure, ship), not one per function.
- **A CLI and systemd timers, no daemon** until a measured need shows up.
- **SQLite** for events, cases and experiments; artifacts (patches, reports, rollback manifests) as files next to it.
- **Thin harness adapters:** a Claude Code mod, a Hermes general plugin (next to tacit's memory provider, not in its place), Codex hooks. Observation reads transcripts as the source of truth. Hooks add live signals but miss things: a call to a tool that does not exist never reaches Claude Code's tool hook.

## Live session layer

The `sessions` packages are exported, so the session board imports them instead of keeping its own copy. They read processes, tmux panes, transcripts and the harnesses' own state, never a model:
- **Discovery:** Claude Code from the agent process under each tmux pane, Claude Code's per-process probe files and `claude agents --json`; Codex from its processes, the rollout files they hold open and its state database; pi, opencode and Hermes from their processes, session files and databases. Harness locations follow the harnesses' own variables (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, `XDG_DATA_HOME`, `HERMES_HOME`, `PI_CODING_AGENT_DIR`). A `Lister` interface lets another tool's list stand in for native discovery.
- **Identity:** a session is keyed by its job, thread or session id, else by its process (pid and start time), never by a tmux pane id, which a restarted tmux server hands out again. Rows from another machine are dropped: their files are not here.
- **Phases:** needs_you, working, background, scheduled, idle, done. The rule, in order: a blocking ask gives needs_you (an async ask blocks only once its turn is over); an open turn gives working; tasks in flight give background; an error gives needs_you; a failed background job gives needs_you; a wakeup or loop gives scheduled; otherwise idle. Without a transcript, only the harness's own status counts, never a value a board wrote back.
- **Reviews:** only a model's review moves idle to needs_you or done, and only when it covered the same idle stretch and quotes the request word for word from the last message. The `verdict` package holds the prompt, the reply parser and that gate; the caller makes the model call.
- **Between runs:** each transcript tracker's place (Claude Code, Codex, pi) is a checkpoint in the store, one per transcript: the read position, what identifies the transcript (device and inode, its first 512 bytes, the 512 bytes before the position), and the state derived from what was read, with the activity log kept entry by entry. A run resumes from it and reads only what was appended. A transcript that shrank, was replaced, or was rewritten so that its first 512 bytes or the 512 bytes before the position changed is read again from the start. A checkpoint holds only what the transcript decided, never the clock or another file, so a resumed run derives what a full re-read derives. Only the build that wrote a checkpoint uses it: a run of another build reads the transcript in full and replaces it, so two builds used in turn both read in full. Checkpoints are derived: a schema change drops them, and one unused for 15 minutes is dropped. The database logs (opencode, Hermes) are read in full on every run, since both harnesses rewrite rows in place, which no cursor can tell. Hermes's log is the conversation as Hermes holds it, its active messages, so a turn it takes back and redoes with as many entries starts no new stretch. Sessions runs on one store take turns on a lock, from before the scan until the save; a run waits up to 10 seconds for the one before, then fails and names it. The checkpoint tables are a cache: when their columns are not this build's, they are made again, and only they are. When each session entered its phase is kept in the store's state tables, which a schema change of the derived tables does not drop.
- **History:** each run compares the sessions it found with the stamps it saved and writes one event per change of stretch, with source `sessions` and actor harness: a session appeared, entered another phase, began a new stretch of the same phase after it moved between two runs, or vanished (unseen for 15 minutes by scans of its harness that worked: while a harness's scan keeps failing, its departures wait, and come dated at their last sighting). A session a source read only in part (a query of its own failed, or a Codex whose thread could not be looked up) changes nothing on that run. Its Meta holds the phases left and entered, the time in the phase left (up to the run that saw the change, and a lower bound up to the last run that saw the old phase), the session's name, and for needs_you the ask kind and text, entering and leaving. These events are observations: nothing can read them again, so a schema change keeps them and `evidence rebuild` refuses them.
- **Attribution:** user-role text in the activity log is what the harness recorded in the user role, labelled with the harness's own origin when it gives one. It is not attribution to the owner; evidence about the owner comes from tacit's attribution.

`momentum sessions` prints the live sessions and their phases from this layer.

## Evidence it keeps

Phase 0 showed that each family needs history the machine does not keep today. The collectors are cheap and keep running when the rest of the tool is down.

- **Sessions:** live state per session and its phase history (how long each session waited on its owner, and on what), input kinds and wait times for owner turns, and the agent's last words before a stop.
- **Machine:** free space per disk every 5 minutes, with the writers behind a fast drop (per-process writes, working directory, the session that owns the process). Per-unit CPU, memory, IO and restart history beyond the journal's retention, plus restart rates with exit reasons. Journal volume per source.
- **Scratch:** an owner and a status for every scratch folder and worktree (which session made it, whether its task or PR is closed, whether its work is pushed). A record of what each cleanup freed, measured, and why.
- **Money:** per-run model spend of every background job, including the ones that do not report a price today.
- **Memory delivery:** a log of every fact a hook injected (trigger, route, score, matched terms, store revision). A standing labelled set of deliveries graded useful, weak or noise. A miss probe that replays sampled prompts at their historical store revision. Without these, nobody can measure whether a pushed fact helped. In Phase 0 most injected lines were unrelated to the prompt they were attached to, and about half of the facts that applied were not delivered.
- **Latent loops:** jobs that fail and retry on a schedule, tunnels and apps that keep dialing targets that are gone, and timers whose project or script no longer exists.

## Cases

A case holds: its family, evidence references, the suspected mechanism, alternatives, the next discriminating test, the expected benefit, the cost spent so far, and its status.

Status runs open, investigating, testing, deployed, measuring, then closed with a reason: improved (with the measurement), not worth it, insufficient evidence, or already solved.
- Cases dedupe by root cause, not by symptom.
- A dismissed case does not come back unless its evidence changes.
- A case unseen past its evidence retention closes by itself.
- A loop that fails the same way twice stops and opens a case instead of retrying.

## The loop

1. Detect, then open or update a case.
2. Investigate.
3. Build a change package.
4. Deploy within authority.
5. Measure.
6. Close, and publish the validated lesson to tacit.

**Prior art comes before building.** Any case that would add a new component, workflow or architecture starts with a search of current primary sources (labs' engineering posts, papers, released kits) for the same problem. It states what they found and why this design follows or departs from it. The earlier orchestrator was built while published guidance and results against its design already existed.

**A change package** holds the patch or configuration change, where it applies (model, harness, task family, tool version), the evaluation plan, the baseline and candidate results, and the rollback.
- Prompt changes are one option among several. A substantial case must weigh a deterministic fix, a tool or interface change, or an architecture change before adding another instruction.
- Instructions the tool injects are checked against the live harness: the tools they name exist, and the command forms they recommend pass the harness's own checks.

## Evaluation

The baseline is the same question answered natively by subagents without the tool (Phase 0). The tool has to beat it on cost and time per useful result, or the native path stays.

Evals come first: one shared end-to-end set of golden cases with known outcomes, for example real failures whose root cause and fix are already verified.
- Each task is graded on whether it was completed.
- Each task runs more than once.
- Every comparison reads which tasks flipped, not only the total.

Scorecard:
- owner interventions and corrections per week;
- recurrence of root causes after their fix;
- improvements shipped with a measured benefit;
- pages and questions to the owner;
- false alarms;
- model spend.

Fewer questions does not count as success when it comes from unauthorized assumptions.

## Milestones

| | Deliverable | Exit condition |
|---|---|---|
| M0 | Phase 0: the families answered natively | A baseline (cost, time, findings) and the list of readers and queries each family needed |
| M1 | Evidence layer: tacit's readers exported, the session scanners, event store, ingestion of transcripts, the failure log, the scout's findings and the detector's incidents | Reconstructs a real failure without mixing the owner's words, generated text and tool output |
| M2 | Case store and the loop, running three families at once | One week in which it ships at least one measured improvement per family and pages less than the tools it replaces |
| M3 | Remaining families, including Outside | An improvement nobody asked for, shipped with evidence |
| M4 | Behavior layer: a compact task contract, guidance checked against the live harness | Held-out cases of under- and over-following improve without new routine approval prompts |
| M5 | Replaces the scout's phone page and the detector; Codex and Hermes adapters | The same cases and improvements survive a switch of harness or model |
