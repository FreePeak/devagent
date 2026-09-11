# Research: Competitor handoff workflows — how they run unattended without babysitting

Date: 2026-09-11
Scope: nine parallel internet scouts (official vendor docs only, ~150 pages fetched)
answering one question per competitor: **how is the ticket→PR handoff built so a human
can walk away and come back — triggers, pre-execution gates, progress visibility,
stuck/failure behavior, completion contract, review/merge loop, and unattended
guardrails.** Complements PRD §4/§19.2 (landscape + pricing) and the 2026-09-07
competitive scan (`2026-09-07-context-ecosystem-scout.md`), which covered pricing and
trigger surfaces; this doc covers the *workflow mechanics* neither of them did.

Vendors covered: Devin (Cognition), GitHub Copilot cloud agent + Agent HQ, OpenAI
Codex (cloud + CLI/IDE interop), Google Jules, Factory Droid, OpenHands, Claude Code
(Anthropic agent platform), Cursor Cloud Agents, and the long tail (Sourcegraph Amp,
Qodo, Blitzy, Windsurf→Devin; Terragon dead, Sweep pivoted).

## 1. Cross-vendor pattern catalog (the anti-babysitting playbook)

Every credible unattended story is assembled from the same seven parts. Nothing here
is proprietary magic — it is plumbing discipline.

1. **Trigger = context import.** The ticket/Slack thread/PR comment *body* becomes the
   session prompt, verbatim, plus accumulated thread history and attachments (Devin
   event payloads auto-appended; Factory Slack threads with images; Codex 👀 reaction
   then chat link; Cursor `[repo=…]` keyword routing; Amp 512 MB attachments). No
   competitor asks the human to restate the task.
2. **Plan gates are soft, timed, or substituted — never a hang.** The industry answer
   to "approve the plan" is not "wait for the human": Jules **auto-approves the plan on
   a timer** once you navigate away and its Jan-2026 **Planning Critic** agent reviews
   every no-human-intervention plan (−9.5% task failures); Factory's Spec Mode asks
   once and the approval dialog itself selects the autonomy level (Low/Med/High)
   for the run; Copilot's issue-assign path has no gate at all but its app exposes
   Interactive/Plan/Autopilot session modes. Only Blitzy keeps a hard human gate
   (Tech Spec → Agent Action Plan approval) — and correspondingly has no walk-away UX.
   OpenHands instead gates per-*action* via a confirmation policy
   (`AlwaysConfirm`/`NeverConfirm`/`ConfirmRisky`) backed by an LLM security analyzer
   that rates each action LOW/MED/HIGH before execution.
3. **Progress is written into the human's tools, not a viewer.** Draft PR opened
   immediately and evolved as a scratchpad (Copilot decomposes the issue into a PR-body
   checklist and ticks items per commit; every commit links to a streamed session log);
   Devin syncs sessions bidirectionally with a Slack thread and a per-run "code
   channel" whose status chip reads working/blocked/done; Claude Code posts and updates
   one comment per task; Amp's thread *is* the artifact (diff + shared tmux + live
   service portals). The human's notification queue is the dashboard.
4. **"Needs input" and "failed" are first-class, notified terminal states.** Explicit
   taxonomies everywhere: OpenHands ships a **Stuck Detector** (on by default) with
   five named signatures — repeated action-observation cycles (4+), action-error
   cycles (3+), agent monologue (3+), context-window-error loops, ping-pong
   alternation (6+) — that halts runs into a `stuck` terminal state; Jules marks
   transient-failure retries bounded then **FAILED + notification**; Devin schedules
   carry an `Error` state after consecutive failures and its API exposes
   exit/error/suspended for polling; Codex's auto-review **circuit-breaks the turn
   after 3 consecutive (or 10-in-50) reviewer denials** rather than looping; Amp
   pauses a failed automation and shows the error for fix-and-resume; Claude Code
   fires typed `Notification` hooks (`agent_needs_input`, `idle_prompt` at 60 s,
   `StopFailure` on API errors) designed to forward to Slack/pager. Nobody silently
   hangs: every vendor has chosen an explicit "stopped and told someone" default.
