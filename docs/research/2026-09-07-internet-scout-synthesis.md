# Research: Internet scout synthesis — prioritized idea backlog for DevAgent

Date: 2026-09-07
Scope: distillation of five parallel internet research scouts (this date, ~90 fetched
sources total) into one prioritized, surface-mapped backlog. Each idea's full mechanism,
evidence, and URLs live in the source doc; this file adds only ranking and DevAgent
mapping. Complements PRD §19 (prior research) without duplicating it.

## Source docs

| Doc | Coverage | Ideas |
|---|---|---|
| `2026-09-07-oss-agent-harnesses-scout.md` | mini-swe-agent, SWE-ReX, OpenHands SDK v1 (2026 rework), Aider, Codex CLI, goose, cline, OpenCode, Claude Code, sandbox-runtime, SWE-smith, SWE-Gym, SWE-rebench | 15 |
| `2026-09-07-self-improving-loop-scout.md` | Voyager, Reflexion, Self-Refine, ACE, AlphaEvolve/OpenEvolve, Mem0/MemGPT, Claude Skills, GEPA, SWE-Gym/SWE-smith, 2026 frontier (EVOMAL self-poisoning) | 14 |
| `2026-09-07-multi-agent-orchestration-scout.md` | Anthropic multi-agent research, Cognition counterpoint, LangGraph, AG2, crewAI, MCP 2025-11-25 Tasks, A2A, GitHub/bors merge queues, Managed Agents | 14 |
| `2026-09-07-verification-eval-scout.md` | SWE-bench Verified, Terminal-Bench, METR TH1.1, MT-Bench judge biases, LLM Monkeys, Squawk/pgroll/Atlas/gh-ost, sandbox ladder (srt/gVisor/Firecracker), OWASP LLM Top 10 | 13 |
| `2026-09-07-context-ecosystem-scout.md` | Manus/Anthropic/12-factor context engineering, harness-is-the-product essays, 7-vendor 2026 competitive scan vs PRD §19.2 | 10 + white space |

## Cross-cutting themes (all five converge)

1. **The harness is the product.** Below the frontier, models converge fast enough that
   harness quality dominates (Beckett, `harness-is-the-product`). DevAgent's defensible
   layer is its durable state — ledger, lessons, KG digests, gates — not worker CLIs
   (commoditized) or triggers (commoditized).
2. **Verification depth is the moat.** The 2026 competitor wave (Copilot self-review,
   Jules CI Fixer, Factory Automated QA/Security Review, Devin Review) is surface-generic.
   Nobody approaches migration safety, FK integrity, async/race review, or threat-model
   gates. The white space §19.2 identified still holds.
3. **Enforcement → feedback.** Sandboxes and gates that tell the worker *why* (violation
   attribution, sentinel exits, stop-gates) convert enforcement into same-attempt
   self-correction instead of post-hoc retries.
4. **Context engineering is measurable.** KV-cache hit rate, fixed-offset prompt
   assembly, compaction budgets are reported properties at the leaders. DevAgent already
   owns the seams (`COMPACT_CONTEXT_MARKER`, FR-GROK-03 cost ticks); it does not yet
   measure or report them.
5. **Ledger as reducer state.** 12-factor "stateless reducer" + OpenHands
   condensation-as-event: never mutate history; rebuild derived views (boards, digests)
   deterministically from the append-only stream.

## Prioritized adoption backlog (impact × tractability)

