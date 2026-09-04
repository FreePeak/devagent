import type { WorkerAdapter, WorkerEvent, WorkerResult, WorkerSpawnOptions } from '../types.js';
import type { SpawnCliResult } from './spawn-utils.js';
import { runWorkerCli } from './herdr-runtime.js';
import { prepareWorkerSpawn } from './sandbox.js';
import { backoffDelay } from '../sessionguard/backoff.js';
import { isNonRetryableApiError } from '../sessionguard/events.js';
import { isRetryableWithoutSession, isTransientProviderError } from '../resilience/classify.js';

const RESUME_PROMPT = 'Continue';
const DEFAULT_API_MAX_ATTEMPTS = Infinity;

/**
 * Cheap liveness probe for the zero-event/empty-output signature (loop 59
 * run-11: a hung opencode burned all retry attempts on full-timeout runs
 * while a trivial probe returned empty output). Short wall-clock deadline:
 * an alive endpoint answers in seconds; anything else (empty output, error,
 * timeout, spawn failure) means the endpoint is dead and retrying the real
 * prompt would only burn the remaining budget.
 */
const PROBE_TIMEOUT_MS = 30_000;
const PROBE_PROMPT = 'Reply with the single word: ok';

function resolveNoProgressTimeoutMs(explicit: number | undefined): number {
  if (explicit !== undefined) return explicit;
  const env = process.env.DEVAGENT_NO_PROGRESS_TIMEOUT_MS;
  if (env !== undefined && env !== '') {
    const n = Number(env);
    if (Number.isFinite(n) && n >= 0) return n;
  }
  return 0;
}

/**
 * Adapter over the OpenCode headless CLI:
 *   opencode run --format json <prompt>
 * stdout is NDJSON: one JSON object per line, possibly interleaved with garbage.
 *
 * On transient API failures (e.g. `Error from provider (Console Go): Upstream
 * request failed: Endpoint is unavailable.`) the turn dies but the session
 * persists. This adapter resumes the same session (`--session <id> Continue`)
 * with exponential backoff forever (default Infinity) until it succeeds,
 * matching the claude-code adapter's resume-retry semantics. The loop only
 * stops on success, wall-clock timeout, spawn failure, or non-retryable
 * auth/billing errors; any provider error keeps looping with backoff.
 * Binary fallback: tries `opencode` then `opencode2` if the first binary is
 * missing (ENOENT -> exitCode -1).
 */
export class OpenCodeAdapter implements WorkerAdapter {
  readonly name = 'opencode' as const;

  constructor(
    private readonly sleep: (ms: number) => Promise<void> = (ms) =>
      new Promise((resolve) => setTimeout(resolve, ms)),
  ) {}

