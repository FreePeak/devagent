# Research: Competitor sweep — September 2026

Date: 2026-09-11
Scope: four parallel internet scouts re-verified the competitive landscape for
DevAgent's §4/§19.2. All facts below fetched 2026-09-11 from primary vendor
docs/pricing pages unless marked `[UNVERIFIED]`. This is the source doc;
PRD §4.1/§4.2 and §19.2 are refreshed from it and do not duplicate it.

Complements: `2026-09-07-context-ecosystem-scout.md` (prior scan), `pilot-probe.md`,
`openhands-sweagent.md`, `multica.md`.

## Headline findings

1. **The migration/DB-migration white space still holds.** Across 19 products
   probed against primary docs (Devin, Copilot cloud agent, Jules, Codex,
   Factory, Claude Code Actions, OpenHands, SWE-agent, Cline, Roomote, Aider,
   Qodo, Pilot, Codegen, Cursor, Amp, Ellipsis, Greptile, CodeRabbit), **zero**
   ship shadow-DB migration dry-runs, apply/rollback verification, schema-diff
   analysis, or FK/destructive-change detection. Adjacent evidence gathering —
   Devin's video proof of browser E2E, Cursor's screenshots/logs artifacts,
   OpenHands' `qa-changes` runs-the-software verifier — is behavioral and
   surface-generic, not domain-gated. Confirmed negatives, not assumptions:
   each scout explicitly searched vendor docs for migration/shadow-DB terms.
2. **Trigger surfaces have fully converged.** Slack + Linear/Jira +
   GitHub issue-assign/@mention + webhook + API + cron + mobile is table
   stakes at every tier now ("ticket in → PR out" is an interface commodity).
   Differentiation must be argued on gates and evidence, never triggers.
3. **Verification is becoming its own product layer** — and the nearest
   neighbors to DevAgent's pitch are *reviewers* expanding into delivery:
   Qodo 2.0 (blast-radius + rule mining + compliance-to-ticket), CodeRabbit
   ("Agentic Change Management", $143M raise claim on its pricing page),
   Greptile, Cursor Bugbot/Security Agents, Devin Review. None executes
   migrations; DevAgent's G2/G3/G4 remain uncontested.
4. **Ellipsis is the closest architectural analogue** (orchestrates Claude
   Code/Codex as workers, per-task sandboxes, "$0 seats, tokens + 10%"
   consumption pricing, BYO-VPC). DevAgent's claims must be phrased against
   this shape, not against Devin alone.
5. **Pricing converged on seats + pooled consumption credits**; several vendors
   now undercut per-seat pricing outright (Ellipsis $0 seats) or charge a
   BYO-key tax (Cursor $0.25/M tokens on Teams/Enterprise). Devin dropped
   self-serve ACUs to enterprise-only credits with flex seats. "Predictable
   per-task cost" survives as a differentiator only if DevAgent *reports*
   per-task cost in the PR body.
6. **One competitor is doing behavioral verification seriously:** OpenHands'
   Verification Stack (diff-review agent + `qa-changes` plugin that boots the
   app, reproduces the bug on base, posts PASS/FAIL/PARTIAL with evidence).
   Highest-priority OSS threat to watch; still no DB/migration gates.

## 1. Incumbents (cloud ticket-to-PR agents)

### Devin (Cognition)
- **Surface:** web, Slack, Teams, Linear, Jira, GitHub/GitLab/Bitbucket/Azure
  DevOps comments, API, email; event Automations (schedules, webhooks,
  incident.io). `devin /handoff` pushes local tasks to cloud; accepts handoff
  from Claude Code/Codex via Devin MCP.
- **Execution:** per-session hosted VM with snapshot "blueprints"; Windows
  sessions; **Devin Outposts** (self-hosted workers on customer infra, fleet
  API); Customer Dedicated Deployment; VPN/OIDC/customer KMS.
- **Validation:** runs repo tests/lint/typecheck in-VM until green; **video
  proof of end-to-end browser tests**; CI-failure auto-fix via Devin Review
  Auto-Fix; CodeQL-style "Security Swarm" scans with severity-gated scan
  validation; SonarQube remediation. **No migration gates** — its "migration"
  marketing is code migration (Java upgrades, JS→TS, COBOL), prompt-driven via
  Playbooks/Knowledge/Skills, never gate-driven. `[confirmed negative]`
- **Worker model:** own loop + own router; parallel sub-Devin sessions;
  Dynamic Workflows (deterministic Python orchestration). Does not spawn
  third-party CLIs as workers (but accepts their handoffs).
- **Pricing (2026-09):** ACUs now **enterprise-only** (order-form rate).
  Self-serve: Free; Pro $20/mo; Max $200/mo; Teams $80/mo min org, full seat
  $40/mo + free flex seats drawing on shared prepaid on-demand credits.
  Devin Desktop = rebranded Windsurf (JetBrains plugin, ACP into Zed/Xcode).
