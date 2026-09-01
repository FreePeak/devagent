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

1. **Nobody validates database migrations.** Every competitor treats "tests pass" as the correctness ceiling. None offer schema-diff analysis, migration dry-runs against a shadow database, rollback verification, or destructive-change detection. This is the core of DevAgent's differentiation.
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
| Worker agents | Claude Code and OpenCode headless CLIs |
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
| FR-IMPL-01 | Execute implementation by spawning a headless worker agent inside an isolated git worktree and dedicated branch (`devagent/<ticket-id>`) | M |
| FR-IMPL-02 | Support two interchangeable workers: Claude Code (`claude -p`) and OpenCode (`opencode run`); selection via flag or config | M |
| FR-IMPL-03 | Fan-out mode: run the same plan through both workers in parallel worktrees and select one diff (or merged diff) after validation scoring | S |
| FR-IMPL-04 | Feed failing test output back to the worker and retry, up to a configurable maximum loop count (default 3) | M |
| FR-IMPL-05 | Enforce per-run budget: maximum wall-clock time and maximum worker steps/tokens; abort cleanly on breach | M |
| FR-IMPL-06 | Fall back from one worker to the other when the primary errors or times out mid-run | S |

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

Optional, budget-bounded structural context derived from an external knowledge graph (default: local `leankg` MCP for freepeak workspaces; off otherwise). Assembled at the same `COMPACT_CONTEXT_MARKER` as the lessons and childTrails digests so G0/G5 read identical content.

| ID | Requirement | Pri |
|---|---|---|
| FR-CTX-01 | Inject a KG-derived context digest into the scout, planner, and repair prompts at `COMPACT_CONTEXT_MARKER`, ratchet-capped at the same character budget as `lessonsMaxChars` (default 4000); oldest entries dropped whole, never split | M |
| FR-CTX-02 | Make the KG provider opt-in via config (`devagent.context.kg: "leankg" \| "off"`, default `off`); when `off` or the provider is unreachable, return an empty digest and continue without it — never block the pipeline | M |
| FR-CTX-03 | Wrap every KG client call in a 1s wall-clock budget; on timeout, downgrade to a cheaper query (`search_code` → `find_function` → empty); surface the degraded mode in the run log | S |
| FR-CTX-04 | Never pass the KG client to a worker adapter; the KG digest is an orchestrator-side concern only, preserving the "any worker, same contract" invariant from section 9 | M |

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
|---|---|---|
| `--ticket <id>` | required | Tracker ticket identifier |
| `--repo <path>` | `.` | Target repository |
| `--worker <name>` | config | `claude-code`, `opencode`, or `both` (fan-out) |
| `--auto-pr` | off | Skip approval gates (headless mode) |
| `--interactive` | on when TTY | Pause at human gates |
| `--max-loops <n>` | 3 | Test-failure retry budget |
| `--timeout <dur>` | 30m | Wall-clock cap per run |
| `--dry-run` | off | Plan only; no workers, no remotes |

### Environment variables (credentials only — FR-OPS-02)

