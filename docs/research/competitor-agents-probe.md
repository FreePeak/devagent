# Research: Competitor agents probe

Date: 2026-08-26
Scope: test probe — list 2 competitor agents to DevAgent (autonomous backend delivery agent).
Deeper dives: `docs/research/openhands-sweagent.md`, `docs/research/multica.md`.

## 1. Devin (Cognition AI)

Proprietary autonomous software-engineering agent (cognition.ai/devin,
github.com/Cognition-AI/devin for integrations). Takes a ticket (Slack, Linear,
Jira), plans, writes and runs code in a cloud VM with its own shell/browser/editor,
and opens PRs. Positioned exactly where DevAgent sits: end-to-end issue-to-PR
delivery rather than autocomplete. Closed source; priced per ACU (agent compute
unit). Differentiator to watch: their session replays and review workflow for
human sign-off.

## 2. OpenHands (formerly OpenDevin)

Open-source coding-agent platform (github.com/OpenHands-AI/OpenHands, MIT).
Event-stream architecture: an LLM agent loop acting through sandboxed runtime
actions (bash, browse, edit) with a browser UI and headless CLI mode. Supports
multiple models and has strong SWE-bench numbers via its agent-sdk/swe-agent line.
Most comparable open-source alternative for autonomous multi-step engineering
work; self-hostable, which Devin is not.
