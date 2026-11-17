# momentum

## Overview

SPEC.md describes the design. Keep it roughly in step with the code when a change alters behavior, but it is not a contract: a gap between its wording and the code is a follow-up, never a reason to hold a change. README.md is the user-facing doc.

## Conventions

- Personal content never enters the repository: no transcripts, no facts from a real store, no hostnames, no measurements of the owner. Test fixtures are synthetic. Cases and evidence live under `~/.local/share/momentum`.
- A test proves something only if it failed before the change and passes after it.
- Prior art before building: a new component starts with a search of current primary sources, and its PR says what they found.
- In prose, write a spaced single dash (` - `), never an em dash. Short sentences, the fact first.
- No Co-Authored-By trailers or "Generated with" lines in commits or PRs.
- No file over 1 MiB and no ELF, Mach-O, PE, WebAssembly or ar-archive file (a program, an object file or a static library, by its first bytes) enters the repository: main keeps every commit of a PR, so CI's tracked check (`go run ./internal/tracked/check HEAD`) reads the whole history of a PR's head and of each push to main, not only the head commit. CI builds the check from the checked-out tree, so review a change to `internal/tracked` or `.github/workflows` by hand.
- After the first commit, changes reach main through a PR.

## Tests

- Run every `go test` and `go build` under a memory cap, after a look at free memory (`free -g`): `systemd-run --user --scope -p MemoryMax=8G -p MemorySwapMax=1G -- go test ...`. A run that outgrows the cap is killed alone, and the machine's other work goes on.
- Run the loop package with `systemd-run --user --scope -p MemoryMax=8G -p MemorySwapMax=1G -- go test -count=1 -timeout 50m ./internal/loop`. Its sandbox checks can exceed Go's default timeout. Use `-run` for focused checks.
- Go's test log is what grows: without `-count=1` the go command logs every file the tests stat or open, then reads that log whole and hashes what it names. The loop package turns the log off in its `TestMain` and fails a run whose log passes a bound (`testLogMax`), so its runs are never cached. In another package, do not walk a large tree through `os.Stat` or `os.Open` in a test, or in code a test runs: the sandbox checks its bind sources with the system calls for that reason.
- The loop's own runs need no prefix: each agent, check and review runs inside the sandbox unit, which has a memory limit and no swap.
