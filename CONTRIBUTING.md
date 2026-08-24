# Contributing to DevAgent

Thanks for your interest in improving DevAgent. This document covers the
basics; the engineering rules of the road live in `docs/SELF-BUILD-LOOP.md`
(self-build protocol) and `docs/PRD.md` (product scope).

## Development setup

```sh
git clone git@github.com:FreePeak/devagent.git && cd devagent
npm install
npm test          # vitest, full suite
npm run typecheck # tsc --noEmit
```

Node 20+ required. Worker CLIs (claude-code, opencode) are optional; tests do
not call them.

## How we work

- One feature or fix per PR. Keep the diff reviewable.
- Tests are not optional: every behavior change ships with a test that fails
  without it.
- Product code always lands via pull request - never push directly to `main`.
- Commit messages: imperative mood, no AI attribution footers.
- If your change touches the orchestration pipeline (`src/orchestrator/`,
  `src/integrations/`), say so explicitly in the PR body; those paths have the
  widest blast radius.

## Filing issues

- Bug reports: include reproduction steps and evidence (logs, failing test).
- Feature requests: start from the problem, not the solution. The roadmap
  backlog lives in `docs/PRD.md` section 17; check it first so we can point
  you at existing plans.
- Questions: use Discussions rather than issues.

## Local verification checklist

```sh
npx vitest run            # all green
npm run typecheck         # clean
npx tsx src/cli.ts --help # smoke
```
