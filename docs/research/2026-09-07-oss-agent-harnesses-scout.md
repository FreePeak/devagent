# Research: 2025–2026 open-source agent harnesses — architecture ideas beyond the existing writeups

Date: 2026-09-07
Scope: survey of open-source coding-agent harnesses and adjacent sandbox/data infrastructure for adoptable architecture ideas. Excludes what `docs/research/openhands-sweagent.md` (ACI design, condenser basics, event-stream lesson) and `docs/research/longhorizon-harness.md` (MEA trust model, evidence-gated transitions) already cover, and PRD §19. Every claim carries a URL fetched on 2026-09-07.

Projects covered: mini-swe-agent, SWE-agent + SWE-ReX, OpenHands SDK v1 (2026 rework), Aider, Codex CLI, goose, cline, OpenCode, Claude Code, anthropics/sandbox-runtime, SWE-smith, SWE-Gym, SWE-rebench.

## 1. mini-swe-agent — radical simplicity as a spec

https://github.com/SWE-agent/mini-swe-agent — a ~100-line agent that scores >74% on SWE-bench verified. Its control flow (https://mini-swe-agent.com/latest/advanced/control_flow/) is a step loop with exactly three budget knobs in `AgentConfig`: `step_limit`, `cost_limit` (default $3.0), `wall_time_limit_seconds` — plus `max_consecutive_format_errors` (default 3): after 3 malformed model responses in a row, the agent exits instead of burning budget. Termination uses a `Submitted` exception raised by the environment when the model's command output starts with a magic string; all exceptions inheriting `InterruptAgentFlow` carry a message that is appended to the trajectory, so even failure exits leave an inspectable record. Environments are pluggable behind one `execute` interface (docs list local, docker, bubblewrap, singularity, modal, contree, swerex_docker).

Ideas:
- **[workers|gates] Sentinel exit protocol.** DevAgent parses worker streams with heuristics; instead require every headless worker to declare completion by emitting a magic string the herdr pane-run watcher greps for (a `Submitted`-class terminator), and treat *any* pane teardown without the sentinel as an abort with a structured reason. Cheap, model-agnostic, and kills the whole "what did the worker mean" ambiguity class.
- **[gates] Consecutive-format-error breaker.** Repeated malformed attempts (bad JSON, empty panes) should trip a counter and abort the attempt — not retry until the wall timeout. Our omp watchdog counts bytes; a `max_consecutive_format_errors`-style counter on parse failures is more decisive.
- **[orchestrator] Triple default budget per worker** (steps, cost, wall-clock) rather than only wall-clock; the $3/task default is a concrete starting point for per-run budget caps.

## 2. SWE-agent + SWE-ReX — separate deployment from execution

https://github.com/SWE-agent/SWE-agent is the ACI project already analyzed in `openhands-sweagent.md` — skip. Its newer infrastructure component SWE-ReX (https://github.com/SWE-agent/SWE-ReX) is not: "sandboxed code execution for AI agents, locally or on the cloud. Massively parallel." Architecture docs (https://github.com/SWE-agent/SWE-ReX/blob/main/docs/architecture.md) split a *deployment* layer (create/destroy environments: local, docker, fargate, modal, daytona, remote) from a *runtime* layer (execute commands inside a live environment), with a long-lived server per environment that fork-serves parallel agents.

Ideas:
- **[orchestrator] Deployment/runtime split behind one interface.** DevAgent's herdr pane-run assumes local tmux panes. Extracting an `ExecutionBackend` seam (local pane | docker | remote ssh) would let the loop keep working when panes wedge and enable fleet runs, without touching the planner/executor logic.
- **[workers] Bubblewrap-class local sandboxing** as one backend (mini-swe-agent ships a bubblewrap environment; SWE-ReX defaults to it) — cheap isolation that complements the compose-based G2.

## 3. OpenHands SDK v1 — the 2026 rework

