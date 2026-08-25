# DevAgent Production Readiness Review

**Date:** 2026-08-25 · **Scope:** full src/ (~6k LOC, 43 modules), test/ (~4.5k LOC), CI, scripts, docs.
**Method:** four parallel deep-dives (architecture, reliability/concurrency, security/sandboxing, testing/release). All headline claims re-verified against source by hand.

## Verdict

DevAgent has an unusually honest core for an agent product: dependency-injected pipeline state machine, worktree-per-run isolation, evidence-gated trust in the orchestrator, and ~261 tests including real-git e2e. But it is **not production ready**, for three reasons:

1. **The validation layer can be turned into host RCE by the thing it validates.** Gate G2 executes shell commands loaded from a config file *inside the LLM-written worktree* (`migration-apply-gate.ts:56,86,95`).
2. **Every spawned worker inherits every secret in the environment** (`spawn-utils.ts:40-42`), and workers are prompt-fed with ticket content — the exfiltration path is one injected ticket away.
3. **Shipping is broken by construction**: `bin` points at a gitignored `dist/`, there is no `files`/`prepublishOnly`, no LICENSE, and four disagreeing version numbers.

Everything else is fixable engineering discipline. Ranked plan below.

---

## P0 — Blockers (fix before any real deployment)

### 1. G2 loads migration commands from the worker-written worktree and runs them via `sh -c`
- Evidence: `src/validation/migration-apply-gate.ts:56` calls `loadConfig(worktreePath)`; `:86` and `:95` run `g2.migrationUp` / `migrationDown` through `spawnCli('sh', ['-c', …])` on the host. A worker (or a prompt-injected ticket) that edits `devagent.json` in its own worktree achieves arbitrary host command execution during "validation" — with Docker access attached.
- Fix: load G2 config **only** from the trusted main checkout (the repo `cfg.repoPath`, never `worktreePath`); schema-validate strictly; treat absence of trusted config as skip (already the semantic); longer term, execute migrations inside the DB container instead of host shell.

### 2. Full host environment leaks to every child process
- Evidence: `src/workers/spawn-utils.ts:40-42` clones `process.env` minus 4 model-related vars. Every `claude`/`opencode` child, every gate subprocess, and docker-compose variable substitution see `LINEAR_API_KEY`, `GITHUB_TOKEN`, `JIRA_*`, `GITLAB_TOKEN`, webhook secrets. Same in `sessionguard/spawn.ts:22-25`.
- Fix: per-child env **allowlist** (PATH/HOME/SHELL/LANG + explicitly passed tokens); pass credentials as explicit parameters to integrations instead of ambient env; document that compose `${VAR}` substitution no longer sees secrets.

### 3. No sandbox boundary around LLM-authored code execution
- Evidence: G1 runs repo-native `npm test`/`go test ./...` on the host (`test-gate.ts:24-33,50`) — i.e., scripts the worker may have rewritten. Compose file is fully repo-controlled (`migration-apply-gate.ts:14-21`): no image pinning, no network policy, no mount restrictions, no CPU/mem caps anywhere in src/. PRD's claimed isolation ("workers confined to worktree cwd; Docker network isolation", `docs/PRD.md:483`) is aspirational; `test-gate.ts:8-9` admits "Docker-based sandbox arrives later".
- Fix (staged): (a) short term — strip env per #2, add a compose policy check before `up` (reject `privileged`, `network_mode: host`, docker.sock binds, host paths outside the worktree; enforce `mem_limit`/`cpus` via override file DevAgent owns); (b) medium term — containerize G1/G2 execution end-to-end; (c) publish an explicit trust-boundary doc so operators know what they're running.

### 4. `spawnCli` treats spawn failure as success
- Evidence: `src/workers/spawn-utils.ts:66-67` — comment says "normalize those to -1", code does `typeof rawCode === 'number' ? rawCode : 0`. ENOENT (missing binary) yields `exitCode: 0`. Consequences today: `dockerAvailable()` returns true when docker isn't installed (wrong skip semantics), gates can false-green, and the function is mocked in every test so nothing catches it.
- Fix: return `-1` for non-numeric codes; add the first direct unit test of `spawnCli`.

