# This fork

This repository is the agentic-orchestration fork of [Sortie](https://github.com/sortie-ai/sortie). Its working branch is `plan-bench`; agentic-orchestration pins it as the `sortie/` submodule.

## Merging upstream

- Merge upstream `main` into `plan-bench` at each upstream release, or weekly when no release comes, whichever is sooner.
- Merge, never rebase: open a `merge/upstream-<date>` branch from `plan-bench`, run `git merge upstream/main`, and land it through a pull request. Rebasing would rewrite commits that agentic-orchestration's submodule pins.
- Record in the merge commit body each conflict whose resolution changed the meaning of either side: the file, what each side did, and what was kept. Record the upstream commit merged, so the next merge knows where it starts.
- Keep fork work in fork-owned packages and files, with small hooks in upstream files, so each merge stays small. Fork database migrations are numbered from 1001 and fork ADRs from 1001, so upstream's numbers never collide with them.

Last merged upstream commit: `c48b724b` (2026-10-28).

## Worker leases

Jobs for `environment.kind: remote` are matched to workers in `internal/worker`, wired into the orchestrator in `internal/orchestrator/remote.go`, configured in `internal/config/environment_remote.go` and served by `internal/server/worker.go`, following the `agent-platform/worker-api` (protocol 1) and `agent-platform/worker-dispatch` specs. The lease rule sits behind the `worker.Leaser` interface. Until upstream #1499 settles, the fork follows a fixed-duration rule (`TTLLeaser`): one worker holds a job at a time, a lease lasts its TTL from the grant or last renewal, and an expired lease ends the attempt as failed; the worker's run slot, CPUs and memory return, and its disk stays counted as a stopped machine's until the worker's next check-in. Replacing the rule means implementing `Leaser`.

## Plan sync from the workbench

The agentic-orchestration workbench syncs the plan on its own, with no Sync step for the user. It calls `POST /api/v1/plan/sync` with `"apply": false` to preview, then applies that preview with `"apply": true` and the preview's `preview_id`. After a sync that released a story, it calls `POST /api/v1/refresh` so the released story is dispatched on the next tick instead of the next poll. Both routes take the control token. Nothing else in Sortie changes for this.
