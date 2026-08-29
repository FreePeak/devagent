import { RunLogger, type RunLogger as RunLoggerType } from './logger.js';
import type { ResourceGovernor, OsSnapshot } from './orchestrator/governor.js';

/**
 * Fleet execution (v2 gap: multi-repo management): run one or more tickets
 * against N repositories over a bounded concurrency pool. Failure isolation:
 * a repo that errors is recorded and does not stall the rest (Orca's
 * task-dispatch lesson — workers fail independently).
 */

export interface FleetEntry {
  /** Logical repo name for the result surface */
  name: string;
  /** Absolute path to the repository checkout */
  path: string;
}

export interface FleetRunOptions {
  ticketIds: string[];
  entries: FleetEntry[];
  concurrency: number | 'auto';
  governor?: ResourceGovernor;
  governorSnapshot?: OsSnapshot;
  timeoutMs: number;
  worker: 'claude-code' | 'opencode' | 'both';
  autoPr: boolean;
  maxLoops: number;
  /** Injected so fleet stays unit-testable; same shape the CLI builds. */
  runOne(args: {
    repoPath: string;
    ticketId: string;
    worker: FleetRunOptions['worker'];
    autoPr: boolean;
    maxLoops: number;
    timeoutMs: number;
    log: RunLoggerType;
  }): Promise<{ ok: boolean; summary: string }>;
}

export interface FleetResultItem {
  entry: string;
  ticketId: string;
  ok: boolean;
  summary: string;
  logPath?: string;
}

export interface FleetResult {
  items: FleetResultItem[];
  succeeded: number;
  failed: number;
}

/** Map tickets × repos onto at most `concurrency` concurrent runs. */
export async function runFleet(opts: FleetRunOptions): Promise<FleetResult> {
  const jobs: Array<{ entry: FleetEntry; ticketId: string }> = [];
  for (const entry of opts.entries) {
    for (const ticketId of opts.ticketIds) {
      jobs.push({ entry, ticketId });
    }
  }

  const isAuto = opts.concurrency === 'auto';
  const resolveEffective = (): number => {
    if (!isAuto) {
      const n = typeof opts.concurrency === 'number' ? opts.concurrency : 2;
      return Math.max(1, Math.floor(n));
    }
    if (!opts.governor) return 2;
    const snap = opts.governorSnapshot ?? opts.governor.getSnapshotSync();
    return opts.governor.effectiveAuto(snap);
  };

  const effective = resolveEffective();
  const poolSize = Math.max(1, Math.min(effective, jobs.length));

  const items: FleetResultItem[] = [];
  let cursor = 0;
  let active = 0;

  const worker = async (): Promise<void> => {
    while (true) {
      if (isAuto && opts.governor) {
        const start = Date.now();
        const timeoutMs = opts.governor.pressureWaitTimeoutMs ?? 60_000;
        while (true) {
          const curEff = resolveEffective();
          if (active < curEff) break;
          if (Date.now() - start >= timeoutMs) break;
          await new Promise((r) => setTimeout(r, 50));
        }
      }
      const idx = cursor++;
      if (idx >= jobs.length) return;
      const job = jobs[idx]!;
      active += 1;
      const log = new RunLogger();
      let item: FleetResultItem;
      try {
        const r = await opts.runOne({
          repoPath: job.entry.path,
          ticketId: job.ticketId,
          worker: opts.worker,
          autoPr: opts.autoPr,
          maxLoops: opts.maxLoops,
          timeoutMs: opts.timeoutMs,
          log,
        });
        item = { entry: job.entry.name, ticketId: job.ticketId, ok: r.ok, summary: r.summary, logPath: log.path };
      } catch (err) {
        // isolation: record and continue
        item = { entry: job.entry.name, ticketId: job.ticketId, ok: false, summary: (err as Error).message, logPath: log.path };
      } finally {
        active = Math.max(0, active - 1);
      }
      items.push(item);
    }
  };

  await Promise.all(Array.from({ length: poolSize }, worker));

  return {
    items,
    succeeded: items.filter((i) => i.ok).length,
    failed: items.filter((i) => !i.ok).length,
  };
}