`LINEAR_API_KEY`, `GITHUB_TOKEN` (scoped to contents:write + pull-requests:write on target repos), `DEVAGENT_HOME` (run state/logs).

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
| NFR-05 | DevAgent runs on macOS and Linux with Docker as the only heavyweight dependency |
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
- Selfbuild state bootstrap — auto-create and mirror the `selfbuild/state` orphan branch on first loop run; the durable-state mechanism shipped but no state branch exists on origin yet.
- Merge-queue rebase automation — stacked loops land with expected conflicts against `main`; auto-rebase waves before dispatch instead of manual queue refreshes.
- Lessons feedback loop — `selfbuild-state.sh` mirrors `lessons.md` to the state branch (PR #19) but nothing reads it back; inject curated lessons into worker repair and planning prompts so past failures stop repeating.
- Flaky-test guard for fan-out judging — winner selection assumes deterministic tests; add quarantine/rerun handling for nondeterministic suites.
  > **Completed post-v0.3 (2026-08-28):** one flaky rerun before condemning a
  > candidate, and a clean pass outranks a flaky rescue in winner ranking
  > (`src/workers/fanout.ts:74-95`); closes Q8 below.
- Failure-cluster reporting on ledger analytics — recurring gap categories in the ledger should surface as actionable periodic reports, not just queryable rows.
- Knowledge-grounded context — opt-in structural memory from the local `leankg` MCP (or off by default) injected into scout/planner/repair prompts at the existing `COMPACT_CONTEXT_MARKER`, ratchet-capped at the same 4000-char budget as `lessonsMaxChars`; routes to the freepeak `leankg` server per `skill://leankg-routing`, never `be-knowledge-graph` from a freepeak path, with `rtk` (Grep/Glob/Read) fallback when the server is down. See `docs/research/2026-08-30-devagent-leankg-value-in-harness-era.md` and FR-CTX-01..04.

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
> **Queue sweep (2026-08-31, operator):** 25 queue rows resolved without new
> code — 17 marked done citing shipped PRs (#84 pre-loop guard, #86
> release-gate + scout-replay-in-CI, #96 CI-Fixer; `NESTED_ENV_BLOCKLIST`
> env-scrub in `src/workers/spawn-utils.ts:35,61` + herdr sweep PR #77), and
> 8 duplicates consolidated (7 merge-back variants into
> `IMPROVE-retire-legacy-mergeback`, 1 provider-health variant into
> `OBSERVE-provider-health`). Queue now 15 pending / 33 done / 1 failed.
> Curator: before enqueuing, match candidate titles against merged PR titles
> and these completion notes; skip or auto-done on match.

#### Phase 4 — current backlog (2026-08-31, curation run 19)

- **Zombie-PR hygiene (surviving half of post-PR lifecycle automation)** — PRs #96/#97 shipped CI-Fixer re-dispatch, but nothing auto-closes or skips PRs whose CI stays red across a grace window or whose base is superseded; the queue sweep flagged the factory parking on unresolved PRs (#68 reviewer note).
- **Watchdog regression coverage** — run 1261d6be burned 3x3601s and the suite still has no thinking-only NDJSON fixture (the only `thinking_delta` match in test/ is the omp smoke fixture); add golden fixtures (thinking-only, tool-bearing, mixed) asserting the herdr watcher and `spawnCliStreaming` both trip the no-progress clock.
- **Provider model-id validation at dispatch** — loop 58 burned 3 attempts on `--model coding` exiting 1 in ~12s before PR #92 taught omp to drop aliases; validate `config.model` against each adapter's accepted id shape in preflight so the next adapter fails at the gate, not mid-board.
- **Executor failure surface (`taskInterrupt` + failure evidence)** — kill a worker after 3+ identical `trail.jsonl` failures and write a compact post-mortem (goal, failure class, last gate excerpt) to the ledger on board archive; loops 57/58 both died on the same release-gating goal with only `attempts: 3`-style evidence, and PR #83 still had to mine ledger rows by hand to see why.
- **Reconcile merge-back with per-task PRs** — PR #71 opens per-task PRs while `src/cli.ts:814` still runs `mergeProjectBranches` on `allDone`; retire or gate the legacy path so a fully-done board does not double-merge (Q20).
- **Structural progress signal instead of string matching** — PRs #93/#94 gate the watchdog by substring-matching `"thinking_delta"` (`src/workers/spawn-utils.ts:168`), which is omp/glm-specific; the next provider's streaming shape silently re-enables the silent-hang class, so let each `WorkerAdapter` declare what output counts as progress (Q30/Q33).
- **Consolidate the loop scripts** — the tracked `orchestrate-loop.sh` carries PR #68/#77 recovery while untracked `scripts/orchestrator-loop.sh` runs divergent logic; fold recovery into `src/orchestrator/` and reduce the shell to a thin caller (Q19).
- **Unqueued PRD ingestion** — `docs/prds/` holds four scoped designs (default-isolation, eval-harness, fleet-budget-governance, resource-aware-concurrency) that no curation pass has enqueued; make the curator audit step authoritative (Q15) so planned work stops sitting outside the roadmap while fresh backlog items get invented.

## 18. Open Questions

| # | Question | Owner | Needed by |
|---|---|---|---|
| Q11 | `.devagent/AGENTS.md` auto-load — trust prompt on the operator's managed-settings page (Codex CVE-2025-61260 pattern) or one-time per-repo confirm? | product | Phase 4 |
| Q12 | With PR #57 merged, G0 plan-critic and G5:STRIDE both read the same `COMPACT_CONTEXT_MARKER` digest — should the audit track provenance (which gate, which loop, which trail line) per consumed entry, or treat the digest as opaque input? | eng | Phase 4 |
| Q13 | `taskInterrupt` mid-flight (OpenCode v1.18.20 `task_id` + Codex 0.150.0 interrupt hook): should the orchestrator kill the worker process directly, or send a graceful-stop signal and only force-kill on a short grace timeout? | product | Phase 4 |
| Q14 | With PR #64's governor on by default, should `devagent status` surface the live `effectiveConcurrency` + `lastSample` + per-worker RSS, or stay at the one-line readout added in PR #64 AC-10? Operators need enough to debug "why is auto-1 today" without leaking the per-pid sample into the human view. | eng | Phase 4 |
| Q15 | Now that the curator/queue gap behind PR #64's "sat in docs/prds/ for weeks" is recognized, should the curator's audit step (proposed above) be authoritative (curator enqueues directly) or advisory-only (just emits a warning the next scout cycle reads)? Direct write simplifies, but couples the curator to scout's queue schema. | eng | Phase 4 |
| Q16 | PR #68 archives stuck boards to `.devagent/archive/` and re-bridges from the oldest queued goal — should archive events emit a webhook/notification (the stalled factory ran 6h before a human noticed), and what is the retention policy for archived boards? | eng | Phase 4 |
| Q17 | `requeue_parked` now resets `attempts` to 0 (PR #68), so a task that exhausts `maxTaskRetries` gets a fresh budget on every requeue round — is unbounded retry the right policy, or should cumulative attempt history across rounds cap a task permanently? | eng | Phase 4 |
| Q18 | The 08-29 stuck board burned 3 salvage-instructed attempts on STRIDE wiring — should the executor refuse prompts above a size/complexity threshold (the salvage prompt was ~5 KB of step-by-step edits) and force a plan-split instead, or is dense prescriptive prompting the right shape and only the fail-signal capture (backlog item above) needs fixing? | eng | Phase 4 |
| Q19 | `scripts/orchestrate-loop.sh` still carries PR #68's recovery logic only as an untracked-path script risk — PRs #67/#69 shipped within minutes of each other while the recovery fix lives in a tracked-but-shell-level layer; should stuck-board recovery move into `src/orchestrator/` (typed, tested) with the shell reduced to a thin caller, or stay in the script where iteration is cheaper? | eng | Phase 4 |
| Q20 | With PR #71, per-task PRs open as soon as a task hits `done` while `mergeProjectBranches` still fires on `allDone` — should per-task PRs feed the reviewer/`autoMerge` flow directly (making merge-back a no-op), or stay review-only artifacts until a human decides? (Related: does a per-task PR count as "published" evidence for the task record?) | product | Phase 4 |
| Q21 | ~~The Release workflow publishes on every push to `main` regardless of the `test` job's result — should releases hard-gate on CI green (`needs: test`), or tag first and yank on red? Hard-gate delays tags by one CI run; tag-first risks shipping a broken semver point.~~ Resolved 2026-08-31 (PR #86 hard-gated, unit-test-pinned); removed. | eng | Phase 4 |
| Q33 | Watchdog progress gating is a substring match on `"thinking_delta"` (PRs #93/#94); with Q30's adapter capability block, should "counts as progress" be an adapter-declared classifier (per-line predicate or stream-shape schema) rather than a shared string heuristic, and where does the omp/glm fallback live until then? | eng | Phase 4 |
| Q34 | Run 1261d6be proves the watchdog can silently never fire; should the herdr watcher and `spawnCliStreaming` emit a per-attempt watchdog-health line to the ledger (clock resets, bytes counted, last meaningful-output timestamp) so a never-firing watchdog is visible in analytics instead of inferred from three 3601s timeouts? | eng | Phase 4 |
| Q35 | CI-Fixer (PRs #96/#97) re-dispatches `TASK-fix-<pr>` but fixer outcomes only surface as `autoReviewAndMergeOne` log lines and the terminal `ci-fix-failed` result — should fixer dispatches and outcomes write ledger rows like PR events do, so failure-cluster analytics can count fixer round-trips per goal, or stay log-only? | eng | Phase 4 |
| Q36 | The CI-Fixer fix run (`TASK-fix-<pr>`) gets its own dispatch budget outside the originating task's `maxTaskRetries` — should fixer attempts count against the parent task's retry budget (linking to Q17's cumulative-attempt concern), or is one bounded fix attempt per PR run the right isolation? | eng | Phase 4 |
| Q22 | ~~PR #73 re-bridges a queued goal in the same cycle as board archive, but the archive itself burns the board's salvage history — should archived boards write a compact post-mortem (goal, failure class, gate excerpts) to the ledger so the next bridge can plan around the same failure mode, or is the existing ledger analytics query surface enough?~~ Resolved 2026-08-31: PRs #96/#97's `ci-fix-failed` outcome gives the ledger a structured failure record with summary evidence (matching the Q24 taxonomy); remaining failure-evidence work is tracked by the backlog item "Executor failure surface". | eng | Phase 4 |
| Q23 | PR #77's herdr-sweep trusts the session name alone; if the sweep ever gains an `--all` mode, what stops it from killing a user-attached interactive claude pane that happens to sit in an automation-spawned session (2026-08-26 mass-kill class)? Require per-pane agent-state verification plus a managed-settings-style deny toggle, or keep `--all` out of scope permanently? | product | Phase 4 |
| Q24 | Per-task PRs (PR #71) plus the auto-tag release workflow (PR #75) mean a fully-merged board can produce several PRs and a release in one cycle — should the ledger record release/tag events as first-class outcomes (so per-loop spend-to-shipped-artifact math counts a release), or stay PR-URL-only? | eng | Phase 4 |
| Q25 | G5:STRIDE blocks on HIGH/CRITICAL with no suppression path; fixture credentials in test files (the exact pattern the golden suites ship) will trip it and stall autoMerge — add a per-path/per-finding allowlist committed with the PR, or keep it hard and force workers to rename literals? | eng | Phase 4 |
| Q26 | PR #84 auto-stashes a dirty main before merge-back, but the stash is keyed by SHA and never re-offered — if a curation PR (like #83) is open in the same worktree when the factory merges back, should the popped stash be surfaced as a ledger warning (operator reapplies by hand) or re-applied automatically on the next dispatch? | eng | Phase 4 |
| Q27 | Loops 53-55 and 57/58 each re-burned multiple attempts on the same already-planned goal after a requeue with a fresh attempt budget — should the bridge attach the prior board's failure class to the re-bridged goal so the scout skips it until the root cause ships, or is cross-board retry memory out of scope for the single-tenant model? | product | Phase 4 |
| Q28 | With FR-CTX-01 injecting a KG-derived digest at `COMPACT_CONTEXT_MARKER`, should that digest also be appended to `lessons.md` on a successful merge (so future runs learn from the same structural evidence the planner used), or stay scoped to the single run and rebuild fresh each time? Persisting makes the digest cross-run durable; scoping avoids stale evidence bleeding in. | eng | Phase 4 |
| Q32 | PR #92 drops driver-tier model aliases for omp only, leaving claude-code/opencode adapters to interpret `config.model` their own way — should the model field be normalized once at config load (provider-qualified ids everywhere, aliases resolved to a concrete id), or does each adapter own its id semantics? | eng | Phase 4 |
| Q31 | PR #91 strips LSP/extension discovery from headless omp but the same startup-stall class plausibly exists for other MCP-discovering CLIs; should worker preflight include a cold-start latency budget (fail fast above N seconds) or is the per-adapter no-progress watchdog enough? | eng | Phase 4 |
| Q29 | PR #88 makes the run branch the source of truth after `cleanup=auto` (snapshot onto `devagent/<taskId>`, then publish from that branch), but snapshot and publish remain two stages with the deleted-cwd failure class between them — should auto-cleanup snapshot and per-task publish collapse into one commit path so the worktree's death cannot strand a green task, or is the regression-test guard enough? | eng | Phase 4 |
| Q30 | omp (PRs #87/#89) needed adapter-specific hardening — prewalk off, stream-error parsing, capped retries, a bespoke no-progress timeout — none of which the registry declares. Should `WorkerAdapter` expose a capability/limit block (supported flags, stream quirks, watchdog defaults) the scheduler can honor, or stay as per-adapter internals patched case by case? | eng | Phase 4 |

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

**G4 evidence base.** Dynamic ground truth: Go race detector (TSan-based happens-before, zero false positives, coverage-bound, 5–10x memory overhead). Static: Meta Infer RacerD (compositional inter-procedural detection, incremental CI re-analysis, caught 2500+ issues pre-production at Facebook; annotation-free operation drove adoption). LLM state of the art: ConSynergy hybrid pipeline (LLM chain-of-thought cross-thread reasoning → SMT verification) reaching precision 80% / recall 87.1% on DataRaceBench and related benchmarks; consistent finding across the literature that pure-LLM interleaving reasoning underperforms specialized tools and hybrids win.

**Claude Code headless (`claude -p`).** Verified flags: `--output-format text|json|stream-json` (JSON result includes `session_id`, `total_cost_usd`, per-model usage; `stream-json` emits NDJSON events ending in `type:"result"`, with retryable-failure categories via `system/api_retry` events); `--json-schema '<schema>'` for validated structured output; `--permission-mode default|acceptEdits|plan|auto|dontAsk|bypassPermissions`; `--allowedTools` / `--disallowedTools` allow/deny lists; sessions via `--continue` / `--resume <id>` (cross-directory since v2.1.223); budget via `--max-turns <n>` (errors with subtype `error_max_turns` when reached); plus `--model`, `--fallback-model`, `--append-system-prompt`, `--mcp-config`, and `--bare` for fast deterministic CI starts (skips hooks/skills/plugins discovery). Hooks system (`PreToolUse`, `PostToolUse`, `Stop`, `PermissionRequest`, etc.) enables programmatic permission decisions via JSON output. Version-dependent behaviors to pin: `--json-schema` validation (v2.1.205), `plugin_errors[]`/`mcp_server_errors[]` CI-gate fields (v2.1.219+).

**OpenCode headless (`opencode run`).** One-shot execution with `--format json` (raw JSON events), `-m provider/model`, `-c/--continue`, `-s/--session <id>`, `--agent <name>`, and `--auto` to auto-approve permissions not explicitly denied — the orchestrator escape hatch. Strongest programmatic surface of the two CLIs is `opencode serve`: headless HTTP server (OpenAPI 3.1 at `/doc`) with full REST session CRUD/fork/abort/diff, a permission-answer endpoint, SSE event streams, and `--attach <url>` on `run` to reuse a warm server (avoids cold boot per run). Also ships an ACP stdio server and GitHub Actions mode. Permissions resolve per action (`read`, `edit`, `bash`, `task`, ...) to allow/ask/deny with wildcard patterns, last-match-wins; `.env` reads denied by default.

**Orchestration patterns (community practice).** Git worktrees per worker are the standard isolation primitive (sub-second creation, shared object store) — they prevent filesystem collisions, not logical conflicts, so pair with ownership boundaries over shared files (lockfiles, migrations, contracts). Task claiming via lease + heartbeat so crashed workers' tasks reassign. Verification gates must be run by the supervisor independently of agent claims; failed verification reassigns rather than trusts completion reports. Parse worker stdout as NDJSON line-by-line; enforce budgets via `--max-turns` plus wall-clock kill; retry only on retryable error categories (`rate_limit`, `overloaded`, 5xx) with backoff. Cost heuristics: no agents for <5-min work, read-only exploration before implementation, stop after two failed repair attempts.

Sources: [Claude Code headless](https://code.claude.com/docs/en/headless) · [CLI reference](https://code.claude.com/docs/en/cli-reference) · [Hooks](https://code.claude.com/docs/en/hooks) · [Settings](https://code.claude.com/docs/en/settings) · [GitHub Actions](https://code.claude.com/docs/en/github-actions) · [OpenCode CLI](https://opencode.ai/docs/cli) · [OpenCode server](https://opencode.ai/docs/server) · [OpenCode config](https://opencode.ai/docs/config) · [OpenCode permissions](https://opencode.ai/docs/permissions) · [Parallel agents in isolated worktrees (amux)](https://amux.io/blog/parallel-agents-isolated-worktrees/) · [claude-code-action](https://github.com/anthropics/claude-code-action)

These findings refine section 9's adapter table: Claude Code budget control = `--max-turns` + wall clock; OpenCode permission bypass = `--auto`; both emit parseable JSON event streams; OpenCode additionally offers the serve-based REST surface as a future alternative transport.

### 19.5 Knowledge-graph-grounded context

A research note (`docs/research/2026-08-30-devagent-leankg-value-in-harness-era.md`) evaluated whether the local `leankg` MCP (FreePeak build, Postgres-backed) adds value to DevAgent's harness. Verdict: yes, as the structural complement to the harness's durable state (lessons digest, childTrails digest, worklog, ledger). Live inventory at 2026-08-30 is 358,359 elements and 1,867,483 relationships across 38,597 files; live `mcp_status` ok, but `kg_semantic_context` timed out at 30s, so v1 must use non-semantic graph queries (`search_code`, `find_function`, `get_call_graph`, `get_tested_by`, `get_dependents`). Integration seam is the existing `COMPACT_CONTEXT_MARKER` ratchet (`src/prompt.ts:303-317`); the KG digest joins `lessons` and `childTrails` as a fourth source under the same 4,000-char cap. Scope: orchestrator-side only, opt-in (`devagent.context.kg: "leankg" \| "off"`, default `off`), never reaches a worker adapter. Cross-workspace routing rule (freepeak → `leankg`; BE → `be-knowledge-graph`; never both) is enforced by `skill://leankg-routing`. See FR-CTX-01..04, Phase 4 sub-bullet "Knowledge-grounded context", and Q28.
