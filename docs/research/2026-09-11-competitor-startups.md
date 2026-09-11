# Long-tail ticket→PR & PR-review products — automatic dev-workflow anatomy (2026-09-11)

Scope: breadth refresh of the long tail of DevAgent's competitive landscape beyond the 7 names in PRD §4 and the 2026-09-07 scouts (`docs/research/2026-09-07-*.md`). Lens: **mechanisms** — trigger → plan → execute → validate → PR → review/repair, isolation tech, retry policy, human gates, cost metering. Product status changes (deaths, acquisitions, pivots) are recorded explicitly. Pilot is not restated — deltas only (`docs/research/pilot-probe.md`).

## Sweep AI (sweep.dev)

**What it is / status (2026-09)**: DEAD. YC S23 "AI junior dev" that turned labeled GitHub issues into PRs (the original ticket→PR template). Pivoted ~2024/25 to a JetBrains autocomplete+agent plugin (repo README at `github.com/sweepai/sweep` is now only a redirect notice to the plugin; 7.7k stars, last repo push 2025-09). Reddit threads in r/Jetbrains and r/Zed (Feb–Mar 2026) report the whole company shut down with no announcement; `docs.sweep.dev` now serves `DEPLOYMENT_DISABLED`. Warning: ServiceNow's Sept-2026 "Sweep" acquisition is a *different* company (agentic CRM) — do not conflate.

**Mechanics of the dead product (for the record)**: issue label `sweep` → plan pass (model proposes file-level diff plan) → sandboxed branch edit → test run → PR. No evidence of domain validation; repair loop was comment-driven re-runs.

**What DevAgent can learn or steal**: cautionary datapoint — a first-mover issue→PR agent without validation depth or a sticky surface died twice (pivot, then shutdown). "The models weren't ready, and honestly, neither was the use case" (founder, LinkedIn, 2026).

**Sources**: sweep.dev (current JetBrains site, changelog last 2025-12-01); github.com/sweepai/sweep README; LinkedIn company post; reddit.com/r/Jetbrains/comments/1sbhsr5 + r/ZedEditor/comments/1sf2rpy; calcalistech.com (ServiceNow/Sweep — different company).

## Codegen (codegen.com → ClickUp)

**What it is / status (2026-09)**: Slack/Linear/Jira agent platform ("agents in your SDLC"), **acquired by ClickUp, announced 2025-12-23**. Post-acquisition, codegen.com became an SEO tool-directory; the product lives inside ClickUp Brain ("assign any ClickUp task to the Codegen agent"), while docs.codegen.com still documents the standalone platform. Enterprise traits: on-prem deployment option, API/CLI/SDK, org+repo rule layering.

**Trigger surfaces**: Slack thread, Linear/Jira/ClickUp/Monday ticket assignment or @mention, GitHub `@codegen-agent` comment on issues/PRs, web dashboard, CLI (`codegen agent create`), REST API.

**Workflow pipeline**:
1. Trigger routes to a **dedicated agent per context** (one agent per Slack thread / ticket / issue, full conversation history retained; ticket and resulting PR share the same agent).
2. Context assembly from the connected tool (task description, comments, linked docs).
3. Execution in a cloud sandbox; commits + PR.
4. **PR review agent** runs a first pass on every PR (org-level + repo-level rules combined; inline comments, security scan, architecture feedback).
5. Agent **monitors its own PRs** and answers review comments.

**Isolation & sandbox**: cloud VM sandboxes with a documented Docker base image, **filesystem snapshots for fast init**, per-context filesystem persistence (deps survive across Slack turns), secret injection, network restrictions, VSCode remote editor into the sandbox, web preview for dev servers. Sandbox-setup-log AI analyzer in the API.

**Validation & self-repair loop**: **Check Suite Auto-fixer** — on any failed GitHub check on an agent PR, agent "wakes up", greps CI logs for failure points, pushes fix commits to the same branch, re-validates. **Default 3 retries then "taps out" → flagged for human review**; Enterprise can set retry counts per repo/check. API supports banning/unbanning checks per run (kill switch for runaway fix loops).

**Human gates & approval UX**: human review after the AI first-pass; tap-out escalation; agent-run traces retrievable via API.

**Cost model**: cost-per-run captured in the sandbox execution trace; pricing no longer public post-acquisition (single source for pre-acquisition credit pricing not restated).

**What DevAgent can learn or steal**: (1) retry cap + escalation-to-human as a first-class configured knob (matches our breaker but per-PR and user-visible); (2) `ban checks` kill switch per agent run; (3) sandbox image snapshots to kill cold-start cost — directly relevant to our per-run compose cold starts.

**Sources**: docs.codegen.com (triggering-codegen, checks-autofixer, pr-review, sandboxes/overview, llms.txt); clickup.com/blog/clickup-codegen-acquisition.

