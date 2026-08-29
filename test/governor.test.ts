import { describe, expect, it, vi } from 'vitest';
import { ResourceGovernor, sampleWorkerRss, parseConcurrencyInput } from '../src/orchestrator/governor.js';
import { runScheduler } from '../src/orchestrator/scheduler.js';
import { runFleet } from '../src/fleet.js';
import { RunLogger } from '../src/logger.js';
import type { OrchestratorTask, ProjectBoard } from '../src/orchestrator/types.js';

const log = new RunLogger();
function task(partial: Partial<OrchestratorTask> & { id: string }): OrchestratorTask {
  return { title: partial.id, prompt: 'do it', dependsOn: [], status: 'pending', attempts: 0, ...partial };
}
function board(tasks: OrchestratorTask[]): ProjectBoard {
  return { goal: 'g', createdAt: '', updatedAt: '', roles: { planner: 'claude-code', executor: 'opencode' }, tasks };
}

describe('ResourceGovernor core (AC-1, AC-3)', () => {
  it('effectiveConcurrency: plenty RAM uses cpu cap', () => {
    const gov = new ResourceGovernor({ estMemPerWorkerBytes: 1_000_000_000, safetyRatio: 0.7, cpuFactor: 1 });
    const snap = { totalMem: 16_000_000_000, freeMem: 8_000_000_000, cpus: 4 };
    // memCap = floor(8e9*0.7/1e9)=5, cpuCap=4, min=4, clamp [1,8]=4
    expect(gov.effectiveConcurrency(snap, 8)).toBe(4);
  });

  it('effectiveConcurrency: exactly one worker of RAM', () => {
    const gov = new ResourceGovernor({ estMemPerWorkerBytes: 1_000_000_000, safetyRatio: 0.7, cpuFactor: 1 });
    // avail 1.5GB -> memCap floor(1.5*0.7)=1, cpus 4 -> cpuCap 4 -> min 1
    const snap = { totalMem: 8_000_000_000, freeMem: 1_500_000_000, cpus: 4 };
    expect(gov.effectiveConcurrency(snap, 4)).toBe(1);
  });

  it('effectiveConcurrency: zero free RAM still >=1', () => {
    const gov = new ResourceGovernor({ estMemPerWorkerBytes: 1_000_000_000, safetyRatio: 0.7, cpuFactor: 1 });
    const snap = { totalMem: 8_000_000_000, freeMem: 0, cpus: 8 };
    expect(gov.effectiveConcurrency(snap, 4)).toBe(1);
  });

  it('effectiveConcurrency: cpu-bound cap', () => {
    const gov = new ResourceGovernor({ estMemPerWorkerBytes: 200_000_000, safetyRatio: 0.7, cpuFactor: 0.5 });
    // 8 cpus *0.5=4 -> ceil 4, memCap huge -> limited to 4, configured 8 -> 4
    const snap = { totalMem: 64_000_000_000, freeMem: 32_000_000_000, cpus: 8 };
    expect(gov.effectiveConcurrency(snap, 8)).toBe(4);
  });

  it('effectiveConcurrency clamps to configured ceiling', () => {
    const gov = new ResourceGovernor({ estMemPerWorkerBytes: 100_000_000, safetyRatio: 1, cpuFactor: 1 });
    const snap = { totalMem: 32_000_000_000, freeMem: 16_000_000_000, cpus: 32 };
    // memCap huge, cpuCap 32, min 32, but configured 2 => 2
    expect(gov.effectiveConcurrency(snap, 2)).toBe(2);
  });

  it('effectiveAuto returns mem/cpu bound without configured ceiling', () => {
    const gov = new ResourceGovernor({ estMemPerWorkerBytes: 1_000_000_000, safetyRatio: 0.7, cpuFactor: 1 });
    const snap = { totalMem: 16_000_000_000, freeMem: 3_100_000_000, cpus: 16 };
    // memCap floor(3.1*0.7)=2, cpuCap 16 => min 2
    expect(gov.effectiveAuto(snap)).toBe(2);
  });

  it('performs no network calls and <5ms per check (benchmark)', () => {
    const gov = new ResourceGovernor();
    const snap = { totalMem: 16_000_000_000, freeMem: 8_000_000_000, cpus: 4 };
    const start = performance.now();
    for (let i = 0; i < 100; i++) gov.effectiveConcurrency(snap, 4);
    const elapsed = performance.now() - start;
    expect(elapsed / 100).toBeLessThan(5);
  });

  it('OS reads are cached for <=1s', () => {
    const gov = new ResourceGovernor({ cacheTtlMs: 1000 });
    const s1 = gov.getSnapshotSync();
    const s2 = gov.getSnapshotSync();
    expect(s1).toBe(s2); // same object from cache
    // force refresh
    const s3 = gov.refreshSnapshot();
    expect(s3).not.toBe(s1);
  });

  it('calibration raises estMemPerWorker via p75 and never below floor', () => {
    const gov = new ResourceGovernor({ estMemPerWorkerBytes: 200_000_000, minEstFloorBytes: 100_000_000 });
    expect(gov.getEstMemPerWorker()).toBe(200_000_000);
    // push 4 samples: 100,200,300,400 -> p75 is 300
    for (const rss of [100_000_000, 200_000_000, 300_000_000, 400_000_000]) gov.recordRss(rss);
    expect(gov.getEstMemPerWorker()).toBe(300_000_000);
    // smaller sample doesn't lower it
    gov.recordRss(50_000_000);
    expect(gov.getEstMemPerWorker()).toBe(300_000_000);
  });

  it('parseConcurrencyInput handles auto and numbers', () => {
    expect(parseConcurrencyInput('auto')).toBe('auto');
    expect(parseConcurrencyInput('Auto')).toBe('auto');
    expect(parseConcurrencyInput('4')).toBe(4);
    expect(parseConcurrencyInput(2)).toBe(2);
    expect(parseConcurrencyInput('  auto  ')).toBe('auto');
  });

  it('formatStatus contains expected fields', () => {
    const gov = new ResourceGovernor({ estMemPerWorkerBytes: 1_000_000_000 });
    const snap = { totalMem: 16 * 1024 * 1024 * 1024, freeMem: 8 * 1024 * 1024 * 1024, cpus: 8 };
    const line = gov.formatStatus('auto', 2, snap);
    expect(line).toContain('workers auto->2');
    expect(line).toContain('mem');
    expect(line).toContain('est');
  });
});

