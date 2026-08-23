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

import { ClaudeCodeAdapter } from '../src/workers/claude-code.js';
import { OpenCodeAdapter } from '../src/workers/opencode.js';
import { getWorker, workers } from '../src/workers/index.js';

const baseOpts = { cwd: '/tmp/repo', timeoutMs: 5_000 };

beforeEach(() => {
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
