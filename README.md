# DevAgent

The Autonomous Backend Delivery Agent for modern engineering teams.

DevAgent integrates with your issue tracker (Linear in v1; Jira, GitHub Issues planned), parses backend specs, drafts database migrations, writes production-grade API code using headless coding-agent CLIs (Claude Code, OpenCode) as execution workers, validates every change inside sandboxed Docker containers, and delivers tested Pull Requests with auto-generated documentation for frontend teams.

```bash
# Process a ticket headlessly
devagent run --ticket LINEAR-204 --repo ./backend-service --auto-pr

# Interactive mode with mid-step human approvals
devagent run --ticket JIRA-8821 --interactive
```

## Why DevAgent

- **Set-and-forget backend ops** — assign a ticket to `@devagent` and get back a green, tested PR. A virtual team member, not an IDE extension.
- **Specialized domain intelligence** — general AI coders break database integrity and ignore async race conditions. DevAgent explicitly validates migration scripts, foreign-key safety, lock-risk patterns, and event-queue logic before anything leaves the machine.
- **Closed-loop testing** — nothing is submitted because it "looks right". Every change is verified against the real test suite and migrated schema inside an isolated container first.
- **Multi-worker fan-out** — the same ticket can run through Claude Code and OpenCode in parallel isolated worktrees; the validated winner becomes the PR.

## Documentation

| Document | Format | Description |
|---|---|---|
| [Product Requirements Document](docs/PRD.md) | Markdown | Full PRD: problem, personas, requirements (FR/NFR), architecture, pipeline, validation gates, CLI spec, integrations, metrics, risks, roadmap |
| [Product Requirements Document](docs/PRD.html) | HTML | Same document, styled single-file HTML for sharing |
| [cc-guard: auto-resume for headless sessions](docs/cc-guard.md) | Markdown | Supervisor that restarts Claude Code sessions killed by API failures ("Connection lost mid-response") via `devagent guard` |

Research sources backing the PRD are cited inline and collected in the [research appendix](docs/PRD.md#19-research-appendix).

## Status

v0.3.0 — v1 complete, fleet + observability landed (2026-08):

- **CLI**: `devagent run|serve|validate|log|status|dashboard|fleet|config`
- **Workers**: headless Claude Code (`claude -p`) and OpenCode (`opencode run`); fan-out mode (`--worker both`) runs parallel legs and picks the test-passing winner; retries carry gate evidence back as repair prompts
- **Gates**: G1 repo-native tests, G2 up/down migration apply (compose; honest skips without Docker), G3 static migration analysis (8 rules), G4 concurrency review scoped to the run's own diff
- **Fleet**: `devagent fleet --ticket A --ticket B --repo api=/repos/api ...` runs the ticket×repo matrix over a bounded pool with per-job failure isolation
- **Triggers**: CLI plus webhook server (`serve`) — HMAC verification, delivery dedup, latest-wins per ticket via lock registry
- **Delivery**: branch push + gh PR with plan, changed-file evidence, acceptance criteria (`--auto-pr`)
- **Resilience**: Linear 429 handling honors Retry-After with jittered backoff
- **Observability**: JSONL run logs, `status`, `log`, and a static HTML `dashboard`
- 125 tests green incl. end-to-end over real git fixtures

Deferred to later: deeper sandbox isolation beyond compose conventions, remote execution. See the [roadmap](docs/PRD.md#17-roadmap).

## Development

```bash
npm install
npm run typecheck && npm test   # verify
npm run dev -- --help           # command overview
npm run dev -- config           # smoke-test the CLI
```

Credentials via environment only: `LINEAR_API_KEY`, `GITHUB_TOKEN`, `LINEAR_WEBHOOK_SECRET` (for `serve`). See [PRD section 12](docs/PRD.md#12-cli-specification) for the full CLI contract.

## License

TBD.