### 5. Publishing path is broken; licensing/version chaos
- Evidence: `bin: ./dist/cli.js` (`package.json:7`) but `dist/` gitignored (`.gitignore:2`) and no `files` field / `prepublishOnly` → `npm publish` ships without the CLI. No LICENSE ("TBD", README:61; npm default UNLICENSED). Versions: `package.json:3`=0.1.0, `cli.ts:16`=0.1.0, `mcp.ts:206`=0.3.0, README=0.3.0.
- Fix: `"files": ["dist"]`, `"prepublishOnly": "npm run build && npm test"` (+ shebang preserved in build), pick MIT/Apache-2.0, read version once from package.json everywhere, add CHANGELOG + tags.

### 6. Single-worker `--auto-pr` can open empty PRs
- Evidence: `publishStage` (`deps.ts:83-141`) pushes the branch directly; `commitAllChanges` is invoked **only** on the fan-out winner path (`deps.ts:164`). If the winning worker left uncommitted edits (typical agent behavior), branch HEAD == base HEAD → PR silently omits the entire change set.
- Fix: always `commitAllChanges` before `pushBranch` in publishStage; fail the publish if nothing to commit AND diff vs base is empty.

---

## P1 — Reliability (needed for unattended operation)

### 7. Run-lock registry races
- `runregistry.ts:31-44`: acquire is `existsSync → rmSync → writeFileSync` — TOCTOU lets two processes both acquire. `release()` (:49-53) deletes unconditionally — a slow run whose TTL expired releases its successor's live lock. TTL is fixed 60 min (`:29`) while worst-case runtime ≈ 3 h (`maxLoops 3 × 30-min timeout` + gates, `config.ts:25-26`). Stored `pid` is never checked.
- Fix: atomic create via `fs.open(path,'wx')`; release only after re-reading and matching own `{pid,startedAt}` token; heartbeat mtime touches + liveness probe `process.kill(pid,0)` before breaking stale locks; derive TTL from configured timeouts.

### 8. Serve lifecycle crashes and strands work
- No `server.on('error')` → EADDRINUSE = raw crash (`cli.ts:201`); no SIGTERM/SIGINT drain; zero `process.on('unhandledRejection')` in repo (MCP reply chain has no `.catch`, `mcp.ts:251-253`); dispatch errors swallowed pre-logger (`cli.ts:194-196`); `dispatchRun` hardcodes `repoPath: process.cwd()` ignoring `serve --repo` (`cli.ts:224` vs `:174`).
- Fix: error handler, signal handlers draining in-flight dispatches (deadline-bounded), global rejection logger, honor `--repo`, add `/healthz`.

### 9. git/gh invocations have no timeouts and allow interactive prompts
- `git/worktree.ts:4-21`, `integrations/github.ts:9,30` use bare `execFile` with no timeout and no `GIT_TERMINAL_PROMPT=0` — a credential prompt hangs the pipeline forever, past every other guard.
- Fix: route all git/gh through `spawnCli` with budgets; set `GIT_TERMINAL_PROMPT=0`, `GIT_ASKPASS=/bin/true`, `ssh -oBatchMode=yes`.

### 10. Dedup/idempotency evaporates on restart; replay possible
- Webhook dedup is an in-memory Set ≤10k (`webhook.ts:62-82`); delivery-id header is unsigned (signature covers body only, `webhook.ts:34-44`); no timestamp window → captured requests replay with fresh IDs; restart + provider redelivery re-runs completed tickets.
- Fix: persist delivery IDs keyed `(deliveryId, bodyHash)` with TTL (SQLite/JSONL); reject stale events where providers supply timestamps; consider signing scheme coverage notes in docs.