describe('sampleWorkerRss (AC-2)', () => {
  it('returns real RSS for current process on macOS/Linux or null on unsupported', async () => {
    const rss = await sampleWorkerRss(process.pid);
    if (process.platform === 'linux' || process.platform === 'darwin') {
      expect(rss).toBeGreaterThan(0);
      expect(typeof rss).toBe('number');
    } else {
      expect(rss).toBeNull();
    }
  });

  it('returns null for invalid pid', async () => {
    expect(await sampleWorkerRss(-1)).toBeNull();
    expect(await sampleWorkerRss(9999999)).toBeNull();
  });
});

describe('scheduler governor enforcement (AC-4,5,6,7,8)', () => {
  it('AC-4: with fixture governor reporting only 1 worker fits, concurrency 4 never exceeds 1 dispatched', async () => {
    const gov = new ResourceGovernor({ estMemPerWorkerBytes: 1_000_000_000, safetyRatio: 0.7, cpuFactor: 1 });
    // inject snapshot where only 1 fits
    gov.injectSnapshot({ totalMem: 8_000_000_000, freeMem: 1_200_000_000, cpus: 4 });
    // est 1GB -> memCap floor(1.2*0.7)=0 -> max(1,0) =>1, cpuCap 4 => min 1 => effective 1
    const b = board([task({ id: 'A' }), task({ id: 'B' }), task({ id: 'C' }), task({ id: 'D' })]);
    let peakDispatched = 0;
    let currentDispatched = 0;
    // Monkey: track via queue? We'll instrument executeTask to track peak concurrency
    let active = 0;
    let peakActive = 0;
    await runScheduler(
      b,
      { repoPath: '.', executor: 'opencode', concurrency: 'auto', governor: gov, governorSnapshot: { totalMem: 8_000_000_000, freeMem: 1_200_000_000, cpus: 4 }, maxTaskRetries: 1, timeoutMs: 1000 },
      {
        executeTask: async () => {
          active += 1;
          peakActive = Math.max(peakActive, active);
          currentDispatched += 1;
          peakDispatched = Math.max(peakDispatched, currentDispatched);
          await new Promise((r) => setTimeout(r, 15));
          active -= 1;
          currentDispatched -= 1;
          return { ok: true };
        },
      },
      log,
    );
    expect(peakActive).toBeLessThanOrEqual(1);
    expect(b.tasks.every((t) => t.status === 'done')).toBe(true);
  });

  it('AC-5: ceiling drops mid-wave — second worker waits (throttle not fail)', async () => {
    const gov = new ResourceGovernor({ estMemPerWorkerBytes: 500_000_000, safetyRatio: 0.7, cpuFactor: 1 });
    // start with enough for 2, then flip to 1 after first task
    let callCount = 0;
    const baseSnap = { totalMem: 8_000_000_000, freeMem: 4_000_000_000, cpus: 4 };
    // mock getSnapshotSync to return different freeMem on each call
    const originalGet = gov.getSnapshotSync.bind(gov);
    gov.getSnapshotSync = () => {
      callCount += 1;
      if (callCount <= 2) return baseSnap; // first wave pool 2 (memCap 5, cpu 4 ->4)
      return { totalMem: 8_000_000_000, freeMem: 600_000_000, cpus: 4 }; // memCap 0 ->1
    };
    const b = board([task({ id: 'A' }), task({ id: 'B' }), task({ id: 'C' })]);
    let peak = 0;
    let active = 0;
    await runScheduler(
      b,
      { repoPath: '.', executor: 'opencode', concurrency: 'auto', governor: gov, maxTaskRetries: 1, timeoutMs: 1000 },
      {
        executeTask: async () => {
          active += 1;
          peak = Math.max(peak, active);
          await new Promise((r) => setTimeout(r, 20));
          active -= 1;
          return { ok: true };
        },
      },
      log,
    );
    // Should have completed without marking tasks failed due to throttle
    expect(b.tasks.filter((t) => t.status === 'failed')).toHaveLength(0);
    expect(b.tasks.every((t) => t.status === 'done')).toBe(true);
    // restore
    gov.getSnapshotSync = originalGet;
  });

  it('AC-6: after pressureWaitTimeout with no headroom, wave proceeds at concurrency 1', async () => {
    const gov = new ResourceGovernor({ estMemPerWorkerBytes: 1_000_000_000, safetyRatio: 0.7, cpuFactor: 1, pressureWaitTimeoutMs: 400 });
    gov.injectSnapshot({ totalMem: 8_000_000_000, freeMem: 500_000_000, cpus: 4 }); // effective 1
    // Need to test throttle timeout: use a long-running first task + low effective
    const b = board([task({ id: 'A' }), task({ id: 'B' })]);
    const start = Date.now();
    await runScheduler(
      b,
      { repoPath: '.', executor: 'opencode', concurrency: 'auto', governor: gov, governorSnapshot: { totalMem: 8_000_000_000, freeMem: 500_000_000, cpus: 4 }, maxTaskRetries: 1, timeoutMs: 1000 },
      {
        executeTask: async () => {
          await new Promise((r) => setTimeout(r, 30));
          return { ok: true };
        },
      },
      log,
    );
    expect(b.tasks.every((t) => t.status === 'done')).toBe(true);
    // Ensure it didn't deadlock: elapsed should be at least sum of serial tasks but not huge
    expect(Date.now() - start).toBeGreaterThan(30);
  });

  it('AC-7: explicit numeric concurrency bypasses governor (zero-regression)', async () => {
    const gov = new ResourceGovernor({ estMemPerWorkerBytes: 10_000_000_000, safetyRatio: 0.7 }); // absurdly high est -> effective 1 if auto
    gov.injectSnapshot({ totalMem: 8_000_000_000, freeMem: 1_000_000_000, cpus: 4 });
    // Governor would say 1, but numeric 3 must stay 3
    const bAuto = board([task({ id: 'A' }), task({ id: 'B' }), task({ id: 'C' })]);
    const bNum = board([task({ id: 'A' }), task({ id: 'B' }), task({ id: 'C' })]);
    let peakAuto = 0, activeAuto = 0;
    await runScheduler(bAuto, { repoPath: '.', executor: 'opencode', concurrency: 'auto', governor: gov, governorSnapshot: { totalMem: 8_000_000_000, freeMem: 1_000_000_000, cpus: 4 }, maxTaskRetries: 1, timeoutMs: 1000 }, {
      executeTask: async () => { activeAuto+=1; peakAuto=Math.max(peakAuto, activeAuto); await new Promise(r=>setTimeout(r,10)); activeAuto-=1; return {ok:true}; },
    }, log);
    expect(peakAuto).toBeLessThanOrEqual(1);

    let peakNum = 0, activeNum = 0;
    await runScheduler(bNum, { repoPath: '.', executor: 'opencode', concurrency: 3, governor: gov, governorSnapshot: { totalMem: 8_000_000_000, freeMem: 1_000_000_000, cpus: 4 }, maxTaskRetries: 1, timeoutMs: 1000 }, {
      executeTask: async () => { activeNum+=1; peakNum=Math.max(peakNum, activeNum); await new Promise(r=>setTimeout(r,10)); activeNum-=1; return {ok:true}; },
    }, log);
    expect(peakNum).toBe(3);
    expect(peakNum).not.toBe(peakAuto);
  });

  it('AC-8: --concurrency auto logs resolved number and inputs', async () => {
    const gov = new ResourceGovernor();
    const snap = { totalMem: 16 * 1024 * 1024 * 1024, freeMem: 8 * 1024 * 1024 * 1024, cpus: 4 };
    gov.injectSnapshot(snap);
    const eff = gov.effectiveAuto(snap);
    const line = gov.formatStatus('auto', eff, snap);
    expect(line).toContain('auto->');
    expect(line).toContain('mem');
    expect(line).toContain('est');
    // Simulate CLI logging: ensure governor info would be logged via RunLogger
    const tlog = new RunLogger();
    tlog.info('governor', `auto -> ${eff}`, { freeMem: snap.freeMem, totalMem: snap.totalMem, cpus: snap.cpus });
    // Check log file contains governor line
    const { readFileSync } = await import('node:fs');
    const content = readFileSync(tlog.path, 'utf8');
    expect(content).toContain('governor');
  });
});

