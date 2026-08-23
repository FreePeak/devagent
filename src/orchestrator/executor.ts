import type { OrchestratorTask, ProjectBoard } from './types.js';
import type { RunLogger } from '../logger.js';
import type { WorkerName } from '../types.js';
import { createWorktree } from '../git/worktree.js';
import { buildImplementationPrompt, buildRepairPrompt } from '../prompt.js';
import { sanitizeTicketId } from '../git/worktree.js';

/**
 * Executor role: implements one small precise task in its own worktree
 * (branch devagent/<task-id>), verifies with the repo's test suite (G1),
 * and reports done/failed with evidence. Fresh worktree per retry.
 */

export async function executeTask(args: {
  task: OrchestratorTask;
  board: ProjectBoard;
  repoPath: string;
  timeoutMs: number;
  log: RunLogger;
  executor: WorkerName;
}): Promise<{ ok: boolean; worktreePath?: string; detail?: string }> {
  const { task, repoPath, timeoutMs, log, executor } = args;
  const { getWorker } = await import('../workers/index.js');
  const { runTestGate } = await import('../validation/test-gate.js');

  const ticket = {
    id: task.id,
    title: task.title,
    description: [
      task.prompt,
      task.expectedOutput ? `\nCompletion check (must hold when you finish):\n${task.expectedOutput}` : '',
    ].join(''),
    labels: ['orchestrated'],
    acceptanceCriteria: task.expectedOutput ? [task.expectedOutput] : [],
    url: '',
    trackerInternalId: task.id,
  };
  const plan = { ticket, classification: 'endpoint-only' as const, tasks: [], summary: task.title };

  let worktreePath: string | undefined;
  try {
    // branch devagent/<TASKID>-<attempt> keeps retries isolated (fresh tree)
    const wt = await createWorktree(repoPath, `${sanitizeTicketId(task.id)}-a${task.attempts}`);
    worktreePath = wt.worktreePath;
  } catch (err) {
    return { ok: false, detail: `worktree creation failed: ${(err as Error).message}` };
  }

  const worker = getWorker(executor);
  const prompt = buildImplementationPrompt(plan);
  const maxAttempts = 2; // in-worker repair loop; scheduler owns cross-wave retries

  for (let attempt = 1; attempt <= maxAttempts; attempt++) {
    const result = await worker.spawn({
      prompt: attempt === 1 ? prompt : buildRepairPrompt(plan, attempt - 1, 'previous attempt failed the test gate'),
      cwd: worktreePath,
      timeoutMs,
    });
    if (result.timedOut || result.exitCode !== 0) continue;
    const g1 = await runTestGate(worktreePath, timeoutMs);
    if (g1.passed) {
      // Commit the work: uncommitted changes never reach merge-back
      // (live-smoke lesson: gate passed in-tree but merge integrated nothing).
      const { spawnCli } = await import('../workers/spawn-utils.js');
      await spawnCli('git', ['add', '-A'], { cwd: worktreePath, timeoutMs: 30_000 });
      const commit = await spawnCli(
        'git',
        ['commit', '-m', `task ${task.id}: ${task.title}`, '--no-verify'],
        { cwd: worktreePath, timeoutMs: 30_000 },
      );
      if (commit.exitCode !== 0 && !/nothing to commit/.test(commit.stderr + commit.stdout)) {
        return { ok: false, worktreePath, detail: `commit failed: ${commit.stderr.slice(0, 200)}` };
      }
      return { ok: true, worktreePath };
    }
    log.warn('task', `${task.id} G1 failed on executor attempt ${attempt}`, {});
  }
  return { ok: false, worktreePath, detail: 'test gate failed after executor attempts' };
}
