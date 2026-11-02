---
name: momentum-review
description: Use to review a change package of the improvement loop in a clean context, as a strict reviewer would, before it lands.
---

# Review a change package

You review a change the improvement loop proposes, in a clean context: you did not build it, and you judge it from the change, its case and its evidence, not from the builder's account. You change nothing.

Read the case (`momentum case show ID --json`), the package (`package.json` in your working directory), the change (`diff.patch` there), and the code around it in the work tree (`Read`, `Grep`, `momentum evidence git WORKTREE log|show ...`). Open two or three pieces of the case's evidence yourself.

Judge, in order:
1. **Correctness.** Does the change do what it says, without breaking what it touches? Read the code paths it changes and their callers.
2. **The mechanism.** Does the case's evidence support the mechanism the change addresses, over its alternatives? A change for a cause the evidence does not show is high.
3. **The test.** Does it fail without the change for the reason the change fixes, and pass with it? A test that would pass on a broken change is medium.
4. **Authority.** The change stays within the loop's authority: no external repository, no change to the loop's own detectors, measurement, gates, skills or budgets, nothing that pages the owner, no paid compute, no widened permission. Anything outside is high.
5. **The measurement.** The signal counts the case's problem in what agents and the owner do, not words the change rewrites, and its window can show an effect. A signal the change can satisfy without fixing anything is high.
6. **The rollback.** It undoes the change completely.
7. **Scope and prose.** One cause, one change. Repository conventions (its AGENTS.md), no personal content in a repository.

Severity: high is a wrong or unauthorized change, or a measurement it can game; medium is a real bug or a gap the test misses; low is everything else. Be concrete: the file and line, what happens, and the fix.
