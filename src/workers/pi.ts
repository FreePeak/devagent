import type { WorkerAdapter, WorkerEvent, WorkerResult, WorkerSpawnOptions } from '../types.js';
import type { SpawnCliResult } from './spawn-utils.js';
import { runWorkerCli } from './herdr-runtime.js';
import { prepareWorkerSpawn } from './sandbox.js';
import { backoffDelay } from '../sessionguard/backoff.js';
import { isNonRetryableApiError } from '../sessionguard/events.js';
import { isRetryableWithoutSession } from '../resilience/classify.js';

import { isNdjsonProgressLine } from './progress.js';

const RESUME_PROMPT = 'Continue';
const DEFAULT_API_MAX_ATTEMPTS = Infinity;
/**
 * If set on opts, override the per-attempt no-progress watchdog for pi.
 * Defaulting a nonzero watchdog is also load-bearing for stdin semantics:
 * spawnCli's execFile path leaves stdin an open pipe (a claude-code
 * requirement), but pi waits for stdin EOF and hangs forever on it
 * (2026-09-01 live smoke: direct CLI 10-24s; execFile 0 bytes until
 * wall-clock kill). A nonzero value routes the launch through
 * spawnCliStreaming, which ends stdin after spawn. 10 minutes mirrors the
 * omp adapter's default so retries fire instead of the wall clock being
 * the only safety net.
 */
const DEFAULT_NO_PROGRESS_TIMEOUT_MS = 10 * 60 * 1000;

function resolveNoProgressTimeoutMs(explicit: number | undefined): number {
  if (explicit !== undefined && explicit > 0) return explicit;
  const env = process.env.DEVAGENT_NO_PROGRESS_TIMEOUT_MS;
  if (env !== undefined && env !== '') {
    const n = Number(env);
    if (Number.isFinite(n) && n > 0) return n;
  }
  return DEFAULT_NO_PROGRESS_TIMEOUT_MS;
}

/**
 * Adapter over the pi headless CLI:
 *   pi --mode json -p <prompt> [--model <m>] [--thinking <v>]
 * stdout is NDJSON: one JSON object per line.
 *
 * pi never retries a response dropped mid-stream; when that happens
 * the turn dies but the session transcript persists. This adapter resumes the
 * same session (`--continue` / `-c`) with exponential backoff until
 * the turn completes, attempts are exhausted, or the error is auth/billing.
 *
 * pi uses provider-qualified model ids (`provider/model`, fuzzy matched).
 * Driver tier aliases like "coding" (devagent.json model) are NOT pi ids:
 * pi will error or fall back to its configured default. Drop any value
 * without a `/` so pi falls back to ~/.pi/agent/config.yml model.
 */
export class PiAdapter implements WorkerAdapter {
  readonly name = 'pi' as const;

  /**
   * PRD Q33: pi-specific progress classification. pi streams
   * message_update/thinking_delta lines during deliberation and
   * tool_execution_* lines when working; only the latter (plus answer text)
   * reset the no-progress watchdog.
   */
  isProgress(line: string): boolean {
    return isNdjsonProgressLine(line);
  }

  constructor(
    private readonly sleep: (ms: number) => Promise<void> = (ms) =>
      new Promise((resolve) => setTimeout(resolve, ms)),
  ) {}

