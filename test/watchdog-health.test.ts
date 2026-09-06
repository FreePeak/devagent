import { afterAll, afterEach, describe, expect, it } from 'vitest';
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { execPath } from 'node:process';
import { LEDGER_DIR, appendWatchdogHealthRecord } from '../src/orchestrator/ledger.js';
import { spawnCli, spawnCliStreaming } from '../src/workers/spawn-utils.js';
import { loadConfig } from '../src/config.js';
/**
 * Q34: spawn-cli watchdog-health rows. One row per CLI launch whenever a
 * no-progress clock is armed and the dispatcher supplied ledger identity —
 * never for clock-less (probe/one-off) spawns, never conflating wall-clock
 * expiry with a watchdog fire.
 *
 * Real timers are unavoidable here (rule exception): the watchdog interval
 * and wall timer live inside spawnCliStreaming and poll Date.now() while a
 * REAL child process runs; vi.useFakeTimers only mocks this process's clock
 * and cannot advance a separate OS child's output timing. Same pattern as
 * guard.test.ts (noProgressTimeoutMs: 500 silent child) and herdr.test.ts
 * (timeoutMs: 700 sleep).
 */

const dirs: string[] = [];
const tempDir = (prefix: string): string => {
  const d = mkdtempSync(join(tmpdir(), prefix));
  dirs.push(d);
  return d;
};
const rows = (repo: string): Array<Record<string, unknown>> => {
  const file = join(repo, LEDGER_DIR, 'events.jsonl');
  if (!existsSync(file)) return [];
  return readFileSync(file, 'utf8')
    .split('\n')
    .filter((l) => l.trim())
    .map((l) => JSON.parse(l) as Record<string, unknown>);
};

const ctx = (repo: string) => ({ repoPath: repo, taskId: 'Q34-1', attempt: 2, worker: 'omp' });

afterEach(() => {
  for (const d of dirs) rmSync(d, { recursive: true, force: true });
  dirs.length = 0;
});

afterAll(() => {
  rmSync(join(tmpdir(), 'watchdog-health-suite'), { recursive: true, force: true });
});

describe('spawn-cli watchdog-health rows', () => {
  it('writes one row with watchdogFired:false for a run that progressed then exited', async () => {
    const repo = tempDir('wh-progress-');
    const dir = tempDir('wh-child-');
    const stub = join(dir, 'progress.mjs');
    writeFileSync(stub, `console.log('{"type":"tool_execution_start"}'); process.exit(0);`);
    const r = await spawnCliStreaming(execPath, [stub], {
      cwd: dir,
      timeoutMs: 10_000,
      noProgressTimeoutMs: 5_000,
      watchdogLedger: ctx(repo),
    });
    expect(r.timedOut).toBe(false);
    expect(r.exitCode).toBe(0);
    const all = rows(repo);
    expect(all).toHaveLength(1);
    expect(all[0]).toMatchObject({
      kind: 'event',
      event: 'watchdog-health',
      site: 'spawn-cli',
      taskId: 'Q34-1',
      attempt: 2,
      worker: 'omp',
      noProgressTimeoutMs: 5_000,
      watchdogFired: false,
    });
    expect(all[0]!.clockResets).toBeGreaterThanOrEqual(1);
    expect(all[0]!.meaningfulBytes as number).toBeGreaterThan(0);
    expect(all[0]!.wallClockMs as number).toBeLessThan(10_000);
    expect(typeof all[0]!.ts).toBe('string');
  });

  it('writes watchdogFired:true when the watchdog kills a silent child', async () => {
    const repo = tempDir('wh-fire-');
    const dir = tempDir('wh-silent-');
    const stub = join(dir, 'silent.mjs');
    writeFileSync(stub, `setTimeout(() => {}, 60_000);`);
    const r = await spawnCliStreaming(execPath, [stub], {
      cwd: dir,
      timeoutMs: 30_000,
      noProgressTimeoutMs: 500,
      watchdogLedger: ctx(repo),
    });
    expect(r.timedOut).toBe(true);
    const all = rows(repo);
    expect(all).toHaveLength(1);
    expect(all[0]).toMatchObject({
      event: 'watchdog-health',
      site: 'spawn-cli',
      watchdogFired: true,
      noProgressTimeoutMs: 500,
      clockResets: 0,
      meaningfulBytes: 0,
    });
    // Idle at kill time is at least the armed window.
    expect(all[0]!.idleMs as number).toBeGreaterThanOrEqual(500);
  });

  it('separates wall-clock expiry from watchdog fire', async () => {
    const repo = tempDir('wh-wall-');
    const dir = tempDir('wh-wallchild-');
    const stub = join(dir, 'silent.mjs');
    writeFileSync(stub, `setTimeout(() => {}, 60_000);`);
    const r = await spawnCliStreaming(execPath, [stub], {
      cwd: dir,
      timeoutMs: 700,
      noProgressTimeoutMs: 30_000,
      watchdogLedger: ctx(repo),
    });
    expect(r.timedOut).toBe(true);
    const all = rows(repo);
    expect(all).toHaveLength(1);
    expect(all[0]!.watchdogFired).toBe(false);
    expect(all[0]!.noProgressTimeoutMs).toBe(30_000);
  });

  it('never writes a row without ledger context', async () => {
    const repo = tempDir('wh-noctx-');
    const dir = tempDir('wh-noctxchild-');
    const stub = join(dir, 'progress.mjs');
    writeFileSync(stub, `console.log('{"type":"text_end"}'); process.exit(0);`);
    await spawnCliStreaming(execPath, [stub], { cwd: dir, timeoutMs: 10_000, noProgressTimeoutMs: 5_000 });
    expect(rows(repo)).toEqual([]);
    expect(existsSync(join(repo, LEDGER_DIR))).toBe(false);
  });

  it('never writes a row when the clock is not armed (spawnCli execFile path)', async () => {
    const repo = tempDir('wh-noclock-');
    const dir = tempDir('wh-noclockchild-');
    const r = await spawnCli(execPath, ['-e', 'process.exit(0)'], {
      cwd: dir,
      timeoutMs: 10_000,
      noProgressTimeoutMs: 0,
      watchdogLedger: ctx(repo),
    });
    expect(r.timedOut).toBe(false);
    expect(rows(repo)).toEqual([]);
  });

  it('appendWatchdogHealthRecord is best-effort and readLedger keeps ignoring event rows', async () => {
    const repo = tempDir('wh-append-');
    expect(() =>
      appendWatchdogHealthRecord(repo, {
        ts: '2026-09-04T00:00:00Z',
        kind: 'event',
        event: 'watchdog-health',
        taskId: 'T',
        attempt: 1,
        worker: 'pi',
        site: 'herdr-pane',
        noProgressTimeoutMs: 600_000,
        watchdogFired: false,
        coldStartFired: false,
        wallClockMs: 1_000,
        clockResets: 3,
        meaningfulBytes: 512,
        idleMs: 250,
      }),
    ).not.toThrow();
    const raw = readFileSync(join(repo, LEDGER_DIR, 'events.jsonl'), 'utf8').trim().split('\n');
    expect(raw).toHaveLength(1);
    expect(JSON.parse(raw[0]!)).toMatchObject({ event: 'watchdog-health', site: 'herdr-pane', clockResets: 3 });
  });
});