`openhands-sweagent.md` covers V0's event-stream. The V1 redesign is new: arXiv:2511.03690 (https://arxiv.org/html/2511.03690v2, MLSys 2026, "complete architectural redesign"). Key verified facts:
- Four principles: optional isolation (local by default, sandbox opt-in); **stateless by default, one source of truth** (all components immutable and validated at construction; a single conversation state object holds mutable context); strict separation of concerns; two-layer composability.
- Production data: **V1 reduced system-attributable failures by 61% vs V0 over a 15-day comparison**, with negligible event-sourcing overhead.
- **Condensation as an event**: the Condenser writes a `CondensationEvent` into the log; the agent *applies* condensations when rendering the LLM view. The full event log is never destroyed — only the view is compressed. LLMSummarizingCondenser cuts API cost up to ~2x.
- **SecretRegistry**: per-conversation credential store, late-bound at execution time, all secret values masked in outputs (the Bash tool scans commands for secret keys and exports only referenced ones).
- **SecurityAnalyzer + ConfirmationPolicy**: an `LLMSecurityAnalyzer` appends a `security_risk` field (low/medium/high/unknown) to each tool call; `ConfirmRisky` blocks actions above a threshold, with dynamic relaxation ("adaptive trust") for safe read-only ops.
- Conversation factory: identical API for local and remote (containerized) execution.

Ideas:
- **[context|selfbuild] Condensation-as-event for the ledger.** Our earlier note (L4 in openhands-sweagent.md) said "converge on the ledger as the single stream" but left the board/worklog derived-state problem open. V1's answer is stronger: never mutate or delete history — derived views (the board JSON, retry-prompt context) are *rebuilt* by applying recorded condensation events. Immutable ledger + view-rebuild also gives deterministic replay, which V1 credits for the 61% failure reduction. If the ledger is already append-only, the remaining work is making every derived file a pure function of it.
- **[gates] Risk-field + confirmation policy** instead of binary gate pass/fail: each G-gate (or each model-issued action, when a worker is hooked) carries a risk rating; high-risk classes (migration edits, `git push`, dependency changes) require a second look, low-risk read-only ops skip checks. Adaptive trust lowers cost without lowering the ceiling.
- **[workers] SecretRegistry pattern.** Workers currently inherit the environment's credentials. A per-run secret store that injects only the secrets a run declares, masks values in all captured output, and dies with the run shrinks both leak surface and the "credential in the transcript" problem.

## 4. Aider — repo map and edit formats

https://aider.chat/docs/repomap.html: Aider sends a token-budgeted map of the whole repo (key classes/functions with signatures, ~1k tokens default, dynamically resized) built by **graph ranking over the file-dependency graph** — identifiers most referenced elsewhere rank highest. https://aider.chat/docs/more/edit-formats.html: edit formats are per-model (`whole`, `diff` search/replace, `udiff` for models prone to "lazy coding", `diff-fenced`, `editor-*`), chosen by model family. https://aider.chat/docs/usage/modes.html: **architect mode** — an architect model proposes changes in plain language; a separate editor model transcribes them into syntactically valid edits.

Ideas:
- **[context] Repo-map fallback provider.** LeanKG is the structural provider, but it is heavyweight and can be down/stale. A tree-sitter-based, PageRank-ranked, 1k-token repo map is a cheap deterministic fallback that fits the existing context-provider seam (`kgProvider`) and degrades gracefully.
- **[workers|lessons] Edit-format steering.** When the lessons digest flags a worker model "eliding code" or producing malformed patches, switch its edit contract (udiff vs whole-file) before switching models — a per-model remediation knob cheaper than model escalation.
- **[orchestrator] Architect/editor split for weak models.** For models that plan well but edit badly (we have some on the provider list), let the planner's output be consumed by a small deterministic "editor" pass that converts instructions into patches, keeping the expensive model out of the edit loop.

## 5. Codex CLI — sandbox + approval architecture

