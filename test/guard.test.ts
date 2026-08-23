import { afterEach, describe, expect, it } from 'vitest';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { execPath } from 'node:process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  isNonRetryableApiError,
  parseStreamLine,
} from '../src/sessionguard/events.js';
import { backoffDelay, DEFAULT_BACKOFF } from '../src/sessionguard/backoff.js';
import {
  buildResumeArgv,
  runGuard,
  type AttemptResult,
  type AttemptRunner,
  type LineHandler,
} from '../src/sessionguard/guard.js';
import { spawnClaude } from '../src/sessionguard/spawn.js';

describe('parseStreamLine', () => {
  it('classifies init events with session id', () => {
    const event = parseStreamLine(
      '{"type":"system","subtype":"init","session_id":"abc-123"}',
    );
    expect(event).toEqual({ kind: 'init', sessionId: 'abc-123' });
  });

  it('classifies api_retry events', () => {
    const event = parseStreamLine(
      '{"type":"system","subtype":"api_retry","attempt":2,"max_retries":10,"retry_delay_ms":1247,"error_status":null,"error":"unknown"}',
    );
    expect(event).toMatchObject({ kind: 'api_retry', attempt: 2, delayMs: 1247 });
  });

  it('classifies synthetic assistant errors from mid-stream drops', () => {
    const line =
      '{"type":"assistant","isApiErrorMessage":true,"message":{"model":"<synthetic>","content":[{"type":"text","text":"API Error: Connection lost mid-response."}]}}';
    expect(parseStreamLine(line)).toEqual({
      kind: 'synthetic_error',
      text: 'API Error: Connection lost mid-response.',
    });
  });

  it('classifies result events including is_error', () => {
    const event = parseStreamLine(
      '{"type":"result","is_error":true,"session_id":"s1"}',
    );
    expect(event).toEqual({ kind: 'result', isError: true, sessionId: 's1' });
  });

  it('returns other for malformed or unrelated lines', () => {
    expect(parseStreamLine('not json').kind).toBe('other');
    expect(parseStreamLine('{"type":"assistant"}').kind).toBe('other');
  });

  it('flags non-retryable auth/billing failures', () => {
    expect(isNonRetryableApiError('Invalid API key provided')).toBe(true);
    expect(isNonRetryableApiError('your credit balance is too low')).toBe(true);
    expect(isNonRetryableApiError('API Error: Connection lost mid-response')).toBe(
      false,
    );
  });
});

describe('buildResumeArgv', () => {
  it('replaces the original prompt with a resume invocation', () => {
    const argv = buildResumeArgv(
      ['claude', '-p', 'do the thing', '--permission-mode', 'bypassPermissions'],
      'sess-9',
      'Continue',
    );
    expect(argv).toEqual([
      'claude',
      '--permission-mode',
      'bypassPermissions',
      '--resume',
      'sess-9',
      '-p',
      'Continue',
    ]);
  });

  it('handles --print long form and preserves other flags', () => {
    const argv = buildResumeArgv(['claude', '--print', 'task'], 's', 'go');
    expect(argv).toEqual(['claude', '--resume', 's', '-p', 'go']);
  });

  it('replaces a prior --resume id when re-resuming', () => {
    const argv = buildResumeArgv(
      ['claude', '--resume', 'old-s', '-p', 'Continue'],
      'new-s',
      'Continue',
    );
    expect(argv).toEqual(['claude', '--resume', 'new-s', '-p', 'Continue']);
  });
});

describe('backoffDelay', () => {
  it('grows exponentially up to the ceiling', () => {
    const fixed = () => 0.5; // zero jitter
    expect(backoffDelay(1, DEFAULT_BACKOFF, fixed)).toBe(2_000);
    expect(backoffDelay(3, DEFAULT_BACKOFF, fixed)).toBe(8_000);
    expect(backoffDelay(10, DEFAULT_BACKOFF, fixed)).toBe(60_000);
  });

  it('applies bounded jitter', () => {
    const value = backoffDelay(1, { baseDelayMs: 1000, maxDelayMs: 8000, factor: 2 }, () => 0);
    expect(value).toBeGreaterThanOrEqual(750);
    expect(value).toBeLessThanOrEqual(1250);
  });
});

function fakeRunner(script: AttemptResult[]): AttemptRunner {
  let call = 0;
  return async (argv) => {
    const outcome = script[Math.min(call, script.length - 1)]!;
    call++;
    void argv;
    return outcome;
  };
}

