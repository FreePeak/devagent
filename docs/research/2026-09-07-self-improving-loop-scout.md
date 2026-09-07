# Self-Improving Loops and Experience-Driven Agents: adoption survey for DevAgent's selfbuild loop and lessons system

Date: 2026-09-07
Scope: self-improving / experience-driven agent research (Voyager, Reflexion, Self-Refine, ACE, AlphaEvolve + OpenEvolve, Mem0 + MemGPT, Claude Skills, Anthropic long-horizon harness post, GEPA, SWE-Gym, SWE-smith, and a 2025-2026 frontier scan) mapped onto DevAgent's selfbuild loop, lessons digest + eval guard, curator, gates, and worker dispatch. Does not duplicate PRD §19.

## What DevAgent already has (orientation)

From docs/SELF-BUILD-LOOP.md, src/lessons/guard.ts, src/curator/audit.ts:

- **Loop**: research → ideas → validate → plan → implement → test → push; append-only `.selfbuild/ledger.jsonl` (`{loop, ts, goal, status, duration_s}`); issue-first tracker pick (GitHub issues labeled `selfbuild`, priority-sorted); PRD as a state document; curator (`scripts/prd-curator.sh`) closes shipped issues, files ≤3/pass, strikes shipped PRD lines.
- **Guardrails**: circuit breaker (3 consecutive failures), starvation gate, lessons ratchet (append-only `.selfbuild/lessons.md`), failure feedback (prior ledger tail injected into the research prompt).
- **Lessons eval guard** (src/lessons/guard.ts): trigram-Jaccard dedupe at 0.8 before append (motivated by the lessons-eval-guard lesson repeating 7× and burning the 4000-char digest budget); evaluate→accept — the repo suite runs against the PROPOSED lessons-file state, green keeps, red reverts; `lessons-eval` ledger rows (excerpt hash, similarity, predictedImpact, suite result); `loop-result` rows joined by loop number for impact telemetry (Q39); 20% held-out slice; must-beat-best-so-far acceptance; caps 40 lines / 4000 chars.
- **Curator audit** (src/curator/audit.ts): advisory-only PRD-coverage scan (unqueued / stale ≥14d), never enqueues (Q15 coupling rationale).

The gaps this research targets: lessons are prose bullets with weak provenance and no lifecycle beyond append; prompt/dispatch templates are static; goal selection is deterministic sorting with an LLM fallback; failed iterations are summarized into the next research prompt but not structured per-issue; the digest is a flat always-on injection.

## ACE: evolving playbooks, not summaries (arXiv:2510.04618)

The closest match to DevAgent's lessons system. ACE treats contexts as **evolving playbooks** maintained by three roles — Generator (produces rollouts), Reflector (extracts lessons from execution feedback), Curator (applies **structured incremental delta updates** merged deterministically, without an LLM rewriting the whole context).

Evidence:

- **Brevity bias + context collapse are the failure modes of monolithic rewriting**: in their AppWorld case study, a rewritten context collapsed from 18,282 tokens (66.7 accuracy) to 122 tokens at the next step, dropping accuracy to 57.1 — worse than the 63.7 no-adaptation baseline. DevAgent's append-only ratchet is structurally immune to collapse but its 4000-char digest cap induces a different compression pressure: old-but-distinct lessons get truncated wholesale.
- **Results**: +10.6% agents, +8.6% finance across offline/online adaptation; ReAct+ACE 76.2 on AppWorld (+12.5 over ReAct); **works without ground-truth labels** when reliable execution feedback exists (+14.8% avg in the label-free agent setting) — code-execution success/failure substitutes for labels.
- **Cost**: vs GEPA's prompt-validation loop, ACE cut adaptation latency 82.3% and rollouts 75.1% (offline AppWorld); vs Dynamic Cheatsheet, 91.5% latency and 83.6% token cost (online FiNER). KV-cache reuse makes the longer playbook cheap: 91.8% of input tokens served from cache, 82.6% billed-cost reduction.
- **Limitation**: without reliable feedback signals both ACE and Dynamic Cheatsheet degrade (finance online without labels: −3.4). Directly validates DevAgent's guardrail: judge lessons only on deterministic oracles.

Adoption for DevAgent (surface: lessons, selfbuild):