https://github.com/openai/codex (Rust, Apache-2.0). Security model (https://learn.chatgpt.com/docs/agent-approvals-security):
- Two layers: **sandbox mode** (read-only / workspace-write / danger-full-access, OS-enforced) and **approval policy** (when to stop and ask). Network is off by default in workspace-write; the `network_proxy` feature constrains enabled network to an allowlist-first domain policy (deny wins, DNS-rebinding checks block hostnames resolving to private IPs, local binding blocked by default).
- **Protected paths inside writable roots**: `<writable_root>/.git` is **read-only** even in a writable workspace (as are `.codex` and `.agents`) — the agent physically cannot rewrite history or tamper with its own instruction files.
- **Two-phase cloud runtime**: a setup phase with network (dependency install) whose secrets are removed *before* the offline agent phase starts.
- **Auto-review**: `approvals_reviewer = "auto_review"` routes approval-worthy actions through a reviewer agent that checks data exfiltration, credential probing, persistent security weakening, destructive actions; low/medium risk proceeds, critical denied. Plus asynchronous safety monitoring that can pause a task.
- Codex's own repo dogfoods its harness: `.codex/skills/` with `babysit-pr` (including `gh_pr_watch.py` + tests), `code-review-*` variants, remote-tests — the agent maintaining its own PR-watching procedure as a versioned skill.

