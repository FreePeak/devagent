import type { RunLogger } from '../logger.js';
import type { ImplementationPlan } from '../planner.js';
import type { WorkerName } from '../types.js';
import { buildImplementationPrompt, loadLessons } from '../prompt.js';
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
  branch?: string;
  ok: boolean;
  testsPassed: boolean | null;
  /** True when tests failed once and passed on the single flaky rerun. */
  flaky?: boolean;
  durationMs: number;
}

export interface FanoutOptions {
  repoPath: string;
  timeoutMs: number;
  /** Repo-local lessons file override (see loadLessons). */
  lessonsFile?: string;
  scoreLeg?(worktreePath: string, timeoutMs: number): Promise<boolean | null>;
  /**
   * Merge-assist hook (PRD section 17 Phase 4): invoked once after winner
   * selection with every usable leg plus the winner, before runFanout
   * returns. Awaited, so publishStage sees the settled git state.
   */
  onSelected?(legs: FanoutLeg[], winner: FanoutLeg): Promise<void>;
}

/** Returns the winning leg, or null if no leg produced a usable result. */
export async function runFanout(
  plan: ImplementationPlan,
  workers: WorkerName[],
  log: RunLogger,
  opts: FanoutOptions,
): Promise<FanoutLeg | null> {
  const prompt = buildImplementationPrompt(plan, loadLessons(opts.repoPath, opts.lessonsFile));
  const legs = await Promise.all(
    workers.map(async (workerName, i): Promise<FanoutLeg> => {
      const worker = getWorker(workerName);
      let cwd = opts.repoPath;
      let worktreePath: string | undefined;
      let branch: string | undefined;
      try {
        const wt = await createWorktree(opts.repoPath, `${plan.ticket.id}-${workerSuffix(workerName, i)}`);
        cwd = wt.worktreePath;
        worktreePath = wt.worktreePath;
        branch = wt.branch;
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
      // Flaky guard (PRD section 17 Phase 4): one rerun before condemning a
      // leg — nondeterministic suites must not discard otherwise good work.
      let testsPassed = ok && worktreePath && opts.scoreLeg ? await opts.scoreLeg(worktreePath, opts.timeoutMs) : null;
      let flaky = false;
      if (testsPassed === false && worktreePath && opts.scoreLeg) {
        testsPassed = await opts.scoreLeg(worktreePath, opts.timeoutMs);
        if (testsPassed === true) {
          flaky = true;
          log.warn('implement', `Fanout ${workerName}: tests failed then passed on rerun (flaky)`);
        }
      }
      return { worker: workerName, worktreePath, branch, ok, testsPassed, flaky, durationMs: Date.now() - started };
    }),
  );

  const usable = legs.filter((l) => l.ok);
  if (usable.length === 0) return null;

  // Clean pass outranks a flaky rescue: a pass that needed a rerun earns no
  // more trust than a leg we could not score at all.
  const rank = (l: FanoutLeg): number =>
    (l.testsPassed === true && !l.flaky ? 2 : l.testsPassed === false && !l.flaky ? 0 : 1) * 10 +
    (l.worker === 'claude-code' ? 1 : 0);

  const winner = usable.reduce((best, leg) => (rank(leg) > rank(best) ? leg : best));
  if (opts.onSelected) await opts.onSelected(usable, winner);
  return winner;
}

function workerSuffix(name: WorkerName, index: number): string {
  return name.replace(/[^A-Za-z0-9-_]/g, '') || `leg${index}`;
}