5. **Completion contract = self-verified before human invite.** The table-stakes
   sequence is: run repo tests in the isolated env → iterate to green → *then*
   self-review → *then* open/flip the PR and request human review. Reactive CI
   iteration is universal: Jules **CI Fixer** (PR watched; Actions failure → fix →
   resubmit loop), Copilot pre-push checks + "Fix failing checks"/failing-run
   investigate buttons, Claude Code per-PR **auto-fix toggle** (CI failures and
   review comments each wake the agent), Devin self-driven lint/test loops plus
   Auto-Fix of Devin Review comments, and Cursor's autofix loop with the two
   smartest guard rails in the field: **skip the fix if the check also fails on
   base, and hard-cap at 10 CI-failure follow-ups** per PR. Pre-PR self-review is
   now also table stakes: Copilot's mandatory-by-default pass (CodeQL + secret
   scanning + GitHub Advisory dependency check + Copilot code review, resolving
   findings before finalizing), Factory's Automated Code Review (submits an approval
   when clean) plus STRIDE-based Security Review with a candidate→validation
   two-pipe, Devin Review, Claude Code's managed Code Review emitting a
   machine-readable `bughunter-severity` check-run gate CI can fail on.
6. **Evidence artifacts close the trust gap.** Beyond green checks: Devin attaches
   **annotated screen recordings** of E2E runs to the PR thread ("it works → merge");
   Factory's QA report is a PR comment with screenshots/terminal snapshots/GIF diffs
   and can be a *required* GitHub check; Cursor embeds screenshots/videos/logs into
   the PR description; Blitzy delivers a "Project Guide" doc. The pattern: the PR
   carries proof a reviewer consumes passively — no re-running anything locally.
