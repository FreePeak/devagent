import { spawnCli } from '../workers/spawn-utils.js';
import { appendOperatorDegradedRecord } from '../orchestrator/ledger.js';
import { recordProxyProbe } from './proxy-state.js';

/**
 * Operator-role provider preflight (PRD §17 Phase 4, Q40).
 *
 * Operator loops (prd-curator.sh, po-loop.sh, selfbuild-loop.sh,
 * warroom-loop.sh, reviewer-loop.sh) must not dispatch an agent cycle while
 * the provider is hard-down: the loop either dies noisily mid-cycle or — the
 * 2026-09-02 curator failure class — completes "successfully" with a silent
 * noop. This gate reuses the orchestrate-loop probe pattern
 * (scripts/orchestrate-loop.sh): up to three cheap `omp -p OK` probes against
 * the configured worker CLI; on failure it records a structured
 * `operator-degraded` ledger row, updates the shared circuit state
 * (.devagent/proxy-state.json via recordProxyProbe), and reports a decision
 * the caller must honor by skipping that cycle's agent dispatch.
 */

/** Roles a preflight can gate; each maps to one operator loop script. */
export const PREFLIGHT_ROLES = ['prd-curator', 'po', 'selfbuild', 'warroom', 'reviewer'] as const;

export type PreflightRole = (typeof PREFLIGHT_ROLES)[number];

export function isPreflightRole(value: string): value is PreflightRole {
  return (PREFLIGHT_ROLES as readonly string[]).includes(value);
}

/** Probe invocations per gate run. */
export const PREFLIGHT_PROBE_ATTEMPTS = 3;

/** Hard wall-clock cap per probe. Must stay cheap but clear the slowest
 * observed gateway round-trip: omniroute/dev replies land at 25-38s (2026-09-03
 * live: shell probe 26s, execFile 25s, several >30s), so a 30s cap turned the
 * gate into a coin-flip that tripped the selfbuild circuit breaker. 60s keeps
 * the probe bounded while clearing the tail. */
export const PREFLIGHT_PROBE_TIMEOUT_MS = 60_000;

/** Sleep between failed probes (mirrors orchestrate-loop's 5s). */
export const PREFLIGHT_RETRY_DELAY_MS = 5_000;

/** The exact prompt every probe sends. */
export const PREFLIGHT_PROBE_PROMPT = 'OK';

/** Stable ledger taskId for operator preflight rows. */
export const PREFLIGHT_LEDGER_TASK_ID = 'operator-preflight';

/** One probe outcome. */
export interface PreflightProbe {
  ok: boolean;
  /** Bounded excerpt of the failure (stderr/stdout tail) when not ok. */
  detail?: string;
}

/** Final gate decision. */
export interface PreflightDecision {
  /** true = proceed with the cycle's agent dispatch. */
  ok: boolean;
  role: PreflightRole;
  attempts: number;
  /** Worker CLI probed, when the caller declared it (repo config). */
  worker?: string;
  /** Model id probed, when the caller declared it (empty = CLI default). */
  model?: string;
  /** Bounded last-failure excerpt; present when the gate degraded. */
  detail?: string;
}

/** Bound an error excerpt for ledger/log output. */
function boundDetail(text: string): string {
  return text.replace(/\s+/g, ' ').trim().slice(0, 200);
}

/**
 * One provider probe: ask the worker CLI to reply to "OK" and require an
 * answer. `--mode json` success looks like an event stream containing
 * `"text":"OK"`; a probe that exits 0 without it is degraded. This mirrors
 * the orchestrate-loop probe but runs through the same spawn path as the
 * worker adapters so env hardening (nested-env blocklist, PATH fallback)
 * stays consistent.
 */
export async function runPreflightProbe(
  cmd: string,
  args: string[],
  opts: { cwd: string; env?: Record<string, string>; /** Wall-clock cap; default PREFLIGHT_PROBE_TIMEOUT_MS. */ timeoutMs?: number },
): Promise<PreflightProbe> {
  const run = await spawnCli(cmd, args, { cwd: opts.cwd, timeoutMs: opts.timeoutMs ?? PREFLIGHT_PROBE_TIMEOUT_MS, env: opts.env });
  const ok = run.exitCode === 0 && run.stdout.includes('"text":"OK"');
  return ok
    ? { ok: true }
    : { ok: false, detail: boundDetail(`${run.stderr}\n${run.stdout}`) || `exit ${run.exitCode}` };
}