- **2025→2026:** Windsurf fold-in; ACU→credits switch for self-serve; Outposts;
  Devin Review standalone (devinreview.com); security suite relocated to
  /security with batch remediation API.
- **Sources:** docs.devin.ai (get-started, billing/self-serve, billing/usage,
  billing/enterprise, sdlc-integration, release-notes/2026,
  use-cases/migration-modernization, cloud/outposts/overview, cli).

### GitHub Copilot cloud agent (GitHub) — renamed from "coding agent"
- **Surface:** agents panel, issue assign, `@copilot` on PRs, VS Code, Slack +
  Teams (preview), security-campaign alert assignment; **Automations**
  (hourly/daily/weekly schedule; issue/PR events with search+path filters).
- **Execution:** ephemeral Actions-powered dev environment; one repo, one
  branch, one PR; hard **59-minute session cap**; firewall + MCP (GitHub +
  Playwright default-on); custom instructions, agent skills, lifecycle hooks,
  custom agents, Copilot Memory (preview).
- **Validation:** runs tests/linters in its sandbox; self-checks with CodeQL,
  Advisory-DB dependency scan (CVSS High/Critical), secret scanning; second
  opinion from Copilot code review. **Actions workflows do not run on the PR
  until a human with write access approves them** — repo CI is not in the
  loop by default. `[confirmed negative on migrations]`
- **Pricing (2026):** usage-based **AI credits, 1 credit = $0.01**; included
  per user/month: Business 1,900, Enterprise 3,900 (promo 3,000/7,000 for
  existing customers Jun 1–Sep 1 2026); org-pooled, no rollover. Cloud agent +
  code review consume Actions minutes **and** AI credits. Seats also meter
  Agent HQ third-party agents in the same pool.
- **2025→2026:** rename to cloud agent; premium-requests → AI credits +
  per-token; Automations; hooks; skills; Memory preview.
- **Sources:** docs.github.com/en/copilot/concepts/agents/cloud-agent/
  {about-cloud-agent, about-automations, risks-and-mitigations};
  concepts/billing/.../usage-based-billing.

### Google Jules
- **Surface:** web UI only (repo+branch picker, prompt, plan gate); API/CLI
  `[UNVERIFIED — not found in docs]`. GitHub-only repos.
- **Execution:** fresh cloud VM per task with internet; user setup scripts;
  no long-lived processes.
- **Validation:** builds/tests per your setup script, retries; **plan-approval
  gate** before code changes; no CI iteration guarantee, no scanner.
  `[confirmed negative on migrations]`
- **Pricing:** task quotas, no per-use billing: Free 15 tasks/24 h (3
  concurrent); Pro 100/15; Ultra 300/60. Individuals (@gmail, 18+) only.
  Free tier models Gemini 2.5 Pro; paid "starting with Gemini 3 Pro".
- **2025→2026:** experimental beta → Public Beta with paid tiers; AGENTS.md
  auto-read; auto-retry; notifications.
- **Sources:** jules.google/docs, /docs/faq, /docs/usage-limits.

### OpenAI Codex cloud (in ChatGPT)
- **Surface:** chatgpt.com/codex web, CLI/IDE/mobile, GitHub PRs, **GitLab
  (Beta)**, Linear, Slack; scheduled/event Automations; GitHub Action;
  delegate-from-ChatGPT; app-server/SDK embedding.
- **Execution:** isolated cloud container (public `codex-universal` image),
  setup + maintenance scripts, secrets stripped before agent phase, container
  cache ≤12 h, internet **off by default** in agent phase behind an HTTP
  proxy allowlist.
- **Validation:** terminal loop — edits, runs checks discovered from AGENTS.md;
  separate Code Review + Security Review products (`@codex review`,
  `@codex security review`, custom rules) and a **Codex Security** product
  line (CLI/plugin/SARIF/CI). `[confirmed negative on migrations]`
- **Worker model:** own models (GPT-5.6 Sol/Terra/Luna, GPT-6 Astra, 5.5/5.4,
  5.3-Codex-Spark preview); exposes itself as a worker to others via
  app-server/SDK and a Claude Code plugin.
- **Pricing:** bundled ChatGPT plans: Free $0, Go $8, Plus $20, Pro $100 (5×)
  / $200 (20×), Business $20/user/mo annual ($25 monthly), Enterprise contact.
  Rolling **5-hour windows + weekly limits per model**; local messages and
  cloud chats share the allowance; on-demand API fallback.
- **2025→2026:** docs moved to learn.chatgpt.com; GitLab beta; Codex Security
  as a product; automations/scheduled tasks; container caching; Windows/Linux
  desktop app with native sandbox.
- **Sources:** learn.chatgpt.com/docs/{cloud, environments/cloud-environment,
  pricing, developers, third-party/github, security}.

### Factory (Droid)
- **Surface:** Droid CLI (mac/linux/win ARM), Factory desktop app, web/mobile
  session sync, Slack (sessions onto Droid Computers), GitHub/Linear/GitLab
  triage→code-gen, SDLC-stage automations, API/SDK, headless exec.