| # | Idea | Surface | DevAgent mapping | Source |
|---|---|---|---|---|
| 1 | Trusted-checkout-only gate config + srt-style OS sandbox wrapping of every gate subprocess (FS/network fence, credential masking, violation store) | gates, workers | Closes PRODUCTION-READINESS #1 (G2 host-RCE: `migration-apply-gate.ts:56` runs worker-written config via `sh -c`); verification-eval ideas 2+11 | https://github.com/anthropics/sandbox-runtime |
| 2 | Squawk + `atlas migrate lint` as G3 engines; atlas.sum-style migration-history hash fail-closed at G2 apply time | gates, merge | PRD:383 already names Squawk/Atlas as intent; hash catches mid-run history edits; deterministic, no network | https://github.com/sbdchd/squawk, https://github.com/ariga/atlas |
| 3 | Bors-style batched merge queue: test base + all done-branches as one integrated tree, bisect halves on failure + GitHub merge-group knobs (timeout, min/max group, only-merge-non-failing) | merge | Implements the still-unshipped PRD:885 board-level merged-result oracle; O(log n) failure isolation replaces serial per-branch gating in `src/orchestrator/merge.ts` | orchestration-scout ideas 1-2 |
| 4 | Sentinel exit protocol + consecutive-format-error breaker | workers, gates | Every worker declares completion with a magic string the pane watcher greps for; 3 malformed responses in a row aborts the attempt instead of burning the wall timeout. Replaces byte-count heuristics (Q33-adjacent) | https://github.com/SWE-agent/mini-swe-agent |
| 5 | Synthetic-bug gate calibration: SWE-smith-style procedural mutations on DevAgent's own code, assert G1-G6 (incl. the board-level oracle) catch them | selfbuild, gates | Measures gate sensitivity with a deterministic oracle — matches the "judge only on deterministic oracles" guardrail; doubles as the G6 test fixture generator | https://github.com/SWE-bench/SWE-smith |
| 6 | Two-sided G1 oracle: FAIL_TO_PASS (ticket acceptance criteria) vs PASS_TO_PASS (suite minus flaky list), plus an infra-failure class distinct from solution failure | gates, merge | Turns G1 from binary green into a SWE-bench-Verified-shaped oracle; separates harness failures (starvation/503 rows) from worker failures in ledger analytics | https://openai.com/index/introducing-swe-bench-verified/ |
| 7 | ACE delta-op lessons: ADD/UPDATE/DELETE ops merged deterministically in `src/lessons/guard.ts`; digest becomes a relevance-projected slice (top-k by measured impact) instead of newest-40-lines | lessons, selfbuild | UPDATE replaces the near-duplicate rejection that discards real refinements; DELETE lets curator strikes retire superseded lessons; Q39 impact telemetry scores the projection | https://arxiv.org/abs/2510.04618 |
| 8 | KV-cache hit rate as a measured, reported property of every worker turn (stable-prefix lint + cache-read/write ratio per turn) | context, workers | FR-GROK-03 cost ticks + fixed-offset `COMPACT_CONTEXT_MARKER` splice already produce the data; nobody in the competitive set reports this | https://manus.im/blog/Context-Engineering-for-AI-Agents-Lessons-from-Building-Manus |
| 9 | Write-protect `.git` inside worker sandboxes | gates, orchestrator | Kills the proven sweep-in class (another session's commit absorbing uncommitted worker changes — twice in project history); worker commits/pushes go through the orchestrator only | https://github.com/openai/codex |
| 10 | Verifier-scored fan-out ranking: rank candidates by aggregate gate score (G1 counts, G3 violations, G4 findings, oracle result) instead of first-to-green | orchestrator | LLM Monkeys: coverage without verifier selection does not convert to solved-rate; extends existing fan-out winner ranking | https://arxiv.org/abs/2407.21787 |
| 11 | Effort-scaling ladder in planner prompts + disjointness gate (`paths[]` in `parsePlan`) before widening a wave | orchestrator | Anthropic per-complexity budgets stop maximal fan-out on trivial goals; Cognition's implicit-decisions principle operationalized as disjoint-file-scope evidence | https://www.anthropic.com/engineering/built-multi-agent-research-system |
| 12 | Per-issue episodic memory (Reflexion): structured reflection per failed attempt (what failed, which gate, root-cause hypothesis) keyed by tracker issue, injected on retry | selfbuild | Extends Q27 retry memory from failure-class to full post-mortem; replaces the lossy ledger-tail research prompt | https://arxiv.org/abs/2303.11366 |
| 13 | Cattle sandboxes + session-log resume; named hook lifecycle (pre-dispatch, pane-start, gate-fail, pre-merge) with configurable handlers | fleet, selfbuild | Managed Agents pattern directly addresses the orphan-pane class; hooks end the fork-the-driver extension model for the bash loop | https://www.anthropic.com/engineering/managed-agents |
| 14 | Repo-map fallback provider: tree-sitter + PageRank 1k-token map behind the existing context-provider seam | context | Cheap deterministic degradation when LeanKG is stale/down; completes the FR-CTX degradation ladder | https://aider.chat/docs/repomap.html |
| 15 | Ledger time-horizon metric (METR TH) + cost-per-verified-merge per iteration | selfbuild, product | Log estimated human-hours of merged PRs; the loop's own capability curve steers which backlog categories get autonomy; generalizes Cognition's AI Productivity Guarantee into an in-band budget signal | https://metr.org/blog/2026-1-29-time-horizon-1-1/ |

### Explicitly rejected / deferred

- **A2A for internal wiring** — the protocol itself disclaims sub-agent use; consider
  only as an external AgentCard surface if third-party orchestrators should drive
  DevAgent workers (orchestration-scout idea 13).
- **Single-LLM blocking verdicts** in G4/curator — MT-Bench position/verbosity/
  self-enhancement biases; require deterministic confirmation or a swapped-order second
  judge (verification-eval idea 6/§4).
- **Chasing competitor surfaces** (Agent HQ BYO workers, multi-vendor panels) —
  commoditized; the measured state layer is where roadmap weight belongs.

## Competitive note (2026-09 refresh of §19.2)

Pricing converged everywhere to seats + pooled credits (Devin full/flex seats, Copilot
AI Credits, Kiro credits, Codex Go $8 tier); trigger surfaces commoditized (Slack /
Linear / PR-comment / issue-assign are table stakes); validation remains generic
self-review + CI fixers. The only outcome-economics outlier is Cognition's AI
Productivity Guarantee (vendor-scored "productive engineering hours", $10M backing).
White space intact for DevAgent: loop-level memory with measured impact, domain gates
G2-G5, outcome-priced economics computed in-band by the ledger, cache-aware orchestration
reporting. Full vendor-by-vendor detail: context-ecosystem-scout §2-3.
