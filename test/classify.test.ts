import { describe, expect, it } from 'vitest';
import { isTransientProviderError } from '../src/resilience/classify.js';

describe('isTransientProviderError', () => {
  it('flags rate-limit and overload messages as transient', () => {
    expect(isTransientProviderError('429 too many requests')).toBe(true);
    expect(isTransientProviderError('service unavailable')).toBe(true);
    expect(isTransientProviderError('upstream request failed')).toBe(true);
  });

  it('flags omniroute [claude-code:unrecognized_model] as transient (loop-66 fix)', () => {
    // The omniroute proxy's single command-code account periodically empties
    // upstream streams on rate limit and surfaces them as
    // [claude-code:unrecognized_model] on stderr. Without these patterns
    // the executor's 2-attempt logic-attempts cap marks every task as
    // failed during the outage (1h+ of false-failed workers in loop-66).
    expect(isTransientProviderError('[claude-code:unrecognized_model] {"model":"cmd/minimax/minimax-m3-free","query_source":"sdk"}')).toBe(true);
    expect(isTransientProviderError('Empty stream at flush')).toBe(true);
    expect(isTransientProviderError('Claude returned an empty response (no content block)')).toBe(true);
  });

  it('still flags hard network errors as transient', () => {
    expect(isTransientProviderError('connect ECONNREFUSED 127.0.0.1:20128')).toBe(true);
    expect(isTransientProviderError('ETIMEDOUT')).toBe(true);
    expect(isTransientProviderError('fetch failed')).toBe(true);
  });

  it('returns false for unrelated content', () => {
    expect(isTransientProviderError('Result: I implemented the feature as requested')).toBe(false);
    expect(isTransientProviderError('test passed: 1/1')).toBe(false);
  });

  it('returns false for null/empty input', () => {
    expect(isTransientProviderError(null)).toBe(false);
    expect(isTransientProviderError(undefined)).toBe(false);
    expect(isTransientProviderError('')).toBe(false);
  });
});
