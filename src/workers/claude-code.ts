import type { WorkerAdapter, WorkerEvent, WorkerResult, WorkerSpawnOptions } from '../types.js';
import type { SpawnCliResult } from './spawn-utils.js';
import { runWorkerCli } from './herdr-runtime.js';
import { prepareWorkerSpawn } from './sandbox.js';
import { backoffDelay } from '../sessionguard/backoff.js';
import { isNonRetryableApiError } from '../sessionguard/events.js';
import { isRetryableWithoutSession } from '../resilience/classify.js';

const RESUME_PROMPT = 'Continue';
const DEFAULT_API_MAX_ATTEMPTS = Infinity;

function resolveNoProgressTimeoutMs(explicit: number | undefined): number {
  if (explicit !== undefined) return explicit;
  const env = process.env.DEVAGENT_NO_PROGRESS_TIMEOUT_MS;
  if (env !== undefined && env !== '') {
    const n = Number(env);
    if (Number.isFinite(n) && n >= 0) return n;
  }
  // 0 = watchdog off (callers like deps.ts/consume use config to pass 10m in prod).
  // Keeping direct adapter invocations fast in unit tests.
  return 0;
}

/**
 * Adapter over the Claude Code headless CLI:
 *   claude -p <prompt> --output-format json [--max-turns N]
 * stdout is a single JSON object with fields like `result` and `session_id`.
 *
 * Claude Code never retries a response dropped mid-stream; when that happens
 * the turn dies but the session transcript persists. This adapter resumes the
 * same session (`--resume <id> -p "Continue"`) with exponential backoff until
 * the turn completes, attempts are exhausted, or the error is auth/billing.
 */
export class ClaudeCodeAdapter implements WorkerAdapter {
  readonly name = 'claude-code' as const;

  constructor(
    private readonly sleep: (ms: number) => Promise<void> = (ms) =>
      new Promise((resolve) => setTimeout(resolve, ms)),
  ) {}

  async spawn(opts: WorkerSpawnOptions): Promise<WorkerResult> {
    const start = Date.now();
    const maxAttempts = opts.apiMaxAttempts ?? DEFAULT_API_MAX_ATTEMPTS;
    const noProgressTimeoutMs = resolveNoProgressTimeoutMs(opts.noProgressTimeoutMs);
    const wallDeadline = opts.timeoutMs > 0 ? start + opts.timeoutMs : Infinity;

    let args = baseArgs(opts);
    let sessionId: string | null = null;
    let last: SpawnCliResult | null = null;

    for (let attempt = 1; attempt <= maxAttempts; attempt++) {
      // Wall-clock budget: stop retrying when the run's overall timeout is spent.
      if (Date.now() >= wallDeadline) {
        if (last) last.timedOut = true;
        break;
      }
      const prepared = await prepareWorkerSpawn('claude', args, {
        cwd: opts.cwd,
        timeoutMs: opts.timeoutMs,
        ...(opts.env ? { env: opts.env } : {}),
        ...(noProgressTimeoutMs ? { noProgressTimeoutMs } : {}),
        ...(opts.watchdogLedger ? { watchdogLedger: opts.watchdogLedger } : {}),
      });
      last = await runWorkerCli(prepared.cmd, prepared.args, {
        ...prepared.opts,
        ...(noProgressTimeoutMs ? { noProgressTimeoutMs } : {}),
        ...(opts.herdr ? { herdr: true } : {}),
      });

      const outcome = interpret(last);
      if (outcome.sessionId) sessionId = outcome.sessionId;

      const ok = !last.timedOut && outcome.exitCode === 0 && !outcome.isError;
      if (ok) break;

      if (attempt === maxAttempts) break;
      if (last.exitCode === -1 && !last.timedOut) break; // spawn failure (ENOENT) — never retry forever
      if (outcome.errorText && isNonRetryableApiError(outcome.errorText)) break;

      // Transient detection: watchdog timeouts and provider errors (Console
      // Go, upstream, etc.) are retried forever. Without a session we retry
      // from scratch only for provider/timeout signals (not generic ECONNREFUSED).
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
      if (sessionId) {
        args = [
          '--resume',
          sessionId,
          '-p',
          RESUME_PROMPT,
          '--output-format',
          'json',
          ...(opts.maxSteps !== undefined ? ['--max-turns', String(opts.maxSteps)] : []),
        ];
      } else {
        args = baseArgs(opts);
      }
    }

    return finalize(last!, start);
  }
}

