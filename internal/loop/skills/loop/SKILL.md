# Build one cause's change

Use the case's mechanism and findings to fix one cause with one change and its test. Prefer a deterministic fix before another instruction. Keep every unrelated session's work intact.

Keep real-store text, paths and names out of the title, body and findings.

/work is an independent clone at BASE. Its files and Git history are yours to edit. /out is writable scratch. /ro and /run/input are read-only host inputs. Toolchains and module caches are read-only. Build caches are under /tmp.

Prove the test fails without the change and passes with it. Use synthetic fixtures. Never edit the loop's detectors, measurement, gates, sandbox, skills, budgets or configuration. Never work in an external repository, use paid compute or reach the owner's phone or desktop.

There is no network for research or repository traffic. If prior art needs a search, say so in the proposal body. Do not invent sources.

Read review.md when present, and refusal.md, which says why the host refused this case's last build. Adapt previous.patch to BASE when present. Finish with summary, test_command, title, body, rollback, signal, window and lesson. The host makes the patch, checks it and proposes the change.

test_command is a command, not a description: the program and its arguments, one word each, for example `["go", "test", "./internal/capacity", "-run", "TestNamedTargetIsKept"]`. The host runs it twice in the sandbox, without a shell, from the tree's root: on BASE with only your test files (it must fail) and on your change (it must pass). Its first word is a test runner on the sandbox's PATH (go, cargo, npm, pnpm, bun, npx, node, python3, pytest, uv, make, bats). No pipes, redirections, variables or `~`. Say what the test proves in the body.

The case's `NextTest` is the investigation's proposed fix, in prose. Build that fix; it is not the test command.

The paths the loop may land are in the build prompt. Prefer a fix inside them. When the right fix needs other paths, make it there anyway: the host checks and reviews it the same way and proposes it for a person to land by hand. The paths the prompt says are never changed refuse the whole change.

Choose a week, or two for a rarer problem. A window is whole weeks. The signal must hold the case's problem in at least five active hours in the gate. The host compares the three independent windows before that gate with the window after the change is in effect.

When the machine runs a binary or an installed configuration, return its host file as installed. It must name a regular file without links in a directory admitted by loop.read_dirs, outside run storage. The host checks its revision or rebuild before measuring. Otherwise the repository checkout is what takes effect.
