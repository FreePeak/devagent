/**
 * Classification of Claude Code headless stream-json output.
 *
 * Reference: https://code.claude.com/docs/en/headless.md and empirical
 * inspection of v2.1.x output. Terminal API failures are never retried by
 * Claude Code once a response started streaming ("Connection lost
 * mid-response"); recovery requires starting a new turn in the same session,
 * which is what the guard supervisor does.
 */

export interface InitEvent {
  kind: 'init';
  sessionId: string;
}

export interface ApiRetryEvent {
  kind: 'api_retry';
  attempt: number;
  maxRetries?: number;
  delayMs?: number;
  errorStatus: number | null;
  error: string;
}

export interface ResultEvent {
  kind: 'result';
  isError: boolean;
  sessionId?: string;
}

export interface SyntheticErrorEvent {
  kind: 'synthetic_error';
  text: string;
}

export type StreamEvent =
  | InitEvent
  | ApiRetryEvent
  | ResultEvent
  | SyntheticErrorEvent
  | { kind: 'other' };

interface RawStreamLine {
  type?: string;
  subtype?: string;
  session_id?: string;
  sessionId?: string;
  attempt?: number;
  max_retries?: number;
  retry_delay_ms?: number;
  error_status?: number | null;
  error?: string;
  is_error?: boolean;
  message?: { model?: string; content?: unknown };
  isApiErrorMessage?: boolean;
}

/** Errors that will never succeed on retry; abort instead of burning attempts. */
const NON_RETRYABLE_PATTERNS: RegExp[] = [
  /invalid api key/i,
  /authentication/i,
  /unauthorized/i,
  /credit balance/i,
  /billing/i,
  /not found.*model|model.*not found/i,
];

export function isNonRetryableApiError(text: string): boolean {
  return NON_RETRYABLE_PATTERNS.some((p) => p.test(text));
}

export function parseStreamLine(line: string): StreamEvent {
  const trimmed = line.trim();
  if (!trimmed.startsWith('{')) return { kind: 'other' };
  let raw: RawStreamLine;
  try {
    raw = JSON.parse(trimmed) as RawStreamLine;
  } catch {
    return { kind: 'other' };
  }
  if (raw.type === 'system' && raw.subtype === 'init') {
    const sessionId = raw.session_id ?? raw.sessionId;
    if (sessionId) return { kind: 'init', sessionId };
    return { kind: 'other' };
  }
  if (raw.type === 'system' && raw.subtype === 'api_retry') {
    return {
      kind: 'api_retry',
      attempt: raw.attempt ?? 0,
      maxRetries: raw.max_retries,
      delayMs: raw.retry_delay_ms,
      errorStatus: raw.error_status ?? null,
      error: raw.error ?? 'unknown',
    };
  }
  if (
    raw.type === 'assistant' &&
    (raw.isApiErrorMessage === true || raw.message?.model === '<synthetic>')
  ) {
    return { kind: 'synthetic_error', text: syntheticText(raw.message?.content) };
  }
  if (raw.type === 'result') {
    return {
      kind: 'result',
      isError: raw.is_error === true,
      sessionId: raw.session_id ?? raw.sessionId,
    };
  }
  return { kind: 'other' };
}

function syntheticText(content: unknown): string {
  if (typeof content === 'string') return content;
  if (Array.isArray(content)) {
    return content
      .map((block) =>
        typeof block === 'object' && block !== null && 'text' in block
          ? String((block as { text: unknown }).text)
          : '',
      )
      .filter(Boolean)
      .join(' ');
  }
  return '';
}