/**
 * Q31: the cold-start (first-progress) deadline. Startup chatter that never
 * evidences new work must not keep a wedged plugin/MCP init alive to the
 * 10-minute silence clock; the first adapter-classified progress line
 * disarms the deadline and the no-progress watchdog takes over from there.
 * Real timers stay (same documented exception as above): the deadline lives
 * inside spawnCliStreaming's interval polling Date.now() against a real OS
 * child's output timing, which fake timers cannot advance.
 */
describe('spawn-cli cold-start deadline', () => {
  it('kills a silent child at the cold-start budget and records coldStartFired', async () => {
    const repo = tempDir('cs-fire-');
    const dir = tempDir('cs-firechild-');
    const stub = join(dir, 'silent.mjs');
    writeFileSync(stub, `setTimeout(() => {}, 60_000);`);
    const r = await spawnCliStreaming(execPath, [stub], {
      cwd: dir,
      timeoutMs: 30_000,
      noProgressTimeoutMs: 30_000,
      coldStartTimeoutMs: 600,
      watchdogLedger: ctx(repo),
    });
    expect(r.timedOut).toBe(true);
    expect(r.coldStart).toBe(true);
    const all = rows(repo);
    expect(all).toHaveLength(1);
    expect(all[0]).toMatchObject({
      event: 'watchdog-health',
      site: 'spawn-cli',
      coldStartFired: true,
      watchdogFired: false,
      noProgressTimeoutMs: 30_000,
      clockResets: 0,
    });
  });

  it('non-progress startup chatter does not extend the budget', async () => {
    const repo = tempDir('cs-chatter-');
    const dir = tempDir('cs-chatterchild-');
    const stub = join(dir, 'chatter.mjs');
    // omp-class plugin/MCP init: continuous output that evidences no new work.
    writeFileSync(stub, `setInterval(() => console.log('loading plugin root...'), 100);`);
    const r = await spawnCliStreaming(execPath, [stub], {
      cwd: dir,
      timeoutMs: 30_000,
      noProgressTimeoutMs: 30_000,
      coldStartTimeoutMs: 600,
      watchdogLedger: ctx(repo),
    });
    expect(r.timedOut).toBe(true);
    expect(r.coldStart).toBe(true);
    const all = rows(repo);
    expect(all[0]).toMatchObject({ coldStartFired: true, watchdogFired: false });
  });

  it('the first progress line disarms the deadline; later silence trips the no-progress clock', async () => {
    const repo = tempDir('cs-disarm-');
    const dir = tempDir('cs-disarmchild-');
    const stub = join(dir, 'progress-then-silence.mjs');
    writeFileSync(stub, `console.log('{"type":"tool_execution_start"}'); setTimeout(() => {}, 60_000);`);
    const r = await spawnCliStreaming(execPath, [stub], {
      cwd: dir,
      timeoutMs: 30_000,
      noProgressTimeoutMs: 600,
      coldStartTimeoutMs: 30_000,
      watchdogLedger: ctx(repo),
    });
    expect(r.timedOut).toBe(true);
    expect(r.coldStart).toBeUndefined();
    const all = rows(repo);
    expect(all[0]).toMatchObject({ coldStartFired: false, watchdogFired: true });
  });

  it('leaves a launch that progresses inside the budget untouched', async () => {
    const repo = tempDir('cs-ok-');
    const dir = tempDir('cs-okchild-');
    const stub = join(dir, 'progress.mjs');
    writeFileSync(stub, `console.log('{"type":"tool_execution_start"}'); process.exit(0);`);
    const r = await spawnCliStreaming(execPath, [stub], {
      cwd: dir,
      timeoutMs: 10_000,
      noProgressTimeoutMs: 5_000,
      coldStartTimeoutMs: 600,
      watchdogLedger: ctx(repo),
    });
    expect(r.timedOut).toBe(false);
    expect(r.coldStart).toBeUndefined();
    const all = rows(repo);
    expect(all[0]!.coldStartFired).toBe(false);
  });

  it('spawnCli routes to the streaming watchdog when only the cold-start clock is armed', async () => {
    const dir = tempDir('cs-route-');
    const stub = join(dir, 'silent.mjs');
    writeFileSync(stub, `setTimeout(() => {}, 60_000);`);
    const r = await spawnCli(execPath, [stub], {
      cwd: dir,
      timeoutMs: 30_000,
      coldStartTimeoutMs: 600,
    });
    expect(r.timedOut).toBe(true);
    expect(r.coldStart).toBe(true);
  });

  it('coldStartTimeoutMs: 0 disables the deadline (wall clock stays the only net)', async () => {
    const dir = tempDir('cs-off-');
    const stub = join(dir, 'silent.mjs');
    writeFileSync(stub, `setTimeout(() => {}, 60_000);`);
    const r = await spawnCli(execPath, [stub], {
      cwd: dir,
      timeoutMs: 700,
      noProgressTimeoutMs: 0,
      coldStartTimeoutMs: 0,
    });
    expect(r.timedOut).toBe(true);
    expect(r.coldStart).toBeUndefined();
  });
});

