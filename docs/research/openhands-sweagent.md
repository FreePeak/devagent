# Research: OpenHands & SWE-agent — verification and interface lessons for DevAgent

*Researched 2026-08-24 (loop 49). Sources: OpenHands platform paper (arXiv:2407.16741), SWE-agent ACI paper (Yang et al. 2024, NeurIPS) and ablation data.*

## Why these two

OpenHands (ex-OpenDevin) is the largest open-source generalist coding-agent
platform (~2.1k contributors); SWE-agent (Princeton NLP) is the reference for
Agent-Computer Interface (ACI) design on SWE-bench. Both publish failure-mode
data rare in this space, which makes their lessons directly transferable.

## Extracted lessons

### L1 — "Agents succeed quickly and fail slowly" (SWE-agent §B.9)

Resolved runs finish at median $1.21 / 12 steps; unresolved ones average $2.52 /
21 steps. **Escalating budgets does not rescue failures** — 93% of resolved
instances never exhaust budget.

**DevAgent status:** aligned by design — tight `--max-task-retries` (default 1)
plus recovery contracts (loop 43) instead of blind retry escalation.
**Backlog:** add a per-board wall-clock/cost ceiling that stops *dispatching*
new waves once spend exceeds a threshold (`orchestrate --budget <usd>`), since
the same curve implies late-wave work is disproportionately waste.

### L2 — Recovery odds decay after failed edits (SWE-agent §B.3.3)

Any edit succeeds ~90% of the time eventually; after one failed edit the odds
drop to ~57%. Half of all unresolved trajectories are incorrect
implementations, not tooling errors.

**DevAgent mapping:** evidence gaps + audit verdicts already externalize
*what* failed. The scheduler should treat "second consecutive audit fail of
the same criterion" as a contract problem (recovery re-plan), not an execution
problem. **Backlog:** track consecutive unmet-criterion repeats per task;
auto-escalate to `planRecovery` earlier when a criterion fails twice.

### L3 — Guardrails beat freedom at the action layer (SWE-agent edit-lint)

Invalid edits are discarded pre-application with the lint error shown back;
this intervention outperformed unrestricted editing. OpenHands ships the same
idea as AgentSkills + linter-in-the-loop.

**DevAgent status:** G1–G4 gates are post-hoc (after the worker finishes).
**Backlog:** surface gate rules to executors up-front as machine-checkable
constraints in the task contract (planner already emits `constraints`; extend
the executor prompt template with the G3 migration rule list so violations
never get written).

### L4 — Event stream as the backbone (OpenHands §2.1)

Every component (agent, runtime, UI, delegation) communicates through typed
Action/Observation events on one stream; state = event stream + accumulators.
This is why their UI, replay, and eval harnesses stay consistent.

**DevAgent status:** three parallel histories exist today — JSONL run logs,
the board JSON, and the loop-48 ledger.
**Backlog:** converge on the ledger as the single append-only stream
(`kind: 'audit' | 'task-transition' | 'wave'`), with the board derived from it
at load time rather than maintained separately.

### L5 — Condenser: compression is a first-class component (OpenHands)

History processors keep agent context concise: drop verbose search output,
keep first error only ("all past error messages except the first are
omitted").

**DevAgent mapping:** audit prompts embed full criteria results and gap lists;
retry prompts accumulate answers and gap history monotonically.
**Backlog:** cap executor repair-prompt history to first-error-plus-latest
(the SWE-agent rule), and trim audit summaries older than the current attempt.

### L6 — Sandboxed runtimes with explicit risk tiers (OpenHands §2.2)

Local runtime = no isolation (documented loudly); Docker sandbox = default;
remote runtime = scale. Risk tiering is explicit, not implied.

**DevAgent status:** compose-based G2 + worktrees give isolation but the tier
is implicit. **Backlog:** print the effective isolation tier in run output and
`project` view so consumers can judge trust accordingly.

## Priority for next implementation loops

1. L4 convergence (ledger as single stream) — unlocks cheap analytics for
   every other lesson
2. L2 consecutive-failure escalation — small diff in scheduler, big win rate
   impact
3. L1 board-level budget ceiling — operator cost control

## What we deliberately did not adopt

- **Generalist action space** (bash+browser+IPython): DevAgent's narrow
  ticket→PR domain is its differentiator; breadth would dilute the gates.
- **Community agent hub**: multi-planner support adds variance without
  evidence of resolution-rate gains at our task sizes.