function baseArgs(opts: WorkerSpawnOptions): string[] {
  // claude-code ignores variant; model is the only knob.
  // Driver tier aliases ("coding", devagent.json model) are claude-proxy
  // selectors, not API-key model ids: `--model coding` fails with
  // "403 Combo "coding" is not allowed for this API key" (2026-09-01 live
  // loop 58: implement worker retried it forever because events=0 +
  // null resultText reads as a logic failure, not transient). Drop any
  // value without a "/" or known claude family prefix so claude falls back
  // to ~/.claude/settings.json model. Pass explicit provider ids through.
  const rawModel = opts.model?.trim();
  const model =
    rawModel && (/^claude-/.test(rawModel) || /^(opus|sonnet|haiku)(-|$)/.test(rawModel))
      ? rawModel.split('#')[0]!
      : undefined;
  return [
    '-p',
    opts.prompt,
    '--output-format',
    'json',
    ...(model ? ['--model', model] : []),
    ...(opts.maxSteps !== undefined ? ['--max-turns', String(opts.maxSteps)] : []),
  ];
}

interface RunOutcome {
  exitCode: number;
  isError: boolean;
  sessionId: string | null;
  errorText?: string;
  parsed: Record<string, unknown> | null;
}

export function interpretForTest(run: SpawnCliResult): RunOutcome {
  return interpret(run);
}

function interpret(run: SpawnCliResult): RunOutcome {
  let parsed: Record<string, unknown> | null = null;
  if (run.stdout.trim()) {
    try {
      const candidate: unknown = JSON.parse(run.stdout);
      if (Array.isArray(candidate)) {
        // Newer claude CLI: --output-format json emits the full event stream
        // as a JSON array; the terminal entry carries type:'result'
        // (live-smoke lesson: treating arrays as unparsable made every
        // planner call look empty).
        for (let i = candidate.length - 1; i >= 0; i--) {
          const e = candidate[i] as Record<string, unknown> | null;
          if (e !== null && typeof e === 'object' && !Array.isArray(e) && e.type === 'result') {
            parsed = e;
            break;
          }
        }
      } else if (candidate !== null && typeof candidate === 'object') {
        parsed = candidate as Record<string, unknown>;
      }
    } catch {
      parsed = null;
    }
  }
  const isError = parsed?.is_error === true;
  const rawResult = parsed?.result;
  const sessionId =
    parsed !== null && typeof parsed.session_id === 'string' ? parsed.session_id : null;
  return {
    exitCode: run.exitCode,
    isError: isError || (run.exitCode !== 0 && parsed === null),
    sessionId,
    errorText:
      typeof rawResult === 'string' && rawResult
        ? rawResult
        : run.stderr.trim()
          ? run.stderr.trim().split('\n').slice(-3).join('\n')
          : undefined,
    parsed,
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

  const outcome = interpret(run);
  if (!run.stdout.trim() || outcome.parsed === null) {
    return {
      exitCode: outcome.exitCode,
      events: [],
      resultText: null,
      sessionId: outcome.sessionId,
      durationMs: Date.now() - start,
      timedOut: false,
      // Stderr often carries the real failure reason (e.g.
      // [claude-code:unrecognized_model]) when the upstream returned an
      // empty stream. Surface it so the executor can classify properly.
      errorText: outcome.errorText ?? (run.stderr.trim() || undefined),
    };
  }

  const events: WorkerEvent[] = [{ type: 'result', ...outcome.parsed }];
  const resultText =
    outcome.exitCode === 0 && typeof outcome.parsed.result === 'string'
      ? outcome.parsed.result
      : null;

  return {
    exitCode: outcome.exitCode,
    events,
    resultText,
    sessionId: outcome.sessionId,
    durationMs: Date.now() - start,
    timedOut: false,
    errorText: outcome.errorText,
  };
}
