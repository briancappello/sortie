# This fork

This repository is the agentic-orchestration fork of [Sortie](https://github.com/sortie-ai/sortie). Its working branch is `plan-bench`; agentic-orchestration pins it as the `sortie/` submodule.

## Merging upstream

- Merge upstream `main` into `plan-bench` at each upstream release, or weekly when no release comes, whichever is sooner.
- Merge, never rebase: open a `merge/upstream-<date>` branch from `plan-bench`, run `git merge upstream/main`, and land it through a pull request. Rebasing would rewrite commits that agentic-orchestration's submodule pins.
- Record in the merge commit body each conflict whose resolution changed the meaning of either side: the file, what each side did, and what was kept. Record the upstream commit merged, so the next merge knows where it starts.
- Keep fork work in fork-owned packages and files, with small hooks in upstream files, so each merge stays small. Fork database migrations are numbered from 1001 and fork ADRs from 1001, so upstream's numbers never collide with them.

Last merged upstream commit: `c48b724b` (2026-10-28).
