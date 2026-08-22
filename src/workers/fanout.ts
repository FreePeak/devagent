import type { RunLogger } from '../logger.js';
import type { ImplementationPlan } from '../planner.js';
import type { WorkerName } from '../types.js';
import { buildImplementationPrompt } from '../prompt.js';
import { getWorker } from './index.js';
import { createWorktree } from '../git/worktree.js';

/**
 * Fan-out mode (FR-IMPL-03): run the same plan through multiple workers in
 * parallel isolated worktrees; score each leg with the repo's own test suite;
 * exactly one leg wins (tests decide, claude-code breaks ties).
 */

export interface FanoutLeg {
  worker: WorkerName;
  worktreePath?: string;
  ok: boolean;
  testsPassed: boolean | null;
  durationMs: number;
}

export interface FanoutOptions {
  repoPath: string;
  timeoutMs: number;
  scoreLeg?(worktreePath: string, timeoutMs: number): Promise<boolean | null>;
}

/** Returns the winning leg, or null if no leg produced a usable result. */
export async function runFanout(
  plan: ImplementationPlan,
  workers: WorkerName[],
  log: RunLogger,
  opts: FanoutOptions,
): Promise<FanoutLeg | null> {
  const prompt = buildImplementationPrompt(plan);
  const legs = await Promise.all(
    workers.map(async (workerName, i): Promise<FanoutLeg> => {
      const worker = getWorker(workerName);
      let cwd = opts.repoPath;
      let worktreePath: string | undefined;
      try {
        const wt = await createWorktree(opts.repoPath, `${plan.ticket.id}-${workerSuffix(workerName, i)}`);
        cwd = wt.worktreePath;
        worktreePath = wt.worktreePath;
      } catch (err) {
        log.warn('implement', `Fanout ${workerName}: worktree failed (${(err as Error).message})`);
      }
      const started = Date.now();
      const result = await worker.spawn({ prompt, cwd, timeoutMs: opts.timeoutMs });
      const ok = !result.timedOut && result.exitCode === 0;
      log.info('implement', `Fanout ${workerName} finished`, {
        exitCode: result.exitCode,
        timedOut: result.timedOut,
        durationMs: result.durationMs,
      });
      const testsPassed = ok && worktreePath && opts.scoreLeg ? await opts.scoreLeg(worktreePath, opts.timeoutMs) : null;
      return { worker: workerName, worktreePath, ok, testsPassed, durationMs: Date.now() - started };
    }),
  );

  const usable = legs.filter((l) => l.ok);
  if (usable.length === 0) return null;

  const rank = (l: FanoutLeg): number =>
    (l.testsPassed === true ? 2 : l.testsPassed === null ? 1 : 0) * 10 + (l.worker === 'claude-code' ? 1 : 0);

  return usable.reduce((best, leg) => (rank(leg) > rank(best) ? leg : best));
}

function workerSuffix(name: WorkerName, index: number): string {
  return name.replace(/[^A-Za-z0-9-_]/g, '') || `leg${index}`;
}
