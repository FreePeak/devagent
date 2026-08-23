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

The next loop number is `ledger lines + 1`. Crash recovery is implicit: an unfinished
iteration simply reruns under its own number.

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
7. **Push** — `--auto-pr` pushes the branch and opens a PR (or commit-to-main when
   `SELFBUILD_PUSH_MODE=main`, matching historical loop practice).

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

## Orca integration

Two supported modes:

**A. Watchdog automation (recommended).** Orca re-triggers one iteration per schedule;
each run is an isolated worktree session:

```sh
orca automations create \
  --name devagent-selfbuild \
  --trigger '15 */2 * * *' \
  --provider claude \
  --repo name:devagent \
  --workspace-mode new-per-run \
  --base-branch main \
  --prompt "Execute exactly ONE iteration of the DevAgent self-build loop per docs/SELF-BUILD-LOOP.md. Read .selfbuild/ledger.jsonl for the next loop number, then run phases 1-7 end to end. Do not start a second iteration." \
  --enabled --json
```

Manage with `orca automations list|show|runs|remove`.

**B. Long-lived terminal.** Run `npm run selfbuild` inside an Orca terminal tab
(`orca terminal create`); the script loops internally. Pair with cc-guard
(`devagent guard-status --resume`) for API-failure recovery.

## Guardrails

- Circuit breaker stops the loop after repeated failures instead of thrashing.
- Every iteration starts from an up-to-date main (`git pull --ff-only`; skipped cleanly
  when offline).
- Research phase explicitly reviews the previous iteration's failure mode so defects
  compound into fixes, not repeats.
- No secrets are read or written; workers inherit repo-local env only.
