import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// Controllable fake for execFile. Tests set `nextResult` before calling spawn.
type ExecFileCallback = (
  error: (Error & { code?: number | string }) | null,
  stdout: string,
  stderr: string,
) => void;

let nextResult: {
  error: (Error & { code?: number | string }) | null;
  stdout: string;
  stderr: string;
  delayMs?: number;
} = { error: null, stdout: '', stderr: '' };

/** Optional FIFO of per-call results; consumed before falling back to nextResult. */
let resultQueue: typeof nextResult[] = [];

const execFileMock = vi.fn(
  (
    _cmd: string,
    _args: string[],
    opts: { signal?: AbortSignal },
    cb: ExecFileCallback,
  ) => {
    const result = resultQueue.length > 0 ? resultQueue.shift()! : nextResult;
    if (opts?.signal) {
      // Simulate a real child: abort kills the process and surfaces an error.
      opts.signal.addEventListener('abort', () => {
        setImmediate(() =>
          cb(Object.assign(new Error('This operation was aborted'), { code: 'ABORT_ERR', killed: true } as never), '', ''),
        );
      });
    }
    if (result.delayMs !== undefined) {
      const timer = setTimeout(() => cb(result.error, result.stdout, result.stderr), result.delayMs);
      timer.unref?.();
    } else {
      setImmediate(() => cb(result.error, result.stdout, result.stderr));
    }
    return undefined as never;
  },
);

vi.mock('node:child_process', () => ({
  execFile: (...args: unknown[]) => (execFileMock as unknown)(...(args as [])),
}));

// Watchdog routing depends on DEVAGENT_NO_PROGRESS_TIMEOUT_MS. When the
// ambient environment (selfbuild driver) exports it, adapters resolve a
// positive default and route through spawnCliStreaming -> spawn, which this
// execFile-only mock does not define. Pin the env so routing is deterministic
// (watchdog off -> execFile path) regardless of the operator's shell.
beforeEach(() => {
  delete process.env.DEVAGENT_NO_PROGRESS_TIMEOUT_MS;
});

import { ClaudeCodeAdapter } from '../src/workers/claude-code.js';
import { OpenCodeAdapter } from '../src/workers/opencode.js';
import { getWorker, workers } from '../src/workers/index.js';

const baseOpts = { cwd: '/tmp/repo', timeoutMs: 5_000 };

beforeEach(() => {
  // Hermetic vs the dispatching loop: selfbuild-loop.sh exports
  // DEVAGENT_NO_PROGRESS_TIMEOUT_MS=600000, which arms the watchdog default
  // and routes spawnCli through spawnCliStreaming — a code path these mocks
  // (execFile only) do not cover. Watchdog-arm behavior is covered in
  // watchdog-health.test.ts with real timers.
  delete process.env.DEVAGENT_NO_PROGRESS_TIMEOUT_MS;
  nextResult = { error: null, stdout: '', stderr: '' };
  resultQueue = [];
});

afterEach(() => {
  vi.clearAllMocks();
});

function ok(stdout: string) {
  nextResult = { error: null, stdout, stderr: '' };
}