/**
 * The typed gate. Runs up to PREFLIGHT_PROBE_ATTEMPTS probes with
 * PREFLIGHT_RETRY_DELAY_MS between failures, records the circuit transition
 * (recordProxyProbe) and — on failure — one structured `operator-degraded`
 * ledger row, then decides. Callers MUST skip the cycle's agent dispatch
 * when `decision.ok` is false; that skip is the visible degradation.
 */
export async function runPreflightGate(args: {
  repoPath: string;
  role: PreflightRole;
  /**
   * Probe argv WITHOUT the prompt: `["<cmd>", "-p", ...flags]`. The gate owns
   * prompt placement — the prompt immediately follows the `-p` flag, then
   * caller flags, matching buildOmpArgs and the orchestrate-loop probe (a
   * trailing prompt would be swallowed as `-p`'s value).
   */
  argv: string[];
  cwd?: string;
  /** Worker CLI being probed (repo config; recorded on the ledger row). */
  worker?: string;
  /** Model id being probed (repo config; empty string = CLI default). */
  model?: string;
  /** Injection seam for tests. */
  probe?: (cmd: string, a: string[], o: { cwd: string; env?: Record<string, string> }) => Promise<PreflightProbe>;
  /** Injection seam for tests: sleep between failed probes. */
  delayMs?: (ms: number) => Promise<void>;
}): Promise<PreflightDecision> {
  const probe = args.probe ?? runPreflightProbe;
  const delayMs = args.delayMs ?? ((ms: number) => new Promise<void>((r) => setTimeout(r, ms)));
  const cwd = args.cwd ?? args.repoPath;
  const [cmd, promptFlag, ...flags] = args.argv;
  if (!cmd || promptFlag !== '-p') {
    throw new Error('preflight argv must start with ["<cmd>", "-p", ...flags] (prompt flag before flags)');
  }

  let attempts = 0;
  let last: PreflightProbe = { ok: false, detail: 'no probe ran' };
  while (attempts < PREFLIGHT_PROBE_ATTEMPTS) {
    attempts += 1;
    try {
      last = await probe(cmd, [promptFlag, PREFLIGHT_PROBE_PROMPT, ...flags], { cwd });
    } catch (err) {
      last = { ok: false, detail: boundDetail((err as Error).message) };
    }
    if (last.ok) break;
    if (attempts < PREFLIGHT_PROBE_ATTEMPTS) await delayMs(PREFLIGHT_RETRY_DELAY_MS);
  }

  const decision: PreflightDecision = {
    ok: last.ok,
    role: args.role,
    attempts,
    ...(args.worker ? { worker: args.worker } : {}),
    ...(args.model !== undefined ? { model: args.model } : {}),
    ...(last.ok ? {} : { detail: last.detail }),
  };
  if (last.ok) {
    // Advance the shared circuit on success too: without this a provider
    // recovery leaves a stale `open` from the last failure, and consumers
    // reading the circuit keep short-circuiting (2026-09-03: circuit stuck
    // open since 13:21Z despite green probes from 01:58Z).
    recordProxyProbe(args.repoPath, { ok: true });
  } else {
    // Shared circuit state so `devagent status --providers` reports the
    // operator-loop degradation exactly like the orchestrate-loop gate.
    recordProxyProbe(args.repoPath, { ok: false, detail: `preflight[${args.role}]: ${last.detail ?? 'failed'}` });
    // Structured degradation row: the ledger is the evidence a degraded
    // factory cycle was skipped, not silently noop'd (Q40).
    appendOperatorDegradedRecord(args.repoPath, {
      ts: new Date().toISOString(),
      kind: 'event',
      event: 'operator-degraded',
      taskId: PREFLIGHT_LEDGER_TASK_ID,
      attempt: attempts,
      role: args.role,
      ok: false,
      attempts,
      worker: args.worker ?? '',
      model: args.model ?? '',
      ...(last.detail ? { detail: last.detail } : {}),
    });
  }
  return decision;
}