describe('resilience.coldStartTimeoutMs config', () => {
  const withEnv = async (value: string, fn: () => void): Promise<void> => {
    const saved = process.env.DEVAGENT_COLD_START_TIMEOUT_MS;
    process.env.DEVAGENT_COLD_START_TIMEOUT_MS = value;
    try {
      fn();
    } finally {
      if (saved === undefined) delete process.env.DEVAGENT_COLD_START_TIMEOUT_MS;
      else process.env.DEVAGENT_COLD_START_TIMEOUT_MS = saved;
    }
  };

  it('reads DEVAGENT_COLD_START_TIMEOUT_MS, including 0 as disable', async () => {
    await withEnv('45000', () => {
      expect(loadConfig('/nonexistent-path-for-sure').resilience?.coldStartTimeoutMs).toBe(45_000);
    });
    await withEnv('0', () => {
      expect(loadConfig('/nonexistent-path-for-sure').resilience?.coldStartTimeoutMs).toBe(0);
    });
  });

  it('env overrides the file value; negative values are rejected', async () => {
    const repo = tempDir('cs-cfg-');
    writeFileSync(join(repo, 'devagent.json'), JSON.stringify({ resilience: { coldStartTimeoutMs: 30_000 } }));
    expect(loadConfig(repo).resilience?.coldStartTimeoutMs).toBe(30_000);
    await withEnv('120000', () => {
      expect(loadConfig(repo).resilience?.coldStartTimeoutMs).toBe(120_000);
    });
    writeFileSync(join(repo, 'devagent.json'), JSON.stringify({ resilience: { coldStartTimeoutMs: -1 } }));
    expect(() => loadConfig(repo)).toThrow(/Invalid resilience\.coldStartTimeoutMs/);
  });
});