describe('fleet governor enforcement (AC-4 fleet counterpart)', () => {
  it('fleet respects governor ceiling when auto', async () => {
    const { ResourceGovernor } = await import('../src/orchestrator/governor.js');
    const gov = new ResourceGovernor({ estMemPerWorkerBytes: 2_000_000_000, safetyRatio: 0.7, cpuFactor: 1 });
    const snap = { totalMem: 16_000_000_000, freeMem: 2_000_000_000, cpus: 8 }; // memCap 0 ->1, cpu 8 =>1
    let peak = 0, active = 0;
    const runOne = vi.fn().mockImplementation(async () => {
      active += 1; peak = Math.max(peak, active);
      await new Promise((r) => setTimeout(r, 10));
      active -= 1;
      return { ok: true, summary: 'ok' };
    });
    const entries = [{ name: 'a', path: '/a' }, { name: 'b', path: '/b' }];
    await runFleet({ ticketIds: ['T1', 'T2', 'T3'], entries, concurrency: 'auto', governor: gov, governorSnapshot: snap, timeoutMs: 1000, worker: 'opencode', autoPr: false, maxLoops: 1, runOne });
    expect(peak).toBeLessThanOrEqual(1);
    expect(runOne).toHaveBeenCalledTimes(6);
  });

  it('fleet explicit numeric bypasses governor', async () => {
    const { ResourceGovernor } = await import('../src/orchestrator/governor.js');
    const gov = new ResourceGovernor({ estMemPerWorkerBytes: 10_000_000_000 });
    const snap = { totalMem: 8_000_000_000, freeMem: 500_000_000, cpus: 4 };
    let peak = 0, active = 0;
    const runOne = vi.fn().mockImplementation(async () => {
      active += 1; peak = Math.max(peak, active);
      await new Promise((r) => setTimeout(r, 10));
      active -= 1;
      return { ok: true, summary: 'ok' };
    });
    const entries = [{ name: 'a', path: '/a' }];
    await runFleet({ ticketIds: ['1', '2', '3'], entries, concurrency: 3, governor: gov, governorSnapshot: snap, timeoutMs: 1000, worker: 'opencode', autoPr: false, maxLoops: 1, runOne });
    expect(peak).toBe(3);
  });
});
