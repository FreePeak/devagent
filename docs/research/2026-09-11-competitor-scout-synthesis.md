# Research: DevAgent competitor scout synthesis — how the field builds the automatic dev workflow (2026-09-11)

Method: eight parallel web scouts, same lens — mechanism detail of the
ticket→plan→execute→validate→PR→review loop (triggers, isolation, verification
and self-repair, human gates, metering). ~290 source URLs across the eight docs,
majority primary (official docs, engineering blogs, papers, changelogs), fetched
2026-09-11. `web_search` was rate-limited mid-run; discovery fell back to
curl+Brave + direct doc fetches (`.md` / `llms.txt` conventions), which did not
reduce primary-source coverage. Successor to `2026-09-07-internet-scout-synthesis.md`
(harness-pattern lens) and PRD §4/§19.2 (product/tricing lens, 2026-08-22); it does
not restate them.

## Source docs

| Doc | Coverage | Words / sources |
|---|---|---|
| `2026-09-11-competitor-devin.md` | Devin Brain+Devbox, microVMs, Outposts, automations, stacked PRs, Review, Security Swarm, Fusion, SWE-2, outcome pricing | 4.3k / 58 |
| `2026-09-11-competitor-copilot-codex.md` | Copilot cloud agent (renamed from "coding agent"), Agent HQ, HydraFusion, Codex cloud/CLI/SDK, cache-aware routing | 4.8k / 72 |
| `2026-09-11-competitor-oss-agents.md` | OpenHands V1 SDK, SWE-agent ACI, Agentless, AutoCodeRover, Aider, mini-SWE-agent, CodeMonkeys | 3.9k / 33 |
| `2026-09-11-competitor-enterprise-platforms.md` | Factory (Software Factory/Missions/Spec Mode), Cursor (Cloud Agents/Bugbot/PR Routing), AWS Q→Kiro/Transform, Amp | 4.4k / 68 |
| `2026-09-11-competitor-google.md` | Jules pipeline + Planning Critic, Gemini CLI hooks/checkpointing, Antigravity 2.0 verification ladder | 4.1k / ~30 |
| `2026-09-11-competitor-anthropic-claude-code.md` | claude-code-action pipeline, Agent SDK budget/permission contracts, routines, Managed Code Review, sandbox-runtime | 4.8k / 37 |
| `2026-09-11-competitor-startups.md` | Sweep (dead), Codegen→ClickUp, Qodo (review-only pivot), CodeRabbit, Greptile, Ellipsis, Roomote, Cline Kanban, Trae SOLO, Junie, Pilot delta | 5.0k / ~26 |
| `2026-09-11-competitor-workflow-patterns.md` | 12 cross-vendor patterns: trigger, plan gate, isolation tiers, snapshot/resume, secrets, verification, gates, metering, context, reliability, benchmarks, self-improving loops | 3.9k / 44 |

## The pipeline shape the whole field converged on

trigger (chat mention / issue assign / PR comment / **CI failure** / cron / webhook)
→ plan artifact with an approval or critic pass → isolated per-task environment
(microVM or ephemeral CI runner) with **default-denied egress** → code + run +
verifier → PR with attached evidence → review agent + human approve → agent
iterates on review comments and CI failures until green → cost/insight telemetry
curated back into knowledge. Divergences that still matter: cloud-ephemeral vs
OS-sandbox-on-your-hardware (only the second group's mechanisms are portable to a
local-first agent), action-complexity metering that excludes test-wait time
(Devin) vs request units (GitHub) vs raw tokens (BYO), and binary approval vs
risk-tiered autonomy ladders (Factory, Cursor).

## What changed in the last 7–20 days that moves the moat

1. **The migration white space narrows at the static layer.** CodeRabbit runs
   **Squawk by default** against migration-path globs (plus SQLFluff, Prisma lint);
   Ellipsis ships a prompt-only "Schema Migration Reviewer" template. **Still
   untouched by anyone**: executing migrations against a shadow DB, up/down
   reversibility, FK-integrity replay, async/race gating. PRD §3.2/§4 wording must
   narrow from "nobody validates migrations" to "nobody does *execution-based*
   migration validation" — static lint is no longer absent from the field.
2. **Evidence-per-PR is no longer unique.** Devin's test mode attaches chaptered,
   annotated screen recordings plus per-assertion pass/fail lists; Greptile TREX
   runs tests in a sandbox and attaches logs/traces/screenshots/video to the PR;
   Factory Automated QA posts screenshot evidence. What survives as differentiator:
   backend domain-gate *depth* (G2/G3/G4) — and Cognition is one product decision
   away from domain gates.
