# Orchestrator × Factory — bridge decision (devagent self-build)

Date: 2026-08-25 · orchestrator `main@e0079ab` (planner `src/orchestrator/planner.ts:23` / scheduler `src/orchestrator/scheduler.ts:76` / auditor `src/orchestrator/auditor.ts:18` / `types.ts:104 BOARD_FILE .devagent-project.json`) vs factory `3-role` (`src/queue.ts` + `src/scout.ts` + `scripts/build-loop.sh` + `src/tracker.ts` + `src/create.ts:114 rolePlistSpecs`).

## What each does

**Factory (queue model).** Scout (`src/scout.ts:158 runScoutOnce`) researches PRD §4+§17 + ledger + lessons, writes one idea per cycle as `.devagent/prds/<id>.md` + task `.devagent/queue/<id>.json` (`status pending → claimed → done/failed`, FIFO via `claimNextPending`). Builder (`scripts/build-loop.sh`) polls the oldest `pending`, drives one task through `devagent consume --auto-pr --auto-merge` (worktree-per-task `src/git/worktree.ts:51`, gates G1/G3/G4 via `src/consume.ts`, auto-merge via `src/integrations/github.ts:127`, self-update `src/self-update.ts`), records `.selfbuild/ledger.jsonl`. Tracker (`src/tracker.ts`) snapshots queue + scout heartbeat + ledger + git + gh into `.selfbuild/progress.md`. Coordinator is `devagent create` — provisions Orca worktrees (`orca worktree create`) and up to three LaunchAgents (`rolePlistSpecs`, `plutil -lint` clean) so the factory survives reboots; `shouldInstallLaunchAgent` guards ephemeral tmp repos.

**Orchestrator (DAG model).** Planner agent decomposes a goal+repo into 2–6 small precise tasks with `prompt` + `acceptanceCriteria[]` + `dependsOn[]` (validated by `parsePlan`/`hasCycle`/`fallbackPlan`). `createBoard` persists a `.devagent-project.json` `ProjectBoard` (goal + deduped tasks + roles). Scheduler `runScheduler(board, {concurrency, maxTaskRetries, maxRecoveries, maxWaves})` dispatches `ready` tasks in dependency waves (`recomputeReadiness`, each wave isolated), then the executor applies four domain gates (G1–G4). Executor outcomes land as `untrusted` until an independent auditor `auditor.ts` verifies each criterion against host evidence (file exists / test pass / content) and emits `AuditVerdict {pass|fail|ask, integrity clean|suspect|violation, criteriaResults[]}` — only `pass/clean` flips to `done`. Retry budget, `repeatGaps`/`repeatGapThreshold`, and recovery contracts `runRecoveryPlanner` (`src/orchestrator/planner.ts:152`) handle regressions. Merge passes through `src/orchestrator/merge.ts:19` (sequential `git merge --no-ff`, worktree cleanup).

## Decision

**Keep both; bridge them.** They solve different axes:

| Axis | Factory | Orchestrator |
|---|---|---|
| Unit of work | single end-to-end feature (one PR) | DAG of 2–6 interdependent micro-tasks inside a goal |
| Decomposition | scout picks ONE idea via backlog heuristics | planner LLM decomposes ONE goal into a dependency graph |
| Validation | repo gates (tests + migration rules) | gates **plus** independent auditor with evidence per criterion |
| Resume | queue files keyed by id; ledger starvation/circuit breaker | board file + attempt suffix; wave-persisted, resumable |
| Target | broad PRD backlog (long-horizon breadth) | deep contract around one goal (long-horizon depth) |

Self-build needs **both**: breadth for discovery and depth for contracts that gate trust. The bridge is small and local.

## Bridge design (implemented in this goal)

Module `src/orchestrator/queue-bridge.ts` (new, deps on `src/queue.ts`, `src/orchestrator/types.ts`, `src/orchestrator/planner.ts:parsePlan`):

- Reads queued tasks as goal strings; for each pending goal calls the planner (or a dry-run stub) to expand it into board tasks; writes/extends `.devagent-project.json`. Idempotent by goal hash; respects `dependsOn` and audit semantics. Conversely, can flatten board tasks back to queue items for Orca workers that only watch the queue. On a fresh bridge the source queue item is marked `done` so the builder lane never double-builds the same idea.
- Tracker expands: when a board exists it reports `ready/pending/done/failed/ask` counts into `.selfbuild/progress.md` alongside queue counts.
- Builder (`build-loop.sh`) prefers the board when it exists (orchestrates), else falls back to single-task `consume`. No new LaunchAgent needed — the existing builder slot drives whichever source is present.
- `create --orchestrator` adds `com.devagent.orchestrator` slot reusing `rolePlistSpecs` patterns (keeps the shared tmp-guard). The slot runs `scripts/orchestrate-loop.sh`, which auto-runs `devagent queue bridge` when no board exists, then drives `orchestrate --goal <ORCHESTRATOR_GOAL> --resume` until the board reaches terminal states. `repoPath` is normalized to an absolute path in `runCreate` — relative `--repo` values previously produced broken plists (`WorkingDirectory .`).

Why not merge into one store: queue files are orphan-worktree-friendly (one file per feature) and survive `orca worktree rm` semantics; the board's single JSON is better for dependency reasoning. Keeping both stores is cheaper than migrating workspaces.

## Verification plan

- `src/orchestrator/queue-bridge.ts` unit tests: mocked queue + board fixture → bridge creates board, no secrets in evidence.
- `src/tracker.ts` extension: snapshot now includes `board` section when present (counts by status) — existing `test/tracker.test.ts` extended, backward-compat with no board.
- `src/create.ts` `--orchestrator` flag: four-plist generation with `plutil -lint` OK.
- `scripts/build-loop.sh` dry-run: picks board task vs queue task based on which file exists; logs which path was taken.
- E2E dry-run in fresh Orca dev workspace `devagent-orchestrator-dev` (child of `main`, on `origin/main`): `create --scout --tracker --builder --orchestrator --dry-run` previews 4 plists, `scout --once --dry-run → queue → bridge → board → builder dry iteration` all inspectable without touching real `main`'s queue/ledger.
- Live smoke still **bounded** in that workspace: one real scout fallback → queue → bridge → builder consumes one board task (bounded concurrency, limited retries), `consume --auto-pr` → local branch, PR creation only if `GITHUB_TOKEN` present; tracker then reflects the new board+queue counts and the `~/.devagent/runs` log validates the consume path.
