import type { WorkerAdapter, WorkerEvent, WorkerResult, WorkerSpawnOptions } from '../types.js';
import { spawnCli, type SpawnCliResult } from './spawn-utils.js';
import { prepareWorkerSpawn } from './sandbox.js';
import { backoffDelay } from '../sessionguard/backoff.js';
import { isNonRetryableApiError } from '../sessionguard/events.js';

const RESUME_PROMPT = 'Continue';
const DEFAULT_API_MAX_ATTEMPTS = Infinity;

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

    let sessionId: string | null = null;
    let last: SpawnCliResult | null = null;
    let lastEvents: WorkerEvent[] = [];
    let lastResultText: string | null = null;
    let binary: 'opencode' | 'opencode2' = 'opencode';
    let args = baseArgs(opts);

    for (let attempt = 1; attempt <= maxAttempts; attempt++) {
      const prepared = await prepareWorkerSpawn(binary, args, {
        cwd: opts.cwd,
        timeoutMs: opts.timeoutMs,
        ...(opts.env ? { env: opts.env } : {}),
      });
      let raw = await spawnCli(prepared.cmd, prepared.args, prepared.opts);
      // Binary fallback: if `opencode` is not installed, try `opencode2` once.
      if (isSpawnFailure(raw) && binary === 'opencode') {
        binary = 'opencode2';
        const fallbackPrepared = await prepareWorkerSpawn(binary, args, {
          cwd: opts.cwd,
          timeoutMs: opts.timeoutMs,
          ...(opts.env ? { env: opts.env } : {}),
        });
        raw = await spawnCli(fallbackPrepared.cmd, fallbackPrepared.args, fallbackPrepared.opts);
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

      if (raw.timedOut) break;

      const ok = raw.exitCode === 0 && !outcome.isError;
      if (ok) break;

      if (attempt === maxAttempts) break;
      if (raw.exitCode === -1) break; // spawn failure (ENOENT etc) — never loops forever
      if (!sessionId) break; // cannot resume without a session id
      if (outcome.errorText && isNonRetryableApiError(outcome.errorText)) break;

      await this.sleep(backoffDelay(attempt));
      args = resumeArgs(sessionId, opts);
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
    };
  }
}

function baseArgs(opts: WorkerSpawnOptions): string[] {
  return ['run', '--format', 'json', ...(opts.model ? ['--model', opts.model] : []), opts.prompt];
}

function resumeArgs(sessionId: string, opts: WorkerSpawnOptions): string[] {
  return ['run', '--format', 'json', ...(opts.model ? ['--model', opts.model] : []), '--session', sessionId, RESUME_PROMPT];
}

function isSpawnFailure(run: SpawnCliResult): boolean {
  return run.exitCode === -1 && !run.stdout.trim() && !run.stderr.trim() && !run.timedOut;
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
