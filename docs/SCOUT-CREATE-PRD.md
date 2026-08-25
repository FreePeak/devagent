# Scout + Create — Factory PRD

> Goal: `devagent create` bootstraps a 24/7 autonomous factory:
> 1 opencode scout (research → PRD → tasks) + N Orca-workspace devagents (queue → implement → test → PR → auto-merge → self-update) on macOS.

Status: Draft 2026-08-25. Implements gap in docs/PRD.md §17 + docs/SELF-BUILD-LOOP.md (single-process loop → factory).

## 1. Problem

`scripts/selfbuild-loop.sh` is a single-process `Research → Ideas → Validate → Plan → Implement → Testing → Push` loop. It conflates discovery and implementation, has no queue decoupling, no concurrent workers, no 24/7 daemon beyond a bash while-loop, and no macOS persistence. The requested factory separates concerns: scout writes to a queue/ledger; workers consume independently in isolated Orca worktrees and keep devagent itself current.

## 2. Definitions

- **Scout**: 1 long-lived `opencode run --format json` process (fallback `claude-code`) that every `scout.intervalMinutes` researches competitor moves + backlog, selects one iteration-sized idea, writes a markdown PRD to `.devagent/prds/` (or `.selfbuild/scout/` for backwards compat), enqueues a task JSON to `.devagent/queue/`, appends to ledger. Runs as macOS LaunchAgent for 24/7.
- **Queue**: repo-local filesystem queue `.devagent/queue/<taskId>.json` + `.devagent/prds/<taskId>.md`, statuses `pending → claimed → done/failed` with claim via atomic rename/file-lock.
- **Worker**: any `devagent` invocation that claims one queued task, creates `.devagent-worktrees/<taskId>` (and optionally an Orca worktree `orca worktree create`), runs `pipeline`/`task` with validation gates G1/G3/G4, pushes branch `devagent/<taskId>` and opens PR via `gh`, optionally auto-merges.
- **Create**: `devagent create` — one-shot factory bootstrap: init dirs/config, optionally register repo with Orca (`orca repo add`), install scout LaunchAgent, spawn initial workers, print dashboard URL.

## 3. User stories

- As Mai I run `devagent create --repo . --workers 3 --scout` once and get a 24/7 scout + 3 Orca workers consuming the queue.
- As Duc I run `devagent scout --once --dry-run` in CI and assert a PRD+task were produced.
- As Tuan I `launchctl list | grep devagent.scout` and see the scout heartbeat; `devagent scout-status` shows last cycle.

## 4. Requirements

### FR-SCOUT-01 — Scout daemon (opencode 24/7)
- `devagent scout [--once] [--dry-run] [--interval <min>]` runs loop: research prompt → opencode → parse Goal/PRD → write prd + enqueue → ledger → sleep. `--once` exits after one cycle. Heartbeat file `.devagent/scout.heartbeat.json` updated every cycle.
- Research prompt sources: `docs/PRD.md §4 + §17`, `.selfbuild/ledger.jsonl`, `.selfbuild/lessons.md`, optional web-search note. Output expectation: markdown with `## Goal` + `## PRD`.

### FR-QUEUE-01 — Queue + PRD store
- `src/queue.ts` exposes `enqueueTask`, `listTasks(status)`, `claimNextPending(workerId)`, `updateTaskStatus`, `writePrd/readPrd`, `pruneDone`. Directory `.devagent/queue/` + `.devagent/prds/`. Single-task file = unit of concurrency; no external DB.

### FR-CREATE-01 — `devagent create`
- `devagent create --repo <path> [--scout] [--workers <n>] [--auto-merge] [--self-update]` creates dirs, writes/merges `devagent.json` defaults (`scout.worker=opencode`, `queue.dir=.devagent/queue`), installs LaunchAgent plist when `--scout` and on macOS, optionally `orca worktree create` for workers. Idempotent.

