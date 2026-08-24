import type { AuditVerdict, OrchestratorTask, ProjectBoard } from './types.js';
import { recomputeReadiness } from './types.js';
import type { RunLogger } from '../logger.js';
import type { WorkerName } from '../types.js';

/**
 * Wave scheduler (sprint-orchestrator lesson): repeatedly execute all ready
 * tasks in bounded parallel waves. Failure isolation is wave-scoped — a
 * failed task marks itself failed and blocks dependents but never kills
 * siblings. Each attempt runs on a fresh worktree (Orca lesson: no context
 * contamination across retries).
 *
 * Evidence-gated completion (LongHorizon-Harness lesson): an executor
 * success only moves the task to 'untrusted'; it becomes 'done' solely on
 * an independent audit verdict with clean integrity. Failed audits are
 * externalized into evidenceGaps so the retry targets the actual gap.
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
    lessonsFile?: string;
    log: RunLogger;
  }): Promise<ExecuteTaskResult>;
  /**
   * Independent read-only audit of an 'untrusted' task. Returns null for an
   * inconclusive run (worker crash, unparsable report) — treated as
   * retryable, never as pass.
   */
  auditTask?(args: { task: OrchestratorTask; board: ProjectBoard; repoPath: string; timeoutMs: number; log: RunLogger }): Promise<AuditVerdict | null>;
  /**
   * Planner-written recovery contract granted when a task exhausts its retry
   * budget (LH manager lesson: re-contract around the failure instead of
   * blocking the subtree). Return null to let the failure go terminal.
   */
  planRecovery?(args: { task: OrchestratorTask; board: ProjectBoard }): Promise<{ prompt: string; acceptanceCriteria?: string[] } | null>;
}

export interface SchedulerOptions {
  repoPath: string;
  /** Repo-local lessons file override (see loadLessons). */
  lessonsFile?: string;
  executor: WorkerName;
  concurrency: number;
  maxTaskRetries: number;
  /** Recovery-contract grants per task before a failure goes terminal */
  maxRecoveries?: number;
  /**
   * Consecutive audits repeating the same primary gap that trigger early
   * recovery escalation (default 2 — SWE-agent decay data says a third
   * identical attempt rarely recovers).
   */
  repeatGapThreshold?: number;

  timeoutMs: number;
  /**
   * Hard cap on dispatch waves (SWE-agent L1: unresolved runs average 2x the
   * cost of resolved ones — bound the board instead of escalating budgets).
   * When reached, unfinished tasks stay pending and the run ends with a
   * resumable board.
   */
  maxWaves?: number;
  /** Called after each wave with terminal task transitions persisted (LangGraph pending-writes lesson). */
  onWavePersisted?: (board: ProjectBoard) => void;
}

