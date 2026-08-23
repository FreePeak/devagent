/**
 * cc-guard: supervisor for headless Claude Code sessions.
 *
 * Claude Code never retries a response that died mid-stream ("Connection
 * lost mid-response") because partial output is already committed to the
 * transcript. The supported recovery is starting a new turn in the same
 * persisted session (`claude --resume <id> -p ...`). This module automates
 * exactly that: run the command, watch the structured stream-json output,
 * and on terminal API failure re-launch against the same session id with
 * exponential backoff until the turn completes or attempts are exhausted.
 */

import { backoffDelay, DEFAULT_BACKOFF, type BackoffOptions } from './backoff.js';
import {
  isNonRetryableApiError,
  parseStreamLine,
  type StreamEvent,
} from './events.js';

export interface AttemptResult {
  exitCode: number | null;
  timedOut: boolean;
  sessionId?: string;
  resultIsError: boolean;
  sawResult: boolean;
  syntheticErrorText?: string;
}

export interface LineHandler {
  onLine(line: string, stream: 'stdout' | 'stderr'): void;
}

export interface SpawnOpts {
  noProgressTimeoutMs: number;
  env?: NodeJS.ProcessEnv;
}

/** Runs one launch of the claude CLI to completion. */
export type AttemptRunner = (
  argv: string[],
  handler: LineHandler,
  opts: SpawnOpts,
) => Promise<AttemptResult>;

export interface GuardOptions {
  /** Full invocation, e.g. ['claude', '-p', 'do the thing']. */
  argv: string[];
  /** Prompt used when resuming an interrupted session. */
  resumePrompt?: string;
  /** Total launches allowed, including the first. */
  maxAttempts?: number;
  backoff?: Partial<BackoffOptions>;
  /** Kill + resume when the child emits nothing for this long. 0 disables. */
  noProgressTimeoutMs?: number;
  env?: NodeJS.ProcessEnv;
  log?: (message: string) => void;
  onLine?: (line: string, stream: 'stdout' | 'stderr') => void;
  runner?: AttemptRunner;
  sleep?: (ms: number) => Promise<void>;
  random?: () => number;
}

export interface GuardResult {
  ok: boolean;
  attempts: number;
  resumed: number;
  sessionId?: string;
  reason?: 'attempts_exhausted' | 'non_retryable_error' | 'no_session_id';
  lastError?: string;
}

export function buildResumeArgv(
  argv: string[],
  sessionId: string,
  resumePrompt: string,
): string[] {
  const out = [...argv];
  for (let i = 0; i < out.length; i++) {
    const token = out[i];
    if ((token === '-p' || token === '--print') && i + 1 < out.length) {
      const next = out[i + 1];
      if (next !== undefined && !next.startsWith('-')) {
        out.splice(i, 2);
        break;
      }
    }
  }
  out.push('--resume', sessionId, '-p', resumePrompt);
  return out;
}

const sleepDefault = (ms: number) =>
  new Promise<void>((resolve) => setTimeout(resolve, ms));

export async function runGuard(options: GuardOptions): Promise<GuardResult> {
  const maxAttempts = options.maxAttempts ?? 5;
  const resumePrompt = options.resumePrompt ?? 'Continue';
  const backoff: BackoffOptions = { ...DEFAULT_BACKOFF, ...options.backoff };
  const runner = options.runner;
  if (!runner) throw new Error('runGuard requires a runner (see spawnClaude)');
  const sleep = options.sleep ?? sleepDefault;
  const random = options.random ?? Math.random;
  const log = options.log ?? (() => {});

  let sessionId: string | undefined;
  let lastError: string | undefined;

  for (let attempt = 1; attempt <= maxAttempts; attempt++) {
    const isFirst = attempt === 1 || sessionId === undefined;
    if (!isFirst && !sessionId) {
      return {
        ok: false,
        attempts: attempt - 1,
        resumed: attempt - 2,
        reason: 'no_session_id',
        lastError,
      };
    }
    const argv =
      isFirst || !sessionId
        ? options.argv
        : buildResumeArgv(options.argv, sessionId, resumePrompt);

    const forwarded: LineHandler = {
      onLine: (line, stream) => options.onLine?.(line, stream),
    };
    const outcome = await runner(argv, forwarded, {
      noProgressTimeoutMs: options.noProgressTimeoutMs ?? 0,
      env: options.env,
    });

    if (!sessionId && outcome.sessionId) sessionId = outcome.sessionId;
    if (outcome.syntheticErrorText) lastError = outcome.syntheticErrorText;
    else if (outcome.timedOut) lastError = 'no-progress watchdog timeout';

    const ok = outcome.exitCode === 0 && !outcome.resultIsError;
    if (ok) {
      return { ok: true, attempts: attempt, resumed: attempt - 1, sessionId };
    }

    const errorText = outcome.syntheticErrorText ?? '';
    if (errorText && isNonRetryableApiError(errorText)) {
      return {
        ok: false,
        attempts: attempt,
        resumed: attempt - 1,
        sessionId,
        reason: 'non_retryable_error',
        lastError: errorText,
      };
    }
    if (attempt === maxAttempts) break;

    const delay = backoffDelay(attempt, backoff, random);
    log(
      `[cc-guard] attempt ${attempt}/${maxAttempts} failed (${
        outcome.timedOut
          ? 'watchdog'
          : outcome.resultIsError
            ? 'api error'
            : `exit ${outcome.exitCode}`
      }); resuming session ${sessionId ?? '?'} in ${delay}ms`,
    );
    await sleep(delay);
  }

  return {
    ok: false,
    attempts: maxAttempts,
    resumed: maxAttempts - 1,
    sessionId,
    reason: 'attempts_exhausted',
    lastError,
  };
}

export type { StreamEvent };
export { parseStreamLine };