  async spawn(opts: WorkerSpawnOptions): Promise<WorkerResult> {
    const start = Date.now();
    const maxAttempts = opts.apiMaxAttempts ?? DEFAULT_API_MAX_ATTEMPTS;
    const noProgressTimeoutMs = resolveNoProgressTimeoutMs(opts.noProgressTimeoutMs);
    const wallDeadline = opts.timeoutMs > 0 ? start + opts.timeoutMs : Infinity;

    let args = buildPiArgs(opts);
    let sessionId: string | null = null;
    let last: SpawnCliResult | null = null;

    for (let attempt = 1; attempt <= maxAttempts; attempt++) {
      // Wall-clock budget: stop retrying when the run's overall timeout is spent.
      if (Date.now() >= wallDeadline) {
        if (last) last.timedOut = true;
        break;
      }

      const prepared = await prepareWorkerSpawn('pi', args, {
        cwd: opts.cwd,
        timeoutMs: opts.timeoutMs,
        ...(opts.env ? { env: opts.env } : {}),
        ...(noProgressTimeoutMs ? { noProgressTimeoutMs } : {}),
      });

      last = await runWorkerCli(prepared.cmd, prepared.args, {
        ...prepared.opts,
        ...(noProgressTimeoutMs ? { noProgressTimeoutMs } : {}),
        ...(opts.herdr ? { herdr: true } : {}),
        label: `devagent pi #${attempt}`,
      });

      const outcome = interpretPi(last);
      if (outcome.sessionId) sessionId = outcome.sessionId;

      const ok = !last.timedOut && last.exitCode === 0 && !outcome.isError;
      if (ok) break;

      if (attempt === maxAttempts) break;
      if (last.exitCode === -1 && !last.timedOut) break; // spawn failure (ENOENT) — never retry forever
      if (outcome.errorText && isNonRetryableApiError(outcome.errorText)) break;

      // Transient detection: watchdog timeouts and provider errors are retried forever.
      // Without a session we retry from scratch only for provider/timeout signals.
      const errorText = outcome.errorText ?? last.stderr ?? '';
      if (!sessionId) {
        const retryableWithoutSession = isRetryableWithoutSession({
          timedOut: last.timedOut,
          exitCode: last.exitCode,
          errorText,
          stderr: last.stderr,
        });
        if (!retryableWithoutSession) break;
      }

      // Wall-clock budget already checked above; also don't retry if we'd exceed it after sleep
      if (Date.now() >= wallDeadline) break;

      await this.sleep(backoffDelay(attempt));
      // pi supports --continue / -c to resume the most recent session in cwd
      args = buildPiArgs(opts, true);
    }

    return finalize(last!, start);
  }
}

/**
 * Build the exact argv we pass to `pi` for a given spawn. Pure function —
 * exercised at the test seam without spawning the CLI.
 *
 *   pi --mode json -p <prompt> [--model <provider/model>] [--thinking <level>]
 *   pi --mode json --continue <resumePrompt>  (resume)
 */
export function buildPiArgs(opts: WorkerSpawnOptions, resume = false): string[] {
  const rawThinking = opts.variant?.trim();
  // pi requires provider-qualified model ids (`provider/model`, fuzzy matched).
  // Driver tier aliases like "coding" (devagent.json model) are NOT pi ids:
  // `--model coding` exits 1 in ~12s with no output. Drop any value without a `/`
  // so pi falls back to its configured default model (~/.pi/agent/config.yml).
  const rawModel = opts.model?.trim();
  const piModel =
    rawModel !== undefined && rawModel !== '' && rawModel.includes('/') ? rawModel : undefined;

  const base: string[] = ['--mode', 'json'];
  if (resume) {
    return [
      ...base,
      '--continue',
      RESUME_PROMPT,
      ...(piModel ? ['--model', piModel] : []),
      ...(rawThinking ? ['--thinking', rawThinking] : []),
    ];
  }
  return [
    ...base,
    '-p',
    opts.prompt,
    ...(piModel ? ['--model', piModel] : []),
    ...(rawThinking ? ['--thinking', rawThinking] : []),
  ];
}

interface PiOutcome {
  isError: boolean;
  sessionId: string | null;
  errorText?: string;
  resultText: string | null;
  parsed: Record<string, unknown> | null;
  timedOut: boolean;
}

/** Test seam: re-export of the parser. */
export function interpretPiForTest(run: SpawnCliResult): PiOutcome {
  return interpretPi(run);
}

/** Concatenated text parts of an assistant message; empty string when textless. */
function extractTextParts(m: Record<string, unknown>): string {
  const content = m.content;
  if (!Array.isArray(content)) return '';
  let out = '';
  for (const part of content) {
    if (part !== null && typeof part === 'object' && !Array.isArray(part)) {
      const p = part as Record<string, unknown>;
      if (typeof p.text === 'string' && p.text) out += p.text;
    }
  }
  return out;
}

/**
 * Parse pi's stdout into the worker outcome shape.
 * pi --mode json emits an NDJSON event stream (one JSON object per line).
 * The terminal assistant message carries the assistant text.
 *
 * We keep the FIRST assistant message_end (the user-answer turn), not the last —
 * pi may emit additional turns (e.g., prewalk) after the answer, and we want
 * the original assistant text, not subsequent thoughts.
 */
