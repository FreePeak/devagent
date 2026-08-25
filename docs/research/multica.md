# Research: Multica and DevAgent integration

Date: 2026-08-24
Local clone: `~/work/opensources/multica` (shallow, main @ 658b0b7-era)
Sources: multica.ai, github.com/multica-ai/multica (47k stars), local code exploration.

## What Multica is

Open-source "project management for human + agent teams" (MUL- prefixed commits,
TypeScript apps + Go backend, Next.js 16 frontend, PostgreSQL 17 + pgvector).
Agents are first-class workspace members: profiles, issue assignment, comments,
status updates. Full task lifecycle enqueue -> claim -> start -> complete/fail,
broadcast over WebSocket. Self-hostable via Docker Compose (`make selfhost`)
or Helm chart.

## Architecture

| Component | Tech | Notes |
|---|---|---|
| Backend | Go single binary (Chi router) | `server/cmd/server/router.go` |
| Frontend | Next.js 16 | `apps/` |
| DB | Postgres 17 + pgvector | docker-compose.selfhost.yml |
| Daemon | Go, runs on each contributor machine | detects agent CLIs on PATH, claims tasks |

## Programmatic surface (for external orchestrators)

- Auth: PAT prefix `mul_`, `Authorization: Bearer` header
  (`server/internal/middleware/auth.go:34-51`). Create via `/api/tokens`
  (`handler/personal_access_token.go:60`).
- Issues: `POST /api/issues` with `assignee_type: "agent"` auto-enqueues an
  agent task (`internal/service/issue.go:362-367`). Full CRUD under
  `/api/issues` (`router.go:1781-1829`). Public API v1 at `/v1` with pinned
  OpenAPI spec (`server/pkg/publicapi/v1/routes.go`, `openapi.yaml`) — currently
  plugin-scoped; PATs use `/api/*`.
- Comments: `POST /api/issues/{id}/comments`; `@agent` mentions trigger runs
  (`handler/comment.go:73-88`).
- Results: `GET /api/tasks/{taskId}/messages` or CLI
  `multica issue run-messages <task-id>`.
- WebSocket: `task:queued/dispatch/running/progress/completed/failed/message`
  events (`server/pkg/protocol/events.go:34-42`); PAT accepted as first frame;
  scope subscription via `{type:"sub"}` frames (`realtime/hub.go:869-890`).
- Autopilots: schedule (cron, IANA tz) + webhook triggers only
  (`handler/autopilot.go:1289`). Webhook URL carries secret token, honors
  `Idempotency-Key`, GitHub-style event filters + signature verification.
  Dispatch job: `scheduler/jobs_autopilot.go`.
- CLI: cobra app with scriptable `issue list/get/create/assign/comment`,
  `autopilot *`, `skill import/refresh`, `runtime list`. No watch/follow
  command — live observation requires WS.
- MCP: Multica consumes MCP (workspace-level server config, per-agent binding,
  daemon-side broker `daemon/remote_mcp_broker.go`). It does NOT expose itself
  as an MCP server.

## Runtime model (can devagent be plugged in as a runtime?)

- Backend interface: `type Backend interface { Execute(ctx, prompt, opts)
  (*Session, error) }` (`server/pkg/agent/agent.go:17-23`); hard-coded factory
  switch over 24 whitelisted types (`agent.go:300-327, 365-427`). Fails closed
  on unknown types.
- `packages/plugin-sdk` is UI-only (sandboxed iframe panels) — NOT a runtime
  extension point (`plugin-sdk/index.ts:1-13`).
- Two protocol families matter: hand-rolled ACP over stdio JSON-RPC
  (hermes/grok/kimi/qoder/etc.; client in `hermes.go:469-582` + helpers
  `acp_*.go`) and Claude-style stream-json (`claude.go:720-725`).
- **Custom Runtime Profiles** (zero-fork route): declare a custom runtime whose
  `command_name` wraps any executable, constrained to an existing protocol
  family (`cmd/multica/cmd_runtime_profile.go:16-30`, daemon registration
  `daemon/daemon.go:2739-2810`). Protocol-critical flags (`-p`,
  `--output-format`, `--permission-mode`) are stripped from fixed args — the
  wrapper must speak the family protocol itself.
