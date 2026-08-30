# Plan: Add omp worker support to devagent

**Branch:** `feat/omp-support` (worktree `.worktrees/omp-support`)
**Status:** In progress
**Method:** TDD (red -> green -> refactor-free loop), one seam per cycle

## Goal

Add `omp` as a first-class worker (CLI coding agent) alongside the existing
`claude-code` worker, so the selfbuild loop can drive iterations through the
`omp` CLI headlessly.

## Background

- devagent spawns per-iteration workers via CLI adapters. The current adapter
  is `claude-code`, which shells out to `claude -p --output-format json` and
  parses the JSON result.
- Observed 2026-08-30 (memory + /tmp/omp-smoke.*): `omp -p "Reply with
  exactly: OK"` on model `omniroute/bai/glm-5.3-flash` produced no stdout for
  240s and required `pkill -x omp`. Same prompt via `claude -p` returned valid
  JSON in 31.6s. Therefore the omp adapter MUST carry an explicit timeout and
  kill the child process on expiry, and unit tests MUST NOT spawn live omp
  processes.
- Freepeak convention: feature branches live in worktrees under `.worktrees/`.

## Recon findings

- Test runner: vitest 2.1.8 (`npm test` -> `vitest run`); config
  `vitest.config.ts` with `include: ['test/**/*.test.ts']`, `environment: node`,
  `testTimeout: 30_000`.
- `src/types.ts:25` — `WorkerName = 'claude-code' | 'opencode'` (line 25).
  `WorkerAdapter` interface at lines 80-83.
- `src/workers/index.ts` — `workers` registry map (line 11); `getWorker`
  factory (line 17) throws on unknown.
- `src/workers/claude-code.ts:118-129` — `baseArgs(opts)` returns
  `['-p', prompt, '--output-format', 'json', ...model, ...maxSteps]`.
  `interpretForTest` at line 139; `finalize` at line 185.
- `src/workers/opencode.ts` — second adapter reference; `baseArgs` line 165
  returns `['run', '--format', 'json', ...model, prompt]`; `interpretOpencode`
  + `interpretOpencodeForTest` for parse seam.
- `src/workers/spawn-utils.ts` — `SpawnCliResult` and `spawnCli`.
- `src/workers/herdr-runtime.ts` — `runWorkerCli` shared runtime.
- `src/workers/sandbox.ts` — `prepareWorkerSpawn` shared prep.
- `src/config.ts:118-119` — config validation rejects workers not in
  `['claude-code', 'opencode', 'both']`; same for `scout.worker` at line 125.
- `src/cli.ts:116` `--worker` flag help; line 158, 527, 643-644 cast worker
  name to existing union — all require union widening + cast broadening.
- omp CLI flags (from `omp --help` v18.0.11, file `/tmp/omp-help.txt`):
  - `-p, --print` (line 30): non-interactive prompt-then-exit.
  - `--mode=<value>` (line 27): `text` (default), `json`, `rpc`, or `rpc-ui`.
  - `--model=<value>` (line 10): fuzzy provider/model match.
  - `-c, --continue` (line 31) and `-r, --resume=<value>` (line 32): session
    resume (no session-id flag, so we use `--session-dir` for lookup and
    `--continue` for the resume prompt).
  - `--max-time=<value>` (line 56): stop after duration.
  - `--no-session` (line 36): ephemeral.
  - `--thinking=<value>` (line 42).
  - `--cwd=<value>` (line 26).
  - `--print-thoughts` (line 55).
  - `--api-key=<value>` (line 20) for provider override.
  - No `--output-format json` — omp uses `--mode json` instead.
  - Observed 2026-08-30: `omp -p` on `omniroute/bai/glm-5.3-flash` produced
    no stdout for 240s; adapter MUST carry an explicit timeout via

## Steps

1. [x] Create worktree + branch `feat/omp-support`.
2. [x] Write this plan document; commit it.
3. [x] Recon (read-only): confirm test runner + scripts in `package.json`;
   locate the claude-code adapter file/symbol; worker type union in
   `src/types.ts`; config load/validation path; `omp --help` output flags.
   Record exact files/symbols below.
4. [x] Install dependencies in the worktree.
5. [x] RED: failing tests for Seams A-C in the repo's existing test layout.
6. [x] GREEN: implement omp adapter mirroring the claude-code adapter's
   exported interface; wire registry + type union + validation.
7. [x] Full suite + typecheck green in the worktree. Tests are never modified
   to pass; implementation is fixed instead.
8. [x] Timeout-guarded live smoke captured (`test/workers/__fixtures__/omp-smoke-2026-08-30.jsonl`,
   1591 lines of real NDJSON); parser test asserts session id + result text.
9. [x] Commit implementation on `feat/omp-support`. No push.

## Recon findings (filled during recon)

- Test runner: vitest 2.1.8 (`npm test` -> `vitest run`); config
  `vitest.config.ts` with `include: ['test/**/*.test.ts']`, `environment: node`,
  `testTimeout: 30_000`. Typecheck via `npm run build` (tsc).
- claude-code adapter: `src/workers/claude-code.ts`. `baseArgs(opts)` at
  `src/workers/claude-code.ts:118-129` returns
  `['-p', prompt, '--output-format', 'json', ...model, ...maxSteps]`.
  `interpretForTest` at line 139; `finalize` at line 185.
- Worker type union: `src/types.ts:25` -- `WorkerName = 'claude-code' | 'opencode' | 'omp'`.
  `WorkerAdapter` interface at lines 80-83.
- Config validation: `src/config.ts:118-120` rejects workers not in
  `['claude-code', 'opencode', 'omp', 'both']`; same for `scout.worker` at line 125.
- CLI flag surface: `src/cli.ts:30/116/465` `--worker <name>` help text and
  cast sites at `158`, `527`, plus scout casts at `1157` and `1261`.
- omp CLI flags: from `omp --help` v18.0.11 (snapshot: `/tmp/omp-help.txt`):
  `-p, --print` (headless prompt-then-exit), `--mode=<value>` (`text|json|rpc|rpc-ui`),
  `--model=<value>` (fuzzy provider/model match), `-c, --continue` and
  `-r, --resume=<value>` (session resume), `--max-time=<value>`, `--no-session`,
  `--thinking=<value>`, `--cwd=<value>`, `--print-thoughts`, `--api-key=<value>`.
  No `--output-format json` -- omp uses `--mode json` instead.
  Observed 2026-08-30: `omp -p` on `omniroute/bai/glm-5.3-flash` produced no
  stdout for 240s; adapter carries an explicit 10-min no-progress timeout plus
  wall deadline and SIGKILLs the child.

## Risks / edge cases

- omp headless hang -> adapter timeout + child kill; smoke capped at 120s.
- Unknown omp output shape -> parse only the captured fixture; add explicit
  error path for unparseable output.
- Config validation rejecting `worker: "omp"` -> schema/union update is part
  of the selection slice, not an afterthought.
- Worktree lacks `node_modules` -> install before first red run.
