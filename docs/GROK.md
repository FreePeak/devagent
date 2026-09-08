# DevAgent × Grok: integration plan, competitive scan, and the easy-install path

> Written 2026-09-06. Scope: (1) where the codebase stands today, (2) a fresh scan of the
> competing agent products with an eye on **install ease and minimal tool surface**, (3) the
> concrete plan to make Grok (xAI) a first-class devagent worker, and (4) the plan that gets a
> new user from zero to a running agent in three commands. This doc is the working companion to
> PRD §20 (Grok Bot benchmark, FR-GROK, FR-CTRL, FR-UI) and §21 (FR-SIMPLE) — it does not
> replace them; it turns them into an ordered, evidence-backed build plan.

---

> **2026-09-08 status (FR-GO-16, #205):** the repo is Go-only — the Node tree
> (src/, package.json, npm tooling) this plan was written against has been
> deleted. M0 shipped ahead of that: the Grok Build CLI adapter lives at
> `internal/workers/grok.go` (registered like the other adapters; env allowlist
> adds `XAI_API_KEY` in `internal/workers/sandbox.go`). Install is the release
> binary (README "Install"); `npm i -g devagent` and the `src/*.ts` file:line
> citations below describe the pre-migration tree and are kept as the
> historical record of the plan and its evidence.

---

## 1. TL;DR

- **The market moved to "install and go."** Every serious competitor now leads with a
  one-line script install, browser OAuth (no API keys), and a headless flag (`-p` / `exec` /
  `-x`). Friction today is not model quality — it is minutes-to-first-task.
- **devagent is architecturally ready for Grok but not packaging-ready for humans.** The
  worker-adapter layer (PRD §9, `src/workers/*`) makes a `grok` worker a ~1-file change with
  8 known touch points; but the product cannot be installed with one command (no npm publish,
  worker CLIs are external, `init` implements only part of FR-SIMPLE-01).
- **Grok ships two first-party paths**: the official **Grok Build CLI** (`grok -p
  --output-format streaming-json`, Apache-2.0, open-sourced 2026-07) and the
  **OpenAI-compatible API** (`api.x.ai/v1`) with the cheapest coding model at $1/M input,
  $2/M output (`grok-build-0.1`). CLI-first was already decided (PRD Q42): the CLI gives us
  the full agent loop (tools, AGENTS.md, MCP, resume) and maps 1:1 onto the existing omp
  adapter pattern.
- **Recommended order:**
  1. **M0 — grok worker + install story** (FR-GROK-01/02 + npm publish + real `init`): this
     is what makes devagent "few tools, everybody starts."
  2. **M1 — cost/cache/resilience polish** (FR-GROK-03/04/06) + per-worker provider probe.
  3. **M2 — surface work**: Batch routing (FR-GROK-05), devagent-as-MCP inside Grok Build
     TUI (already built, needs docs), desktop control app per PRD §20.4.
- **What we do not build:** a chat-first bot front door, a cloud computer, or an X/Twitter
  integration. Grok Bot is the benchmark to match locally, not a surface to chase (§5.3).

---

## 2. Codebase snapshot (what exists, verified 2026-09-06)

### 2.1 The one abstraction that matters

Every model call funnels through a worker adapter — devagent never calls an LLM API directly
(PRD §8.2 rule 1). The dispatch chain:

```
devagent.json + env creds
  → loadConfig allowlist check        src/config.ts:164-166 (worker), :171-172 (scout.worker)
  → model-id preflight                src/workers/model-id.ts:48-65 (per-adapter predicate registry)
  → getWorker(name)                   src/workers/index.ts:15-30 (registry)
  → adapter builds CLI argv           pattern: src/workers/omp.ts buildOmpArgs
  → env scrub + optional seatbelt     src/workers/sandbox.ts (DEFAULT_ENV_ALLOWLIST)
  → herdr pane or direct child        src/workers/herdr-runtime.ts:40-70
  → execFile + wall timeout +         src/workers/spawn-utils.ts (SIGKILL wall clock,
    no-progress watchdog                meaningful-NDJSON-line progress only)
  → NDJSON/JSON parser → WorkerResult src/workers/omp.ts interpretOmp
  → gates G1–G4 on the worktree       worker-agnostic, PRD §11
```

The `WorkerAdapter` contract is three members (`src/types.ts:142-154`): `name`,
optional `isProgress(line)`, and `spawn(WorkerSpawnOptions): Promise<WorkerResult>`.
Current adapters: `claude-code`, `opencode`, `omp` (default), `pi` (`src/types.ts:53`).

### 2.2 The Grok worker touch list (copy the omp.ts pattern)

*(Node-tree touch list as specced 2026-09-06; the adapter shipped in Go —
`internal/workers/grok.go` — with the same semantics: argv builder, NDJSON
interpret, model-id predicate, allowlist widening, probe branch.)*

1. `src/types.ts:53` — add `'grok'` to `WorkerName`.
2. **New `src/workers/grok.ts`** — pure `buildGrokArgs` (`grok -p --output-format
   streaming-json`, model forwarded only when exact-slug or `xai/`-qualified), `interpretGrok`
   NDJSON walk (session id from `type:"session"`, assistant text policy, in-stream
   `errorMessage` capture — Grok Build exits 0 on provider failures, same as omp), bounded
   retry/resume loop via `prepareWorkerSpawn` → `runWorkerCli`.
3. `src/workers/index.ts:15-21` — register + export.
4. `src/workers/model-id.ts:48-54` — grok predicate: exact slugs (`grok-4.6`,
   `grok-build-0.1`, `grok-4.3`, dated pins) + `xai/` prefix; reject tier aliases (FR-GROK-02;
   the loop-58 `--model coding` burn is the precedent).
5. `src/config.ts:164-166` and `:171-172` — widen both allowlists with `'grok'`.
6. CLI help strings + `init` worker-bin map (`init.ts:126`: grok → binary `grok`) +
   `src/commands/probe-argv.ts:9-22` — add a grok probe branch (used by both `init` and
   `devagent preflight`).
7. `src/workers/sandbox.ts` — add `XAI_API_KEY` to `DEFAULT_ENV_ALLOWLIST` (the
   `/_API_KEY$/` scrubber strips it otherwise).
8. `npm run build` — the shipped bin (`dist/src/cli.js`) and the scout LaunchAgent both run
   compiled output; a src-only change keeps throwing the stale-dist error at runtime.
9. Selfbuild/warroom pinning needs **no code change**: `SELFBUILD_RESEARCH_BIN` /
   `SELFBUILD_PO_BIN` / `SELFBUILD_WORKER` envs (`scripts/selfbuild-loop.sh:45-51,490`) —
   research/PO phases can run Grok today via env; the executor needs items 1–8.
10. Tests: captured live NDJSON fixture under `test/workers/__fixtures__/` (the
    `omp-smoke-2026-08-30.jsonl` precedent) for the parser; argv seams are pure functions.

Later per-role upgrades (already specced, not needed for M0): FR-GROK-03 exact-cost ledger
rows from `usage.cost_in_usd_ticks`, FR-GROK-04 `prompt_cache_key` stickiness per task,
FR-GROK-06 xAI 429-RPS/TPM classification + in-xAI fallback chain, FR-GROK-05 Batch routing.

### 2.3 Install & first-run story at writing time — the honest list

What a new user did then: clone → install deps → build the TS bundle → make `devagent`
resolvable (an undocumented global link) → separately install a worker CLI and log into its
provider → export `LINEAR_API_KEY`/`GITHUB_TOKEN` → `devagent init` → dispatch. Frictions,
verified at the time (F1's no-installable-package gap closed by the release binaries —
README "Install"):

| # | Friction | Evidence |
|---|---|---|
| F1 | No installable package: bin exists only after manual build + link; nothing published to npm | `package.json:7-8` (`bin` → `dist/src/cli.js`); no install docs in README/CONTRIBUTING |
| F2 | Worker CLI install/auth fully external and unguided; four CLIs with four login models | `init.ts:56-67` only *advises*; probe is **omp-only** (`init.ts:96-99`, `probe-argv.ts:10-22`) |
| F3 | `init` implements ~half of FR-SIMPLE-01: no Docker check, no credential capture, no verified smoke dispatch | PRD.md:1268 vs `init.ts:111-131` |
| F4 | `run`/`fleet`/`serve` hard-require `LINEAR_API_KEY` while `init` marks it optional → first `run` fails after a green init | `cli.ts:119-121,172-174,226-232` vs `init.ts:113-124` |
| F5 | Model-id footgun: unqualified aliases rejected/burned mid-board; surfaced only via `status --providers` | `model-id.ts:27-39`; loop-58 incident |
| F6 | README/PRD prose says "validated in sandboxed Docker containers" but G1 runs tests directly and G2 auto-skips without Docker — Docker is *not* on the required path | `test-gate.ts:26-63`, `migration-apply-gate.ts:37-52`; README.md:18, PRD.md:37 |
| F7 | Doc drift: `docs/SELF-BUILD-LOOP.md` says `SELFBUILD_WORKER` default `claude-code`; script default is `omp` | selfbuild-loop.sh:45 vs docs/SELF-BUILD-LOOP.md |

F6 cuts both ways: it is a doc lie, **and** it is an install win — the main path already
needs only the devagent binary, `git`, one worker CLI, and `gh` (for PRs). The "few tools"
story is nearly true; the docs and the packaging just didn't say so.

---

## 3. Competitive scan (fresh, fetched 2026-09-06)

### 3.1 Install-ease table

Condensed from the full scan (all claims verified against vendor docs/GitHub on 2026-09-06;
source URLs in PRD §19.2 and below where new):

| Product | Install | Auth | Headless | Open? |
|---|---|---|---|---|
| Claude Code | `curl -fsSL https://claude.ai/install.sh \| bash` | browser OAuth; Pro/Max/Enterprise or Console | `-p --output-format json` | no |
| Codex CLI | `curl -fsSL https://chatgpt.com/codex/install.sh \| sh` | "Sign in with ChatGPT" OAuth | `codex exec` | Apache-2.0 |
| Gemini CLI | `npx @google/gemini-cli` | Google OAuth; **free 60 req/min + 1k/day** | `-p` | Apache-2.0 |
| opencode | `curl -fsSL https://opencode.ai/install \| bash` | BYO keys / Zen proxy | `opencode run` | open source |
| Cursor CLI | `curl https://cursor.com/install -fsS \| bash` | account OAuth | `agent -p` | no |
| Cline | `npm i -g cline` | OAuth or BYO key | `cline "p" --json --yolo` | Apache-2.0 |
| Amp | ampcode.com script | OAuth; `AMP_API_KEY` for headless | `amp -x` | CLI closed |
| Goose | curl script | keys or ACP subscription OAuth | CLI + headless modes | Apache-2.0 |
| Qwen Code | `npm i -g @qwen-code/qwen-code` | OAuth free tier **discontinued 2026-04**; now paid/3P keys | `qwen -p`, `qwen serve` | Apache-2.0 |
| Crush | brew/npm | API keys (or Charm Hyper sub) | `--yolo` | FSL-1.1-MIT |
| Aider | `curl -LsSf https://aider.chat/install.sh \| sh` | **API keys only** | `--message --yes-always` | Apache-2.0 |
| Copilot cloud agent | **no install** — `@copilot` on GitHub issues/PRs | Copilot subscription | cloud-only | no |
| Devin | `curl -fsSL https://cli.devin.ai/install.sh \| bash` + web app | account + paid plan | API/CLI handoff to cloud | no |
| Jules | **no install** — jules.google.com | Google + GitHub OAuth | async VM-based | no |
| **Grok Build** | `curl -fsSL https://x.ai/cli/install.sh \| bash` | browser OAuth (SuperGrok/X Premium+) or `XAI_API_KEY` | `grok -p --output-format streaming-json` | **Apache-2.0** (xai-org/grok-build) |

Sources: code.claude.com/docs/en/setup; github.com/openai/codex; github.com/google-gemini/gemini-cli; opencode.ai/docs; cursor.com/docs/cli; github.com/cline/cline; ampcode.com/docs; goose docs; github.com/QwenLM/qwen-code; github.com/charmbracelet/crush; aider.chat/docs/install.html; docs.github.com Copilot cloud agent; docs.devin.ai; jules.google/docs; docs.x.ai/build/overview.

### 3.2 Synthesis: what makes an agent feel "install and go"

1. **One-line script → single static binary.** The headline install is always `curl | sh`;
   npm is the fallback, never the story.
2. **OAuth over API keys.** Codex/Claude/Gemini/Cursor/Amp/Cline log in via browser; products
   that demand raw API keys read as "developer tool," not "install and go."
3. **Zero-config after auth.** Repo conventions (`AGENTS.md`), skills, and MCP discovered
   automatically; config optional.
4. **Headless as a first-class flag**, always the same shape: `-p`/`exec`/`-x` + JSON output.
5. **Bot surface = distribution.** GitHub issue assignment (Copilot, Jules), Slack/Teams
   (Devin, Copilot), IM bridges (Qwen Code channels, Cline connect). This is where competitors
   live where users already are.

### 3.3 Where devagent stands

**Wedge (unchanged, still true):** nobody else validates migrations (G2/G3), races (G4), or
gates merges on evidence (G5 + audit-gated orchestration). PRD §4.2. Generic coding CLIs are
becoming interchangeable model shells — Claude Code, Codex, Gemini CLI, opencode, and now
Grok Build all speak "headless prompt in, diff out" — which *is the point of the worker
adapter layer*: devagent is the validation-and-orchestration layer above them, and adding a
worker is a day of work, not a bet.

**Gap, and it is the one users feel first:** every competitor above can be *tried* in under
two minutes; devagent needs clone/build/link/worker-setup. The competitive scan says the
fix is packaging, not features: one-command install, one OAuth/key moment, one smoke
dispatch that proves the loop end-to-end. That is exactly FR-SIMPLE-01/02 — currently the
biggest open risk in the product (§2.3 F1–F4).

---

## 4. Grok integration plan

### 4.1 What xAI offers today (verified 2026-09-06)

- **Grok Build CLI** (official): `curl -fsSL https://x.ai/cli/install.sh | bash`; Rust,
  Apache-2.0, open-sourced 2026-07 (github.com/xai-org/grok-build). Headless
  `grok -p "..." --output-format streaming-json` (NDJSON events — same adapter shape as
  omp); `~/.grok/config.toml` `[model.*]` with `base_url`/`env_key` (so our omniroute proxy
  plugs in directly); `grok inspect`; **ACP support** ("build your own bots and agent
  orchestration apps"); MCP servers discovered like any other agent. Auth: browser OAuth on
  first launch or `XAI_API_KEY`. (docs.x.ai/build/overview; x.ai/news/grok-build-cli;
  x.ai/news/grok-build-open-source)
- **API** (`https://api.x.ai/v1`): OpenAI-compatible `chat/completions` + `responses`
  (`max_turns`); every response carries `usage.cost_in_usd_ticks` (exact cost, free ledger
  accounting). Models & list prices (per 1M tokens): **grok-4.6** 500k ctx, $2.00/$6.00 —
  recommended default for code; **grok-build-0.1** 256k ctx, **$1.00/$2.00** — fastest
  coding model, aliases `grok-code-fast-1`; **grok-4.3** 1M ctx, $1.25/$2.50, batch-eligible.
  Cached input ≈ 25–50% of input price; `prompt_cache_key` sticky routing recommended.
  Function calling ≤128 tools, structured outputs, server-side tools (~$5/1k calls, opt-in).
  Tier-0 (free entry) rate limits are generous: grok-4.6 150 RPS / 50M TPM; grok-build-0.1
  37 RPS / 10M TPM. (docs.x.ai/developers/models; docs.x.ai/developers/rate-limits;
  x.ai/news/grok-4-1-fast)
- **Grok Bot** (x.ai/bot): "AI teammates on a cloud computer" — launched 2026-08-11, plans
  widened 2026-08-26, Enterprise 2026-09-03. Bundled with SuperGrok tiers. This is the
  consumer benchmark from PRD §20.1; it is cloud-hosted and plan-gated — devagent's wedge is
  the same teammate UX, local, private, BYO-model.
- **No first-party Telegram/Discord bot**; no xAI GitHub-issue→PR product (Grok reaches
  GitHub via Copilot: grok-4.5 2026-07-28, grok-4.6 2026-08-14 in Copilot). IM-bot patterns
  are community-made (superagent-ai/grok-cli Telegram remote; community grok-telegram-bot
  driving the official CLI over ACP).
- **Cost sanity** [INFERENCE — arithmetic on listed rates]: one autonomous worker turn on
  `grok-build-0.1` (~50k tok repo context + ~10k out) ≈ $0.05–0.07 (cached ≈ $0.02–0.03); a
  10-turn bug-fix session ≈ $0.25–0.75; grok-4.6 ≈ 2–3× that. Cheaper than any seat-based
  plan for bursty autonomous work, and `usage.cost_in_usd_ticks` makes the ledger exact.

### 4.2 Direction A — `grok` as a first-class worker (build this)

CLI-first per PRD Q42. M0 = §2.2 touch list 1–10. Hardening carried over from the omp
lessons (these are the known traps, all already solved for omp — port, don't rediscover):

| Trap | omp precedent | Port to grok |
|---|---|---|
| Headless turn never terminates | `--no-prewalk` (986+ thinking deltas on glm) | watch for `--no-…` equivalents; if none, watchdog is the net |
| LSP/MCP discovery stalls startup 60–487s in worktrees | `--no-lsp --no-extensions` | verify `grok inspect` discovery cost in a worktree before enabling extensions |
| Unqualified model alias exits 1 in ~12s | `buildOmpArgs` drops ids without `/`; `model-id.ts` gate | exact-slug + `xai/` predicate (FR-GROK-02) |
| Provider failure exits 0 | in-stream `errorMessage` capture | same in `interpretGrok` |
| Thinking deltas count as progress, watchdog never fires | `isNdjsonProgressLine` meaningful-line filter | copy; streaming-json events map cleanly |
| 429s | Retry-After handling, `resilience.apiMaxAttempts` | classify xAI RPS-vs-TPM 429 separately (FR-GROK-06) |

Verification for M0 is live, not fixture-only: a real `grok -p` smoke against this repo
(prompt → diff → G1 green), plus the parser fixture test. Acceptance: `devagent run
--prompt "..." --worker grok --auto-pr` on a scratch repo ends in a green PR; `devagent
preflight --role executor` with `worker=grok` passes; `SELFBUILD_WORKER=grok` drives a loop
iteration.

### 4.3 Direction B — devagent as tools *inside* Grok (already built; document it)

`devagent mcp` (src/server/mcp.ts) already exposes `devagent_dispatch / status / log /
board / ledger / answer` over stdio. Grok Build discovers MCP servers natively
(`grok inspect`). So a Grok Build user can add devagent as an MCP server and dispatch
ticket-to-PR runs from inside the Grok TUI — **zero new code**, one config snippet in the
docs. This is the cheapest possible "integrate with the grok bot" story and it ships with
M0's documentation pass.

### 4.4 Direction C — Grok Bot as a front door (defer)

Grok Bot can sign into tools and message teammates; community bots already drive the
official CLI over ACP from Telegram. A devagent front door on an IM surface would be a
thin wrapper over the existing HTTP `POST /api/answer` + webhook `serve` transport
(FR-CTRL). But §20.5 explicitly says: not a chat-first product in v1; dispatch + approval
is the interaction. Defer until M0/M1 land; when picked up, the transport already exists.

### 4.5 Sequencing

| Milestone | Contents | Why now |
|---|---|---|
| **M0** | grok adapter (§2.2), npm publish + install one-liner, real `init` (worker assist, per-worker probe, credential capture, smoke dispatch — FR-SIMPLE-01), doc honesty pass (F6/F7) | The competitive scan says minutes-to-first-task decides adoption; both gaps close together |
| **M1** | FR-GROK-03/04/06 (cost ledger, cache key, 429 classes), per-worker probes beyond grok, `run`-without-Linear path promoted (prompt-driven `task` as the default first-run) | Operational polish after the happy path exists |
| **M2** | FR-GROK-05 Batch routing, desktop control app (FR-UI, PRD §20.4), IM front door if demand appears | Surface work on a stable core |

---

## 5. Easy install & use — the "few tools" contract

Target (realized at FR-GO-15/16): **download the release binary, `devagent init`,
`devagent orchestrate --goal "..."`** — three commands, one login moment, no Docker, no
herdr, no tracker credentials for the first run.

### 5.1 Few tools, stated honestly

Required: the devagent binary (release download; Go 1.25+ only when building from
source), git, one worker CLI (any of grok/claude/opencode/omp/pi — pick one at init),
`gh` for PR delivery. Explicitly *not* required for the main path: Docker (G1 runs
tests directly; G2 gracefully skips), herdr (panes are opt-in visibility), Linear/Jira keys
(`task`/`orchestrate` are prompt-driven; tracker keys unlock `run`/`fleet`/`serve` only).
The docs must say exactly this (F6) — it converts the install story from "heavy platform"
to "one CLI plus the agent CLI you already have."

### 5.2 Work items (maps to FR-SIMPLE-01/02 and F1–F5)

1. **Publish the CLI** (F1): release binaries per platform (README "Install" leads with
   the `gh release download` / `curl` one-liner — the competitor pattern: script/one-liner
   is the headline).
2. **Init completes FR-SIMPLE-01** (F2/F3): check git; offer to install the chosen worker
   CLI (grok: the official curl installer; claude/opencode likewise) or detect it; per-worker
   provider probe (extend `buildProbeArgvFor` — omp keeps its hardening flags, grok gets
   `grok -p "OK" --output-format streaming-json`); credential capture to the OS keychain or
   an env block; finish with a **verified smoke dispatch** (fixture goal → done) and a
   plain-language checklist. Exit non-zero only on required failures.
3. **Make first `run` impossible to fail on credentials** (F4): either `init` captures the
   tracker keys or `run` fails fast at *argument parsing* with the same checklist language,
   never mid-pipeline.
4. **Model-id guardrail at init time** (F5): when `--model` is given, validate against the
   registry immediately and print the corrected form (e.g. `xai/grok-build-0.1`), so the
   loop-58 burn cannot happen on a fresh machine.
5. **`status` stays the one next-action surface** (FR-SIMPLE-03/04): already card/chip
   based; keep "current phase + one next action" the invariant on every new surface
   (docs, installer summary, smoke output).

### 5.3 Non-goals (reaffirmed from PRD §20.5)

No cloud computer, no chat-first transport, no UI PTY-wrapping of worker CLIs, no second
event system. The desktop app (FR-UI) reads the same daemon API; the IM front door reads
the same HTTP API; both are later transports, not new architecture.

---

## 6. Verification & sources

- Repo claims: every file:line in this doc verified against the working tree on 2026-09-06
  (worker contract `src/types.ts:53,142-154`; registry `src/workers/index.ts:15-30`;
  allowlists `src/config.ts:164-172`; model-id registry `src/workers/model-id.ts:48-65`;
  init gaps `src/commands/init.ts`; probe `src/commands/probe-argv.ts:9-22`; loop pins
  `scripts/selfbuild-loop.sh:45-51,372-376,490`).
- Web claims: docs.x.ai/build/overview; docs.x.ai/developers/models;
  docs.x.ai/developers/rate-limits; x.ai/news/grok-build-cli; x.ai/news/grok-build-open-source;
  x.ai/news/introducing-grok-bot; x.ai/news/grok-bot-more-plans; x.ai/news/grok-bot-for-enterprise;
  x.ai/news/grok-4-1-fast (Agent Tools API); github.com/xai-org/grok-build — all fetched
  2026-09-06. Competitor rows: vendor docs/GitHub fetched same day (URLs in §3.1).
- Items marked [INFERENCE]: §4.1 cost arithmetic.
- Known doc drift to fix alongside M0: F6 (Docker oversell in README/PRD prose), F7
  (`SELFBUILD_WORKER` default in docs/SELF-BUILD-LOOP.md).
