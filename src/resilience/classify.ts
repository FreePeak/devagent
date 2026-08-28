import { isNonRetryableApiError } from '../sessionguard/events.js';

/**
 * Transient provider errors that are safe to retry forever.
 * Covers: Console Go endpoint, upstream failures, rate/overload, network,
 * timeouts. Non-retryable auth/billing is excluded via isNonRetryableApiError.
 */
const TRANSIENT_PATTERNS: RegExp[] = [
  /endpoint is unavailable/i,
  /upstream request failed/i,
  /error from provider/i,
  /provider.*unavailable/i,
  /connection lost/i,
  /connection refused/i,
  /ETIMEDOUT/i,
  /ECONNRESET/i,
  /ECONNREFUSED/i,
  /ENOTFOUND/i,
  /fetch failed/i,
  /network error/i,
  /overloaded/i,
  /rate limit/i,
  /too many requests/i,
  /429|529/,
  /timeout/i,
  /timed out/i,
  /unavailable/i,
  /service unavailable/i,
  /bad gateway/i,
  /gateway timeout/i,
  /socket hang up/i,
  // omniroute proxy surfaces rate-limited empty upstream streams as
  // "[claude-code:unrecognized_model]" on stderr and a JSON array with no
  // .result field. The proxy's own log shows "all 1 active accounts rate
  // limited" — the only fix is to retry once the upstream cooldowns. Loop
  // stalls without this (loop-66 incident: 1h+ of false-failed workers).
  /unrecognized_model/i,
  /\[claude-code:unrecognized_model\]/,
  /empty stream/i,
  /empty response/i,
];

export function isTransientProviderError(text: string | undefined | null): boolean {
  if (!text) return false;
  if (isNonRetryableApiError(text)) return false;
  return TRANSIENT_PATTERNS.some((p) => p.test(text));
}

/** Patterns that justify a retry even when no session id was emitted. */
const SESSIONLESS_TRANSIENT: RegExp[] = [
  /endpoint is unavailable/i,
  /upstream request failed/i,
  /error from provider/i,
  /provider.*unavailable/i,
  /overloaded/i,
  /rate limit/i,
  /too many requests/i,
  /429|529/,
  /unavailable/i,
  /service unavailable/i,
  /timeout/i,
  /timed out/i,
];

function isSessionlessTransient(text: string): boolean {
  if (isNonRetryableApiError(text)) return false;
  return SESSIONLESS_TRANSIENT.some((p) => p.test(text));
}

/** True when the failure should be retried from scratch (no session). */
export function isRetryableWithoutSession(opts: {
  timedOut?: boolean;
  exitCode?: number;
  errorText?: string;
  stderr?: string;
}): boolean {
  if (opts.timedOut) return true; // watchdog/timeout is transient by default
  const text = opts.errorText ?? opts.stderr ?? '';
  if (!text.trim()) return false;
  return isSessionlessTransient(text);
}

export { isNonRetryableApiError };
