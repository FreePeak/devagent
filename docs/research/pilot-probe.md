# Research: Pilot (qf-studio/pilot)

Date: 2026-09-04
Scope: competitive probe requested by operator — "learn from it, continue devagent or contribute?"
Sources: github.com/qf-studio/pilot README + repo tree (fetched 2026-09-04 via GitHub API). No local clone; line-level behavior unverified.

## 1. What Pilot is

Go CLI + daemon ("AI that ships your tickets"): claims labeled GitHub issues (or Telegram
messages / Linear / Jira / Asana), plans, implements with the Claude Code CLI (OpenCode as
second backend), runs test/lint/build quality gates with auto-retry, opens a PR, and an
"autopilot" loop watches CI and auto-merges (dev/stage/prod autonomy levels). 665 stars,
BSL 1.1 (converts to Apache-2.0 after 4 years; competing SaaS requires a license), Go 1.22+.

Direct overlap with DevAgent's core loop: ticket → plan → implement (headless coding CLI)
→ gates → PR → auto-merge, plus the same worker-CLI pattern (Claude Code primary, OpenCode
secondary — DevAgent's is omp/opencode/claude-code/pi, BYO-provider).

## 2. What Pilot has that DevAgent lacks (steal list)

| Pilot capability | DevAgent status | Action |
|---|---|---|
| Cross-platform desktop app (macOS universal dmg / Windows exe / Linux tar.gz) shipped from releases | §20.4 addendum only (FR-UI-01..09, unstarted) | Validates Tauri 2 cross-platform bet; their release-matrix CI is a template for FR-UI-09 |
| Live TUI/HTML dashboard: current task + phase %, queue depth, token/cost cards, budget-vs-spend | `devagent dashboard` static kanban (`src/observe.ts`); no live view | FR-UI-08 pipeline view: mirror their "Current Task / Token / Cost / Recent" card layout |
| Telegram bot as first-class control surface (chat/research/plan-with-Execute-button/task, voice, images) | None | Not in PRD v1 scope; §20 dispatch sheet is the analog. Candidate backlog item, not a pivot |
| Daily briefs (scheduled Slack/Email/Telegram reports) + alerting (failures, cost thresholds, stuck detection) | None (Q41: 25 degraded cycles + 2 breaker trips overnight with no notification surface) | Direct answer to Q41's "no notification surface" — briefs/alerts channel is the missing piece |
| Persistent SQLite metrics (token/cost/task counts surviving restarts) | Ledger JSONL rows carry cost only for providers that report it (FR-GROK-03); no aggregate store | Add a `devagent metrics` aggregation over ledger JSONL instead of a second store — NFR-04 already says ledger is reproducible state |
| Epic decomposition into sequential subtasks via cheap-model API | Orchestrator planner (2–6 tasks/goal) + fan-out (FR-IMPL-03) | Already covered; sequential-vs-parallel is a config knob for them, waves for us |
| Model routing (Haiku trivial → Opus complex) + effort routing | Per-role model config; preflight model-id validation (Q32) | Complexity-tiered routing is a plausible Phase 5 refinement behind the WorkerAdapter contract |
| Cross-project memory / knowledge graph (`.agent/knowledge/`) | Per-repo lessons + eval-guard + KG digest (FR-CTX-01..04) | Parity; DevAgent's eval guard (accept-rate scoring) is ahead of their pattern store |
| `pilot upgrade` hot self-update + rollback | `selfUpdate` flag post-merge (FR-SELF-01) | Parity; note their rollback subcommand |
| Execution replay (record/playback/export HTML/JSON/MD) | Scout fixtures replay only (golden.json); ledger is replayable evidence but no playback UI | Replay surface fits FR-UI-08's SSE-derived view — same ledger source |

## 3. What DevAgent has that Pilot lacks (moat list)

- Domain-specific validation gates: migration safety (G2), FK integrity (G3), async/race
  review (G4), STRIDE threat modeling (G5) — Pilot's gates are generic test/lint/build.
- Evidence-gated orchestration: independent auditor (untrusted → done), evidenceGaps,
  ask/human-answer flow, recovery grants, budget-capped waves — Pilot trusts executor
  output with self-review only.
- BYO-provider depth: 4 worker adapters + per-provider model-id predicates + omniroute
  proxy; Pilot is Claude-Code-first (BYOK = Anthropic key/Bedrock/Vertex only).
- Persistent lessons with an eval guard that ranks by measured loop impact (Q39).
- Orchestration surfaces: DAG boards, queue-bridge, PR hygiene (zombie-PR sweep).

## 4. License (verified from the repo's LICENSE file, 2026-09-04)

Pilot is BSL 1.1 (Licensor: Aleksei Petrov; Change Date four years from publication,
then Apache-2.0). Verified terms, sharper than the README table:

- **Copy/modify/derivative/redistribute + non-production use**: granted. **Production use**:
  granted only under the Additional Use Grant — and the grant's carve-out is what matters:
  a "competitive offering" is any **paid** Product that "significantly overlaps with the
  capabilities of Pilot Cloud". DevAgent (ticket→plan→implement→gates→PR delivery) is
  squarely such an overlap if ever monetized.
- **Contributing**: PRs to Pilot grant your work into a BSL-1.1 codebase controlled by the
  Licensor — free labor on someone else's roadmap, with the derivative-work terms above
  attaching to everything you build there.
- **No code-level borrowing either direction**: DevAgent is MIT; copying Pilot code in is
  prohibited pre-Change-Date. This mirrors the PRD's R7 rule for Orca — reuse interface
  designs and patterns, never binaries or source. Everything in §2's steal list is
  pattern-level (UX layouts, alerting model, release-matrix shape), which is safe.

## 5. Verdict: continue devagent; mine Pilot as prior art

Concretely: Pilot ships exactly the UX surface DevAgent §20 specifies but hasn't built —
cross-platform desktop app, live progress/cost dashboard, notification surface. Treat it as
proof the FR-UI/FR-CTRL direction is right, and mine specifics:

1. FR-CTRL alerting endpoints + FR-UI-04 notifications: adopt the briefs/alerts model
   (failure / cost-threshold / stuck detection) — resolves Q41's notification gap.
2. FR-UI-08 card layout: current-task phase %, queue depth, token/cost budget cards —
   directly reusable on top of the FR-CTRL-04 SSE stream.
3. Backlog candidate (not v1): Telegram-style mobile control surface; keep chat-as-transport
   out per §20.5, dispatch-by-message only.
4. metrics aggregation: computed from the existing ledger JSONL (NFR-04), no SQLite copy.