- **Execution:** local worktree, cloud background sessions, **Droid Computers**
  (persistent managed 4 CPU/8 GB compute or BYOM on any VPS/on-prem;
  Remote-SSH support; iptables inbound-closed).
- **Validation:** "Software Factory" SDLC model (Private Preview): explicit
  Validate stage = Code Review + QA + **Security Audit as PR checks** ("PR
  Validations" metric); Release stage deployment gates; Triage stage
  classifies/routes inbound tickets; AutoWiki docs. `[confirmed negative:
  Validate stage is reviewer agents, no migration/schema execution gates]`
- **Pricing:** Pro $20 / Plus $100 (unlocks Droid Computers) / Max $200 /
  Teams $60/mo team + $40/mo seat (≤10 seats, 10 h/mo shared computer time);
  Enterprise custom (SSO/SCIM, audit, on-prem/hybrid). Multiplier tiers
  replaced token credits.
- **2025→2026:** CLI+credits → four-surface platform; ephemeral cloud
  templates deprecated for persistent Droid Computers; Software Factory SDLC
  automation preview; enterprise rollout/architecture docs.
- **Sources:** factory.ai/pricing; docs.factory.ai/{welcome/index,
  droid-computers/overview, software-factory/overview}.

### Claude Code GitHub Actions / web background agents (Anthropic)
- **Surface:** `@claude` in issues/PRs, review comments, issue opened/assigned/
  labeled, custom trigger phrases, any event with explicit `prompt` (incl.
  `schedule` cron); separate Code Review product (no workflow file); Claude
  Code on the web / `--cloud` / mobile; `/schedule` routines; Slack; GitLab CI.
- **Execution:** your GitHub runner (OIDC workload-identity federation), or
  Anthropic-managed cloud VMs with per-environment network levels + setup
  scripts + self-hosted environments option.
- **Validation:** **only what you wire up**; `actions: read` grants CI-result
  visibility; **Auto-fix pull requests** (per-PR toggle) reacts to CI check
  failures + reviewer comments (`/autofix-pr`). `[confirmed negative]`
- **Position:** the strongest *worker substrate* — Devin, Codex, Copilot
  third-party-agent interop all orbit it. DevAgent is its orchestrator, not
  its competitor; but Anthropic's own web/auto-fix/routines surface now
  overlaps DevAgent's ergonomics at subscription price.
- **Pricing:** no product fee — API rates or Claude subscription
  (`claude setup-token`); web/cloud + auto-fix gated to Pro/Max/Team +
  Enterprise premium seats (research preview).
- **2025→2026:** claude-code-action **v1.0 rewrite** (mode auto-detect;
  `prompt`/`claude_args` unified; legacy inputs deprecated); plugin/
  marketplace inputs; Foundry auth; structured JSON outputs; bot-actor
  controls (`allowed_bots`, `*[bot]`).
- **Sources:** github.com/anthropics/claude-code-action;
  code.claude.com/docs/en/{github-actions, claude-code-on-the-web}.

## 2. Open-source challengers

Star counts via shields.io badge JSON, fetched 2026-09-11.

### OpenHands (All Hands AI → Agent Canvas) — top OSS threat
- **What changed:** pivoted to **Agent Canvas**, a multi-backend control
  center: monorepo split (Agent Canvas + software-agent-sdk + automation +
  sandbox-server); legacy Docker GUI archived. Agents can be OpenHands' own
  SDK agent **or any ACP-compatible third-party CLI (Claude Code, Codex,
  Gemini)** — it now hosts DevAgent's workers.
- **Verification Stack (new, headline):** Layer 1 diff-reading code-review
  agent; Layer 2 **`qa-changes` plugin that actually runs the software** —
  boots the server, exercises real HTTP/CLI/browser, reproduces the bug on the
  base branch, posts PASS/FAIL/PARTIAL with evidence; plus an `iterate` skill
  looping fixes until clean; experimental LLM "Critic" + stuck detector.
  **Explicitly does NOT run the test suite** (delegates to CI). No
  migration/schema capability anywhere in the docs index (`llms.txt`
  reviewed) `[confirmed negative]`.
- **Surface:** GitHub/Slack/Linear/Jira/Bitbucket events, webhooks, cron
  Automations, CLI, API; local (npm/Docker/VM/Modal/K8s) or Cloud/Enterprise.
- **Distribution:** 87k stars, MIT, weekly releases. Cloud pricing dollars
  `[UNVERIFIED — pricing page unreachable]`.
- **Why it matters:** closest OSS analog — same triggers, now same workers,
  and its "run the software, post evidence" pattern is the nearest approach
  to DevAgent's G1/behavioral story. G2/G3/G4 remain wide open.
- **Sources:** github.com/OpenHands/OpenHands;
  docs.openhands.dev/{overview/introduction, openhands/usage/use-cases/qa-changes,
  llms.txt}; openhands.dev/blog/verification-stack.

### Qodo (ex-CodiumAI) — nearest "integrity" brand
- **What changed:** OSS **PR-Agent donated** to community →
  github.com/the-pr-agent/pr-agent (12.9k stars, MIT, moving to a foundation;
  Codium-ai/pr-agent is a legacy mirror). Product rebranded **Qodo 2.0**:
  agentic PR review, blast-radius risk assessment, cross-repo review, rule
  mining from PR history, context engine over tickets/PR history/business
  requirements, plus an "Agentic Toolbox" of CLI quality tools embedded inside
  your coding agent.
- **Validation depth:** analysis-layer — reviews + test-generation heritage
  (Qodo Gen: 902k VS Code / 648k JetBrains installs); compliance-to-ticket
  checks; **does not execute code or migrations** `[confirmed negative]`.
- **Pricing (qodo.ai/pricing, modified 2026-07-31):** credit-based,
  $0.012/credit pooled; packs 2,500/5,000/20,000 credits (~18/36/144
  reviews/mo); Pro Team no annual commitment; Enterprise SSO/BYOK/on-prem;
  free for OSS.
- **Threat:** medium — complementary reviewer, but owns the "code integrity"
  vocabulary DevAgent's gates also speak; naming collision risk.
- **Sources:** github.com/the-pr-agent/pr-agent; qodo.ai; docs.qodo.ai.

### Cline — IDE agent turned automation platform
- **What changed:** from VS Code extension to "autonomous coding agent as an
  SDK, IDE extension, or CLI assistant": headless CLI for CI/CD,
  `cline schedule create --cron`, `cline connect slack/telegram/discord/linear`,
  multi-agent teams (`--team-name`), @cline/sdk, desktop app, JetBrains plugin.
- **Validation depth:** runs tests/linters/compiler as it edits, checkpoints,
  auto-approve mode; no evidence gates, no sandbox isolation, no PR
  verification loop `[confirmed negative]`.
- **Distribution:** 68k stars, Apache-2.0; BYOK 200+ models; "8M+ developers"
  marketing `[UNVERIFIED]`.
- **Threat:** medium — duplicates DevAgent's dispatch surface (cron + Linear +
  Slack headless) with generic validation only.
