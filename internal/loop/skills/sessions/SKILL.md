---
name: momentum-sessions
description: Use for a case of the sessions family, sessions that stopped, stalled or wait on the owner. Find why they did not move on without him, and what would move them.
---

# Sessions

The standing question: which sessions stopped, stalled or wait on him, and which of those can move on without him?

## The evidence

A sessions case holds supervision's own record (a rule that gave up, a message that did not land or whose turn stopped the same way again), the owner's nudges to stopped sessions that no rule would have continued (by why none would), or the waits on him past the owner window (by what the session asked).

Read the stop: the session's events before it and up to what moved it (`momentum evidence events --session ID --since T --until T`), its last words, its last call, the error it ended on, what was in flight. `momentum supervise replay --since 7d --points FILE` writes every stop the rules saw, with what each rule decided and why; `--anchor` prints how long stops of each rule sat before what came next.

## Finding the mechanism

Two questions for each stop:
1. Why did the session stop? The harness ended the turn on an error, a request never got its reply, a wakeup was lost, the turn ended on a promise with nothing to wake it, it asked something that did not need him.
2. Why did no rule move it? No rule covers that stop; a rule's wait is longer than the owner's patience (he came first); a rule is dry (it records what it would have sent); a rule was kept off (the owner window, a draft on the input line, an automated run).

The discriminating test is the replay: run the rules over the history with a changed wait or a rule made live, and count what it would have done before the owner's own step and what it would have done unasked.

## Judgment calls

- A rule goes live only when the replay shows it would act unasked no more often than the owner's own nudges went to sessions that would have gone on by themselves (SPEC, Supervision). Show the replay's counts in the proposal.
- A wait changes only within what the history shows: past what the sessions do by themselves at such stops, below the owner's patience.
- Never type into a session yourself, never answer a session's question for him, and never act on an automated run.
- A question a session asks that only he can answer is his: the case is the question's cause (what made the agent ask), not the wait.
