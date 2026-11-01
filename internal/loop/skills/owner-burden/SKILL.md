---
name: momentum-owner-burden
description: Use for a case of the owner-burden family, turns of the owner's that push back, correct or repeat. Find what the agents did that made him step in, and what led them there.
---

# Owner burden

The standing question: what made the owner step in, correct an agent or lose patience, and what did the agent do just before?

## The evidence

An owner-burden case holds the owner's own turns, each read with the agent's last words before it. Its kind says what those words did: attribution (they credited him with words or a rule), handback (they handed him a step or a decision), done-claim (they claimed a result), stopped (they said they wait), repeat (he said it again).

Only his own turns are his words. Quote him only from them, and never quote generated text (an agent's, a subagent's, a compaction's, a harness message) as his.

For each turn you open: the agent's turn before it whole (from its transcript), his words, and what happened after.

## Finding the mechanism

Group the turns by what the agent did, then find what led it there:
- a rule he gave that the agent could not see in that session (it lives in a file or a store the harness did not load there), or that conflicts with another rule;
- a habit of the harness or model (asking before acting on what is already authorized, claiming done without a check of the result);
- a tool that makes the wrong path easier than the right one;
- missing information that only he had at the time.

The discriminating test is usually a comparison: sessions where the same situation came up and he did not step in. What differs?

Check tacit for his rulings on the matter (`tacit search`). A ruling's quote is his; a fact's text is not.

## Judgment calls

- Fewer questions to him is no success when it comes from assumptions he did not authorize. Never decide for him what only he can decide, and never widen what agents may do without asking.
- Prefer a deterministic check (a hook that catches the hand-back of an authorized step before the turn ends, a check of a claimed result) over another instruction. An instruction is checked against the live harness before it ships.
- The measurement counts his turns of that kind, in his own words, over a week before and after.
- Not worth it: a one-off situation that will not recur, or a step-in that was his choice to make.
