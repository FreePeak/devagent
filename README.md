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

Research sources backing the PRD are cited inline and collected in the [research appendix](docs/PRD.md#19-research-appendix).

## Status

Pre-code, design phase (2026-08). See the [roadmap](docs/PRD.md#17-roadmap) — Phase 0/1 targets the end-to-end spine: one Linear ticket to one green PR.

## License

TBD.