### 11. Entry points bypass locking entirely
- `fleet`, `task`, `orchestrate`, MCP `devagent_dispatch` never call `tryAcquireRun`; all task runs share synthetic id `'TASK'` (`task.ts:27-34`) → colliding worktrees/branches under concurrency (also confirmed by your own `.selfbuild/lessons.md` lines 19,23).
- Fix: unique per-run slug everywhere; acquire repo+branch lock in every entry point; serialize fleet jobs per repo.

### 12. Zombie process trees & leaked Docker state
- Timeout kills only the direct child (`spawn-utils.ts:35-38,43-56`); grandchildren survive holding ports/git locks. G2 skips teardown when `compose up` itself fails (`migration-apply-gate.ts:78-83` vs teardown only at :89/:98/:103).
- Fix: spawn detached + kill process group; wrap G2 in try/finally that tears down on every failure branch including up-failure.

### 13. GitLab publisher has zero resilience
- No retries on MR/note creation; a single failed poll throws away the whole 10-minute CI wait (`gitlab.ts:156-158`); OpenCode adapter lacks the resume-retry its Claude sibling has (`opencode.ts:12-33`).
- Fix: reuse the Linear-style Retry-After/backoff helper across integrations; tolerate N consecutive failed polls until deadline.

### 14. Observability won't scale; redaction is shallow
- JSONL logs never rotate (`logger.ts:34`); dashboard re-reads/parses every log ever written synchronously (`observe.ts:106-161`); no metrics, no health endpoint, no failure notifications. Redaction is top-level-key regex only — nested objects and message strings (which embed stderr tails and full prompts) pass through (`logger.ts:50-58`); artifacts land world-readable in `$HOME/.devagent`.
- Fix: rotation + retention sweep in `maintenance`; summarize dashboards from first/last lines; recursive value-pattern scrubbing (`lin_api_…`, `ghp_…`, `Bearer …`); chmod 0600; add a `/metrics` or push-on-failure hook.

---

## P2 — Architecture & scale

15. **God modules**: `deps.ts` inline-wires tracker selection + all gates + publishing + fan-out merge-assist; `cli.ts` is 750 lines with full command bodies (orchestrate spans `cli.ts:460-558`). Extract command modules and stage factories.
16. **Duplicated tracker selection** (`deps.ts:44-56` mirrors `cli.ts:62-75`, comment admits it) — introduce a `Tracker` interface; adding providers shouldn't touch two files.
17. **Ad hoc gate contracts**: three different gate result shapes (`pipeline.ts:38-50`); hardcode-ordered gates in the pipeline. Unify on one `Gate { name, shouldRun(plan), run(ctx): Result }` list.
18. **Double G1 execution**: tests run once per implement attempt (`deps.ts:243-244`) and again post-stage (`pipeline.ts:110-111`) sharing one timeout budget — doubled wall-clock on big suites.
19. **Worktree reuse contaminates re-runs**: existing dir returned as-is (`worktree.ts:64-67`); crashed attempts leak half-applied changes into the next "fresh" run. Reset-or-suffix on reuse.
20. **Keyword-classification planner decides which safety gates run** (`planner.ts:13-44`): misclassify `migration-required` → G2 skipped silently. Add a cheap post-implement re-check (e.g., changed-files include `migrations/**` ⇒ force G2 regardless of classification).
21. **Fragile rollback primitive**: `git reset --hard HEAD@{1}` assumes adjacent reflog entry (`merge.ts:77`); concurrent ref activity breaks it. Record the pre-merge SHA explicitly.
22. **MCP surface is unauthenticated with destructive capability**: `devagent_dispatch` takes arbitrary absolute `repoPath` + autoPr (`mcp.ts:178-191`); `devagent_answer` writes the board as "human input". Add env-configured repo allowlist + explicit opt-in flags.
23. **Fleet fan-out multiplies**: `--worker both` × concurrency spawns 2×N concurrent agents against shared git metadata; no per-repo serialization (`fleet.ts:53-87`).

