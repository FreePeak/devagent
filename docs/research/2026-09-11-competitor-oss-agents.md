# Open-source autonomous SWE agents — automatic dev-workflow anatomy (2026-09-11)

Refresh of `docs/research/2026-09-07-oss-agent-harnesses-scout.md` (harness-level ideas) and `docs/research/openhands-sweagent.md` (ACI + condenser lessons, 2026-08-24). This file goes one level deeper into **pipeline mechanics** — how each system actually turns an issue into a validated PR — and adds what changed since those writeups. Affiliation note: per the paper and blog, CodeMonkeys is Stanford's Scaling Intelligence Lab + University of Oxford; the "Scale AI" attribution circulating for it is [unverified] — neither primary source mentions Scale AI. Repo-status facts below were checked via GitHub API on 2026-09-11 unless marked otherwise.

## OpenHands (All Hands AI → "OpenHands"; ex-OpenDevin) — SDK V1, Agent Canvas, Cloud

**What it is / status (2026-09)**: The largest open-source coding-agent platform (87.3k stars, `OpenHands/OpenHands`, pushed 2026-09-10). Two generations live simultaneously: V0 = monolithic app with EventStream + AgentController (paper arXiv:2407.16741); **V1 (2025-11 →) = a complete rewrite as a 4-package Software Agent SDK** (`openhands.sdk/tools/workspace/agent_server`, arXiv:2511.03690). The V1 paper's production data: system-attributable failures down 61% vs V0 over a 15-day A/B. V1 already has `.41.x` point releases in the wild (cubxxw architecture writeup sampled v1.41.0). Company pivoted branding from "All Hands AI" to OpenHands; products: Agent Canvas (self-hostable web app, formerly the GUI), CLI, OpenHands Cloud, OpenHands Enterprise (K8s/Helm + Sysbox).

**Trigger surfaces**: (a) Cloud GitHub/GitLab/Bitbucket app — repo events open conversations; Jira/Linear integrations "coming soon" (Jira Cloud doc exists); (b) V0 GitHub Action resolver: label `fix-me` on an issue → agent session → branch + PR (see Workflow); (c) V1 SDK GitHub workflows: `pr-review` (label or all PRs → inline review comments), `assign-reviews`, `todo-management`, automated QA/dependency-upgrade use-cases; (d) Automations: cron **and event-driven** (GitHub webhook → conversation) in Agent Canvas; (e) CLI/headless: `openhands --headless --task … --json` (JSONL output) for CI; Slack app; `/launch` plugin URL route.