## Qodo (Gen → review-only pivot)

**What it is / status (2026-09)**: ex-CodiumAI. **Deprecated all code generation (autocomplete + chat codegen) on 2026-04-23**; now "AI codebase quality and governance platform": PR review, code governance, Agentic Toolbox (review/rules exposed *inside* coding agents). Positioning quote: "the tool generating code should not be the same tool reviewing and verifying it… you never let the builder be their own inspector."

**Trigger surfaces**: PR opened/reopened/marked-ready (auto), `/agentic_review` PR comment, `handle_push_trigger` for per-commit persistent reviews; `.pr_agent.toml` (PR-Agent OSS heritage) or portal config. No issue→code path remains.

**Workflow pipeline**:
1. Trigger → review-in-progress placeholder comment.
2. **Multi-agent review**: specialized agents (correctness, standards, architecture, risk) run in parallel over full-repo context; a **judge agent** merges findings, dedups, drops low-confidence; severity-ranked findings with what/why/how-to-fix anatomy.
3. Findings strikethrough automatically when resolved (incl. by fixes in related repos).
4. **Remediation agent** (Research Preview): fixes findings at/above a severity threshold; by default **creates a child branch and a separate remediation PR** (`open_new_pr`, auto-close after done); chat-driven fix or auto-fix at scale.
5. **Rule Miner** continuously converts the team's PR history into enforced rules; PR-history indexing weights findings by what the team historically fixed vs dismissed.
6. New surfaces: **blast radius** risk assessment, **cross-repo code review** (flag a PR that breaks an API/data model owned by another repo), requirement gaps (linked issue/spec compliance), UX deviations (Figma drift).

**Isolation & sandbox**: review runs in Qodo's cloud; on-prem deploy supported. No build/test execution loop documented — verification is analysis + your CI.

**Validation & self-repair loop**: see remediation agent; one-click committable suggestions for single-file fixes; larger fixes ship as "agent prompts" to hand to your coding tool.

**Human gates**: human reviewer is the merge gate; `@qodo fix` flows produce PRs for review; dismissals are learned.

**Cost model**: **$0.012/credit, pooled across the team**; credit packs (2,500 ≈ 18 reviews/mo, 20,000 ≈ 144/mo → ~140 credits/review); unlimited-reviews tier; no per-seat headline.

**What DevAgent can learn or steal**: (1) **judge-agent + confidence filter over parallel specialized reviewers** is the exact shape of our G4/curator layer — they productized the bias hygiene we planned (swap-order second judge still ours alone); (2) **Rule Miner: rules synthesized from the repo's own merged-PR history** — a natural extension of DevAgent lessons (mine the ledger/PR history for gate rules); (3) remediation-as-separate-PR (keeps the audited PR clean); (4) their independent-verification thesis is free marketing for our gate design.

**Sources**: docs.qodo.ai (code-review, use-qodo-in-prs, remediate-findings-in-prs, qodo-ide, llms.txt); qodo.ai/blog/an-update-on-code-generation-at-qodo (2026-04-23); qodo.ai/pricing.

## CodeRabbit

**What it is / status (2026-09)**: the scale leader of this cluster: **$143M Series C at $1.5B valuation (2026-08-12**, Atomico/Smash co-led), repositioned as "**Agentic Change Management**" — the control layer for changes made by humans *and* agents. Thesis: "issue tracking is dead… the backlog is moving from tickets to pull requests; the PR becomes the auditable planning and decision point." Cites: GitHub on pace for 14× commits; autonomous agents open 35% of PRs at 90th-percentile adoption (Jellyfish). Per our classification: still **review-first**, but now with a genuine Slack→PR loop and merge-time gates.

**Trigger surfaces**: PR opened/updated (auto), `@coderabbitai` commands, CLI (pre-commit, works with Claude Code/Cursor/Codex/Gemini), IDE extensions, **CodeRabbit Agent for Slack** (mention → investigate → plan → open PR), connected Jira/Linear for planning context.

**Workflow pipeline**:
1. PR opened → **walkthrough comment**: file-change summary, Mermaid sequence diagrams, 1–5 review-effort estimate, related issues/PRs, suggested labels + reviewers (with NL rules, auto-assign), **linked-issue assessment** (checks PR actually delivers the issue; feeds merge gates).
2. Inline findings with one-click fixes; chat for follow-ups; **Learnings** from conversations persist as review preferences.
3. **Autofix** (Essentials+): applies fixes for unresolved findings as a commit or a **stacked PR**; Finishing Touches: generate unit tests, docstrings, resolve merge conflicts, custom recipes.
4. **Pre-Merge Checks** (Essentials+): built-in checks (docstring coverage ≥ threshold, etc.) + **custom checks written in natural language**, each `error` or `warning` mode; `issue_assessment` can be enforced as a merge requirement; rerun on demand, `@coderabbitai ignore pre-merge checks` to unblock.
5. Post-merge actions (changelog, tickets, notifications); Triage (rank PR queue by value/risk); Change Stack (layered diff walkthrough); Security (repo scans, attack surface, per-PR security review).

