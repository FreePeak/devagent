<div align="center">

<img src="assets/icon.svg" width="96" alt="DevAgent logo"/>

# DevAgent

**The Autonomous Backend Delivery Agent — ticket in, tested pull request out.**

[![CI](https://github.com/FreePeak/devagent/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/FreePeak/devagent/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-6366F1.svg)](LICENSE)
[![Node](https://img.shields.io/badge/node-%E2%89%A520-22D3EE)](package.json)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](CONTRIBUTING.md)

</div>

---

DevAgent integrates with your issue tracker (Linear in v1; Jira, GitHub Issues planned), parses backend specs, drafts database migrations, writes production-grade API code using headless coding-agent CLIs (Claude Code, OpenCode) as execution workers, validates every change inside sandboxed Docker containers, and delivers tested Pull Requests with auto-generated documentation for frontend teams.

```bash
# Process a ticket headlessly
devagent run --ticket LINEAR-204 --repo ./backend-service --auto-pr

# Interactive mode with mid-step human approvals
devagent run --ticket JIRA-8821 --interactive
```

## Dashboard

Every orchestration run is observable. `devagent dashboard` renders a static
status board from run logs — kanban board, per-date run analytics, and feature
progress across projects:

| Board | Runs by date | Features |
|---|---|---|
| ![Board view](docs/screenshots/dashboard-board.png) | ![Runs by date view](docs/screenshots/dashboard-runs-by-date.png) | ![Features view](docs/screenshots/dashboard-features.png) |


## Why DevAgent

- **Set-and-forget backend ops** — assign a ticket to `@devagent` and get back a green, tested PR. A virtual team member, not an IDE extension.
- **Specialized domain intelligence** — general AI coders break database integrity and ignore async race conditions. DevAgent explicitly validates migration scripts, foreign-key safety, lock-risk patterns, and event-queue logic before anything leaves the machine.
- **Closed-loop testing** — nothing is submitted because it "looks right". Every change is verified against the real test suite and migrated schema inside an isolated container first.
- **Multi-worker fan-out** — the same ticket can run through Claude Code and OpenCode in parallel isolated worktrees; the validated winner becomes the PR.

## Orchestration

Beyond single tickets, `devagent orchestrate` decomposes a product goal into a
dependency DAG of small tasks and runs executors over it in bounded parallel
waves (LongHorizon-Harness pattern: plan -> execute -> audit -> checkpoint).

```bash
# Review the plan before spending executor tokens
devagent orchestrate --goal "Add CSV export to the orders API" --repo ./backend --plan-only

# Execute: planner decomposes, executors implement in worktrees, auditor verifies
devagent orchestrate --goal "Add CSV export to the orders API" --repo ./backend

# Resume a persisted board (.devagent-project.json); answer a paused task
devagent orchestrate --goal "" --resume --answer T3="use the analytics replica"
```

Key properties:

- **Evidence-gated completion** — an executor's success only moves a task to
  `untrusted`; it becomes `done` solely on an independent read-only audit
  verdict with clean integrity (workspace mutation during an audit voids the
  verdict). `--no-audit` restores executor-gates-only trust.
- **Role tiering** — planner, executor, and auditor are separate workers;
  point the auditor at a cheaper CLI (`--auditor opencode`) since auditing is
  the dominant token cost.
- **Targeted retries** — failed audits externalize unmet criteria as evidence
  gaps; the retry contract targets the gap instead of redoing blind work.
- **Recovery contracts** — when retries exhaust, the planner rewrites the
  contract around recorded failures (`--max-recoveries`, default 1) before a
  failure goes terminal.
- **Human in the loop** — auditors may return `ask`; the branch pauses until
  you answer via CLI (`--answer <id>=<text>`), MCP (`devagent_answer` tool,
  questions surfaced by `devagent_board`), or HTTP
  (`POST /api/answer` on `serve`, Bearer `DEVAGENT_ANSWER_TOKEN`).
- **Merge-back** — completed branches integrate topologically onto the base
  branch with gates re-run per merge.
- **Worker sandboxing** — agent-CLI workers never inherit secret-shaped env
  vars (`GITHUB_TOKEN`, cloud credentials, ...); an allowlist keeps only what
  the CLIs need (extend with `DEVAGENT_WORKER_ENV_ALLOWLIST`). On macOS,
  `DEVAGENT_SANDBOX=seatbelt` additionally runs workers under `sandbox-exec`
  with writes confined to the worktree and temp dirs, and
  `DEVAGENT_SANDBOX_NETWORK=deny` blocks all socket creation for fully
  offline worker runs. For tighter egress control,
  `DEVAGENT_SANDBOX_NETWORK=allowlist` denies all sockets except the resolved
  endpoints in `DEVAGENT_SANDBOX_NETWORK_ALLOWLIST` (comma-separated
  `host[:port]`, default port 443), e.g.
  `DEVAGENT_SANDBOX_NETWORK_ALLOWLIST="api.anthropic.com, registry.npmjs.org"`.
  Git, Docker, and test runner processes are unaffected.

## Documentation

| Document | Format | Description |
|---|---|---|
| [Product Requirements Document](docs/PRD.md) | Markdown | Full PRD: problem, personas, requirements (FR/NFR), architecture, pipeline, validation gates, CLI spec, integrations, metrics, risks, roadmap |
| [Product Requirements Document](docs/PRD.html) | HTML | Same document, styled single-file HTML for sharing |
| [cc-guard: auto-resume for headless sessions](docs/cc-guard.md) | Markdown | Supervisor that restarts Claude Code sessions killed by API failures ("Connection lost mid-response") via `devagent guard` |
| [LongHorizon-Harness analysis](docs/research/longhorizon-harness.md) | Markdown | Research backing evidence-gated orchestration: MEA loop, audit economics, recovery strategy (arXiv:2608.01964) |

Research sources backing the PRD are cited inline and collected in the [research appendix](docs/PRD.md#19-research-appendix).

## Status

v0.3.0 — v1 complete, fleet + observability landed (2026-08); evidence-gated
orchestration landed 2026-08-24 (loops 40-46):

- **CLI**: `devagent run|serve|validate|log|status|dashboard|fleet|config|orchestrate|project|mcp`
- **Workers**: headless Claude Code (`claude -p`) and OpenCode (`opencode run`); fan-out mode (`--worker both`) runs parallel legs and picks the test-passing winner; retries carry gate evidence back as repair prompts
- **Gates**: G1 repo-native tests, G2 up/down migration apply (compose; honest skips without Docker), G3 static migration analysis (8 rules), G4 concurrency review scoped to the run's own diff
- **Fleet**: `devagent fleet --ticket A --ticket B --repo api=/repos/api ...` runs the ticket×repo matrix over a bounded pool with per-job failure isolation
- **Triggers**: CLI plus webhook server (`serve`) — HMAC verification, delivery dedup, latest-wins per ticket via lock registry
- **Delivery**: branch push + gh PR with plan, changed-file evidence, acceptance criteria (`--auto-pr`)
- **Resilience**: Linear 429 handling honors Retry-After with jittered backoff
- **Orchestration**: goal -> DAG -> audited parallel execution with recovery
  contracts, human-in-the-loop ask/answer (CLI/MCP/HTTP), plan-only preview,
  topological merge-back (see [Orchestration](#orchestration))
- **MCP**: stdio server (`devagent mcp`) exposing dispatch/status/log/board/answer tools
- 250+ tests green incl. end-to-end over real git fixtures

Deferred to later: deeper sandbox isolation beyond compose conventions, remote execution. See the [roadmap](docs/PRD.md#17-roadmap).

## Development

```bash
npm install
npm run typecheck && npm test   # verify
npm run dev -- --help           # command overview
npm run dev -- config           # smoke-test the CLI
```

Credentials via environment only: `LINEAR_API_KEY`, `GITHUB_TOKEN`, `LINEAR_WEBHOOK_SECRET` (for `serve`). See [PRD section 12](docs/PRD.md#12-cli-specification) for the full CLI contract.

## Contributing

Contributions welcome — see [CONTRIBUTING.md](CONTRIBUTING.md) for setup and
conventions. For security issues, see [SECURITY.md](SECURITY.md); please do
not open public issues for vulnerabilities.

## License

[MIT](LICENSE) © FreePeak and DevAgent contributors.