export async function runScheduler(
  board: ProjectBoard,
  opts: SchedulerOptions,
  deps: SchedulerDeps,
  log: RunLogger,
): Promise<ProjectBoard> {
  const maxRecoveries = opts.maxRecoveries ?? 1;
  const repeatGapThreshold = opts.repeatGapThreshold ?? 2;

  /** Grant one planner-written re-contract before a failure goes terminal. */
  const grantRecovery = async (task: OrchestratorTask): Promise<boolean> => {
    if (!deps.planRecovery || (task.recoveries ?? 0) >= maxRecoveries) return false;
    let rec: { prompt: string; acceptanceCriteria?: string[] } | null = null;
    try {
      rec = await deps.planRecovery({ task, board });
    } catch (err) {
      log.warn('task', `${task.id} recovery planning crashed: ${(err as Error).message}`, {});
      return false;
    }
    if (!rec?.prompt) return false;
    task.recoveries = (task.recoveries ?? 0) + 1;
    task.prompt = rec.prompt;
    if (rec.acceptanceCriteria?.length) task.acceptanceCriteria = rec.acceptanceCriteria.slice(0, 10);
    // fresh budget on the new contract; attemptSuffix keeps worktrees collision-free
    task.attempts = 0;
    task.status = 'pending';
    task.evidenceGaps = undefined;
    task.audit = undefined;
    task.failureDetail = undefined;
    task.repeatGaps = 0; // new contract, fresh streak

    log.info('task', `${task.id} granted recovery contract #${task.recoveries}`, {});
    return true;
  };

  let wave = 0;
  for (;;) {
    board.tasks = recomputeReadiness(board.tasks);
    const queue = board.tasks.filter(
      (t) => t.status === 'ready' || (t.status === 'failed' && t.attempts < opts.maxTaskRetries),
    );
    if (queue.length === 0) break;
    // Budget ceiling: stop dispatching, leave the board resumable
    if (opts.maxWaves !== undefined && wave >= opts.maxWaves) {
      log.warn('task', `wave budget exhausted after ${wave} wave(s); ${queue.length} task(s) still queued`, {
        queued: queue.map((t) => t.id),
      });
      break;
    }
    wave += 1;

    const workers = Array.from({ length: Math.min(opts.concurrency, queue.length) }, async () => {
      for (;;) {
        const task = queue.shift();
        if (!task) return;
        task.status = 'dispatched';
        task.attempts += 1;
        log.info('task', `Dispatching ${task.id}: ${task.title}`, { attempt: task.attempts });
        try {
          const r = await deps.executeTask({ task, board, repoPath: opts.repoPath, timeoutMs: opts.timeoutMs, lessonsFile: opts.lessonsFile, log });
          task.worktreePath = r.worktreePath ?? task.worktreePath;
          if (r.ok && !deps.auditTask) {
            // legacy mode: no auditor configured — executor gates are the trust boundary
            task.status = 'done';
            task.failureDetail = undefined;
            log.info('task', `${task.id} done`, {});
          } else if (r.ok && deps.auditTask) {
            // Evidence gate: executor success is only a claim until audited
            task.status = 'untrusted';
            let v: AuditVerdict | null = null;
            try {
              v = await deps.auditTask({ task, board, repoPath: opts.repoPath, timeoutMs: opts.timeoutMs, log });
            } catch (err) {
              log.warn('audit', `${task.id} audit crashed: ${(err as Error).message}`, {});
            }
            if (v && v.verdict === 'pass' && v.integrity === 'clean') {
              task.audit = v;
              task.evidenceGaps = undefined;
              task.status = 'done';
              task.failureDetail = undefined;
              log.info('task', `${task.id} done (audited)`, { criteria: v.criteriaResults.length });
            } else if (v && v.verdict === 'ask') {
              // Not a failure and not retryable: the branch waits for a human
              // answer via `orchestrate --resume --answer <id>=<text>`
              task.audit = v;
              task.status = 'ask';
              task.failureDetail = `needs human input: ${v.summary.slice(0, 200)}`;
              log.warn('task', `${task.id} paused for human input`, { question: v.summary.slice(0, 200) });
            } else {
              // Externalize the failure into state so the retry targets the
              // gap instead of redoing blind work (LH recovery lesson)
              const gaps =
                v && v.verdict === 'fail'
                  ? v.criteriaResults.filter((c) => !c.met).map((c) => `unmet: ${c.criterion} — ${c.evidence.slice(0, 200)}`)
                  : [];
              if (v?.integrity !== 'clean' && v) gaps.push(`integrity ${v.integrity}: workspace mutation or provenance concern`);
              if (!v) gaps.push('audit inconclusive: worker crashed or report unparsable');
              task.evidenceGaps = gaps;
              if (v) task.audit = v;
              // L2 (SWE-agent §B.3.3): recovery odds decay when the same gap
              // repeats — escalate to a recovery re-contract early instead of
              // burning the remaining retry budget against the same wall.
              const primaryGap = gaps[0] ?? '';
              const prevPrimary = task.failureDetail ?? '';
              task.repeatGaps = primaryGap && primaryGap === prevPrimary ? (task.repeatGaps ?? 0) + 1 : 1;
              const earlyEscalation =
                (task.repeatGaps ?? 0) >= repeatGapThreshold &&
                (task.recoveries ?? 0) < (opts.maxRecoveries ?? 1);
              task.status =
                task.attempts < opts.maxTaskRetries && !earlyEscalation
                  ? 'pending'
                  : (await grantRecovery(task))
                    ? 'pending'
                    : 'failed';

              task.failureDetail = gaps[0] ?? 'audit failed';
              log.warn('task', `${task.id} audit rejected (${v ? v.verdict + '/' + v.integrity : 'inconclusive'})`, {
                attempt: task.attempts,
                gaps: gaps.length,
              });
            }
          } else {
            task.failureDetail = r.detail;
            // retryable within budget -> pending for the next wave; else one
            // recovery re-contract, then terminal failure
            task.status =
              task.attempts < opts.maxTaskRetries ? 'pending' : (await grantRecovery(task)) ? 'pending' : 'failed';
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
