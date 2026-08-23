import type { OrchestratorTask, ProjectBoard } from './types.js';
import { recomputeReadiness } from './types.js';
import type { RunLogger } from '../logger.js';
import type { WorkerName } from '../types.js';

/**
 * Wave scheduler (sprint-orchestrator lesson): repeatedly execute all ready
 * tasks in bounded parallel waves. Failure isolation is wave-scoped — a
 * failed task marks itself failed and blocks dependents but never kills
 * siblings. Each attempt runs on a fresh worktree (Orca lesson: no context
 * contamination across retries).
 */

export interface ExecuteTaskResult {
  ok: boolean;
  worktreePath?: string;
  detail?: string;
}

export interface SchedulerDeps {
  executeTask(args: {
    task: OrchestratorTask;
    board: ProjectBoard;
    repoPath: string;
    timeoutMs: number;
    log: RunLogger;
  }): Promise<ExecuteTaskResult>;
}

export interface SchedulerOptions {
  repoPath: string;
  executor: WorkerName;
  concurrency: number;
  maxTaskRetries: number;
  timeoutMs: number;
  /** Called after each wave with terminal task transitions persisted (LangGraph pending-writes lesson). */
  onWavePersisted?: (board: ProjectBoard) => void;
}

export async function runScheduler(
  board: ProjectBoard,
  opts: SchedulerOptions,
  deps: SchedulerDeps,
  log: RunLogger,
): Promise<ProjectBoard> {
  for (;;) {
    board.tasks = recomputeReadiness(board.tasks);
    const queue = board.tasks.filter(
      (t) => t.status === 'ready' || (t.status === 'failed' && t.attempts < opts.maxTaskRetries),
    );
    if (queue.length === 0) break;

    const workers = Array.from({ length: Math.min(opts.concurrency, queue.length) }, async () => {
      for (;;) {
        const task = queue.shift();
        if (!task) return;
        task.status = 'dispatched';
        task.attempts += 1;
        log.info('task', `Dispatching ${task.id}: ${task.title}`, { attempt: task.attempts });
        try {
          const r = await deps.executeTask({ task, board, repoPath: opts.repoPath, timeoutMs: opts.timeoutMs, log });
          task.worktreePath = r.worktreePath ?? task.worktreePath;
          if (r.ok) {
            task.status = 'done';
            task.failureDetail = undefined;
            log.info('task', `${task.id} done`, {});
          } else {
            task.failureDetail = r.detail;
            // retryable within budget -> pending for the next wave; else failed
            task.status = task.attempts < opts.maxTaskRetries ? 'pending' : 'failed';
            log.warn('task', `${task.id} failed (attempt ${task.attempts})`, { detail: r.detail?.slice(0, 200) });
          }
        } catch (err) {
          task.status = 'failed';
          task.failureDetail = (err as Error).message;
          log.error('task', `${task.id} crashed: ${(err as Error).message}`, {});
        }
      }
    });
    await Promise.all(workers);
    opts.onWavePersisted?.(board); // crash mid-run loses nothing already done
  }
  board.tasks = recomputeReadiness(board.tasks);
  return board;
}
