import { describe, expect, it, afterEach, beforeEach, vi } from 'vitest';
import { buildPiArgs, interpretPiForTest, PiAdapter } from '../../src/workers/pi.js';
import { getWorker, workers } from '../../src/workers/index.js';
import type { WorkerSpawnOptions } from '../src/types.js';
import type { SpawnCliResult } from '../../src/workers/spawn-utils.js';

const baseOpts = (overrides: Partial<WorkerSpawnOptions> = {}): WorkerSpawnOptions => ({
  prompt: 'do the thing',
  cwd: '/tmp/work',
  timeoutMs: 60_000,
  ...overrides,
});

const run = (overrides: Partial<SpawnCliResult> = {}): SpawnCliResult => ({
  exitCode: 0,
  stdout: '',
  stderr: '',
  timedOut: false,
  ...overrides,
});

const SESSION_HEADER = '{"type":"session","version":3,"id":"abc-123","timestamp":"2026-09-01T00:00:00.000Z","cwd":"/tmp/work"}';
const ASSISTANT_END = '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"RESULT_TEXT"}],"stopReason":"stop"}}';

describe('pi adapter - registration', () => {
  it('is registered in the worker map and resolvable via getWorker', () => {
    expect(workers.pi).toBeInstanceOf(PiAdapter);
    expect(getWorker('pi')).toBe(workers.pi);
    expect(getWorker('pi').name).toBe('pi');
  });
});

describe('pi adapter - argument building', () => {
  it('builds the minimum non-interactive argv: --mode json, -p, prompt', () => {
    expect(buildPiArgs(baseOpts())).toEqual(['--mode', 'json', '-p', 'do the thing']);
  });

  it('forwards --model when a provider-qualified id is provided', () => {
    expect(buildPiArgs(baseOpts({ model: 'omniroute/bai/glm-5.3-flash' }))).toEqual([
      '--mode', 'json', '-p', 'do the thing', '--model', 'omniroute/bai/glm-5.3-flash',
    ]);
  });

  it('drops driver-tier aliases without a slash (e.g. "coding") — not pi ids', () => {
    expect(buildPiArgs(baseOpts({ model: 'coding' }))).toEqual(['--mode', 'json', '-p', 'do the thing']);
  });

  it('forwards --thinking when variant is provided', () => {
    expect(buildPiArgs(baseOpts({ variant: 'high' }))).toEqual(['--mode', 'json', '-p', 'do the thing', '--thinking', 'high']);
  });

  it('builds a resume argv using --continue with the resume prompt and no -p', () => {
    expect(buildPiArgs(baseOpts(), true)).toEqual(['--mode', 'json', '--continue', 'Continue']);
  });

  it('keeps model and thinking on the resume argv', () => {
    expect(buildPiArgs(baseOpts({ model: 'openai/gpt-4o', variant: 'high' }), true)).toEqual([
      '--mode', 'json', '--continue', 'Continue', '--model', 'openai/gpt-4o', '--thinking', 'high',
    ]);
  });
});

