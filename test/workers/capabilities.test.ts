import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Mock } from 'vitest';
import type { WorkerName, WorkerSpawnOptions } from '../../src/types.js';
import { resolveNoProgressTimeoutMs } from '../../src/workers/spawn-utils.js';
import { getWorker, workers } from '../../src/workers/index.js';

/**
 * PRD Q30: the no-progress watchdog budget is declared per adapter
 * (`WorkerAdapter.capabilities`) and resolved once by the spawn path
 * (`resolveNoProgressTimeoutMs`). These tests pin the declarations to the
 * values each adapter resolved before the collapse, and the precedence the
 * shared resolver must keep.
 */

const TEN_MINUTES_MS = 10 * 60 * 1000;

/** Watchdog defaults as resolved pre-Q30: claude-code/opencode armed nothing, omp/pi/grok 10m. */
const DECLARED_DEFAULTS: Record<WorkerName, number> = {
  'claude-code': 0,
  opencode: 0,
  omp: TEN_MINUTES_MS,
  pi: TEN_MINUTES_MS,
  grok: TEN_MINUTES_MS,
};

/** Adapter that arms a clock by default (omp/pi/grok) vs one that declares none. */
const armed = { defaultNoProgressTimeoutMs: TEN_MINUTES_MS };
const off = { defaultNoProgressTimeoutMs: 0 };

const ENV = 'DEVAGENT_NO_PROGRESS_TIMEOUT_MS';
let savedEnv: string | undefined;

beforeEach(() => {
  // Hermetic vs selfbuild-loop.sh's exported watchdog default: every case
  // below sets the env it means to exercise.
  savedEnv = process.env[ENV];
  delete process.env[ENV];
});

afterEach(() => {
  if (savedEnv === undefined) delete process.env[ENV];
  else process.env[ENV] = savedEnv;
});

const baseOpts = (overrides: Partial<WorkerSpawnOptions> = {}): WorkerSpawnOptions => ({
  prompt: 'do the thing',
  cwd: '/tmp/work',
  timeoutMs: 60_000,
  ...overrides,
});

describe('WorkerAdapter capabilities - registry', () => {
  it('every registered adapter declares a capability block', () => {
    const names = Object.keys(workers) as WorkerName[];
    expect(names).toHaveLength(5);
    for (const name of names) {
      const caps = getWorker(name).capabilities;
      expect(caps, `${name} must declare capabilities`).toBeDefined();
      expect(Number.isFinite(caps?.defaultNoProgressTimeoutMs), name).toBe(true);
      expect(caps?.defaultNoProgressTimeoutMs, name).toBeGreaterThanOrEqual(0);
    }
  });

  it('declares exactly the watchdog default each adapter resolved before the collapse', () => {
    for (const name of Object.keys(workers) as WorkerName[]) {
      expect(getWorker(name).capabilities?.defaultNoProgressTimeoutMs, name).toBe(
        DECLARED_DEFAULTS[name],
      );
    }
  });
});

describe('resolveNoProgressTimeoutMs - declared default', () => {
  it('applies the declared default when the caller passes nothing', () => {
    expect(resolveNoProgressTimeoutMs(undefined, armed)).toBe(TEN_MINUTES_MS);
    expect(resolveNoProgressTimeoutMs(undefined, off)).toBe(0);
  });

  it('falls back to a disarmed clock for an adapter that declares no capabilities', () => {
    expect(resolveNoProgressTimeoutMs(undefined)).toBe(0);
    expect(resolveNoProgressTimeoutMs(undefined, undefined)).toBe(0);
  });

  it('lets the operator env default override the declaration', () => {
    process.env[ENV] = '120000';
    expect(resolveNoProgressTimeoutMs(undefined, armed)).toBe(120_000);
    expect(resolveNoProgressTimeoutMs(undefined, off)).toBe(120_000);
  });

  it('ignores a non-positive or malformed env default', () => {
    for (const raw of ['', '0', '-5', 'not-a-number', 'Infinity']) {
      process.env[ENV] = raw;
      expect(resolveNoProgressTimeoutMs(undefined, armed), raw).toBe(TEN_MINUTES_MS);
      expect(resolveNoProgressTimeoutMs(undefined, off), raw).toBe(0);
    }
  });
});