Ideas:
- **[gates|orchestrator] Write-protect `.git` in worker worktrees.** Workers today can run arbitrary git in their pane; the working-tree sweep-in hazard (another session's commit absorbing uncommitted changes) is in our memory twice. Enforcing `.git` read-only inside worker sandboxes — commits and pushes happen only via the harness — would have prevented that entire class. This is the single most transferable idea in this survey.
- **[gates] Auto-review LLM gate for risky actions.** Instead of blocking everything or nothing: a cheap reviewer pass that scores risky actions (network use in G2, pushes, dependency edits) and only escalates critical ones. Maps directly onto our severity-gated validation philosophy.
- **[gates] Two-phase run with secrets hygiene**: dependency install allowed network + declared secrets; the actual build/test phase runs network-off with a domain allowlist. G2's compose runs could adopt this split verbatim.

## 6. block/goose — recipes, distros, self-hosted CI agents

https://github.com/block/goose (53,991★, moved to the Agentic AI Foundation in April 2026 — https://goose-docs.ai/blog/2026/04/07/goose-moves-to-aaif). Repo shows `.github/recipes/code-review.yaml` (declarative automation recipes committed to the repo), `goose-issue-solver.yml` and `goose-pr-reviewer.yml` GitHub workflows (the agent runs itself in CI as issue solver and PR reviewer), `.goosehints` (per-repo instruction file), and **Custom Distributions** (https://github.com/aaif-goose/goose/blob/main/CUSTOM_DISTROS.md — build your own goose distro with preconfigured providers, extensions, branding).

Ideas:
- **[product|selfbuild] Ship DevAgent as CI workflows.** We self-host via daemon + TUI; goose's pattern of publishing `devagent-issue-solver.yml` / `devagent-pr-reviewer.yml` as installable GitHub Actions would let external repos adopt the loop without the daemon, and give us free real-world flight hours.
- **[product] "Distro" profiles**: named bundles of provider config + gate set + lessons digest per target repo, so onboarding a new repo is one file instead of environment archaeology.

## 7. cline — SDK, Kanban, scheduled agents, connectors

https://github.com/cline/cline (67,620★; README raw: https://raw.githubusercontent.com/cline/cline/main/README.md). Products: SDK (`@cline/sdk`, programmatic tools + **lifecycle hooks for logging, auditing, policy enforcement**), headless CLI, and **Kanban** — "run many agents in parallel from a web-based task board. Each card gets its own worktree, auto-commit, and dependency chains" (https://github.com/cline/kanban). Also: **scheduled agents** (`cline schedule create --cron ...` persisting across restarts), **messaging connectors** (Telegram/Slack/Discord/**Linear** — "each conversation thread maps to an agent session"), multi-agent teams with persistent team state, checkpoint-based undo of all agent edits, and mid-work linter/compiler monitoring ("monitors linter and compiler errors as it works, fixing issues... before you even see them"; "for long-running processes like dev servers, Cline continues working in the background and reacts to new output as it appears").

Ideas:
- **[workers] Reactive stream watching.** Our no-progress watchdog kills; Cline's pattern *feeds*: tail the worker's long-running output (compile/test streams) and push new error lines back into the agent's context as they appear, so a compile error is fixed in the same attempt instead of after a gate failure round-trip.
- **[product] Linear thread replies.** DevAgent already reads Linear tickets; replying status/gate results into the same ticket thread (Cline's connector model) is a small feature that makes the loop observable to non-operators.
- **[product] Scheduled recurring agent tasks** (dependency checks, digest runs) as first-class cron config persisted by the daemon.

## 8. OpenCode — system agents, doom_loop, task permissions

https://opencode.ai/docs/agents/ (sst/opencode). Built-ins: primary agents Build/Plan; subagents General/Explore/**Scout** ("read-only agent for external docs and dependency research... clone a dependency repository into OpenCode's managed cache... cross-reference local code against upstream implementations"); **hidden system agents** (compaction, title, summary) that run automatically; per-agent `permission` keys including **`doom_loop`** ("Recovery prompts when an agent appears stuck") and `external_directory`; per-agent `max_steps`, `temperature`, model override; **`permission.task` globs** controlling which subagents an agent may invoke (denied ones are removed from the Task tool description so the model won't attempt them).

Ideas:
- **[context] Hidden system agents.** Model compaction, run summaries, and session titles as small automatic secondary workers rather than inline logic — matches our digest/curator structure and keeps the main loop lean.
- **[orchestrator] Task-permission globs.** Which worker roles may dispatch which sub-workers should be config, not convention — prevents the "subagent spawns subagent" cost blowups we saw in the 29GB runs-dir incident.
- **[workers] Doom-loop recovery prompts** as a model-facing mechanism (detect repeated identical tool calls → inject a recovery prompt) complementing the byte-count watchdog.
- **[context] Scout dependency cache** for cross-repo research without dirtying the workspace.

## 9. Claude Code — hooks, isolation enforcement, recorded recipes

https://code.claude.com/docs/en/hooks, /en/sub-agents, /en/memory, /en/skills. Verified mechanics most relevant here:
- **Hook lifecycle** with per-event decision JSON: `PreToolUse` can deny with a reason shown to the model; `PostToolUse` can rewrite tool output (`updatedToolOutput`) or inject context; **async hooks** run in background and deliver results next turn, with **`asyncRewake`** — an async hook exiting code 2 wakes Claude *even when idle* with its stderr as a system reminder.
- **Stop hook gating**: a Stop hook can block finishing and re-prompt ("run the test suite first"); `stop_hook_active` input plus an **8-consecutive-block cap** prevents infinite loops.
- **Worktree isolation enforcement** (sub-agents docs): with `isolation: worktree`, Claude Code "blocks a command that redirects git into the main checkout" and refuses commands it cannot verify stay inside the worktree — isolation checked at the command-content level, not just cwd.
- **Recorded recipes**: `/run-skill-generator` and `/verify` capture "what worked" (install commands, env vars, launch script) as a committed per-project skill so later runs "follow the recorded recipe instead of rediscovering it".
- **Auto memory**: Claude-written learnings loaded every session (first 200 lines / 25KB, per repository, shared across worktrees), distinct from user-authored CLAUDE.md; subagents can keep persistent memory too.
- Skills support **dynamic context injection** (`` !`git diff HEAD` `` lines executed before the model reads the skill).

Ideas:
- **[selfbuild|product] Expose a hook lifecycle on the DevAgent loop itself.** Named events (pre-dispatch, pane-start, gate-fail, pre-merge, ledger-append) with user-configurable command handlers would let operators add audit logging, policy checks, and notifications without forking `selfbuild-loop.sh` — the driver-bake/restart pain we live with exists because the loop has no extension points.
- **[gates] asyncRewake for G-gates.** Start the validation suite as a background job when the worker is still finishing; wake the worker immediately on failure with the failing output, skip the wake entirely on green. Removes a full serial wait from the common path and short-circuits the fail path.
- **[gates] Stop-gate with cap.** Port the Stop-hook pattern: the worker's "done" is refused with "suite not green — fix and continue" up to N consecutive refusals, then the attempt aborts with a structured reason. Currently our retry only happens post-hoc via the orchestrator.
- **[gates] Command-content git isolation**: block/rework any worker command that redirects git output into the main checkout or `cd`s out of its worktree — the enforceable version of the hygiene we currently rely on discipline for.
- **[lessons] Recorded recipes → lessons digest.** Lessons currently describe *what failed*; a recipe system captures *what worked* (exact fix sequence, launch commands) into a committed artifact the next attempt loads. This is the positive-space complement to our failure-only digest.

## 10. anthropics/sandbox-runtime — OS sandbox with violation feedback

https://github.com/anthropics/sandbox-runtime ("lightweight sandboxing... at the OS level, without requiring a container"; macOS sandbox-exec, Linux bubblewrap+seccomp, Windows alpha). Verified specifics:
- Filesystem: **writes denied by default** (`allowWrite` allowlist with `denyWrite` override); reads allowed by default with `denyRead`.
- Network: allow-only domain list with optional `:port` suffixes, unix sockets blocked by default, optional TLS-terminating MITM proxy.
- **Violation attribution**: each wrapped command carries a `commandId`/`commandText` key; violations (seatbelt logs, seccomp events, proxy denies) are stored per-key and can be replayed into the command's stderr via `annotateStderrWithSandboxFailures(key, stderr)` / `getViolationsForCommand(key)`.
- **Model-facing deny reasons**: `deniedDomainReasons` maps a deny rule to a reason string surfaced to the model, e.g. `"github.com:22": "SSH pushes to GitHub are blocked; use an https:// remote"`.

Ideas:
- **[gates|workers] Violations as feedback, not just enforcement.** DevAgent G2 runs in compose and discards the *why* of sandbox denials. Adopting attribution + stderr annotation means a worker whose command was blocked learns *why* in-band ("SSH push blocked; use https remote") and self-corrects in the same attempt — enforcement that teaches instead of just failing.
- **[gates] srt-wrap G2 commands** instead of (or inside) full compose: allowWrite-only filesystem scoped to the worktree, network allow-listed to the registry/package host. Faster than container spin-up and available on the macOS host where compose isn't.
- **[gates] Allow-only network default** for every worker command, with per-repo allowlists in the distro profile (idea from §6).

## 11. Data flywheels — SWE-smith, SWE-Gym, SWE-rebench

- **SWE-smith** (https://github.com/SWE-bench/SWE-smith, NeurIPS 2025 spotlight): procedurally generates bugs at scale (`configs/bug_gen/`: `class_basic`, `func_fun`, `lm_modify`, `lm_rewrite`) plus issue generation from test suites (`configs/issue_gen/`), yielding training trajectories and difficulty ratings without human labor.
- **SWE-Gym** (https://github.com/SWE-Gym/SWE-Gym, ICML 2025): first training environment for real-repo SWE agents; also trained **verifiers/critics** on agent trajectories (scripts/openhands-verifier, moatless-verifier) — the OpenHands critic trained on it reached SOTA on SWE-bench Verified with inference-time scaling.
- **SWE-rebench** (https://github.com/SWE-rebench, org page; paper arXiv:2505.20411): fully automated pipeline mining real GitHub issues into tasks with environment setup, and **decontamination analysis that discovered contamination in well-known LLMs**; V2 is language-agnostic (https://huggingface.co/papers/2602.23866).

Ideas:
- **[selfbuild|gates] Synthetic-bug gate validation.** Use SWE-smith-style procedural mutations (drop a call, swap an argument, remove a guard) on DevAgent's own codebase and assert the G-gate suite catches them. This measures *gate sensitivity* — a deterministic oracle for whether our gates would catch a plausible regression — and is exactly the kind of ledger-verifiable pick the selfbuild loop wants.
- **[merge|gates] Trajectory-informed critic as a pre-merge gate.** Without training anything: mine our accumulated ledger + gate outcomes into a rubric a cheap model applies to each PR diff before autoMerge (the SWE-Gym verifier recipe, implemented as a prompted critic over our own history). Distinct from the async-review we have: it is primed on *our* failure distribution, not generic review.
- **[selfbuild] Decontamination discipline.** Before declaring a selfbuild iteration "verified," check the eval ticket isn't one the loop already shipped/absorbed (our `already_shipped` phantom-pick class) — SWE-rebench's contamination checks are the formalized version of a check we do ad hoc.

## 12. Priority ordering for DevAgent

| # | Idea | Surface | Why first |
|---|------|---------|-----------|
| 1 | Write-protect `.git` in worker sandboxes (Codex) | gates/orchestrator | kills a proven recurring failure class (sweep-in history rewrites) |
| 2 | Sandbox violation attribution + model-facing reasons (srt) | gates/workers | turns enforcement into same-attempt self-correction |
| 3 | asyncRewake-style background gates (Claude Code) | gates | removes the serial gate wait; fail-fast with in-band errors |
| 4 | Condensation-as-event + immutable-config ledger views (OpenHands V1) | context/selfbuild | 61% system-failure reduction claim validates the ledger-convergence path |
| 5 | Sentinel exit protocol (mini-swe-agent) | workers/gates | removes completion-ambiguity across all worker CLIs |
| 6 | Stop-gate with cap (Claude Code) | gates | completion requires green suite inside the attempt, not after |
| 7 | Hook lifecycle for the driver (Claude Code) | selfbuild/product | ends the fork-the-bash-script extension model |
| 8 | Synthetic-bug gate validation (SWE-smith) | selfbuild | deterministic oracle for gate sensitivity |
| 9 | Recorded success recipes (Claude Code) | lessons | positive-space lessons, loadable by the next attempt |
| 10 | Repo-map fallback provider (Aider) | context | cheap deterministic context when LeanKG is stale/down |
| 11 | Risk-field + confirmation policy (OpenHands V1, Codex auto-review) | gates | graduated enforcement instead of binary gates |
| 12 | Edit-format steering / architect-editor split (Aider) | workers | cheaper than model escalation for weak editors |
| 13 | Reactive stream watching (cline) | workers | fixes compile errors in-attempt |
| 14 | Two-phase runtime + secrets hygiene (Codex, OpenHands SecretRegistry) | gates/workers | network only where needed; secrets die with setup |
| 15 | CI-workflow distribution + Linear thread replies (goose, cline) | product | adoption surface beyond the daemon |

## Sources

- https://github.com/SWE-agent/mini-swe-agent
- https://mini-swe-agent.com/latest/advanced/control_flow/
- https://github.com/SWE-agent/SWE-agent
- https://github.com/SWE-agent/SWE-ReX
- https://github.com/SWE-agent/SWE-ReX/blob/main/docs/architecture.md
- https://arxiv.org/html/2511.03690v2 (OpenHands Software Agent SDK, MLSys 2026)
- https://github.com/OpenHands/software-agent-sdk
- https://aider.chat/docs/repomap.html
- https://aider.chat/docs/more/edit-formats.html
- https://aider.chat/docs/usage/modes.html
- https://github.com/openai/codex
- https://learn.chatgpt.com/docs/agent-approvals-security
- https://github.com/block/goose
- https://github.com/aaif-goose/goose/blob/main/CUSTOM_DISTROS.md
- https://goose-docs.ai/blog/2026/04/07/goose-moves-to-aaif
- https://github.com/cline/cline
- https://raw.githubusercontent.com/cline/cline/main/README.md
- https://github.com/cline/kanban
- https://opencode.ai/docs/agents/
- https://code.claude.com/docs/en/hooks
- https://code.claude.com/docs/en/sub-agents
- https://code.claude.com/docs/en/memory
- https://code.claude.com/docs/en/skills
- https://github.com/anthropics/sandbox-runtime
- https://github.com/SWE-bench/SWE-smith
- https://github.com/SWE-Gym/SWE-Gym
- https://github.com/SWE-rebench
- https://huggingface.co/papers/2602.23866 (SWE-rebench V2)
