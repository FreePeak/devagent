# DevAgent — Product Requirements Document

> The Autonomous Backend Delivery Agent for modern engineering teams.
>
> Status: Draft v0.1 · 2026-08-22 · Owner: linh.doan

---

## Table of Contents

1. [Executive Summary](#1-executive-summary)
2. [Problem Statement](#2-problem-statement)
3. [Product Vision and Positioning](#3-product-vision-and-positioning)
4. [Competitive Landscape](#4-competitive-landscape)
5. [Target Users and Personas](#5-target-users-and-personas)
6. [Scope](#6-scope)
7. [Functional Requirements](#7-functional-requirements)
8. [System Architecture](#8-system-architecture)
9. [Worker Adapter Layer](#9-worker-adapter-layer)
10. [Delivery Pipeline](#10-delivery-pipeline)
11. [Validation Gates (Domain Intelligence)](#11-validation-gates-domain-intelligence)
12. [CLI Specification](#12-cli-specification)
13. [Integrations](#13-integrations)
14. [Non-Functional Requirements](#14-non-functional-requirements)
15. [Metrics and Success Criteria](#15-metrics-and-success-criteria)
16. [Risks and Mitigations](#16-risks-and-mitigations)
17. [Roadmap](#17-roadmap)
18. [Open Questions](#18-open-questions)
19. [Research Appendix](#19-research-appendix)
20. [Product Direction Addendum: Grok Bot, xAI Integration, Cross-Platform Control App](#20-product-direction-addendum-grok-bot-xai-integration-cross-platform-control-app)
21. [Product Direction Addendum: Simplicity First (FR-SIMPLE)](#21-product-direction-addendum-simplicity-first-fr-simple)
22. [Addendum: Full Go Migration (FR-GO)](#22-addendum-full-go-migration-fr-go)
23. [Addendum: Driver Validation (FR-VAL)](#23-addendum-driver-validation-fr-val)

---

## 1. Executive Summary

DevAgent is a vertical autonomous agent that converts backend tickets into tested, reviewed Pull Requests with minimal human involvement. A user assigns a ticket to `@devagent` in Linear or Jira; DevAgent parses the spec, plans the work, drafts database migrations and API code using headless coding-agent CLIs (Claude Code, OpenCode) as execution workers, validates the result in sandboxed Docker containers, and opens a Pull Request with auto-generated documentation for frontend teams.

DevAgent is **not** a general-purpose coding tool. It is a delivery pipeline product: the value is in the closed loop (ticket → plan → code → validate → PR) and in backend-specific domain intelligence that general coding agents lack — migration safety validation, foreign-key integrity checks, and async/race-condition review.

## 2. Problem Statement

Backend ticket delivery has three chronic failure modes:

1. **Latency**: small-to-medium backend tickets (new endpoint, schema tweak, queue consumer) wait days in backlog even though they are well-specified.
2. **Correctness risk from automation**: general AI coders break database integrity — destructive migrations, missing foreign-key constraints, lock-timeout-inducing index builds on large tables — and ignore async race conditions because they never execute what they write against real data.
3. **Review burden**: PRs from AI tools often "look right" but are unverified, shifting cost onto human reviewers who must mentally simulate behavior.

DevAgent addresses all three by owning delivery end to end and refusing to submit anything it has not proven green in an isolated environment.

## 3. Product Vision and Positioning

### 3.1 Positioning statement

| Dimension | General coding tool (Cursor, Copilot IDE) | DevAgent |
|---|---|---|
| User role | Human writes code, AI assists | Human assigns ticket, AI delivers PR |
| Input | Editor + human intent | Ticket ID from Linear/Jira/GitHub |
| Scope | Any code, any task | Backend only: migrations + API + tests |
| Output | Suggestions in editor | Green, tested Pull Request |
| Trust model | Human verifies every keystroke | Closed-loop validation before human review |

### 3.2 Key differentiators

1. **Set-and-forget backend ops** — DevAgent behaves as a virtual team member, not an IDE extension.
2. **Specialized domain intelligence** — explicit validation of migration scripts, foreign-key safety, and event-queue/async logic; the failure modes generic agents miss.
3. **Closed-loop testing** — nothing is submitted until it passes inside a sandboxed container against real migrations.
4. **Multi-worker fan-out** — the same ticket can run through multiple coding-agent CLIs in parallel isolated worktrees; DevAgent cross-checks both diffs and delivers the winner.

### 3.3 What DevAgent is not

- Not a model provider — it orchestrates existing agent CLIs.
- Not an IDE plugin or pair-programmer.
- Not autonomous infrastructure ops (no deployments to production).

## 4. Competitive Landscape

### 4.1 Overview

| Product | Trigger | Testing/validation depth | Pricing model |
|---|---|---|---|
| Devin (Cognition) | Slack, Linear/Jira, GitHub, API, web | Repo tests in own VM; no structured verification; retry loops burn credits | $0/$20/$200 per-user + pay-as-you-go ACUs (~$2.25 / 15 active min) |
| GitHub Copilot coding agent | Issue assignment, @copilot comment | Reuses repo CI on ephemeral Actions runners; iterates on CI failures; human review mandatory | Bundled in Copilot Pro/Business/Enterprise seats |
| OpenHands (All Hands AI) | Issue assignment, manual/API | Whatever tests exist in repo/Docker runtime — fully user-supplied | OSS free + API costs; Cloud ~$20/mo + tokens |
| Factory Droid | CLI, Slack, PR comments, tickets, webhooks | Repo tests + audit/compliance controls, no domain checks | Pro ~$20 / Plus ~$100 / Max ~$200 per-user; quota risk below Enterprise |
| Google Jules | Task assignment; plan-approval gate | Plan preview + diff review; tests only if configured in VM | Free 15 tasks/day; paid via Google AI Pro/Ultra tiers |
| OpenAI Codex cloud agent | ChatGPT sidebar, CLI/IDE/SDK, GitHub | Container test runs; no structured gates | Bundled in ChatGPT plans; credit-metered |
| Claude Code GitHub Actions | @claude mention, any GitHub event | Auto-fix on CI failures; validation = user's CI | Anthropic subscription/API usage |

### 4.2 The white space DevAgent occupies

1. **Nobody does execution-based migration validation.** Static migration lint is no longer absent from the field (CodeRabbit runs Squawk on migration globs by default as of 2026-09-11; Ellipsis ships a prompt-only schema reviewer), but every competitor still treats "tests pass" as the correctness ceiling: none offer schema-diff analysis, migration dry-runs against a shadow database, rollback verification, or destructive-change detection. This is the core of DevAgent's differentiation.
2. **Validation is outsourced to the target repo's CI.** Copilot, Codex, Jules, Droid, and Claude Code Actions all inherit whatever the repository already has. Under-tested backends get green-lit unvalidated PRs.
3. **Backend correctness is implicit.** No vendor ships contract-level verification (API behavior vs spec, transactional integrity, concurrency hazards) as a product feature.
4. **Cost opacity punishes exactly this workload.** Devin ACUs and Codex credits punish long-running backend work; DevAgent's fixed-gate design gives predictable per-task cost.
5. **Ticket-driven autonomy exists but is shallow** — Devin and Droid already do ticket→PR, which validates the workflow; their gap is verification depth, not trigger plumbing.

Implication for positioning: compete on *trust per PR* (validation evidence attached), not on raw generation capability.

*(Full profiles with sources: [appendix 19.2](#192-competitive-profiles).)*

## 5. Target Users and Personas

### P1 — Backend Engineer "Mai" (primary)

Mid-level backend developer on a 5–15 person team. Spends a large share of each sprint on well-specified CRUD endpoints, schema adjustments, and queue consumers. Wants those tickets gone without context-switching cost; will review DevAgent PRs like any teammate's, but expects tests and migration notes already in place.

**Success for Mai**: assigns ticket before standup, reviews a green PR after lunch.

### P2 — Tech Lead / EM "Duc" (buyer)

Owns delivery velocity and production stability. Cautious about AI-generated code touching the database. Needs approval gates on migrations, audit logs per run, and hard budget caps before rolling out team-wide.

**Success for Duc**: zero migration-caused incidents attributable to DevAgent; measurable backlog burn-down.

### P3 — Frontend Engineer "Khanh" (beneficiary)

Blocked on API contracts. Benefits from DevAgent's auto-generated PR documentation: endpoint specs, request/response examples, and migration notes attached to every PR.

### P4 — Platform/DevOps "Tuan" (operator)

Installs and operates DevAgent: manages credentials (Linear/GitHub tokens), Docker sandbox capacity, CLI worker versions, and run budgets. Never wants agent credentials with more scope than necessary.

## 6. Scope

### 6.1 In scope (v1)

| Area | Decision |
|---|---|
| Issue tracker | Linear only (Jira, GitHub Issues in v2) |
| Target repositories | Backend services in one language ecosystem chosen at first deployment (Go or TypeScript/Node) |
| Git host | GitHub via `gh` CLI (GitLab in v2) |
| Worker agents | Claude Code, OpenCode, omp CLIs — headless spawn is opt-in via config (FR-IMPL-07); by default each tool opens like a normal interactive session |
| Ticket types | New/modified REST endpoints, schema migrations, queue/event consumers |
| Execution | Per-run git worktree + branch; validation inside Docker Compose |
| Human control | `--interactive` approval gates; full headless `--auto-pr` mode |

### 6.2 Out of scope (v1)

- Frontend/UI code generation
- Deploying or promoting code to production environments
- Non-backend languages (mobile, ML training code)
- Real-time collaboration features, IDE integrations
- Self-hosted model management (workers bring their own auth/subscriptions)

## 7. Functional Requirements

Requirements use IDs `FR-<area>-NN`. Priority: **M** = must-have for v1, **S** = should-have, **C** = could-have.

### 7.1 Ticket ingestion (area: TICKET)

| ID | Requirement | Pri |
|---|---|---|
| FR-TICKET-01 | Fetch a ticket by identifier from Linear via GraphQL API given a configured API credential | M |
| FR-TICKET-02 | Extract structured spec fields from the ticket: title, description, acceptance criteria, labels, linked issues | M |
| FR-TICKET-03 | Post status comments to the ticket at each pipeline stage transition (started, implementing, validating, PR-opened, failed) | M |
| FR-TICKET-04 | React to ticket assignment events (webhook) to trigger runs without manual CLI invocation | S |
| FR-TICKET-05 | Detect insufficient-specification tickets and refuse with a clarifying question posted back to the ticket | M |

### 7.2 Planning (area: PLAN)

| ID | Requirement | Pri |
|---|---|---|
| FR-PLAN-01 | Produce a written implementation plan (task list, files to touch, migration outline) from the ticket spec before any code is written | M |
| FR-PLAN-02 | Attach the plan to the ticket so humans can correct it before implementation begins in interactive mode | M |
| FR-PLAN-03 | Classify the ticket: endpoint-only / migration-required / consumer-only; route to the appropriate validation gate set | M |

### 7.3 Implementation (area: IMPL)

| ID | Requirement | Pri |
|---|---|---|
| FR-IMPL-01 | Execute implementation by spawning a worker agent inside an isolated git worktree and dedicated branch (`devagent/<ticket-id>`); headless spawn is config-controlled per FR-IMPL-07 | M |
| FR-IMPL-02 | Support two interchangeable workers: Claude Code (`claude -p`) and OpenCode (`opencode run`); selection via flag or config | M |
| FR-IMPL-03 | Fan-out mode: run the same plan through both workers in parallel worktrees and select one diff (or merged diff) after validation scoring | S |
| FR-IMPL-04 | Feed failing test output back to the worker and retry, up to a configurable maximum loop count (default 3) | M |
| FR-IMPL-05 | Enforce per-run budget: maximum wall-clock time and maximum worker steps/tokens; abort cleanly on breach | M |
| FR-IMPL-06 | Fall back from one worker to the other when the primary errors or times out mid-run | S |
| FR-IMPL-07 | Make headless worker execution opt-in: workers (omp, Claude Code, OpenCode, and future adapters) run headlessly only when config turns it on (`spawn.visibility: "headless"`, §20.8 FR-VIS-04); the default opens each coding tool like a normal interactive session the operator can watch and steer | M |

### 7.4 Validation gates (area: VALID)

| ID | Requirement | Pri |
|---|---|---|
| FR-VALID-01 | Bring up the target service's Docker Compose stack (or equivalent sandbox definition) inside the run's workspace and execute the test suite; capture structured results | M |
| FR-VALID-02 | Apply migrations against a throwaway database snapshot seeded with representative data; verify up-migration success, down-migration success, and application boot against the migrated schema | M |
| FR-VALID-03 | Static migration analysis: detect destructive operations (DROP COLUMN/TABLE, type narrowing), non-concurrent index creation, missing FK indexes, and lock-risk patterns; block PR on critical findings | M |
| FR-VALID-04 | Data-loss heuristic: compare row counts/table shapes before and after migration on the snapshot; flag any shrinkage | S |
| FR-VALID-05 | Async/race-condition review pass: a dedicated audit of the diff targeting concurrency hazards (unawaited promises, shared mutable state, queue handler idempotency); findings attached to PR as review comments | S |
| FR-VALID-06 | Refuse to open a PR while any critical validation gate fails; post the failure report to the ticket instead | M |

### 7.5 Delivery (area: DELIVER)

| ID | Requirement | Pri |
|---|---|---|
| FR-DELIVER-01 | Open a Pull Request containing: summary, test evidence (suite output digest), migration notes, and API contract documentation generated from the diff | M |
| FR-DELIVER-02 | Generate frontend-facing API docs (endpoint list, request/response schema examples) from changed route/handler code and attach to the PR description | S |
| FR-DELIVER-03 | Link PR back to the originating ticket and close the loop (Linear state transition) when PR merges | S |
| FR-DELIVER-04 | In interactive mode, pause before migration application and before PR creation for explicit human approval | M |

### 7.6 Operations (area: OPS)

| ID | Requirement | Pri |
|---|---|---|
| FR-OPS-01 | Write a structured run log per execution (stage timeline, worker commands, outputs, decisions) queryable for debugging | M |
| FR-OPS-02 | All credentials supplied exclusively via environment variables; never stored in config files or logs | M |
| FR-OPS-03 | Pin worker CLI versions in configuration; warn when installed versions drift | S |
| FR-OPS-04 | Dry-run mode that executes planning and prints what would happen without spawning workers or touching remotes | M |

### 7.7 Context (area: CTX)

Optional, budget-bounded structural context assembled at the same `COMPACT_CONTEXT_MARKER` as the lessons and childTrails digests so G0/G5 read identical content. Two layers: (1) an always-on **local markdown baseline** (`.devagent/context/*.md`, commitable, zero dependencies — no server, no index); (2) an opt-in **LeanKG** layer ([github.com/FreePeak/LeanKG](https://github.com/FreePeak/LeanKG), Postgres-backed knowledge-graph MCP) that adds indexed structural context when a server is attached, `off` otherwise.

| ID | Requirement | Pri |
|---|---|---|
| FR-CTX-01 | Inject a context digest into the scout, planner, and repair prompts at `COMPACT_CONTEXT_MARKER`, ratchet-capped at the same character budget as `lessonsMaxChars` (default 4000); oldest entries dropped whole, never split. Content is layered: baseline markdown sections plus a KG digest when the KG layer is on | M |
| FR-CTX-02 | The local markdown baseline is always available and requires no configuration: every file under `.devagent/context/` feeds the digest subject to the shared budget; absence of a KG backend degrades nothing | M |
| FR-CTX-03 | The KG layer is opt-in via config (`devagent.context.kg: "leankg" \| "off"`, default `off`); when `off` or the provider is unreachable, the digest is baseline-only and the pipeline continues — never blocked | M |
| FR-CTX-04 | Never pass the KG client to a worker adapter; the context digest is an orchestrator-side concern only, preserving the "any worker, same contract" invariant from section 9 | M |
| FR-CTX-05 | Wrap every KG client call in a 1s wall-clock budget; on timeout or absence, omit the KG layer and surface the degraded mode in the run log. Do not build a query-fallback ladder inside DevAgent: LeanKG's single-tool router degrades internally (L3 vectors → L2 fuzzy → L1 exact → L0 cold) and stamps every response with `retrieval: {rung, reason}` + `freshness: fresh \| possibly_stale \| cold` — consume that provenance verbatim in the digest and run log | S |

## 8. System Architecture

DevAgent is a thin orchestration layer over existing agent CLIs and infrastructure. It calls no LLM API directly; all code generation happens inside worker agent processes.

```mermaid
flowchart TB
    subgraph Sources
        L[Linear ticket / webhook]
    end

    subgraph DevAgent["DevAgent orchestrator (this product)"]
        T[Ticket adapter]
        P[Planner]
        O[Run orchestrator<br/>budgets, retries, gates]
        W[Worker adapter layer]
        V[Validation engine]
        G[Git worktree manager]
        PR[PR publisher]
        LOG[(Structured run logs)]
    end

    subgraph Workers["Headless coding-agent CLIs"]
        CC[Claude Code - claude -p]
        OC[OpenCode - opencode run]
    end

    subgraph Sandbox["Per-run sandbox"]
        WT1[worktree A + branch]
        WT2[worktree B + branch]
        DK[Docker Compose stack<br/>app + throwaway DB snapshot]
    end

    GH[GitHub - gh CLI]

    L --> T --> P --> O
    O --> W
    W --> CC --> WT1
    W --> OC --> WT2
    G --> WT1 & WT2
    WT1 & WT2 --> V
    DK --> V
    V --> O
    O --> PR --> GH
    PR --> L
    O --> LOG
```

### 8.1 Component responsibilities

| Component | Responsibility |
|---|---|
| Ticket adapter | Fetch/parse tickets, post comments, subscribe to assignment webhooks |
| Planner | Convert ticket spec into an implementation plan and route classification |
| Run orchestrator | Owns the pipeline state machine, budgets, retry loops, approval gates |
| Worker adapter | Uniform spawn/collect interface over heterogeneous agent CLIs (section 9) |
| Validation engine | Test execution, migration safety, data-loss and async audits (section 11) |
| Worktree manager | Create/clean isolated worktrees and branches per run or per fan-out leg |
| PR publisher | Assemble PR body (summary, test evidence, docs), open PR, link ticket |
| Run logger | Append-only structured JSONL log per run for postmortems |

### 8.2 Key design rules

1. **No direct LLM calls** — model/vendor changes never touch DevAgent code; only worker adapters.
2. **Every run is disposable** — workspace state lives in a worktree plus Docker volumes, both torn down after the run.
3. **Gates are separate from workers** — validation runs on the diff/worktree regardless of which worker produced it, enabling cross-worker comparison on equal footing.

## 9. Worker Adapter Layer

The single abstraction that makes DevAgent worker-agnostic:

```typescript
interface WorkerAdapter {
  readonly name: 'claude-code' | 'opencode';
  // Spawn a headless run inside cwd (the run's worktree).
  spawn(opts: {
    prompt: string;          // task instructions incl. plan and constraints
    cwd: string;
    timeoutMs: number;
    maxSteps?: number;
  }): Promise<WorkerResult>;
}

interface WorkerResult {
  exitCode: number;
  diffSummary: { filesChanged: number; insertions: number; deletions: number };
  events: WorkerEvent[];     // parsed structured output for logging/replay
}
```

| Concern | Claude Code | OpenCode |
|---|---|---|
| Headless invocation | `claude -p "<prompt>"` | `opencode run "<prompt>"` |
| Structured output | `--output-format json` / `stream-json` | configured output format |
| Permission handling | `--permission-mode` (non-interactive acceptance of edits) | permission/approval config |
| Session continuity | `--resume` / `--continue` for follow-up turns | session resume support |


Spawn mode follows config, not the adapter: headless invocation is opt-in (`spawn.visibility: "headless"` per FR-IMPL-07 / FR-VIS-04); by default a worker opens like a normal interactive session. The table above describes the headless surface each adapter exposes when that mode is enabled.

Exact flag surfaces are version-dependent and must be pinned via configuration (FR-OPS-03); adapters translate the stable `WorkerAdapter` contract to whatever the installed CLI supports.

## 10. Delivery Pipeline

State machine per ticket:

```mermaid
stateDiagram-v2
    [*] --> Fetch: run --ticket LINEAR-204
    Fetch --> Plan: spec parsed
    Plan --> SpecCheck
    SpecCheck --> Clarify: insufficient spec
    Clarify --> [*]: question posted to ticket
    SpecCheck --> Implement: plan approved (interactive)<br/>or auto-continue (headless)
    Implement --> Validate: worker returns diff
    Validate --> Implement: tests failed, loops < max (retry with failure output)
    Validate --> MigrationGate: tests green
    MigrationGate --> ReviewPass: migration safe
    MigrationGate --> Failed: critical finding
    ReviewPass --> Approval: interactive mode
    Approval --> Publish: human approves
    ReviewPass --> Publish: headless mode
    Publish --> [*]: PR opened, ticket updated
    Failed --> [*]: report posted to ticket
```

Stage transitions post progress comments to the ticket (FR-TICKET-03). Every transition is appended to the run log (FR-OPS-01).

Fan-out variant (FR-IMPL-03): `Implement` fans into two parallel legs (one per worker); `Validate` scores each leg (tests passed, gate findings count, diff size) and selects the winning leg before `MigrationGate`.

## 11. Validation Gates (Domain Intelligence)

This is where DevAgent differs from generic coding agents. Gates run identically regardless of which worker produced the diff.

### Gate G1 — Test execution

- Bring up the repository's sandbox definition (Docker Compose by convention: `docker-compose.devagent.yml` or repo default).
- Run the configured test suite; capture pass/fail counts, failing test names, and output excerpts.
- On failure: feed structured failure output back to the worker (FR-IMPL-04), max N loops.

### Gate G2 — Migration application

Applies only to migration-classified tickets (FR-PLAN-03):

1. Snapshot the sandbox database seeded with representative fixture data.
2. Apply up-migration; assert success and boot of the service against the migrated schema.
3. Apply down-migration; assert schema returns to baseline.
4. Compare table/row shapes before vs after; flag shrinkage as data loss (FR-VALID-04).

### Gate G3 — Static migration analysis

Linter-style rules over migration files; critical severity blocks PR:

| Rule | Severity | Rationale |
|---|---|---|
| DROP TABLE / DROP COLUMN present | critical | data destruction |
| Column type narrowing (int→smallint, text→varchar(n) smaller) | critical | potential truncation/failure |
| CREATE INDEX without CONCURRENTLY (Postgres) | high | lock outage on large tables |
| Added FK without supporting index | high | full-scan on deletes/joins |
| NOT NULL on existing column without default/backfill | high | fails on populated tables |
| Missing down-migration | medium | unrecoverable deploys |

The rule corpus is public and codifiable: Squawk rules, strong_migrations checks, and Atlas analyzers (destructive / data_depend / incompatible codes) together form a near-complete dangerous-pattern taxonomy. DevAgent integrates these tools where they fit (Squawk for Postgres SQL, `atlas migrate lint` for replay-based analysis) rather than reinventing them; the G3 table above is the vendor-neutral rule contract.

### Gate G4 — Async/race-condition review

A dedicated audit pass (separate worker prompt, not the implementer) over the diff targeting concurrency hazards. Research consensus (ConSynergy 2025, RaceBench 2026): pure-LLM interleaving reasoning is weak at fine-grained variable tracking; best results come from hybrid pipelines where an LLM proposes suspicious sites and static/dynamic tools confirm. Therefore G4 is layered:

1. **Deterministic lints first** (cheap, zero false-positive classes): `no-floating-promises` / `require-atomic-updates` (TypeScript), `go test -race` under the test workload (Go, happens-before detection, zero false positives but coverage-bound).
2. **LLM hypothesis pass**: unawaited promises/fire-and-forget calls, shared mutable state across requests, queue-handler idempotency, transaction boundaries spanning I/O.
3. Findings posted as PR review comments (advisory in v1); LLM-only findings are labeled as such so reviewers can weight them.

### Gate G0 — Issue readiness (pre-dispatch)

Type-specific ready-for-dev scoring of the incoming ticket BEFORE any worker
dispatch, so credits are not burned on under-specified work. Implemented in
`src/validation/readiness-gate.ts` (pure length/regex rubric — no LLM, no
network, never throws on missing fields) and wired into the pipeline as the
optional `runGateG0` dep (default provided by `buildDeps`).

- The ticket is classified first (FR-PLAN-03), then scored against common
  criteria (substantive title, description, machine-checkable acceptance
  criteria — 65 points) plus two class-specific signals (35 points):
  endpoint surface + verification for `endpoint-only`; schema entities +
  down-migration/rollback expectation for `migration-required`; transport +
  delivery semantics for `consumer-only`.
- Score >= 60 (threshold `G0_READINESS_THRESHOLD`) dispatches; anything less
  rejects with a `Finding` per unmet criterion. Rejection surfaces as a
  `failed` pipeline outcome (`G0 readiness gate rejected: ...`) with the
  findings detail posted to the tracker when credentials allow (FR-TICKET-03
  path); a failed comment post never masks the gate verdict.
- Unknown classifications skip honestly (consistent with G2/G3 skip
  semantics); dry-run plan-only runs surface G0 like `checkSpec`, and
  hand-built deps without the gate keep the pre-G0 behavior.

### G5: STRIDE merge gate

Static STRIDE-category review over the worker's branch diff, run in the
consume/autoMerge path between the G4 validate stage and `autoMergePr`. The
rubric lives in `src/validation/stride-gate.ts` (regex rules over parsed
unified-diff hunks, no LLM, no network); `src/gates/stride.ts` adapts it to
the gate-executor contract (`evaluateStride({ diff, contextDigest })`).

Behavior:

- Findings carry a STRIDE category (Spoofing, Tampering, Repudiation,
  InformationDisclosure, DenialOfService, ElevationOfPrivilege) and a
  severity of HIGH, CRITICAL, MEDIUM, or LOW.
- CRITICAL promotion: any HIGH finding whose evidence contains a committed
  credential literal (`api_key|secret|password|token` followed by a quoted
  8+ character value) is promoted to CRITICAL.
- Merge policy: HIGH or CRITICAL findings block `autoMergePr` — the task
  completes with `merged: false` and a `stride gate blocked merge` detail;
  MEDIUM and LOW are advisory.
- Per-path allowlist (Q25): the PR may commit
  `.devagent/stride-allowlist.json` (`{"paths": [...glob patterns...]}`, glob
  semantics: `**` crosses path segments, `*` stays within one, a bare name
  matches any basename). `consume` reads the file from the PR branch itself
  (`git show <branch>:.devagent/stride-allowlist.json`) so the suppression is
  reviewable in the diff, then `evaluateStride` drops findings whose file
  matches an allowed path before severity is computed. An absent, unreadable,
  or malformed allowlist fails closed (no suppression).
- A missing, failed, or empty diff is treated as an empty diff (gate passes).
- Every run logs a JSONL entry (DEVAGENT_HOME/runs/<runId>.jsonl) with
  `data.gate === 'stride'`, plus `severityMax` and a compact findings list.
- `contextDigest` is provenance-only and passed through verbatim, treated as
  opaque (Q12).

## 12. CLI Specification

```bash
# Headless: ticket to PR without human touchpoints
devagent run --ticket LINEAR-204 --repo ./backend-service --auto-pr

# Interactive: pause at plan approval, migration gate, and pre-publish
devagent run --ticket JIRA-8821 --interactive

# Worker selection
devagent run --ticket LINEAR-204 --worker claude-code   # or opencode | both
```

### Commands

| Command | Purpose |
|---|---|
| `devagent run` | Execute the full pipeline for one ticket |
| `devagent plan --ticket <id>` | Produce and print/post the implementation plan only |
| `devagent validate --worktree <path>` | Run all applicable gates against an existing worktree |
| `devagent status [--run <id>]` | Show recent runs and stage states |
| `devagent log --run <id>` | Print structured run log |
| `devagent config` | Show effective configuration (workers, budgets, credentials presence) |

### Flags (`run`)

| Flag | Default | Meaning |
| `--auto-pr` | off | Skip approval gates (headless pipeline mode); does not change how worker tools themselves are spawned (that is `spawn.visibility`, FR-IMPL-07) |
| `--ticket <id>` | required | Tracker ticket identifier |
| `--repo <path>` | `.` | Target repository |
| `--worker <name>` | config | `claude-code`, `opencode`, or `both` (fan-out) |
| `--auto-pr` | off | Skip approval gates (headless mode) |
| `--interactive` | on when TTY | Pause at human gates |
| `--headless` | config (`spawn.visibility`) | Force headless worker spawn for this run; `--visible` forces interactive spawn — default follows config and opens workers like normal interactive sessions (FR-IMPL-07) |
| `--max-loops <n>` | 3 | Test-failure retry budget |
| `--timeout <dur>` | 30m | Wall-clock cap per run |
| `--dry-run` | off | Plan only; no workers, no remotes |

### Environment variables (credentials only — FR-OPS-02)

`LINEAR_API_KEY`, `GITHUB_TOKEN` (scoped to contents:write + pull-requests:write
on target repos), `DEVAGENT_HOME` (run state/logs). `GITHUB_TOKEN` env wins; the
`gh auth token` keyring fallback (issue #234) resolves with a 5s timeout and
tests stub the resolver seam rather than exec gh (#262).

## 13. Integrations

### 13.1 Linear (v1 tracker)

- **API**: GraphQL at `https://api.linear.app/graphql`; SDK `@linear/sdk` for TypeScript.
- **Auth**: OAuth2 application with granular scopes over personal API keys. The "assign to @devagent" pattern uses Linear's first-class **Agent** support: OAuth scopes `app:assignable` (issue delegation) and `app:mentionable` (@mentions). Assignment sets the agent as **delegate** — humans keep ownership; DevAgent acts on their behalf.
- **Trigger path**: assigning or mentioning creates an **AgentSession**; a `created` `AgentSessionEvent` webhook is the run trigger. Progress reports back via **AgentActivities** (`thought`, `action`, `elicitation` for clarifying questions, `response` for final) which render as thread comments.
- **Webhook handling**: verify HMAC-SHA256 `Linear-Signature`; dedup on `Linear-Delivery` UUID; respond within 5 s and process asynchronously (retries at 1 m / 1 h / 6 h; persistent failure disables the webhook).
- **Rate limits**: leaky-bucket; API key = 2,500 req/user/hr, OAuth app = 5,000. GraphQL complexity budget applies — keep paginated child counts small. Never poll; use webhooks and filters.

### 13.2 Jira Cloud (v2)

- REST v3, auth via scoped API token + Basic auth (dedicated service account) or OAuth 2.0 (3LO) with `read:jira-work` / `write:jira-work`.
- Run trigger: dynamic webhook with JQL filter; assignment detection from `changelog` items where `field == assignee`.
- Honor HTTP 429 + `Retry-After` (points-based shared quota); watch the CAPTCHA lockout trap on repeated failed basic-auth logins.

### 13.3 GitHub (v1 git host)

- **Auth model**: GitHub App over fine-grained PAT — distinct `app-name[bot]` identity gives clean audit attribution; installation tokens are short-lived (~1 h) minted per run.
- **Least-privilege permissions**: Contents read+write, Pull requests read+write, Issues read+write (commenting); nothing else.
- **Mechanics**: branch push via git HTTPS (`x-access-token:<token>`), `gh pr create` headless with `GH_TOKEN`, arbitrary calls via `gh api`.
- **Branch protection**: do not grant bypass by default — DevAgent PRs go through normal review; bypass lists (rulesets) may include the App only if auto-merge is later desired. Signed-commit rules require signed bot commits where enabled.
- **Rate limits**: 5,000 req/hr (App); secondary limit ≤100 concurrent requests; handle 403 with `Retry-After`.

### 13.4 GitLab (v2)

Project access tokens (bot user, 365-day cap, rotation endpoint) with scopes `api`, `read_repository`, `write_repository`; Developer role suffices for branch + MR creation; bots cannot self-approve MRs (aligns with human-review gate).

### 13.5 Cross-cutting integration requirements

1. Webhook idempotency via provider delivery IDs on all trackers.
2. Respond-fast/process-late on every webhook endpoint.
3. Exponential backoff with jitter honoring provider-specific retry semantics.
4. Short-lived credentials preferred everywhere; static keys only where unavoidable (Jira token).
5. Ticket fields are untrusted input (prompt-injection defense, R5).

## 14. Non-Functional Requirements

| ID | Requirement |
|---|---|
| NFR-01 | A run never exceeds its wall-clock budget; all subprocesses are killed and the worktree torn down on breach |
| NFR-02 | Worker credentials are never logged; run logs redact environment values |
| NFR-03 | Concurrent runs are isolated: separate worktrees, branches, Docker projects, no shared mutable state |
| NFR-04 | All run state is reproducible from the structured log alone (postmortems need no live access) |
| NFR-05 | DevAgent runs on macOS and Linux with Docker as the only heavyweight dependency. Phase 5 (§20.4) widens to Windows: the Node CLI/daemon and the Tauri control app run natively, Docker Desktop substitutes for Docker, and 24/7 loop automation moves from LaunchAgents to Task Scheduler |
| NFR-06 | Adding a third worker CLI requires only a new `WorkerAdapter` implementation — no core changes |

## 15. Metrics and Success Criteria

| Metric | Definition | v1 target |
|---|---|---|
| Ticket-to-PR rate | % of accepted tickets that produce an opened PR without human code edits | ≥ 60% on curated ticket set |
| PR acceptance | % of DevAgent PRs merged within 7 days without major rework | ≥ 70% |
| Migration incident rate | Production incidents attributable to DevAgent migrations | 0 |
| Loop closure | Median wall-clock ticket → PR-opened | < 45 min |
| Retry efficiency | % of runs succeeding within the retry loop vs. escalating to fan-out | track, no target v1 |

**v1 exit criterion (the spine works):** one real "add a GET /health endpoint" ticket flows end-to-end automatically — fetch → plan → implement (both workers selectable) → tests green in Docker → gates pass → PR opened with evidence.

## 16. Risks and Mitigations

| # | Risk | Mitigation |
|---|---|---|
| R1 | Headless CLI flags change between worker releases | Pin versions in config (FR-OPS-03); adapter contract isolates breakage; CI smoke test per pinned version |
| R2 | Agent produces plausible-but-wrong migration that passes naive checks | Gate G2 requires up+down+boot against seeded snapshot; G3 static rules; human approval gate in interactive mode |
| R3 | Runaway cost/time in retry loops | Hard budgets (FR-IMPL-05); escalate to fan-out only per policy, never unbounded |
| R4 | Ticket specs too vague for autonomous work | Spec-check refusal with clarifying question posted to ticket (FR-TICKET-05) |
| R5 | Prompt injection via ticket content (attacker files a ticket instructing the agent) | Treat ticket fields as untrusted data; workers receive sanitized plan, not raw instructions; no credential-bearing commands in prompts |
| R6 | Sandbox escape via worker tool use | Workers confined to worktree cwd; Docker network isolation; no host Docker socket exposure to worker processes |
| R7 | Dependency on fast-moving external projects (Orca, dsh) if reused | Reuse interface designs, not binaries; keep adapters thin so any piece can be replaced |
| R8 | Go port regression in the loop long tail (model-id predicates, watchdog semantics, NDJSON parsing edge cases) | Port golden fixtures first; soak gate compares ledger rows between implementations before any cutover (FR-GO-15); Node stayed the fallback until FR-GO-16 (resolved 2026-09-08 — the soak caught the publish/close long-tail #238, which is exactly what the gate exists for) |
| R9 | No official Go SDK for Linear/Jira | Thin GraphQL/REST clients against the documented APIs; contract tests recorded from the Node implementations before they are removed |
| R10 | Migration stalls mid-way, leaving two half-maintained implementations | Single plan of record: issue #207 master tracker; every Phase G1 issue is independently mergeable; FR-GO-16 is the only deletion step and is gated on the soak gate |

## 17. Roadmap

> **Status (2026-08):** Phases 0–3 are implemented and CI-protected on `main`
> (v0.3.0): spine, G1–G4 gates, worktree isolation, retry-with-evidence,
> fan-out, webhook server + latest-wins registry, fleet mode, rate-limit
> handling, JSONL logs + HTML dashboard, 127 tests green incl. E2E over real
> git fixtures.

### Phase 0 — Foundations ✅

Repo skeleton, config loading, credential handling, run logger.

### Phase 1 — The spine ✅

Worker dispatch (Claude Code), Linear fetch, worktree + branch, test loop with repair-prompt retries, PR open with evidence.

### Phase 2 — Differentiators ✅

Gates G2/G3/G4, OpenCode adapter + fan-out mode, migration safety rules.

> **Completed post-v0.3 (2026-08-24):** merge-assist for fan-out winners;
> second language ecosystem via declarative `testCommand` override +
> Python/pyproject convention detection (PR #15).

### Phase 3 — Hardening ✅

Webhook-triggered runs with HMAC verification and dedup, run dashboard/status commands, end-to-end fixture tests, Linear + GitHub rate-limit resilience.

> **Completed post-v0.3 (2026-08-24):** Jira adapter, GitLab publisher,
> GitHub Issues ingestion end-to-end (PR #18); orchestration run ledger +
> outcome analytics (`devagent_ledger` MCP tool), repeat-gap escalation to
> recovery contracts, wave budget ceiling (`--max-waves`), auto review +
> auto merge (`autoMerge`, PR #17).
>
> **Completed post-v0.3 (2026-08-24, curation run 2):** autoMerge CI-status
> gate — merges now require a green check rollup (`evaluateChecks` blocks on
> failures, waits out pending runs before judging), so Q7 below is resolved
> as yes.

### Phase 4 — Expansion (post-v1)

- Remote execution — dispatch pipelines to a shared host so worker capacity is pooled across repos instead of per-workspace.
  > **Completed post-v0.3 (2026-08-25):** `devagent task --remote` delegates the pipeline
  > to a shared host over SSH (preflight probe, bounded timeout, PR URL extraction, PR #31).
  > **Concurrency-safe identity (2026-08-25):** task ids are per-dispatch — `--id` /
  > `$DEVAGENT_TASK_ID`, defaulting to a collision-free `TASK-<suffix>` — and forward through
  > remote dispatch, so concurrent runs no longer collide on the hardcoded
  > `.devagent-worktrees/TASK` worktree + `devagent/TASK` branch.
- Deeper sandbox isolation — network and filesystem allowlists for workers before any untrusted-repo run.
  > **Completed post-v0.3 (2026-08-24):** credential-shaped env vars are stripped from
  > all agent-CLI worker spawns by default (`src/workers/sandbox.ts`, override via
  > `DEVAGENT_WORKER_ENV_ALLOWLIST`); opt-in macOS seatbelt confinement
  > (`DEVAGENT_SANDBOX=seatbelt`) denies worker writes outside the worktree and temp
  > dirs, with network left default-allow as a named policy knob for future tightening.
  > Git/docker/gh/test-runner spawns keep the full parent env — they need credentials.
- Lessons feedback loop — `selfbuild-state.sh` mirrors `lessons.md` to the state branch (PR #19) but nothing reads it back; inject curated lessons into worker repair and planning prompts so past failures stop repeating.
- Flaky-test guard for fan-out judging — winner selection assumes deterministic tests; add quarantine/rerun handling for nondeterministic suites.
  > **Completed post-v0.3 (2026-08-28):** one flaky rerun before condemning a
  > candidate, and a clean pass outranks a flaky rescue in winner ranking
  > (`src/workers/fanout.ts:74-95`); closes Q8 below.
- ~~Failure-cluster reporting on ledger analytics — recurring gap categories in the ledger should surface as actionable periodic reports, not just queryable rows.~~
  > **Completed post-v0.3 (2026-09-07):** both halves of the cluster view
  > ship. Criteria half — `clusterFailures` ranks recurring unmet acceptance
  > criteria across failed audits, surfaced by `devagent ledger --clusters`.
  > failureClass half — `clusterFailureClasses` (`src/orchestrator/ledger.ts`)
  > groups `taskInterrupt` event rows (which `readLedger`'s audit filter never
  > returned, so `clusterFailures` was criteria-only by construction) by
  > executor failure class — occurrences, distinct tasks, first-seen gate
  > excerpt — printed as a second section under `--clusters` (text and
  > `--json`); `scripts/selfbuild-loop.sh` captures the report each iteration
  > and echoes it into the research/PO prompts, so recurring executor deaths
  > steer goal selection instead of staying queryable rows.
- Knowledge-grounded context — two-layer digest at the existing `COMPACT_CONTEXT_MARKER`: an always-on local markdown baseline (`.devagent/context/*.md`, zero dependencies) plus opt-in structural memory from the local `leankg` MCP, consumed as the degradation boundary (LeanKG's single-tool ladder degrades internally and stamps `retrieval`/`freshness` provenance; no hand-rolled fallback in DevAgent), ratchet-capped at the same 4000-char budget as `lessonsMaxChars`; routes to the freepeak `leankg` server, never `be-knowledge-graph` from a freepeak path. See `docs/research/2026-08-30-devagent-leankg-value-in-harness-era.md` and FR-CTX-01..05.

> **Completed post-v0.3 (2026-08-25 → 2026-08-28):** Lessons feedback
> loop read by workers — 40-line cap + 4000-char `lessonsMaxChars`
> budget across all dispatch paths (PR #39, closes Q9). Three-role
> self-build factory — scout + `track` heartbeat + builder as separate
> LaunchAgents (PR #40). Herdr runtime — reattachable panes, queue
> bridge, resilience, Makefile LaunchAgent control, clean-env scrub
> (PRs #41–#47). Infinity cycling — boards archive + stale-worktree
> prune (PR #42). Reaper scoped to `devagent` headless workers, closing
> the 2026-08-26 mass-kill incident (PRs #48, #52). Scout now forwards
> `config.model` (PR #53).
>
> **Completed post-v0.3 (2026-08-28):** `fanout/ingestChildTrails` plumbing
> shipped (PRs #56 + #57) — `buildChildTrailsDigest` ratchet at 4000 chars
> splices the prior-loop child `worklog.jsonl` excerpts into the next-loop
> planner prompt at `COMPACT_CONTEXT_MARKER`; G0 and G5:STRIDE read the same
> digest at the same assembly point (`src/prompt.ts:303-317`), so the
> industry-baseline plumbing lights up automatically for every subsequent
> gate. Closes the iter 50/52/53/54 convergence lesson, closes Q10 as
> "fixed-size ratchet-capped excerpt."

> **Completed post-v0.3 (2026-08-28):** Scout output-shape regression —
> `extractScoutPayload` accepts array / object / NDJSON / no-marker
> shapes with 4 unit tests (PR #58, `test/scout.test.ts`); future
> Claude/OpenCode format changes fail at scout, not mid-loop.
> Proxy/infra-error policy — cheap `claude -p OK` probe gates
> orchestrate-loop dispatch (PR #61, opt-out `ORCHESTRATOR_MODEL_PROBE=0`)
> and `isTransientProviderError` now matches `unrecognized_model`,
> `empty stream`, `empty response` (PR #60, `test/classify.test.ts`);
> together they end the "loop stuck, not shipping PRs" pattern during
> omniroute rate-limit windows.
>
> **Completed post-v0.3 (2026-08-28, defect cluster):** PRD-curator
> self-checkout race (PR #55 — `scripts/prd-curator.sh` was leaving
> HEAD on the `docs/prd-curation-*` branch after opening its PR, the
> exact branch-race hazard in
> [[selfbuild-checkout-branch-races]]). Herdr pane env-file scrub
> (commit 443dee8 — `src/integrations/herdr.ts` `renderEnvFile`
> filters to `/^[A-Za-z_][A-Za-z0-9_]*$/`; non-identifier keys like
> `npm_config_node_pre_gyp:cache` were aborting the pane's `source`
> with zsh "not valid in this context", so no PATH/HOME ever reached
> the worker). Uniform spawn-helper PATH fallback (commit 5149887 —
> all child-process spawns now route through `runCli`/`syncCli`/
> `spawnChild`, so the next worker added can't bypass the PATH
> fallback). Herdr watchdog grace + resilient reaper scope + model
> forwarding to workers (commits 3c67178). Reaper unblocks
> dependency-blocked tasks (commit 2446a38). Worker wall-clock raised
> to 60 min, no-progress to 15 min (commit 2602dd2).
>
> **Completed post-v0.3 (2026-08-29):** Resource-aware concurrency
> governor (PR #64 — `src/orchestrator/governor.ts` samples
> `os.freemem()` + per-worker RSS, computes
> `effectiveConcurrency` with a 0.7 safety ratio and p75 1 GB
> per-worker estimate, `<5 ms` per call, `<1 s` cache; wired into
> `scheduler.ts`, `fleet.ts`, and `cli.ts` so `--concurrency auto`
> is the default-friendly path; closes the
> `docs/prds/PRD-resource-aware-concurrency` PRD that the curator
> had been omitting from the queue). G5:STRIDE threat-modeling
> gate (PR #65 — `src/validation/stride-gate.ts` flags
> hardcoded credentials, SQLi, command injection, unsafe
> deserialization; 7/7 unit tests, full suite 535/535 green) —
> the gate is now a real artifact, not a backlog item, and the
> `COMPACT_CONTEXT_MARKER` plumbing from PR #57 already feeds it.
> Stuck-board recovery (PR #68 — `scripts/orchestrate-loop.sh`
> archive-and-rebridge): `requeue_parked` now resets `attempts` to 0,
> boards stuck after two fruitless requeue rounds archive to
> `.devagent/archive/` so the queue bridge can re-plan from the oldest
> queued goal, and the bridge also fires when the board has no
> dispatchable tasks — ends the 2026-08-29 04:15–10:00 UTC stall class
> (park/requeue deadlock, dead-gated bridge, zombie PRs).
>
> **Completed post-v0.3 (2026-08-29, curation run 10):** per-task PR
> publish (PR #71 — `publishTaskPr` dep on `SchedulerDeps`,
> `src/orchestrator/scheduler.ts:203`): when a task reaches `done`, the
> scheduler best-effort pushes its attempt branch and opens a PR instead
> of leaving published branches stranded behind the all-done
> `mergeProjectBranches` gate — the loop-69 root cause where one failing
> gate task (T1) blocked every other task's PR from ever existing.
>
> **Completed post-v0.3 (2026-08-29, curation run 11):** auto release
> engineering (PR #75 — `.github/workflows/release.yml` +
> `scripts/release/next-version.mjs`): every merge to `main` computes the
> next semver from Conventional-Commit titles and publishes a tagged GitHub
> release with generated notes; 6 unit tests cover the bump matrix.
> Queue-bridge latency fix (PR #73): archiving a stuck board now re-bridges
> the oldest queued goal in the same cycle instead of falling through to
> `sleep POLL_SECS`, closing the one-poll-interval idle gap in PR #68.
>
> **Completed post-v0.3 (2026-08-29, PR #77):** session-scoped herdr
> stale-pane sweep — `devagent herdr-sweep [--dry-run]` (`src/cli.ts:1096`)
> closes panes in the `devagent` herdr session whose agent is idle/unknown/
> done or a bare shell, with the session name as the trust boundary; the
> orchestrator runs it non-fatally at the top of every cycle
> (`scripts/orchestrate-loop.sh:106`). Reaper path untouched, so non-herdr
> (user) processes stay unreachable.

> **Completed post-v0.3 (2026-08-30, curation run 14):** G5:STRIDE is live
> in the merge path — `src/gates/stride.ts` adapts `runStrideGate`,
> `src/consume.ts` diffs `main...PR branch`, runs `evaluateStride` before
> `autoMergePr`, and HIGH/CRITICAL severities block the merge (PR #80).
> Scout payload extraction is regression-proofed by a fixture-driven
> golden suite with `scout --replay` (`src/scout.ts:324`, six fixtures +
> `golden.json`, PRs #81/#82) — future Claude/OpenCode shape changes fail
> at replay, not mid-loop.
>
> **Completed post-v0.3 (2026-08-30, curation run 15):** clean-main
> guard before merge-back (PR #84 — `ensureCleanMainWorktree` +
> `popStashBySha` at `src/git/worktree.ts:247`, wired into the
> `mergeProjectBranches` path in `src/cli.ts:805`): a dirty or
> wrong-HEAD main worktree auto-stashes tracked + untracked changes
> and fails fast with an exact error instead of the loops 52/53/54
> dirty-merge failures; 6 temp-repo tests cover the clean/dirty/
> detached/wrong-branch matrix.
>
> **Completed post-v0.3 (2026-08-30, curation run 16):** Release workflow
> hard-gated on CI — `release.yml` declares `needs: [test]` so a red main can
> no longer ship a semver tag (PR #86); the needs-chain is asserted by
> `test/release-workflow.test.ts` as a unit-test invariant, per the loop 57/58
> lesson that a bare workflow edit alone fails to stick. Closes Q21 as
> hard-gate. Scout replay in CI — the golden fixture suite
> (`test/scout-golden.test.ts`) runs inside `npx vitest run` in `ci.yml`, so a
> Claude/OpenCode output-shape change is caught by CI without manual replay.
> Publish-after-cleanup defect — when `cleanup=auto` snapshots worker output
> onto the run branch and removes the worktree, `publishTaskPr` now commits
> from the run branch instead of the deleted cwd (PR #88; regression test at
> `test/task-publish.test.ts:116`; loops 57/58 tripped the breaker 3x on this
> before any retry landed). New worker adapter: omp (PR #87, hardened same-day
> in PR #89) — NDJSON stream parser with a live-smoke fixture, registry entry,
> and a 10-min per-adapter no-progress watchdog after `omp -p` silent hangs;
> the follow-up disabled prewalk, parses stream errors, and caps infinite
> retry.

> **Completed post-v0.3 (2026-08-31):** omp startup + model-id hardening —
> headless argv emits `--no-lsp --no-extensions`, cutting omp cold-start from
> 78–487s (worst: stuck in `discoverAndLoadMCPTools`) to ~17–21s (PR #91), and
> provider-unqualified model aliases are dropped so omp falls back to its
> configured `modelRoles.default` (PR #92); loop 58's exit-1-in-12s class now
> dies at the adapter boundary, not mid-board. Watchdog tuning needs no code
> edit — `resilience.noProgressTimeoutMs` threads from config through the
> executor to adapter opts (`src/orchestrator/executor.ts:69`).
>
> **Completed post-v0.3 (2026-08-31, defect cluster):** watchdog progress
> gating — glm-style models stream `thinking_delta` forever with zero tool
> calls, so byte-counting read silence as progress and three attempts burned
> the full 60-min wall clock (run 1261d6be); the herdr watcher strips
> thinking lines before counting bytes (PR #93) and `spawnCliStreaming` now
> gates the progress clock on meaningful output only (PR #94).
>
> **Completed post-v0.3 (2026-08-31, curation run 19):** Post-PR lifecycle
> automation, CI-Fixer half — when `evaluateChecks` returns failed checks at
> the publishStage→autoMerge boundary, `autoReviewAndMergeOne` re-dispatches a
> bounded fixer worker (`TASK-fix-<pr>`) with the failed check names, re-polls,
> and merges only on green; terminal failures emit a structured
> `ci-fix-failed` outcome in the Q24 error taxonomy
> (`src/integrations/autopr.ts:363`), with failed-then-green, still-red, and
> no-fixer unit tests. PR #96 landed the pure decision function, PR #97 the
> dispatch loop. Zombie-PR hygiene (auto-close red-across-grace-window or
> superseded-base PRs) is the surviving half and stays on the backlog; the
> completed "Scout replay in CI" bullet (PR #86) is retired below.
>
> **Completed post-v0.3 (2026-09-01, curation run 21):** CI-Fixer ledger
> rows — PR #100 writes one dispatch + one outcome row per `TASK-fix-<pr>`,
> closing Q35 with round-trip regression tests for still-red sequences;
> failure-cluster analytics can now count fixer attempts per goal. Q27
> SHA re-burn guard — commit 60638d3 blocks the bridge from re-issuing an
> already-shipped goal after a fresh requeue, narrowing the loop-53/55/57/58
> failure class to a single-shot recovery (the deeper failure-class carryover
> stays on the backlog). Scout deterministic fallback + maxQueued guard —
> commit aefc6bf resolves empty/idless scout payloads to a stable task id
> so the scout queue cannot silently grow unbounded. Commits 20ce8fd adds pi
> to the worker registry and 34cf3d9 makes
> task-path dispatch honor `herdr.enabled`, unblocking the selfbuild loop
> when omp's model alias path is unavailable.
>
> **Queue sweep (2026-08-31, operator):** 25 queue rows resolved without new
> code — 17 marked done citing shipped PRs (#84 pre-loop guard, #86
> release-gate + scout-replay-in-CI, #96 CI-Fixer; `NESTED_ENV_BLOCKLIST`
> env-scrub in `src/workers/spawn-utils.ts:35,61` + herdr sweep PR #77), and
> 8 duplicates consolidated (7 merge-back variants into
> `IMPROVE-retire-legacy-mergeback`, 1 provider-health variant into
> `OBSERVE-provider-health`). Queue now 15 pending / 33 done / 1 failed.
> Curator: before enqueuing, match candidate titles against merged PR titles
> and these completion notes; skip or auto-done on match.
>
> **Worker default (2026-09-02, operator):** all agents — run/task/fleet,
> orchestrate planner+executor+auditor, selfbuild dev+PO, PRD curator,
> warroom, dogfood — now default to omp (`omp -p --mode json`). pi remains
> a registered worker; historical notes above describe the pi era.
>
> **Completed post-v0.3 (2026-09-01 → 09-02, curation run 22):** Executor
> failure surface — duplicate trailing `trail.jsonl` signatures mark
> `taskInterrupt`, abort the worker, and write a ledger post-mortem (goal,
> failure class, gate excerpt, trail hash) on board archive (PR #102,
> `src/orchestrator/executor.ts:106`, `ledger.ts:126`). Zombie-PR hygiene —
> `devagent pr-hygiene` + the `allDone` sweep close base-superseded `TASK-*`
> PRs, flag red-across-grace ones, and skip `autoMerge` until green (PR #103,
> `src/orchestrator/pr-hygiene.ts`). Landing-evidence triage (2026-09-12):
> when a TASK PR's body cites an issue that is CLOSED, the same sweep closes
> it — `shipped-elsewhere` citing the `(#N)` landing commit on main, or
> `superseded` with an explicit not-shipped comment when no landing commit
> exists; an open issue keeps the PR open with a once-per-PR evidence
> comment (internal/orchestrator/pr-hygiene.go).
> Legacy `mergeProjectBranches` gated so a
> board that published per-task PRs no longer double-merges (PR #107,
> `src/cli.ts:850`). Adapter-declared `WorkerAdapter.isProgress` replaces the
> `"thinking_delta"` substring heuristic (Q33, commit 8d08e6f), and the
> zero-event hung-worker signature now yields a distinct no-progress outcome
> so callers fall back without the timeout burn (PR #106). `devagent status
> --providers` exposes probe, transient class, and circuit state (PR #105).
> Bullets retired: selfbuild state bootstrap (012d78c) and merge-queue rebase
> automation — `devagent rebase-stack` rebases stacked branches onto updated
> parents in a throwaway worktree (`src/git/rebase-stack.ts`). Loop-friction
> fixes: queue-first goal selection ends the scout deadlock (aac28b6),
> herdr-sweep each iteration (b302210), reviewer idle backoff (df7c6c9),
> cleanup ancestry fallback (3ee9a9a), release remote-tag + idempotent tag
> (9c7132a), CI-Fixer local dispatch fallback (ac100ba).

> **Completed post-v0.3 (2026-09-02, curation run 23):** Provider
> model-id validation at dispatch (Q32) — `validateModelId` declares
> each adapter's accepted id shape (`src/workers/model-id.ts:62`) and
> preflight rejects unsupported ids before worker spend
> (`src/deps.ts:206`, `src/orchestrator/executor.ts:199`), surfaced in
> `status --providers` (`src/cli.ts:436`). Lessons eval guard,
> deterministic slices — content-similarity dedupe rejects
> near-duplicate appends (PR #116), then the evaluate→accept step runs
> the regression suite on the staged lessons state, reverting on red,
> one accept/reject ledger row per gated append (PR #117,
> `src/lessons/guard.ts:278`, wired at `src/cli.ts:1437`); the
> must-beat-best-so-far and held-out tiers stay future work. Scout
> heartbeat — `devagent scout-status` prints liveness + queue depth
> (`src/cli.ts:1333`, PRs #112–#114). Operator hardening same day,
> outside the backlog: research moved to local-evidence-only at a 900s
> budget after 34 consecutive 300s-cap kills (5d8a319, 063831e), NDJSON
> assistant-text extraction into goal files (d3adf17), and all agent
> roles defaulted to omp (799fd86).
> **Completed post-v0.3 (2026-09-02 → 09-03, curation run 24):** Operator-role
> provider preflight (Q40) — `devagent preflight --role <curator|po|selfbuild|
> warroom|reviewer>` (`src/cli.ts:1044`, `src/resilience/preflight.ts`) probes
> the worker CLI under a 60s cap; on failure it writes a structured
> `operator-degraded` ledger row, advances the shared circuit state, and exits
> nonzero so the calling loop skips its agent cycle — visible degradation, not
> a silent noop. Wired into the curator/reviewer/selfbuild/warroom loops (PR
> #119), default-on with `OPERATOR_PROBE_DISABLED=1` opt-out (PR #122).
> Same-day defect cluster (PR #120): probe stdin closed (22 overnight stalls,
> 25 DEGRADED cycles, 2 breaker trips), success-path circuit advance so a
> recovered provider isn't stuck open, PO-dispatch timeout guard, research/PO
> dispatches get `/dev/null` stdin (omp `readPipedInput` hang), `CLAUDE_TIMEOUT`
> 300→600s. Lessons impact telemetry (Q39) — `recordLoopResult` loop rows join
> `lessons-eval` accept/reject rows; `loadLessonScores` (score = accept rate −
> repeat-failure delta) ranks the `lessonsMaxChars` digest by measured effect
> instead of recency (PR #120, `src/lessons/guard.ts`, `src/prompt.ts`).
> Closes Q40 + Q39; must-beat-best-so-far and held-out tiers stay future work.
> **Completed post-v0.3 (2026-09-04):** PRD freshness gate (operator fix,
> outside the backlog) — the scout and the selfbuild loop select work from
> `docs/PRD.md`, but nothing refreshed the tree from origin before reading
> it, so a manual PRD update pushed from another machine stayed invisible
> until an unrelated `git pull` landed and the factory kept building the
> older doc version. `syncWorkSelectionDocs` (`src/git/doc-sync.ts`) fetches
> origin and fast-forwards before each live scout cycle (opt-out
> `scout.syncDocs=false`) and each loop iteration (`SELFBUILD_NO_SYNC_DOCS=1`
> or `--no-sync-docs`), refusing the sync while `docs/PRD.md` is locally
> modified — the loop then records a `provider-degraded` row and skips its
> agent cycle instead of silently building stale work, and a failed scout
> sync degrades to a skipped heartbeat cycle (ace2b88).
> **Completed post-v0.3 (2026-09-07):** FR-VIS-09 extended from the loop driver
> to queue claims — the `claimTask` last-writer-wins TODO meant two consumers
> that both read a `pending` task each believed they owned it, and the later
> writer silently overwrote the earlier one's record. Claims now take an atomic
> `link()` lock per task (`acquireClaimLock`, `src/queue.ts:172`; `link()` fails
> EEXIST so exactly one caller wins, and it is portable to macOS where `flock`
> is unavailable — the same primitive family as the driver's mkdir lock) and
> issue a fencing token: `QueuedTask.leaseGeneration` plus `leaseOwner` and
> `leaseExpiresAt` (`src/queue.ts:27-31`). A `claimed` task past its lease is
> reclaimable and the reclaim bumps the generation — a token is never reused —
> so `claimNextPending` recovers a crashed worker's task instead of wedging
> (`src/queue.ts:443`). Every claim-lifecycle write carries the token and is
> refused when stale: `completeTask`/`failTask`/`requeueTask`
> (`src/queue.ts:348,358,375`) and the guarded `updateTask`/`setTaskStatus`
> (`expectedGeneration`); `requeueTask` bumps the generation on release so the
> releasing worker's own late writes die with the release. `src/consume.ts:311`
> threads its claim token through completion, requeue, and failure, and
> `scripts/selfbuild-queue-claim.mjs` hands the loop the token
> (`leaseGeneration`) which `scripts/selfbuild-queue-done.mjs` presents via
> `DEVAGENT_QUEUE_LEASE` — a stale token refuses loudly instead of clobbering
> the new owner. Legacy records with no lease fields stay reclaimable. 9
> deterministic lease tests (`test/queue.test.ts:144`); `devagent queue list`
> output is unchanged.

#### Phase 4 — history (backlog migrated to GitHub issues 2026-09-07)

> **Tracker migration (2026-09-07, operator):** the static backlog below is
> retired. Open work now lives as GitHub issues labeled `selfbuild`, ordered by
> `priority:P0` > `P1` > `P2` — the selfbuild loop claims from that queue
> (issue-first, deterministic: priority rank then oldest issue number), the
> PRD curator files and re-prioritizes new issues instead of maintaining this
> list, and every PR must land with its `docs/PRD.md` state update
> (PRD-per-PR policy; `docs/SELF-BUILD-LOOP.md` "Tracker + PRD policy").
> Filed at migration: #144–#148 (existing operator items) and #179 daemon
> control API (P1), #180 network sandbox allowlist (P2), #181 desktop control
> app (P2). Struck lines below are the shipped history.
>
> **Go migration (2026-09-07, operator decision):** the core (CLI, daemon,
> orchestrator, loop drivers, TUI) migrated from TypeScript/Node to a single
> Go binary — PRD §22, master tracker issue #207 (closed), phases G0–G3
> (#190–#206). **Complete 2026-09-08:** all 16 FR-GO requirements shipped
> (FR-GO-01..14 in the G1 wave, FR-GO-15 cutover + FR-GO-16 retirement the
> following day); the selfbuild loop self-hosts on the Go binary
> (`SELFBUILD_DEVAGENT_BIN` + `SELFBUILD_TEST_CMD`), Node is deleted from
> main, and CI/release are single-language Go (issue #204/#205 closed).
> 2026-09-10 (issue #300): the repo-level gate default flipped from `npm
> test` to `go test ./...` — after the Node retirement an npm default could
> only ENOENT, stamping green iterations `failed-tests` (loop 216 evidence).
> 2026-09-11 (issue #301): goal construction carries the phase-1 pick forward.
> Iteration 219 re-implemented #290 from scratch — burning a full 60-min
> attempt — while green, mergeable PR #298 sat open (it has since merged,
> 2026-09-10T16:37Z, and #290 is closed), because
> `.selfbuild/research/loop-219.md` said "**#290 — merge PR #298, not a
> rewrite**" and the goal file contained only the raw implement template.
> `loopdriver.researchPick` now reads the iteration's research artifact,
> appends its pick rationale to the dispatched goal, and turns a present-tense
> "merge/land PR #N" directive into a verify-and-merge dispatch (`gh pr checks`
> → merge → close → PRD update) instead of a rewrite — but only while that
> pull request is still `OPEN`, because research is also asked whether a
> *merged* PR already covers the pick, and a landed PR's merged state would
> otherwise satisfy the ship gate for work nobody did. Such an iteration ships
> on that pull request's merged state rather than the `PR opened:` line it can
> never print, and fast-forwards the repo test gate onto the merged tree before
> verdicting it. The ledger status stays `ok`: `internal/lessons` scores any
> non-`ok` loop-result row as a failed loop, so the land is recorded in the
> iteration log, the `loop-phase` detail and the row's goal text instead.

~~- **Cross-board retry memory beyond the SHA guard** — commit 60638d3 stops re-issuing shipped goals, but re-queued failures still get a fresh attempt budget; carry the prior board's failure class onto the re-bridged goal so the scout deprioritizes until the root-cause fix lands (Q27).~~
~~- **Regression oracle before board merge** — gates judge single PRs and PR #108's committed STRIDE allowlist widens suppression paths; add a board-level "is the system at least as good?" check (full suite on the merged result) ahead of `autoMerge`, per the Kitchen Loop zero-regression rule.~~
~~- **GRADIENT — structural gradient sensor** — exit-code scalar architecture gate (sentrux: `.sentrux/rules.toml`, lowest-scoring root cause per change) plus an adjacent-category scan (sensors, MCP servers, harness tooling) in scout/selfbuild research prompts; the agent-products-only funnel is why sentrux was missed entirely (2026-09-01 human deep-dive; Q38).~~
- **Release/tag events as ledger outcomes** — ~~the release workflow needed same-day hotfixes (9c7132a remote-tag resolution + idempotent tag) yet the ledger stays PR-URL-only; record tag/release outcomes so per-loop spend-to-shipped-artifact math can count releases (Q24).~~ **Shipped 2026-09-04:** `ReleaseLedgerRecord` (`release-created` event: tag, sha, version, source) in `src/orchestrator/ledger.ts:195` with `appendReleaseRecord`, a `devagent record release` CLI path, and a `release.yml` "Record release event in ledger" step — per-loop spend-to-shipped-artifact math can now count releases.
- **Consolidate the loop scripts** — ~~recovery keeps landing in shell (aac28b6 queue-first selection, b302210 sweep-each-iteration, baa4eda discovery sweep) while untracked `scripts/orchestrator-loop.sh` runs divergent logic; fold recovery into `src/orchestrator/` and reduce the shell to a thin caller (Q19).~~ **Shipped 2026-09-07 (PR #165):** `src/orchestrator/board-recovery.ts` (typed, tested — `test/orchestrator/board-recovery.test.ts`) behind a `devagent board-recovery` CLI (`src/cli.ts:128`); the gate performs the action it verdicts and prints one verdict line, and `scripts/orchestrate-loop.sh:118-153` is now a thin caller that only maps `wait`/`requeue`/`archive` to loop control.
~~- **PRD-backlog reconciliation at pick time** — Q40 was re-selected three runs running (PRs #119/#120/#122) because the Phase 4 backlog lags merges; cross-check a pick against merged PR titles and these completion notes before dispatch, and strike shipped items in the same run (Q27 family; extends the curator title-match rule from the 2026-08-31 queue sweep).~~
~~- **Lessons must-beat-best-so-far + held-out tier** — Q39's measured score now ranks the digest, but nothing requires a candidate to beat the current best on loops it did not inform; add the AHE/Meta-Harness held-out slice so ranking cannot game the training distribution (flagged "future work" in the run-24 note above).~~
~~- **Simplicity-first surface pass (§21 FR-SIMPLE)** — the 2026-09-05 operator direction (§21) makes user simplicity the deciding quality: ship the implementable v1 slice — `devagent init` guided setup with prerequisite checks + verified smoke (FR-SIMPLE-01), the ≤3-step goal path with sane defaults (FR-SIMPLE-02), and human-readable-by-default status rendering in the §20.8 card/chip language with `--json` opt-out (FR-SIMPLE-03, FR-SIMPLE-04); walk the §21.1 four principles over the onboarding path at release review (FR-SIMPLE-06).~~

### Phase 5 — Personal agent surface (proposed, post-v1)

Direction addendum 2026-09-03 (section 20): DevAgent becomes the local-first, BYO-provider counterpart to xAI/Cursor's Grok Bot — named role agents with approval gates and durable memory, dispatched from a lightweight cross-platform desktop control app (Tauri 2: macOS menubar/tray, Windows + Linux tray), with first-class Grok/xAI worker support.

> Tracker status (2026-09-09): open Phase 5 work is tracked as GitHub issues,
> not here. Shipped 2026-09-09: #179 daemon control API (FR-CTRL-01..05, live
> on :7788), #181 desktop control app v1 (Tauri 2 thin client, `app/`,
> FR-UI-01..05/07/08 functional, FR-UI-06 signing + FR-UI-09 3-OS CI
> scaffolded — PR #267), #146 TUI polish (FR-TUI-P-01..12, PR #268). The
> network sandbox allowlist (#180, P2) is the remaining Phase 4 leftover.
> Shipped from this phase: Grok/xAI worker adapter (`internal/workers/grok.go`),
- **Easy handoff / control plane (FR-HAND, #145)** — Shipped 2026-09-08: cold path is exactly `devagent init` → `devagent tui` (optional `start` alias); TUI dispatch sheet (`n`, FR-HAND-02) + approve sheet (`g`, FR-HAND-07); `POST /dispatch` threads `autoPr` into the spawned `task --auto-pr` argv gated on `GITHUB_TOKEN` (FR-HAND-03); init adds worker-detect chips (FR-HAND-04), herdr default-on advisory (FR-HAND-05), Orca repo registration without worktree provisioning (FR-HAND-06). Look/feel bar: #146.
- **Daemon control API** — ~~localhost REST + SSE control surface on the existing `serve` pattern with per-boot token auth and Origin/Host validation (FR-CTRL-01..05).~~ **Shipped 2026-09-08 (#179, PR #243):** internal/daemon serving `/status` `/agents` `/events` SSE `/history` + `POST /dispatch` `/approve`, per-boot token + loopback bind, wired as `devagent daemon` and live on :7788.
- **Cross-platform desktop control app** — ~~Tauri 2 tray + dashboard on macOS, Linux, and Windows: dispatch agents/roles/tools, live agent log tails, approval inbox, notifications, pipeline visualization (FR-UI-01..09).~~ **Shipped 2026-09-09 (#181, PR #267):** Tauri 2 thin client at `app/` (Rust core + TS webview): FR-UI-01/02/03/04/07/08 functional (tray state, dispatch sheet, SSE dashboard, approval inbox + notifications, stage timeline), FR-UI-05 wired (autostart/single-instance), FR-UI-06 signing and FR-UI-09 3-OS CI scaffolded by design (no fake signing); see `app/README.md`.
- **Grok/xAI worker support** — `grok` (Grok Build CLI) WorkerAdapter + native xAI API fallback, per-provider model-id predicate, exact per-run cost in the ledger, sticky prompt-cache keys, batch/off-peak routing (FR-GROK-01..06).
- **Bot-style UX floor** — named persistent agent identities, teach-once routines, visible bot-to-bot handoff (§20.1 benchmark; scope = Q45).
- **Visible worker sessions + jump-in** — worker runs surface in a persistent terminal workspace (herdr panes) the operator can attach to and steer at any time, like a human running the coding agent in an open terminal; headless becomes the explicit CI/server mode, not the only mode (§20.8 FR-VIS).
- **Terminal TUI dashboard** — pilot-style full-screen TUI (`devagent tui`): current task + phase, queue depth, token/cost vs budget cards, approval hotkeys — over SSH, on macOS/Linux/Windows (§20.8 FR-TUI).

## 18. Open Questions

| # | Question | Owner | Needed by |
|---|---|---|---|
| Q11 | ~~`.devagent/AGENTS.md` auto-load — trust prompt on the operator's managed-settings page (Codex CVE-2025-61260 pattern) or one-time per-repo confirm?~~ Resolved 2026-09-07: one-time per-repo confirm — config `context.agentsMd` (`ask` default, `on`, `off`; validated beside `context.kg`, `src/config.ts:289`) gates the loader (`loadAgentsMd`, `src/prompt.ts`); `ask` injects nothing until `devagent trust agents-md --repo <path>` records the approval once in `<repo>/.devagent/trust.json` (`trustAgentsMd`), and trusted content reaches worker/planner prompts through the existing `spliceCompactContext` knowledge digest (`buildKnowledgeContext` AGENTS.md layer). Unit + CLI coverage in `test/agents-md.test.ts`. Removed. | product | Phase 4 |
| Q12 | ~~With PR #57 merged, G0 plan-critic and G5:STRIDE both read the same `COMPACT_CONTEXT_MARKER` digest — should the audit track provenance (which gate, which loop, which trail line) per consumed entry, or treat the digest as opaque input?~~ Resolved 2026-09-07: opaque input — `evaluateStride` documents `contextDigest` as provenance-only and passes it through verbatim to the gate result without tracking which entry each gate consumed (`src/gates/stride.ts:123`); no per-gate/per-loop audit trail was added. Removed. | eng | Phase 4 |
| Q13 | ~~`taskInterrupt` mid-flight: kill the worker process directly, or graceful-stop signal with force-kill on grace timeout?~~ Resolved 2026-09-01 (PR #102): direct kill — duplicate trailing failure signatures mark `taskInterrupt` and `killStaleProcessTree` fires (`src/orchestrator/executor.ts:164`); no graceful-stop tier. Removed. | product | Phase 4 |
| Q14 | ~~With PR #64's governor on by default, should `devagent status` surface the live `effectiveConcurrency` + `lastSample` + per-worker RSS, or stay at the one-line readout added in PR #64 AC-10? Operators need enough to debug "why is auto-1 today" without leaking the per-pid sample into the human view.~~ Resolved 2026-09-07 (PR #174): stay one-line, enriched — `formatStatus` appends the cached sample's age and an aggregate calibration count (`sample <age>s, cal <n>`, `src/orchestrator/governor.ts:174-182`), enough to debug "why is auto-1 today" while never surfacing the per-pid RSS into the human view. Removed. | eng | Phase 4 |
| Q15 | ~~Now that the curator/queue gap behind PR #64's "sat in docs/prds/ for weeks" is recognized, should the curator's audit step (proposed above) be authoritative (curator enqueues directly) or advisory-only (just emits a warning the next scout cycle reads)? Direct write simplifies, but couples the curator to scout's queue schema.~~ Resolved 2026-09-07: advisory-only — `auditPrdCoverage` scans `docs/prds/*.md` against `listTasks()` coverage and returns findings beside a hard `enqueued: 0` invariant (`src/curator/audit.ts:100`, `src/curator/audit.ts:175`); `devagent prd-audit --repo <path> [--json]` (`src/cli.ts:160`) prints warnings on stderr and always exits 0, and `scripts/prd-curator.sh:50` runs it right after the sync so the warnings land in the curation cycle log for the next scout cycle to read. The curator calls no queue writer (`enqueueTask`/`writePrd` are never referenced from `src/curator/`), so it stays decoupled from the queue schema — the read-only contract is pinned by `test/curator-audit.test.ts`. Removed. | eng | Phase 4 |
| Q16 | ~~PR #68 archives stuck boards to `.devagent/archive/` and re-bridges from the oldest queued goal — should archive events emit a webhook/notification (the stalled factory ran 6h before a human noticed), and what is the retention policy for archived boards?~~ Resolved 2026-09-07: yes to both. The Q41 paging transport is generalized into a typed operator-alert seam (`src/resilience/operator-alert.ts`: `OperatorAlert` union + `postOperatorAlert`), and `archiveBoard` (`src/orchestrator/board-recovery.ts:324`) now fires `event:'board-archived'` for both gate verdicts (completed `board` + stuck `board-stuck`) through the same best-effort injectable `notify` seam. Retention is bounded: `pruneArchive` keeps the newest `resilience.archiveKeep` stamped archives (default `ARCHIVE_RETENTION_KEEP` = 20) on every archive, and `scripts/orchestrate-loop.sh`'s inline `mv` is gone so no archive escapes notify/retention. Proven in `test/orchestrator/board-recovery.test.ts` (injected notify, fake clock) plus a `devagent board-recovery` CLI smoke against a local webhook. Removed. | eng | Phase 4 |
| Q17 | ~~`requeue_parked` now resets `attempts` to 0 (PR #68), so a task that exhausts `maxTaskRetries` gets a fresh budget on every requeue round — is unbounded retry the right policy, or should cumulative attempt history across rounds cap a task permanently?~~ Resolved 2026-09-07: capped — every dispatch increments a lifetime `totalAttempts` counter that neither recovery grants nor requeue resets (`src/orchestrator/scheduler.ts:230`); at `--max-total-attempts` (default 0 = unbounded, `src/cli.ts:136`) the scheduler refuses recovery grants (`src/orchestrator/scheduler.ts:118-133`) and `requeueParked` refuses the fresh budget so capped tasks stay terminal (`src/orchestrator/board-recovery.ts:245-249`). Removed. | eng | Phase 4 |
| Q18 | ~~The 08-29 stuck board burned 3 salvage-instructed attempts on STRIDE wiring — should the executor refuse prompts above a size/complexity threshold (the salvage prompt was \~5 KB of step-by-step edits) and force a plan-split instead, or is dense prescriptive prompting the right shape and only the fail-signal capture (backlog item above) needs fixing?~~ Resolved 2026-09-07 (PR #173): refuse — a prompt-size preflight measures the task's own instruction payload before worktree creation or any worker spend and fails the dispatch with `failureClass: 'prompt-oversized'`, forcing a plan-split (`src/orchestrator/executor.ts:235-244`); ceiling `resilience.maxPromptBytes` (default 4096, `src/orchestrator/executor.ts:37`), 0 disables the guard. Removed. | eng | Phase 4 |
| Q19 | ~~`scripts/orchestrate-loop.sh` still carries PR #68's recovery logic only as an untracked-path script risk — PRs #67/#69 shipped within minutes of each other while the recovery fix lives in a tracked-but-shell-level layer; should stuck-board recovery move into `src/orchestrator/` (typed, tested) with the shell reduced to a thin caller, or stay in the script where iteration is cheaper?~~ Resolved 2026-09-07 (PR #165): moved — `src/orchestrator/board-recovery.ts` with the `devagent board-recovery` CLI (`src/cli.ts:128`) and unit coverage (`test/orchestrator/board-recovery.test.ts`); `scripts/orchestrate-loop.sh:118-153` is a thin caller mapping the gate's verdict (`wait`/`requeue`/`archive`) to loop control. Removed. | eng | Phase 4 |
| Q20 | ~~Per-task PRs vs `mergeProjectBranches` on `allDone` — feed reviewer/`autoMerge` directly, or stay review-only until a human decides?~~ Resolved 2026-09-01 (PR #107): the legacy path is gated — a board that already published per-task PRs skips `mergeProjectBranches` (`src/cli.ts:850`), so merge-back is a no-op there. Removed. | product | Phase 4 |
| Q21 | ~~The Release workflow publishes on every push to `main` regardless of the `test` job's result — should releases hard-gate on CI green (`needs: test`), or tag first and yank on red? Hard-gate delays tags by one CI run; tag-first risks shipping a broken semver point.~~ Resolved 2026-08-31 (PR #86 hard-gated, unit-test-pinned); removed. | eng | Phase 4 |
| Q33 | ~~Adapter-declared progress classifier vs shared substring heuristic, and where does the omp/glm fallback live?~~ Resolved 2026-09-01 (commit 8d08e6f): adapter-declared — optional `WorkerAdapter.isProgress` predicate, shared omp/glm fallback in `src/workers/progress.ts`, unit-covered in `test/progress.test.ts`. Removed. | eng | Phase 4 |
| Q34 | ~~Run 1261d6be proves the watchdog can silently never fire; should the herdr watcher and `spawnCliStreaming` emit a per-attempt watchdog-health line to the ledger (clock resets, bytes counted, last meaningful-output timestamp) so a never-firing watchdog is visible in analytics instead of inferred from three 3601s timeouts?~~ Resolved 2026-09-04 (PR #136): emitted from both — `event: 'watchdog-health'` rows written per armed attempt by `spawnCliStreaming` at `src/workers/spawn-utils.ts:221` and the herdr watcher at `src/integrations/herdr.ts:322` via `appendWatchdogHealthRecord` (`src/orchestrator/ledger.ts:296`), carrying clock resets, meaningful bytes, idle time, and fire flags. Removed. | eng | Phase 4 |
| Q35 | ~~CI-Fixer (PRs #96/#97) re-dispatches `TASK-fix-<pr>` but fixer outcomes only surface as `autoReviewAndMergeOne` log lines and the terminal `ci-fix-failed` result — should fixer dispatches and outcomes write ledger rows like PR events do, so failure-cluster analytics can count fixer round-trips per goal, or stay log-only?~~ Resolved 2026-09-01 (PR #100 writes one dispatch + one outcome ledger row per `TASK-fix-<pr>` with still-red round-trip tests); removed. | eng | Phase 4 |
| Q36 | ~~The CI-Fixer fix run (`TASK-fix-<pr>`) gets its own dispatch budget outside the originating task's `maxTaskRetries` — should fixer attempts count against the parent task's retry budget (linking to Q17's cumulative-attempt concern), or is one bounded fix attempt per PR run the right isolation?~~ Resolved 2026-09-07: isolated — the fixer gets exactly one bounded `TASK-fix-<pr>` re-dispatch per PR before the verdict falls back to request-changes (`src/integrations/autopr.ts:583-597`), outside the parent task's `maxTaskRetries`; the cross-round runaway concern is answered by Q17's lifetime `totalAttempts` cap (`--max-total-attempts`, `src/cli.ts:136`), the shared Q17/Q36 mechanism. Removed. | eng | Phase 4 |
| Q22 | ~~PR #73 re-bridges a queued goal in the same cycle as board archive, but the archive itself burns the board's salvage history — should archived boards write a compact post-mortem (goal, failure class, gate excerpts) to the ledger so the next bridge can plan around the same failure mode, or is the existing ledger analytics query surface enough?~~ Resolved 2026-08-31: PRs #96/#97's `ci-fix-failed` outcome gives the ledger a structured failure record with summary evidence (matching the Q24 taxonomy); remaining failure-evidence work is tracked by the backlog item "Executor failure surface". | eng | Phase 4 |
| Q23 | ~~PR #77's herdr-sweep trusts the session name alone; if the sweep ever gains an `--all` mode, what stops it from killing a user-attached interactive claude pane that happens to sit in an automation-spawned session (2026-08-26 mass-kill class)? Require per-pane agent-state verification plus a managed-settings-style deny toggle, or keep `--all` out of scope permanently?~~ Resolved 2026-09-07: both halves of the safety requirement, and `--all` stays out of scope permanently — the sweep lists exactly one session and nothing widens it. Per-pane verification shipped as FR-VIS-07; the managed-settings-style toggle ships as FR-VIS-10: `herdr.sweep` (`src/config.ts:39`, resolved by `herdrSweepConfig` at `src/config.ts:427`) carries `enabled` (env `DEVAGENT_HERDR_SWEEP=0|1`) and `denySessions`, and `findStalePanes`/`sweepStalePanes` (`src/integrations/herdr.ts:462`, `:699`) return nothing when the sweep is disabled or the resolved session is denied. A pane the FR-VIS-02 roster reports as live, or any sweep launched while `DEVAGENT_OPERATOR_ATTACHED` is set, is spared ahead of every other class — including `--orphans` — and reported with `reason: operator-attached` instead of closed. Unset defaults are today's behavior; an invalid `herdr.sweep` block fails closed. Removed. | product | Phase 4 |
| Q24 | ~~Per-task PRs (PR #71) plus the auto-tag release workflow (PR #75) mean a fully-merged board can produce several PRs and a release in one cycle — should the ledger record release/tag events as first-class outcomes (so per-loop spend-to-shipped-artifact math counts a release), or stay PR-URL-only?~~ Resolved 2026-09-04: first-class — `ReleaseLedgerRecord` (`release-created`: tag, sha, version, source) at `src/orchestrator/ledger.ts:195` with `appendReleaseRecord`, `devagent record release` CLI, and the `release.yml` "Record release event in ledger" step write it; the §17 run-24 backlog bullet was stale and is struck. Removed. | eng | Phase 4 |
| Q25 | ~~G5:STRIDE blocks on HIGH/CRITICAL with no suppression path; fixture credentials in test files (the exact pattern the golden suites ship) will trip it and stall autoMerge — add a per-path/per-finding allowlist committed with the PR, or keep it hard and force workers to rename literals?~~ Resolved: a PR may commit `.devagent/stride-allowlist.json` (`{"paths": [...glob patterns...]}`) read from the PR branch at gate time; findings whose file matches an allowed path are suppressed. Absent or malformed allowlists fail closed. | eng | Phase 4 |
| Q26 | ~~PR #84 auto-stashes a dirty main before merge-back, but the stash is keyed by SHA and never re-offered — if a curation PR (like #83) is open in the same worktree when the factory merges back, should the popped stash be surfaced as a ledger warning (operator reapplies by hand) or re-applied automatically on the next dispatch?~~ Resolved 2026-09-07 (PR #172): ledger warning — `restoreAutoStash` pops the auto-stash by SHA after merge-back and writes a `merge-back-stash` ledger row with outcome `restored` or `retained` (`src/orchestrator/merge.ts:116-124`); a failed pop leaves the stash intact for manual recovery — no automatic re-apply on the next dispatch. Removed. | eng | Phase 4 |
| Q27 | ~~Loops 53-55 and 57/58 each re-burned multiple attempts on the same already-planned goal after a requeue with a fresh attempt budget — should the bridge attach the prior board's failure class to the re-bridged goal so the scout skips it until the root cause ships, or is cross-board retry memory out of scope for the single-tenant model?~~ Resolved 2026-09-07: attached — `bridgeQueueToBoard` reads the newest goal-matching archived board and stamps its executor failure class onto the re-bridged goal (`archivedBoardFailureClass`, `src/orchestrator/queue-bridge.ts:110`); the queue carries it on `QueuedTask.failureClass` (`src/queue.ts:29`) and `claimNextPending` drains clean tasks first so a failure-carrying goal waits until the root cause ships (`src/queue.ts:198`). Unit-covered (`test/queue.test.ts:85`, `test/queue-bridge.test.ts:53`); the §17 backlog twin was already struck, this row was the stale half. Removed. | product | Phase 4 |
| Q28 | ~~With FR-CTX-01 injecting a layered digest (markdown baseline + opt-in LeanKG) at `COMPACT_CONTEXT_MARKER`, should the KG portion also be appended to `lessons.md` on a successful merge (so future runs learn from the same structural evidence the planner used), or stay scoped to the single run and rebuild fresh each time? Persisting makes the digest cross-run durable; scoping avoids stale evidence bleeding in — and LeanKG's per-response `freshness` stamp (FR-CTX-05) gives the gate a machine-readable stale-evidence signal either way.~~ Resolved 2026-09-07: persisted, freshness-gated. The dispatch-time provider now records its raw reply (`LeanKgProvider.last`, `src/leankg.ts:210`); `captureKgEvidence` (`src/leankg.ts:229-235`) lifts the verbatim provenance line the run consumed — rendered by `provenanceLine` (`src/leankg.ts:71-79`), never re-derived — onto `ImplementResult.kgEvidence` at the digest build (`src/deps.ts:321`, returned `src/deps.ts:436`) and threads it through the implement outcome (`src/pipeline.ts:139`). `recordMergedKgEvidence` (`src/consume.ts:69-96`) appends it through `appendLessonGuarded` only when `isFreshKgEvidence` (`src/leankg.ts:242`) admits a `fresh` stamp; `stale`, `possibly_stale`, `cold` and an absent stamp are omitted per FR-CTX-05. The call sits after `autoMergePr` resolves (`src/consume.ts:548`), so a gate-blocked merge persists nothing. Dedupe and the evaluate step stay the guard's own; the held-out must-beat tier is off because the candidate is machine-captured evidence, not a proposal claiming a predicted improvement to rank against. Unit-covered with fake `leankg` replies (`test/consume-kg-lessons.test.ts:155` fresh-append, `:172` stale-omit, `:182` dedupe, `:256`/`:288` merge and blocked-merge wiring). Removed. | eng | Phase 4 |
| Q32 | ~~PR #92 drops driver-tier model aliases for omp only, leaving claude-code/opencode adapters to interpret `config.model` their own way — should the model field be normalized once at config load (provider-qualified ids everywhere, aliases resolved to a concrete id), or does each adapter own its id semantics?~~ Resolved 2026-09-02 (PR #115): each adapter owns its id semantics behind a declared predicate — `validateModelId` (`src/workers/model-id.ts:62`) gates preflight dispatch (`src/deps.ts:206`, `src/orchestrator/executor.ts:199`) with unit-covered valid/alias cases. Removed. | eng | Phase 4 |
| Q31 | ~~PR #91 strips LSP/extension discovery from headless omp but the same startup-stall class plausibly exists for other MCP-discovering CLIs; should worker preflight include a cold-start latency budget (fail fast above N seconds) or is the per-adapter no-progress watchdog enough?~~ Resolved 2026-09-07 (PR #164): budget added — `coldStartTimeoutMs` (`src/workers/spawn-utils.ts:33`) kills a launch when no adapter-classified progress line arrives within the deadline and sets `coldStart` on the result (`src/workers/spawn-utils.ts:42`); configured via `resilience.coldStartTimeoutMs` (`src/config.ts:69`) and wired through the omp/claude-code/grok adapters. Removed. | eng | Phase 4 |
| Q29 | ~~PR #88 makes the run branch the source of truth after `cleanup=auto` (snapshot onto `devagent/<taskId>`, then publish from that branch), but snapshot and publish remain two stages with the deleted-cwd failure class between them — should auto-cleanup snapshot and per-task publish collapse into one commit path so the worktree's death cannot strand a green task, or is the regression-test guard enough?~~ Resolved 2026-09-07: collapsed — `finalizeRunWorktree` (`src/git/worktree.ts:176`) now pushes the run branch to `origin` right after the snapshot commit and before `worktree remove`, so remote persistence precedes the worktree's death; a failed push holds the tree in place instead of stranding the snapshot (`FinalizeResult.pushed` / `error`, logged at `src/deps.ts:450`), and publish's own push (`src/cli.ts:1009`) is now an idempotent no-op. Proven by the local-bare-remote tests in `test/worktree.test.ts`. Removed. | eng | Phase 4 |
| Q30 | ~~omp (PRs #87/#89) needed adapter-specific hardening — prewalk off, stream-error parsing, capped retries, a bespoke no-progress timeout — none of which the registry declares. Should `WorkerAdapter` expose a capability/limit block (supported flags, stream quirks, watchdog defaults) the scheduler can honor, or stay as per-adapter internals patched case by case?~~ Resolved 2026-09-07: declared — optional `WorkerAdapter.capabilities` (`src/types.ts:204`, following the Q33 `isProgress` precedent) carries `defaultNoProgressTimeoutMs`; all five adapters declare it (claude-code/opencode 0 = clock off, omp/pi/grok 10m) and the five duplicated resolutions collapse into `resolveNoProgressTimeoutMs` (`src/workers/spawn-utils.ts:64`), which keeps the historical precedence — explicit positive caller value, then caller 0 honored only by a 0-declaring adapter, then `DEVAGENT_NO_PROGRESS_TIMEOUT_MS`, then the declared default — so no adapter's default moves; unit-covered in `test/workers/capabilities.test.ts`. Removed. | eng | Phase 4 |
| Q38 | GRADIENT structural sensor (sentrux): should an external scalar architecture gate block dispatch/merge like G1–G5, or start advisory-only (report the lowest-scoring root cause to the scout) until it has a track record on this repo? | eng | Phase 4 |
| Q39 | ~~PR #117's eval guard requires `predictedImpact` on machine-appended lessons but never scores it — should accept/reject outcomes be aggregated against loop results (accept rate, repeat-failure delta) to rank the 4000-char `lessonsMaxChars` digest by measured effect, or is gate-level counting enough?~~ Resolved 2026-09-03 (PR #120): aggregated — `recordLoopResult` loop rows join `lessons-eval` rows and `loadLessonScores` ranks the digest by measured effect at `COMPACT_CONTEXT_MARKER` assembly (`src/lessons/guard.ts`, `src/prompt.ts`). Removed. | eng | Phase 4 |
| Q40 | ~~The curator noop'd 3x on 2026-09-02 under dead provider auth (`unrecognized_model`, disabled-key 403, missing omniroute key) while writing "[noop] PRD already accurate" — should operator loops (curator/warroom/PO) fail loud with a ledger row on provider-probe failure, or write a structured `operator-degraded` outcome and keep cycling?~~ Resolved 2026-09-03 (PRs #119/#120/#122): degraded — `devagent preflight` writes a structured `operator-degraded` ledger row and the loop skips its agent cycle, cycling until the per-loop circuit breaker trips; default-on with `OPERATOR_PROBE_DISABLED=1` opt-out. Removed. | eng | Phase 4 |
| Q41 | ~~`devagent preflight` (PRs #119/#120) writes an `operator-degraded` row per failed cycle and each loop trips its own breaker after `MAX_FAILS`, but the 2026-09-03 overnight logged 25 DEGRADED cycles + 2 breaker trips with no notification surface — should consecutive cross-role degradation page a human, or is per-loop breaker exit + `status --providers` enough?~~ Resolved 2026-09-07: page — on the cycle where the trailing degradation streak first reaches `DEGRADE_STREAK_THRESHOLD`, the pager POSTs one `provider-degraded-breach` alert to `resilience.degradeWebhookUrl`: once per outage episode, silent mid-streak, best-effort so paging can never change the caller's decision. Follow-up 2026-09-07 (doc-sync hole closed): that write side lived inside `runPreflightGate` (`src/resilience/preflight.ts`), so only preflight failures paged while the loop's doc-sync refusals — `record` rows mirrored into the same streak by `scripts/selfbuild-loop.sh` — stayed silent (loops 145–154: ten consecutive operator-diverged rows, operator blind). The pager is now shared (`src/resilience/degrade-pager.ts`, same injectable `notify` seam), `DegradeBreachAlert` carries a `source` field (`preflight` or `doc-sync`) in the `OperatorAlert` union, and `devagent page-degrade-breach` gives the loop a `\|\|`-guarded caller beside its sync-failure records. Removed. | eng | Phase 4 |
| Q42 | Grok integration path: ship the Grok Build CLI (`grok -p`) as a WorkerAdapter, a native xAI OpenAI-compat API worker, or both with CLI-first? The CLI matches the adapter pattern and the omniroute proxy; the native API unlocks Responses-API loop caps, `parallel_tool_calls`, and server-side tools. | eng | Phase 5 |
| Q43 | xAI auth: console `XAI_API_KEY` only, or also the SuperGrok device-code OAuth flow (OpenCode-style) so consumer Grok/X Premium plans can drive workers without a separate API key? | product | Phase 5 |
| Q44 | Control API transport: token-authed `127.0.0.1` HTTP (curl-testable, script-friendly) as primary with UDS as the app's secure path, or UDS-first with the Tauri Rust core as the sole client? | eng | Phase 5 |
| Q45 | How far does the Grok Bot UX floor go in v1: named persistent identities + approval inbox only, or also teach-once routines and bot-to-bot handoff threads? | product | Phase 5 |
| Q46 | Benchmark coverage: the operator named z.ai's coding-agent UI ("zcode") as a possible second benchmark for the control app's run view alongside Grok Bot (§20.1) — its capabilities are UNRESEARCHED as of 2026-09-04 (no primary sources fetched yet; offline session). Research it first (web fetch of z.ai docs + hands-on), then decide: add a §20.1-style benchmark matrix, or rely on Grok Bot + Orca + the §20.7 prior-art set as sufficient coverage. | product | Phase 5 |
| Q47 | ~~The operator wants a pilot-style TUI dashboard (§20.8 FR-TUI) — what is the right v1 fidelity: read-only status board (metrics + task cards, zero input risk), or interactive (approve/deny hotkeys, attach-to-session jump-in) from day one?~~ Resolved 2026-09-08/09: interactive shipped — approval/dispatch hotkeys landed via FR-HAND-02/07 (#145) and the read surface was polished to the FR-TUI-P bar (PR #268, closes #146); every mutation still goes through the same gate machinery (FR-CTRL-03). | eng | Phase 5 |
> Resolved 2026-08-24: Q1 (ecosystem conventions + `testCommand` override now
> cover npm/Go/Python), Q2 (plain webhooks shipped in Phase 3), Q3 (policy is
> one attempt, then fan-out on failure), Q6 (single-tenant CLI + webhook
> server shipped).
>
> Resolved 2026-08-24 (curation run 2): Q7 — yes; autoMerge now requires a
> green CI check rollup before merging (`evaluateChecks`, PR #17).
>
> Resolved 2026-08-25 (loop 67): Q9 — distilled; lessons injection is bounded
> by line count AND a character budget (`lessonsMaxChars`, default 4000) that
> drops oldest entries whole, newest-first, never splitting a line.
>
> Resolved 2026-08-28: Q10 — `fanout/ingestChildTrails` ships as fixed-size
> ratchet-capped excerpt (PRs #56 + #57, `buildChildTrailsDigest` at
> `src/prompt.ts:226`, 4000-char cap, oldest-first).
>
> Resolved 2026-08-29 (curation run 7): Q4, Q5 — both were tagged "Phase 2" which is shipped; the questions retired without an explicit decision (G2 ships with repo-provided seed fixtures; G4 findings ship with a per-finding `severity` field and block on `high` unconditionally at `src/deps.ts:90` — no config flag today).
>
> Resolved 2026-08-29 (curation run 7): Q8 — rerun budget; a failing candidate gets one flaky rerun and a clean pass outranks a flaky rescue in winner ranking (`src/workers/fanout.ts:74-95`).
>
> Resolved 2026-08-30 (curation run 12): Q22 — yes; archive-and-rebridge
> post-mortems (goal, failure class, last gate excerpt) are now a named
> Phase 4 backlog item, so the ledger-analytics-only status quo is rejected.
>
> Resolved 2026-08-30 (curation run 16): Q21 — hard-gate; `release.yml`
> declares `needs: [test]` (PR #86), with the needs-chain pinned by
> `test/release-workflow.test.ts` as a test-enforced invariant rather than a
> bare workflow edit (loops 57/58 lesson).

---

## 19. Research Appendix

### 19.1 Base-layer analysis: Orca and DeepSeek Harness

**Orca (stablyai/orca)** — Electron-based Agent Development Environment; orchestrates parallel coding agents in git worktrees; ~51k stars.

- More scriptable than its GUI-first surface suggests: orchestration CLI with runs, tasks, dispatch, human decision gates (`gate-create`/`gate-resolve`), supervised workers with fencing semantics, and structured completion reports (`--outcome succeeded|failed --files-modified --report-path`). These primitives map almost 1:1 onto DevAgent's pipeline needs.
- Production-grade worktree hygiene worth copying as a taxonomy: base prefetch, lineage pruning, retirement backfill scans, removal safety fencing.
- Agent-facing Linear skill pattern: JSON CLI + version-matched docs served by the binary + explicit "treat returned fields as untrusted" guardrail.
- `orca serve` provides headless mode, but the CLI still drives an Electron daemon — headless, not library-embeddable.
- **Verdict**: prior art and pattern source; optionally a dev-time cockpit. Wrong foundation for a headless ticket-to-PR service (fast-moving daily releases, 2k+ open PRs, GUI-centric runtime).

**DeepSeek Harness (`@deepseek-ai/dsh`)** — plugin-based agent framework on Cordis ("everything is a plugin"); developer preview with promised breaking changes; ~183k stars.

- Already solves the driver-layer problem DevAgent faces: bundles exist that spawn Claude Code and Codex as subagents, plus an automation-only **ACP** (Agent Client Protocol) JSON-RPC stdio server — a neutral standard worth adopting at the adapter boundary.
- Right abstractions to borrow: durable session-event log with "model-visible means logged" invariant; waterfall event seams for pre/post-execute hooks.
- Zero delivery-pipeline features (no tickets, worktrees, PRs) — adopting it means building all of DevAgent's actual domain anyway, inside someone else's unstable abstractions.
- **Verdict**: watchlist; study its ACP bridge and driver interfaces when designing `WorkerAdapter`.

**Fan-out vs retry synthesis**: single-worker retry is cheap when success probability is high but suffers context contamination across retries; fan-out converts latency into reliability (success ≈ 1−(1−p)^N) at N× cost and needs a deterministic judge (tests). DevAgent policy: one attempt → on test failure, small-N parallel fan-out with varied prompts/workers → tests select exactly one winner → failed legs' logs retained as retry context.

### 19.2 Competitive profiles

**Devin (Cognition).** Archetype "AI software engineer": autonomous cloud agent in its own VM (shell, editor, browser). Strongest ticket-driven player — Slack mentions, Linear/Jira assignment, GitHub events, REST API. Pricing collapsed from $500/mo team plan to consumption: Free / Pro $20 / Max $200 / Teams from $80/org, plus pay-as-you-go ACUs (~$2.25 per 15 active minutes; auto-reloading credits can silently overspend). Documented weaknesses: struggles with complex unfamiliar codebases (the stated reason for repricing), inconsistent results across identical runs, and its own Session Insights classifying >20-ACU sessions as "unhealthy" — an implicit admission that broad tasks burn credits through failure loops. No structured post-build verification beyond repo tests.

**GitHub Copilot coding agent.** Distribution winner. Assign Copilot to an issue or @mention it; it opens a draft PR, commits with progress logs, marks ready for review. Validation runs on ephemeral GitHub Actions runners with network allowlists, read-only repo permissions, branch-scoped short-lived tokens; it cannot approve PRs or trigger workflows. Iterates against CI failures before handoff — but that CI is the repo's own; no opinionated validation layer (no migration checks, no semantic backend review). Requires Copilot Pro/Business/Enterprise.

**OpenHands (All Hands AI).** Open-source challenger (ex-OpenDevin): plan → edit → run shell → test → iterate inside Docker-sandboxed runtimes; issue assignment resolves to PRs on GitHub/GitLab. Managed Cloud or self-hosted (free, LLM costs only); model-agnostic via API keys. Weaknesses: validation depth is whatever the target repo has; self-hosting demands ops investment; quality varies sharply by model.

**Factory Droid.** Enterprise-oriented; broadest execution topology (CLI, VS Code, Slack, PR comments/labels, Linear/Jira, API/webhooks); multi-model routing per task; parallel Droids; spec-driven workflows; Droid Computers (managed cloud dev environments or BYOM) for private-network/Fortune 500 use. Weaknesses: rolling rate limits can exhaust quota mid-task below Enterprise tier; sales-led pricing; validation is "run repo's tests" plus audit logs — no domain-specific correctness checks.

**Google Jules.** Async Gemini agent bound to GitHub: clones into a private Google Cloud VM, presents a step-by-step plan first, executes on its own branch, PRs only after approval. CLI + API companions. Pricing rides Google AI subscriptions (free 15 tasks/day; Pro ~$19.99/mo 100 tasks/day; Ultra 300/day). Beta-labeled, individual-account gating, English-only support, capacity not guaranteed, task-count pricing incentivizes shallow tasks.

**OpenAI Codex cloud agent.** Woven into ChatGPT: cloud tasks in isolated containers preloaded with the repo produce diffs/PRs; triggered from ChatGPT, CLI, IDE, SDK, GitHub; parallel execution; includes code review and Slack integrations. Credit-metered within ChatGPT plans (5-hour rolling windows), extendable by purchased credits; API-key fallback. Opaque variable credit consumption ("similar tasks consume different amounts"); limits shared across agentic features; no structured validation beyond container test runs.

**Claude Code GitHub Actions / background agents.** Framework more than product: `claude-code-action` runs Claude Code in repo workflows (@claude mentions, any event trigger), built on the Agent SDK; autonomously responds to CI failures and review comments; explicit security posture (actor verification, least-privilege App permissions). Web/mobile background sessions plus scheduled routines round it out. DIY burden: you own workflow YAML, runner environment, and validation wiring.

Sources: [devin.ai/pricing](https://devin.ai/pricing/) · [TechCrunch on Devin pricing](https://techcrunch.com/2025/04/03/devin-the-viral-coding-ai-agent-gets-a-new-pay-as-you-go-plan/) · [Copilot coding agent docs](https://docs.github.com/en/copilot/concepts/agents/copilot-coding-agent) · [OpenHands](https://github.com/All-Hands-AI/OpenHands) · [factory.ai/pricing](https://factory.ai/pricing) · [Droid Computers](https://factory.ai/news/droid-computers) · [Jules usage limits](https://jules.google/docs/usage-limits/) · [Codex pricing](https://developers.openai.com/codex/pricing) · [ChatGPT plans](https://openai.com/chatgpt/pricing/) · [Claude Code GitHub Actions](https://code.claude.com/docs/en/github-actions)

### 19.3 Headless CLI orchestration

**G4 evidence base.** Dynamic ground truth: Go race detector (TSan-based happens-before, zero false positives, coverage-bound, 5–10x memory overhead). Static: Meta Infer RacerD (compositional inter-procedural detection, incremental CI re-analysis, caught 2500+ issues pre-production at Facebook; annotation-free operation drove adoption). LLM state of the art: ConSynergy hybrid pipeline (LLM chain-of-thought cross-thread reasoning → SMT verification) reaching precision 80% / recall 87.1% on DataRaceBench and related benchmarks; consistent finding across the literature that pure-LLM interleaving reasoning underperforms specialized tools and hybrids win.

**Claude Code headless (`claude -p`).** Verified flags: `--output-format text|json|stream-json` (JSON result includes `session_id`, `total_cost_usd`, per-model usage; `stream-json` emits NDJSON events ending in `type:"result"`, with retryable-failure categories via `system/api_retry` events); `--json-schema '<schema>'` for validated structured output; `--permission-mode default|acceptEdits|plan|auto|dontAsk|bypassPermissions`; `--allowedTools` / `--disallowedTools` allow/deny lists; sessions via `--continue` / `--resume <id>` (cross-directory since v2.1.223); budget via `--max-turns <n>` (errors with subtype `error_max_turns` when reached); plus `--model`, `--fallback-model`, `--append-system-prompt`, `--mcp-config`, and `--bare` for fast deterministic CI starts (skips hooks/skills/plugins discovery). Hooks system (`PreToolUse`, `PostToolUse`, `Stop`, `PermissionRequest`, etc.) enables programmatic permission decisions via JSON output. Version-dependent behaviors to pin: `--json-schema` validation (v2.1.205), `plugin_errors[]`/`mcp_server_errors[]` CI-gate fields (v2.1.219+).

**OpenCode headless (`opencode run`).** One-shot execution with `--format json` (raw JSON events), `-m provider/model`, `-c/--continue`, `-s/--session <id>`, `--agent <name>`, and `--auto` to auto-approve permissions not explicitly denied — the orchestrator escape hatch. Strongest programmatic surface of the two CLIs is `opencode serve`: headless HTTP server (OpenAPI 3.1 at `/doc`) with full REST session CRUD/fork/abort/diff, a permission-answer endpoint, SSE event streams, and `--attach <url>` on `run` to reuse a warm server (avoids cold boot per run). Also ships an ACP stdio server and GitHub Actions mode. Permissions resolve per action (`read`, `edit`, `bash`, `task`, ...) to allow/ask/deny with wildcard patterns, last-match-wins; `.env` reads denied by default.

**Orchestration patterns (community practice).** Git worktrees per worker are the standard isolation primitive (sub-second creation, shared object store) — they prevent filesystem collisions, not logical conflicts, so pair with ownership boundaries over shared files (lockfiles, migrations, contracts). Task claiming via lease + heartbeat so crashed workers' tasks reassign. Verification gates must be run by the supervisor independently of agent claims; failed verification reassigns rather than trusts completion reports. Parse worker stdout as NDJSON line-by-line; enforce budgets via `--max-turns` plus wall-clock kill; retry only on retryable error categories (`rate_limit`, `overloaded`, 5xx) with backoff. Cost heuristics: no agents for <5-min work, read-only exploration before implementation, stop after two failed repair attempts.

Sources: [Claude Code headless](https://code.claude.com/docs/en/headless) · [CLI reference](https://code.claude.com/docs/en/cli-reference) · [Hooks](https://code.claude.com/docs/en/hooks) · [Settings](https://code.claude.com/docs/en/settings) · [GitHub Actions](https://code.claude.com/docs/en/github-actions) · [OpenCode CLI](https://opencode.ai/docs/cli) · [OpenCode server](https://opencode.ai/docs/server) · [OpenCode config](https://opencode.ai/docs/config) · [OpenCode permissions](https://opencode.ai/docs/permissions) · [Parallel agents in isolated worktrees (amux)](https://amux.io/blog/parallel-agents-isolated-worktrees/) · [claude-code-action](https://github.com/anthropics/claude-code-action)

These findings refine section 9's adapter table: Claude Code budget control = `--max-turns` + wall clock; OpenCode permission bypass = `--auto`; both emit parseable JSON event streams; OpenCode additionally offers the serve-based REST surface as a future alternative transport.

### 19.4 Migration-safety tooling and techniques

**Tool landscape.**

- **Squawk** (Postgres SQL linter, Rust; CLI + GitHub Action): lock/blocking rules (`require-concurrent-index-creation`, `changing-column-type`, `constraint-missing-not-valid`, `disallowed-unique-constraint`), timeout hygiene (`require-lock-timeout`, `require-statement-timeout`), data loss (`ban-drop-table/column/database`, `ban-truncate-cascade`), client-breaking (`renaming-column/table`, `prefer-text-field`, `prefer-timestamptz`, `prefer-identity`).
- **Atlas (Ariga)**: declarative schema diff, drift detection vs live DB, and `atlas migrate lint` which replays the migration directory on a dev DB and runs analyzers — `destructive` (fails CI), `data_depend` (succeeds locally, fails depending on table contents), `incompatible` (renames/drops breaking rolling deploys), `non_linear` (edited/rebased applied migrations). Per-check error/skip policy in `atlas.hcl`; `force = true` makes checks non-bypassable.
- **migra**: original deprecated; maintained successor adds pg_dump input, JSON output with risk classification, GitHub Action, AI explain modes. Lesson: DDL-parsing vs live-introspection is a key design fork.
- **Flyway/Liquibase validate**: history/structure integrity only (checksums, changelog well-formedness) — not semantic safety. Useful as an additional gate, insufficient alone.
- **Prisma**: `migrate diff` between schema sources with `--exit-code` as a pure diff gate; the **shadow database** replays full migration history into a throwaway DB to detect drift and proactively evaluate generated SQL for data loss. Canonical implementation of "validate against production-like state."
- **django-migration-linter / strong_migrations**: static classification of backward-incompatible operations; strong_migrations' danger criterion ("blocks reads/writes more than a few seconds after acquiring a lock") is the right severity bar.

**Dangerous-pattern taxonomy** (synthesis of the above; drives gates G2/G3):

| Category | Patterns |
|---|---|
| Long locks / write blocking | non-concurrent index ops; ALTER COLUMN TYPE rewrite; volatile column defaults forcing rewrite; SET NOT NULL full scan; inline-validated FK/check constraints |
| Lock convoy | missing lock_timeout/statement_timeout on DDL sessions; migration queues behind long-held locks and freezes traffic |
| Data loss | DROP TABLE/COLUMN/DATABASE; TRUNCATE CASCADE; dropping NOT NULL; truncating type casts |
| Client breaking (rolling deploy) | column/table renames, removed enum values, breaking type changes while old code still runs |
| Data-dependent failure | constraint/index creation that succeeds locally but fails on dirty production data |
| Transaction hazards | CONCURRENTLY inside transaction; huge backfills inside migration transactions; leftover INVALID indexes from failed concurrent builds |
| History divergence | editing/deleting applied migrations; non-linear migration directories after branch merges |

**Validation techniques.**

- Shadow-DB replay (Prisma-style): replay the full migration chain into a throwaway DB seeded with representative data; diff end-state vs expectation; evaluate delta for data loss.
- Expand-contract as the enforcement model for risky changes: expand (additive: nullable column, NOT VALID constraint) → migrate (dual-write + batched backfill ~10k rows with replica-lag monitoring) → contract (drop old shape only after zero-reference grace period). Gate-based checkpoint sequencing where only the final irreversible step is gated hardest.
- Tooling exemplars: pgroll (versioned Postgres schemas via search_path, trigger-based data copy, trivial rollback); PlanetScale deploy requests (copy-table cutover with ~30-min revert window — and their admitted gap: no referential-integrity validation when adding FKs, an opening for DevAgent's deeper checks).

Sources: [Squawk rules](https://squawkhq.com/docs/rules) · [Atlas analyzers](https://atlasgo.io/lint/analyzers) · [migra](https://github.com/djrobstep/migra) · [strong_migrations](https://github.com/ankane/strong_migrations) · [django-migration-linter](https://github.com/3YOURMIND/django-migration-linter) · [Flyway validate](https://documentation.red-gate.com/flyway/reference/commands/validate) · [Liquibase validate](https://docs.liquibase.com/commands/utility/validate.html) · [Prisma CLI](https://www.prisma.io/docs/orm/reference/prisma-cli-reference) · [Prisma shadow DB](https://www.prisma.io/docs/orm/prisma-migrate/understanding-prisma-migrate/shadow-database) · [PlanetScale deploy requests](https://planetscale.com/docs/concepts/deploy-requests) · [pgroll](https://github.com/xataio/pgroll) · [Expand-and-contract methodology](https://www.zero-downtime-schema.com/zero-downtime-schema-evolution-patterns/expand-and-contract-methodology/) · [ConSynergy (MDPI 2025)](https://www.mdpi.com/1999-5903/17/12/578) · [RaceBench artifact (2026)](https://doi.org/10.5281/zenodo.20242300) · [Infer RacerD](https://fbinfer.com/docs/checker-racerd/) · [Go race detector](https://go.dev/doc/articles/race_detector) · [typescript-eslint no-floating-promises](https://typescript-eslint.io/rules/no-floating-promises)

### 19.5 Knowledge-graph-grounded context
A research note (`docs/research/2026-08-30-devagent-leankg-value-in-harness-era.md`) evaluated whether the local `leankg` MCP (FreePeak build, [github.com/FreePeak/LeanKG](https://github.com/FreePeak/LeanKG), Postgres-backed) adds value to DevAgent's harness. Verdict: yes, as the structural complement to the harness's durable state (lessons digest, childTrails digest, worklog, ledger). Live inventory at 2026-08-30 was 358,359 elements and 1,867,483 relationships across 38,597 files; `mcp_status` ok but `kg_semantic_context` timed out at 30s — the measurement behind the v1 non-semantic constraint, and the exact failure mode LeanKG's v4.3.0 ladder (branch `docs/prd-v4.3.0-ladder-setup`, 8d15c78f) removes: a single router tool (`leankg_context`) probes per-project capabilities in <10ms and routes L3 vectors (ANN + rerank + traverse) → L2 FTS/`pg_trgm` fuzzy + ontology → L1 exact + regex → L0 cold guidance with a background auto-index kick, never erroring; every response carries `retrieval: {rung, reason}` + `freshness: fresh \| possibly_stale \| cold`. DevAgent therefore consumes LeanKG as the degradation boundary (FR-CTX-05: no hand-rolled ladder here) and treats `freshness` as prompt-visible provenance so gates judge evidence quality. Zero-config v4.3.0 goals (FR-ZCP-01/02: cwd-based resolution, lazy auto-attach + background first index; embeddings optional) align with DevAgent's own zero-dependency posture: the local markdown baseline (FR-CTX-02) is the always-on default and LeanKG the enhancement tier above it. Integration seam is the existing `COMPACT_CONTEXT_MARKER` ratchet (`src/prompt.ts:303-317`); the context digest joins `lessons` and `childTrails` under the same 4,000-char cap. Scope: orchestrator-side only, opt-in (`devagent.context.kg: "leankg" \| "off"`, default `off`), never reaches a worker adapter. Cross-workspace routing rule (freepeak → `leankg`; BE → `be-knowledge-graph`; never both) stays PRD prose (`skill://leankg-routing` is not materialized — create the skill at implementation time). See FR-CTX-01..05, Phase 4 sub-bullet "Knowledge-grounded context", and Q28.

### 19.6 Pilot probe (2026-09-04)

A competitive probe of qf-studio/pilot (Go ticket-to-PR autopilot, 665★, BSL 1.1 —
`docs/research/pilot-probe.md`) confirms the §20 direction: Pilot ships the exact surface
DevAgent specifies but hasn't built (cross-platform desktop app from releases, live
token/cost dashboard, failure/cost/stuck alerting via Telegram/Slack/Email briefs).
Actionable imports: the alerting model resolves Q41's no-notification-surface gap;
the dashboard card layout (current-task phase %, queue depth, budget-vs-spend) is a
template for FR-UI-08. Verdict recorded there: continue DevAgent — Pilot is
Claude-Code-locked with generic gates and no independent auditor, while DevAgent's
domain gates, evidence-gated orchestration, BYO-provider adapters, and eval-scored
lessons are the differentiators. BSL 1.1 forbids copying code into this MIT repo.


### 19.7 Internet scout synthesis (2026-09-07)

A five-scout parallel web research pass (open-source harnesses, self-improving
loops, multi-agent orchestration, verification/evals/sandboxing, context
engineering + 2026 competitive scan; ~90 fetched sources) distilled into a
15-item prioritized adoption backlog mapped to DevAgent surfaces:
`docs/research/2026-09-07-internet-scout-synthesis.md` with the five full
scout docs beside it (`docs/research/2026-09-07-*-scout.md`). Headline
convergence: harness quality is the product (durable state = moat);
verification depth stays the white space; gates that explain themselves beat
gates that merely block; context/caching behavior becomes a reported metric.
Top five backlog items: sandboxed gate subprocesses (closes
PRODUCTION-READINESS #1), Squawk/Atlas G3 engines + migration-history hashing,
bors-style batched merged-result oracle (PRD:885), sentinel worker exits +
format-error breakers, synthetic-bug gate calibration.

### 19.8 Competitor deep scout: how the field builds the automatic dev workflow (2026-09-11)

Eight-scout parallel web pass (~290 primary sources) profiling the workflow
mechanics of every competitor cluster: Devin/Cognition, GitHub Copilot + OpenAI
Codex, open-source harnesses (OpenHands/SWE-agent/Agentless/Aider), enterprise
platforms (Factory/Cursor/AWS Kiro/Amp), Google (Jules/Gemini CLI/Antigravity),
the Claude Code substrate, the ticket→PR/review-loop long tail (Sweep dead,
Codegen→ClickUp, Qodo review-only), and 12 cross-vendor workflow patterns:
`docs/research/2026-09-11-competitor-scout-synthesis.md` + eight per-cluster docs.
Moat deltas: migration white space narrows to *execution-based* validation only
(CodeRabbit ships Squawk statically; nobody runs shadow-DB/reversibility/FK
gates); evidence-per-PR and cache-aware orchestration are now competitor-shipped;
local-first is attacked by Devin Outposts/air-gapped Factory; the
orchestrator-over-CLIs pattern became a product category (Ellipsis Agent Cloud,
Greptile /greploop). Highest-value imports: schema-required worker exits, hooks
as external-gate contract on worker CLIs (Gemini/Copilot/Factory), event-sourced
evidence artifacts (Jules bashOutput shape), documented reviewer→worker retry
caps, path-triggered deterministic rule injection (OpenHands V1), readiness as
intake gate (Factory), pre-sandbox budget enforcement (Ellipsis).

---


## 20. Product Direction Addendum: Grok Bot, xAI Integration, Cross-Platform Control App

> Added 2026-09-03 (operator direction + deep research); cross-platform scope widened 2026-09-04; §20.8 visible sessions + TUI added 2026-09-04. DevAgent should become the local-first, private, BYO-provider counterpart to [Grok Bot](https://x.ai/bot) — a team of named AI teammates dispatched from a lightweight desktop control app (macOS menubar/tray; Windows and Linux system tray) — and should integrate Grok/xAI as a first-class worker provider. This section records the benchmark, the integration requirements, the app design, and the terminal visibility layer; it feeds Phase 5 in §17 and Q42–Q47 in §18.

### 20.1 The benchmark: Grok Bot (x.ai/bot)

`x.ai/bot` is **Grok Bot** — xAI's "AI teammates that finish the work" product (early beta, built with Cursor; download served from `api2.cursor.sh`, sales via `cursor.com`). It is not an X-bot platform. What it promises, and what DevAgent should match locally:

| Grok Bot capability | Local-first analog in DevAgent |
|---|---|
| Named, persistent Bots ("Chief of Staff", "Bug Reproduction") with their own VM | Named role agents — the existing roles (scout/worker/reviewer/curator/PO) promoted to persistent, titled identities with durable memory (lessons digest, ledger history) |
| Bots work in parallel, pass work between themselves in shared threads | Parallel fan-out workers (FR-IMPL-03) + visible bot-to-bot handoff in a shared run thread instead of user-as-router |
| Teach-once routines: watch a workflow once, replay on schedule | Recorded routines: capture a dispatch's prompt/role/tool set as a named, schedulable routine |
| Approval gates — "only come back when something needs your approval" | Existing approval gates surfaced as a cross-surface approval inbox (menu bar, notifications) |
| Message-the-teammate interaction (desktop + iOS) | Dispatch-by-prompt from the desktop control app (v1); chat-like thread per agent run |
| Bots "get smarter over time" | Lessons feedback loop ships (PR #39, eval-ranked per Q39); childTrails digest ships (PR #57); KG context digest specified (FR-CTX-01..05) but not yet implemented — local markdown baseline + opt-in LeanKG |

Grok Bot is cloud-hosted and Cursor/XAI-plan-gated ($20+/mo, shared per-account "computer"). DevAgent's wedge: the same teammate UX, **local, private, BYO-model**.

### 20.2 Grok/xAI integration (FR-GROK)

xAI's agent-relevant surface (docs.x.ai, verified 2026-09-03):

- **OpenAI-compatible REST** at `https://api.x.ai/v1` with `XAI_API_KEY`; stateful `/v1/responses` (`previous_response_id`, `max_turns`, WebSocket mode) plus `/v1/chat/completions`.
- **Models**: `grok-4.6` (500k ctx, $2/$6 per 1M in/out, reasoning effort low–xhigh), `grok-build-0.1` (256k ctx, $1/$2, coding), `grok-4.3` (1M ctx, $1.25/$2.50, batch-eligible). Cached input ≈ 25% of input price — **`prompt_cache_key` / `x-grok-conv-id` sticky routing is "highly recommended"**; cache-cold retries pay full input price.
- **Every response's `usage.cost_in_usd_ticks`** gives exact per-request USD cost — free ledger cost accounting, no price tables to maintain.
- **Tool calling**: ≤128 schema-strict functions, `parallel_tool_calls` supported; SSE streaming delivers **tool calls whole, not token-streamed**; `reasoning_content` deltas appear on reasoning models.
- **Server-side tools** (`web_search`, `x_search`, `code_execution`, remote MCP) bill ~$5/1k calls — opt-in per worker role only.
- **Batch API** 20% off (grok-4.3/4.20 family) for scheduled/nightly runs; rate limits tier by spend; 429s distinguish RPS vs TPM (a 500k-ctx prompt can eat TPM in one shot; cached tokens still count).
- **Grok Build CLI** (`grok`, install `curl -fsSL https://x.ai/cli/install.sh | bash`): headless `grok -p --output-format streaming-json` (NDJSON events — same adapter pattern as omp), `~/.grok/config.toml` `[model.*]` with `base_url`/`env_key` so devagent's omniroute proxy plugs in directly, and ACP support.
- **No first-party Anthropic-compatible endpoint** — Claude-Code-style harnesses reach Grok via the OpenAI-compat surface, LiteLLM, or a gateway. Model ids: only provider-qualified/exact slugs forward (omp lesson, PR #92).

| ID | Requirement | Pri |
|---|---|---|
| FR-GROK-01 | Ship a `grok` WorkerAdapter following the omp pattern: NDJSON streaming-json parser, `--no-prewalk`-class hardening as discovered, per-adapter no-progress watchdog, resume support | S |
| FR-GROK-02 | Extend the model-id predicate registry (`src/workers/model-id.ts`) for the xai provider: exact slugs (`grok-4.6`, `grok-build-0.1`, `grok-4.3`, dated pins) and `xai/`-prefixed ids; reject unqualified aliases | M |
| FR-GROK-03 | Record xAI `usage.cost_in_usd_ticks` verbatim in the run ledger for every grok worker run, enabling exact per-loop cost analytics | S |
| FR-GROK-04 | Set a per-task prompt-cache key (`prompt_cache_key` / `x-grok-conv-id`) on xAI-backed worker sessions so cache hits stick within a task's retry loop | S |
| FR-GROK-05 | Route scheduled/off-peak grok dispatches through the Batch API when the model family supports it; keep interactive dispatches on streaming | C |
| FR-GROK-06 | Transient-error classification covers xAI 429 (RPS vs TPM separately), 5xx, and stream-tool-call-whole shapes; fallback chain within xAI (`grok-4.6 → grok-4.3 → grok-build-0.1`) before cross-provider fallback | S |

Positioning note: Grok Bot is added to §4 competitive landscape as the consumer-agent archetype (cloud "own computer", plan-gated, shared per-account isolation). DevAgent does not compete on a cloud computer; it competes on local control, validation depth (§11 gates), and BYO-provider freedom — Grok being one first-class provider among several.


### 20.3 Daemon control API (FR-CTRL)

Prerequisite for the desktop control app: a machine-local control surface the app (and any script) can drive. Builds on the existing `serve` webhook server (`src/cli.ts:216`) — same process pattern, new endpoints, no new daemon process:

| ID | Requirement | Pri |
|---|---|---|
| FR-CTRL-01 | Local control server serving: `GET /status` (aggregate loop state), `GET /agents` (roster + per-agent state), `POST /dispatch` (prompt, role, tools, repo, worker, budget), `POST /approve` (gate decisions), `GET /events` (SSE: agent state changes + log tails), `GET /history` (ledger queries). Issue #315: `runs.failed_recent` is a **windowed** count (failed rows whose terminal write is inside the last 24h), not the all-time queue-failed count that pinned the tray at failed over a 17-day-old row | M |
| FR-CTRL-02 | Auth: per-boot bearer token written `0600` to `DEVAGENT_HOME/daemon-token`; bind `127.0.0.1` only; validate `Host` (DNS-rebinding) and `Origin` headers (drive-by CSRF — the Vibe Kanban `ALLOWED_ORIGINS` 403 pattern) | M |
| FR-CTRL-03 | Every dispatch through the control API goes through the same pipeline/budget/gate machinery as CLI dispatch — the API is a transport, not a bypass. FR-HAND-03 (#145): `POST /dispatch` accepts `autoPr` (TUI sends true; scripts may pass false) and threads it into the spawned `devagent task --auto-pr` argv only when `GITHUB_TOKEN` is set — without the token the run stays local and the response names the token/PR step as the next action. Issue #315: the request **claims** the queue row it just enqueued (`claimedBy: daemon`, fresh lease) *before* spawning and threads the row id into the child as `--id`, so the worker's run lock/worktree/branch all name the row the dashboard card shows and the selfbuild loop cannot claim the same goal; the **child** releases that claim at exit (`pipeline.FinishDispatchClaim`, fenced on the claim generation — `done` on a clean run else `failed`), which survives a daemon restart, and the TUI queue card follows the row status so a claimed row names its holder instead of "waiting for a worker claim" | M |
| FR-CTRL-04 | Structured events on the SSE stream derive from the existing run-log/ledger JSONL (no second event system); reconnect with `Last-Event-ID` replay | S |
| FR-CTRL-05 | Optional Unix-domain-socket listener (same HTTP code over UDS; Windows equivalent: named pipe) for the Tauri Rust core; filesystem permissions replace tokens on that path | S |

Anti-pattern (explicit non-requirement): the **app** does **not** PTY-wrap worker CLIs. The orchestrator already owns worker processes and emits structured NDJSON/ledger events; UI-level TUI parsing recreates the omp `thinking_delta`/watchdog problems fixed in PRs #93/#94. The terminal visibility layer (§20.8) is the sanctioned exception's home: there the PTY is owned by the terminal multiplexer/herdr server, not parsed by a UI, and the structured event stream stays the dashboard's data source.

### 20.4 Cross-platform desktop control app (FR-UI)

Stack decision (researched against SwiftUI, Electron, Raycast — see §20.7): **Tauri 2 desktop app, cross-platform from day one** (macOS menubar/tray; Windows and Linux system tray). Reasons: TS webview UI matches the repo's single language skillset; Tauri 2 ships the same tray + webview + notification + autostart surface on all three OSes, so FR-UI-01..09 are written platform-neutral with per-OS packaging notes; system WKWebView/WebView2/WebKitGTK keeps bundle ~3–10 MB and idle RAM ~60–120 MB (Electron: ~85–100 MB bundle, 200–400 MB multi-process); built-in signed updater against GitHub Releases `latest.json` on every OS; single-instance plugin; env-var-driven notarization on macOS (App Store Connect API key; ad-hoc signing for local dev). Raycast is rejected as control plane (menu-bar commands are unload-on-finish, no persistent connection, macOS-only); Electron is rejected on footprint + unsigned-macOS-API breakage (Keychain `safeStorage`, login items, Squirrel updater); SwiftUI is rejected as macOS-only.

Prior art mined (§20.7): Omnara (role=profile YAML, approval gates away from terminal), Conductor (workspace-per-task, diff-first review), Sculptor (pairing mode, session replay), Vibe Kanban (task-queue UX, local-server origin security), claude-squad (profiles ≈ roles), Happy (approval pushes + per-session cost). Crystal's deprecation and Vibe Kanban's sunset are scope-discipline warnings: ship small. Orca (§19.1) remains the closest prior art for worktree-centric multi-agent visualization; the operator-named "zcode" (z.ai) benchmark is pending research (Q46).

| ID | Requirement | Pri |
|---|---|---|
| FR-UI-01 | Tray app with aggregate state icon: running / idle / failed (circuit-breaker), driven by `GET /status` polling or SSE; macOS menu bar, Windows + Linux system tray | M |
| FR-UI-02 | Dispatch sheet: prompt text + role picker (existing role configs) + tool/provider toggles + target repo → `POST /dispatch` | M |
| FR-UI-03 | Dashboard window: agent roster with live status; per-agent live log tail (SSE → virtualized list); task history from `GET /history` | M |
| FR-UI-04 | Approval inbox: pending gates with Approve/Deny → `POST /approve`; surfaced as native notifications on each OS (approval-needed, failure, completion) with deep-link into the dashboard | M |
| FR-UI-05 | Launch-at-login per OS: `SMAppService` (macOS 13+), `tauri-plugin-autostart` (Windows registry Run key / Linux `~/.config/autostart`); single-instance enforcement via `tauri-plugin-single-instance` on all OSes | S |
| FR-UI-06 | Signed builds via CI — macOS notarization (`APPLE_API_ISSUER`/`APPLE_API_KEY`), Windows Authenticode, Linux AppImage/`.deb` — auto-update via `tauri-plugin-updater` on GitHub Releases `latest-{platform}.json` | S |
| FR-UI-07 | The app holds no credentials and no business logic: it is a thin client of FR-CTRL; losing it degrades to CLI-only operation. UI parity: macOS is the reference surface; Windows/Linux ship the same v1 feature set through the tray/window shell (no menubar-specific affordances required) | M |
| FR-UI-08 | Pipeline visualization: per-run live DAG view of the flow scout → plan → implement → gates G0–G5 → PR, rendered from the run-log/ledger JSONL via the FR-CTRL-04 SSE event stream; per-stage status (pending/running/pass/fail/skip), per-task stage badge in the agent roster, elapsed + retry counts per stage; static fallback reuses the `dashboard` board model (`src/observe.ts`) | M |
| FR-UI-09 | Cross-platform parity gate: the app builds and smoke-launches on macOS, Windows, and Linux in CI (Tauri's bundler matrix); a platform-specific regression (tray icon missing, updater path, notification permission) blocks release, not a follow-up | S |

### 20.5 Scope boundaries

- **Not a cloud computer.** No hosted VMs, no browser-in-the-cloud sessions; workers execute in local worktrees/containers (§8.2). The "own computer" story stays the user's machine — macOS, Linux, or Windows.
- **Not a chat-first product.** v1 interaction is dispatch + approval, not free-form conversation; chat-like threads per run are a later layer on the control API, never the transport to workers.
- **Not an X-bot platform.** Posting/replying/DM automation on X (X API v2, pay-per-usage, strict ToS: automated labels, approval-gated AI replies, no scraping) is out of scope; if ever added it is a separate integration behind approval gates.
- **No Electron, no Raycast control plane** (§20.4); no multi-device sync, no mobile client in v1 — Happy's relay pattern is the known path if wanted later.
- **No model lock-in.** Grok is one provider behind the WorkerAdapter contract (§9, FR-IMPL-02); nothing in the UI or control API may hard-code xAI.

### 20.6 Open questions resolved into this addendum

- Q42 (CLI vs native API): CLI-first (`grok -p` adapter), native xAI API worker as the follow-up — but both behind FR-GROK-02's model-id predicate so either path is dispatchable per role.
- Q43 (auth): console `XAI_API_KEY` in v1; SuperGrok device-code OAuth tracked as C-priority (matches OpenCode's proven pattern).
- Q44 (transport): token-authed `127.0.0.1` HTTP primary (curl-testable, matches `serve`), UDS as FR-CTRL-05 hardening for the app path.
- Q45 (UX floor): v1 = named identities + approval inbox + dispatch sheet; teach-once routines and bot-to-bot handoff threads are Phase 5 stretch, gated on the control API's event stream proving out.

### 20.7 Research sources (2026-09-03)

Research grounding for §20. Primary sources fetched 2026-09-03.

**Grok Bot (x.ai/bot).** xAI's "AI teammates" product: early beta, built with Cursor (macOS download from `api2.cursor.sh/.../grok-bot-*`, sales via `cursor.com/contact-sales?product=grok-bot`). Persistent named Bots on a shared per-account cloud VM (browser, filesystem, terminal); connectors/MCP plus computer use for non-API apps; teach-once routines; cross-Bot collaboration threads; approval-gated completion ("only come back when something needs your approval"); macOS + iOS surfaces. Distribution: Cursor Pro $20/mo+ or SuperGrok plans. Sources: [x.ai/bot](https://x.ai/bot) · [docs.x.ai/grok-bot/overview](https://docs.x.ai/grok-bot/overview)

**xAI API (docs.x.ai, Sept 2026).** OpenAI-compatible (`api.x.ai/v1`, `XAI_API_KEY`). Models: `grok-4.6` (500k ctx, $2.00/$6.00 per 1M in/out, ≥200k-prompt tier $4/$12, reasoning effort low/med/high/xhigh), `grok-4.5` (500k, $2/$6), `grok-4.3` (1M, $1.25/$2.50, Batch-eligible), `grok-build-0.1` (256k, $1/$2, coding). Cached input ≈ 25% of input price; `prompt_cache_key`/`x-grok-conv-id` sticky routing recommended. `usage.cost_in_usd_ticks` = exact per-request USD. Tools: ≤128 strict-schema functions, `parallel_tool_calls`, Responses-API `max_turns`/`max_tool_calls`; server-side `web_search`/`x_search`/`code_execution` at $5/1k calls; remote MCP token-billed. Streaming: SSE; **function calls arrive whole**. Structured outputs: `additionalProperties` defaults false; constraint caps (minLength/maxLength 2048, items 256); restricted regex subset. Rate limits tier by cumulative spend (T0 $0 → T4 $5k); 429 = RPS or TPM (cached tokens count). Batch API −20% (grok-4.3/4.20 family, ≤24h); Priority tier 2x. `deferred:true` + poll for long tasks. No Anthropic-compat endpoint (page 404s) — gateway/LiteLLM needed for Claude-style harnesses. Sources: [docs.x.ai/developers/models](https://docs.x.ai/developers/models) · [pricing](https://docs.x.ai/developers/pricing) · [function calling](https://docs.x.ai/developers/tools/function-calling) · [structured outputs](https://docs.x.ai/developers/model-capabilities/text/structured-outputs) · [rate limits](https://docs.x.ai/developers/rate-limits) · [prompt caching](https://docs.x.ai/developers/advanced-api-usage/prompt-caching/maximizing-cache-hits) · [release notes](https://docs.x.ai/developers/release-notes)

**Grok in agent tools today.** Aider: LiteLLM `xai/` prefix. OpenCode: 75+ providers via models.dev registry; xAI has two auth paths — console API key or SuperGrok device-code OAuth (auto-refresh; consumer Grok/X Premium plans work without a separate key). Cline: first-class "xAI (Grok)" provider. `grok-cli` (superagent-ai, ~3.4k★): headless `-p --format json` NDJSON stream, `--batch-api` for cheap unattended runs, schedules, sub-agents, MCP, Telegram control. Cross-tool gotchas baked into FR-GROK: per-tool id naming (bare slug vs `xai/` vs dated slugs), `logprobs` silently ignored on grok-4.20+, penalties unsupported on reasoning models, stale-slug risk in registries. Sources: [aider.chat/docs/llms/xai](https://aider.chat/docs/llms/xai.html) · [opencode.ai/docs/providers](https://opencode.ai/docs/providers) · [docs.cline.bot](https://docs.cline.bot/provider-config/other-30-plus-providers#xai-grok) · [github.com/superagent-ai/grok-cli](https://github.com/superagent-ai/grok-cli) · [models.dev/api.json](https://models.dev/api.json)

**X API (if bots on X are ever wanted).** Pay-per-usage credits: post create $0.015 (with URL $0.200), DM read $0.010 / DM interaction $0.015, webhooks $0.005–0.010/event; 3M post reads/mo cap; up to 20% back in xAI credits when linking an xAI team. ToS: "Automated" label mandatory, AI replies need prior X approval, replies only to engagers, DMs only after user DMs first, no scraping (permanent suspension). Sources: [docs.x.com/x-api/getting-started/pricing](https://docs.x.com/x-api/getting-started/pricing) · [developer-guidelines](https://docs.x.com/developer-guidelines.md)

**Desktop app frameworks (cross-platform).** Tauri 2: `tray-icon` feature on all three OSes (macOS menu bar, Windows + Linux system tray); updater plugin (minisign keypair, static `latest.json` on GitHub Releases); signing env-var driven — macOS notarization (`APPLE_CERTIFICATE`, `APPLE_API_ISSUER`/`APPLE_API_KEY`, ad-hoc `-` for dev), Windows Authenticode, Linux AppImage/`.deb` typically unsigned; autostart via `tauri-plugin-autostart` (registry Run key / `~/.config/autostart`). SwiftUI `MenuBarExtra`: native but macOS-only + second toolchain. Electron: Chromium in every install; unsigned macOS builds break `safeStorage`, login items, Squirrel autoUpdater. Raycast: menu-bar commands load on demand and unload after finishing — no persistent daemon connection; macOS-only. Sources: [v2.tauri.app/learn/system-tray](https://v2.tauri.app/learn/system-tray/) · [updater](https://v2.tauri.app/plugin/updater/) · [macOS signing](https://v2.tauri.app/distribute/sign/macos/) · [MenuBarExtra](https://developer.apple.com/documentation/swiftui/menubarextra) · [Electron code signing](https://www.electronjs.org/docs/latest/tutorial/code-signing) · [Raycast menu-bar](https://developers.raycast.com/api-reference/menu-bar-commands.md)

**Prior art (agent-control UIs).** Omnara (omnara-ai, Apache-2.0): agent profiles as YAML, durable state in Postgres, Slack/web approvals, signed+notarized macOS daemon in CI. Conductor (conductor.build): per-task workspace/branch/terminal/diff, review-diff→PR→archive. Crystal (stravu, deprecated Feb 2026 → Nimbalyst): multi-session Claude Code over worktrees, explicit permission dialogs. Sculptor (Imbue, MIT): container agents, Pairing Mode two-way container↔repo sync, session replay. Vibe Kanban (BloopAI, Apache-2.0, ~28k★, sunsetting): local Rust server + React UI, `VK_ALLOWED_ORIGINS` origin-403 security pattern, inline-comment feedback to agents. claude-squad (Go TUI, AGPL-3.0): tmux+worktrees, named profiles. Happy (slopus, MIT): approval push notifications anywhere, per-session cost, Electron desktop. Sources: [omnara](https://github.com/omnara-ai/omnara) · [conductor.build](https://conductor.build/) · [crystal](https://github.com/stravu/crystal) · [sculptor](https://github.com/imbue-ai/sculptor) · [vibe-kanban](https://github.com/BloopAI/vibe-kanban) · [claude-squad](https://github.com/smtg-ai/claude-squad) · [happy](https://github.com/slopus/happy)

**IPC/security.** Recommended: HTTP + SSE over `127.0.0.1` with per-boot `0600` bearer token, `Host` (DNS-rebinding) + `Origin` (drive-by CSRF) validation — Vibe Kanban's proven pattern; UDS via `server.listen(path)` as hardening (browser JS can't reach UDS; Tauri Rust core bridges), named pipe on Windows. gRPC rejected (codegen ×3 languages, grpc-web proxy); PTY-in-UI rejected (§20.3 anti-pattern). Launch-at-login: `SMAppService` (macOS 13+) — `mainAppService` for the app, `agent(plistName:)` for user-scoped LaunchAgents; `tauri-plugin-autostart` covers Windows (registry Run key) and Linux (`~/.config/autostart`); single instance via `tauri-plugin-single-instance` + lockfile/flock daemon-side. Sources: [SMAppService](https://developer.apple.com/documentation/servicemanagement/smappservice) · [single-instance plugin](https://github.com/tauri-apps/plugins-workspace/tree/v2/plugins/single-instance) · [vibe-kanban](https://github.com/BloopAI/vibe-kanban)

### 20.8 Visible worker sessions and terminal TUI (FR-VIS, FR-TUI)

Added 2026-09-04 (operator direction): coding agents should run **open, like when a human
runs them** — the operator can jump into any worker session at any time and see/steer what
the agent is doing — plus a pilot-style full-screen TUI dashboard. Today the factory runs
workers as invisible child processes; the herdr integration (`docs/HERDR.md`,
`src/integrations/herdr.ts`) already provides the right primitive — panes in a persistent
named session, attachable via `herdr session attach devagent`, surviving disconnects and
reboots — but it is opt-in with **silent** fallback to invisible processes. §20.8 inverts
that posture.

**Visible sessions (FR-VIS)** — the "jump in anytime" guarantee:

| ID | Requirement | Pri |
|---|---|---|
| FR-VIS-01 | Worker launches default to visible panes: the herdr runtime becomes the default spawn path when the `herdr` binary is present (config `herdr.enabled` default flips to `true`); fallback to invisible child processes stays automatic but is **loud** — one stderr warning per spawn site plus a `visibility=fallback` field on the run's ledger row, never silent | M |
| FR-VIS-02 | `devagent sessions` lists live worker sessions (task id, role, worker CLI, pane id, workspace, elapsed, agent_status from the herdr pane list) so the operator can find the pane to jump into; `devagent attach <task>` prints/opens the attach command for that pane. **Fixed 2026-09-12 (#317):** herdr's agent status machine never advances for **headless** worker invocations (`omp -p --mode json` via pane-run), so the roster (`/agents` → TUI chips, `devagent sessions`) rendered mid-run workers as "● idle"; `mapPaneState` now mirrors FR-VIS-07's sweep discriminator — an idle/unknown row whose `pane process-info` foreground process is a worker binary maps to `running`, agent_status stays the signal for interactive panes (`internal/herdr/roster.go` `paneState`) | M |
| FR-VIS-03 | Jump-in steering: attaching to a pane puts the operator in the worker's live terminal exactly like a human-run coding agent session — type into it, watch it think, interrupt it. The orchestrator's no-progress watchdog must not fight a human at the wheel: pane-level operator presence (attach time) suppresses auto-kill timers for that run and the ledger records `operator-attached` | S |
| FR-VIS-04 | `pilot start`-style flags on the devagent loop drivers: `--headless` (explicit opt-out for CI/servers/LaunchAgents — the old invisible behavior becomes the named mode) and `--visible` (default); `devagent.json` `spawn.visibility: "visible" \| "headless"` per project with the env override `DEVAGENT_VISIBILITY` | M |
| FR-VIS-05 | CI/factory parity: LaunchAgent-installed roles (scout/builder/tracker/orchestrator) keep running headless by default (no terminal to attach to), but every run they dispatch still lands in an attachable pane — visibility is a property of the **worker run**, not of the loop process | S |

Added 2026-09-05 (operator escalation "every agent in vision mode as DEFAULT"): the §20.8
rollout left three headless gaps — loop research/PO phases ran as bare `omp` child
processes, the scout dispatched through `spawnCli` directly, and the loop drivers'
herdr-sweep closed **in-flight** worker panes (a mid-run pane reports `agent_status:
idle` because pane liveness comes from the done-marker poll, not the agent state
machine). FR-VIS-06..08 close them; FR-VIS-09 removes the double-driver failure mode.

| ID | Requirement | Pri |
|---|---|---|
| FR-VIS-06 | Every agent role dispatches through the pane runtime, not just coding workers: loop research and PO selection run inside herdr panes via `devagent pane-run` (falls back to a direct child only when herdr is unreachable, exit code 3 — loud, never silent); the scout routes its worker spawn through `runWorkerCli` so discovery lands in an attachable pane too | M |
| FR-VIS-07 | Sweep safety: `herdr-sweep` closes only panes whose cwd sits inside `.devagent-worktrees/` (automation-owned; operator scratch panes in the same session are untouchable) AND whose foreground process is not a worker CLI (`pane process-info` distinguishes a live omp/pi/claude/opencode from an idle shell) — an in-flight pane is never sweepable | M |
| FR-VIS-08 | Loop-process visibility: loop-phase events (research/po/task) stay on the SSE stream and TUI cards (FR-VIS-02 surface), with the phase's pane discoverable via `devagent sessions`/`attach` while it runs | S |
| FR-VIS-09 | Single-instance locking: `selfbuild-loop.sh` holds a portable mkdir lock (`.selfbuild/loop.lock.d`, stale-holder recovery via pid liveness) so an orphaned ppid=1 driver and a fresh start can never race loop numbering, ledger writes, or pane sweeps. **Extended 2026-09-07 to queue claims:** the same single-owner guarantee covers task claiming — an atomic `link()` claim lock per task, an incrementing fencing token (`leaseGeneration`/`leaseOwner`/`leaseExpiresAt`), expired leases reclaimable with a generation bump, and complete/fail/requeue writes refused when their token is stale | M |
| FR-VIS-10 | Sweep blast radius from the operator's side (Q23, added 2026-09-07): `herdr.sweep` is the managed-settings-style deny toggle — `enabled` (env `DEVAGENT_HERDR_SWEEP=0|1`) stops the sweep before it lists a pane, `denySessions` names sessions it must never touch even when one is the resolved target, and `orphans` (env `DEVAGENT_HERDR_SWEEP_ORPHANS=0|1`) overrides a loop driver's own `--orphans` either way; on top of that, any pane the FR-VIS-02 roster reports as live or any sweep launched under `DEVAGENT_OPERATOR_ATTACHED` is spared ahead of every other class and reported `operator-attached`. Unset = FR-VIS-07 behavior; invalid config fails closed | M |

**TUI dashboard (FR-TUI)** — pilot's dashboard as the reference layout
(`docs/research/pilot-probe.md` §2; BSL 1.1 — patterns only, no code):

| ID | Requirement | Pri |
|---|---|---|
| FR-TUI-01 | `devagent tui` full-screen terminal dashboard over the FR-CTRL API (HTTP+SSE on 127.0.0.1; works over SSH, on macOS/Linux/Windows terminals; zero browser, zero desktop app dependency) | M |
| FR-TUI-02 | Pilot's card layout as v1: current task + phase + elapsed, queue depth, token/cost today/week vs budget, recent tasks with per-task duration + cost, aggregate run/idle/failed status — all fed by the same structured ledger/JSONL + SSE stream as the desktop app (FR-CTRL-04; no second event system, no PTY parsing) | M |
| FR-TUI-03 | Live log tail view per agent (scrollable, follows the active pane's structured events) and jump hint showing the `devagent attach <task>` command for the selected run — the TUI is the discovery surface for FR-VIS-02 jump-in | M |
| FR-TUI-05 | Single-key ops: `u` upgrade/rollback hint (pilot's pattern), `k` kill run (goes through the same gate machinery as CLI — the TUI is a transport, not a bypass, per FR-CTRL-03), `?` help overlay | C |
| FR-TUI-04 | Interactive v1 scope (Q47): approval hotkeys (approve/deny pending gates via `POST /approve`) + dispatch sheet mirroring FR-UI-02; ship read-only cards first if Q47 resolves conservative. Shipped via FR-HAND-02/07 (#145): `n` opens the one-line goal dispatch sheet, `g` answers a paused 'ask' task (`y`/`n`/free text → `POST /approve`); look/feel polish shipped 2026-09-09 (#146, PR #268 — FR-TUI-P-01..12) | S |

**Conventions-research polish wave (2026-09-09)** — a benchmark pass over how the
majority of professional terminal tools are built (k9s, lazygit, htop, gh-dash,
opencode TUI, Charm/bubbletea+lipgloss canon, dry, cointop, superfile; full notes in
`docs/research/tui-conventions.md`) surfaced eight dependency-free gaps, all shipped:

1. **`NO_COLOR`/`TERM=dumb` degradation** — monochrome palette (structure kept, zero
   color SGR); `NO_COLOR` outranks `COLORTERM=truecolor` (no-color.org + htop `-C`).
2. **PAUSED aggregate + attention banner** — a paused 'ask' gate renders
   `● PAUSED` (amber) and a hero banner `task X paused — [g] answer · [k] kill`;
   approval-needed is never silent (opencode/crush permission attention).
3. **`/` log search** — live prompt, case-insensitive filter over level/stage/
   message/runId, `n`/`N` match walking in filtered-space scroll coordinates,
   Esc clears (lazygit/gh-dash search canon).
4. **Selection-following viewports** — sessions list and worker cards window around
   the cursor with `↑N/↓N hidden` title indicators (lazygit/htop: the selection is
   never allowed to slide off-screen).
5. **OSC-2 window title** mirrors the header aggregate (`devagent — RUNNING`),
   k9s-style glanceability without stealing a row.
6. **Grouped help overlay** — views/act/move/live-log/general sections.
7. **termios `IUTF8`** flag in raw mode; **upgrade overlay reads
   `internal/version`** (release-stamped, no more hardcoded 0.1.0).
8. Contract-pinned in `internal/tui/pro_polish_test.go`; verified end-to-end over a
   PTY against the live daemon (help overlay, live search echo, filtered viewport,
   clean quit) and across the palette ladder (truecolor 2072 B / 16-color 1423 B /
   mono 1236 B frames).

**CloddsBot skin wave (2026-09-09, operator request "make my devagent look like
CloddsBot")** — the visual language was re-skinned after
[CloddsBot](https://github.com/alsk1992/CloddsBot)'s terminal toolkit
(`src/tui/index.ts` + its onboard wizard), presentation only — every state,
keybinding, payload and the mono/truecolor degradation contract are unchanged:

1. **Cyan identity + centered box titles** — `BoxLines` now draws CloddsBot's
   signature `╭──── Title ────╮` (title centered in the rule) with the whole
   frame in the CloddsBot cyan `#56b6c2` (truecolor; ANSI 36 fallback; empty in
   mono), replacing the dim left-gutter title and slate border.
2. **Header tagline** — the title bar composes CloddsBot-style:
   `DevAgent · autonomous backend delivery agent` + state chip; narrow terminals
   drop the tagline instead of clamping the bar.
3. **Onboarding wizard** — `devagent init` renders the CloddsBot onboard
   composition: ANSI-Shadow "DEVAGENT" wordmark (cyan), bold product line + dim
   tagline + dim `·`-separated stats + dim rule, then numbered step chips
   (`bgCyan " 1 "`) with bold check names and ✓/✗/⚠ outcome glyphs; the smoke
   checklist uses the same glyph language. Pinned literals ("DevAgent setup —",
   "git found", "Next: state your goal in one sentence") survive for scripts.
4. **Outcome glyphs** — `StatusGlyph`/`SuccessText`/`WarnText`/`FailText` mirror
   CloddsBot's ✓/✗/⚠/ℹ/● marks; chips keep the `● state` convention.
5. Contract-pinned in `internal/tui/clodds_skin_test.go` (banner shape, centered
   title, glyph map, step chip, tagline width-gating); verified live over a PTY
   (dashboard frame, `init`, `status`) across the palette ladder.

**View-switch render desync fix (2026-09-10)** — pressing `1`/`2`/`3` could
leave the dashboard permanently one-row-off (stale rows from the previous
view persisting under the new one, mixed footer). Root cause was in the
incremental differ, not the key table: with OPOST off a bare `\n` is
LF-only, and drawLocked unconditionally appended `\n` after RenderFrame —
on an all-skip frame (the norm after a view switch, where only the body
changes) the differ's exit cursor already sits one row below the frame, so
that LF landed on the terminal's bottom row and scrolled the alternate
screen, desyncing the diff forever after. Two fixes in
`internal/tui/{frame.go,loopimpl.go}`: (1) RenderFrame erases leftover rows
with absolute positioning (`ESC[<n>;1H` then `ESC[J`) instead of ED from
wherever the row loop left the cursor (a skipped-vs-rewritten last row left
it at different depths — ED-from-cursor could chop a preserved row or leave
stale cells); (2) drawLocked no longer emits the trailing LF, clamps the
frame to `rows-1` clamped DOWN to the real geometry (the old 12-row floor
forced frames taller than short terminals), and hard-caps `len(next)` for
degenerate 2–3-row terminals. Pinned by TestLoopFullHeightIdenticalFrameEmitsNoLF,
TestRenderFrameShrinkErasesFromFirstLeftoverRow and
TestLoopDrawNeverExceedsTerminalRows (cursor-row tracker asserting no move
past the bottom row); all three fail on the pre-fix code. Verified
end-to-end over a PTY against the live daemon: the pre-fix 2→3→1 switch
overflowed the bottom row 3 times in a 10-second session; post-fix, zero.

Boundary with §20.4: the Tauri desktop app and the TUI are two renderers over the same
FR-CTRL API — the TUI is the SSH/headless-server surface, the app is the desktop surface;
neither parses PTYs (anti-pattern in §20.3). The PTY lives in the herdr server (FR-VIS),
which is a terminal multiplexer's job, not a UI's.

## 21. Product Direction Addendum: Simplicity First (FR-SIMPLE)

> Added 2026-09-05 (operator direction): the deciding product quality is **simplicity
for the user** — simple enough and easy to use, everything visualized for human reading,
easy to set up and install, and showing only the needed tools — so that in a few steps
the user states a goal and the agents build the goal product. That is the whole product
promise. This section constrains how every existing and future surface (§12 CLI, §20
daemon + control app, §20.8 TUI/visible sessions) is designed and reviewed; it adds no
new runtime subsystem and does not change the pipeline contract (§8, §10–11).

### 21.1 Principles (every surface, every release)

1. **Few steps to value** — the common paths (state a goal, approve a gate, read the
   result) are reachable in at most three commands or three clicks; anything longer is
   a power-user affordance, never the default path.
2. **Show only what is needed** — prompts, CLI output, and dashboards present the
   current step and the single next action; advanced knobs (budgets, worker selection,
   remote dispatch, model choice) stay discoverable but out of the happy path.
3. **Visualized for human reading** — every user-facing status, gate outcome, and ledger
   view renders in plain language with visual structure (cards, status chips, progress,
   attach hints — the §20.8 card/chip language is the reference); raw JSON/NDJSON is
   opt-in behind `--json` for scripts, never the default presentation.
4. **Easy to set up and install** — from a clean machine to a running factory in one
   guided command that checks prerequisites, captures credentials, and proves itself
   with a verified smoke run; no doc archaeology, no manual env wiring.

### 21.2 Requirements

| ID | Requirement | Pri |
|---|---|---|
| FR-SIMPLE-01 | `devagent init`: guided setup that checks prerequisites (worker CLI present, provider reachable, git + Docker), captures credentials, writes `devagent.json`, and finishes with a verified smoke dispatch (fixture goal → done) printed as a plain-language checklist; success never dumps raw logs | M |
| FR-SIMPLE-02 | Three-step goal path: from zero to a dispatched goal in ≤3 steps (init → state the goal in one sentence → agents run); the default `run`/loop flow asks no question it can answer itself and takes sane defaults for everything optional | M |
| FR-SIMPLE-03 | Human-readable by default: run status, gate outcomes, queue, and ledger render in the §20.8 card/chip visual language on every surface (CLI, TUI, desktop app); machine formats stay available via `--json` | M |
| FR-SIMPLE-04 | Progressive disclosure: at any moment each surface shows the current phase and the one next action (including the `devagent attach <task>` hint from FR-VIS-02); everything else is one keystroke/click away but never required to reach the first PR | S |
| FR-SIMPLE-05 | First-run "where do I look" screen: after init, one view answers what the factory is doing right now, what happens next, and where to look — composed from the existing FR-CTRL status/events API (no second event system, no new tooling to learn) | S |
| FR-SIMPLE-06 | Simplicity regression review: each release walks the four principles over the onboarding path and default surfaces; a change that adds a required step or a required concept is fixed or flagged before release | C |

**Boundary:** simplicity is the default presentation, not a restriction — everything
stays scriptable over FR-CTRL and flags; automation is unaffected.

## 22. Addendum: Full Go Migration (FR-GO)

> Added 2026-09-07 (operator decision): DevAgent's core — CLI, daemon,
> orchestrator, loop drivers, TUI — migrates from TypeScript/Node to a
> **single Go binary**. Full plan of record: master tracker issue
> [#207](https://github.com/FreePeak/devagent/issues/207).

### 22.1 Decision and rationale

The Node implementation was chosen at v0.1.0 (§1 note: PRD:131 left
"Go or TypeScript/Node" open). The migration reverses that choice for the
core runtime:

1. **Memory footprint.** The daemon, TUI, and 24/7 loop drivers are permanent
   residents; Node pays its heap cost around the clock. The worker fleet
   (external agent CLIs) dominates peak memory and is unaffected by the host
   language — but the always-on orchestration surface is pure overhead Node
   pays and Go does not. Baseline measured at FR-GO-01 (Node daemon idle
   RSS 16 MB, CLI cold start 0.11–0.13 s — `docs/GO-BASELINE.md`); Go
   targets: idle daemon RSS ≤ 50 MB, cold start ≤ 0.05 s.
2. **Deployment simplicity.** One static binary replaces the npm-link +
   `tsc` dist flow, which has a documented failure class (stale `dist/`
   after pulls, driver scripts baked at exec). Install becomes: download
   release artifact, run.
3. **Cold-start latency.** Dispatch-path cold starts drop from Node's
   process+heap warmup to a static binary's, tightening the §15 loop-closure
   metric.
4. **Operator alignment.** The surrounding polyrepo is Go-heavy (CLIs, MCP
   servers); one language for the agent harness itself.

### 22.2 Boundary

- **In scope:** `src/**` (CLI, orchestrator, workers, integrations, gates,
  TUI, daemon), the `selfbuild-*` script helpers, the install path
  (`~/.local/bin/devagent` is the release Go binary since FR-GO-16; the Node link survives as `~/.local/bin/devagent-node-legacy` only on the operator machine).
- **Unchanged:** worker CLIs (claude, opencode, omp, pi, grok — external
  processes behind the WorkerAdapter contract, §9); herdr (already Go);
  Docker sandboxing; the ledger/run-log JSONL schemas (the migration
  contract — byte-compatible both directions).
- **Tauri 2 control app (§20.4, #181):** shipped 2026-09-09 (PR #267) as a
  thin FR-CTRL client at `app/` — its core is Rust; the webview UI consumes
  the daemon REST/SSE surface. Whether its UI stack ever moves to a
  Go/Wails stack is a deferred decision gate, recorded in #207.

### 22.3 Strategy: strangler-fig, gated by parity

Phases G0 → G1 → G2 → G3. Each G1 issue is independently mergeable behind
the existing CLI surface. Parity is defined by three gates:

1. **Golden fixtures** — the 99-file vitest suite's fixtures (scout replay,
   adapter streams, config matrices, ledger round-trips) are ported and must
   pass in Go (scoreboard: #206).
2. **Ledger contract** — both implementations read and write
   `.selfbuild/ledger.jsonl` and the run-log JSONL byte-compatibly (FR-GO-04).
3. **Soak** — the selfbuild loop runs N consecutive green iterations
   self-hosting on the Go binary with the Node path as fallback; a ledger-row
   diff between the two is the merge gate for cutover (FR-GO-15).

### 22.4 Requirements

| ID | Requirement | Pri | Issue |
|---|---|---|---|
| FR-GO-01 | Go module scaffold, CI-go workflow (test + golangci-lint, macOS/Linux), baseline RSS/cold-start measurement with a recorded target | M | ✅ [#191](https://github.com/FreePeak/devagent/issues/191) — PR [#208](https://github.com/FreePeak/devagent/pull/208) |
| FR-GO-02 | Full CLI command-surface parity (cobra) + config/credentials/trust loading, verified by a checked-in parity matrix | M | ✅ [#193](https://github.com/FreePeak/devagent/issues/193) — PR [#209](https://github.com/FreePeak/devagent/pull/209) |
| FR-GO-03 | Git layer parity: worktrees + clean-main guard, state branch (bounded network ops), rebase-stack, doc-sync | M | ✅ [#195](https://github.com/FreePeak/devagent/issues/195) — PR [#210](https://github.com/FreePeak/devagent/pull/210) |
| FR-GO-04 | Run logger + JSONL ledger + analytics, byte-compatible schema both directions | M | ✅ [#197](https://github.com/FreePeak/devagent/issues/197) — PR [#211](https://github.com/FreePeak/devagent/pull/211) |
| FR-GO-05 | Worker adapters (claude/opencode/omp/pi/grok) + model-id registry + env scrub/sandbox + no-progress watchdog semantics | M | ✅ [#190](https://github.com/FreePeak/devagent/issues/190) — PR [#217](https://github.com/FreePeak/devagent/pull/217) |
| FR-GO-06 | Scout + `--replay` golden suite + research extractor (abort/empty paths) + prompts/planner | M | ✅ [#192](https://github.com/FreePeak/devagent/issues/192) — PR [#214](https://github.com/FreePeak/devagent/pull/214) |
| FR-GO-07 | Orchestrator: scheduler, executor, queue + bridge, board recovery, merge, autopr + CI-fixer, resilience, lessons | M | ✅ [#194](https://github.com/FreePeak/devagent/issues/194) — PRs [#221](https://github.com/FreePeak/devagent/pull/221) + [#222](https://github.com/FreePeak/devagent/pull/222) + batch C [#226](https://github.com/FreePeak/devagent/pull/226) |
| FR-GO-08 | Gates G0–G5 incl. STRIDE allowlist + blocking, regression oracle | M | ✅ [#196](https://github.com/FreePeak/devagent/issues/196) — PR [#215](https://github.com/FreePeak/devagent/pull/215) |
| FR-GO-09 | Integrations: Linear (thin GraphQL client), Jira, GitHub/GitLab, webhooks (HMAC, dedup), rate limits | M | ✅ [#199](https://github.com/FreePeak/devagent/issues/199) — PR [#212](https://github.com/FreePeak/devagent/pull/212) |
| FR-GO-10 | herdr integration: panes, sessions/attach, orphan-aware sweep with test seams | M | ✅ [#201](https://github.com/FreePeak/devagent/issues/201) — PR [#213](https://github.com/FreePeak/devagent/pull/213) |
| FR-GO-11 | TUI + dashboard HTML + card/chip language at the #146 polish bar | M | ✅ [#198](https://github.com/FreePeak/devagent/issues/198) — PR [#216](https://github.com/FreePeak/devagent/pull/216) (pure-Go renderer; bubbletea dropped per the no-new-deps contract) |
| FR-GO-12 | Control API + SSE in Go (supersedes the Node implementation half of #179) | M | ✅ [#200](https://github.com/FreePeak/devagent/issues/200) — PR [#224](https://github.com/FreePeak/devagent/pull/224) (FR-CTRL-01..05; daemon command wiring at cutover) |
| FR-GO-13 | Loop driver port (selfbuild-loop + state/queue helpers) with a recorded bash-vs-Go decision gate | M | ✅ [#202](https://github.com/FreePeak/devagent/issues/202) — PR [#225](https://github.com/FreePeak/devagent/pull/225); per DECISION.md, bash stays production until the FR-GO-15 soak survives one full live iteration |
| FR-GO-14 | Windows path: cross-compile, Task Scheduler automation, named-pipe UDS equivalent (NFR-05) | C | ✅ [#203](https://github.com/FreePeak/devagent/issues/203) — PR [#218](https://github.com/FreePeak/devagent/pull/218) (windows-cross + windows-build CI green; pipe listener is a documented stub; Windows named-pipe coverage is a follow-up now that the Node daemon is retired (#205)) |
| FR-GO-15 | Cutover: production entrypoint flips to Go, release artifacts, self-hosting soak gate | M | ✅ [#204](https://github.com/FreePeak/devagent/issues/204) — shipped 2026-09-08: soak iterations 169 (Go driver + Go CLI pane dispatch, byte-parity `ok` row) and 171 (task→publish→merge end-to-end, PR #242) on the live loop; publish/close integrity fixed (#238: `taskPublishStage` publishes from the surviving run branch, `no-pr` non-productive row instead of closing the issue); `SELFBUILD_TEST_CMD` seam (#230, PR #237); release pipeline publishes Go binaries on 5 targets (PR #232 — Release v1.0.0 was the first all-Go release); install flow = release binary (PR #231: README install, Node deprecation banner `DEVAGENT_SUPPRESS_DEPRECATION=1`, scout LaunchAgent plist runs the Go-resolved binary); production `~/.local/bin/devagent` now the Go binary, `devagent daemon` wired to internal/daemon (PR #243, last exit-3 stub blocking cutover) |
| FR-GO-16 | Node retirement: delete src/test/dist/package.json, docs rewritten, single-language CI | C | ✅ [#205](https://github.com/FreePeak/devagent/issues/205) — shipped 2026-09-08: Node tree deleted (PR #240: src/ 106 files, test/ 110, package.json, tsconfig, vitest, ci.yml, selfbuild bash/mjs helpers; Node-dependent Go behavior re-based — lessons guard, consume self-update, TUI upgrade, herdr sweep ancestry, Makefile targets on `devagent-go`); Go-only CI + single-implementation grep gate, Go-only release pipeline (PR #235); all non-PRD docs rewritten to Go reality (PR #236); #206 tracks the vitest-coverage port to Go |

Test-parity scoreboard across all of the above: [#206](https://github.com/FreePeak/devagent/issues/206).
Master tracker with definition of done: [#207](https://github.com/FreePeak/devagent/issues/207).

## 23. Addendum: Driver Validation (FR-VAL)

> Added 2026-09-10 (operator goal): continuous, evidence-backed proof that the
> selfbuild loop driver **works perfectly** — not only that its unit tests
> pass. Motivated by live findings from the 2026-09-10 operation session:
> a starvation-halted loop with 133 hollow restarts, a wedged post-halt
> driver, an idle-showing TUI during real work (#287), invisible worker
> spawns (#288), and a no-progress watchdog that never fired on the
> headless spawn path. Requirements were scoped against external prior art
> scouted from the internet (SWE-bench end-to-end evaluation; AWS
> Well-Architected REL12-BP04 + Azure fault-injection chaos practice;
> OpenTelemetry GenAI semantic conventions for uniform agent telemetry;
> LLM-as-a-judge rubric + drift-regression eval practice for FR-VAL-05).

### 23.1 Requirements

| ID | Requirement | Pri | Issue |
|---|---|---|---|
| FR-VAL-01 ✅ | Golden soak self-test: a hermetic CI test runs the full loop driver end-to-end (pick → research → PO → task → gate → ledger) over a fixture repo with fake worker CLIs, asserting N consecutive `ok` ledger rows, correct artifact creation, already-shipped guard behavior, and failure-path classification — shipped as `TestGoldenSoak` in `internal/loopdriver/run_test.go` (issue-first path over the existing `installFakes` fixture; the deterministic `GH_ROTATE_STATE` fake gh serves pick N as issue #200+N): 3 consecutive `ok` rows with goal artifacts under `.selfbuild/`, the 4th re-pick rejected as `skipped` by the already-shipped guard (no 4th task dispatch, guard-side issue close), and the repo-gate failure pin (`TestCmd=false` → `failed-tests` row, tracker issue left open). Runs in CI via the existing `go test ./...` job — mutation-checked: breaking the ledger row writer or the issue pick order turns it red | M | [#289](https://github.com/FreePeak/devagent/issues/289) |
| FR-VAL-02 ✅ | `devagent doctor`: one-command machine validation (version stamp, config, DEVAGENT_HOME, git remote, gh auth incl. invalid-env-token detection, herdr session, daemon /status, provider preflight, stale artifacts) with human + `--json` output for the TUI/Tauri app | M | ✅ [#290](https://github.com/FreePeak/devagent/issues/290) — shipped as PR [#298](https://github.com/FreePeak/devagent/pull/298) (`internal/commands/doctor.go` + `internal/cli/actions_doctor.go`, merged 2026-09-10T16:37Z); status row corrected by issue #301's work, which measured the row still open after the merge |
| FR-VAL-03 ✅ | Driver observability parity: truthful `runs.active` (closes #287), visible worker panes via wired `HerdrPaneRunner` (closes #288), periodic watchdog-health rows + enforced no-progress kill on ALL spawn paths, and a loopdriver heartbeat (`{iteration, phase, pid}`) surfaced on `GET /status` | M | ✅ [#291](https://github.com/FreePeak/devagent/issues/291) — shipped 2026-09-11: run lock acquired in `taskCommand` before `RunTask` (live-smoked: second concurrent `task --id` exits 1 "already active" and the lock is visible under `DEVAGENT_HOME/locks` while the first runs); `workers.WireHerdrPaneRunner` installed from `cli.Execute` (nil seam kept for tests); 30s periodic `watchdog-health` rows on the direct path (`emitWatchdogRow` in `spawnCliStreaming`'s poll loop) and the pane path (`emitPaneWatchdogRow` in `RunCommandInHerdrPane`'s poll loop) with the no-progress kill enforced on both; driver `writeHeartbeat` at every `phase()` boundary → `.selfbuild/heartbeat.json` → `GET /status` `loop:{iteration,phase,pid,updatedAt}` |
| FR-VAL-04 | Chaos soak: nightly fault-injection scenarios — SIGKILL driver mid-iteration (stale-lock break), SIGKILL worker mid-run (attempt 2/3 retry), fake provider hang (watchdog kill), network blackhole during state push (deferred push), repeated failure (circuit breaker) — each asserting recovery + correct ledger classification | S | [#292](https://github.com/FreePeak/devagent/issues/292) |
| FR-VAL-05 | Quality-drift ratchet: LLM-judge rubric scoring of shipped PRs recorded as `eval-score` ledger rows, trailing-window regression warning in `ledger --clusters` + research prompts, nightly CI drift job; rubric version recorded per score | S | [#293](https://github.com/FreePeak/devagent/issues/293) |

Non-goal: FR-VAL does not add a new runtime subsystem; it hardens the
existing driver with validation surfaces (test, command, telemetry, chaos
schedule) so "the driver works perfectly" is a checkable claim, not a hope.

---
*Last updated: 2026-09-12 (pr-hygiene landing-evidence triage) — the zombie-PR sweep now checks the issues a `devagent/TASK-*` PR cites (`#N` in the body): a CLOSED issue with a `(#N)` commit on main closes the PR as `shipped-elsewhere` citing the sha; a CLOSED issue with no landing commit closes it as `superseded` ("not shipped"); an open issue keeps a red-across-grace PR open with a once-per-PR evidence comment. Triage lives inside the existing sweep (internal/orchestrator/pr-hygiene.go), not a new path; PR body is now part of PrStatus (prFields).
*Last updated: 2026-09-12 (issue #321: loop supervision mode is versioned and checked) — `devagent doctor` gains a read-only `supervision` row: `DetectSupervision` (internal/commands/supervision.go) scans launchd/systemd unit dirs for a unit running `devagent-go loop` (adjacency-matched so the builder/orchestrator `-loop.sh` agents never match) and classifies the restart policy — `restart=always` / `KeepAlive=true` warns naming the unit (the #286 secondary hollow-restart hazard), `on-failure` or no policy passes, and nothing registered passes labeled `unsupervised (nohup) — intentional halts leave the loop stopped`. Also new: `devagent supervision` one-liner, the same mode printed by `make loop-status`, and a reviewable, opt-in launchd template `launchagents/com.devagent.selfbuild-loop.plist.template` (KeepAlive {SuccessfulExit=false} only; outside `agents-install`'s `*.plist` wildcard so installing it is a deliberate act). Pinned by fixture-file unit tests, no live supervisor required. Details §23.*
*Last updated: 2026-09-12 (CI flake: TestReleaseKeepsReplacedLock tick-proofed) — the #316 regression test built its "later holder" decoy from a second `NowFunc()` read taken before `TryAcquireRun` stamped the lock; one ms tick between the two reads made the decoy byte-identical to `l1`'s own payload and `Release` correctly unlinked it, failing the macOS CI job on an otherwise-green PR (#329's first run, rerun green; ~0.25%/iteration measured locally). The decoy now derives from `l1.startedAt` — the value the acquisition actually stamped — so the test is wall-clock independent.*
*Last updated: 2026-09-12 (issue #317: TUI worker panes no longer render idle while headless workers stream) — herdr's `agent_status` never leaves "idle" for `omp -p --mode json` panes launched via pane-run, so `mapPaneState`'s status-only mapping showed live workers as "● idle" in `/agents` and the TUI (and fed the stale classification). The FR-VIS-02 roster now mirrors the FR-VIS-07 sweep discriminator: `paneState` probes `pane process-info` for idle/unknown rows and upgrades a live worker foreground to `running`; interactive panes keep the agent_status signal. Pinned by `TestListSessionPanesUpgradesLiveHeadlessWorker`.*
*Last updated: 2026-09-12 (round 2 of merged = shipped, #328) — open-PR detection at pick time: when research names no PR, the claimed issue's timeline is checked for a cross-referenced OPEN pull request and the iteration routes to the verify-and-merge template instead of a rewrite that cannot open a second PR (live-verified: iteration 261 detected PR #327 and landed #316); the lint gate's tier 2 fails only for a completed run reporting findings — timeouts/context-loading/exit≠1 and go.mod-less repos are logged and skipped, never failed-lint rows the repo did not earn — and it now logs the local golangci-lint version (CI pins its own; a local v2.1.6 certified green where CI's v2.13.2 was stricter, and one shared-cache artifact produced phantom findings from another branch's WIP until a rerun cleared it); `ProductiveGoals` (backlog strikethrough via CheckBacklogPick) now filters on the shipped subset so a `pr-open` row never reads as landed in the PRD. Decision entries: internal/loopdriver/DECISION.md.*
*Last updated: 2026-09-12 (issue #316: task run locks are per-run, releases are conditional, dispatch refusals are observable) — two overlapping `devagent task` runs both locked the generic `TASK` key: the newcomer's TTL stale-break stole a live long run's lock, the finished run's deferred `Release()` unlinked a newcomer's live lock, `runs.active` lied in both directions, and a refused daemon dispatch died silently on discarded stderr while the TUI still said "goal dispatched". Three fixes, each pinned by a regression test: (1) the `task` command resolves its task id BEFORE `TryAcquireRun` — no `--id`/`DEVAGENT_TASK_ID` falls back to `pipeline.DefaultTaskID`, never the constant `TASK` — so the lock key, the worktree and the branch all name the same run and concurrent runs can never contend (this also covers loopdriver and MCP dispatches, which spawn `devagent task` bare); (2) `RunLock` records its pid+startedAt and `Release()` unlinks only when the file still carries that payload, so a stale-broken predecessor can never delete the winner's lock; `TryAcquireRun` symmetrically never stale-breaks a LIVE holder's lock regardless of TTL (loop runs routinely outlive the 1h TTL) — a dead holder still breaks immediately and the TTL now only backstops pid-less/corrupt payloads, mirroring `countActiveRuns`; (3) `DefaultDispatchRunner` captures the detached child's stdout/stderr into `<DEVAGENT_HOME>/runs/dispatch-<taskID>.log` (header line + append, best-effort), so a refused spawn is observable. Verified live 2026-09-12 against the built binary: two concurrent task runs held distinct locks with correct payloads, each exit removed only its own lock, and `/status runs.active` read 0 → 2 → 0 across the window; a live `POST /dispatch` child's stderr landed in its dispatch log. Pinned by `TestReleaseKeepsReplacedLock`, the rewritten `TestTryAcquireRunDedupAndStaleBreak` (live-holder refusal + pid-less TTL break + predecessor-release guard) and `TestDispatchLogWriter`.
*Last updated: 2026-09-11 (merged = shipped + format/lint gate, #323) — the loop's post-merge-back gate is now two-step: a format/lint check (`gofmt -l` over tracked Go files, plus `golangci-lint run` when the binary is on PATH, both through internal/spawn's bounded drain; `SELFBUILD_NO_LINT_GATE=1` opts out) runs before `go test ./...` and records `failed-lint`; and tracker issues close only when the work is IN main — a verify-and-merge pick, an opened-PR-since-merged, or push-mode `main` closes it, while a merely-open PR records the productive `pr-open` row and leaves the issue re-pickable (`AlreadyShipped` now ships on ok|merged|pushed; `pr-open` scores as success in lessons `isLoopSuccess` and renders amber in the TUI). Decision + rationale in internal/loopdriver/DECISION.md. Live evidence: one unformatted file on main made six shipped iterations unmergeable (six open PRs, five over closed issues — #323 Case B); the gofmt commit plus PR #322/#324 recovered #308 and #315, and the new gate keeps the next PR from inheriting that class.*
*Last updated: 2026-09-11 (issue #315: a dispatched goal claims its own queue row) — the TUI/`POST /dispatch` goal rendered as a phantom "queued · waiting for a worker claim" card while its worker streamed, and 27 minutes later the selfbuild loop claimed the same still-`pending` row and ran the goal a second time in a second worktree. `dispatchEndpoint` now claims the row it just enqueued (`claimedBy: daemon`, fresh lease, `attempts` 1) **before** spawning, and the row id reaches the child: `DispatchSpec.TaskID` is set by `parseDispatch` and `dispatchArgv` emits `--id <taskID>`, so the worker's run lock (`locks/TASK-*.lock`), worktree (`.devagent-worktrees/TASK-*`) and branch (`devagent/TASK-*`) all name the row the dashboard card shows. The release lives in the CHILD — `devagent task` defers `pipeline.FinishDispatchClaim(repo, taskID, …)` at every exit, fenced on the claim generation read back from the row (`done` on a clean run, `failed` with the note otherwise; a row another worker reclaimed meanwhile is never overwritten, and an id with no daemon-claimed row is left alone for the CI fixer/remote paths) — because a detached child outlives a daemon restart (`setDetach`), so a daemon-side settlement goroutine would silently drop the completion and leave the row claimed for the full lease; only a spawn that never produced a child is settled in the daemon (`DefaultDispatchRunner`, which keeps a reaper-only `cmd.Wait` goroutine — Go collects a process only through `Wait`, and this process outlives every run it starts). The TUI queue card follows the row status (`queueRowChip`/`queueClaimLine`, `TuiQueuedTask.claimedBy` mapped in `NormalizeAgents`), so a claimed row reads "claimed by daemon" instead of waiting for a claim; a pending row renders exactly as before. Verified against a built binary on a scratch repo: `POST /dispatch` → `{"ok":true,"taskId":"TASK-a58cd4c6"}` with the row `claimed`/`daemon`/`leaseGeneration 1` in the same second, `/status runs.active 1` off that row's lock, `ClaimNextPending` under `selfbuild-loop-*` returning nil, the worker's worktree named `.devagent-worktrees/TASK-a58cd4c6` (auto-cleaned, row `done` on its clean exit); after the release moved into the child, a run of dispatches whose repo config the child rejects showed the children settling their own claims (`TASK-47254d79` → `failed`), with the daemon's own settlement path exercised only by a null PID. Pinned by `TestDispatchHappyPath` (claimed + loop cannot re-claim), `TestDispatchArgvTaskID`, `TestFinishDispatchClaim` (done/failed/foreign-claim/stale-generation) and `TestQueuedCardFollowsRowStatus` (+ the `NormalizeAgents` claimedBy pin). Same issue, second defect: `/status runs.failed_recent` is now windowed to 24h instead of the all-time queue-failed count, so the 17-day-old `SCOUT-20260825-lvvj` row stops painting "1f recent" and pinning the tray at failed (`TestStatusFailedRecentWindow`; `queue.ParseISO` exported for the window). Prior: 2026-09-11 (selfbuild-loop liveness: preflight, run lock, status, TUI input) — the loop
had stopped shipping entirely (breaker cycling: provider-degraded rows, zero dispatches, three
consecutive failed iterations, then the starvation halt). Root causes fixed, each mutation-checked:
(1) `RunPreflightProbe` completes on the STREAMED `"text":"OK"` marker via `spawn.RunCliUntil` instead
of waiting for process exit — measured live: the answer arrived at ~24s (`stopReason:"stop"`, ttft
8.1s) with the marker present 3x while the CLI lingered past a 70s kill, so a HEALTHY provider was
stamped provider-degraded; the probe now returns `ok, attempts=1` in 5.3s (785e1ae);
(2) `ledger.TryAcquireRun` breaks a lock whose holder pid is DEAD regardless of TTL — a
breaker-killed incarnation's `TASK.lock` (pid 6762 gone) refused every `devagent task` for up to the
1h TTL, so issue-first pick claimed #286, research completed, and dispatch died at the lock (f8efde0);
(3) same liveness rule for `countActiveRuns`, so `/status runs.active` stops reporting 0 for a task
whose lock outlived the TTL (the 87-minute #286 task), and the TUI header/hero read the daemon's
lock-derived count instead of the (empty, workers-bypass-herdr) pane roster (be4c9a5, 1e5e49e);
(4) the TUI dispatch/approve sheets are multi-line with a fixed terminal-sized viewport (Ctrl+N /
Alt+Enter insert breaks; Enter still submits) (be4c9a5). Outcome verified end-to-end: loop 252
`[ok]` shipped issue #308 as PR #322 (gate passed, merged 5c19df9) after shipping #286 as PR #320
(merged 0ead693) via its own verify-and-merge dispatch. Prior: 2026-09-11 (issue #308, second half: the tree kill reaches every caller) — `RunCli`
is now `RunCliUntil(..., nil)`, so every git/gh/launchctl/worktree/gate/worker call site inherits
the swept process-group kill and the 3s bounded drain instead of CommandContext's direct-child kill.
Measured pre-change: a `RunCli` timeout (500ms wall) against a child that had forked a grandchild
holding the inherited stdout pipe did not return until that grandchild's own 45s sleep ended
(45.02s — #273's loop pin, red-before pin `TestRunCliTimeoutKillsGrandchild`); post-change the same
call returns in 0.51s with the grandchild reaped. The retarget also surfaced a mapping hole: on the
WaitDelay path `Wait` returns `exec.ErrWaitDelay` (not an `ExitError`) when a descendant holds a
pipe, and RunCliUntil's tail mapped every non-ExitError wait to -1 — a child that exited 0 with a
lingering grandchild would have reported -1 ("spawn failure") to all ~28 callers. `runDevagent`'s
existing rule now lives in spawn too: `ErrWaitDelay` resolves from `cmd.ProcessState.ExitCode()`
(os/exec prefers a real ExitError, so ProcessState is the child's own verdict), pinned red-before by
`TestRunCliDrainCutoffKeepsChildExitCode` (3.01s `ExitCode:-1` pre-fix, `ExitCode:0` post-fix).
`internal/loopdriver`'s duplicated `setOwnProcessGroup`/`killDispatchTree` are deleted — its
`proc_*.go` keep only `processAlive`, and the outer dispatch wall calls the shared
`spawn.SetOwnProcessGroup` + `spawn.KillProcessTree` (which also upgrades that wall's post-deadline
sweep from one `kill(-pgid)` to the retry-until-ESRCH rounds #312 added), closing the "helper was
duplicated, not lifted" residue #312 left. `RunPreflightProbe` reached `RunCliUntil` separately
(785e1ae).
Prior: 2026-09-11 (issue #286, starvation-halt teardown) — the
starvation halt can no longer hang the driver. A child spawned during the
iteration (pane-run/exec helper) that left a grandchild holding the stdout
pipe write-end pinned os/exec's `io.Copy` forever: the driver printed
`[starvation] … halting loop` and never exited (PID 84739, 2026-09-10;
SIGQUIT showed the copy goroutine parked in `io.copyBuffer` on the exec
pipe). Two bounds: (1) every captured-pipe teardown in `internal/loopdriver`
now sets `WaitDelay = pipeDrainDelay` (3s — the #248 bounded-drain
semantics generalized package-wide: `gitQuiet`, `runDevagent`
`CombinedOutput`, the outer dispatch wall, `pickIssue`/`prState` gh
captures, state-sync git), and a drain cutoff never relabels a child that
itself exited 0 (push/pull/PR verdicts feed loop control); (2) every
terminal `RunLoop` verdict (starvation, iteration cap, lock contention,
circuit breaker, log-open failure) arms an exit watchdog
(`ExitWatchdogDelay`, 10s default) that forces exit with the verdict code
if the graceful unwind still blocks — the hard exit skips the deferred lock
release, which self-heals on the next start (stale-holder break). Pinned by
`TestRunLoopStarvationHaltArmsExitWatchdog` (exit seam swapped for a
recorder), `TestRunDevagentDrainBoundedOnHeldPipe` (helper exits 0, its
grandchild holds the pipe — child output and rc 0 preserved within the
drain bound) and `TestTaskDispatchWallKillsWedgedWorkerTree` (retargeted at
`pipeDrainDelay`).
The issue's secondary (a `restart=always` supervisor turning each
intentional halt into a hollow restart — 133 overnight) has no in-repo
enforcement point: `make loop-start` is plain `nohup` with no restart
policy, and nothing in the tree registers the loop with `restart=always` —
that policy lived in the external hub process config. The driver-side
guarantee is what landed: intentional halts exit 0 (so `on-failure`
correctly ignores them) and the watchdog guarantees the process actually
leaves.

*Update (2026-09-12, issue #321):* the secondary now has an in-repo detection point, though still no enforcement point — doctor is read-only. `internal/commands/supervision.go` (`DetectSupervision`) scans the standard launchd/systemd unit directories for a unit whose ExecStart / ProgramArguments is `devagent-go loop` (adjacency-matched, so sibling agents running build-loop.sh / orchestrate-loop.sh never count) and classifies its restart policy: `restart=always` / `KeepAlive=true` → a warn row naming the unit path; `restart=on-failure` / `KeepAlive {SuccessfulExit=false}` / no policy → pass; nothing registered → pass, labeled `unsupervised (nohup)`. Surfaced three ways: doctor check 10 (`supervision`), `devagent supervision` (one-liner), and `make loop-status` (prints the mode after the RUNNING/STOPPED line). A recommended, reviewable launchd template is versioned at `launchagents/com.devagent.selfbuild-loop.plist.template` (KeepAlive keyed on `SuccessfulExit=false` only — the exit-0-halt contract honored; the `.template` suffix keeps `make agents-install`'s `*.plist` wildcard from installing it — supervision is opt-in, the loop deliberately runs unsupervised). Pinned by `TestDetectSupervisionClassifications`, `TestDetectSupervisionIgnoresSiblingAgents`, and `TestDoctorSupervisionRowVerdicts` (fixture unit files; no live supervisor required).
Prior: 2026-09-11 (competitor deep scout) — eight-scout web pass on how
every competitor builds its automatic dev workflow (Devin/Outposts, Copilot
cloud-agent rename + HydraFusion, OpenHands V1, Factory Missions, Jules
Planning Critic, Antigravity verification ladder, Claude Code routines,
startup-tail deaths), distilled into
`docs/research/2026-09-11-competitor-scout-synthesis.md` + eight cluster docs,
summarized in §19.8; §4.2 item 1 narrowed to *execution-based* migration
validation (CodeRabbit ships Squawk statically). Prior: 2026-09-11 (spawn
tree-kill follow-up, on top of #312) — the
early-completion kill is now a swept kill, not a one-shot. #312's
`killProcessTree` signaled the process group once; `kill(-pgid)` reaches only
the members alive at that instant, so a child forked microseconds after the
leader's death survived, inherited the stdout write-end, and left
`RunCliUntil`'s reader without EOF — the call hung until the context deadline
with the correct verdict already in hand (reproduced deterministically:
marker at t=214ms, `kill(-pgid)` returning nil, the orphaned `sleep` alive
with the killed child's pgid; macOS CI-Go went red on #312's merge commit
with exactly this 20.00s signature, 1-in-40 locally). The kill now does the
direct leader kill first (removes whoever does the forking) and then sweeps
the group up to 10 rounds x 5ms until `Kill(-pgid)` returns ESRCH — the
group empty, not a guess. Pinned by the pre-existing
`TestRunCliUntilEarlyCompletion` (60x and 200x -count runs and -race clean;
was a 1-in-40 hang before the fix).
Prior: 2026-09-11 (issue #308, first half) — `spawn.RunCliUntil`
landed in `internal/spawn`: the streaming/early-completion exec primitive
(predicate receives accumulated stdout per chunk; marker → process-tree kill
with `ExitCode -1`/`TimedOut=false` — the predicate, not the exit code, is
the verdict; partial stdout/stderr preserved for gate/ledger details;
#248 bounded-drain `WaitDelay` 3s; kill does the unix group sweep
`Kill(-pid)` AND the direct `Process.Kill` floor for windows). Pinned by
TestRunCliUntilEarlyCompletion, TestRunCliUntilKillsGrandchild (grandchild
reaping), TestRunCliUntilNilPredicate, TestRunCliUntilTimeout,
TestRunCliUntilSpawnFailure. ~~`RunCli` and its ~28 call sites keep the
direct-child timeout kill for now — retargeting them at the tree kill is
the second half of #308 and needs its own per-caller timeout-test sweep;
`RunPreflightProbe` is not yet wired to it.~~ Delivered: `RunPreflightProbe`
reached `RunCliUntil` in 785e1ae (the paragraph's "not yet wired" aged out
there), and the RunCli retarget is the 2026-09-11 footer entry above.
Prior: 2026-09-11 (issue #301, loopdriver research pick) — the
self-build loop no longer loses what phase 1 decided. Goal construction read
only the tracker title, so iteration 219 dispatched "Implement GitHub issue
#290 … in full" while `.selfbuild/research/loop-219.md` had picked
"#290 — merge PR #298, not a rewrite" (and loop 216 had picked the same).
`internal/loopdriver` now parses that artifact (`researchPick`: the
`## Pick` section, else the last line, only when the tracker issue is that
pick's subject), carries the rationale into the dispatched goal (`withPickRationale`,
single-lined and capped at 500 chars) and switches a **present-tense**
"merge/land/ship … PR #N" directive to `mergeGoalTemplate` — a verify-and-merge
dispatch (`gh pr checks`, merge, close the issue, PRD status) instead of a
from-scratch implementation. Past tense and bare "via" are excluded and the
route additionally requires the named pull request to be `OPEN` at pick time
(`prState`): research is asked whether a *merged* PR already covers the pick,
and landing-the-merged-PR evidence is permanently true, so an unprotected
match would record a productive row and close an issue nobody worked. Because
such a dispatch lands an **existing** pull request it can never print
`PR opened:`, so its #238 ship evidence is that pull request's merged state
(`prMerged`, decoded `gh pr view --json state` — the file's own `pickIssue`
convention, and it drops a whitespace assumption the fixtures cannot police);
an unmerged one still records the non-productive `no-pr` row and leaves the
issue open. A landed merge fast-forwards the repo test gate onto the merged
tree before verdicting it. Its ledger status stays `ok`: `internal/lessons`
tallies every non-`ok` loop-result row as a failed loop (`guard.go`'s
`status != LoopStatusOK`, in both the overall baseline and the per-lesson rate)
and the TUI status palette has no case for `merged`, so the implemented-vs-landed
distinction is recorded in the iteration log, the `loop-phase` detail and the
row's goal text. Pinned by `TestRunLoopMergePickDispatchesVerifyAndMerge`,
`TestRunLoopMergePickIgnoresAlreadyLandedPR`,
`TestRunLoopMergePickWithoutMergedPRRecordsNoPR`,
`TestRunLoopImplementPickCarriesRationale` and the
`TestResearchPickReadsTheArtifact` table (foreign-issue picks, a fallback
mention of the *next* issue, past-tense history, extract failures,
`#2900` ≠ `#290`), plus the prompts-level unit pins `TestWithPickRationale`
and `TestMergeGoalTemplateDropsImplementTemplate` (`prompts_test.go`).
Prior: 2026-09-11 (preflight probe: cap reverted, attribution
corrected) — the 60s→120s raise (b5059c9) is **reverted**: measuring the probe
gave two distinct modes — a 15-75s tail (one gate cleared at 56s) and a
hard-stall mode that never answered within 170s (5/5) — and 120s cannot fix the
second; it only doubles the burn, since preflight runs twice per iteration
(selfbuild + po). 60s stands. Also corrected: the degraded iterations are **not**
upstream saturation (a raw gateway completion on the same route answers in
1.0-2.1s, 5/5, while the CLI stalls) — the cost is the worker CLI's
per-invocation machinery, and at least one sample shows `"text":"OK"` already in
the output when the cap fired, i.e. `RunPreflightProbe` waits for process
**exit**, paying full CLI teardown after the answer arrived. That
complete-on-streamed-marker change is the durable fix, filed separately.
`advisor: enabled` and `retry.maxRetries: 999999` remain unconfirmed confounds
present in both arms — not attributed. The breaker counting degraded rows stays
(bash parity: `selfbuild-loop.sh:277`, `TestRunLoopPreflightBreaker`). Full
measurements live on `PreflightProbeTimeoutMs`.
Prior: 2026-09-11 (config decision, reverted) — tried pinning
`devagent.json` worker+scout `model` off the `onegw/free` combo to the single
`b-ai/glm-5.3-flash` leg (7fdef93) to stop repeated `provider-degraded`
iterations; **reverted** — it bought nothing and cost resilience. Measured:
preflight still degraded on the pinned leg (iteration 229's row carries
`model":"b-ai/glm-5.3-flash"`, `attempts:3`), while iteration 227 (pre-pin, on
the combo) had already shipped #291 end-to-end, and the combo kept streaming
729KB/885KB with 165 clock resets for the workers that later stalled. The pin
also removed 5-target combo fall-through against `b-ai`'s own `429001`
concurrency caps under ~50 co-resident omp agents.
Prior: 2026-09-11 (issue #291, FR-VAL-03) — driver observability
parity: `taskCommand` acquires the run lock before `RunTask` so `runs.active`
on /status is truthful (closes #287; live-smoked with a rejected concurrent
task); `cli.Execute` wires `HerdrPaneRunner` to the herdr pane runtime so
`spawn.visibility: visible` really gets panes (closes #288); watchdog-health
rows now emit every 30s on both the direct and herdr-pane spawn paths while
the no-progress kill stays enforced; the loop driver writes
`.selfbuild/heartbeat.json` at every phase boundary and `GET /status` exposes
it as `loop`, and the TUI iteration card now sources `iteration N · phase X`
from that heartbeat (ledger tail demoted to fallback for old daemons).
Prior: 2026-09-10 (issue #300) — post-merge-back repo-level test gate
default flipped from `npm test` (stale after the Node-tree retirement, PR
#240) to `go test ./...`; the npm default ENOENTed in 0.18s
and stamped every green iteration `failed-tests` (last ledger `ok` row: loop
176, 2026-09-08), feeding the 2026-09-10 starvation halt and breaker trip.
Pinned by TestRunRepoTestsDefaultIsGoTest and the updated
TestConfigFromEnvTestCmd defaults. Details §22.2.
Prior: 2026-09-10 (TUI view-switch fix) — pressing 1/2/3 could leave
with OPOST off, drawLocked's unconditional trailing `\n` after an
all-skip frame (the norm after a view switch) scrolled the alternate
screen and desynced every later diff. RenderFrame now erases leftovers
via absolute CUP+ED (never cursor-relative), drawLocked drops the trailing
LF, clamps rows to the real geometry and hard-caps degenerate short
terminals; three regression tests pin it (each fails pre-fix). Details §20.8.
Prior: 2026-09-10 (issue #289) — FR-VAL-01 golden soak self-test landed
as `TestGoldenSoak` in `internal/loopdriver/run_test.go`: 3 consecutive
green iterations end-to-end, already-shipped guard rejecting the 4th re-pick,
and the failed-tests classification pin, all hermetic over the existing
installFakes fixture (rotating fake gh); runs in CI via the existing
`go test ./...` job, mutation-checked (ledger writer + pick order).
Prior: 2026-09-10 (PR #294, issue #273) — outer taskDispatch wall
enforced: the dispatch child runs in its own process group
(`setOwnProcessGroup`, unix; documented no-op degradation off unix) with
the #248 bounded-drain `WaitDelay` (3s) generalized to the outer wall;
`killDispatchTree` SIGKILLs the surviving group at the deadline so nothing
dispatched outlives the anchor. The rc resolves from ProcessState, not
Wait's error: a dispatch that finished 0 before the wall keeps 0, only a
wall-killed child reports 124 (GNU timeout convention) — a shipped task is
never relabeled failed by a drain that crossed the anchor. Pinned by
TestTaskDispatchWallKillsWedgedWorkerTree and TestDispatchRcMapping.
Prior: §23 Driver Validation addendum (FR-VAL-01..05, issues #289–#293)
scoped from internet prior art (SWE-bench, AWS/Azure chaos engineering,
OpenTelemetry GenAI semconv, LLM-as-judge drift practice); the
TestHeaderBarReassertsInverse palette-agnostic contract fix (dd46154) —
the red-in-color-mode test behind loop 209's failed-tests row and main's
red CI since f101909; the #271 flake fixes (98a30bd, 40a1bac, f30c170).