  async spawn(opts: WorkerSpawnOptions): Promise<WorkerResult> {
    const start = Date.now();
    const maxAttempts = opts.apiMaxAttempts ?? DEFAULT_API_MAX_ATTEMPTS;
    const noProgressTimeoutMs = resolveNoProgressTimeoutMs(opts.noProgressTimeoutMs);
    const wallDeadline = opts.timeoutMs > 0 ? start + opts.timeoutMs : Infinity;

    let sessionId: string | null = null;
    let last: SpawnCliResult | null = null;
    let lastEvents: WorkerEvent[] = [];
    let lastResultText: string | null = null;
    let binary: 'opencode' | 'opencode2' = 'opencode';
    let noProgress = false;
    let args = baseArgs(opts, binary);

    for (let attempt = 1; attempt <= maxAttempts; attempt++) {
      if (Date.now() >= wallDeadline) {
        if (last) last.timedOut = true;
        break;
      }
      const prepared = await prepareWorkerSpawn(binary, args, {
        cwd: opts.cwd,
        timeoutMs: opts.timeoutMs,
        ...(opts.env ? { env: opts.env } : {}),
        ...(noProgressTimeoutMs ? { noProgressTimeoutMs } : {}),
        ...(opts.watchdogLedger ? { watchdogLedger: opts.watchdogLedger } : {}),
      });
      let raw = await runWorkerCli(prepared.cmd, prepared.args, {
        ...prepared.opts,
        ...(noProgressTimeoutMs ? { noProgressTimeoutMs } : {}),
        ...(opts.herdr ? { herdr: true } : {}),
      });
      // Binary fallback: if `opencode` is not installed, try `opencode2` once.
      if (isSpawnFailure(raw) && binary === 'opencode') {
        binary = 'opencode2';
        args = baseArgs(opts, binary);
        const fallbackPrepared = await prepareWorkerSpawn(binary, args, {
          cwd: opts.cwd,
          timeoutMs: opts.timeoutMs,
          ...(opts.env ? { env: opts.env } : {}),
          ...(noProgressTimeoutMs ? { noProgressTimeoutMs } : {}),
          ...(opts.watchdogLedger ? { watchdogLedger: opts.watchdogLedger } : {}),
        });
        raw = await runWorkerCli(fallbackPrepared.cmd, fallbackPrepared.args, {
          ...fallbackPrepared.opts,
          ...(noProgressTimeoutMs ? { noProgressTimeoutMs } : {}),
          ...(opts.herdr ? { herdr: true } : {}),
        });
        if (isSpawnFailure(raw)) {
          last = raw;
          lastEvents = [];
          lastResultText = null;
          break;
        }
      }

      last = raw;
      const outcome = interpretOpencode(raw);
      if (outcome.sessionId) sessionId = outcome.sessionId;
      lastEvents = outcome.events;
      lastResultText = outcome.resultText;

      const zeroEvent = isZeroEventNoProgress(outcome, raw);
      const ok = raw.exitCode === 0 && !outcome.isError && !raw.timedOut && !zeroEvent;
      if (ok) break;

      // Zero-event empty-output signature: probe cheaply before burning the
      // next full attempt (loop 59 run-11). A dead endpoint bails out
      // immediately with noProgress so callers can fall back; a live one
      // gets the normal backoff retry (fresh prompt, since a silent run
      // typically emitted no session id to resume).
      if (zeroEvent) {
        if (attempt < maxAttempts && Date.now() < wallDeadline) {
          const probe = await probeEndpointAlive(opts, binary, attempt);
          if (!probe.alive) {
            noProgress = true;
            break;
          }
          await this.sleep(backoffDelay(attempt));
          args = sessionId ? resumeArgs(sessionId, opts, binary) : baseArgs(opts, binary);
          continue;
        }
        // No budget for another attempt: surface the no-progress outcome
        // instead of a false success.
        noProgress = true;
        break;
      }

      if (attempt === maxAttempts) break;
      if (raw.exitCode === -1 && !raw.timedOut) break; // spawn failure (ENOENT etc) — never loops forever
      if (outcome.errorText && isNonRetryableApiError(outcome.errorText)) break;
      // Wall-clock overall budget already checked at loop top
      if (Date.now() >= wallDeadline) break;

      const errorText = outcome.errorText ?? raw.stderr ?? '';
      if (!sessionId) {
        const retryableWithoutSession = isRetryableWithoutSession({
          timedOut: raw.timedOut,
          exitCode: raw.exitCode,
          errorText,
          stderr: raw.stderr,
        });
        if (!retryableWithoutSession) break;
      }

      await this.sleep(backoffDelay(attempt));
      if (sessionId) args = resumeArgs(sessionId, opts, binary);
      else args = baseArgs(opts, binary);
    }

    const final = last!;
    if (final.timedOut) {
      return {
        exitCode: final.exitCode,
        events: [],
        resultText: null,
        sessionId: null,
        durationMs: Date.now() - start,
        timedOut: true,
      };
    }

    return {
      exitCode: final.exitCode,
      events: lastEvents,
      resultText: lastResultText,
      sessionId,
      durationMs: Date.now() - start,
      timedOut: false,
      // Zero-event attempts (probe-dead bail or exhausted budget) surface as
      // noProgress, never as a false success.
      ...(noProgress ? { noProgress: true } : {}),
    };
  }
}

function modelArgs(opts: WorkerSpawnOptions, binary: 'opencode' | 'opencode2'): string[] {
  if (!opts.model) return [];
  const rawModel = opts.model.trim();
  const variant = opts.variant?.trim();
  // opencode2 encodes variant as provider/model#variant; opencode uses a separate --variant flag.
  // If the model already carries a #variant suffix, keep it as-is and don't duplicate.
  const hasHashVariant = rawModel.includes('#');
  if (!variant || hasHashVariant) return ['--model', rawModel];
  if (binary === 'opencode2') return ['--model', `${rawModel}#${variant}`];
  return ['--model', rawModel, '--variant', variant];
}

function baseArgs(opts: WorkerSpawnOptions, binary: 'opencode' | 'opencode2' = 'opencode'): string[] {
  return ['run', '--format', 'json', ...modelArgs(opts, binary), opts.prompt];
}

function resumeArgs(sessionId: string, opts: WorkerSpawnOptions, binary: 'opencode' | 'opencode2' = 'opencode'): string[] {
  return ['run', '--format', 'json', ...modelArgs(opts, binary), '--session', sessionId, RESUME_PROMPT];
}

function isSpawnFailure(run: SpawnCliResult): boolean {
  return run.exitCode === -1 && !run.stdout.trim() && !run.stderr.trim() && !run.timedOut;
}

/**
 * The zero-event/empty-output signature: exit 0 but no parsed events and no
 * result text (pure garbage or empty stdout parses the same). Not a success —
 * the worker turned nothing. Classified as noProgress so callers can
 * distinguish it from success and fall back cheaply.
 */