3. **Cache-aware orchestration is now a shipped feature, not white space.**
   GitHub's auto model selection switches models only at cache boundaries, bills
   cached tokens ~10%, and documents what invalidates the cache. The 2026-09-07
   white-space item #4 (cache-aware orchestration as reported feature) is closed by
   a competitor; DevAgent's remaining edge is measuring it *in-band* per worker turn.
4. **"Local-first" is under direct attack.** Devin Outposts (2026-07-22) runs
   sessions on customer machines via `devin worker start --outpost=`, Factory sells
   fully air-gapped deployments, Greptile/Cursor/Roomote offer self-host. The moat
   shifts from *where it runs* to *what it owns*: durable loop state, BYO-provider
   adapters, and gate depth.
5. **The orchestrator-over-CLIs pattern is now a product category.** Ellipsis
   Agent Cloud deploys agents-as-YAML in-repo (prompt/model/**harness**/trigger/
   budget) by merge; Greptile `/greploop` drives *any* coding agent against review
   findings until resolved; Roomote and Cline Kanban are open harness-agnostic
   loops. DevAgent's worker-adapter architecture is validated — and commoditized.
6. **Gate erosion at GitHub.** "Copilot approvals" (2026 preview) lets Copilot code
   review submit an approving review that *satisfies* required-approval branch
   protection. The "human approves every PR" assumption is weakening industry-wide;
   DevAgent's evidence-gated merge story should position against this explicitly.
7. **Deaths confirm the category is hard**: Sweep shut down (Feb–Mar 2026),
   Codegen absorbed by ClickUp, Qodo deprecated all code generation (2026-04-23)
   and went review+governance only. Qodo's stated reason is free moat marketing for
   DevAgent: "you never let the builder be their own inspector."

## Comparative snapshot (detail in the source docs)

| Vendor | Isolation | Validation/self-repair | Human gate | Metering |
|---|---|---|---|---|
| Devin | dedicated microVM/session, snapshot/resume; Outposts on customer infra | repo CI + Test mode (screen recording, assertions) + Devin Review + Security Swarm per-finding repro | plan-first, approval cards, mandatory security profiles | ACU/credits; action-complexity, test-wait exempt; outcome-priced (R²≈0.74) |
| Copilot cloud agent | ephemeral Actions runner, agent firewall (admittedly bypassable), **fails open** on setup errors | CodeQL/secret/dep scans + self-review loop pre-PR | draft PR, no self-approve; "Copilot approvals" erodes count-gate | AI Credits (per-token tables) + Actions minutes |
| Codex | cloud containers; CLI seatbelt/landlock | repo toolchain in container; cloud code review | PR review | plan quota + credits |
| Cursor Cloud Agents | cloud VM + `.cursor/environment.json`, 3 secret classes, egress proxy | Bugbot (+Autofix, **3 attempts/PR cap**) | per-directory APPROVAL_POLICY.md routing | tokens + $0.25/Mtok rate; Bugbot usage-based |
| Factory Droid | Seatbelt/bubblewrap, **refuses to start unsandboxed**; airgap ladder | Automated QA (drive the app, screenshots) + Security Review; Mission Mode = 2 validation roles/milestone | Spec Mode read-only until ExitSpecMode; autonomy Off–High org cap | seats + credits |
| Jules | per-task VM from validated env snapshots | in-VM tests + CI Fixer + Render deploy-fix | soft plan gate (auto-approves; Planning Critic audits) | tasks/day quotas |
| OpenHands V1 | Docker/runtime abstraction | Goal-Completion judge LLM + stuck detector (hard thresholds) + **TD-trained critic ranker** | issue-label trigger, PR handoff | per-role cost registry; org/lifetime budgets |
| Claude Code substrate | sandboxed bash, bubblewrap, OIDC | `/autofix-pr`, Managed Code Review (severity-tagged, neutral check) | hooks preToolUse approve/deny; routines without permission prompts | budget caps incl. subagent spend |
| Ellipsis | per-session scoped tokens, sandbox | incremental review; typed JSON exits | in-repo YAML control | **budget caps enforced before sandbox creation** |
| Greptile | sandbox per PR | TREX writes *and runs* tests; `/greploop` repair contract | review comments as gate | credits per review tier |

## Highest-value mechanics to import (mapped to DevAgent surfaces)

1. **Structured, schema-validated worker exits** — Devin `structured_output_required`
   and Ellipsis typed JSON-Schema session exits are the API-primitive version of
   backlog item #4 (sentinel exits). Adopt schema-required completion per worker
   adapter; malformed exits become a breaker class.