**Isolation & sandbox**: review-side tool execution sandboxed (e.g., ESLint "sandboxed execution" per docs); **Slack agent runs in a per-thread sandbox with its own git worktree branch + snapshot chain** — thread starts restore the latest snapshot, run completes snapshot; a *shared workspace* sandbox (admin-configured runtimes, package setup, reset-to-clean; `sqlite3` among guaranteed tools) — shared across the team, scoped by "Scopes" (repos + spend limits per conversation).

**Validation & self-repair loop**: 50+ deterministic linters/security scanners run on every PR **including Squawk (Postgres migration linter, v2.63, enabled by default)** on migration-path globs (`**/migrations/**`, `**/db/migrate/**` …), plus SQLFluff and Prisma Lint; Squawk is skipped if CI already runs it; chill/assertive profiles; `.squawk.toml` respected. Repair: Autofix + agent-driven fixes from Slack threads.

**Human gates**: human reviewer is the merge decision; pre-merge checks are the enforcement layer; "request changes workflow" syncs review decisions with resolved feedback and checks.

**Cost model**: per-seat: Essentials **$24**/dev/mo, Team **$48**, Advanced **$72**, Enterprise custom; PR-review rate limit 5/dev/hour; free for OSS. Slack agent metered in **agent-minutes** with a usage dashboard.

**What DevAgent can learn or steal**: (1) **linked-issue assessment as a merge-blocking gate** — direct analog to our ticket-acceptance check; ship it as a gate with evidence, they only do NL diff; (2) **NL-defined custom checks with error/warning modes** — a nicer gate-authoring UX than our config-only gates; (3) Squawk-by-default means static migration lint is now table stakes (see synthesis); (4) stacked-PR autofix; (5) their "PR as auditable planning point" framing validates evidence-attached-per-PR.

**Sources**: docs.coderabbit.ai (llms.txt, pre-merge-checks.md, autofix, slack-agent, slack-agent/sandboxes.md, walkthroughs.md, tools/squawk.md); coderabbit.ai/blog/introducing-agentic-change-management; coderabbit.ai/pricing.

## Greptile

**What it is / status (2026-09)**: review-first, moving toward "the **central validation layer**" for all agent-generated code. 22k+ teams claimed; Agent v4 shipped (late 2025); TREX in early access.

**Trigger surfaces**: PR opened/pushed (configurable: every push, on request, by path/label filters), custom rules triggers.

**Workflow pipeline**:
1. Graph index of the repo (files/functions/dependencies).
2. **Swarm of parallel review agents** assess the diff + impact beyond the diff.
3. Review payload: 0–5 **confidence score**, PR summary, sequence/flow diagrams, **auto-generated unit tests for changed code**, inline findings with one-click "fix in Claude/Codex/Cursor" handoff.
4. Learns standards by reading the team's PR comments; custom rules incl. per-path `./greptile/rules`.
5. **TREX** (early access): autonomously **writes and runs tests for every PR in a sandbox** — spins up services, dev servers, mocks, API calls, and **browser agents clicking through UI**; failure → PR comment with **logs, screenshots, traces, scripts, video** attached; claims ~20% more bugs than review alone.
6. **`/greploop`**: any coding agent (Claude Code, Codex, Devin…) iterates against Greptile findings **until all issues are resolved**; Greptile MCP shares comment context.

**Isolation & sandbox**: cloud sandbox per TREX run; **self-hosted deployment in your own AWS, even air-gapped, with BYO LLM providers**.

**Human gates**: human reviewer consumes confidence score + evidence; no autonomous merge.

**Cost model**: **credits**: Pro $30/seat/mo incl. 50 credits; **1 credit = 1 standard review, 3 credits = 1 TREX review**, extra credits $1; Starter free (1 dev, 50 credits).

**What DevAgent can learn or steal**: (1) TREX is the closest thing to our G1 in the market — **evidence-attached runtime verification** (logs/screens/traces on the PR) is exactly our evidence-attached-per-PR moat, and they're charging 3× for it; (2) `/greploop` — turn reviewer findings into an agent-iteration contract; (3) confidence-score-per-PR as a merge recommendation; (4) the "validation layer for any agent" positioning is a direct challenge to our differentiation narrative — but TREX is still agent-run-tests, not domain gates.

**Sources**: greptile.com (+ /agent.md, /trex.md, FAQ on homepage).