function interpretPi(run: SpawnCliResult): PiOutcome {
  let parsed: Record<string, unknown> | null = null;
  let sessionId: string | null = null;
  let terminalMessage: Record<string, unknown> | null = null;
  let streamError: string | null = null;

  // Try single-JSON first (legacy callers). If it parses as object/array,
  // skip the NDJSON walk.
  if (run.stdout.trim()) {
    try {
      const candidate: unknown = JSON.parse(run.stdout);
      if (Array.isArray(candidate)) {
        for (let i = candidate.length - 1; i >= 0; i--) {
          const e = candidate[i] as Record<string, unknown> | null;
          if (e !== null && typeof e === 'object' && !Array.isArray(e) && e.type === 'result') {
            parsed = e;
            break;
          }
        }
      } else if (candidate !== null && typeof candidate === 'object') {
        const obj = candidate as Record<string, unknown>;
        // NDJSON guard: real pi's first line {"type":"session",...} parses
        // as a single object but is just a stream header. Only treat the
        // single-JSON fast path as valid when the object has result
        // metadata; otherwise fall through to the NDJSON walk.
        if (
          'result' in obj ||
          'is_error' in obj ||
          ('type' in obj && obj.type === 'result') ||
          'session_id' in obj ||
          'id' in obj
        ) {
          parsed = obj;
        }
      }
    } catch {
      // Not a single JSON document — fall through to NDJSON walk.
    }
  }

  let rawResult: string | undefined;
  if (parsed !== null) {
    const raw = parsed.result;
    if (typeof raw === 'string') rawResult = raw;
    // pi uses "id" for session id in the session header event
    if (typeof parsed.id === 'string') sessionId = parsed.id;
    if (typeof parsed.session_id === 'string') sessionId = parsed.session_id;
  } else {
    // NDJSON walk: each non-empty line is one event. We keep the LAST
    // assistant message that carries text content — pi's agentic turns
    // emit one assistant message_end per turn (tool-call turns may have no
    // text), and the final answer is the last textual turn (2026-09-01
    // live smoke: first assistant turn was a toolCall turn, answer text in
    // the second). errorMessage capture stays first-wins.
    for (const line of run.stdout.split('\n')) {
      const trimmed = line.trim();
      if (!trimmed) continue;
      let event: Record<string, unknown> | null = null;
      try {
        const v: unknown = JSON.parse(trimmed);
        if (v !== null && typeof v === 'object' && !Array.isArray(v)) {
          event = v as Record<string, unknown>;
        }
      } catch {
        continue;
      }
      if (event === null) continue;
      // pi session header: {"type":"session","version":3,"id":"uuid",...}
      if (event.type === 'session' && typeof event.id === 'string') {
        sessionId = event.id;
      }
      // pi emits message_end for both user and assistant turns; only the
      // assistant's text is the worker result.
      if (event.type === 'turn_end' || event.type === 'message_end') {
        const msg = event.message;
        if (msg !== null && typeof msg === 'object' && !Array.isArray(msg)) {
          const m = msg as Record<string, unknown>;
          if (m.role === 'assistant') {
            const text = extractTextParts(m);
            if (text) terminalMessage = m;
            if (streamError === null && typeof m.errorMessage === 'string') {
              streamError = m.errorMessage;
            }
          }
        }
      }
    }
  }

  if (terminalMessage !== null) {
    rawResult = extractTextParts(terminalMessage);
  }

  // Determine error status: explicit isError, or non-zero exit with no result
  const isError = streamError !== null || (run.exitCode !== 0 && rawResult === undefined && parsed === null);
  const errorText = streamError ?? (parsed?.errorMessage as string | undefined) ?? (run.stderr.trim() ? run.stderr.trim().split('\n').slice(-3).join('\n') : undefined);

  return {
    isError,
    sessionId,
    errorText,
    resultText: rawResult ?? null,
    parsed,
    timedOut: run.timedOut,
  };
}

function finalize(run: SpawnCliResult, start: number): WorkerResult {
  if (run.timedOut) {
    return {
      exitCode: run.exitCode,
      events: [],
      resultText: null,
      sessionId: null,
      durationMs: Date.now() - start,
      timedOut: true,
      errorText: run.stderr.trim() || undefined,
    };
  }

  const outcome = interpretPi(run);
  if (!run.stdout.trim() || !outcome.resultText) {
    return {
      exitCode: outcome.isError ? 1 : run.exitCode,
      events: [],
      resultText: null,
      sessionId: outcome.sessionId,
      durationMs: Date.now() - start,
      timedOut: false,
      // Stderr often carries the real failure reason when the upstream returned
      // an empty stream. Surface it so the executor can classify properly.
      errorText: outcome.errorText ?? (run.stderr.trim() || undefined),
    };
  }

  const events: WorkerEvent[] = outcome.parsed ? [{ type: 'result', ...outcome.parsed }] : [];

  return {
    exitCode: run.exitCode,
    events,
    resultText: outcome.resultText,
    sessionId: outcome.sessionId,
    durationMs: Date.now() - start,
    timedOut: false,
    errorText: outcome.errorText,
  };
}
