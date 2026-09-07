# Self-Build Infinity Loop

DevAgent builds DevAgent. Each loop iteration executes the full product cycle against
this repository, using DevAgent's own pipeline (`devagent task`) as the implementation
engine and Orca as the execution environment.

```
1.Research -> 2.Ideas -> 3.Validate -> 4.Plan -> 5.Implement -> 6.Testing -> 7.Push --+
    ^                                                                                 |
    +---------------------------------------------------------------------------------+
```

## State

All loop state lives in `.selfbuild/` (gitignored):

| Path | Purpose |
|---|---|
| `.selfbuild/ledger.jsonl` | Append-only log: one JSON line per completed iteration `{loop,ts,goal,status,duration_s}` |
| `.selfbuild/research/loop-N.md` | Phase 1 output for iteration N |
| `.selfbuild/goals/loop-N.md` | Phase 2-3 output: validated goal statement |
| `.selfbuild/logs/loop-N.log` | Full phase log |
| `.selfbuild/lessons.md` | Ratchet-only lessons file: research appends durable lessons (dated), never deletes or edits existing ones |

The next loop number is `ledger lines + 1`. Crash recovery is implicit: an unfinished
iteration simply reruns under its own number.

**Durable state across workspaces.** `.selfbuild/` is gitignored and Orca spawns each
run in a fresh workspace, so without help every run would restart at loop 1 (this
actually happened on 2026-08-24: runs improvised loop numbers from ambient context).
`scripts/selfbuild-state.sh` mirrors `ledger.jsonl` + `lessons.md` to the orphan branch
`selfbuild/state` on origin:

```sh
scripts/selfbuild-state.sh pull   # merge origin state into local .selfbuild/ before numbering
scripts/selfbuild-state.sh push   # publish after every recorded iteration (failures included)
```

Merge policy: ledger is keyed by loop number, later `ts` wins on collision; lessons are
a ratchet-only union. `selfbuild-loop.sh` calls both automatically; hand-run iterations
(one-shot automation prompts) MUST call `pull` first and `push` after recording.

## Phases

2. **Ideas** — Select ONE work item informed by the newest research and the ledger's recent failures/gaps. Selection is **issue-first (2026-09-07 operator policy)**: the task tracker is the GitHub issue queue (open issues labeled `selfbuild`, ordered `priority:P0` > `priority:P1` > `priority:P2`, oldest first within a tier), never a static PRD backlog section. Only when the tracker is empty does the LLM selection path run, deriving one goal from research + ledger + lessons.
3. **Validate** — Goal must pass three checks: it implements an open tracker
   issue (or, empty-tracker fallback, a research-derived goal that nothing in
   the ledger says shipped); scoped to a single iteration (implementable +
   testable in one pass); no open dependency on an earlier failed loop. Written
   to `.selfbuild/goals/loop-N.md`.
4. **Plan** — `devagent task --prompt "<goal>"` plans via the built-in planner.
5. **Implement** — same `devagent task` invocation drives worker CLIs (claude-code /
   opencode) in isolated worktrees through its internal plan-implement-test loops.
6. **Testing** — `devagent task` gates internally (test-gate, migration rules,
   async-review); after merge-back the driver additionally runs repo-level `npm test`.
   Failure marks the iteration failed and feeds diagnostics into the next Research phase.
7. **Push** — `--auto-pr` pushes the branch and opens a PR. **Policy (locked 2026-08-24):
   product code always ships as a PR, never direct to origin/main**; direct main is
   reserved for docs and `.selfbuild` protocol chores. `SELFBUILD_PUSH_MODE=main`
   remains available but is not the operating default. **PRD-per-PR policy (2026-09-07):
   every PR lands with its `docs/PRD.md` state update** — the sections the change
   affects plus the *Last updated* footer, applied in the same branch via the
   dispatch-prompt policy rider. The shipped iteration closes its tracker issue
   (merge-side auto-close also works when the goal carries an issue reference).

## Running

Single-process infinite runner:

```sh
npm run selfbuild                 # uses defaults below
```

Environment knobs (all optional):