## Ellipsis

**What it is / status (2026-09)**: YC W24 code-review bot → pivoted to the **Ellipsis Agent Cloud (2026-07-28)**: "managed cloud for coding agents… the harness and the model are config fields; the governance is the platform." Runs Claude Code (live) and Codex (coming) in cloud sandboxes; claims most-used AI code review among YC companies.

**Trigger surfaces**: YAML-declared triggers — cron + EventBridge `rate/cron/at`; PR react events (`opened/pushed/merged/closed/review_submitted/commented`) with branch/path/label/repo-include-exclude/bot-vs-user filters; push (branch+paths); issue opened+label; `linear_issue`, `sentry`, `slack_channel`; mentions on GitHub/Slack/Linear; dashboard/API/CLI on-demand.

**Workflow pipeline**:
1. **Agent-as-code**: a YAML file in `agents/` — model, harness, prompt, repositories, permissions, budget, typed output. Deployed by merging to the default branch; invalid edit → sync error while last valid config stays live; every behavior change is a reviewed PR diff.
2. Trigger fires → **session** starts: isolated cloud sandbox, repos cloned, **scoped token minted per session** (narrower than the install grant, enforced by GitHub, revoked at session end) — "a tool or prompt injection cannot exceed it."
3. Session runs the harness (e.g., Claude Code) with the YAML prompt; **typed output**: declare a JSON Schema and every session exits through it (fails loudly otherwise).
4. Work lands as PRs/comments/answers; every session fully recorded (trigger, transcript, tool calls, output, cost) and searchable.

**Isolation & sandbox**: one isolated cloud machine per session; parallel sessions on the same repo don't collide; work survives the laptop; sessions resumable and steerable mid-task via web/API/CLI/Slack.

**Validation & self-repair loop**: review agents are **incremental by default** (review the range since their own last posted review); budgets stop runaway sessions. No built-in test/CI fixer documented — repair is via your own agent definitions (one YAML per specialty, e.g. strict migration reviewer vs lenient docs reviewer).

**Human gates**: "engineers move to the control points" — define job, set boundary, review consequence; read-only reviewer tokens as a config choice.

**Cost model**: **usage-based (tokens + compute), explicitly no seats**; $10 new-user / $100 new-org credit; **budget block in YAML: per-session cap + trailing 1/7/28-day caps; a session that would breach a limit is cancelled *before* a sandbox exists; a running session stops at the cap with a `budget_hit` record**; alerts fire before limits; BYO Anthropic key/Bedrock so spend lands on your commitment. SOC 2 Type I, Type II in progress.

**Migration/DB capability — the white-space check**: they ship a **"Schema Migration Reviewer" agent template**: PR-opened trigger, harness claude_code + claude-opus-4-8, instructions to "flag risky locking behavior, missing backfills, missing indexes, and rollout order problems," multi-repo environment, **budget $0.80/session, $4/day, $20/week**. It is a prompt-only LLM reviewer — **no SQL lint engine, no shadow DB, no migration execution**.

**What DevAgent can learn or steal**: (1) **budgets as YAML enforced *before* compute starts** with typed exit schemas — the strongest cost-governance design in the cluster; DevAgent's ledger cost ticks should become enforceable per-attempt/per-day caps with a `budget_hit` ledger event; (2) agents-as-code with git-deployed config + sync-error safety; (3) per-session scoped tokens minted at spawn (matches our SecretRegistry/sandbox fence ideas); (4) incremental review ranges.

**Sources**: ellipsis.dev/blog/the-ellipsis-agent-cloud; ellipsis.dev/blog/the-future-of-software-is-the-agent; ellipsis.dev/agents (+/agents/templates/schema-migration-reviewer); ellipsis.dev/docs/triggers; ellipsis.dev/llms.txt.

## devlo

**What it is / status (2026-09)**: "AI Software Platform" — chat editor + **Code Review, "Tickets to PRs", Custom Agents, Team Metrics**; Slack app listed. Site is fully client-rendered and docs subdomain does not resolve — mechanism, triggers, sandbox, and pricing could not be verified from primary sources (single source: site nav/meta). Classified as **issue→PR claimed, mechanism unverified**.

**Sources**: devlo.ai (nav + meta description only); docs.devlo.ai (NXDOMAIN).

## CodeAnt AI

**What it is / status (2026-09)**: YC W24 AI code review; 2026 expansion into **AI Pentesting** (exploit simulation, OWASP Top 10, attack-path mapping; low/medium findings free) and cloud-security (CSPM) — a security-platform pivot with review still core. Claims #1 of 20 tools on Martian's Code Review Bench (87.6 F1 security-patch detection); SOC 2 Type II; Gartner Cool Vendor 2026.