describe('resolveNoProgressTimeoutMs - explicit override', () => {
  it('keeps an explicit positive budget ahead of the declaration and the env', () => {
    process.env[ENV] = '120000';
    expect(resolveNoProgressTimeoutMs(5_000, armed)).toBe(5_000);
    expect(resolveNoProgressTimeoutMs(5_000, off)).toBe(5_000);
  });

  it('honors 0 = watchdog off for an adapter that declares 0', () => {
    expect(resolveNoProgressTimeoutMs(0, off)).toBe(0);
    process.env[ENV] = '120000';
    expect(resolveNoProgressTimeoutMs(0, off)).toBe(0);
  });

  it('treats a nonzero declaration as a floor for adapters that must stay watched', () => {
    expect(resolveNoProgressTimeoutMs(0, armed)).toBe(TEN_MINUTES_MS);
    process.env[ENV] = '120000';
    expect(resolveNoProgressTimeoutMs(0, armed)).toBe(120_000);
  });
});

describe('spawn path honors the declared capability', () => {
  let runWorkerCliMock: Mock;
  let prepareWorkerSpawnMock: Mock;

  const okRun = (payload: unknown) => ({
    exitCode: 0,
    stdout: JSON.stringify(payload),
    stderr: '',
    timedOut: false,
  });

  const ompRun = okRun({ type: 'result', is_error: false, session_id: 'omp-1', result: 'ok' });
  const claudeRun = okRun({ type: 'result', is_error: false, session_id: 'cc-1', result: 'ok' });

  /** Third positional arg of a runWorkerCli/prepareWorkerSpawn call: the spawn opts. */
  function spawnOptsOf(call: unknown[] | undefined): Record<string, unknown> {
    return (call as unknown[])[2] as Record<string, unknown>;
  }

  beforeEach(() => {
    vi.resetModules();
    prepareWorkerSpawnMock = vi.fn().mockResolvedValue({
      cmd: 'worker',
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
    runWorkerCliMock.mockReset();
    prepareWorkerSpawnMock.mockReset();
  });

  it('arms omp with its declared 10-minute budget when the caller passes none', async () => {
    // Module-loading boundary: vi.doMock only reaches modules imported after
    // resetModules, so the adapter is re-imported per test (same seam as
    // test/workers/pi.test.ts).
    const { OmpAdapter } = await import('../../src/workers/omp.js');
    runWorkerCliMock.mockResolvedValue(ompRun);
    await new OmpAdapter().spawn(baseOpts());
    expect(spawnOptsOf(runWorkerCliMock.mock.calls[0]).noProgressTimeoutMs).toBe(TEN_MINUTES_MS);
  });

  it('keeps an explicit caller budget ahead of the declaration', async () => {
    const { OmpAdapter } = await import('../../src/workers/omp.js');
    runWorkerCliMock.mockResolvedValue(ompRun);
    await new OmpAdapter().spawn(baseOpts({ noProgressTimeoutMs: 5_000 }));
    expect(spawnOptsOf(runWorkerCliMock.mock.calls[0]).noProgressTimeoutMs).toBe(5_000);
  });

  it('never disarms a declared floor: caller 0 still arms omp', async () => {
    const { OmpAdapter } = await import('../../src/workers/omp.js');
    runWorkerCliMock.mockResolvedValue(ompRun);
    await new OmpAdapter().spawn(baseOpts({ noProgressTimeoutMs: 0 }));
    expect(spawnOptsOf(runWorkerCliMock.mock.calls[0]).noProgressTimeoutMs).toBe(TEN_MINUTES_MS);
  });

  it('leaves the clock off for claude-code, whose declaration arms nothing', async () => {
    const { ClaudeCodeAdapter } = await import('../../src/workers/claude-code.js');
    runWorkerCliMock.mockResolvedValue(claudeRun);
    await new ClaudeCodeAdapter().spawn(baseOpts());
    expect('noProgressTimeoutMs' in spawnOptsOf(runWorkerCliMock.mock.calls[0])).toBe(false);
    expect('noProgressTimeoutMs' in spawnOptsOf(prepareWorkerSpawnMock.mock.calls[0])).toBe(false);
  });
});