| Var | Default | Meaning |
|---|---|---|
| `SELFBUILD_MAX_ITERATIONS` | `0` | 0 = run until circuit-breaker trips |
| `SELFBUILD_MAX_CONSECUTIVE_FAILURES` | `3` | Circuit breaker: abort after N failed loops in a row |
| `SELFBUILD_WORKER` | `omp` | Worker CLI passed to `devagent task` |
| `SELFBUILD_PUSH_MODE` | `pr` | `pr` (branch + PR via auto-pr) or `main` (direct commit) |
| `SELFBUILD_CLAUDE` | `omp … --model router/dev` | Research/PO headless invocation (research + PO both use pane-run via herdr with this binary as fallback) |
| `SELFBUILD_WORKER` | `omp` | Worker CLI passed to `devagent task` |
| `SELFBUILD_ISSUE_LABEL` | `selfbuild` | Issue label defining the loop's tracker queue |
| `SELFBUILD_ISSUE_MAX` | `50` | Max issues fetched per pick (deterministic sort: priority rank, then issue number) |
| `SELFBUILD_GH_REPO` | derived from `git remote get-url origin` | Target repo for the tracker pick (`gh issue`) |
| `SELFBUILD_DRY_RUN` | `0` | `1` executes all phases without side effects (stub outputs, no claude/task/push) |


## Tracker + PRD policy (2026-09-07 operator decision)

- **The task tracker is GitHub issues, not a document.** The driver claims the
  highest-priority open `selfbuild` issue each iteration (deterministic: priority
  rank, then oldest issue number), builds it, and closes it with evidence. The
  LLM selection path exists only as the empty-tracker fallback; anything it picks
  still ships against the repo directly.
- **The PRD is a state document.** `docs/PRD.md` records what the repo IS:
  status blockquotes, completion notes, architecture, the *Last updated* footer.
  It must reflect the current repo state at all times — enforced three ways:
  1. the loop skips an iteration (operator-degraded) when `docs/PRD.md` is locally dirty;
  2. every dispatch prompt carries the PRD-per-PR policy rider (update affected
     sections + footer in the same PR);
  3. the curator strikes shipped backlog lines and refreshes state claims daily.
- **Priorities:** `priority:P0` (do next) > `priority:P1` > `priority:P2` (backlog).
  Unlabeled selfbuild issues sort as P2. The curator re-prioritizes and files new
  issues (max 3/pass) from recent delivery history; the Q27 re-burn guard still
  skips any goal that already shipped.
- **Curation** (`scripts/prd-curator.sh`) now reconciles the tracker: closes
  shipped issues with evidence, files + re-prioritizes open ones, strikes shipped
  PRD lines, and publishes the PRD state diff as a docs PR. Tracker mutations are
  live on GitHub immediately; only the PRD diff ships as a PR.

## Guardrails (Kitchen Loop lineage)

Research phase 1 of this loop's first iteration (arXiv 2603.25697 "Kitchen Loop",
Ouroboros) surfaced four patterns now encoded here:

1. **Circuit breaker** — abort after `SELFBUILD_MAX_CONSECUTIVE_FAILURES` failed
   iterations within one run (regression-failure gate, default threshold 3).
2. **Starvation gate** — halt when the ledger shows N consecutive non-productive
   iterations across ALL runs; catches slow thrash that the per-run breaker misses.
3. **Lessons ratchet** — durable findings are appended to `lessons.md`, monotonic and
   version-controlled by convention; later iterations consume them instead of
   re-deriving (spec-anchored improvement, not metric-chasing).
4. **Failure feedback** — each iteration's research prompt includes the prior ledger
   tail so defects compound into fixes rather than repeats.

## Orca integration

Two supported modes:

**A. Watchdog automation (recommended).** Orca re-triggers one iteration per schedule;
each run is an isolated worktree session:

```sh
orca automations create \
  --name devagent-selfbuild \
  --trigger '23 */2 * * *' \
  --provider claude \
  --repo name:devagent \
  --workspace-mode new-per-run \
  --base-branch main \
  --prompt "Execute exactly ONE iteration of the DevAgent self-build loop per docs/SELF-BUILD-LOOP.md. FIRST run scripts/selfbuild-state.sh pull, then read .selfbuild/ledger.jsonl for the next loop number (ledger lines + 1), then run phases 1-7 end to end. Pick the highest-priority open GitHub issue labeled selfbuild (priority:P0 > P1 > P2; the tracker is refilled by the prd-curator automation). After recording the iteration outcome in the ledger, run scripts/selfbuild-state.sh push. PUSH CODE AS A PULL REQUEST; never direct to origin/main for product code. Do not start a second iteration." \
  --enabled --json
```