function isZeroEventNoProgress(outcome: OpencodeOutcome, run: SpawnCliResult): boolean {
  return run.exitCode === 0 && !run.timedOut && outcome.events.length === 0 && !outcome.resultText;
}

interface ProbeResult {
  /** True when the probe got a non-empty response (endpoint alive). */
  alive: boolean;
}

/**
 * Short-deadline trivial probe run before spending another full retry attempt
 * on the zero-event signature. Alive = non-empty output; empty/error/timed-out
 * responses all mean the endpoint is dead and further attempts are waste.
 */
async function probeEndpointAlive(
  opts: WorkerSpawnOptions,
  binary: 'opencode' | 'opencode2',
  attempt: number,
): Promise<ProbeResult> {
  const probeArgs = baseArgs({ ...opts, prompt: PROBE_PROMPT }, binary);
  const prepared = await prepareWorkerSpawn(binary, probeArgs, {
    cwd: opts.cwd,
    timeoutMs: PROBE_TIMEOUT_MS,
    ...(opts.env ? { env: opts.env } : {}),
  });
  const raw = await runWorkerCli(prepared.cmd, prepared.args, {
    ...prepared.opts,
    ...(opts.herdr ? { herdr: true } : {}),
  });
  return { alive: raw.exitCode === 0 && !raw.timedOut && raw.stdout.trim().length > 0 };
}

interface OpencodeOutcome {
  isError: boolean;
  sessionId: string | null;
  errorText?: string;
  events: WorkerEvent[];
  resultText: string | null;
}

export function interpretOpencodeForTest(run: SpawnCliResult): OpencodeOutcome {
  return interpretOpencode(run);
}

function interpretOpencode(run: SpawnCliResult): OpencodeOutcome {
  const events: WorkerEvent[] = [];
  let sessionId: string | null = null;
  let errorText: string | undefined;

  for (const line of run.stdout.split('\n')) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    let obj: unknown;
    try {
      obj = JSON.parse(trimmed);
    } catch {
      continue;
    }
    if (obj === null || typeof obj !== 'object' || Array.isArray(obj)) continue;
    const record = obj as Record<string, unknown>;
    events.push({ type: typeof record.type === 'string' ? record.type : 'event', ...record });
    const sid =
      typeof record.sessionID === 'string'
        ? record.sessionID
        : typeof record.session_id === 'string'
          ? record.session_id
          : typeof (record as Record<string, unknown>).sessionID === 'string'
            ? String((record as Record<string, unknown>).sessionID)
            : null;
    if (sid) sessionId = sid;
    // also check nested part.sessionID
    const part = record.part as Record<string, unknown> | undefined;
    if (!sessionId && part && typeof part.sessionID === 'string') sessionId = part.sessionID;
  }

  // Detect error event
  let isError = false;
  for (const e of events) {
    if (e.type === 'error' || (e as Record<string, unknown>).error !== undefined) {
      isError = true;
      errorText = extractErrorText(e);
      break;
    }
  }
  if (!isError && run.exitCode !== 0) {
    isError = true;
    // Prefer stderr tail, else stdout-embedded message
    if (run.stderr.trim()) errorText = run.stderr.trim().split('\n').slice(-3).join('\n');
    else if (events.length === 0 && run.stdout.trim()) errorText = run.stdout.trim().slice(0, 500);
  }
  if (!errorText && isError && run.stderr.trim()) {
    errorText = run.stderr.trim().split('\n').slice(-3).join('\n');
  }

  // resultText: last text found
  let resultText: string | null = null;
  for (const event of events) {
    const t = extractText(event);
    if (t) resultText = t;
  }

  return { isError, sessionId, errorText, events, resultText };
}

function extractErrorText(event: WorkerEvent): string | undefined {
  const rec = event as Record<string, unknown>;
  const err = rec.error as unknown;
  if (!err) return undefined;
  if (typeof err === 'string') return err;
  if (typeof err === 'object' && err !== null) {
    const e = err as Record<string, unknown>;
    if (typeof e.message === 'string' && e.message) return e.message;
    if (e.data && typeof e.data === 'object' && e.data !== null) {
      const d = e.data as Record<string, unknown>;
      if (typeof d.message === 'string' && d.message) return d.message;
      if (typeof d.error === 'string') return d.error;
    }
    if (typeof e.error === 'string') return e.error;
    try {
      return JSON.stringify(e).slice(0, 500);
    } catch {
      return String(e);
    }
  }
  return undefined;
}

function extractText(event: WorkerEvent): string | null {
  const rec = event as Record<string, unknown>;
  if (typeof rec.text === 'string' && rec.text) return rec.text;
  if (typeof rec.part === 'string' && rec.part) return rec.part;
  const part = rec.part as Record<string, unknown> | undefined;
  if (part && typeof part.text === 'string' && part.text) return part.text;
  // opencode2 nested: part.content[].text etc not needed for now
  return null;
}
