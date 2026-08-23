export interface BackoffOptions {
  baseDelayMs: number;
  maxDelayMs: number;
  factor: number;
}

export const DEFAULT_BACKOFF: BackoffOptions = {
  baseDelayMs: 2_000,
  maxDelayMs: 60_000,
  factor: 2,
};

/**
 * Exponential backoff with +/-25% jitter so parallel guards do not sync up.
 * Attempt is 1-based; attempt 1 waits roughly baseDelayMs.
 */
export function backoffDelay(
  attempt: number,
  options: BackoffOptions = DEFAULT_BACKOFF,
  random: () => number = Math.random,
): number {
  const capped = Math.max(1, attempt);
  const raw = Math.min(
    options.maxDelayMs,
    options.baseDelayMs * Math.pow(options.factor, capped - 1),
  );
  const jitter = 1 + (random() * 0.5 - 0.25);
  return Math.max(0, Math.round(raw * jitter));
}