Manage with `orca automations list|show|runs|remove`.

**Tracker curation loop.** The build loop consumes the GitHub issue queue as its
goal backlog, so the tracker must stay current or iterations starve/repeat.
`scripts/prd-curator.sh` runs ONE curation pass: research what shipped (recent merged
PRs, ledger failures, lessons), verify claimed capabilities against the repo, then
reconcile the tracker (close shipped issues with evidence, file + re-prioritize)
and refresh docs/PRD.md as a state document (strike shipped backlog lines, update
section 18). Tracker mutations are live on GitHub immediately; the PRD state diff
ships as a docs PR:

```sh
npm run prdcurate                 # one pass, opens a PR when the PRD changes
SELFBUILD_DRY_RUN=1 npm run prdcurate   # preview only
```

Schedule it alongside the build loop so goals never go stale:

```sh
orca automations create \
  --name devagent-prd-curator \
  --trigger '47 6 * * *' \
  --provider claude \
  --repo name:devagent \
  --prompt "Run bash scripts/prd-curator.sh once. It researches recent delivery history, reconciles the GitHub issue tracker (closes shipped selfbuild issues with evidence, files and re-prioritizes new ones), and updates docs/PRD.md as a state document, then pushes a branch and opens a PR with the PRD diff. Do not merge it yourself unless repo tests are green." \
  --base-branch main \
  --enabled --json
```

The two loops are loosely coupled through the tracker: curator files and
prioritizes issues, build loop consumes them. Neither depends on the other's
schedule.

**Spawned-session auto-cleanup.** Mode A leaves one Orca workspace
(`auto-devagent-selfbuild-run-N-<ts>`) plus a live terminal behind per run, forever.
`scripts/orca-selfbuild-cleanup.sh` reclaims them automatically (macOS):

```sh
scripts/orca-selfbuild-cleanup.sh                        # dry-run: show what would go
scripts/orca-selfbuild-cleanup.sh --apply                # close terminals + delete worktrees
scripts/orca-selfbuild-cleanup.sh --install-launchagent  # schedule hourly via LaunchAgent
```

Deletion is gated on two safety checks per workspace: nothing modified within
`ORCA_MIN_AGE_SECS` (default 3600) and HEAD already merged into origin/main (or pushed
verbatim to origin), so no unpushed work is ever destroyed. Nested
`.devagent-worktrees/TASK` registrations are detached first; `orca worktree rm` refuses
the parent otherwise. The LaunchAgent writes `~/Library/Logs/orca-selfbuild-cleanup.log`.
Other knobs: `ORCA_SELFBUILD_REPO` (repo selector, default `name:devagent`),
`ORCA_MAIN_REPO` (git context for merge checks).

**B. Long-lived terminal.** Run `npm run selfbuild` inside an Orca terminal tab
(`orca terminal create`); the script loops internally. Pair with cc-guard
(`devagent guard-status --resume`) for API-failure recovery.

**C. Factory (scout + Orca workers).** `devagent create --repo . --scout --workers 3` supersedes the single-process loop with a decoupled factory:

- `devagent scout --interval 30` (24/7 LaunchAgent, `opencode` worker) researches `docs/PRD.md §4+§17` + ledger + lessons, writes PRD + task to `.devagent/prds/` + `.devagent/queue/`, heartbeats to `.devagent/scout.heartbeat.json`.
- Multiple `devagent consume --auto-pr [--auto-merge]` workers (each in its own Orca worktree from `orca worktree create`) claim tasks, implement in isolated `.devagent-worktrees/<id>`, validate G1/G3/G4, push `devagent/<id>` and open PR via `gh`, optionally auto-merge and `self-update`.
- See [docs/SCOUT.md](SCOUT.md) and `docs/SCOUT-CREATE-PRD.md` for the full runbook.

## Guardrails (operational)

- Circuit breaker stops the loop after repeated failures instead of thrashing.
- Every iteration starts from an up-to-date main (`git pull --ff-only`; skipped cleanly
  when offline).
- Research phase explicitly reviews the previous iteration's failure mode so defects
  compound into fixes, not repeats.
- No secrets are read or written; workers inherit repo-local env only.
