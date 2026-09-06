import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { execFileSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { tmpdir } from 'node:os';
import {
  DEGRADE_STREAK_THRESHOLD,
  degradationStreak,
  isDegradationRow,
  readDegradationStreak,
} from '../src/resilience/degradation.js';
import { EVENTS_FILE } from '../src/lessons/guard.js';
import { LEDGER_DIR } from '../src/orchestrator/ledger.js';

/** Row builders mirroring the real producers (preflight.ts, selfbuild-loop.sh). */
const loopResult = (loop: number, status: string, ts: string): Record<string, unknown> => ({
  ts,
  kind: 'event',
  event: 'loop-result',
  loop,
  status,
  goal: `Goal: surface consecutive cross-role provider degradation (loop ${loop})`,
});

const operatorDegraded = (
  role: string,
  ts: string,
  ok = false,
): Record<string, unknown> => ({
  ts,
  kind: 'event',
  event: 'operator-degraded',
  taskId: 'operator-preflight',
  role,
  worker: 'omp',
  model: 'omniroute/dev',
  ok,
  attempts: 3,
  ...(ok ? {} : { detail: 'unrecognized_model: probe 403' }),
});

describe('isDegradationRow (row classification)', () => {
  it('counts operator-degraded rows from every preflight role', () => {
    for (const role of ['prd-curator', 'po', 'selfbuild', 'warroom', 'reviewer']) {
      expect(isDegradationRow(operatorDegraded(role, '2026-09-05T12:00:00Z'))).toBe(true);
    }
  });

  it('treats an ok:true operator-degraded row (passed probe) as productive', () => {
    expect(isDegradationRow(operatorDegraded('selfbuild', '2026-09-05T12:00:00Z', true))).toBe(false);
  });

  it('counts loop-result provider-degraded and operator-diverged rows', () => {
    expect(isDegradationRow(loopResult(106, 'provider-degraded', '2026-09-05T12:00:00Z'))).toBe(true);
    expect(isDegradationRow(loopResult(107, 'operator-diverged', '2026-09-05T12:00:00Z'))).toBe(true);
  });

  it('treats every other loop-result status as productive (the provider answered)', () => {
    for (const status of ['ok', 'failed', 'failed-tests', 'invalid', 'skipped', 'push-failed', 'operator-degraded']) {
      expect(isDegradationRow(loopResult(108, status, '2026-09-05T12:00:00Z'))).toBe(false);
    }
  });

  it('treats unrelated ledger rows as productive', () => {
    expect(isDegradationRow({ ts: 'x', kind: 'audit', taskId: 'T', attempt: 1, verdict: 'pass' })).toBe(false);
    expect(isDegradationRow({ ts: 'x', kind: 'event', event: 'lessons-eval', loop: 108 })).toBe(false);
    expect(isDegradationRow({ ts: 'x', kind: 'event', event: 'loop-phase', loop: 108, phase: 'task' })).toBe(false);
  });
});

describe('degradationStreak (trailing-run walk, newest-first)', () => {
  it('flags the loops 106-108 shape: three provider-degraded rows in 75s', () => {
    const rows = [
      loopResult(104, 'ok', '2026-09-05T11:50:00Z'),
      loopResult(105, 'ok', '2026-09-05T11:58:00Z'),
      loopResult(106, 'provider-degraded', '2026-09-05T12:00:00Z'),
      loopResult(107, 'provider-degraded', '2026-09-05T12:00:30Z'),
      loopResult(108, 'provider-degraded', '2026-09-05T12:01:15Z'),
    ];
    const s = degradationStreak(rows);
    expect(s.count).toBe(3);
    expect(s.breach).toBe(true);
    expect(s.threshold).toBe(DEGRADE_STREAK_THRESHOLD);
    expect(s.oldestTs).toBe('2026-09-05T12:00:00Z');
    expect(s.latestTs).toBe('2026-09-05T12:01:15Z');
    expect(s.windowMs).toBe(75_000);
    expect(s.roles).toEqual([]);
  });

  it('empty ledger yields a zero streak without breach', () => {
    const s = degradationStreak([]);
    expect(s.count).toBe(0);
    expect(s.breach).toBe(false);
    expect(s.latestTs).toBeNull();
    expect(s.oldestTs).toBeNull();
    expect(s.windowMs).toBeNull();
  });

  it('a productive newest row yields count 0', () => {
    const rows = [
      loopResult(106, 'provider-degraded', '2026-09-05T12:00:00Z'),
      loopResult(107, 'provider-degraded', '2026-09-05T12:00:30Z'),
      loopResult(108, 'ok', '2026-09-05T12:01:15Z'),
    ];
    expect(degradationStreak(rows).count).toBe(0);
  });

  it('stops at the first productive row — only the trailing run counts', () => {
    const rows = [
      loopResult(100, 'provider-degraded', '2026-09-05T10:00:00Z'),
      loopResult(101, 'provider-degraded', '2026-09-05T10:01:00Z'),
      loopResult(102, 'ok', '2026-09-05T10:02:00Z'),
      operatorDegraded('po', '2026-09-05T10:03:00Z'),
      loopResult(103, 'provider-degraded', '2026-09-05T10:04:00Z'),
    ];
    const s = degradationStreak(rows);
    expect(s.count).toBe(2);
    expect(s.breach).toBe(false);
    expect(s.oldestTs).toBe('2026-09-05T10:03:00Z');
  });

  it('aggregates cross-role: operator-degraded rows from any role join loop-result rows in one streak', () => {
    const rows = [
      loopResult(106, 'ok', '2026-09-05T12:00:00Z'),
      operatorDegraded('selfbuild', '2026-09-05T12:01:00Z'),
      loopResult(107, 'provider-degraded', '2026-09-05T12:02:00Z'),
      operatorDegraded('warroom', '2026-09-05T12:03:00Z'),
      operatorDegraded('selfbuild', '2026-09-05T12:04:00Z'),
    ];
    const s = degradationStreak(rows);
    expect(s.count).toBe(4);
    expect(s.breach).toBe(true);
    // Newest-first, deduplicated.
    expect(s.roles).toEqual(['selfbuild', 'warroom']);
  });

  it('honors a threshold override in both directions', () => {
    const rows = [1, 2, 3, 4].map((n) =>
      loopResult(n, 'provider-degraded', `2026-09-05T12:0${n}:00Z`),
    );
    expect(degradationStreak(rows, 5).breach).toBe(false);
    expect(degradationStreak(rows, 5).threshold).toBe(5);
    expect(degradationStreak(rows, 2).breach).toBe(true);
  });

  it('never breaches at zero rows, even with a zero threshold', () => {
    expect(degradationStreak([], 0).breach).toBe(false);
  });

  it('keeps counting when timestamps are unparseable; windowMs is then null', () => {
    const rows = [
      loopResult(1, 'provider-degraded', 'not-a-date'),
      loopResult(2, 'provider-degraded', '2026-09-05T12:00:00Z'),
      loopResult(3, 'provider-degraded', 'also-broken'),
    ];
    const s = degradationStreak(rows);
    expect(s.count).toBe(3);
    expect(s.breach).toBe(true);
    expect(s.latestTs).toBe('also-broken');
    expect(s.windowMs).toBeNull();
  });

  it('computes the window from the streak edges even when a middle ts is unparseable', () => {
    const rows = [
      loopResult(1, 'provider-degraded', '2026-09-05T12:00:00Z'),
      loopResult(2, 'provider-degraded', 'not-a-date'),
      loopResult(3, 'provider-degraded', '2026-09-05T12:02:00Z'),
    ];
    const s = degradationStreak(rows);
    expect(s.count).toBe(3);
    expect(s.windowMs).toBe(120_000);
  });
});

describe('readDegradationStreak (fixture ledger)', () => {
  let repo: string;
  beforeEach(() => {
    repo = mkdtempSync(join(tmpdir(), 'da-degrade-'));
  });
  afterEach(() => {
    rmSync(repo, { recursive: true, force: true });
  });

  const writeLedger = (lines: string[]) => {
    const file = join(repo, EVENTS_FILE);
    mkdirSync(join(repo, LEDGER_DIR), { recursive: true });
    writeFileSync(file, `${lines.join('\n')}\n`, 'utf8');
  };

  it('returns a zero streak when events.jsonl is absent', () => {
    const s = readDegradationStreak(repo);
    expect(s.count).toBe(0);
    expect(s.breach).toBe(false);
  });

  it('walks the trailing run of a fixture ledger, skipping corrupt lines', () => {
    writeLedger([
      JSON.stringify(loopResult(105, 'ok', '2026-09-05T11:58:00Z')),
      '{not json at all',
      JSON.stringify(loopResult(106, 'provider-degraded', '2026-09-05T12:00:00Z')),
      JSON.stringify(loopResult(107, 'operator-diverged', '2026-09-05T12:00:30Z')),
      JSON.stringify(operatorDegraded('reviewer', '2026-09-05T12:01:15Z')),
    ]);
    const s = readDegradationStreak(repo);
    expect(s.count).toBe(3);
    expect(s.breach).toBe(true);
    expect(s.windowMs).toBe(75_000);
    expect(s.roles).toEqual(['reviewer']);
  });

  it('reports no breach once a productive row lands after the outage', () => {
    writeLedger([
      JSON.stringify(operatorDegraded('po', '2026-09-05T12:00:00Z')),
      JSON.stringify(operatorDegraded('po', '2026-09-05T12:00:30Z')),
      JSON.stringify(operatorDegraded('po', '2026-09-05T12:01:00Z')),
      JSON.stringify(operatorDegraded('po', '2026-09-05T12:01:30Z', true)),
      JSON.stringify(loopResult(109, 'ok', '2026-09-05T12:05:00Z')),
    ]);
    expect(readDegradationStreak(repo).count).toBe(0);
  });
});

describe('devagent status --providers (Q41 breach line)', () => {
  let repo: string;
  const cli = join(import.meta.dirname, '..', 'src', 'cli.ts');

  beforeEach(() => {
    repo = mkdtempSync(join(tmpdir(), 'da-degrade-cli-'));
    mkdirSync(join(repo, LEDGER_DIR), { recursive: true });
  });
  afterEach(() => {
    rmSync(repo, { recursive: true, force: true });
  });

  const statusProviders = (extraArgs: string[] = []) =>
    execFileSync('npx', ['tsx', cli, 'status', '--providers', '--repo', repo, ...extraArgs], {
      cwd: join(import.meta.dirname, '..'),
      stdio: 'pipe',
      env: {
        PATH: process.env.PATH,
        HOME: process.env.HOME,
        DEVAGENT_HOME: process.env.DEVAGENT_HOME ?? process.env.HOME ?? '.',
      },
      timeout: 30_000,
    }).toString();

  const writeLedger = (rows: Record<string, unknown>[]) =>
    writeFileSync(
      join(repo, EVENTS_FILE),
      `${rows.map((r) => JSON.stringify(r)).join('\n')}\n`,
      'utf8',
    );

  it('prints the breach line at three consecutive degraded rows, with the outage window', () => {
    writeLedger([
      loopResult(106, 'provider-degraded', '2026-09-05T12:00:00Z'),
      loopResult(107, 'provider-degraded', '2026-09-05T12:00:30Z'),
      loopResult(108, 'provider-degraded', '2026-09-05T12:01:15Z'),
    ]);
    const out = statusProviders();
    expect(out).toContain('degradation: 3 consecutive degraded rows in 75s');
    expect(out).toContain('provider outage (threshold 3)');
  });

  it('names the roles when the streak spans operator-degraded rows', () => {
    writeLedger([
      operatorDegraded('po', '2026-09-05T12:00:00Z'),
      operatorDegraded('selfbuild', '2026-09-05T12:00:30Z'),
      loopResult(107, 'operator-diverged', '2026-09-05T12:01:00Z'),
    ]);
    const out = statusProviders();
    expect(out).toContain('across roles: selfbuild, po');
  });

  it('stays silent below the threshold', () => {
    writeLedger([
      loopResult(106, 'provider-degraded', '2026-09-05T12:00:00Z'),
      loopResult(107, 'provider-degraded', '2026-09-05T12:00:30Z'),
    ]);
    expect(statusProviders()).not.toContain('degradation:');
  });

  it('--degrade-threshold overrides the breach bar', () => {
    writeLedger([
      loopResult(106, 'provider-degraded', '2026-09-05T12:00:00Z'),
      loopResult(107, 'provider-degraded', '2026-09-05T12:00:30Z'),
      loopResult(108, 'provider-degraded', '2026-09-05T12:01:00Z'),
    ]);
    expect(statusProviders(['--degrade-threshold', '4'])).not.toContain('degradation:');
    expect(statusProviders(['--degrade-threshold', '2'])).toContain('degradation: 3 consecutive');
  });
});
