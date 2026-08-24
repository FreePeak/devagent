# Security Policy

## Reporting a vulnerability

Do not open a public issue for security vulnerabilities. Report privately to
**infra@freepeak.dev** with:

- Affected component and commit/version
- Reproduction steps or proof of concept
- Impact assessment

You will receive an acknowledgment within 72 hours. We will coordinate a fix
and disclosure timeline with you.

## Scope

In scope: anything that lets DevAgent execute unintended code, escape its
worker sandbox, exfiltrate secrets from `.env` / repo-local config, or tamper
with repositories it operates on.

Out of scope: vulnerabilities in worker CLIs themselves (claude-code,
opencode) - report those upstream; social engineering of human operators.

## Design invariants worth testing

DevAgent enforces a few rules reviewers should treat as security boundaries:

- Workers inherit repo-local env only; no secret material flows into prompts
  (`docs/SELF-BUILD-LOOP.md`, guardrails section)
- Product code reaches `main` exclusively through PRs with green CI
- Sandbox validation gates run inside isolated containers before delivery
