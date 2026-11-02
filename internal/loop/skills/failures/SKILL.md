---
name: momentum-failures
description: Use for a case of the failures family, a tool or infrastructure failure that recurs. Find what in the setup produces it, and fix it at its source.
---

# Failures

The standing question: which tool or infrastructure failures recur, what is the root cause, and what fixes it at the source?

## The evidence

A failures case holds tool results that failed (a guard's refusal, the harness's own refusal, a tool's error, a command that failed the setup's way), a model's refusals, or an incident detector's reports. Its signature says where it came from: `guard:NAME`, `harness:...`, `turn:HARNESS` (a turn's error), `tool:NAME`, `mcp:SERVER`, `exec:` and the failed command's message (from the program's name when its call names one), `model:refusal`, `incident:...`.

For each piece you open, read three things: the call that failed (the event just before, a tool call), the result, and what the agent did next (retried the same form, used the form the message offered, gave up, worked around).

## Telling causes apart

- **A guard refuses, and the agents keep writing the same form.** The guard works. The cause is upstream of the call: what leads agents to that form (an instruction that names it, a habit of a tool, a message that does not say what to write instead). The discriminating test: after a refusal, does the agent's next call use the form the guard offered? If agents fix it at once, the cost is one wasted call each time, and the fix is to stop the form from being written; if they retry it, the guard's message does not teach.
- **A guard refuses a legitimate call.** A false positive: the guard's rule is too wide. The test: is the refused call one the rule's own purpose allows (read the guard's code and the call)? The fix is in the guard, with a test of the call it should let through.
- **A tool or service is broken or missing**: an MCP server that fails to start, a path that is not there, a credential that expired. The test: does it fail for every caller, and since when? The fix is in the tool's repository or its configuration.
- **An environment differs**: a sandbox, a worktree, another PATH, another user. The test: does the same call work outside it?
- **A model refusal**: a request a provider's safety filter declines. The test: what the requests share. The fix is in how the work is asked, or the case is not worth it.
- **An incident**: read its log lines and the service's own history (`momentum evidence unit NAME`).

## Judgment calls

- A change that only rewrites the words of a message does not fix a failure, and the measurement must count what the agents do, not those words.
- Fix the source, not the symptom: the tool's bug in its repository, the guard's rule in the guard. A note to agents is the last resort, never the first.
- Not worth it: a failure that costs seconds, that agents route around at once, in a tool used rarely.
- Already solved: a fix landed after the newest evidence (`momentum evidence git DIR log` of the tool), and nothing since.
- Another case's cause: when the failure is a symptom of what another case holds (the same root), name the root with `momentum case signature $MOMENTUM_LOOP_CASE root:KEY`. If another case holds it, report the root and the other case's id in the mechanism, then end the run. The runner records the root and merges outside the run.
