# PRD: DevAgent Dogfood Round 1

**Source:** live testing of DevAgent v0.3.x against its own repository (2026-08-23).
**Method:** executed every CLI command (`config`, `validate`, `dashboard`, `status`, `clean`, `run --dry-run`, `fleet`, `serve`) without credentials, then traced code paths for each failure observed.
**Scope:** four defects found; each becomes one ticket dispatched through DevAgent's own pipeline.

## Context

DevAgent promises "ticket to tested PR." This round makes DevAgent eat its own cooking: the issues below were found by *using* the tool, and are fixed *by* the tool's pipeline (worktree isolation → implement → G1 gate → publish-ready branch).

## Tickets

### DA-DOG-01 — `--dry-run` must work without network credentials

**Found:** `devagent run --ticket X --dry-run` exits 1 with `LINEAR_API_KEY is not set`, despite advertising "plan only; no workers, no remotes."

**Problem:** credential gating happens before the dry-run short-circuit, so the cheapest smoke test of the product requires production secrets.

**Acceptance criteria:**
- [ ] `devagent run --ticket ANY --dry-run` succeeds with no environment credentials
- [ ] Dry-run output prints the plan summary and classification
- [ ] Dry-run never calls fetchTicket/postTicketComment/workers/remotes

### DA-DOG-02 — `fleet` validates arguments in the wrong order

**Found:** `devagent fleet --ticket E-1 --repo badformat` reports `LINEAR_API_KEY is not set.` The malformed `--repo` entry is never surfaced.

**Problem:** argument validation is masked by the credential check; users fix the wrong thing.

**Acceptance criteria:**
- [ ] Malformed `--repo` entries produce `Invalid --repo entry "badformat" (expected name=path)` regardless of credential state
- [ ] Argument validation precedes credential validation
- [ ] Exit code remains 1 on either failure

### DA-DOG-03 — Re-running a ticket loses worktree isolation

**Found:** second `run` for the same ticket throws on branch creation (`devagent/ENG-x` already exists), logs a warning, and executes the worker **in the repo root** — violating FR-IMPL-01's isolation guarantee silently.

**Problem:** no reuse or recovery path for existing run branches/worktrees.

**Acceptance criteria:**
- [ ] When `devagent/<ticket>` branch already exists, createWorktree reuses it (checkout existing worktree if present, else attach new worktree to the existing branch)
- [ ] Worker never falls back to repo-root execution when the repo is a git repository; worktree failure aborts the run with a clear error instead
- [ ] Unit tests cover both reuse paths

### DA-DOG-04 — Manual `run` bypasses the latest-wins lock registry

**Found:** only webhook `dispatchRun` acquires the per-ticket lock; two concurrent `devagent run --ticket X` invocations race on the same worktree/branch (colliding via DA-DOG-03's old behavior).

**Problem:** the dedup invariant "one active pipeline per ticket key across processes" holds only for webhook triggers.

**Acceptance criteria:**
- [ ] `run` acquires `tryAcquireRun` before fetch and releases in `finally`
- [ ] A second concurrent `run` for the same ticket exits non-zero with `Run for <ticket> already active`
- [ ] Stale locks (>1h) are broken, matching registry semantics

## Non-goals

- Remote execution / sandbox hardening (blocked on owner decisions, see PRD roadmap)
- Any change to gate rules G1–G4

## Success metric

All four tickets implemented via DevAgent's own `runPipeline`, G1 green on every resulting branch, full suite ≥ previous count (142).

## Outcome (2026-08-23)

| Ticket | Result | Verified |
|---|---|---|
| DA-DOG-01 | ✅ delivered (`15de52c`) — dry-run offline via `buildDryRunDeps` | live: exit 0, plan printed, no remotes |
| DA-DOG-02 | ✅ delivered (`c5c6236`) — args validated first | live: correct error without creds; CLI test asserts exit 1 |
| DA-DOG-03 | ✅ delivered (`3f7b216`) — branch reuse + abort instead of repo-root fallback | 152→156 suite; reuse paths unit-tested |
| DA-DOG-04 | ✅ delivered (`3998054`) — run acquires latest-wins lock | live: concurrent run rejected, exit 1 |

**Process findings from dogfooding itself** (feed into next round):
- **DA-DOG-05 (harness):** 10-min worker timeout was too small for real tickets; both attempts timed out and the retry restarted from scratch because branch reuse didn't exist yet. Timeout raised to 25 min in the dispatch runner; consider per-stage budgets + resumable attempts.
- **DA-DOG-06 (process):** one worker ignored the ticket and refactored unrelated server code; G1 passed anyway since tests were unaffected. Mitigation shipped: prompt now forbids out-of-scope refactors and restates binding ACs in repair prompts. Still missing: an automated AC-overlap gate on the changed-file set.

Final state: 156 tests / 22 files green, all work merged to `main` and pushed.