**Trigger surfaces**: PR opened/updated on GitHub/GitLab/Bitbucket/Azure DevOps; CI/CD review hook; CLI; IDE extension (VS Code/Cursor/JetBrains/VS/Windsurf).

**Workflow pipeline**: full-codebase-context review per PR → severity-ranked inline findings, each with **one-click "Fix in IDE"** (opens your editor with the fix prompt loaded), steps-of-reproduction for bugs, sequence diagrams, PR summary, AI Learnings (feedback → future reviews), quality gates + review dashboard.

**Isolation & sandbox**: SaaS; AWS Marketplace deployment; docs describe self-hostable posture for enterprises (docs.codeant.ai). No documented sandbox test-execution loop — validation is analysis + your CI.

**Human gates**: human reviewer decides; review dashboard for triage.

**Cost model**: per-seat subscription + 14-day trial; pricing figures JS-gated on the site (single source for existence, not numbers).

**What DevAgent can learn or steal**: "Fix in IDE" — one-click finding→editor-with-prompt is a nice evidence→repair handoff; pentest findings as ticket sources is a trigger surface we don't cover (matches our STRIDE gate's inputs).

**Sources**: codeant.ai/ai-code-review; codeant.ai/pricing; docs.codeant.ai.

## Cline (incl. Kanban) — open-source harness, no ticket→PR cloud

**What it is / status (2026-09)**: Apache-2.0 agent core ("8M+ developers") with five surfaces: VS Code/JetBrains/Cursor/Zed/Neovim-ACP extensions, **CLI** (interactive + headless `cline --json` for CI/CD), Desktop app (macOS/Win; **scheduled agents** on cron), **SDK** (`@cline/sdk` — custom tools, multi-agent teams), and **Kanban** (`npx kanban`, research preview). Enterprise: SSO/RBAC, OpenTelemetry export. Model access: usage-billed Cline provider, ClinePass flat $9.99/mo, or BYOK.

**Trigger surfaces**: chat, CLI arg/headless pipe, cron schedules, messaging connectors (Slack/Telegram/Discord/Linear — each thread = one session), CI pipelines.

**Workflow pipeline**: Plan mode (explore, clarify, strategy) → Act mode (execute; every edit/command needs approval or auto-approve toggled) → checkpoints for undo. Kanban: board of cards; agent can decompose a big task into linked cards; **play starts the card in an ephemeral per-task git worktree (gitignored dirs like node_modules symlinked)**; live hooks render each agent's latest message/tool-call on its card; review = TUI + diff + line comments sent back to the agent; **Ship = Commit or Open PR via a dynamic agent prompt that resolves merge conflicts**; or auto-commit/auto-PR for full autonomy; card → trash cleans up the worktree (resume IDs kept); **completing a card auto-starts linked cards → autonomous dependency chains**. `cline --team-name …` coordinator/subtask multi-agent with persistent team state.

**Isolation & sandbox**: local worktrees per task (no cloud); Desktop runs scheduled sessions; no remote sandbox product.

**Validation & self-repair loop**: agent watches linter/compiler output and running processes ("reacts to test failures"); no independent gate layer — review is human diff inspection.

**Cost model**: OSS free + your tokens (BYOK) or Cline usage billing / $9.99 ClinePass; enterprise seats.

**What DevAgent can learn or steal**: (1) **symlink gitignored deps into each task worktree** — kills our per-run reinstall cost; (2) card-completion → auto-start of linked cards is our wave sequencing, but user-facing and debuggable; (3) their "research preview bypasses permissions" warning is the honest version of the risk we gate; (4) resume-ID worktree cleanup.

**Sources**: github.com/cline/cline README; docs.cline.bot (cline-overview); github.com/cline/kanban README.

## Roomote (Roo Code's cloud agent)

**What it is / status (2026-09)**: **source-available (FCL-1.0 → Apache-2.0) cloud coding agent from the Roo Code org** ("Your own cloud coding agent… without paying for a black box"), one-click deployable on Railway/Render/self-host script/Coolify/Fly; hosted Cloud option. Pushed daily (2026-09-11).

**Trigger surfaces**: Slack/Teams/Telegram/Discord message, Web UI, connected Linear/Jira/GitHub Issues triage ("reads new tickets, asks clarifying questions, starts working"), Sentry/log links pasted as tasks; MCP server so Claude Code/Codex/Cursor can start/inspect/steer Roomote tasks.

**Workflow pipeline**: task message → **ephemeral sandbox (Modal, E2B, Daytona, Blaxel, or local Docker)** → clone repo → implement → **run the test suite** → screenshot → push branch → **open PR with diff + screenshots + live preview URL**. Handles bug repro (Sentry/stack-trace inputs), chores, "migration files", features; parallel agents across repos.

