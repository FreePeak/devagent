import { afterAll, afterEach, describe, expect, it } from 'vitest';
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { execPath } from 'node:process';
import { LEDGER_DIR, appendWatchdogHealthRecord } from '../src/orchestrator/ledger.js';
import { spawnCli, spawnCliStreaming } from '../src/workers/spawn-utils.js';
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