2. **Hooks as an external-gate contract on worker CLIs** — Gemini CLI exposes 11
   lifecycle hooks incl. `BeforeTool` (block/rewrite) and `AfterAgent` (force
   RETRY/HALT); Copilot `.github/hooks/*.json` preToolUse can programmatically
   approve/deny; Factory uses exit-2-with-stderr semantics. A normalized hook
   contract lets DevAgent bolt G-gates onto *third-party* workers without forking.
3. **Recorded, replayable orchestration** — Devin Dynamic Workflows records every
   agent call hash-keyed and re-runs downstream agents on prompt edit; this is the
   clean formalization of ledger-as-reducer. Apply to selfbuild-loop resume.
4. **Stacked PRs with silent conflict resolution and atomic bottom-up merge**
   (Devin) outclass serial per-branch merge gating; combine with the bors-style
   batched merge queue (backlog #3).
5. **Snapshot economics** — Devin hypervisor snapshot/resume across async gaps,
   Jules validated-env snapshots reused per repo, Codegen sandbox image snapshots.
   DevAgent analog: snapshot the validated Compose environment once per repo and
   reuse across tasks to amortize cold start.
6. **Documented retry bounds on reviewer→worker loops** — Cursor Bugbot Autofix
   caps at 3 attempts/PR "to prevent loops"; Codegen taps out to a human after 3;
   SWE-agent makes cost (not steps) the stop signal and ships partial diffs with
   typed exit status. DevAgent's watchdog should adopt explicit per-loop retry caps
   + cost-based termination + partial-diff-with-status semantics.
7. **Event-sourced evidence artifacts** — Jules activities carry
   `{command, output, exitCode}` bash artifacts and changeSet unidiffs with
   `baseCommitId`, immutable with cursor pagination. This is the API shape for
   DevAgent's evidence-per-PR: emit gate evidence as typed, replayable events.
8. **Risk-tiered autonomy instead of binary approval** — Factory autonomy
   Off/Low/Medium/High with an org-level `maxAutonomyLevel`; Cursor routes
   approvals per-directory via `APPROVAL_POLICY.md` and refuses to let a PR's own
   edits change its policy files (base-branch version wins — a directly portable
   anti-self-modification rule for gate config).
9. **Verification ladder with adversarial roles** — Antigravity's `/boost`
   (isolated worktrees, failed assertions loop back) and Sentinel→Orchestrator→
   Workers with **exclusive file ownership** plus Critic/Challenger/Auditor gates
   ("ensures tests genuinely pass") is the closest competitive mirror of DevAgent's
   role architecture; OpenHands shows a *trained* critic (TD-propagated test
   outcome, +5.8pts with 5 rollouts, public weights) beats prompted judging.
10. **Readiness as an intake gate** — Factory Agent Readiness scores repos on 9
    pillars and requires Level 4+ before Missions; `/readiness-fix` auto-remediates.
    DevAgent should gate autonomy on a measured repo-readiness score instead of
    trusting every repo equally.
11. **Pre-flight budget enforcement** — Ellipsis enforces per-session and trailing
    1/7/28-day caps *before* creating a sandbox, stops running sessions at cap with
    `budget_hit` events. Maps directly onto ledger budget caps + FR-GROK-03 ticks.
12. **Path-triggered deterministic rule injection** — OpenHands V1 skills inject
    rules on file-glob match with no model discretion: the mechanism for forcing
    migration-safety rules into worker context whenever `**/migrations/**` is touched.

## Benchmarks: the scoreboard is being deprecated

SWE-bench Verified is under contamination + flawed-test fire (OpenAI's own 59.4%
flawed-test audit; SWE-Bench Illusion), METR shows >16h task unreliability, and
vendor-claimed 93.9–96% Verified scores are unauditable. There is no credible
external scoreboard left. Implication (matches backlog #5/#15): whoever publishes
reproducible in-repo verifiers with cost-normalized results owns the eval
conversation — DevAgent's synthetic-bug gate calibration + cost-per-verified-merge
metric is that, if shipped and written up.

## Method notes and claim discipline

- All scouting done 2026-09-11; dates are attached to time-sensitive claims in the
  source docs. Load-bearing numeric claims carry sources in the per-cluster docs;
  single-source items are flagged inline (e.g. Copilot premium-request metering and
  Kiro spec/hook details — 403s on GitHub/Kiro billing pages; GPT-5.1-Codex-Max
  compaction; vendor-claimed Verified percentages).
- `web_search` outage handled via curl+Brave and direct `.md`/`llms.txt` doc
  fetches; no source-quality reduction observed vs the 2026-09-07 pass.
- Recommendation: fold items 1/2/7/10/11 above into the 2026-09-07 backlog as
  deltas (they sharpen, not replace, items #1–#6), and update PRD §3.2/§4
  white-space wording per change #1.