**Workflow pipeline (stage by stage)** — V1 SDK single-conversation path:
1. **Trigger** → client creates a `Conversation` (agent config, workspace type, security policy, skills). Identical code for local and remote: `LocalWorkspace` / `DockerWorkspace` / `RemoteAPIWorkspace` are swapped behind one interface; `DockerWorkspace` auto-spawns an `agent-server` (FastAPI+WS) inside the container.
2. **Context assembly**: skills are the V1 successor of V0 "microagents" — repo skills (`.openhands/`), **global keyword-triggered skills** from a registry (`OpenHands/extensions`), and **path-triggered rules** that are injected deterministically whenever the agent touches a matching glob (Claude-Code-rules semantics, no model discretion).
3. **Reasoning-act loop**: stateless `Agent` emits typed `Action`s; the Conversation executes them via `Tool` executors (TerminalTool, FileEditorTool — or **ApplyPatchTool under the GPT-5 preset** — plus MCP tools) and appends `Observation`s. Everything is an immutable event on an append-only log; state = log + derived views; deterministic replay. Conversation exposes `run()`, pause/resume, fork, send-message-while-running, ask-agent sidebar questions.
4. **History condensation**: Condensers compress the *LLM view* only; a `CondensationEvent` is itself written to the log, so compression is auditable/replayable. `LLMSummarizingCondenser` halves API cost in the paper's measurement.
5. **Security gate**: optional `LLMSecurityAnalyzer` (separate LLM instance) tags every action `security_risk ∈ {low, medium, high, unknown}`; `ConfirmationPolicy` (`AlwaysConfirm` / `NeverConfirm` / `ConfirmRisky`) holds the conversation at `WAITING_FOR_CONFIRMATION`; rejection feeds a reason string back ("Please try a safer approach"). `SecretRegistry` late-binds per-conversation secrets and masks values in outputs.
6. **Stuck detection (default-on)**: semantic detector over the post-last-user-message event history — flags ≥4 identical action-observation pairs, ≥3 action-error pairs, ≥3-message agent monologue, ≥6 alternating ping-pong cycles, and repeated context-window errors; comparison is by semantic content, not event identity. Halts execution when tripped.
7. **Termination options**: plain `run()` ends when the agent *claims* done; two stricter drivers exist: **Critic** (experimental; per-run LLM quality score 0–1, auto follow-up refinement while score < threshold (default 0.6), `max_iterations` default 3) and **Goal Completion Loop** (`run_goal`: independent judge LLM audits the transcript for *verifiable evidence* (test output, file contents), returns `{score, complete, missing}`, re-prompts with `missing` until complete or `max_iterations` default 10; conservative parse fallback = "not complete" so the loop can't falsely finish).
8. **PR creation** — V0 resolver mechanics (`openhands/resolver`, PyPI `openhands-resolver`): GitHub Action `on: issues: labeled` + `if label == fix-me`; job checks out repo, feeds issue title/body **plus all issue comments** to a headless OpenHands run; `send_pull_request.py` opens a PR from branch `openhands-<issue>` (with progress-comment + link-back on failure to fix: "the resolver wasn't able to fix the issue (and a branch with its intermediate progress)"). Known failure mode captured in the repo: the model creating a PR itself makes `send_pull_request` create a duplicate (#13007); missing-app-permissions errors surface as opaque failures (#11472). V1 replaces this with SDK GitHub workflows + Cloud app; the resolver code remains as legacy V0. Self-hosted resolver dogfooding stat (2024): 37% of resolver commits authored by the agent.

**Isolation & sandbox**: Docker sandbox is the recommended local provider; `Process` sandbox explicitly documented as *no isolation*; Remote sandbox (Cloud/agent-server API) for scale. Enterprise adds Sysbox-in-K8s and "Docker-in-sandbox" (compose/builds inside the agent's container without host privileges). Warm-pool deferred-init pre-warms agent-server pods before user matching.

**Validation & self-repair loop**: No fixed validation harness in the product — the model runs tests in-sandbox; the *quality* mechanisms are the critic/goal-loop/judge stack above plus (for SWE-bench) the trained critic. Self-repair = critic-driven iterative refinement, or goal-loop re-prompting with judge-computed "missing" evidence.

**Human gates & approval UX**: confirmation-mode console flow (preview each pending action with the model's own `summary` field as headline, yes/no, default-**no** on EOF/Ctrl-C), `ConfirmRisky` for gated autonomy, conversation-level pause/resume and send-message-while-running for mid-run steering, Cloud org budgets with 80/90/100% alerts and hard block at cap, per-user lifetime budgets.

**Cost model / metering mechanics**: every LLM is created with a `usage_id` and registered in an `LLMRegistry`; `llm.metrics` accumulates per-call cost, tokens (incl. cache read/write, reasoning tokens), and latencies; `conversation.conversation_stats` aggregates across agent + condenser + judge LLMs — i.e., auxiliary-model costs are attributable per-role. Cloud meters via credits and org/user budgets (REST `…/members/financial`). `LLM Fallback Strategy` retries transient errors on alternate models; LLM subscriptions (ChatGPT Plus/Pro → Codex models) and LiteLLM keep provider-agnostic access.

**What DevAgent can learn or steal**:
- **Stuck-detector thresholds are directly liftable**: 4×/3×/3×/6× pattern counts as a cheap post-hoc loop breaker complements our byte-count watchdog (which memory shows needs thinking-delta stripping anyway).
- **Judge-audits-transcript termination** (Goal Completion Loop) is exactly DevAgent's "evidence-attached" gate: `{score, complete, missing}` JSON + re-prompt with `missing` + cap, and the "unparseable verdict ⇒ not complete" fallback is the correct conservative direction.
- **`usage_id` cost attribution per auxiliary role** maps 1:1 onto per-gate/per-reviewer cost accounting in the ledger.
- **Path-triggered rules** (deterministic, model-independent injection) beat prompt-suggested conventions; DevAgent could inject migration-safety rules on `migrations/**` touches at the tool layer, not in the task prompt.
- Duplicate-PR and opaque-permission-error issues (#13007/#11472) are cautionary for our own issue→PR path.

**Sources**: docs.openhands.dev (llms.txt index; /sdk/arch/overview; /sdk/guides/agent-stuck-detector; /sdk/guides/security; /sdk/guides/convo-goal; /sdk/guides/critic; /sdk/guides/metrics; /openhands/usage/developers/evaluation-harness; /openhands/usage/cloud/organizations/budgets; /openhands/usage/cli/command-reference); arXiv:2511.03690 (SDK paper); arXiv:2407.16741 (V0 paper); openhands.dev blogs (resolver; SOTA-with-critic); GitHub OpenHands/OpenHands, openhands-resolver PyPI + issues #13007/#11472.

## SWE-agent + mini-SWE-agent (Princeton NLP) — the ACI as the product

**What it is / status (2026-09)**: `SWE-agent/SWE-agent`, MIT, ~19.1k stars, actively maintained. v1.0+ moved all sandboxing to the separate **SWE-ReX** runtime (already covered in the 09-07 scout: deployment/execution split, Local/Docker/Modal/Fargate backends, sentinel-based shell completion detection `###SWE-REX-COMPLETE-<id>###`). **mini-SWE-agent**: ~100-line ReAct loop scoring 65% on SWE-bench Verified (Claude 3.7 Sonnet) — now the *reference harness for the SWE-bench Verified "Bash Only" leaderboard view* (see Cluster synthesis / SWE-bench section). EnIGMA (arXiv:2409.16165, ICML 2025) extends the same framework to CTF (13.5% NYU CTF).

**Trigger surfaces**: CLI `sweagent run` (single GitHub/local issue) and `sweagent run-batch` (SWE-bench subsets, `--num_workers`); `--actions.open_pr` opens a PR with the diff; problem statements from GitHub issue URL, file, or inline text. No productized GitHub label→PR automation.

**Workflow pipeline (stage by stage)**:
1. **Setup**: YAML config (`agent:` / `env:` / `problem_statement:`); tool *bundles* are installed into the sandbox at startup (`install.sh` + `bin/` on PATH); persistent shell session created via SWE-ReX.
2. **Prompt assembly**: system + instance templates render `{{command_docs}}` from bundle YAML signatures — swapping bundles regenerates the tool contract. Empirically-derived tips are baked in ("use `goto 583`, don't scroll", "a command that failed once won't work again unless modified").
3. **Step loop**: `while not done: step()` — model → parsed action → executed in the persistent shell → observation (capped at `max_observation_length=100_000` chars) → next.
4. **Guardrails before execution (100%-precision only)**: `bash -n` syntax check rejects malformed shell without executing; the `edit` tool lints with a hand-picked flake8 ruleset (`--select=F821,F822,F831,E111,E112,E113,E999,E902` — "is this broken Python", not style), **reverts the edit** on newly-introduced errors, and prints a three-part message (error type / window-as-it-would-be / window-original) — the team validated all three parts: omit any and agents re-issue broken edits or misdiagnose. Pre-existing errors are shifted out of the diff so the agent isn't blamed for them.
5. **History processors** run before every model call: `cache_control` (Anthropic prompt-cache breakpoints on last 2 messages) and `last_n_observations` (drop all but last 5 observations' stdout, keep thoughts/actions).
6. **Termination — exactly four paths, all ending in a *degraded success***: (i) `submit` sentinel `<<SWE_AGENT_SUBMISSION>>` (emits `git diff --cached` as the patch; deliberately runs **no** tests — evaluation is external, so submit can't peek at gold signal); (ii) voluntary `exit` / `exit_forfeit`; (iii) **autosubmit-on-error** — cost limit, context overflow, total timeout, consecutive timeouts, content-policy violations all trigger one last `git diff` and ship the partial patch with a typed exit status (`exit_cost`, `exit_context`…); (iv) format-error budget (`max_requeries=3`) exhausted → autosubmit.
7. **Retry layer**: `RetryAgent` runs N full attempts with different configs; **unused budget from a failed attempt rolls into the next attempt**; optional reviewer-on-submit LLM picks the patch — team reports *mixed* results (reviewers sometimes reject correct patches), so it's opt-in.
8. **Multi-attempt selection in benchmarks** is delegated to external rankers (see CodeMonkeys "Barrel of Monkeys").

**Isolation & sandbox**: Local (subprocess), Docker, Modal (1000+ parallel), Fargate behind one `communicate()` interface; bubblewrap available locally. SWE-ReX detects shell completion via unique sentinel appended to every command — that's what makes `gdb`/`ipython`/`nc` usable as multi-turn tools.

**Validation & self-repair loop**: no test-loop inside the agent (by design); self-repair happens only through the guardrail requery ladder (FormatError → re-prompt; blocked action → re-prompt; bash syntax error → re-prompt; timeouts counted separately with `max_consecutive_execution_timeouts=5`). **Deliberately no semantic stuck detection**: the team tried it and dropped it for false-positive cost; the only mitigation is prompt text ("DO NOT re-run the same failed edit command").

**Human gates**: none beyond config; the whole point is unattended operation.

**Cost model / metering mechanics**: **cost is the primary stop signal, not steps** (`per_instance_cost_limit=$3.0` default, `total_cost_limit`, `per_instance_call_limit`) — chosen because step counts vary ~5× across model families; LiteLLM raises `CostLimitExceededError` → autosubmit. ACI ablation headline: raw-bash-only GPT-4 drops from ~12% to ~3% on SWE-bench full — the interface, not the model, is most of the delta. Fail-fast economics (openhands-sweagent.md L1): resolved median $1.21/12 steps vs unresolved $2.52/21 steps.

**What DevAgent can learn or steal**:
- **Autosubmit-on-every-failure-path**: any wall-time/budget/parse abort should still emit `git diff` + typed exit status; our loop currently burns attempts on timeouts that carry recoverable partial work in the worktree.
- **Budget roll-over across retries** (unused $ from attempt N funds attempt N+1) is a better retry policy than equal caps.
- **Diff-scoped edit lint with pre-existing-error exclusion** is a sharper version of "surface gate rules up-front": the worker can't even *write* a broken edit.
- The "100%-precision guardrails only" doctrine is a useful audit bar for any G-gate we add: does it fire only when something is definitely wrong?
- Search-tool output contract (filenames + match counts, hard refusal >100 files, explicit end markers) is a cheap context-hygiene spec for worker prompts.

**Sources**: arXiv:2405.15793 v3 (NeurIPS 2024 paper incl. Appendix A lint-message ablations); swe-agent.com ACI docs; SWE-agent/SWE-agent + SWE-ReX repos; dev.to code-level walkthrough (Apr 2026, cross-checked against paper + source paths); mini-swe-agent.com control-flow + v2 migration docs; swebench.com.

## Agentless (NUS/UIUC) + Agentless pipeline lineage

**What it is / status (2026-09)**: Research artifact (`OpenAutoCoder/Agentless`, arXiv:2407.01489), not a product; but it's the strongest orchestration-vs-autonomy datapoint in the field: 32.0% SWE-bench Lite / 38.8% SWE-bench Verified with GPT-4o at ~$0.34/task — beating most agent harnesses on the same model, adopted by OpenAI as the reference scaffold for GPT-4o/o1 announcements. Its author lineage spawned **SpecRover** (spec inference + reproduction tests), **SWE-smith** (training-data), and "Agentless-Lite" successors; per-neighbor-citation, test-time scaling with discriminators is now standard (see CodeMonkeys).

**Trigger surfaces**: batch CLI over issue JSON; no GitHub integration.

**Workflow pipeline (stage by stage)** — fully deterministic, zero agentic tool-use decisions:
1. **Localize → files**: repo rendered as an indented `tree` structure + issue → LLM names top-3 suspicious files; in parallel, LLM marks *irrelevant* folders, remaining files are chunked (512 tokens), embedded (`text-embedding-3-small`), cosine-ranked against the issue; union of the two lists.
2. **Localize → elements**: file *skeletons* (class/function headers + signatures + module comments only) for all suspicious files in one prompt → LLM lists suspicious classes/functions.
3. **Localize → edit locations**: full code of those elements → 4 sampled passes → concrete line/function/class edit locations.
4. **Repair**: per location set, context window ±10 lines → sample **40 patches per issue** (1 greedy + 39 @ T=0.8) in Aider-style **search/replace diff format** (small edits ≫ hallucination-prone whole-file rewrites).
5. **Reproduction-test generation**: 40 sampled standalone tests with a tri-state contract (`Issue reproduced` / `Issue resolved` / `Other issues`); execute each on the *unpatched* repo, keep only tests that actually reproduce; normalize and pick the majority-vote test.
6. **Patch selection**: run all existing tests on the base repo → candidate regression set → **LLM excludes tests the fix might legitimately update** (non-regression filtering) → keep patches with fewest regression failures → require `Issue resolved` on the reproduction test (fallback to regression-only if none pass) → AST-normalize patches (unparse with docstrings stripped) and majority-vote the final submission.
7. Output: single patch; no PR, no repair iteration (generation is one-shot sampling ×40, not an agent loop).

**Isolation & sandbox**: none needed beyond the test execution environment (SWE-bench docker); that's part of the cost story.

**Validation & self-repair loop**: the "loop" is replaced by **sample-then-select**: coverage is bought with parallel sampling, correctness with an independent verifier stage (tests + majority vote). Notable negative results: agent approaches *lose* accuracy on issues that ship reproducible code examples (they waste turns re-deriving reproduction), while Agentless's explicit test stage exploits them; agents retain an edge only where the issue gives **no** location clue.

**Human gates**: none. **Cost model**: ~$0.34/issue average (single source, paper Table); cost concentrated in sampling, not localization.

**What DevAgent can learn or steal**: the **tri-state reproduction-test contract** and **"execute the generated test on the unpatched repo before trusting it"** filter are directly portable to a DevAgent validation gate (never let a model-authored test count as evidence unless it fails pre-patch); **LLM-filtered regression-test exclusion** prevents false gate failures on legitimately-changed behavior; AST-normalized majority voting is a cheap upgrade over single-attempt dispatch.

**Sources**: arXiv:2407.01489 v2 (method §3, eval §5-6); OpenAutoCoder/Agentless GitHub; cross-refs from CodeMonkeys paper.

## AutoCodeRover (NUS → AutoCodeRoverSG)

**What it is / status (2026-09)**: `AutoCodeRoverSG/auto-code-rover` (repo moved from `nus-apr`), 3.1k stars, **dormant since 2025-04-24** (last push); claims 46.2% SWE-bench Verified (v20240620) at **<$0.7/task, ~7 min/task**. ISSTA 2024 paper (arXiv:2404.05427). Effectively a research end-station; its ideas (structure-aware search, reproduction-driven localization) live on in successors.

**Workflow pipeline (stage by stage)** — v1 two-stage, v2 adds three agents:
1. **Context retrieval (structure-aware)**: LLM drives *code-search APIs that operate on the AST*, not strings — `search_class`, `search_method`, `search_code`, `view_class`, `view_method`, `get_code_context_from_related` — iteratively collecting context in a bounded retrieval loop; when a test suite exists, **spectrum-based fault localization (SBFL)** from failing-test coverage weights the search toward suspicious files (`app/analysis/sbfl.py`).
2. **Patch generation**: write patch over retrieved context (multi-signal, "chunked" retrieval).
3. **Reproducer agent (v2)**: first an LLM guard checks the issue contains reproducible steps (JSON `has-reproducible-example`); then generates `reproducer.py` with a **mandated stack-trace printer** (so downstream localization gets exact line numbers), executes it, and iterates with execution feedback (up to `retries=3`); tests that don't reproduce are registered in a `non_repro_history` with feedback fed into the next attempt.
4. **Reviewer agent (v2)**: PR-reviewer persona re-analyzes root cause and fix over generated patches.
5. **Select agent (v2)** (`app/agents/agent_select.py`): analyze root cause → analyze resolution → rank patches → **3× voting with n=3 samples (9 votes, majority)** and an explicit least-change bias ("choose the one that makes the least changes"); out-of-vote selection raises.

**Isolation**: Docker for end-user runs; per-instance conda testbeds for SWE-bench.

**Validation & self-repair**: reproduction-first localization (a failing reproducer *is* the validation artifact); SBFL is a dynamic gate nobody else in this cluster ships.

**Human gates**: none. **Cost**: <$0.7/task single-source (README).

**What DevAgent can learn or steal**: **repro-with-mandated-stack-trace** as a structured localization signal (turns a generated test into a map for the editor); **vote-with-least-change-bias** patch selection; SBFL-style weighting when a repo has usable tests — closer to DevAgent's compose-based validation than pure-LLM rankers.

**Sources**: AutoCodeRoverSG GitHub (README + agent sources quoted); arXiv:2404.05427 (ISSTA 2024).

## Aider

**What it is / status (2026-09)**: Interactive pair-programming CLI (Paul Gauthier); not an autonomous issue→PR agent, but the origin of two mechanisms everything else copied (repo map, search/replace edits). Repo `Aider-AI/aider`, active; `--architect`, `--auto-lint`, `--auto-test` flags.

**Trigger surfaces**: chat CLI; headless `--message --yes-always` exists but a filed issue (#4923) documents that headless mode applies edits in a single pass without auto-test verification — CI use is explicitly not the design point.

**Workflow pipeline (stage by stage)**:
1. **Repo map**: tree-sitter-tagged dependency graph ranked PageRank-style over file references, token-budgeted (~1k tokens, dynamically resized) — covered in detail in the 09-07 scout (§4); unchanged.
2. **Reasoning vs formatting split (architect mode)**: Architect model *describes* the solution in whatever form it likes; a separate Editor model converts the description into concrete edits in Aider's edit format. Benchmark effect is large and consistent: o1-preview+DeepSeek = 85% (SOTA at the time) vs 79.7% baseline; even self-pairing helps (Sonnet 77.4→80.5, GPT-4o 71.4→75.2). Root cause: a single prompt forces the model to split attention between solving and format-compliance; reasoning-strong models are format-weak.
3. **Edit application**: per-model edit format (`whole`, search/replace `diff`, `diff-fenced`, `udiff`) with fallback/retry on malformed blocks (09-07 scout).
4. **Post-edit lint gate**: built-in linters per language; user `--lint-cmd "lang: cmd"` contract = print errors to stdout/stderr + non-zero exit; aider feeds the error text back and **re-edits until lint passes**.
5. **Post-edit test loop**: `--test-cmd <cmd>` + `--auto-test`: after every AI edit (post-lint), run the suite; non-zero exit → error output is injected and aider fixes and re-runs. Compiled languages are folded in by making `--test-cmd` build+test (e.g. `dotnet build && dotnet test`); formatter-as-linter needs the run-twice shell-script idiom documented in their docs.
6. **Commit**: aider commits each accepted change itself (atomic commits, conventional-message generation).

**Isolation**: none (it edits the user's working tree by design). **Validation**: exactly the lint/test contract above — no sandbox, no PR gate. **Human gates**: the user is the gate, in the loop per message. **Cost**: per-message, no budget machinery.

**What DevAgent can learn or steal**: the **architect/editor split is a per-model remediation knob**, not an architecture: when the lessons digest flags a weak-editing model, route its plan through a cheap editor pass (also in 09-07 scout §4); the **run-twice formatter wrapper** solves the "formatter exits non-zero and looks like a lint failure" problem our G-gates will eventually hit.

**Sources**: aider.chat/2024/09/26/architect.html; /docs/usage/modes.html; /docs/usage/lint-test.html; Aider-AI/aider#4923.

## Dead / faded systems worth a line

- **Mentat (AbanteAI)**: early CLI coding agent (2023); CLI + mentat.ai discontinued and the GitHub repo now **returns 404** (removed, not just archived — single secondary source says "archived + discontinued"; the 404 is verified 2026-09-11). Its follow-up **MentatBot** competed on SWE-bench Lite with an explicit test-generation stage (noted in Agentless §6.2).
- **AutoCodeRover**: dormant (last push 2025-04).
- **CodeMonkeys**: open-sourced then frozen as a paper artifact (repo `scalingintelligence/codemonkeys`, trajectories published in `swe-bench/experiments#171`); its techniques were absorbed into the leaderboard meta (see synthesis).

## Notable SWE-bench Verified leaderboard entries & how their workflows are structured

**Leaderboard mechanics (2026 state)**: swebench.com now splits Verified into the full **system** leaderboard (arbitrary scaffolds, includes "run performed or directly checked by the SWE-bench team" badges) and the default **"Bash Only" view**: *every model evaluated in the same mini-SWE-agent environment* — no tools, no scaffold, one ReAct loop over bash — to measure models instead of harnesses. mini-SWE-agent release numbers version the bash-only rows (1.x string-parsed actions vs 2.x tool-calling are explicitly not comparable; temperature 0.0 in 1.x, unset in 2.x). The site ships resolved-vs-cost, cost-limit and step-limit curves, and per-repo matrices — cost-normalized reporting is now a first-class leaderboard feature. (Exact 2026 top scores: [unverified — leaderboard data is client-side rendered; last verified anchors: OpenHands 66.4% with 5 rollouts + critic (Apr 2025), mini-SWE-agent 65% (Jul 2025).])

**CodeMonkeys (Stanford Scaling Intelligence Lab / Oxford — arXiv:2501.14723)** — the reference batch-and-select system, 57.4% Verified with Claude 3.5 Sonnet, **$2,300 total for all 500 instances (~$4.6/instance)**:
1. **Context**: every non-test Python file read in parallel by a cheap local model (Qwen2.5-Coder-32B) labelling relevant/not-relevant → Sonnet ranks → top files capped at 120k tokens. Costs are **amortized across all downstream samples** (15% of total spend).
2. **Generation**: two chained state machines per candidate — a *testing* SM iterates a reproduction-test script (≤8 iterations), then an *editing* SM iterates the codebase edit seeded with the test output (≤8 iterations); 10 parallel (edit,test) candidates per instance. Edit stage = 60% of cost (mostly cache reads from re-sending code context).
3. **Selection**: model-generated tests vote → top-3 by tests-passed → dedicated selection SM writes discriminating extra tests and picks a final edit (<6% of cost).
4. **Barrel of Monkeys**: the same selection SM run over an *ensemble* pool (CodeMonkeys + top-4 leaderboard submissions) scores 66.2% — beating every member individually. Selection generalizes across heterogeneous scaffolds; coverage ceiling 80.8% (oracle) shows how much is lost to selection, not generation.

**OpenHands critic (Apr 2025)**: instead of prompt-based rerankers, a **trained critic** — TD-propagated terminal test-pass/fail rewards (γ=0.99) backward through trajectory steps, regression head on Qwen2.5-Coder-32B via veRL, served with a token-classification vLLM fork (whose functional code was written by OpenHands itself). 60.6% single-trajectory → 66.4% with 5 rollouts + critic selection; model public on HuggingFace. V1's SDK Critic (score-per-run + iterative refinement) is the productization of this line.

## Cluster synthesis

- **Shared skeleton**: trigger → context assembly → generate → *execute-and-observe loop with 100%-precision guardrails* → evidence-based selection/validation → ship patch even when degraded. Nobody has a working "semantic" self-repair judge except OpenHands V1 (goal loop/critic) — everyone else either samples more (Agentless, CodeMonkeys, OpenHands critic) or doesn't loop at all (SWE-agent).
- **The field split is sampling vs looping, not agentic vs agentic.** Agentless and CodeMonkeys beat many full agent harnesses at far lower cost by freezing control flow and scaling parallel samples with mechanical verifiers (reproduction tests, majority vote, test-voting). For DevAgent's single-dispatch ticket→PR flow, the stealable middle is: one cheap parallel sample burst + mechanical selection before escalating to a full agent loop.
- **Termination doctrine converges on degraded success**: SWE-agent's autosubmit-on-every-error (typed exit status + partial `git diff`) and OpenHands' "unparseable verdict ⇒ keep working" both refuse to throw away partial work. DevAgent's wall-timeout burn episodes (memory: 60-min timeouts discarding streamed worker output) are the anti-pattern.
- **Cost-as-budget beat steps-as-budget everywhere** ($3/task default; CodeMonkeys' stage-level cost table; leaderboard cost-normalized views). Per-stage cost attribution (OpenHands `usage_id`, CodeMonkeys stage table) is table stakes.
- **Benchmarks are being harness-neutralized**: SWE-bench's default Verified view now fixes the scaffold (mini-SWE-agent, bash-only) to expose model capability — meaning scaffold-dependent moats are increasingly discounted, and "our harness adds X points" claims need the bash-only delta as a control.
- **Surprise**: the two "advanced" multi-model review mechanisms with public negative results are the reviewer-on-submit LLM (SWE-agent: sometimes rejects correct patches) and demonstration prompts across model families — while the boring mechanisms (edit-time lint + revert, test-execution-on-unpatched-repo, AST-normalized majority vote) reproduce across four independent systems.
