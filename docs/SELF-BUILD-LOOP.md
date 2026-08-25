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

1. **Research** — Headless agent scans competitor landscape (Devin, Copilot coding agent,
   OpenHands, Factory Droid, Jules, Codex) for moves not yet reflected in `docs/PRD.md`
   section 4, plus self-build-loop architecture patterns from other projects. Output:
   findings + ranked recommendation from the Phase 4 backlog.
2. **Ideas** — Select ONE backlog item (PRD section 17 roadmap / Phase 4) informed by the
   newest research and the ledger's recent failures/gaps.
3. **Validate** — Goal must pass three checks: maps to a PRD backlog item; scoped to a
   single iteration (implementable + testable in one pass); no open dependency on an
   earlier failed loop. Written to `.selfbuild/goals/loop-N.md`.
4. **Plan** — `devagent task --prompt "<goal>"` plans via the built-in planner.
5. **Implement** — same `devagent task` invocation drives worker CLIs (claude-code /
   opencode) in isolated worktrees through its internal plan-implement-test loops.
6. **Testing** — `devagent task` gates internally (test-gate, migration rules,
   async-review); after merge-back the driver additionally runs repo-level `npm test`.
   Failure marks the iteration failed and feeds diagnostics into the next Research phase.
7. **Push** — `--auto-pr` pushes the branch and opens a PR. **Policy (locked 2026-08-24):
   product code always ships as a PR, never direct to origin/main**; direct main is
   reserved for docs and `.selfbuild` protocol chores. `SELFBUILD_PUSH_MODE=main`
   remains available but is not the operating default.

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
| `SELFBUILD_WORKER` | `claude-code` | Worker CLI passed to `devagent task` |
| `SELFBUILD_PUSH_MODE` | `pr` | `pr` (branch + PR via auto-pr) or `main` (direct commit) |
| `SELFBUILD_CLAUDE` | `claude -p` | Headless researcher invocation |
| `SELFBUILD_STARVATION_LIMIT` | `5` | Halt when the last N ledger entries are all non-`ok` (cross-run thrash guard) |
| `SELFBUILD_CLEANUP_DELAY_SECS` | `1800` | Grace period before a pr-mode iteration's worktree + branch (`devagent/TASK`) are removed; deletion only happens once the branch tip is verified on origin |
| `SELFBUILD_DRY_RUN` | `0` | `1` executes all phases without side effects (stub outputs, no claude/task/push) |

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
  --prompt "Execute exactly ONE iteration of the DevAgent self-build loop per docs/SELF-BUILD-LOOP.md. FIRST run scripts/selfbuild-state.sh pull, then read .selfbuild/ledger.jsonl for the next loop number (ledger lines + 1), then run phases 1-7 end to end. Pick ONE item from docs/PRD.md section 17 Phase 4 backlog (kept fresh by the prd-curator automation). After recording the iteration outcome in the ledger, run scripts/selfbuild-state.sh push. PUSH CODE AS A PULL REQUEST; never direct to origin/main for product code. Do not start a second iteration." \
  --enabled --json
```

Manage with `orca automations list|show|runs|remove`.

**PRD curation loop.** The build loop consumes `docs/PRD.md` section 17 as its goal
backlog, so that backlog must stay current or iterations starve/repeat.
`scripts/prd-curator.sh` runs ONE curation pass: research what shipped (recent merged
PRs, ledger failures, lessons), verify claimed capabilities against the repo, then
refresh section 17 (mark shipped items, re-rank/add backlog items) and section 18
(retire answered questions). The diff ships as a docs PR:

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
  --workspace-mode new-per-run \
  --base-branch main \
  --prompt "Run bash scripts/prd-curator.sh once. It researches recent delivery history and updates docs/PRD.md sections 17-18, then pushes a branch and opens a PR. Do not merge it yourself unless repo tests are green." \
  --enabled --json
```

The two loops are loosely coupled through the PRD: curator writes goals, build loop
consumes them. Neither depends on the other's schedule.

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
