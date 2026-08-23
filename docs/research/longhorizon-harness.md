# LongHorizon-Harness: research analysis and feature ideas

Source: https://github.com/AMAP-ML/LongHorizon-Harness
Paper: arXiv:2608.01964 ("LongHorizon-Harness: Advancing Long-Horizon Agents for Real-World Tasks", 2026)
Analyzed: 2026-08-24

## What it is

A Python harness ("loop engineering") wrapped around existing agent CLIs
(Claude Code, Codex CLI, OpenCode) that runs them for dozens of hours across
GUI and CLI domains. It does not change models or agents; it adds a durable
outer loop. Headline eval numbers (Qwen 3.7-Plus backbone):

| Benchmark | Baseline -> Harness |
|---|---|
| WeaveBench PassRate | 51.8% -> 80.7% |
| OSWorld 2.0 binary | 2.8% -> 8.3% (~3x) |
| Terminal-Bench 2.1 | 69.7% -> 77.2%, with ~24% FEWER tokens |

## Core idea: long-horizon work is a state-management problem

The paper reframes long-horizon execution as managing **task state outside
execution**. State changes only when grounded in independently verified
environment facts. Agent self-assessment never mutates trusted state.

### The Manage-Execute-Audit (MEA) round

Each round has three role-scoped participants:

- **Manager** (no environment access): sees original goal T, current state S,
  all accumulated audit reports V. Emits next state, a decision
  (`execute` / `done` / `blocked` / `ask`), and a bounded contract c for the
  next executor: immediate goal, acceptance criteria, boundary constraints,
  and references to relevant prior evidence. Overhead is tiny: 2-8% of tokens.
- **Executor**: fresh context each episode. Receives T, S, contract c, and
  only audit reports referenced by c -- never raw prior trajectories.
  Trajectory is discarded after the episode; only its output report o
  survives. Budget-bounded (1800s/round in their setup).
- **Auditor**: fresh context, does NOT see the executor's reasoning. May use
  o to locate files/logs/tests but judges completion independently against
  c's acceptance criteria. Strictly read-only: harness monitors workspace
  mutations; any mutation flags an integrity violation and such a report can
  never support a completed record. This is the main investment: 19-38% of
  tokens -- and the source of most of the gain.

### State representation

Structured record set with three record types:

- requirements (objectives/constraints from T)
- artifacts (outputs created or modified)
- facts (environment info later rounds need)

Each record carries a status (`completed` / `pending` / `blocked` /
`untrusted`) plus references to audit evidence supporting that status.
Audit reports V are the sole cross-round memory.

### Failure recovery

Failures are externalized into state, not retried in-context. A failed
interaction becomes an evidence-gap record; later executors target the gap
directly. Recovery resumes from latest audited state and discards the failed
trajectory. Example: baseline looped 400+ steps on one dialog; LH scored
0.59 -> 0.92 by recording the gap and re-contracting around it.

## Key empirical findings worth internalizing

1. Independent auditing drives most of the benefit, not planning. Manager
   tokens are nearly free; auditor tokens are the investment that pays.
2. Gains concentrate where baselines fail (tasks scoring <=0.04 recovered to
   0.30-0.92). Hard tasks gained more than medium ones (+12.2 vs +4.2).
3. Token cost is not fixed: 2.3x baseline on WeaveBench, but 24% fewer on
   Terminal-Bench. Stronger models need fewer audit-replan rounds
   (Opus 4.7 dropped 16.5M -> 11.1M tokens/task while improving).
4. Limitations: verification cannot supply capability the model lacks;
   a misinterpreted contract leads to "confidently verified wrong answer";
   small regressions appear on already-strong tasks because extra
   verification/repair disturbs strong trajectories.

## Gap analysis vs DevAgent

DevAgent already has the skeleton of MEA:

- Planner decomposes goals into a dependency DAG
  (`src/orchestrator/types.ts`); executors run in isolated worktrees
  (`src/git/worktree.ts`) with fresh context -- equivalent to LH's executor
  role.
- Validation gates exist (`src/validation/runner.ts`: migration static gate,
  test gate, async review).

The differences are in trust semantics and evidence:

| LongHorizon-Harness | DevAgent today | Gap |
|---|---|---|
| `done` only with clean audit evidence attached to the record | `done` set after executor finishes + gates | Status carries no evidence; executor self-report can become trusted state |
| Dedicated read-only auditor with mutation monitoring | Gates are static/deterministic; async-review exists but is not an independent role with integrity checks | No adversarial auditor; no read-only enforcement on reviewers |
| Bounded contract (goal, acceptance criteria, constraints, prior-evidence refs) | `OrchestratorTask.prompt` + optional `expectedOutput` | Contracts are thin; no explicit acceptance-criteria list the auditor can check item-by-item |
| Failure externalized as evidence-gap records driving targeted re-contracts | `failureDetail` string, attempts counter, generic retry | Retries don't learn from the previous failure's evidence |
| Manager decision includes `blocked` and `ask` (human input) states | `blocked` exists; no `ask` path to a human despite having server/webhook infra | Human-in-the-loop escape hatch missing |
| Run ledger `.lh-harness/runs/<id>/` with events, trajectories, audit reports | `runregistry.ts`, board JSON | Less structured; no first-class audit-report artifacts |

## Feature ideas (prioritized)

### F1. Evidence-gated task transitions (highest value, smallest diff)

Extend `TaskStatus` with an `untrusted` intermediate: an executor finishing a
task moves it to `untrusted`; only an auditor verdict (or deterministic gate
suite) flips it to `done`. Store verdict + evidence refs on
`OrchestratorTask`:

```ts
audit?: {
  verdict: 'pass' | 'fail' | 'blocked';
  criteriaResults: { criterion: string; met: boolean; evidence: string }[];
  integrity: 'clean' | 'suspect' | 'violation';
};
```

This ports LH's central rule: executor claims never directly change state.

### F2. Independent auditor role in the orchestrator

Add a third role alongside planner/executor in `ProjectBoard.roles`. The
auditor worker gets read-only tooling (no worktree write access) and receives
the contract plus the executor's summary -- not its transcript. Verdict
requires per-criterion environmental proof (test output, file content, grep),
mirroring LH's evidence standards. Reuse `async-review.ts` machinery but
enforce read-only scope and independence.

### F3. Bounded contracts

Replace freeform `prompt` with a structured contract:
`{ goal, acceptanceCriteria[], boundaryConstraints[], priorEvidenceRefs[] }`.
Acceptance criteria give F2 something itemized to verify and make
"confidently verified wrong answer" less likely (LH names contract
misinterpretation as a core failure mode). `expectedOutput` folds into
acceptance criteria.

### F4. Targeted failure re-contracting

On `failed`, do not blindly increment `attempts`. Instead require the
auditor/manager to produce an evidence-gap record (what was attempted, what
proof is missing) and attach it to the task; the retry contract references it
and constrains the executor to close the gap. This is the mechanism behind
LH's largest recovery wins.

### F5. `ask` status with human-in-the-loop wiring

Add `ask` to `TaskStatus`. When a manager/auditor hits a decision needing
user input or authorization, surface it through the existing server/webhook
surface (`src/server/webhook.ts`) and pause the DAG branch until answered.

### F6. Structured run ledger

Adopt `.devagent/runs/<run-id>/` layout: `events.jsonl` (stream),
`contracts/`, `audit-reports/`, final report. Board stays as the live view;
the ledger becomes the replayable history. Helps debugging the self-build
loop and postmortems.

## What NOT to copy

- Do not add verification weight to tasks where single-shot quality already
  dominates -- LH saw small regressions on strong baseline tasks from extra
  verify/repair churn. Gate auditing on classification or gate failures
  (cheap heuristic: audit only on `untrusted` after gates pass but risk is
  high, or sample-based).
- Do not build GUI/computer-use support; DevAgent's domain is backend repos,
  where LH's CLI evidence (Terminal-Bench +7.5 pts at lower cost) applies
  directly and OSWorld-style overhead does not.

## Bottom line

DevAgent's planner-executor-DAG matches LH's executor layer, but its trust
model is the weak point: completion today is self-reported plus deterministic
gates. LH's data says the highest-leverage change is an independent,
read-only auditor whose evidence-gated verdicts are the only thing that can
flip a task to `done` (F1+F2), with structured contracts (F3) making those
verdicts checkable and targeted re-contracting (F4) making failures compound
instead of repeat.