describe('pi adapter - output parsing', () => {
  it('extracts sessionId from the session header event', () => {
    const out = interpretPiForTest(run({ stdout: `${SESSION_HEADER}\n${ASSISTANT_END}\n` }));
    expect(out.sessionId).toBe('abc-123');
    expect(out.isError).toBe(false);
  });

  it('extracts assistant text from message_end as resultText', () => {
    const out = interpretPiForTest(run({ stdout: `${SESSION_HEADER}\n${ASSISTANT_END}\n` }));
    expect(out.resultText).toBe('RESULT_TEXT');
  });

  it('keeps the LAST textual assistant message (agentic answer after tool turns)', () => {
    const toolTurn = '{"type":"message_end","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","toolName":"bash"}],"stopReason":"toolUse"}}';
    const later = '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"LATER"}],"stopReason":"stop"}}';
    const out = interpretPiForTest(run({ stdout: `${SESSION_HEADER}\n${toolTurn}\n${later}\n` }));
    expect(out.resultText).toBe('LATER');
  });

  it('falls back to an earlier textual turn when later assistant turns are textless', () => {
    const answer = '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"ANSWER"}],"stopReason":"stop"}}';
    const textless = '{"type":"message_end","message":{"role":"assistant","content":[{"type":"toolCall","id":"c2","toolName":"bash"}],"stopReason":"toolUse"}}';
    const out = interpretPiForTest(run({ stdout: `${SESSION_HEADER}\n${answer}\n${textless}\n` }));
    expect(out.resultText).toBe('ANSWER');
  });

  it('ignores user message_end events when looking for the answer', () => {
    const userEnd = '{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":"PROMPT ECHO"}]}}';
    const out = interpretPiForTest(run({ stdout: `${SESSION_HEADER}\n${userEnd}\n${ASSISTANT_END}\n` }));
    expect(out.resultText).toBe('RESULT_TEXT');
  });

  it('concatenates multiple text parts of one assistant message', () => {
    const multi = '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"A"},{"type":"text","text":"B"}],"stopReason":"stop"}}';
    const out = interpretPiForTest(run({ stdout: `${SESSION_HEADER}\n${multi}\n` }));
    expect(out.resultText).toBe('AB');
  });

  it('flags isError when the assistant message carries errorMessage (provider failure with exit 0)', () => {
    const failed = '{"type":"message_end","message":{"role":"assistant","content":[],"errorMessage":"Provider 401: unauthorized","stopReason":"error"}}';
    const out = interpretPiForTest(run({ stdout: `${SESSION_HEADER}\n${failed}\n` }));
    expect(out.isError).toBe(true);
    expect(out.errorText).toContain('401');
    expect(out.resultText).toBeNull();
  });

  it('flags isError on non-zero exit with no parseable output', () => {
    const out = interpretPiForTest(run({ exitCode: 1, stdout: '', stderr: 'boom' }));
    expect(out.isError).toBe(true);
    expect(out.errorText).toBe('boom');
  });

  it('survives garbage lines interleaved with valid NDJSON events', () => {
    const out = interpretPiForTest(run({ stdout: `${SESSION_HEADER}\nnot json at all\n${ASSISTANT_END}\n` }));
    expect(out.sessionId).toBe('abc-123');
    expect(out.resultText).toBe('RESULT_TEXT');
  });

  it('returns null resultText when no assistant message appears', () => {
    const out = interpretPiForTest(run({ stdout: `${SESSION_HEADER}\n` }));
    expect(out.resultText).toBeNull();
    expect(out.isError).toBe(false);
  });

  it('accepts the legacy single-JSON envelope with a result field', () => {
    const out = interpretPiForTest(run({ stdout: '{"result":"legacy","session_id":"s-1"}' }));
    expect(out.resultText).toBe('legacy');
    expect(out.sessionId).toBe('s-1');
  });

  it('does not treat the session header line as a legacy result envelope', () => {
    const out = interpretPiForTest(run({ stdout: `${SESSION_HEADER}\n${ASSISTANT_END}\n` }));
    // header has no result text; answer must come from the NDJSON walk
    expect(out.resultText).toBe('RESULT_TEXT');
  });
});