describe('claude-code adapter', () => {
  const adapter = new ClaudeCodeAdapter();

  it('builds claude -p args with json output format', async () => {
    ok('{}');
    await adapter.spawn({ ...baseOpts, prompt: 'fix the bug' });
    expect(execFileMock).toHaveBeenCalledWith(
      'claude',
      ['-p', 'fix the bug', '--output-format', 'json'],
      expect.anything(),
      expect.any(Function),
    );
  });

  it('adds --max-turns when maxSteps is given', async () => {
    ok('{}');
    await adapter.spawn({ ...baseOpts, prompt: 'p', maxSteps: 7 });
    expect(execFileMock).toHaveBeenCalledWith(
      'claude',
      ['-p', 'p', '--output-format', 'json', '--max-turns', '7'],
      expect.anything(),
      expect.any(Function),
    );
  });

  it('parses happy-path JSON into resultText and sessionId', async () => {
    ok(JSON.stringify({ result: 'done editing', session_id: 'sess-123' }));
    const res = await adapter.spawn({ ...baseOpts, prompt: 'x' });
    expect(res.exitCode).toBe(0);
    expect(res.resultText).toBe('done editing');
    expect(res.sessionId).toBe('sess-123');
    expect(res.timedOut).toBe(false);
    expect(res.events).toEqual([
      { type: 'result', result: 'done editing', session_id: 'sess-123' },
    ]);
  });

  it('falls back to resultText null on invalid JSON but keeps exitCode', async () => {
    nextResult = { error: Object.assign(new Error('boom'), { code: 3 }), stdout: 'not json', stderr: '' };
    const res = await adapter.spawn({ ...baseOpts, prompt: 'x' });
    expect(res.exitCode).toBe(3);
    expect(res.resultText).toBeNull();
    expect(res.sessionId).toBeNull();
    expect(res.events).toEqual([]);
  });
});

describe('claude-code adapter API-failure resume', () => {
  const noSleep = async () => {};
  const adapter = new ClaudeCodeAdapter(noSleep);

  it('resumes the same session after a mid-stream API failure and succeeds', async () => {
    resultQueue = [
      {
        error: Object.assign(new Error('boom'), { code: 1 }),
        stdout: JSON.stringify({
          is_error: true,
          result: 'API Error: Connection lost mid-response.',
          session_id: 'sess-9',
        }),
        stderr: '',
      },
      { error: null, stdout: JSON.stringify({ result: 'done after resume', session_id: 'sess-9' }), stderr: '' },
    ];
    const res = await adapter.spawn({ ...baseOpts, prompt: 'ship it' });
    expect(execFileMock).toHaveBeenCalledTimes(2);
    const resumeCall = (execFileMock.mock.calls[1] as unknown[])[1] as string[];
    expect(resumeCall.slice(0, 4)).toEqual(['--resume', 'sess-9', '-p', 'Continue']);
    expect(res.exitCode).toBe(0);
    expect(res.resultText).toBe('done after resume');
    expect(res.sessionId).toBe('sess-9');
  });

  it('does not resume when the failure carries no session id', async () => {
    resultQueue = [
      {
        error: Object.assign(new Error('boom'), { code: 1 }),
        stdout: '',
        stderr: 'connect ECONNREFUSED',
      },
      { error: null, stdout: '', stderr: '' },
    ];
    const res = await adapter.spawn({ ...baseOpts, prompt: 'x' });
    expect(execFileMock).toHaveBeenCalledTimes(1);
    expect(res.exitCode).toBe(1);
  });

  it('aborts immediately on non-retryable auth errors', async () => {
    resultQueue = [
      {
        error: Object.assign(new Error('boom'), { code: 1 }),
        stdout: JSON.stringify({
          is_error: true,
          result: 'Invalid API key provided',
          session_id: 'sess-2',
        }),
        stderr: '',
      },
      { error: null, stdout: '', stderr: '' },
    ];
    await adapter.spawn({ ...baseOpts, prompt: 'x' });
    expect(execFileMock).toHaveBeenCalledTimes(1);
  });

  it('respects apiMaxAttempts and stops after exhausting them', async () => {
    const fail = {
      error: Object.assign(new Error('boom'), { code: 1 }),
      stdout: JSON.stringify({
        is_error: true,
        result: 'API Error: Connection refused',
        session_id: 's1',
      }),
      stderr: '',
    };
    resultQueue = [fail, fail, fail, fail];
    const res = await adapter.spawn({ ...baseOpts, prompt: 'x', apiMaxAttempts: 3 });
    expect(execFileMock).toHaveBeenCalledTimes(3);
    expect(res.exitCode).toBe(1);
  });
});