**Isolation & sandbox**: disposable per-task sandboxes from a provider menu (MicroVM-backed on E2B/Modal); self-host keeps everything on your infra; Postgres+Redis app stack provisioned by templates.

**Validation & self-repair loop**: tests + screenshot; no domain gates, no CI-fixer loop documented; clarifying-question flow before work.

**Human gates**: PR review like a teammate; multiplayer board; full audit trail (model, tools, code written).

**Cost model**: self-host free ≤10 users (license beyond), Cloud from **$49/mo**; tokens via **BYOK incl. ChatGPT subscription reuse** or provider keys.

**What DevAgent can learn or steal**: (1) provider-pluggable sandbox layer (Modal/E2B/…/Docker) behind one interface — the shape for our compose-sandbox beyond local Docker; (2) **live preview URL + screenshot evidence on the PR** — cheap, high-trust evidence UX; (3) issue-tracker triage with clarifying questions before execution (our ask/human-answer flow, productized).

**Sources**: github.com/RooCodeInc/Roomote README (+org listing); roomote.dev deploy buttons.

## ByteDance Trae (SOLO → TRAE Work)

**What it is / status (2026-09)**: consumer-ish pivot chain: TRAE IDE → **SOLO standalone app (free beta 2026-03-31, invite-coded; Ultra plan includes it)** → SOLO Mobile (capture tasks on phone, run on desktop/cloud) → **TRAE Work (July 2026): AI-native workspace, web/desktop/mobile, Work + Code (+Design) modes, cloud background execution**. Secondaries describe SOLO as a full-stack "context engineer" doing requirement analysis, codegen, terminal execution, and **browser testing**.

**Trigger surfaces**: natural language / voice / file upload in a conversation; single workspace holds files, context, task history; cloud tasks run in background and parallel.

**Validation & self-repair**: browser-testing-based verification claims (secondary source); no documented PR/CI gates, no migration tooling. Privacy mode toggles training use.

**Cost model**: seats+allowances: Lite $3 / Pro $10 / Pro+ $30 / Ultra $100 per month with $5/$20/$90/$400 usage allowances; SOLO beta free with invite.

**Threat assessment**: none for backend ticket→PR governance today; it optimizes individual flow, not repo-gated delivery. Watch for Work-mode dispatch of coding tasks from non-engineers (same audience CodeRabbit courts).

**Sources**: trae.ai/blog/new_solo_beta_0331 (2026-03-31); trae.ai/pricing; trae.ai/blog/trae_solo_mobile_0506; verdent.ai/guides/agent/what-is-trae-work (2026-07-03); klover.ai SOLO analysis (secondary).

## Tencent CodeBuddy

**What it is / status (2026-09)**: Tencent's AI IDE (intl) + CodeBuddy Code CLI; **CodeBuddy Code 2.0 (Jan 2026)** added Skills, Plan Mode, ACP compatibility, open SDK. Also runs **TokenHub**, a Tencent Cloud model gateway whose docs pitch connecting Claude Code, OpenCode, Cline, Cursor, Roo Code, Codex, OpenClaw etc. to hosted models.

**Trigger surfaces**: IDE chat; Craft (agent) mode for local modifications; **Plan mode: creates a task list, then autonomously executes read/modify/environment-checks after confirmation**; custom agents; `.codebuddy/rules` + `.codebuddy/skills` + Memories; MCP tools (incl. database MCP servers).

**Isolation/validation**: IDE-local; native integrations with **CloudBase / Supabase / CloudStudio for "database management" and one-click preview deploy** — DB access as a *tool integration*, not a migration-safety gate. No test-gate or PR loop documented; smart code review exists as a plugin feature (JetBrains listing).

**Cost model**: Tencent Cloud billing; free trial packages; token/model pricing pages (per-token).

**Sources**: tencentcloud.com/document/product/1256/77277 (IDE docs, updated 2026-03-02); plugins.jetbrains.com/plugin/24379; baike.baidu.com CodeBuddy entry (2.0 changelog); intl.cloud.tencent.com TokenHub docs.

## JetBrains Junie

**What it is / status (2026-09)**: JetBrains' agent inside the AI Assistant plugin (2026.2 docs): "autonomously plan and execute complex, multi-step actions… run tests or terminal commands," reporting progress. One of four pluggable agents (Claude Agent, Codex, Copilot also supported; ACP for external agents).

**Trigger surfaces**: chat prompt in the IDE; automatic IDE context (open file + selection); `AGENTS.md` instructions; `.aiignore` respected.

**Workflow pipeline**: plan → execute with approvals → review/rollback. **Brave mode** skips confirmations (or Auto). **Debug mode** (IDEA Ultimate): agent drives the *IDE debugger* via MCP toolset — breakpoints, stack/thread inspection, expression evaluation on the paused frame — instead of editing code.