### FR-WORKER-01 — Orca fleet provisioner
- Extend `src/integrations/orca.ts` with `createOrcaWorktree`, `listOrcaWorktrees`, `ensureOrcaRepo`. Workers prefer Orca worktrees when `orca` binary + repo is registered; gracefully degrade to git worktrees.

### FR-WORKER-02 — Queue consumer
- `devagent consume` or `devagent fleet --from-queue` claims one `pending` task, runs `createWorktree` + `runPipeline`/`implementStage`, validates G1/G3, pushes + PR, updates task status, optionally `gh pr merge --auto`.

### FR-MERGE-01 — Auto PR + auto-merge
- After green gates, `src/integrations/github.ts` `autoMergePr(repoPath, prUrl)` runs `gh pr merge --auto --squash` (or API). Controlled by `config.autoMerge` / `--auto-merge`. Never merges on red.

### FR-SELF-01 — Self-update
- `scripts/self-update.sh` + `src/self-update.ts` helper: `git pull --ff-only`, `npm ci && npm run build`, `launchctl kickstart` scout if installed. Only runs on demand or after successful merge when `selfUpdate=true`; never auto-pulls with dirty worktree.

### FR-PERSIST-01 — macOS persistence
- `scripts/install-scout-launchagent.sh` writes `~/Library/LaunchAgents/com.devagent.scout.plist` running `devagent scout --interval <n>` with `KeepAlive`+`RunAtLoad`, heartbeat file, log to `~/Library/Logs/devagent-scout.log`. `plutil -lint` clean, install/uninstall idempotent.

## 5. NFR

- No secrets in repo or logs; env-only credentials.
- Degrades cleanly without Orca/opencode/gh installed.
- Single-task files avoid DB; max queue depth unbounded but `list` paginates.
- Tests mock `spawnCli`/`orca` runner; no real `opencode`/`orca`/`gh` in unit tests.

## 6. CLI delta

```
devagent scout [--once] [--dry-run] [--interval <min>] [--worker opencode|claude-code]
devagent scout-status [--repo <path>]
devagent create --repo <path> [--scout] [--workers <n>] [--auto-merge] [--self-update] [--dry-run]
devagent queue list [--status pending|claimed|done|failed] [--repo <path>]
devagent queue show <taskId> [--repo <path>]
devagent consume [--repo <path>] [--once] [--auto-pr] [--auto-merge]
```

Existing commands (`run`, `task`, `fleet`, `orchestrate`, guard, serve) unchanged.

## 7. Architecture delta

- New modules: `src/queue.ts`, `src/scout.ts`, `src/create.ts`, `src/consume.ts` (or queue-consumer), `src/self-update.ts`.
- Extend: `src/config.ts` (scout/queue/create fields), `src/cli.ts` (new commands), `src/integrations/orca.ts` (create/list), `src/integrations/github.ts` (autoMerge).
- State dirs: `.devagent/queue/`, `.devagent/prds/`, `.devagent/scout.heartbeat.json` (gitignored). Reuse `.selfbuild/ledger.jsonl` and `lessons.md`.

## 8. Verification

- `npm run typecheck && npm test` (skips real CLIs).
- `devagent scout --once --dry-run` produces `.devagent/prds/<id>.md` + `.devagent/queue/<id>.json`.
- `devagent create --dry-run --repo /tmp/empty` creates dirs + prints plan without mutating.
- With mocked `orca` runner, fleet creates worktrees; with mocked `gh`, PR+auto-merge path covered.
- `plutil -lint` on generated plist passes.

## 9. Risks

- Scout LLM hallucination → defer to `checkSpec` + human gate; stale queue items pruned manually.
- Orca app not running → fallback to git worktrees; `dropOrcaWorkspace` already does this.
- LaunchAgent duplicate installs → install script is idempotent (`launchctl bootout` before `bootstrap`).

## 10. Roadmap fit

Phase A (this doc): queue, scout once/dry-run, create bootstrap, orca create/list.
Phase B: consume loop + auto-merge + self-update + LaunchAgent.