describe('pi adapter - retry semantics', () => {
  let runWorkerCliMock: ReturnType<typeof vi.fn>;
  let prepareWorkerSpawnMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.resetModules();
    prepareWorkerSpawnMock = vi.fn().mockResolvedValue({
      cmd: 'pi',
      args: [],
      opts: {},
      strippedEnv: [],
    });
    vi.doMock('../../src/workers/sandbox.js', () => ({
      prepareWorkerSpawn: prepareWorkerSpawnMock,
    }));
    runWorkerCliMock = vi.fn();
    vi.doMock('../../src/workers/herdr-runtime.js', () => ({
      runWorkerCli: runWorkerCliMock,
    }));
  });

  afterEach(() => {
    vi.doUnmock('../../src/workers/sandbox.js');
    vi.doUnmock('../../src/workers/herdr-runtime.js');
    vi.resetModules();
  });

  it('resumes with --continue after a mid-stream provider failure and succeeds', async () => {
    const { PiAdapter: FreshPi } = await import('../../src/workers/pi.js');
    runWorkerCliMock
      .mockResolvedValueOnce(run({
        exitCode: 0,
        stdout: `${SESSION_HEADER}\n{"type":"message_end","message":{"role":"assistant","content":[],"errorMessage":"upstream dropped","stopReason":"error"}}\n`,
      }))
      .mockResolvedValueOnce(run({
        exitCode: 0,
        stdout: `${SESSION_HEADER}\n${ASSISTANT_END}\n`,
      }));
    const adapter = new FreshPi();
    const result = await adapter.spawn(baseOpts({ apiMaxAttempts: 3 }));
    expect(result.resultText).toBe('RESULT_TEXT');
    expect(result.sessionId).toBe('abc-123');
    expect(result.exitCode).toBe(0);
    expect(runWorkerCliMock).toHaveBeenCalledTimes(2);
    const secondCallArgs = prepareWorkerSpawnMock.mock.calls[1]?.[1] as string[];
    expect(secondCallArgs).toContain('--continue');
  });

  it('aborts immediately on non-retryable auth errors', async () => {
    const { PiAdapter: FreshPi } = await import('../../src/workers/pi.js');
    runWorkerCliMock.mockResolvedValue(run({
      exitCode: 1,
      stdout: '',
      stderr: 'Invalid API key · Please run /login',
    }));
    const adapter = new FreshPi();
    const result = await adapter.spawn(baseOpts({ apiMaxAttempts: 5 }));
    expect(result.resultText).toBeNull();
    expect(runWorkerCliMock).toHaveBeenCalledTimes(1);
  });

  it('stops after exhausting apiMaxAttempts', async () => {
    const { PiAdapter: FreshPi } = await import('../../src/workers/pi.js');
    runWorkerCliMock.mockResolvedValue(run({
      exitCode: 0,
      stdout: `${SESSION_HEADER}\n{"type":"message_end","message":{"role":"assistant","content":[],"errorMessage":"upstream down","stopReason":"error"}}\n`,
    }));
    const adapter = new FreshPi();
    const result = await adapter.spawn(baseOpts({ apiMaxAttempts: 2 }));
    expect(result.resultText).toBeNull();
    expect(runWorkerCliMock).toHaveBeenCalledTimes(2);
  });

  it('returns resultText null and errorText when spawn produced no output (empty stdout)', async () => {
    const { PiAdapter: FreshPi } = await import('../../src/workers/pi.js');
    runWorkerCliMock.mockResolvedValue(run({ exitCode: 1, stdout: '', stderr: 'pi: command failed' }));
    const adapter = new FreshPi();
    const result = await adapter.spawn(baseOpts({ apiMaxAttempts: 1 }));
    expect(result.resultText).toBeNull();
    expect(result.errorText).toBe('pi: command failed');
  });

  it('passes --mode json -p argv through to prepareWorkerSpawn', async () => {
    const { PiAdapter: FreshPi } = await import('../../src/workers/pi.js');
    runWorkerCliMock.mockResolvedValue(run({ exitCode: 0, stdout: `${SESSION_HEADER}\n${ASSISTANT_END}\n` }));
    const adapter = new FreshPi();
    await adapter.spawn(baseOpts({ prompt: 'ship it' }));
    expect(prepareWorkerSpawnMock).toHaveBeenCalledWith(
      'pi',
      expect.arrayContaining(['--mode', 'json', '-p', 'ship it']),
      expect.anything(),
    );
  });
});