**Isolation/validation**: none beyond the IDE and your toolchain; no cloud, no issue trigger, no PR lifecycle (that's Copilot/Devin territory, not Junie's).

**Cost model**: JetBrains AI subscription tiers.

**What DevAgent can learn or steal**: debugger-as-agent-toolset is a genuinely novel validation surface (runtime state inspection, not just test exit codes) — cheap to emulate in Go via Delve MCP for our async/race review evidence.

**Sources**: jetbrains.com/help/ai-assistant/agents.html + junie-agent.html (2026.2).

## Pilot (qf-studio) — delta since `docs/research/pilot-probe.md` (2026-09-04)

Repo active (pushed 2026-09-10; 665→681 stars; homepage pilot.quantflow.studio; BSL 1.1 unchanged). The notable change is visible in the commit stream: the repo is now **self-developed at high cadence** — daily `docs(nav): PR#5433 + ui PR#155 reviewed APPROVE; box v2.275.0` commits show a Navigator-v7 agent loop running numbered review waves across three repos (main + ui + console), with autopilot machinery hardened the same week (`reconcileOrphanPRs` closed-origin skip, **borrowed-branch registry with persist-failure eviction + TTL**, active-only dashboard queue filter, docs CTR analytics feeding iteration). Still **generic test/lint/build gates** — no migration/FK/race gates in any surface observed. Anatomy unchanged otherwise: see pilot-probe.md. Steal-list addition: borrowed-branch registry TTL is exactly the zombie-PR sweep hardening our PR hygiene wave needs.

## One-liners (rest of the tail)

- **Kodus / Kody** (kodustech/kodus-ai, OSS, 1.4k stars, active): BYOK code review with self-hosted runners; **CLI + AI-agent "autonomous review-fix loops"**; **Kody Issues** tracks unimplemented suggestions from closed PRs until a future PR fixes them; Teams $10/dev/mo (free with your own key). A review-loop-shaped competitor to CodeRabbit at 1/24th the price.
- **Kodu** (kodu.ai): DNS dead as of 2026-09-11 (kodu.ai/www/app all NXDOMAIN); VS Code agent plugin ecosystem references remain. **Presumed dead/offline [unverified cause]**.
- **cto.new** (ClickUp, sibling of Codegen): free AI code agent wired to GitHub/Jira/Slack (directory listing; single source).
- **Cosine Genie**: autonomous SWE that picks up tickets from GitHub/Jira/Linear and opens PRs (directory listing; single source).
- **Kilo** (kilo.ai): promotes the hybrid pattern "plan in IDE, execute in local sandbox, CI + PR review required" — the pattern consensus of 2026.
- **Revolte**: interactive agent sessions product (directory listing).

## Comparison table