- **Sources:** github.com/cline/cline README; cline.bot/pricing (JS-rendered,
  plan dollars unretrievable).

### Roo Code → Roomote — the sleeper
- **BOMBSHELL:** `RooCodeInc/Roo-Code` **archived 2026-05-15** (24k stars
  frozen; verified on repo banner). Org rebranded to **Roomote**:
  self-hosted "your own cloud coding agent", source-available Fair Core
  License 1.0 (converts to Apache-2.0), free ≤10 users, cloud from $49/mo.
- **What it does:** reads Linear/Jira/GitHub tickets → parallel agents → runs
  the app, takes screenshots → opens PR; preview URLs; audit trail;
  sandboxes: Modal/E2B/Daytona/Blaxel/local Docker; BYOK or ChatGPT-sub direct;
  MCP server so you can steer from Claude Code/Codex/Cursor.
- **Validation depth:** CI-aware, runs the app, screenshots; no formal gates
  or evidence bundles `[confirmed negative on migrations]`.
- **Distribution:** 232 stars (new repo) — unproven, young.
- **Threat:** medium-high conceptually — architecturally the nearest OSS
  neighbor to DevAgent (tickets→PR, self-host, BYOK, worker-agnostic); watch
  it for traction, and note the archived-Roo user base it inherited by name.
- **Sources:** github.com/RooCodeInc/Roo-Code (archive banner);
  github.com/RooCodeInc/Roomote README.

### Pilot (qf-studio) — refreshed vs `pilot-probe.md`
- 681 stars (665 on 2026-09-04), BSL 1.1, committed 2026-09-10 — active.
- Validation unchanged: generic test/lint/build gates + auto-retry +
  autopilot CI-watch auto-merge; **no migration/FK/race gates**.
- Worker model unchanged: Claude Code primary, OpenCode secondary.
- **Threat:** high conceptually (same loop, same workers), low adoption;
  BSL blocks commercial reuse.
- **Sources:** `pilot-probe.md`; shields.io refresh 2026-09-11.

### Codegen — commercial ticket→PR, OSS shell
- Trigger: Slack/Linear/Jira/ClickUp/Monday assign or @mention, GitHub
  `@codegen-agent`, web/CLI/API; one agent per ticket/PR/thread with shared
  context. **Checks Auto-fixer**: wakes on CI failure, greps logs, pushes
  fixes, retries ×3 (docs verified).
- Execution: Codegen-hosted sandboxes (closed cloud). Validation: CI check
  fixing + PR review feedback; no migration intelligence `[confirmed negative]`.
- OSS repo is an SDK wrapper (518 stars, Apache-2.0); pricing `[UNVERIFIED]`.
- **Sources:** github.com/codegen-sh/codegen; raw
  docs/capabilities/{checks-autofixer,triggering-codegen}.mdx.

### Defunct / stalled (do not profile in PRD §4)
- **Sweep (sweepai/sweep):** repo title now "AI coding assistant for
  JetBrains"; README is a farewell note. 7.7k stars, last commit Sep 2025.
  **Dead as a ticket→PR agent.**
