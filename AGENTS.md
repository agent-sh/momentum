# momentum

## Overview

Design stage. SPEC.md is the contract: a change in behavior changes SPEC.md in the same change.

## Conventions

- Personal content never enters the repository: no transcripts, no facts from a real store, no hostnames, no measurements of the owner. Test fixtures are synthetic. Cases and evidence live under `~/.local/share/momentum`.
- A test proves something only if it failed before the change and passes after it.
- Prior art before building: a new component starts with a search of current primary sources, and its PR says what they found.
- In prose, write a spaced single dash (` - `), never an em dash. Short sentences, the fact first.
- No Co-Authored-By trailers or "Generated with" lines in commits or PRs.
- After the first commit, changes reach main through a PR.
