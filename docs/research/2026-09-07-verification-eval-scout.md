# Verification, Evals, and Sandboxing — Scout Report for DevAgent Gates G1–G4

Date: 2026-09-07
Scope: Survey of verification benchmarks (SWE-bench, Terminal-Bench), capability/time-horizon measurement (METR), LLM-as-judge pitfalls, inference-scaling verification, migration/DB-safety tooling (Squawk, pgroll, Atlas, gh-ost), and sandboxing/agent-security (sandbox-runtime, gVisor, Firecracker, OWASP LLM Top 10) — mapped onto DevAgent's validation layer (`src/validation/`: `test-gate.ts`, `migration-apply-gate.ts`, `migration-rules.ts`, `readiness-gate.ts`, `async-review.ts`, `stride-gate.ts`, `runner.ts`; `src/gates/stride.ts`) and `docs/PRODUCTION-READINESS.md` findings. Ideas are tagged by DevAgent surface: gates | orchestrator | workers | context | lessons | selfbuild | merge | product.

## Orientation: what DevAgent has today

- **G1 test execution** runs the repo's own test suite in a Docker-Compose sandbox (PRD §Gate G1, `docs/PRD.md:354`). `test-gate.ts:8-9` itself admits "Docker-based sandbox arrives later" — today it is host-native `npm test`/`go test ./...` on worker-rewritten code (PRODUCTION-READINESS #3).
- **G2 migration application** boots a compose Postgres and applies migration up/down from a config file. CRITICAL: `migration-apply-gate.ts:56` loads config from the worker-written worktree and runs it via `sh -c` on the host (PRODUCTION-READINESS #1 calls this a host-RCE hole).
- **G3 static migration analysis** is a linter-rules pass (`migration-rules.ts`); PRD:383 already names Squawk / Atlas analyzers as the intended corpus, but integration is a vendor-neutral placeholder today.
- **G4 async review** (`async-review.ts`) is layered: deterministic lints first, then an LLM hypothesis pass, findings blocking on `high` severity (PRD:1023).
- **STRIDE gate** (`stride-gate.ts`, `gates/stride.ts`) — regex-based static diff review, no LLM, no network.
- **Regression oracle** exists as gate G6 (`runRegressionOracle` in `src/consume.ts`) on single PR branches before autoMerge.

## 1. Benchmarks as oracle design: SWE-bench Verified

SWE-bench evaluates an agent's edit by running two test sets: `FAIL_TO_PASS` (tests that must newly pass) and `PASS_TO_PASS` (tests that must not break) — both required (https://openai.com/index/introducing-swe-bench-verified/). OpenAI's human-validated subset found three systematic failure modes of automated oracles: (1) unit tests that are **overly specific or unrelated to the issue**, rejecting correct solutions; (2) **underspecified issue descriptions**; (3) **environment-setup flakiness** grading valid solutions wrong (same URL). SWE-bench ships Dockerized evaluation for reproducibility (https://www.swebench.com/), and mini-SWE-agent reaches 65% Verified in ~100 lines of Python (https://github.com/SWE-agent/mini-swe-agent), showing scaffold minimality does not cap scores.

**Adopt (gates, merge):** G1 currently answers one binary question ("suite green"). A SWE-bench-shaped gate contract — per-run reporting of (a) ticket-implied FAIL_TO_PASS checks (derived from acceptance criteria / spec gate) and (b) PASS_TO_PASS = full suite minus flaky list, each reported separately — turns G1 from a smoke signal into a two-sided oracle. Classifying "env-setup failure" (gate infra failure) as distinct from "solution failure" prevents the false-green/false-red classes OpenAI documented; DevAgent's skip semantics already distinguish infra failure, so this is a reporting split, not new machinery (gates). The regression oracle should label whether a PR failure is unresolved vs resolved-but-broke-something (merge).

## 2. Terminal-Bench: verifier-scripts + oracle solutions for terminal tasks

Terminal-Bench (laude-institute, ICLR 2026) pairs every task with (a) an instruction, (b) a **test script verifying completion by inspecting the resulting environment**, and (c) a **reference ("oracle") solution that must pass the same test script** (https://github.com/laude-institute/terminal-bench). The harness runs agents in a Docker-sandboxed terminal (`tb run`) and task templates ship `docker-compose.yaml` + `run-tests.sh` + judge scripts (same repo). Harbor is the successor harness for Terminal-Bench 2.0 (https://github.com/laude-institute/harbor).

**Adopt (gates, product):** G2's up/down/boot check is DevAgent's closest analog to a Terminal-Bench task, but the *verification script lives outside the agent's write path* in Terminal-Bench while DevAgent's G2 config lives inside the worker-written worktree (`migration-apply-gate.ts:56`). Idea: a per-repo `devagent-verifications/` directory loaded **only from the trusted main checkout** (the PRODUCTION-READINESS #1 fix) containing small environment-inspection verifier scripts (does the column exist post-up? does seed data survive down→up?) — an oracle any agent can satisfy but none can author (gates). Second idea: store an oracle "golden" migration per known migration-class ticket family so G2 can diff worker-migration behavior vs oracle-migration behavior against the seeded snapshot — a shadow-migration oracle (gates, product).

## 3. METR time horizons: size the gate budget to measured task length

METR's time-horizon metric — the length of human-task an agent completes at 50% reliability — doubled every ~7 months through 2025 (https://metr.org/blog/2025-03-19-measuring-ai-ability-to-complete-long-tasks/). The January 2026 **Time Horizon 1.1** update re-estimated on 228 tasks (up from 170), doubled the count of 8h+ tasks to 31, and re-estimated post-2023 doubling at 131 days (TH1.1) vs 165 days (TH1); estimates proved **sensitive to task-suite composition** (GPT-4 estimates shifted down 35–57%, GPT-5/Opus 4.5 up 55%/11%) (https://metr.org/blog/2026-1-29-time-horizon-1-1/). METR also runs monitorability evals — can monitors catch agents doing "side tasks" (https://metr.org/blog/2026-01-19-early-work-on-monitorability-evaluations/).

**Adopt (orchestrator, selfbuild):** record per-iteration **effective time horizon** in the ledger: for each merged PR, log estimated human-hours (from ticket size/PRD classification) and gate pass/fail. Over a hundred iterations this yields DevAgent's own TH curve, telling the selfbuild loop when a backlog category (e.g. migrations) has crossed the reliability threshold for unattended autonomous operation vs still needing interactive-mode approval (selfbuild, product).
**Adopt (gates, lessons):** METR's monitorability framing maps onto the STRIDE gate. Treat every gate as a monitor and log, per violation-class, whether the agent's next revision moved the pattern (evasion). A monitor-vs-agent evasion ledger turns the lessons digest from prose into measured monitor coverage (lessons, gates).

## 4. LLM-as-judge pitfalls (MT-Bench, arXiv:2306.05685)

MT-Bench established that LLM judges are workable but carry three measured biases: **position bias** (answer order sways verdicts), **verbosity bias** (longer answers score higher), and **self-enhancement bias** (models favor their own outputs); agreement with humans is highest on math/reasoning and much lower on open-ended tasks (https://arxiv.org/abs/2306.05685).

**Adopt (gates, lessons):** DevAgent uses LLM judgment in G4's hypothesis pass and the curator/lessons pipeline. Adopt MT-Bench's mitigations concretely: (1) **never let a single LLM verdict block** — a `high` G4 finding should require either a deterministic confirmation (`go test -race`, lint, runtime repro) or a second independent judge with swapped answer order; (2) score against fixed per-item rubrics rather than comparative judgments; (3) log judge-model + position so verdicts are auditable in the run JSONL (gates). In the curator, guard against self-enhancement by judging lessons with a different model family than the one that wrote them (lessons).

## 5. Inference-scaling verification: repeated sampling + verifier (arXiv:2407.21787)

*Large Language Monkeys* shows repeated sampling scales pass@k massively, but **coverage without a verifier does not become solved-rate** — the bottleneck moves to selecting the correct sample; verifier-free selection (self-consistency voting) captures much of the gain (https://arxiv.org/abs/2407.21787).

**Adopt (orchestrator, gates):** DevAgent already has fan-out (N workers) and a merged-result oracle. The LLM-monkeys result justifies treating **the gates themselves as the verifier**: rank fan-out candidates by gate-score (G1 pass counts, G3 violation count, G4 finding count, regression-oracle result) rather than first-to-green or judge preference. Add a "sample-rank" stage after fan-out that runs the existing gates on each candidate and picks max-verifier-score — this converts compute already spent into solved-rate with no new models (orchestrator). Where gates can't decide (all green), fall back to self-consistency over the N diffs rather than an LLM judge alone (orchestrator, gates).

## 6. Migration safety I — Squawk (static lock/downtime linting)

Squawk is a Rust Postgres-migration linter that flags exactly the downtime classes: `require-concurrent-index-creation` (plain `CREATE INDEX` blocks writes), `constraint-missing-not-valid` (`ADD CONSTRAINT` table-scan lock → `NOT VALID` + later `VALIDATE`), `disallowed-unique-constraint` (ACCESS EXCLUSIVE lock), `prefer-text-field` (varchar resize takes ACCESS EXCLUSIVE), `prefer-identity`/`prefer-bigint-over-int`; config via `.squawk.toml`, `--pg-version` targeting, JSON/GitHub-annotation reporters, Docker image, npm/pip distribution (https://github.com/sbdchd/squawk).

**Adopt (gates):** back (or replace) G3's `migration-rules.ts` regex corpus by executing `squawk` as a subprocess gate: single static binary, nonzero exit on violations, machine-readable output, and its rule set *is* the PRD:383 dangerous-pattern taxonomy. Map Squawk severities → DevAgent severity (`critical` blocks PR, `warning` annotates the PR body). Cheap, deterministic, no network (gates). Adopt its GitHub-annotation reporter idea for gate-findings UX (product).

## 7. Migration safety II — pgroll: expand/contract as a gate-verifiable structure

pgroll performs zero-downtime, reversible Postgres migrations by serving **multiple schema versions simultaneously** via views: additive ("expand") changes first, backfill with triggers propagating writes both ways, client cutover via `search_path`, `complete` removes the old version, instant `rollback` at any point; `validate` and `analyze` subcommands; works on RDS/Aurora, PG ≥14 (https://github.com/xataio/pgroll).

**Adopt (gates, product):** (1) **verified reversibility as a G2 requirement** — G2 already demands down-migration; strengthen to up → verify → down → verify → up again, asserting data equality via checksum on the seeded snapshot after the full cycle (a pgroll-rollback-shaped guarantee without adopting pgroll) (gates). (2) **dual-version consistency check**: for breaking-change migrations, spin the seeded DB through the expand phase and run the repo's *existing* (pre-migration) test suite against both old and new schema versions — directly inspired by pgroll's two-versions-at-once model (gates, product). If a repo adopts pgroll itself, G2 gains `pgroll validate` as a free pre-check (gates).

## 8. Migration safety III — Atlas: declarative schema-as-code + lint-on-replay

Atlas (https://github.com/ariga/atlas; package home https://ariga.io/atlas) provides declarative schema migrations (desired-state `.hcl`/ORM-diff → generated plan), a `migrate lint` that **replays migrations against a dev database to detect destructive/data-dependent/incompatible changes** (the `atlasexec` Go client exists for embedding, with `atlas_security.go` in-tree), and an `atlas.sum` integrity file that detects edited or partially applied migration history (the repo's `broken/…atlas.sum` testdata demonstrates tamper detection).

**Adopt (gates, merge):** (1) wire `atlas migrate lint --dev-url` into G3 for SQL repos — replay-based analysis catches runtime-destructive statements that static regexes miss (G3's stated intent, PRD:383) (gates). (2) Adopt the **atlas.sum pattern** natively: hash the migration directory at dispatch and re-verify at G2 apply-time; a mismatch means the worker edited history mid-run — fail closed rather than applying drifted SQL (gates). The merge path also gains tamper evidence on the PR branch (merge).

## 9. Migration safety IV — gh-ost / pt-OSC: throttled cutover for long-running migrations

gh-ost is GitHub's triggerless online schema-change tool for MySQL: it copies the table while streaming binlog deltas, **throttles on replica lag/load**, supports **testing on a replica first** (`--test-on-replica`), a `what-if` dry-run mode, hooks for orchestration, and explicit cut-over control (https://github.com/github/gh-ost; doc/: `cut-over.md`, `testing-on-replica.md`, `throttle.md`, `what-if.md`).

**Adopt (gates, orchestrator):** G2 currently assumes migrations complete within gate-budget time. For big-migration tickets: (a) **dry-run gate** (`what-if` analog): run migrationUp against a scaled-up seeded-snapshot clone and measure lock-time/row-estimates before allowing the real gate (gates); (b) **throttle-aware timeout**: emit progress heartbeats (rows migrated / ETA) and let the orchestrator's watchdog treat stall-vs-progress differently — DevAgent already has herdr no-progress watchdog machinery to reuse (orchestrator, gates); (c) "test on replica" analog = run the migration against a **shadow DB** built from production-shaped dumps, never on the gate's only seeded instance, keeping the seed pristine for the down-check (gates).

## 10. Shadow-DB / schema-diff verification ladder (synthesis of 6–9)

Combining Squawk (static), Atlas (replay lint), pgroll (dual-version), gh-ost (shadow+throttle): the mature industry stack verifies migrations on **four independent axes** — static lint, replay against a dev DB, dual-version serving, shadow execution. DevAgent's G2/G3 should expose this as a severity ladder: static lint (always) → replay lint (SQL repos) → seeded up/down/boot (migration tickets) → shadow-DB run with production-shaped data (flagged high-risk diffs). One more cheap deterministic oracle: after up, dump `information_schema` and diff against the ORM-generated or declared desired schema — mismatches block (gates). This axis-checklist is also the upgrade path shape for `readiness-gate.ts` (gates).

## 11. Sandboxing — sandbox-runtime: OS-level FS+network fences without containers

Anthropic's `sandbox-runtime` wraps any command with **filesystem and network restrictions enforced at OS level, no container required**: per-platform backends (macOS Seatbelt-style, Linux seccomp/bubblewrap with a generated filter), an **HTTP(S) MITM proxy** enforcing domain allowlists (tls-terminate-proxy, request-filter), credential masking of env/files, a violation monitor and violation store, and an `srt` CLI (`srt npm install`) (https://github.com/anthropics/sandbox-runtime).

**Adopt (gates, workers):** the fastest fix for PRODUCTION-READINESS #1–#3 because it is a drop-in wrapper, not an infra migration: (1) run **every gate subprocess** (`sh -c` in migration-apply-gate, test-gate suites, compose) under `srt` with a config generated from DevAgent's own policy — read/write confined to worktree + gate dirs, network restricted to the DB container and package registry; (2) reuse its **credential masking** approach for PRODUCTION-READINESS #2 (env allowlist + masking) (workers); (3) the violation store is a ready-made lessons source: log sandbox denials per run and feed repeated denials into the lessons digest — workers probing the fence get flagged (lessons, gates). macOS parity matters: DevAgent runs on the operator's macOS host today, where gVisor/Firecracker are unavailable but sandbox-runtime ships a macOS backend (gates).

## 12. Sandboxing — gVisor and Firecracker as the container-hardening tier

gVisor is a userspace application kernel (`runsc` OCI runtime) that intercepts syscalls to minimize container-escape risk, integrates with plain Docker (`docker run --runtime=runsc`), and sits between seccomp-style filters and VMs (https://gvisor.dev/docs/, https://gvisor.dev/docs/user_guide/quick_start/docker/). Firecracker provides KVM microVMs with ~125 ms boot, <5 MiB overhead, rate limiters, and a `jailer` second line of defense; explicitly used for AI-agent sandboxes (E2B) (https://firecracker-microvm.github.io/).

**Adopt (gates, product):** stage the sandbox ladder by risk class: L0 host (today) → L1 sandbox-runtime wrapper (idea 11) → L2 gVisor runtime for the G1/G2 compose stack (a one-line daemon config change for Linux operators; docker-compose workflows untouched) → L3 Firecracker microVM per run for hosted/multi-tenant deployments (product). The PRODUCTION-READINESS compose-policy check (reject `privileged`, `host` network, docker.sock binds; enforce mem/cpus via an override file DevAgent owns) is the immediate slice and is compatible with all tiers (gates). Pin images: the compose file being repo-controlled with no image pinning is an unsandboxed supply-chain risk aligned with OWASP LLM03 (gates).

## 13. Agent security — OWASP LLM Top 10 as the STRIDE gate's rubric skeleton

OWASP's 2025 Top 10 for LLM apps: LLM01 Prompt Injection, LLM02 Sensitive Information Disclosure, LLM03 Supply Chain, LLM10 Unbounded Consumption, plus an Agentic Security Initiative (https://genai.owasp.org/llm-top-10/). DevAgent's threat model already names prompt injection via tickets (R5) and exfiltration via inherited env (PRODUCTION-READINESS #2). Unbounded Consumption (LLM10) maps to existing budget caps; Sensitive Information Disclosure maps to the shallow logger-redaction gap (PRODUCTION-READINESS #14).

**Adopt (gates, product):** restructure the STRIDE gate's regex rubric around the OWASP taxonomy so each finding cites a category (LLM01: ticket-instructed credential commands visible in the diff; LLM02: secret-shaped strings added; LLM03: new unpinned dependency or curl|bash install; LLM10: missing timeout/limit in added long-running code). This gives the gate a maintained external rubric and makes severity mapping defensible (gates). Pair with sandbox-runtime's network allowlist (idea 11) so LLM01/LLM02 findings are *enforced*, not just flagged (gates, workers).

## Ranked adoption order (impact × tractability)

1. Trusted-checkout-only G2 config + sandbox-runtime wrapper on gate subprocesses (ideas 2, 11) — closes the P0 RCE.
2. Squawk + `atlas migrate lint` as G3 engines with atlas.sum-style history hashing (ideas 6, 8) — deterministic, matches PRD intent.
3. FAIL_TO_PASS / PASS_TO_PASS / infra-failure split in G1 and the regression oracle (idea 1).
4. Verifier-scored fan-out ranking (idea 5) — pure orchestration, no new infrastructure.
5. Verified reversibility cycle + schema-diff oracle in G2 (ideas 7, 10).
6. LLM-judge hygiene in G4/curator: swap-order second judge, rubric scoring, cross-family judging (idea 4).
7. Ledger time-horizon metric and monitor-evasion tracking (idea 3).
8. Sandbox ladder L2/L3 for Linux deployments (idea 12); OWASP-mapped STRIDE rubric (idea 13).

## Sources

- https://www.swebench.com/ (SWE-bench leaderboards, Dockerized eval, mini-SWE-agent)
- https://openai.com/index/introducing-swe-bench-verified/ (SWE-bench Verified: FAIL_TO_PASS/PASS_TO_PASS, three oracle failure modes)
- https://github.com/SWE-agent/mini-swe-agent (mini-SWE-agent 65% Verified)
- https://github.com/laude-institute/terminal-bench (Terminal-Bench task format: instruction + test script + oracle solution)
- https://github.com/laude-institute/harbor (Terminal-Bench 2.0 harness)
- https://metr.org/blog/2025-03-19-measuring-ai-ability-to-complete-long-tasks/ (time-horizon metric, 7-month doubling)
- https://metr.org/blog/2026-1-29-time-horizon-1-1/ (TH1.1: 228 tasks, 131-day post-2023 doubling, task-composition sensitivity)
- https://metr.org/blog/2026-01-19-early-work-on-monitorability-evaluations/ (monitorability evals: agents vs side-task monitors)
- https://arxiv.org/abs/2306.05685 (MT-Bench LLM-as-judge: position/verbosity/self-enhancement bias)
- https://arxiv.org/abs/2407.21787 (Large Language Monkeys: repeated sampling, verifier bottleneck)
- https://github.com/sbdchd/squawk (Squawk Postgres migration linter, rules and reporters)
- https://github.com/xataio/pgroll (pgroll zero-downtime dual-schema-version migrations)
- https://github.com/ariga/atlas and https://ariga.io/atlas (Atlas declarative migrations, migrate lint replay, atlas.sum integrity)
- https://github.com/github/gh-ost (gh-ost online schema change: throttle, test-on-replica, what-if)
- https://github.com/anthropics/sandbox-runtime (OS-level FS/network sandbox, MITM proxy, credential masking, violation store)
- https://gvisor.dev/docs/ and https://gvisor.dev/docs/user_guide/quick_start/docker/ (gVisor runsc userspace kernel)
- https://firecracker-microvm.github.io/ (Firecracker microVMs, 125 ms boot, jailer, E2B agent-sandbox usage)
- https://genai.owasp.org/llm-top-10/ (OWASP Top 10 for LLM Apps 2025 + Agentic Security Initiative)