describe('opencode adapter', () => {
  const adapter = new OpenCodeAdapter();

  it('builds opencode run args with prompt last', async () => {
    ok('');
    await adapter.spawn({ ...baseOpts, prompt: 'write tests' });
    expect(execFileMock).toHaveBeenCalledWith(
      'opencode',
      ['run', '--format', 'json', 'write tests'],
      expect.anything(),
      expect.any(Function),
    );
  });

  it('parses NDJSON multi-event output, skipping garbage lines', async () => {
    ok(
      [
        'not-json-garbage',
        JSON.stringify({ type: 'step_start', part: 'thinking' }),
        '',
        '<<<noise>>>',
        JSON.stringify({ type: 'text', text: 'final answer', sessionID: 'oc-99' }),
        '{broken',
      ].join('\n'),
    );
    const res = await adapter.spawn({ ...baseOpts, prompt: 'x' });
    expect(res.exitCode).toBe(0);
    expect(res.events.map((e) => e.type)).toEqual(['step_start', 'text']);
    expect(res.resultText).toBe('final answer');
    expect(res.sessionId).toBe('oc-99');
  });

  it('extracts session_id (snake_case) as well as sessionID', async () => {
    ok(JSON.stringify({ session_id: 'sid-1' }) + '\n' + JSON.stringify({ text: 'hi' }));
    const res = await adapter.spawn({ ...baseOpts, prompt: 'x' });
    expect(res.sessionId).toBe('sid-1');
    expect(res.resultText).toBe('hi');
  });

  it('returns resultText null when no event carries text/part', async () => {
    ok(JSON.stringify({ type: 'step_start' }));
    const res = await adapter.spawn({ ...baseOpts, prompt: 'x' });
    expect(res.resultText).toBeNull();
    expect(res.events.length).toBe(1);
  });

  it('retries on provider error with session resume and eventually succeeds (infinite retry)', async () => {
    const noSleep = async () => {};
    const retryAdapter = new OpenCodeAdapter(noSleep);
    resultQueue = [
      {
        error: Object.assign(new Error('boom'), { code: 1 }),
        stdout: JSON.stringify({
          type: 'error',
          error: { message: 'Error from provider (Console Go): Upstream request failed: Endpoint is unavailable.', status: 503 },
          sessionID: 'ses_retry_1',
        }),
        stderr: '',
      },
      {
        error: Object.assign(new Error('boom'), { code: 1 }),
        stdout: JSON.stringify({
          type: 'error',
          error: { message: 'Error from provider (Console Go): Upstream request failed: Endpoint is unavailable.', status: 503 },
          sessionID: 'ses_retry_1',
        }),
        stderr: '',
      },
      {
        error: null,
        stdout: JSON.stringify({ type: 'text', text: 'done after retry', sessionID: 'ses_retry_1' }),
        stderr: '',
      },
    ];
    const res = await retryAdapter.spawn({ ...baseOpts, prompt: 'ship it' });
    expect(execFileMock).toHaveBeenCalledTimes(3);
    const resumeCall = (execFileMock.mock.calls[1] as unknown[])[1] as string[];
    expect(resumeCall).toEqual(['run', '--format', 'json', '--session', 'ses_retry_1', 'Continue']);
    expect(res.exitCode).toBe(0);
    expect(res.resultText).toBe('done after retry');
    expect(res.sessionId).toBe('ses_retry_1');
  });

  it('aborts immediately on non-retryable auth errors', async () => {
    const noSleep = async () => {};
    const retryAdapter = new OpenCodeAdapter(noSleep);
    resultQueue = [
      {
        error: Object.assign(new Error('boom'), { code: 1 }),
        stdout: JSON.stringify({
          type: 'error',
          error: { message: 'Invalid API key provided' },
          sessionID: 'ses_auth',
        }),
        stderr: '',
      },
      { error: null, stdout: JSON.stringify({ type: 'text', text: 'should not reach', sessionID: 'ses_auth' }), stderr: '' },
    ];
    await retryAdapter.spawn({ ...baseOpts, prompt: 'x' });
    expect(execFileMock).toHaveBeenCalledTimes(1);
  });

  it('re-detects opencode Console Go error via exitCode non-zero even without error event', async () => {
    const noSleep = async () => {};
    const retryAdapter = new OpenCodeAdapter(noSleep);
    resultQueue = [
      {
        error: Object.assign(new Error('boom'), { code: 1 }),
        stdout: JSON.stringify({ type: 'step_start', sessionID: 'ses_x' }),
        stderr: 'Error from provider (Console Go): Upstream request failed: Endpoint is unavailable.',
      },
      {
        error: null,
        stdout: JSON.stringify({ type: 'text', text: 'recovered', sessionID: 'ses_x' }),
        stderr: '',
      },
    ];
    const res = await retryAdapter.spawn({ ...baseOpts, prompt: 'x' });
    expect(execFileMock).toHaveBeenCalledTimes(2);
    expect(res.exitCode).toBe(0);
    expect(res.resultText).toBe('recovered');
  });

  it('retries many times to prove Infinity default (not just 3)', async () => {
    const noSleep = async () => {};
    const retryAdapter = new OpenCodeAdapter(noSleep);
    const fail = {
      error: Object.assign(new Error('boom'), { code: 1 }),
      stdout: JSON.stringify({
        type: 'error',
        error: { message: 'Error from provider: transient' },
        sessionID: 'ses_long',
      }),
      stderr: '',
    };
    resultQueue = [fail, fail, fail, fail, fail, { error: null, stdout: JSON.stringify({ type: 'text', text: 'finally', sessionID: 'ses_long' }), stderr: '' }];
    const res = await retryAdapter.spawn({ ...baseOpts, prompt: 'x' });
    expect(execFileMock).toHaveBeenCalledTimes(6);
    expect(res.resultText).toBe('finally');
  });
});