- **Devika (stitionai):** the 2024 "open-source Devin" — 19.6k stars, MIT,
  last commit Sep 2025, unmaintained.
- **Aider:** 49k stars but stalled — last release Aug 2025, last commit May
  2026; interactive-only, never autonomous.
- **SWE-agent / mini-swe-agent (Princeton):** research baselines;
  SWE-agent 20k (deprecated as default in favor of mini), mini 7.4k, MIT;
  powers Ramp's SWE-Bench harness. `arizer` repo does not exist (404).
- **Cosine Genie:** cosine-ai org has **zero public repos**; status
  unverifiable.
- **goose (now Agentic AI Foundation):** donated to Linux Foundation's AAIF;
  canonical repo `aaif-goose/goose` (block/goose redirects), 54k stars,
  Apache-2.0, active. General local-first agent framework — infrastructure a
  DevAgent could be built on, not a delivery competitor.

## 3. Background agents and reviewer-adjacent

### Cursor (Anysphere) — largest-distribution threat in this sweep
- **Surface:** iOS app, cursor.com/agents web, Desktop cloud mode, Slack
  `@cursor`, GitHub/Bitbucket `@cursor` on PRs/issues, Linear `@cursor`, REST
  API; **Cursor Automations**: cron, GitHub/GitLab/Bitbucket events incl.
  CI-completed, Slack reactions, generic webhooks.
- **Execution:** isolated cloud VMs with full dev environments
  (`.cursor/environment.json` or snapshot), secrets, network egress controls,
  Tailscale/private connectivity, pre-baked Builds, multi-repo environments;
  self-hosted machines/BYO compute.
- **Validation:** runs the repo's own scripts/tests in-VM; handoff artifacts =
  **screenshots, videos, logs** + remote desktop control. **Bugbot** and
  **Security Agents** run as Cursor-managed review agents; PR Routing &
  Approval; analytics API (`cost_cents`, `severity`, `resolution_status`).
  Checks are **non-blocking by default (`neutral`)** `[docs]`. Deterministic
  blocking gates: none documented `[confirmed negative]`.
- **Migration intelligence:** **NONE** across cloud-agent, automations,
  Bugbot, Security Agent docs `[confirmed negative]`.
- **Worker model:** own loop + own models (Composer 2.5; co-trained
  Grok 4.5/4.6); does **not** orchestrate rival CLIs.
- **Pricing:** Start ₹649 (India); Pro $20; Pro Plus $60; Ultra $200;
  Teams Standard $40/user, Premium $120/user (5× limits); Enterprise pooled.
  Third-party models bill at API price; BYOK on Teams/Enterprise carries a
  **$0.25/M-token Cursor Token Rate** (the one vendor taxing BYO keys).
- **2025→2026:** "Background Agents" renamed **Cloud Agents**; IDE-centric →
  agents-first; Automations platform; Bugbot/Security/PR-routing as managed
  agents; Grok Bot distributed through Cursor (below).
- **Threat: HIGH.** Only migration-gate depth separates it from DevAgent's
  pitch; distribution gap is enormous.
- **Sources:** cursor.com/docs/{models-and-pricing, cloud-agent,
  cloud-agent/automations, bugbot, security-agents}; cursor.com/llms.txt.

### Grok Bot (xAI via Cursor) — PRD §20 benchmark, materially changed
- Bots are now **persistent AI teammates on Cursor-hosted cloud computers**
  (shared browser/filesystem/terminal, hibernation, durable disk, daily
  backups); group chats, Bot-to-Bot delegation, learned skills + routines
  (schedule), Slack links; can delegate to Cursor Cloud Agents; desktop app
  (macOS/Windows/Linux) + iPhone; local-machine execution from desktop.
- **No on-prem, no BYO-image, no VPN into your network**; US-hosted; shared
  egress IPs.
- Safety layer: per-action approvals + **"Auto Review" — an independent
  review model gating risky shell/computer-use/automation-write actions**;
  audit logs, 90-day Action Recording, OTel export, MCP allowlist, SCIM
  (Enterprise).
- **Pricing:** included in every paid Cursor individual plan and self-serve
  Teams; or link SuperGrok/SuperGrok Plus/Heavy/X Premium+; **weekly usage
  grant**, overage on-demand via Cursor; Cursor plan + SuperGrok link do NOT
  stack.
- **Relevance to §20:** the "reviewer-model gate" (Auto Review) and the
  weekly-grant metering are the two patterns worth citing; it is *not* a
  ticket→PR coding agent — coding happens via delegation to Cloud Agents.
- **Sources:** cursor.com/docs/{grok-bot, grok-bot/security};
  cursor.com/help/grok-bot/plans. x.ai/bot itself `[not reached]`.

### Ellipsis — closest architecture to DevAgent
- **Pivot:** AI code review SaaS → **managed cloud for coding agents**;
  explicitly orchestrates **Claude Code and Codex** as workers (pluggable
  `type: claude_code` harness per config).
