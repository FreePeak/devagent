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

DevAgent integrates with your issue tracker (Linear, Jira, GitHub Issues;
GitLab PR publishing), parses backend specs, drafts database migrations,
writes production-grade API code using headless coding-agent CLIs (omp by
default; Claude Code, OpenCode, pi, and Grok adapters) as execution workers,
validates every change inside sandboxed Docker containers, and delivers
tested Pull Requests with auto-generated documentation for frontend teams.

```bash
# 1. Setup: guided checks (git, worker CLIs, herdr, orca, credentials),
#    writes devagent.json with sane defaults
devagent init

# 2. Use: state a goal, watch it run, get a tested PR
devagent tui        # press `n`, type the goal in one sentence, Enter
```

That's the whole cold path — two commands. `devagent start` is an alias of
`devagent tui`. Power paths (`devagent run --ticket …`, `devagent task
--prompt …`, `devagent orchestrate --goal …`, the 24/7 `create`/`consume`
factory) are documented below and stay scriptable, but none of them is
required to reach a first tested PR.

## Install

Since the FR-GO-15 cutover (#204), the production entrypoint is the **Go
binary** (`cmd/devagent`). Install it from a release:

```bash
# GitHub CLI
gh release download --repo FreePeak/devagent --pattern 'devagent-darwin-arm64' --dir /tmp/devagent-install
chmod +x /tmp/devagent-install/devagent-darwin-arm64
mkdir -p ~/.local/bin && mv /tmp/devagent-install/devagent-darwin-arm64 ~/.local/bin/devagent

# or plain curl
curl -fL -o /tmp/devagent https://github.com/FreePeak/devagent/releases/latest/download/devagent-darwin-arm64
chmod +x /tmp/devagent && mkdir -p ~/.local/bin && mv /tmp/devagent ~/.local/bin/devagent

# Releases carry plain binaries for darwin/linux (amd64 + arm64) and
# devagent-windows-amd64.exe — swap the asset name for your platform.
```

> **Deprecated (legacy fallback):** `npm link` still works — the Node CLI in
> this repo (`src/cli.ts`) remains functional until FR-GO-16 and prints a
> deprecation notice on stderr (silence with `DEVAGENT_SUPPRESS_DEPRECATION=1`).
> New installs should use the release binary.

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
- **Multi-worker fan-out** — the same ticket can run through multiple worker
  CLIs in parallel isolated worktrees; the validated winner becomes the PR.
- **Self-improving loop** — DevAgent's own roadmap runs through its own
  factory: a 24/7 scout researches the PRD backlog into a task queue, workers
  ship tested PRs with auto-merge, and the orchestration ledger + lessons
  eval guard feed measured-impact context back into every prompt.

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

## Runtime visibility

### TUI dashboard

`devagent tui` is a one-command full-screen live dashboard — worker cards with
attach hints, session roster, a scrollable live log tail (SSE), queue/uptime/
activity metrics with sparkline and meter, iteration phase, detail panels,
kill, and an upgrade hint. It attaches to a running control daemon, and when
none is reachable it embeds an ephemeral one for the session (marked
`daemon:embedded` in the title bar); the daemon stops when you quit.
`--attach-only` never spawns one, and `devagent daemon` remains the way to run
a long-lived shared daemon. Flicker-free htop-style redraw; a non-TTY stdin
prints one snapshot and exits 0.

See [docs/TUI.md](docs/TUI.md) for the keyboard reference, daemon modes, and architecture.

### Herdr worker panes

Worker launches default to **visible herdr panes** when the `herdr` binary is
present — visible live, reattachable, disconnect-proof. When herdr is
unreachable the fallback to invisible child processes is loud (one warning
per spawn site plus a `visibility=fallback` ledger row), never silent:

- Force headless (CI/servers/LaunchAgents): `spawn.visibility: "headless"` in `devagent.json` or `DEVAGENT_VISIBILITY=headless` (`DEVAGENT_HERDR=0` forces the pane runtime off)
- Workers open in a named session — attach with `herdr session attach devagent` to watch them work
- `DEVAGENT_HERDR_KEEP_PANES=1` keeps completed panes around for inspection

Companion commands: `devagent sessions` (live panes), `devagent attach <task>`
(jump-in command), `devagent pane-run` (run a phase inside a pane),
`devagent herdr-sweep` (close stale automation panes only).

See [docs/HERDR.md](docs/HERDR.md) for the full behavior contract.


### LaunchAgent control

| Make target | Effect |
|---|---|
| `make agents-status` | Show loaded launchctl agents, disabled registry, running processes |
| `make agents-on` | Enable and bootstrap all repo plists already installed in `~/Library/LaunchAgents` |
| `make agents-off` | Boot out and disable all agent labels, then kill running loops |
| `make agents-install` | Render `launchagents/*.plist` into `~/Library/LaunchAgents`, lint, load |
| `make agents-uninstall` | Boot out, disable, and delete installed plists (repo copies untouched) |
| `make kill` | Kill loop scripts/workers and watchdog daemons; never touches launchctl state |

## Documentation

| Document | Format | Description |
|---|---|---|
| [Product Requirements Document](docs/PRD.md) | Markdown | Full PRD: problem, personas, requirements (FR/NFR), architecture, pipeline, validation gates, CLI spec, integrations, metrics, risks, roadmap |
| [Product Requirements Document](docs/PRD.html) | HTML | Same document, styled single-file HTML for sharing — regenerate after editing PRD.md: `pandoc docs/PRD.md -f gfm -t html5 -s --toc --toc-depth=2 --metadata title="DevAgent - Product Requirements Document" -H docs/prd-style.html -o docs/PRD.html` (pandoc 3.10.x) |
| [War Room mode](docs/WAR-ROOM.md) | Markdown | Goal-driven infinity loop: abstract idea → research → spec-until-clear → implement-until-evidenced. Built for new products and hackathons (`npm run warroom`) |
| [TUI dashboard](docs/TUI.md) | Markdown | `devagent tui` full-screen live dashboard: keyboard reference, daemon attach/embed modes, architecture |
| [cc-guard: auto-resume for headless sessions](docs/cc-guard.md) | Markdown | Supervisor that restarts Claude Code sessions killed by API failures ("Connection lost mid-response") via `devagent guard` |
| [LongHorizon-Harness analysis](docs/research/longhorizon-harness.md) | Markdown | Research backing evidence-gated orchestration: MEA loop, audit economics, recovery strategy (arXiv:2608.01964) |
| [Scout + Factory (24/7)](docs/SCOUT.md) | Markdown | 24/7 scout (opencode research → PRD → queue) + Orca workers (queue → PR → auto-merge → self-update) on macOS |
| [Scout + Factory PRD](docs/SCOUT-CREATE-PRD.md) | Markdown | Factory requirements: queue, scout daemon, `devagent create`, LaunchAgent, auto-merge, self-update |
| [Self-Build Loop](docs/SELF-BUILD-LOOP.md) | Markdown | Infinity loop driver (`scripts/selfbuild-loop.sh`) + Orca automation modes |
| [Git cleanup of merged MRs/PRs](docs/cleanup-merged.md) | Markdown | `scripts/git-cleanup-merged.sh`: delete local branches + worktrees whose GitLab MR / GitHub PR was merged, across all nested repos in `~/work` (dry-run default, launchd automation) |
| [Herdr runtime support](docs/HERDR.md) | Markdown | Run worker launches inside herdr panes (persistent terminal workspace manager): visible, reattachable, disconnect-proof; default-on with loud fallback, opt-out via `spawn.visibility: "headless"` |
| [DevAgent × Grok](docs/GROK.md) | Markdown | Grok/xAI integration plan (worker adapter M0–M2), 2026-09 competitive install-ease scan, and the few-tools easy-install path |

Research sources backing the PRD are cited inline and collected in the [research appendix](docs/PRD.md#19-research-appendix).

## Status

Releases are tagged automatically on every push to `main` (see
[Releases](https://github.com/FreePeak/devagent/releases)); each release
appends a `release-created` row to the orchestration ledger. Current surface:

- **Trackers & hosts** — Linear, Jira, GitHub Issues ingestion; GitHub + GitLab PR publishing
- **Workers** — headless omp (default), Claude Code, OpenCode, pi, and Grok Build CLI adapters; per-adapter model-id validation, cold-start + no-progress watchdogs, fan-out winner selection with flaky rerun
- **Gates** — G0 issue-readiness scoring, G1 repo-native tests, G2 up/down migration apply, G3 static migration analysis, G4 concurrency review, G5 STRIDE threat-model gate with per-PR allowlists
- **Orchestration** — goal → DAG → evidence-gated audited waves, recovery contracts, ask/answer via CLI/MCP/HTTP, per-task PRs, auto review + merge (CI-green gated), topological merge-back, remote dispatch over SSH (`devagent task --remote`)
- **Factory** — scout → queue → workers → auto-merge → self-update with LaunchAgent persistence (see [Factory](#factory-247-scout--orca-workers))
- **Control plane** — `devagent daemon` (HTTP + SSE + UDS, token auth), `devagent tui` full-screen dashboard, herdr visible worker panes
- **Resilience** — provider preflight with circuit breakers + degradation paging, typed board-recovery and selfbuild gates, doc-sync freshness gate, PR merge hygiene (`automerge`, `autosweep`, `pr-hygiene`), `rebase-stack` merge-queue refresh, prompt-size and lifetime-attempt caps
- **Self-knowledge** — orchestration ledger (PR, fixer, and release outcomes), lessons eval guard, markdown + LeanKG layered context digest
- **Simplicity pass (PRD §21)** — `devagent init` guided setup with verified smoke; card/chip human-readable `status`/`validate`/`ledger` output with `--json` opt-out
- 1300+ tests green, including end-to-end over real git fixtures

Deferred: deeper sandbox profiles beyond seatbelt/compose, pooled multi-tenant
remote execution.

## What's next

Tracked in the [roadmap](docs/PRD.md#17-roadmap) and the [Grok Bot & control app addendum](docs/PRD.md#20-product-direction-addendum-grok-bot-xai-integration-cross-platform-control-app):

- **Native xAI worker + SuperGrok auth** — the Grok Build CLI adapter ships today; native xAI API loops (Responses API caps, batch/off-peak routing) and consumer-plan OAuth are next (Q42/Q43)
- **Cross-platform desktop control app** — Tauri 2 tray + dashboard over the same FR-CTRL API the TUI already uses (macOS, Windows, Linux) (§20.4)
- **Bot-style UX floor** — named persistent agent identities, teach-once routines, visible bot-to-bot handoff (§20.1, Q45)
- **Durable knowledge-graph context** — whether the LeanKG digest persists across runs or stays per-run (Q28)

## Factory (24/7 scout + Orca workers)

```bash
# Bootstrap once (idempotent): scout on LaunchAgent + Orca workers + queue
devagent create --repo . --scout --workers 3 --auto-merge --self-update
devagent create --repo . --scout --workers 2 --dry-run   # preview without mutating

# Scout
devagent scout --once --dry-run                            # one deterministic cycle (no LLM)
devagent scout --once                                      # one live cycle (omp)
devagent scout --interval 30                               # daemon: loop every 30m until SIGINT
devagent scout-status                                      # heartbeat + queue depth

# Queue
devagent queue list --status pending
devagent queue show SCOUT-20260825-xxxx

# Workers: claim + implement + test + PR (+ auto-merge)
devagent consume --auto-pr --auto-merge
```

See [docs/SCOUT.md](docs/SCOUT.md) for the full factory runbook (LaunchAgent management, self-update, Orca provisioner).

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