const noLines: LineHandler = { onLine: () => {} };

describe('runGuard', () => {
  it('resumes the same session id until success', async () => {
    const seenArgv: string[][] = [];
    let call = 0;
    const runner: AttemptRunner = async (argv) => {
      seenArgv.push([...argv]);
      call++;
      if (call < 3) {
        return {
          exitCode: 1,
          timedOut: false,
          sessionId: 'sess-7',
          resultIsError: false,
          sawResult: true,
          syntheticErrorText: 'API Error: Connection lost mid-response.',
        };
      }
      return {
        exitCode: 0,
        timedOut: false,
        sessionId: 'sess-7',
        resultIsError: false,
        sawResult: true,
      };
    };
    const sleeps: number[] = [];
    const result = await runGuard({
      argv: ['claude', '-p', 'ship it'],
      runner,
      maxAttempts: 5,
      sleep: async (ms) => sleeps.push(ms),
      random: () => 0.5,
    });
    expect(result).toMatchObject({
      ok: true,
      attempts: 3,
      resumed: 2,
      sessionId: 'sess-7',
    });
    expect(seenArgv[1]).toEqual([
      'claude',
      '--resume',
      'sess-7',
      '-p',
      'Continue',
    ]);
    expect(sleeps.length).toBe(2);
  });

  it('aborts immediately on non-retryable errors', async () => {
    const result = await runGuard({
      argv: ['claude', '-p', 'x'],
      runner: fakeRunner([
        {
          exitCode: 1,
          timedOut: false,
          sessionId: 's',
          resultIsError: true,
          sawResult: true,
          syntheticErrorText: 'Invalid API key provided',
        },
      ]),
      maxAttempts: 5,
      sleep: async () => {},
    });
    expect(result.ok).toBe(false);
    expect(result.reason).toBe('non_retryable_error');
    expect(result.attempts).toBe(1);
  });

  it('exhausts attempts and reports failure', async () => {
    const failing: AttemptResult = {
      exitCode: 1,
      timedOut: false,
      sessionId: 's',
      resultIsError: false,
      sawResult: false,
    };
    const result = await runGuard({
      argv: ['claude', '-p', 'x'],
      runner: fakeRunner([failing]),
      maxAttempts: 3,
      sleep: async () => {},
    });
    expect(result).toMatchObject({
      ok: false,
      attempts: 3,
      resumed: 2,
      reason: 'attempts_exhausted',
    });
  });
});

describe('spawnClaude integration', () => {
  let dir: string;

  afterEach(() => {
    if (dir) rmSync(dir, { recursive: true, force: true });
  });

  it('parses a real child process emitting stream-json then failing', async () => {
    dir = mkdtempSync(join(tmpdir(), 'cc-guard-'));
    const stub = join(dir, 'stub.mjs');
    writeFileSync(
      stub,
      `const lines = [
        '{"type":"system","subtype":"init","session_id":"stub-1"}',
        '{"type":"system","subtype":"api_retry","attempt":1,"max_retries":2,"retry_delay_ms":10,"error_status":null,"error":"unknown"}',
        '{"type":"assistant","isApiErrorMessage":true,"message":{"model":"<synthetic>","content":[{"type":"text","text":"API Error: Connection refused"}]}}',
      ];
      for (const l of lines) console.log(l);
      process.exit(1);`,
    );
    const lines: string[] = [];
    const outcome = await spawnClaude(
      [execPath, stub],
      { onLine: (line) => lines.push(line) },
      { noProgressTimeoutMs: 0 },
    );
    expect(outcome.sessionId).toBe('stub-1');
    expect(outcome.syntheticErrorText).toContain('Connection refused');
    expect(outcome.exitCode).toBe(1);
    expect(lines).toHaveLength(3);
  });

  it('kills a silent child when the watchdog fires', async () => {
    dir = mkdtempSync(join(tmpdir(), 'cc-guard-'));
    const stub = join(dir, 'silent.mjs');
    writeFileSync(stub, `setTimeout(() => {}, 60_000);`);
    const outcome = await spawnClaude(
      [execPath, stub],
      noLines,
      { noProgressTimeoutMs: 500 },
    );
    expect(outcome.timedOut).toBe(true);
    expect(outcome.exitCode).not.toBe(0);
  });
});