- **Surface:** agents-as-code YAML in your repo ("live on merge"), cron,
  GitHub/Linear/Slack `@ellipsis`, inbound/outbound webhooks, REST API, agent
  CLI.
- **Execution:** declarative per-task sandboxes (`kind: environment`, image/
  setup script, cpu/memory); **BYO-AWS-VPC** deployment ("no credentials,
  code, or LLM calls leave it").
- **Validation:** dev sessions build/fix and **run tests** in-sandbox;
  per-run **budget** on review harnesses; durable per-PR conversation,
  transcripts/search, **structured output** ("agents exit through your JSON
  schema"), live steering. Evidence = review findings + session records;
  no execution gates over migrations `[confirmed negative]`.
- **Pricing (2026-09-11):** **"Your bill = Tokens + 10%"** — provider token
  cost, no markup, **$0 per seat**, no idle charges; CPU $0.142/vCPU-h,
  memory $0.024/GB-h; $100 new-org credit; free for individuals holding a
  Claude Code or Codex subscription; free for OSS.
- **Threat: HIGH** — same worker-orchestration + sandbox + consumption-only
  thesis. DevAgent's remaining differentiation is exactly the G2/G3/G4 gates
  and backend-domain evidence in the PR body.
- **Sources:** ellipsis.dev; ellipsis.dev/pricing.

### Amp (Sourcegraph)
- **Surface:** CLI (`amp`, `-ox` execute mode, streaming JSON), web, macOS/iOS,
  TUI, Slack via personal "Puck", cron Automations; **event-driven Orbs** =
  webhooks you implement in an Amp plugin (no native Linear/Jira/issue-assign
  trigger).
- **Execution:** **Orbs** — Amp-managed remote machines, fresh isolated env
  per thread, sleep when inactive (burst 20 metered, then 1 per 5 min);
  Portals to expose running services; `amp sync` mirrors diffs locally.
- **Validation:** best *self-verification* rhetoric in the set but it is an
  instruction, not a gate: "tell it to prove that its changes work before you
  look at them… the thread contains the conversation, the code changes, the
  running services, and the proof." No schema/migration anything
  `[confirmed negative]`.
- **Pricing:** Megawatt $20/mo (750 orb-hours + $20 usage; link ChatGPT /
  X Premium+ / SuperGrok for unlimited-through-that-sub); Gigawatt $200/mo
  (1,000 xxlarge orb-hours + $200 usage); Unconstrained = API pricing,
  **BYO keys, no markup**.
- **Threat:** medium — shares DevAgent's BYO-keys/local-first sensibility;
  hands work to a human rather than shipping evidenced-green PRs.
- **Sources:** ampcode.com/docs/{pricing, orbs, orbs/event-driven}.

### Zencoder
- Repositioned as "Zenflow" agentic work platform + IDE/desktop plugins;
  GitHub/Slack + "2,000 integrations"; issue→PR mechanics `[UNVERIFIED —
  docs unretrievable this pass]`.
- **Pricing (verified):** Pro $45/mo ($40 annual) 30k credits/seat;
  Pro Plus $95 ($85) 80k credits shared pool + SSO/audit; Pro Max $195 ($175)
  180k credits; Enterprise prepaid, BYOK consumes **no credits**.
- **Threat:** medium — its BYOK-no-credit rule directly attacks
  predictable-cost positioning; verification depth undocumented.
- **Sources:** zencoder.ai/pricing.

### Reviewer cluster: Greptile, CodeRabbit (DevAgent's moat proxies)
- **Greptile:** codebase-aware PR review; free Starter (50 credits/mo),
  Pro $30/seat/mo (50 credits + $1/extra), Enterprise self-host; heavyweight
  "TREX" review = 3 credits. No execution/CI gating documented
  `[docs not reached]`.
- **CodeRabbit:** rebranded to **"Agentic Change Management"** — the control
  layer for agent-generated change. Surfaces: GitHub/GitLab/Azure
  DevOps/Bitbucket, Jira + Linear, **Slack/Discord agent that investigates →
  plans → opens PRs**, CLI pre-commit ("works with Claude Code, Cursor,
  Codex, Gemini"), Plan (issue/PRD → coding plan → any coding agent).
  Validation = bug/race-condition detection, **50+ OSS linter/security-scanner
  integrations**, Walkthrough with sequence diagrams, Change Stack, Security
  Review + continuous security monitoring. No execution sandbox, no migration
  gates `[absence across docs index + pricing matrix]`.
  Pricing (annual): Essentials $24 / Team $48 / Advanced $72 per dev/mo;
  Enterprise custom. Banner claims **$143M raised** (date/round `[UNVERIFIED]`,
  MarketMap cross-check pending).
- **Why they matter:** they own the "verify AI change" narrative and are the
  most plausible vendors to bolt execution gates onto DevAgent's wedge. The
  race is not whether verification sells — it is domain depth (DB/migrations/
  async) versus generic review.

## 4. Market map, funding, leaderboards (MarketMapSweep)

Method note: the fourth scout's only reliable search surface was Google News
RSS (`news.google.com/rss/search`, ~20 queries); Bing RSS degraded and direct
reader fetches of most article bodies did not resolve through the redirect, so
facts below are graded **headline-verified** (outlet + date from Google News
RSS) vs **body-UNVERIFIED**. Cross-check before quoting numbers externally.

### New entrants / structural changes since 2025
- **GitHub Agent HQ is the 2026 platform story.** Announced 2025-10-28 ("any
  agent, any way you work"); 2026-02-04 **Claude and OpenAI Codex run natively
  inside GitHub/VS Code/GitHub Mobile** (public preview, Copilot Pro+/Enterprise):
  multi-agent assignment (same task to Copilot+Claude+Codex, diff the
  approaches), org allow/deny agent policies, audit logs, code-quality check
  preview. Partners "Google, Cognition, xAI" named next; **no self-serve listing
  program for third-party agents found** (UNVERIFIED absence) — so DevAgent
  cannot list today, and Agent HQ + Copilot CLI is the integration surface to
  watch because *DevAgent's worker-orchestration layer is exactly what Agent HQ
  absorbs*. (github.blog 2026-02-04; CNBC/VentureBeat/The Verge 2025-10-28)
- **GitHub Spark deprecated 2026-08-04** — GitHub pruning non-core agent
  products; Copilot Workspace's 2026 fate not reached (likely absorbed into
  Agent HQ/coding agent, UNVERIFIED).
- **Consolidation:** SpaceX acquired **Cursor/Anysphere for $60B** (Reuters/WSJ/
  CNBC headlines 2026-06-16; terms body-UNVERIFIED), then reportedly bid for
  Cognition (2026-08-19, denied by Cognition's CEO). **OpenAI acquired Ona**
  (agent cloud, Bloomberg 2026-06-11). **Databricks acquired Electric** "to give
  every AI agent its own Postgres" (The New Stack 2026-08-11) — direct adjacency
  to DevAgent's DB-safety wedge: infra vendors building agent-native DB sandboxes.
  **Anaconda acquired Kilo Code** (2026-07-15).
- **New independents:** **Entire** (ex-GitHub CEO Dohmke, "answer to the crush of
  AI coding agents", GeekWire 2026-07-08); **Mechanize** (Google reportedly in
  $1.5B+ talks, BI 2026-08-05, UNVERIFIED); **8090** (Chamath, $135M 2026-06-30).
- **Validation is becoming a funded category:** **Neo** $100M (secure AI software,
  2026-07-20), **Blacksmith** $45M @ $550M ("AI-generated code drives validation
  demand", 2026-08-13), **Baz** $17M (review-to-planning, 2026-06-29), **Niteshift**
  $7M (agent cloud infra, 2026-06-13). Plus **AWS frontier agents** (DevOps GA
  2026-03-31, FinOps 2026-06-09) and **AWS "Dogwood" runtime verification for AI
  agents** (2026-08-06).

### Cognition / Factory scale (relevant to §4 "who can win")
- **Cognition (Devin): Series E $2B at $48B** (Reuters 2026-09-08, Bloomberg/WSJ,
  FinSMEs 2026-09-10), on top of $1B @ $25B pre (2026-05-27); ~$900M ARR reported
  (Dealroom 2026-08-28, UNVERIFIED); acquired **Poke** (2026-07-24) and the
  **Dioxus** team (2026-09-10). "Devin writes 89% of its code" (tech-insider blog,
  UNVERIFIED).
- **Factory: $150M Series C at $1.5B** (Khosla + Sequoia, 2026-04-17); later
  Blackstone-backing-at-$3.5B report (City AM 2026-08-03, UNVERIFIED).
- **Replit $400M @ $9B** (2026-03-12); **Asana acquired Stack AI $75M** (2026-05-29).

### Leaderboard status (is generation commoditising?)
- **SWE-bench Verified is saturating** (official data, SWE-bench/swe-bench.github.io
  `data/leaderboards.json`, 180 Verified entries, fetched 2026-09-11): top band is
  **79.2%** (Sonar Foundation Agent + Claude 4.5 Opus, 2025-12-05; live-SWE-agent +
  Claude 4.5 Opus medium, 2025-12-15), then TRAE + Doubao-Seed-Code 78.8%,
  live-SWE-agent + Gemini 3 Pro 77.4%, Atlassian Rovo Dev 76.8%. The board is now
  dominated by **agent harnesses on frontier models** (TRAE, Warp, Harness AI,
  EPAM AI/Run, Sonar, JoyCode) — a tier the current §4 table omits entirely.
- **Terminal-Bench 4.0** (tbench.ai, 2026-09-11) is the live long-horizon board;
  exact top scores **UNVERIFIED** (client-rendered). Headlines: GPT-5.5 narrowly
  beat Claude Mythos Preview on TB 2.0 (VentureBeat 2026-04-23). **SWE-Lancer**
  shows no 2026 movement (dormant, UNVERIFIED absence); SWE-bench Pro (Scale,
  2025-09-19) is the harder successor gaining mindshare.
- Reading: resolved-rate is plateauing as a differentiator — supports DevAgent
  competing on *trust per PR* (gates/evidence), not raw solve capability.

### Verification-gap evidence (does anyone validate migrations?)
- **The gap holds, and now has an incident paper trail:** "Claude Code accidentally
  wiped 2.5 years of data" (Times of India 2026-03-11); "Claude Opus 5 wiped an
  entire production database in minutes" (CyberSecurityNews 2026-07-30);
  "Database wipeout: what AI autonomous actions risk in IT" (Spiceworks
  2026-03-13); "The AI translated 30 years of COBOL perfectly. Then it crashed the
  database" (The New Stack). Headline-verified; bodies not fetched — **verify a
  named incident before citing it in marketing.**
- **No coding-agent vendor advertises schema-diff / shadow-DB dry-run / rollback
  verification as a product gate** (multiple phrasings searched; only infra-layer
  content surfaced). §4.2 claim 1 stands as of 2026-09-11. Closest is Augment
  Code's content roundup "8 AI Coding Agents That Actually Accelerate Database
  Schema Migrations" (2025-10-24) — a listicle, not a verification product.
- **But infra/DB vendors are colonizing the adjacent layer:** Oracle "Agent-safe
  change delivery on Oracle: discovery, online mechanics, idempotent migrations,
  provable rollbacks" (2026-05-27); AWS Dogwood runtime verification (2026-08-06);
  shadow-mode CI. The moat is safe but *not empty* — DevAgent's differentiation
  must stay delivery-loop-integrated (gate the PR), not DB-platform-integrated.

### Taxonomy the PRD should adopt (§4 layers the flat table misses)
1. Model vendors (Claude 4.x/5, GPT-5.x-Codex, Gemini 3, DeepSeek V4-Pro, Qwen,
   MiniMax, open-weight Poolside Laguna).
2. **Agent harnesses/CLIs** measured on SWE-bench/Terminal-Bench (Claude Code,
   Codex CLI, Cursor, TRAE, Warp, Harness AI, EPAM, Kilo, goose) — absent from §4.
3. Cloud autonomous agents (Devin, Jules, Codex cloud, Copilot cloud agent,
   Factory) — what §4 currently treats as the whole market.
4. **Multi-agent control planes** (GitHub Agent HQ, Databricks Omnigent + Unity
   AI Gateway budgets, Entire, Ona-under-OpenAI) — the biggest structural miss;
   sits *above* DevAgent and commoditizes trigger plumbing.
5. **Verification/eval infrastructure** (SWE-bench family, terminal-bench,
   Blacksmith, Neo, AWS Dogwood, shadow-mode CI, GitHub Code Quality) — DevAgent's
   moat now has named neighbors; must position against them, not just "repo tests".
6. Ops/incident agents (AWS DevOps/FinOps/Security, Gemini Cloud Assist).
7. Analyst/market layer (Gartner quadrant 2026-06-05, Omdia Universe 2026-03-31,
   market-size ~$19.4B by 2035 claims, a16z/Sequoia theses) — for §4 framing only.

Positioning note: platform consolidation (SpaceX/Cursor, Agent HQ, OpenAI/Ona,
Databricks/Electric) is the *cloud computer* pole; DevAgent's local-first/BYO
posture (§20) is the opposite pole — worth stating explicitly in §4.

## 5. What this means for PRD §4 (applied in the same change)

- §4.1 table: refresh Devin/Copilot/Jules/Codex/Factory rows (pricing + rename
  facts), add **Cursor Cloud Agents** and **Ellipsis** rows (biggest misses in
  the current table), and move OpenHands' row text to the Agent Canvas +
  Verification Stack reality.
- §4.2 white space: claims 1–3 survive re-verification *with one amendment* —
  the phrase to keep is "nobody validates **database migrations**"; competitors
  now advertise verification generally (OpenHands qa-changes, Devin video
  proof, Cursor artifacts, CodeRabbit scanners), so §4.2 must explicitly
  distinguish *behavioral evidence* (exists) from *domain gates* (nobody).
- New white-space item worth adding: **the worker-orchestration slot is
  contested** (OpenHands ACP, Ellipsis harnesses, Roomote BYOK all run
  Claude Code/Codex) — DevAgent's moat is not "can spawn other agents", it is
  what the gates prove about the output.
- Pricing: replace "fixed-gate design gives predictable per-task cost" with a
  verifiable claim (per-task cost computed and attached to each PR), since the
  market moved to transparent consumption (Ellipsis tokens+10%, Amp no-markup).
- §19.2: keep the original seven profiles but correct stale facts (Devin ACU
  enterprise-only, Copilot rename/credits, Jules quota change, Codex plan
  ladder, Factory tier numbers) and add Cursor Cloud Agents + Ellipsis; point
  at this doc for the 2026-09 per-product sources.