1. **Delta-op schema for lessons**: upgrade `appendLessonGuarded` from add-only to ACE ops — `ADD` (new bullet, evidence-ref), `UPDATE` (revise an existing entry by hash with a new observation; replaces today's near-duplicate rejection, which currently throws away real refinements at 0.8 similarity), `TAG`/`STRIKE` (mark superseded when the ledger shows the underlying issue shipped). The deterministic non-LLM merge step is exactly what guard.ts already does for dedupe; extend it to apply ops against the markdown file.
2. **Reflector input contract**: define the Reflector's legal inputs as gate results, suite outcome, ledger row status, and PR diff — never operator chat or quoted errors inside agent streams (the loop-90 po-aborted incident showed quoted historical errors reading as live). ACE's no-signal degradation is the empirical backing.
3. **Digest = playbook projection**: keep the ratchet file as the comprehensive playbook and make `loadLessonsDigest` a relevance-projected slice (top-k by impact score + recency per task category) instead of newest-40-lines.

## Voyager: automatic curriculum + skill library (arXiv:2305.16291)

Lifelong-learning agent with three components: (1) an **automatic curriculum** — GPT-4 proposes the next task from the agent's current state plus completed/failed task history, biased toward diversity and the capability frontier; (2) a **skill library** — every verified skill is executable code stored in a vector DB, indexed by the embedding of its description, retrieved for related new tasks; (3) iterative prompting with environment feedback. Evidence: 3.3× more unique items, tech-tree milestones up to 15.3× faster, 2.3× longer travel than prior SOTA; the learned skill library **transfers zero-shot to a new world**.

Adoption (surface: workers, context, selfbuild):

4. **Skill library for dispatch recipes**: DevAgent's operational lessons (omp `--no-prewalk`/`--no-lsp` flags, provider-qualified model ids, orphan-pane sweep procedure, stale-dist pitfalls) are exactly Voyager skills — currently stored as prose and re-derived. Store each as a structured recipe (trigger conditions, command/template, evidence) in an embedding-indexed library; dispatch retrieves top-k by task similarity and injects only matching recipes. Replaces the flat 4000-char digest for operational content.
5. **Curriculum-shaped goal selection**: the issue-first pick is priority+age; Voyager suggests a second signal — capability-frontier bias: prefer the oldest issue whose required capability the ledger shows a recent failed attempt nearly reached (failure feedback already in the ledger tail). Cheap heuristic on existing data.

## Reflexion: verbal RL across episodes (arXiv:2303.11366)

Converts binary/scalar environment feedback into a **textual self-reflection** stored in episodic memory and injected into the next attempt of the same task. Actor/Evaluator/Self-reflection/Memory loop, no weight updates. Evidence: 91.0 pass@1 HumanEval vs 80.1 GPT-4 base; Leetcode-Hard 7.5→15.0; ablations show the reflection (not just another retry) is the active ingredient.

Adoption (surface: selfbuild):

6. **Per-issue episodic memory**: today a failed iteration's diagnostics go into the next research prompt as a ledger-tail summary — lossy and issue-agnostic. Keep a structured reflection per tracker issue (what failed, which gate, root-cause hypothesis, what to avoid) written at failure-recording time, injected verbatim when that issue is re-claimed. This survives across runs (mirror with the ledger in selfbuild-state.sh) and turns the re-pick path (already-shipped heuristics, Q27 re-burn guard) into the Reflexion loop.

## Self-Refine: feedback quality is the multiplier (arXiv:2303.17651)

Same LLM alternates FEEDBACK and REFINE on its own output until a stop condition. Evidence: 5–40% absolute gains across 7 tasks; the analysis section is the useful part — **generic feedback ≈ no feedback** (Sentiment Reversal 43.2 → 31.2 generic → 0 without), and on Math Reasoning gains were near zero because the model could not localize its own errors (\\\"everything looks good\\\" on 94% of instances).

Adoption (surface: lessons): the lessons evaluate-step suite run is a FEEDBACK signal, but a green suite does not localize *why* the lesson was good. Require a candidate lesson to carry a `predictedImpact` mechanism claim (\\\"prevents X by Y\\\") that the accept-path records with the suite result — mirroring Self-Refine's finding that actionable beats generic. guard.ts already stores predictedImpact; make its presence meaningful (grade it) rather than a string field.

## AlphaEvolve and OpenEvolve: evolutionary databases of programs (DeepMind blog; openevolve)

AlphaEvolve: an evolutionary coding agent — a **prompt sampler assembles prompts from a programs database** (MAP-Elites-style archive scored by automated evaluators), Gemini Flash for breadth + Gemini Pro for depth, candidates verified by automated metrics and archived. Evidence: Borg scheduling heuristic recovering 0.7% of Google's total compute (in production over a year); 23% Gemini training-kernel speedup → 1% training-time reduction; 32.5% FlashAttention speedup; 4×4 complex matmul in 48 multiplications (beating Strassen's 49); on 50+ open math problems: ~75% rediscovered SOTA, 20% improved best-known.

OpenEvolve (codelion/openevolve, ~7.3k stars): the open implementation — MAP-Elites + island populations, LLM ensembles, an artifact side-channel feeding error output back into the next generation, cascade evaluation to filter bad programs cheaply before full evaluation.

Adoption (surface: selfbuild):

7. **Goal database as an evolution archive**: DevAgent's iteration already has an automated evaluator (G1–G4 gates + repo suite + ledger status). Keep a MAP-Elites-style archive of candidate goals per category (the ledger + curator-filed issues), score by evaluator outcome, and prefer candidates from under-explored categories — the islands prevent the observed single-cluster thrash. Cascade evaluation ≈ the validate phase: reject goals that fail the cheap checks (open dependency, already-shipped) before spending a worker run.

## Mem0 and MemGPT: memory operations and paging (arXiv:2504.19413; arXiv:2310.08560)

Mem0: two-phase pipeline — an extraction phase produces candidate facts from new messages, an update phase compares each candidate against similar existing memories and picks an operation (**ADD / UPDATE / DELETE / NOOP**) via a tool call; a graph variant (Mem0-g) adds entity-relation extraction with conflict detection on Neo4j. Evaluated on LOCOMO: ~26% accuracy gain over OpenAI's memory, ~91% lower p95 latency vs full-context, >90% token savings. MemGPT: OS-inspired **virtual context management** — main context vs external storage, self-editing memory via function calls, paging in what is needed.

Adoption (surface: lessons, context):

8. **Memory-op taxonomy for the guard**: guard.ts's accept/reject is a two-op system; Mem0's ADD/UPDATE/DELETE/NOOP is the industry-standard four-op set and maps cleanly onto the ACE delta scheme above. The DELETE op (strike superseded lessons when the curator closes the underlying issue) is the one DevAgent is missing — today stale lessons age out only via the 40-line window.
9. **Paging for LeanKG context**: MemGPT justifies the existing `ctx_read` compression and FR-CTX digest work — treat LeanKG subgraphs as external memory paged in on demand rather than preloaded, keeping worker startup under the measured 60–487s LSP/MCP-dispatch stall ceiling.

## Claude Skills + the long-horizon harness post (Anthropic)

**Skills** (claude.com/blog/skills): folders with a SKILL.md plus scripts and resources, scanned for relevance and loaded on demand (progressive disclosure); composable, portable across Claude apps / Claude Code / API; published as an open standard (agentskills.io, Dec 18 2025) with org-wide management and a `/v1/skills` API for versioning.

**Effective harnesses for long-running agents** (Nov 26, 2025): for work spanning many context windows, a plain agent loop fails in two predictable ways — one-shotting too much and declaring done prematurely. Fixes that worked: an **initializer agent** writing an environment scaffold + a **feature-list JSON** with a `passes` field per feature (JSON chosen because models are less likely to overwrite it than Markdown; strongly-worded rules like \\\"it is unacceptable to remove or edit tests\\\"); subsequent sessions make **incremental progress on one feature** and must commit + update a progress file; end-to-end testing via browser automation tools dramatically improved verification.

Adoption (surface: orchestrator, lessons, product):

10. **Feature-checklist artifact in devagent task**: emit a JSON feature checklist (from the plan phase) that the worker can only flip `passes` with gate evidence; the test gate rejects a PR with flipped-but-unverified entries. This is the executable version of DevAgent's G-gates and directly attacks the premature-done class the post documents.
11. **Lessons as skills**: convert the lessons digest's operational entries into actual skill folders (surface: product) — portable to both claude-code and opencode workers via the open standard, versioned, loaded on demand rather than spending the always-on 4000-char budget.

## GEPA: reflective prompt evolution (arXiv:2507.19457)

GEPA (Genetic-Pareto) evolves every module prompt of a compound system using natural-language reflection over rollout traces, keeping a **Pareto frontier** of candidates (per-instance best scores) rather than a single incumbent. Evidence: beats GRPO (24,000 rollouts) and MIPROv2 on HotpotQA/HoVer/IFBench/PUPA with ~35× fewer rollouts on average; ACE (which compares against it directly) shows GEPA's full-rewrite loop is the expensive part — ACE's delta updates cut the same adaptation's latency 82.3%.

Adoption (surface: orchestrator, selfbuild):

12. **Dispatch-prompt evolution**: the dispatch prompt (goal + policy riders + lessons digest) is a module prompt that is currently hand-frozen. Maintain a small Pareto set of template variants, score each by the deterministic gate outcomes of the runs that used it (join via ledger), and mutate the worst-performing variant using the run's failure trace. GEPA's sample-efficiency argument is what makes this fit DevAgent's per-run budget caps; ACE's delta-update caveat says mutate by patching rider sections, never rewriting the template.

## SWE-Gym and SWE-smith: self-training flywheels from own trajectories (arXiv:2412.21139; arXiv:2504.21798)

SWE-Gym: 2,438 real Python tasks (11 repos) with executable environments and validated tests; training a 32B model on agent trajectories yields up to 19% absolute resolve-rate gains, scaling without saturation at 491 trajectories; outcome-supervised **verifiers** enable test-time best-of-n selection. SWE-smith: the data-generation inversion — **synthesize bugs into existing repos** (LM rewrites, AST mutations, removing definitions) to break existing tests, producing 50k instances across 128 repos and SWE-agent-LM-32B at 40.2% SWE-bench Verified (SOTA for open weights); synthetic issue text ≈ real issue text.

Adoption (surface: gates, merge, lessons):

13. **Gate calibration corpus from synthetic regressions**: generate known-bad patches (inserted bug classes SWE-smith-style) into devagent's own test corpus and require G1–G4 and the regression oracle (including the unshipped PRD:885 board-level merged-result oracle) to catch them — the verifier-training insight applied to DevAgent's own gates: label gate verdicts against ground truth and measure FP/FN per gate.
14. **Trajectory library**: passing trajectories per task category become few-shot context for future workers (SWE-Gym's training signal, in-context instead of fine-tuned) — pairs with the Voyager skill library as its example-bearing sibling.

## 2025-2026 frontier scan

- **EVOMAL: Self-Poisoning in Self-Evolving Coding Agents** (arXiv:2608.25776): agents that write tools by imitating retrieved skills can be driven to author, store, and execute a malicious skill — the retrieved skill becomes the template for the new one. DevAgent's ratchet + dedupe gate is a partial defense, but the correct posture: lessons stay prose (never executable), recipes carry provenance (evidence URL + committing loop id), and any future skill-execution feature must sandbox and review-by-default. Adopt provenance fields now.
- **MASkills: Continual Skills Optimization for Multi-Agent LLM Systems** (arXiv:2609.02094, EMNLP 2026 Findings): skills as the actionable unit (when to act, how, which tools) outperform raw experience memories for multi-agent continual improvement — relevant to factory mode (scout + workers + curator as distinct agents sharing an experience store with per-agent skill views).
- Also observed in the same scan and worth tracking: CHIME (credit-aware hierarchical memory evolution, arXiv:2609.02074) and Skill Self-Play (co-evolving skills, arXiv:2607.22529).

## Idea index (by DevAgent surface)

| # | Idea | Surface | Anchored in |
|---|---|---|---|
| 1 | Delta-op lesson schema (ADD/UPDATE/TAG/STRIKE) with deterministic merge | lessons | ACE |
| 2 | Reflector input contract: deterministic oracles only | lessons, selfbuild | ACE, Self-Refine |
| 3 | Digest as relevance-projected playbook slice | context | ACE, MemGPT |
| 4 | Operational skill library, embedding-indexed, retrieved at dispatch | workers, context | Voyager, Claude Skills |
| 5 | Capability-frontier bias in goal pick | selfbuild | Voyager |
| 6 | Per-issue episodic failure memory, mirrored with ledger | selfbuild | Reflexion |
| 7 | predictedImpact mechanism-claim grading at accept time | lessons | Self-Refine |
| 8 | MAP-Elites goal archive + cascade validation | selfbuild | AlphaEvolve, OpenEvolve |
| 9 | Four-op guard taxonomy incl. DELETE on issue close | lessons | Mem0 |
| 10 | JSON feature-checklist artifact with evidence-gated `passes` | orchestrator, gates | Anthropic harness post |
| 11 | Lessons packaged as portable SKILL.md folders | lessons, product | Claude Skills |
| 12 | Pareto dispatch-prompt evolution scored by gate outcomes | orchestrator | GEPA |
| 13 | Synthetic-regression gate-calibration corpus + gate FP/FN telemetry | gates, merge | SWE-smith, SWE-Gym |
| 14 | Trajectory library as few-shot context per task category | workers | SWE-Gym |
| 15 | Lesson provenance fields + prose-only ratchet as self-poisoning defense | lessons, selfbuild | EVOMAL |

## Sources

- https://arxiv.org/abs/2305.16291 — Voyager: An Open-Ended Embodied Agent with Large Language Models
- https://huggingface.co/papers/2305.16291 — Voyager full text (skill library, curriculum details)
- https://arxiv.org/abs/2303.11366 — Reflexion: Language Agents with Verbal Reinforcement Learning
- https://huggingface.co/papers/2303.11366 — Reflexion full text (HumanEval/MBPP tables)
- https://arxiv.org/abs/2303.17651 — Self-Refine: Iterative Refinement with Self-Feedback
- https://huggingface.co/papers/2303.17651 — Self-Refine full text (feedback-quality ablations)
- https://arxiv.org/abs/2510.04618 — Agentic Context Engineering (ACE)
- https://arxiv.org/html/2510.04618v3 — ACE full text (collapse case study, cost tables, KV-cache study)
- https://deepmind.google/blog/alphaevolve-a-gemini-powered-coding-agent-for-designing-advanced-algorithms/ — AlphaEvolve announcement
- https://github.com/codelion/openevolve — OpenEvolve (MAP-Elites, islands, cascade evaluation)
- https://arxiv.org/abs/2504.19413 — Mem0: Building Production-Ready AI Agents with Scalable Long-Term Memory
- https://huggingface.co/papers/2504.19413 — Mem0 full text (two-phase pipeline, graph variant, LOCOMO setup)
- https://arxiv.org/abs/2310.08560 — MemGPT: Towards LLMs as Operating Systems
- https://claude.com/blog/skills — Introducing Agent Skills (open standard update Dec 18, 2025)
- https://www.anthropic.com/engineering/effective-harnesses-for-long-running-agents — Effective harnesses for long-running agents
- https://arxiv.org/abs/2507.19457 — GEPA: Reflective Prompt Evolution Can Outperform Reinforcement Learning
- https://huggingface.co/papers/2507.19457 — GEPA full text (Pareto selection, rollout budgets)
- https://arxiv.org/abs/2412.21139 — Training Software Engineering Agents and Verifiers with SWE-Gym
- https://huggingface.co/papers/2412.21139 — SWE-Gym full text (2,438 instances, verifier training)
- https://arxiv.org/abs/2504.21798 — SWE-smith: Scaling Data for Software Engineering Agents
- https://huggingface.co/papers/2504.21798 — SWE-smith full text (bug synthesis pipeline, 40.2% Verified)
- https://arxiv.org/abs/2608.25776 — EVOMAL: Self-Poisoning in Self-Evolving Coding Agents
- https://arxiv.org/abs/2609.02094 — MASkills: Continual Skills Optimization for Multi-Agent LLM Systems
- https://arxiv.org/abs/2609.02074 — CHIME: Credit-Aware Hierarchical Memory Evolution (scan)
- https://arxiv.org/abs/2607.22529 — Skill Self-Play: Pushing the Frontier of LLM Capability with Co-Evolving Skills (scan)
- Repo-internal: docs/SELF-BUILD-LOOP.md, src/lessons/guard.ts, src/curator/audit.ts