---

## P3 — Testing, CI, release engineering

24. **Untested destructive paths**: `mergeProjectBranches` (reset --hard surgery, `merge.ts:39-92`), real `executeTask` (`executor.ts:14-86`), `runAudit` integrity check, GitLab publish path, `handleWebhook` HTTP layer — all zero direct tests. Mirror the `worktree.test.ts` real-git fixture pattern.
25. **CI validates almost nothing about shipping**: extend `.github/workflows/ci.yml` with `npm run build && npm pack --dry-run`, coverage thresholds (`@vitest/coverage-v8` not installed), lint/format (no ESLint/Prettier/Biome config exists), node `[20.x, 22.x]` matrix (code already needs ≥20.11 for `import.meta.dirname` despite engines `>=20`), `npm audit`/Dependabot, concurrency groups, artifact upload of run logs on failure.
26. **G2's real Docker path is never exercised**: no compose fixture exists in-repo; all tests mock the docker binary. Add an optional CI job running G2 against a checked-in postgres compose fixture.
27. **13 of 15 CLI commands have no tests** (`serve`, `validate`, `log`, `status`, `dashboard`, `task`, `orchestrate`, `project`, `mcp`, `clean`, `guard*`, `config`) — including `clean`, which deletes worktrees. Commander-action smoke tests over temp fixtures.
28. **Self-build ledger overclaims**: loop 5 records `testCommand`/Python detection as shipped (PR #15) but no trace exists in src/ (`test-gate.ts:18-34` knows only package.json/go.mod). Make each loop land acceptance criteria as failing tests first; verify merged-tree behavior post-PR.
29. **Docs/config drift**: PRD documents a nonexistent `plan` command and omits Jira/GitLab/webhook-secret env vars; dead config keys `pinnedVersions`/`linearTeamId` promise FR-OPS-03 behavior that doesn't exist; no `.env.example`; `devagent.json` keys undocumented and unvalidated (only `worker` enum checked, `config.ts:52-54`). Generate CLI spec from commander definitions; zod-validate config; delete or implement dead keys.

---

## Strengths worth preserving

- Dependency-injected pipeline (`pipeline.ts:35-53`) with proven dry-run mode.
- Evidence-gated trust model: executor success is `untrusted` until independent audit passes (`scheduler.ts:79-118`); human-in-loop `ask` verdicts are first-class.
- Merge-back discipline: topo order, per-merge G1 re-run, automatic rollback keeping earlier gated merges (`merge.ts:57-90`).
- cc-guard supervisor with jittered backoff and non-retryable classification (`sessionguard/*`).
- Timing-safe HMAC over raw body, respond-inside-5s webhook design (`webhook.ts:40-44,110-112`).
- Nested-env blocklist for spawned CLIs, honest structural gate-skips, atomic board writes.
- Test culture: real git fixtures, real subprocess lock tests, dogfooding that produced regression tests.

## Suggested sequencing

| Phase | Items | Theme |
|-------|-------|-------|
| 1 (days) | #4 spawnCli bug, #6 commit-before-push, #5 packaging/license/version, #9 git timeouts | Stop the bleeding |
| 2 (1-2 wks) | #1 trusted G2 config, #2 env allowlist, #7 locks, #8 serve lifecycle, #12 process trees/Docker teardown | Safety of the autonomous loop |
| 3 (2-4 wks) | #3 compose policy + containerized G1/G2, #10 durable dedup, #11 universal locking, #14 observability | Production isolation & ops |
| 4 (ongoing) | #15-#29 architecture refactor, CI hardening, test debt | Scale & maintainability |

*Quick wins under a day each: #4, #6, #5, #9, `chmod 0700 $DEVAGENT_HOME`, bind serve to 127.0.0.1 by default, `.env.example`, sanitize ticket ids at ref-construction sites (#refs: `deps.ts:86`, `merge.ts:60` vs existing `sanitizeTicketId` `worktree.ts:29-31`).*
