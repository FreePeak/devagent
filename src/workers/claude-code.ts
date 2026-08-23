import type { WorkerAdapter, WorkerEvent, WorkerResult, WorkerSpawnOptions } from '../types.js';
import { spawnCli, type SpawnCliResult } from './spawn-utils.js';
import { backoffDelay } from '../sessionguard/backoff.js';
import { isNonRetryableApiError } from '../sessionguard/events.js';

const RESUME_PROMPT = 'Continue';
const DEFAULT_API_MAX_ATTEMPTS = 3;

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

    let args = baseArgs(opts);
    let sessionId: string | null = null;
    let last: SpawnCliResult | null = null;

    for (let attempt = 1; attempt <= maxAttempts; attempt++) {
      last = await spawnCli('claude', args, {
        cwd: opts.cwd,
        timeoutMs: opts.timeoutMs,
        ...(opts.env ? { env: opts.env } : {}),
      });

      const outcome = interpret(last);
      if (outcome.sessionId) sessionId = outcome.sessionId;

      const ok =
        !last.timedOut &&
        outcome.exitCode === 0 &&
        !outcome.isError;
      if (ok || last.timedOut) break;

      if (
        !sessionId ||
        attempt === maxAttempts ||
        (outcome.errorText !== undefined && isNonRetryableApiError(outcome.errorText))
      ) {
        break;
      }

      await this.sleep(backoffDelay(attempt));
      args = [
        '--resume',
        sessionId,
        '-p',
        RESUME_PROMPT,
        '--output-format',
        'json',
        ...(opts.maxSteps !== undefined ? ['--max-turns', String(opts.maxSteps)] : []),
      ];
    }

    return finalize(last!, start);
  }
}

function baseArgs(opts: WorkerSpawnOptions): string[] {
  return [
    '-p',
    opts.prompt,
    '--output-format',
    'json',
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

function interpret(run: SpawnCliResult): RunOutcome {
  let parsed: Record<string, unknown> | null = null;
  if (run.stdout.trim()) {
    try {
      const candidate: unknown = JSON.parse(run.stdout);
      if (candidate !== null && typeof candidate === 'object' && !Array.isArray(candidate)) {
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
  };
}