- First-class route = fork: new Go backend file + `SupportedTypes` entry +
  probe name in `config.go:881` + skills-dir case in `execenv/context.go:340`.
- Skills: stored server-side, delivered by writing files to provider-native
  discovery paths at claim time (claude -> `.claude/skills/`,
  opencode -> `.opencode/skills/`, default -> `.agent_context/skills/`;
  `execenv/context.go:150-380`), never injected into prompt text.
- Execution env: bare clones under `.repos/`, per-task git worktrees
  (`execenv/local_worktree.go:128`); exports `MULTICA_TOKEN/SERVER_URL/
  TASK_ID/AGENT_NAME...` (`daemon.go:151-170`); prompt tells the agent to pull
  context via `multica issue get <ID> --output json`.

## Integration options for devagent

### Option A (recommended): loose coupling — Multica as front door, devagent as engine

New thin adapter inside devagent's existing ingestion layer (same pattern as
Linear/Jira/GitHub adapters):

```
Multica board/autopilot --(REST poll w/ mul_ PAT or autopilot webhook)--> multica adapter
adapter -> devagent ticket -> pipeline (orchestrate, G1-G4, evidence gate, automerge)
pipeline stage events -> POST /api/issues/{id}/comments back to Multica timeline
PR link posted as final comment
```

- Cost: ~one adapter module + reporter; reuses HMAC/dedup patterns already in
  devagent's webhook server for the autopilot receiver.
- HITL: devagent ask-verdicts / pendingQuestions can surface as Multica
  comments instead of CLI `--answer`.
- Multica's own daemon/runtimes are bypassed for these tickets: execution
  authority stays with devagent's gate chain.

### Option B: devagent as a Multica runtime (tight coupling)

Via Custom Runtime Profile wrapping `devagent task --prompt` behind a shim
that speaks ACP stdio or Claude stream-json. Zero Multica fork, but:

- devagent becomes a single worker inside Multica's queue; its own
  orchestration DAG / fleet mode sits awkwardly under Multica's one-task-per-
  assignment model.
- Skills injection writes to provider dirs keyed by family — needs mapping work.
- Worth it later if you want Multica to own queueing/retries/session-resume.

### Option C: fork Multica, first-class `devagent` backend

Only if this becomes product direction; otherwise maintenance burden of a fork.

## Recommendation

Option A. Rationale:

1. Multica already runs claude/codex/cursor natively — plain assign-and-go
   needs no devagent. Devagent's differentiation is exactly what a bare CLI
   run lacks: gated pipeline, evidence contracts, planner DAG, automerge.
2. Option A is additive, reversible, ~one adapter; it fills devagent's real
   gap (human-facing cockpit, recurring scheduled triggers) without
   surrendering pipeline control.
3. Revisit Option B once the adapter proves daily-useful.

## Local bring-up checklist (self-host)

```bash
cd ~/work/opensources/multica && make selfhost   # Docker Compose stack
open http://localhost:3000                        # login: RESEND_API_KEY email codes,
                                                  # or APP_ENV=development +
                                                  # MULTICA_DEV_VERIFICATION_CODE=888888
brew install multica-ai/tap/multica               # CLI (or scripts/install.sh)
multica setup self-host                           # config + auth + start daemon
multica daemon status                             # verify
```

Machine readiness verified 2026-08-24: Docker 29.4.0 running, Compose v5.1.2,
135 GB free; `claude`, `codex`, `cursor-agent` on PATH (opencode is a shell
function — invisible to execFile-based probing, same gotcha as devagent loop 33).

## Risks / caveats

- License: repo LICENSE is "Other" (custom) — read before redistributing forks.
- No MCP exposure of Multica itself; automation must go REST/WS/CLI.
- WS has no ready-made follow CLI; polling `/api/issues/{id}/active-task` is
  the simple fallback for the adapter.
- Self-host defaults to production env; dev verification-code path must not be
  used on reachable hosts.