describe('timeout handling', () => {
  it('claude-code returns timedOut result with exitCode -1 and no events', async () => {
    nextResult = { error: Object.assign(new Error('killed'), { code: 'SIGKILL' }), stdout: '', stderr: '', delayMs: 60_000 };
    const res = await new ClaudeCodeAdapter().spawn({ ...baseOpts, prompt: 'x', timeoutMs: 50 });
    expect(res.timedOut).toBe(true);
    expect(res.exitCode).toBe(-1);
    expect(res.events).toEqual([]);
    expect(res.durationMs).toBeLessThan(5_000);
  });

  it('opencode returns timedOut result with exitCode -1 and no events', async () => {
    nextResult = { error: Object.assign(new Error('killed'), { code: 'SIGKILL' }), stdout: '', stderr: '', delayMs: 60_000 };
    const res = await new OpenCodeAdapter().spawn({ ...baseOpts, prompt: 'x', timeoutMs: 50 });
    expect(res.timedOut).toBe(true);
    expect(res.exitCode).toBe(-1);
    expect(res.events).toEqual([]);
  });
});

describe('factory', () => {
  it('resolves adapters by name and shares instances', () => {
    expect(getWorker('claude-code')).toBe(workers['claude-code']);
    expect(getWorker('opencode')).toBe(workers.opencode);
    expect(getWorker('claude-code').name).toBe('claude-code');
  });

  it('throws on unknown name', () => {
    expect(() => getWorker('codex' as never)).toThrow(/Unknown worker/);
  });
});

describe('worker model passthrough', () => {
  it('opencode: adds --model provider/id when model is given', async () => {
    const { OpenCodeAdapter } = await import('../src/workers/opencode.js');
    ok('{}');
    await new OpenCodeAdapter().spawn({ ...baseOpts, prompt: 'p', model: 'opencode-go/ox-alpha-free' });
    expect(execFileMock).toHaveBeenCalledWith(
      'opencode',
      ['run', '--format', 'json', '--model', 'opencode-go/ox-alpha-free', 'p'],
      expect.anything(),
      expect.any(Function),
    );
  });

  it('opencode: omits --model when unset (back-compat)', async () => {
    const { OpenCodeAdapter } = await import('../src/workers/opencode.js');
    ok('{}');
    await new OpenCodeAdapter().spawn({ ...baseOpts, prompt: 'p' });
    const args = execFileMock.mock.calls.at(-1)![1] as string[];
    expect(args).toEqual(['run', '--format', 'json', 'p']);
  });

  it('claude-code: adds --model when given (resume attempts keep it)', async () => {
    const { ClaudeCodeAdapter } = await import('../src/workers/claude-code.js');
    ok('{}');
    await new ClaudeCodeAdapter().spawn({ ...baseOpts, prompt: 'p', model: 'claude-opus-4-6' });
    expect(execFileMock).toHaveBeenCalledWith(
      'claude',
      ['-p', 'p', '--output-format', 'json', '--model', 'claude-opus-4-6'],
      expect.anything(),
      expect.any(Function),
    );
  });
});