| Product | Trigger | Runs where | Verifies what | Repair loop | Human gate | Pricing unit | Issue→PR? |
|---|---|---|---|---|---|---|---|
| Sweep AI | `sweep` issue label | their cloud (dead) | test run | comment re-runs | PR review | seats (dead) | Was yes; **dead 2026** |
| Codegen (ClickUp) | Slack thread, Linear/Jira/ClickUp ticket, `@codegen-agent`, CLI/API | their VMs, Docker image + snapshots, FS persists per context | CI checks (monitor) + AI PR first-pass | **checks auto-fixer: 3 retries → tap out to human** | tap-out flag; human review | credit/run (post-acq: ClickUp seats) | **Yes** |
| Qodo | PR open/push, `/agentic_review` | their cloud / on-prem | multi-agent review + judge; requirement gaps; cross-repo conflicts; **no test execution** | **Remediation agent → separate fix PR**; severity threshold | human merge; dismissals learned | **$0.012/credit pooled** (~140/review) | No (review-only) |
| CodeRabbit | PR open/push, Slack agent, CLI/IDE | their cloud; per-thread sandbox **w/ own git worktree + snapshot chain** | 50+ linters **incl. Squawk migration lint (default-on)**; linked-issue assessment; **NL pre-merge merge-gates**; security scans | Autofix (commit or stacked PR); agent opens PRs from Slack | pre-merge checks + human | **$24–72/seat/mo** + agent-minutes | Slack agent: yes |
| Greptile | PR open/push; `/greploop` | their cloud / self-host AWS air-gapped | graph-indexed review swarm; **TREX runs tests+services+browser in sandbox, evidence on PR**; confidence 0–5 | one-click fix handoff; **/greploop iterate-until-clean** | human merge; score advisory | **credits**: $30/seat incl. 50; TREX=3 credits | No (validation layer) |
| Ellipsis | YAML triggers: cron, PR/push/issue react, Linear, Sentry, Slack, mentions | their cloud, isolated machine/session, per-session scoped token | incremental review; typed JSON output; **migration reviewer = LLM prompt only** | your own agent YAMLs; budget stop | config-as-code review; read-only tokens | **usage (tokens+compute), no seats**; budgets enforced pre-sandbox | Linear/issue triggers: **yes** |
| devlo | claimed: tickets (Linear/GitHub) | unverified | unverified | unverified | unverified | unverified | Claimed, unverified |
| CodeAnt AI | PR open/update, CI hook, CLI | SaaS / AWS Marketplace | review + security scans; pentest scans; no test-run loop documented | one-click Fix-in-IDE | human review | seats (numbers gated) | No (review-first) |
| Cline (+Kanban) | chat, headless CLI, cron, Slack/Linear threads | **local** (worktrees) | agent watches linter/compiler/tests | agent self-fix during run | per-action approvals (or auto) | OSS free; $9.99 flat or BYOK | Partial (Linear thread → work) |
| Roomote | Slack/Teams/Telegram/Discord msg, Linear/Jira/GH issues, MCP | **ephemeral sandbox: Modal/E2B/Daytona/Blaxel/Docker** | **runs test suite** + screenshot; live preview URL | clarifying-question loop; re-task | PR review; audit trail | self-host free ≤10; **Cloud $49/mo**; BYOK | **Yes** |
| Trae SOLO/Work | chat/voice/file; workspace | cloud + desktop | browser-testing claims (secondary) | conversational | review output | $3–$100/mo + usage allowance | Loose (workspace, not repo-governed) |
| CodeBuddy | IDE chat; Plan mode task list | IDE/local | IDE diagnostics; CloudBase/Supabase DB tools | conversational | confirmation before plan execution | Tencent Cloud tokens | No |
| Junie | IDE chat | IDE-local | runs tests/terminal; **debugger MCP inspection** | conversational | Brave/Auto approval modes | JetBrains AI subscription | No |
| Pilot | labeled GitHub issue, Linear/Jira/Asana, Telegram | local+cloud, worktrees | test/lint/build + CI watch (auto-merge levels) | auto-retry gates | autopilot levels | OSS/BSL, cloud fee | **Yes** |

## Cluster synthesis

- **The review layer is becoming the control plane.** Every review-only vendor in this cluster shipped orchestration or gates in the last 12 months: CodeRabbit → pre-merge merge-gates + Slack→PR agent; Greptile → TREX sandbox test-runs + `/greploop` until-clean contract; Qodo → remediation agent + rule mining; Ellipsis → full agents-as-code cloud. A review-only wedge no longer exists as a stable business; DevAgent should expect these to move *backwards* into execution next.
- **Mortality + consolidation in DevAgent's exact niche**: Sweep (first-mover issue→PR) is dead; Codegen absorbed into ClickUp; Qodo publicly abandoned generation, arguing "the builder must not verify its own output." That argument is free validation of DevAgent's independent-gate architecture — and a warning that pure-generation SaaS is already commoditized; the surviving value is validation + governance.
- **Migration/DB white space — first crack, not collapse.** CodeRabbit runs **Squawk on migration files by default** (static Postgres lint: locks, unsafe changes) — G3-style static analysis is now table stakes. Ellipsis ships a **prompt-only** migration-reviewer agent template (locking/backfills/indexes/rollout order) with a dollar-budget. **Nobody executes migrations against a shadow DB, verifies up/down reversibility, replays for FK integrity, or gates async/race behavior.** DevAgent's G2 (compose-boot + apply + reversibility) and FK/async gates remain unclaimed — but the PRD/§19.2 claim "nobody covers migrations" must be narrowed to *execution-based* migration validation; static migration lint can no longer be cited as absent.
- **Budget governance became product surface.** Ellipsis (session+trailing caps enforced *before* sandbox creation, `budget_hit` events, typed exit schemas) and CodeRabbit (Scopes with spend limits, agent-minute metering) treat spend as a safety mechanism, not a billing afterthought. DevAgent's ledger cost ticks should graduate into enforced per-attempt/per-day budgets with ledger events.
- **Worktree-per-task + snapshot is converged isolation tech**: CodeRabbit threads, Cline Kanban cards, Ellipsis sessions, Codegen contexts, Roomote tasks all do worktree+snapshot(+token-scoping). Differentiation is not isolation; it is what *runs inside* (CodeRabbit linters, Greptile TREX, DevAgent's G-gates).
- **Evidence UX is converging on "proof on the PR"**: Greptile TREX (logs/screens/traces/video), Roomote (screenshots + live preview URL), CodeRabbit (linked-issue assessment, pre-merge check results). "Evidence-attached-per-PR" can no longer be claimed as unique — it must be *deeper* (deterministic gate transcripts, monitor-evasion tracking, migration replays).
