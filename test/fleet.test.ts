import { describe, expect, it, vi } from 'vitest';
import { runFleet, type FleetRunOptions } from '../src/fleet.js';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const entries = [
  { name: 'api', path: '/repos/api' },
  { name: 'worker', path: '/repos/worker' },
];

function baseOpts(over: Partial<FleetRunOptions> = {}): FleetRunOptions {
  return {
    ticketIds: ['ENG-1'],
    entries,
    concurrency: 2,
    timeoutMs: 1000,
    worker: 'claude-code',
    autoPr: false,
    maxLoops: 3,
    runOne: vi.fn().mockResolvedValue({ ok: true, summary: 'completed' }),
    ...over,
  };
}

describe('runFleet', () => {
  it('runs every ticket × repo combination', async () => {
    const runOne = vi.fn().mockResolvedValue({ ok: true, summary: 'completed' });
    const r = await runFleet(baseOpts({ ticketIds: ['A', 'B'], runOne }));
    expect(r.items).toHaveLength(4);
    expect(r.succeeded).toBe(4);
    expect(r.failed).toBe(0);
    expect(runOne).toHaveBeenCalledTimes(4);
  });

  it('respects the concurrency bound', async () => {
    let inFlight = 0;
    let peak = 0;
    const runOne = vi.fn().mockImplementation(async () => {
      inFlight++;
      peak = Math.max(peak, inFlight);
      await new Promise((r) => setTimeout(r, 5));
      inFlight--;
      return { ok: true, summary: 'completed' };
    });
    await runFleet(baseOpts({ ticketIds: ['1', '2', '3', '4', '5'], concurrency: 2, runOne }));
    expect(peak).toBeLessThanOrEqual(2);
    expect(runOne).toHaveBeenCalledTimes(10); // 5 tickets x 2 repos
  });

  it('isolates failures: a throwing repo does not stall others', async () => {
    const runOne = vi.fn().mockImplementation(async ({ repoPath }) => {
      if (repoPath === '/repos/api') throw new Error('repo exploded');
      return { ok: true, summary: 'completed' };
    });
    const r = await runFleet(baseOpts({ ticketIds: ['A', 'B'], runOne }));
    expect(r.succeeded).toBe(2);
    expect(r.failed).toBe(2);
    expect(r.items.filter((i) => !i.ok).every((i) => i.summary.includes('exploded'))).toBe(true);
  });

  it('records per-run log paths', async () => {
    const r = await runFleet(baseOpts());
    for (const item of r.items) {
      expect(item.logPath).toMatch(/\.jsonl$/);
    }
  });

  it('handles empty inputs without spawning workers', async () => {
    const runOne = vi.fn();
    const r = await runFleet(baseOpts({ entries: [], runOne }));
    expect(r.items).toHaveLength(0);
    expect(runOne).not.toHaveBeenCalled();
    void join;
    void tmpdir;
    void mkdtempSync;
  });
});