describe('opencode zero-event no-progress detection', () => {
  const noSleep = async () => {};

  it('classifies exit-0 zero-event empty-output as noProgress, not success', async () => {
    ok('');
    const res = await new OpenCodeAdapter(noSleep).spawn({ ...baseOpts, prompt: 'x' });
    expect(res.exitCode).toBe(0);
    expect(res.timedOut).toBe(false);
    expect(res.events).toEqual([]);
    expect(res.resultText).toBeNull();
    expect(res.noProgress).toBe(true);
  });

  it('surfaces noProgress on the last zero-event attempt when the budget is exhausted', async () => {
    // apiMaxAttempts=1: no probe budget remains, the zero-event attempt must
    // surface as noProgress rather than a false success.
    resultQueue = [{ error: null, stdout: '', stderr: '' }];
    const res = await new OpenCodeAdapter(noSleep).spawn({ ...baseOpts, prompt: 'x', apiMaxAttempts: 1 });
    expect(execFileMock).toHaveBeenCalledTimes(1);
    expect(res.noProgress).toBe(true);
  });

  it('bails out immediately when the cheap probe returns empty output', async () => {
    // First call: real prompt returns exit 0 with zero events. Second call:
    // the cheap probe also returns empty output (dead endpoint).
    resultQueue = [
      { error: null, stdout: '', stderr: '' },
      { error: null, stdout: '', stderr: '' },
    ];
    const res = await new OpenCodeAdapter(noSleep).spawn({ ...baseOpts, prompt: 'ship it', apiMaxAttempts: 3 });
    expect(execFileMock).toHaveBeenCalledTimes(2);
    const probeCall = (execFileMock.mock.calls[1] as unknown[])[1] as string[];
    expect(probeCall.at(-1)).toBe('Reply with the single word: ok');
    expect(res.noProgress).toBe(true);
    expect(res.exitCode).toBe(0);
    expect(res.resultText).toBeNull();
  });

  it('keeps retrying when the cheap probe proves the endpoint alive', async () => {
    // Attempt 1: zero-event exit-0. Probe: alive (non-empty response).
    // Attempt 2: normal success.
    resultQueue = [
      { error: null, stdout: '', stderr: '' },
      { error: null, stdout: JSON.stringify({ type: 'text', text: 'pong', sessionID: 's-p1' }), stderr: '' },
      { error: null, stdout: JSON.stringify({ type: 'text', text: 'real work done', sessionID: 's-p1' }), stderr: '' },
    ];
    const res = await new OpenCodeAdapter(noSleep).spawn({ ...baseOpts, prompt: 'ship it', apiMaxAttempts: 3 });
    expect(execFileMock).toHaveBeenCalledTimes(3);
    expect(res.noProgress).toBeUndefined();
    expect(res.exitCode).toBe(0);
    expect(res.resultText).toBe('real work done');
  });

  it('normal successful runs are unaffected (no noProgress flag)', async () => {
    ok(JSON.stringify({ type: 'text', text: 'shipped', sessionID: 'oc-1' }));
    const res = await new OpenCodeAdapter(noSleep).spawn({ ...baseOpts, prompt: 'x' });
    expect(res.exitCode).toBe(0);
    expect(res.resultText).toBe('shipped');
    expect(res.noProgress).toBeUndefined();
  });
});