7. **Humans keep merge; agents keep the branch alive until then.** Universal: no
   auto-merge of agent-authored code by default. Copilot *cannot* mark ready/approve/
   merge and needs an extra approval under its app identity; Devin never merges
   itself (auto-merge is GitHub's native toggle it can enable on request);
   "Nothing merges until a person reviews" (Cursor) — though Cursor's **PR
   Routing & Approval agent** cracks the door: risk-scored **auto-approval of
   low-risk PRs**, refusing when findings need human review. The Copilot *app*
   "agent merge" is the other crack: background "fix what's blocking, merge when
   GitHub allows" (survives app restart, self-disables after merge).
   Ticket-close-on-merge is documented nowhere — every scout independently found
   the same post-merge status-sync gap.
8. **Guardrails make leaving safe (and cap the blast radius).** Fresh isolated
   compute per task (Firecracker microVMs in a separate AWS account — Cursor; VM
   from blueprint snapshot — Devin; ephemeral Actions runners — Copilot; Docker/
   Sysbox sandboxes — OpenHands). Egress by default least: Codex cloud **agent-phase
   internet off by default** with GET/HEAD/OPTIONS-only allowlist presets and a
   security proxy; Copilot firewall whose blocked attempts surface as PR warnings;
   Factory kernel-enforced sandbox (Seatbelt/bubblewrap+seccomp) with **un-bypassable
   blocklists that resolve wrappers/substitution** and org settings that outrank the
   user's; Claude Code's OS sandbox with credential **masking** (secret files visible
   but content-redacted) and `failIfUnavailable` as a hard policy gate. Budget as stop,
   not meter-read: Devin per-session/automation **`max_acu_limit`** + org/user monthly
   caps (Automations just stop; Review degrades to dumb diff viewer); OpenHands
   per-conversation USD budget that **stops the conversation** plus org budget alerts
   at 80/90/100%; Copilot user budgets as hard stop across all features; Claude SDK
   `error_max_budget_usd` as a resumable result subtype; Cursor spend-limit hit halts
   on-demand usage. Loop/timeout ceilings: Copilot **59-minute session cap**; OpenHands
   `max_iterations` (default 10) on goal loops; Claude Stop-hook continuation capped
   at 8; Devin auto-sleep after 30 min idle (wakes on any message); Amp auto-pauses
   idle orbs at 5 min; Jules daily/concurrent task ceilings *are* the autonomy cap.
   Prompt-injection hardening is now explicit: Claude rejects bot actors by default
   (agent-looping-agent prevention) and requires write-access actors; Devin's
   bot-comment allowlist defaults to ignoring all bots (lint bots always processed);
   Copilot filters hidden/HTML-comment injection, doesn't run Actions workflows on
   agent pushes until a human approves the run, and attributes automation PRs to the
   creator who then cannot self-approve; Codex strips secrets **before** the agent
   phase (setup scripts get them, the agent doesn't); Cursor Runtime Secrets render
   `[REDACTED]` in transcripts and commits.
9. **Session persistence = resume, rerun, or handoff.** Devin VM state is resumable
   (`resumable: true`, sleep/wake, comments route back into the existing session);
   Claude resumes by ID after SIGTERM with crash state restored, forks sessions, and
   mirrors transcripts to S3/Redis via SDK `SessionStore`; OpenHands' append-only
   immutable event log *is* state (pause/resume, server restarts); Cursor snapshots
   environments (90-day rolling) and coalesces event-burst follow-ups into
   resumable agents; Devin CLI `/handoff` packages the *local* conversation, branch,
   and ≤100 KB diff into a cloud session — an explicit local→cloud baton pass;
   Codex does both directions: IDE→cloud delegation carries plan + uncommitted
   changes, and `codex apply` pulls the cloud diff back onto the local checkout.
   Jules has no resume — it has **rerun** with failure logs guiding the fix; Copilot
   cloud "resume" is re-assign/re-comment (fresh session, same branch).

## 2. Vendor notes (condensed; all URLs fetched 2026-09-11)

### Devin — the ticket-native reference
Linear/Jira assignment, playbook labels (`!plan`, `!implement`), Slack bang-keywords,
and **Automations** (every trigger × {start session, message existing session,
triage monitor, email notify}, Terraform-manageable, webhook ingress with secret +
payload filter) ([automations](https://docs.devin.ai/product-guides/automations.md)).
No blocking plan gate; scoping-only Jira mode and Ask→Agent as soft gates. Session
→ Slack code channel with working/**blocked**/done chip; time-travel command history
and `/btw` side chats. Out-of-credits: Automations stop, Review degrades; ACU caps
per session/automation/org; sessions sleep 30 min idle, wake on message; 30-day
resume window. Completion = tests/lint/CI green + Devin Review Auto-Fix + optional
annotated E2E video attachment; auto-responds to PR comments until archived.
`structured_output_schema` forces an API contract before the turn may end;
`resumable` VM state; security profiles bind network/MCP/git read-only policies with
mandatory intersect-only org enforcement; Knowledge + Playbooks + DeepWiki give
scheduled runs consistent context. Managed Devins propose child sessions **for
approval before launching**. ([index](https://docs.devin.ai/llms.txt),
[handoff](https://docs.devin.ai/work-with-devin/devin-handoff.md),
[security profiles](https://docs.devin.ai/product-guides/security-profiles.md),
[Review](https://docs.devin.ai/work-with-devin/devin-review.md),
[insights](https://docs.devin.ai/product-guides/session-insights.md),
[retry-loop forensics](https://cognition.com/blog/closing-the-agent-loop-devin-autofixes-review-comments))

### GitHub Copilot coding agent — draft-PR-as-journal + mandatory self-review
Assignment starts work instantly (👀 → draft PR); **PR body checklist ticks with
pushed commits**; live session logs (reasoning + tool calls) linked from every
commit; review-request is the come-back notification ([blog](https://github.blog/ai-and-ml/github-copilot/assigning-and-completing-issues-with-coding-agent-in-github-copilot/)).
Pre-PR validation in the ephemeral runner + built-in CodeQL/secret/dependency scans +
Copilot reviewing its own diff, resolving findings *before* requesting human review
([about-cloud-agent](https://docs.github.com/en/copilot/concepts/agents/cloud-agent/about-cloud-agent)).
59-min session cap; stuck = manual unassign/reassign; budgets hard-stop; no raw
`git push` (restricted push API), workflow runs need human approval once per push,
injection chars filtered; hooks (`preToolUse`, `errorOccurred`) for custom policy +
failure alerts ([hooks](https://docs.github.com/en/copilot/concepts/agents/hooks)).
Automations add scheduled/event triggers with a **rationale + confidence system** —
low-confidence changes are held for human approval ([automations](https://docs.github.com/en/copilot/concepts/agents/cloud-agent/about-automations)).
Agent HQ puts Claude/Codex behind the same triggers, org agent policy, audit logs,
and parallel multi-agent assignment for cross-review ([Agent HQ](https://github.blog/news-insights/company-news/pick-your-agent-use-claude-and-codex-on-agent-hq/)).

### OpenAI Codex cloud — environment-setup gate, egress-off default, reviewer-agent approvals
Triggers: cloud UI, `@codex` PR comments (`review`/`security review`/any instruction),
Linear assign/triage-auto-assign, Slack, `codex cloud exec` CLI, GitHub Action,
scheduled RRULE + Gmail/Slack/PR-event **automations**, Codex Remote (phone)
([cloud](https://learn.chatgpt.com/docs/cloud), [automations](https://learn.chatgpt.com/docs/automations)).
Gate is the *environment*: checkout → setup script (internet on, secrets visible) →
agent phase (internet default-off, secrets stripped). Local/scheduled runs route
sandbox-boundary approvals to an **auto-review reviewer agent** with the 3-strikes
denial circuit breaker ([auto-review](https://learn.chatgpt.com/docs/sandboxing/auto-review)).
Notifications push/email/SMS per category; pet/activity states Running/Needs
input/Ready/Blocked. Usage-limit mid-turn: the turn *continues* under fair use —
deliberate anti-abort semantics. Best-of-N attempts exposed via `attempt_total`
attempts preserved for human selection ([cli](https://learn.chatgpt.com/docs/developer-commands?surface=cli)).
No documented cloud hang-watchdog, auto-retry, or auto-merge (gaps).

### Google Jules — the soft-gate pioneer
Label `jules` → plan → (auto-)approve → branch → PR + issue comment; scheduled tasks,
proactive suggested-tasks (resolvable-`#TODO` scan, email discovery, human clicks
Start), REST API with `AutomationMode.AUTO_CREATE_PR` and `AWAITING_PLAN_APPROVAL` /
`AWAITING_USER_FEEDBACK` blocking states ([running-tasks](https://jules.google/docs/running-tasks.md),
[API](https://jules.google/docs/api/reference/overview.md)). **Auto-approve timer +
Planning Critic** = the flagship anti-babysitting move
([review-plan](https://jules.google/docs/review-plan.md),
[critic](https://jules.google/docs/changelog/2026-01-26-1.md)). Bounded transient
retries → FAILED + notify; rerun is the recovery; pause/await states never guess
([errors](https://jules.google/docs/errors.md)). **CI Fixer** fix→resubmit loop and
PR comments (👀 per comment, then fix commit) with Reactive Mode (act only on
`@Jules`) ([changelog](https://jules.google/docs/changelog)). Environment snapshots
pin the per-repo dev VM; daily task limits are the de-facto autonomy cap; no
command allowlists/checkpoints (gaps).

### Factory Droid — autonomy levels + incident-response as a first-party handoff
**Spec Mode**: read-only plan + clarifying questions → `ExitSpecMode` approval whose
dialog *chooses the autonomy level* for implementation; org `maxAutonomyLevel` hides
higher options; approved plans persist as dated Markdown under `.factory/docs/`
([spec](https://docs.factory.ai/autonomy-and-safety/specification-mode)). `droid exec`
CI mode fails fast on permission violations (sandbox violations auto-deny, never hang
headless). Missions: orchestrator + workers + per-milestone validation agents with
feature retry limits; troubleshooting is explicitly human-steered re-plan
([missions](https://docs.factory.ai/missions/overview)). **Incident Response**
(private preview): alert-bot Slack post → Droid RCA session on a service-account
Droid Computer → fix PR + runbook update → status/session links posted back into the
thread ([incident](https://docs.factory.ai/software-factory/incident-response)).
Pre-human gates: Automated Code Review (approves when clean), Security Review
(two-pipe candidate→validation, P0–P3), Automated QA (diff-selected flows, evidence
report, **can be a required check**, failure patterns fed back into the QA skill)
([QA](https://docs.factory.ai/software-factory/automated-qa)). Kernel sandbox +
allow/deny/**blocklists** (un-bypassable, wrapper-resolving), Droid Shield secret
detection, Triage agent routing tickets to humans *or* Droid sessions
([sandbox](https://docs.factory.ai/autonomy-and-safety/sandbox),
[triage](https://docs.factory.ai/software-factory/triage)). No autonomous
hang-retry or hard budget-stop (cost alerts are DIY via OTEL) — gaps.

### OpenHands — the OSS control plane: confirmation policies, stuck detector, event log
Label `openhands` / `@openhands` → live Cloud conversation link on the issue → PR +
task summary if resolved; Slack; event automations with JMESPath webhook filters;
CI via copied SDK workflow patterns ([github](https://docs.openhands.dev/openhands/usage/cloud/github-installation.md),
[event-automations](https://docs.openhands.dev/openhands/usage/automations/event-automations.md)).
Autonomy dial = `AlwaysConfirm`/`NeverConfirm`/`ConfirmRisky` ×
`LLMSecurityAnalyzer` LOW/MED/HIGH action ratings; rejection reasons feed back into
the agent's next attempt; headless is always-approve by contract
([security](https://docs.openhands.dev/sdk/guides/security.md),
[headless](https://docs.openhands.dev/openhands/usage/cli/headless.md)). API
execution states include **`stuck`** — produced by the built-in five-signature Stuck
Detector; goal-driver loops judge events up to `max_iterations` ([stuck](https://docs.openhands.dev/sdk/guides/agent-stuck-detector.md),
[goals](https://docs.openhands.dev/sdk/guides/agent-server/conversation-goals.md)).
**Stop hooks can deny completion until lint/tests/CI pass** — the SDK repo dogfoods
one ([hooks](https://docs.openhands.dev/openhands/usage/customization/hooks.md)).
Append-only event log = resumable state; per-conversation USD budget stops runs
mid-stream (continuable); org budget alerts 80/90/100%; 8-hour scoped GitHub tokens;
rrweb browser-session recording for replay ([budgets](https://docs.openhands.dev/openhands/usage/cloud/organizations/budgets.md),
[observability](https://docs.openhands.dev/sdk/guides/observability.md)).

### Claude Code — the platform others are built on (and DevAgent's own worker)
The most complete hooks story: typed `Notification` events (`permission_prompt` ~6 s,
`idle_prompt` 60 s, `agent_needs_input`, `agent_completed`, quota auto-resume),
`Stop`-hook completion gates (exit-2 block until tests pass; 8-consecutive-block
ceiling), `StopFailure` for API-error alerting, `PermissionRequest` hook deciding in
non-interactive sessions so prompts *deny instead of hang* — `--permission-prompts
none` hard-codes that for headless ([hooks](https://code.claude.com/docs/en/hooks.md),
[headless](https://code.claude.com/docs/en/headless.md)). GitHub: @claude with
actor-verification gates (write access, human-only bots-rejected), interactive
(comment-updating) vs automation (any-event) modes
([actions](https://code.claude.com/docs/en/github-actions.md)). Cloud sessions
(`--cloud`, mobile-monitored) with follow-up queueing, per-PR **auto-fix** toggle
over CI failures + review comments (asks the human when architectural)
([web](https://code.claude.com/docs/en/claude-code-on-the-web.md)). **Routines** =
schedule/API/GitHub-event unattended runs whose fired payloads arrive wrapped as
untrusted, with branch-protection-style push rules and daily caps
([routines](https://code.claude.com/docs/en/routines.md)). **Channels** push
external events (CI/chat/webhooks via MCP) into a *live* session
([channels](https://code.claude.com/docs/en/channels.md)). `/goal` = verifiable
completion condition re-judged by a fresh model each turn
([goal](https://code.claude.com/docs/en/goal.md)); agent-view supervisor groups
background sessions Needs-input/Working/Completed/Failed with cheap-model one-line
summaries ([agent-view](https://code.claude.com/docs/en/agent-view.md)); checkpoints
`/rewind` (bash/subagent edits *not* tracked — caveat), `--resume` after SIGTERM
with crash-state restore, SDK budget-stop result subtypes
([sessions](https://code.claude.com/docs/en/sessions.md)).

### Cursor Cloud Agents — subscriptions, evidence, and human takeover
Seven triggers + Automations (SCM/Slack/Linear/**Sentry/PagerDuty incident events**,
webhooks) + REST API with idempotent `agentId` and `mode: plan|agent`
([cloud-agent](https://cursor.com/docs/cloud-agent.md), [automations](https://cursor.com/docs/cloud-agent/automations.md)).
The standout mechanic: **Subscriptions** — the agent ends its turn *subscribed* to
PR/CI/Slack/Linear/timer events and wakes itself when they land (bursts coalesced,
180-day max), so "waiting on review/CI" costs nothing and self-resumes; plus the
autofix loop's two smart guards (base-failure skip, 10-follow-up cap) and
**Builds** as the pre-gate (agents start from last *successful* env build; failed
build never replaces it) ([capabilities](https://cursor.com/docs/cloud-agent/capabilities.md),
[builds](https://cursor.com/docs/cloud-agent/builds.md)). Run states
FINISHED/ERROR/CANCELLED/**EXPIRED** (lifecycle timeout). Remote-desktop **takeover**:
human grabs the agent's mouse/keyboard mid-run and hands control back — plus mobile
Live Activities for up to 8 agents ([mobile](https://cursor.com/docs/cloud-agent/mobile.md)).
Signed HSM commits; Firecracker isolation; Runtime Secrets redacted everywhere;
per-scope egress allowlists; spend limits halt execution at cap
([security](https://cursor.com/docs/cloud-agent/security.md)). PR Routing &
Approval agent auto-approves low-risk PRs by policy (see §1.7)
([approval-agents](https://cursor.com/docs/approval-agents.md)).

### Long tail
- **Sourcegraph Amp** — async-first orbs (fresh VM per thread, 5-min idle auto-pause,
  wake/sleep keeps thread state); gate at *shipping*: Ship Behavior = commit → rebase
  → **full test suite** → push, conflicts escalate to human, custom prompts encode
  "do not merge" ([shipping](https://ampcode.com/docs/markdown/orbs/shipping));
  Puck Slack persona; natural-language scheduled automations that **pause on failure**
  for fix-and-resume; `amp sync` mirrors the working diff locally while the agent
  still runs. ([orbs](https://ampcode.com/docs/markdown/orbs))
- **Qodo** — the review-side completer, not ticket→PR: PR-open auto-review,
  severity-gated **Remediation Agent** opens a fix PR (or pushes to source branch)
  and auto-closes when the loop concludes; "Review Resolver" skill lets *other*
  coding agents pull findings pre-PR ([remediation](https://docs.qodo.ai/code-review/remediate-findings-in-prs)).
- **Blitzy** — maximum-gate design: Tech Spec → **Agent Action Plan** (per-file plan,
  inline-editable; "does not improvise beyond the plan") → generate → PR + Project
  Guide; review→fix loop is a structured Refine-prompt format, merge is a prescribed
  checklist; self-correction only if your CI fires on the agent's branch
  ([AAP](https://docs.blitzy.com/project-lifecycle/aap-review)). Docs carry an
  author-todo artifact — treat accuracy claims cautiously.
- **Windsurf** — absorbed: agent docs now serve docs.devin.ai as "Devin Desktop 2.0";
  Agent Command Center Kanban (in-flight/**blocked**/ready) is now Devin furniture
  ([ACC](https://docs.devin.ai/desktop/agent-command-center)).
- **Dead/pivoted**: Terragon shut down 2026-02-09; Sweep → JetBrains assistant.

## 3. Where DevAgent's pipeline (§10) meets the field

Already aligned: ticket-driven triggers + progress comments (FR-TICKET-03 = §1.3),
Clarify state posting questions to the ticket (§1.2 exists where Devin/Jira-scoping
make it optional), G1–G6 closed-loop validation *before* Publish (§1.5 — DevAgent's
domain gates exceed the whole field's generic self-review wave), fan-out with
verifier selection (only Codex attempts come close, and a human picks), PR-not-merge
contract (§1.7), sandboxed workers + budgets (§1.8 — per-iteration caps exist in the
loop driver).

Deltas the field considers solved that DevAgent hasn't shipped:

1. **Park-and-wake, not hang-or-exit** (§1.4/§1.9): every leader treats
   awaiting-human-input as a durable parked state resumed by the reply event
   (Devin comments route into the sleeping session; Cursor subscriptions; Claude
   auto-fix toggle). DevAgent's Clarify exits the run; a ticket comment restarts
   from Fetch. Cheap win: make Clarify→wake event-driven on ticket/PR comments.
2. **Stuck taxonomy + forensics**: OpenHands' five signatures and Devin's
   retry-loop detection are directly implementable as ledger-driven loop-driver
   checks (repeated identical action-observation, monologue, ping-pong), feeding
   FR-VAL-style calibration; today DevAgent distinguishes failed-vs-stuck via
   wall clocks (#273 dispatch wall) but not loop signatures.
3. **Completion notified through the review system itself** — making the human a
   PR reviewer *is* the notification (Copilot does no more). Combined with the
   Pilot-probe alerting import (§19.6), the review-request trick costs one GitHub
   API call.
4. **Advisory plan gate for interactive mode** (Jules timer/Critic substitution vs
   DevAgent's hard `plan approved (interactive)` branch): auto-proceed after N
   minutes with a Critic pass logged as evidence, preserving the audit trail.
5. **CI-fix follow-up guards** (Cursor's base-failure skip + 10-cap) as the shape
   for any future DevAgent post-PR CI loop; DevAgent's Validate→Implement retry
   already loops with a max, and the field confirms bounded + *notified* is the
   right bar.
6. **Evidence artifact attached at Publish**: migration dry-run logs + gate-report
   as the backend-shaped equivalent of Devin's video/Factory's QA GIF (§1.6).
7. **Secrets-out-of-agent-phase** (Codex strip-after-setup, Cursor Runtime-Secret
   redaction): the preflight/setup worker pattern DevAgent's gates can copy — gate
   config gets credentials, the diff-producing worker never sees them.
8. **`structured_output_schema`-style completion contract** (Devin API): a worker
   turn cannot end until it satisfies a JSON-schema report — sharper than the
   sentinel-exit idea already backlogged (§19.7 idea 4).

Gaps every vendor shares (DevAgent opportunity, unchanged from §4.2): none syncs
ticket state after merge; none validates migration safety, FK integrity, or async
races pre-PR; Devin's per-session ACU cap and Copilot's 59-min wall are the only
*hard* ceilings near a single ticket — DevAgent's G-gates plus per-ticket cost
ceiling remain differentiated.

## Sources

Primary (official docs, fetched 2026-09-11): vendor doc indexes
[Devin](https://docs.devin.ai/llms.txt) ·
[Copilot](https://docs.github.com/en/copilot/concepts/agents/cloud-agent/about-cloud-agent) ·
[Codex](https://learn.chatgpt.com/docs/llms.txt) ·
[Jules](https://jules.google/docs/running-tasks.md) ·
[Factory](https://docs.factory.ai/llms.txt) ·
[OpenHands](https://docs.openhands.dev/llms.txt) ·
[Claude Code](https://code.claude.com/docs/en/hooks.md) ·
[Cursor](https://cursor.com/docs/cloud-agent.md) ·
[Amp](https://ampcode.com/docs/markdown/orbs) ·
[Qodo](https://docs.qodo.ai/code-review/remediate-findings-in-prs) ·
[Blitzy](https://docs.blitzy.com/project-lifecycle/aap-review).
Per-vendor page-level URLs are inline above. Known fetch gaps: Devin pricing page
(rate-limited), legacy openhands-action repo (404, superseded), Terragon (dead),
Copilot docs restructure invalidated the 2025-era URLs in §19.2 (cloud-agent paths
above are current). Full scout payloads preserved in session artifacts; this doc is
the merged, de-duplicated view.
