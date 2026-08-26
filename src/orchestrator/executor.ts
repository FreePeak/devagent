import { join } from 'node:path';
import type { OrchestratorTask, ProjectBoard } from './types.js';
import type { RunLogger } from '../logger.js';
import type { WorkerName } from '../types.js';
import { createWorktree } from '../git/worktree.js';
import { buildImplementationPrompt, buildRepairPrompt, loadLessons } from '../prompt.js';
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
  /** Repo-local lessons file override (see loadLessons). */
  lessonsFile?: string;
  log: RunLogger;
  executor: WorkerName;
}): Promise<{ ok: boolean; worktreePath?: string; detail?: string }> {
  const { task, repoPath, timeoutMs, log, executor } = args;
  const { getWorker } = await import('../workers/index.js');
  const { runTestGate } = await import('../validation/test-gate.js');

  const criteria = [...(task.acceptanceCriteria ?? []), ...(task.expectedOutput ? [task.expectedOutput] : [])];
  const ticket = {
    id: task.id,
    title: task.title,
    description: [
      task.prompt,
      task.boundaryConstraints?.length
        ? `\nBoundary constraints (must respect):\n${task.boundaryConstraints.map((c) => `- ${c}`).join('\n')}`
        : '',
      // Targeted re-contracting: retry closes the audited gap, not blind redo
      task.evidenceGaps?.length
        ? `\nPrevious attempt failed independent audit. Close these specific gaps:\n${task.evidenceGaps.map((g) => `- ${g}`).join('\n')}`
        : '',
    ].join(''),
    labels: ['orchestrated'],
    acceptanceCriteria: criteria,
    url: '',
    trackerInternalId: task.id,
  };
  const plan = { ticket, classification: 'endpoint-only' as const, tasks: [], summary: task.title };

  let worktreePath: string | undefined;
  try {
    // branch devagent/<TASKID>-a<attempt>[r<recovery>] keeps retries isolated
    // (fresh tree); recovery grants extend the suffix to avoid branch reuse
    const { attemptSuffix } = await import('./types.js');
    const wt = await createWorktree(repoPath, `${sanitizeTicketId(task.id)}-${attemptSuffix(task.attempts, task.recoveries)}`);
    worktreePath = wt.worktreePath;
  } catch (err) {
    return { ok: false, detail: `worktree creation failed: ${(err as Error).message}` };
  }

  const worker = getWorker(executor);
  const lessons = loadLessons(repoPath, args.lessonsFile);
  const prompt = buildImplementationPrompt(plan, lessons);
  const maxAttempts = 2; // in-worker repair loop; scheduler owns cross-wave retries
  const { loadConfig, herdrEnabled } = await import('../config.js');
  const fullCfg = loadConfig(repoPath);
  const resilienceCfg = fullCfg.resilience;
  const noProgressTimeoutMs = resilienceCfg?.noProgressTimeoutMs ?? 10 * 60_000;
  const apiMaxAttempts = resilienceCfg?.apiMaxAttempts;
  const useHerdr = herdrEnabled(fullCfg);

  // Transient check uses classify module (sync after first import)
  const { isTransientProviderError: isTransient } = await import('../resilience/classify.js');
  const { isNonRetryableApiError: isNonRetryable } = await import('../sessionguard/events.js');

  let attempt = 1;
  let logicAttempts = 0;
  let infraRetries = 0;
  // Last gate failure detail, threaded into the repair prompt so retries
  // target the actual gap (G1 output or G5 clause results).
  let gateFailureDetail = 'previous attempt failed the test gate';
  while (logicAttempts < maxAttempts || infraRetries < 200) {
    const spawnOpts: Record<string, unknown> = {
      prompt: attempt === 1 ? prompt : buildRepairPrompt(plan, attempt - 1, gateFailureDetail, lessons),
      cwd: worktreePath,
      timeoutMs,
      ...(apiMaxAttempts !== undefined ? { apiMaxAttempts } : {}),
      ...(noProgressTimeoutMs !== undefined ? { noProgressTimeoutMs } : {}),
      ...(useHerdr ? { herdr: true } : {}),
    };
    const result = await worker.spawn(spawnOpts as never);
    try {
      const { findStaleWorkerPids, killStaleProcessTree } = await import('../resilience/reaper.js');
      // Repo-scoped: only headless workers whose cwd is inside this repo's
      // worktrees may be reaped — never the user's interactive sessions.
      const stale = findStaleWorkerPids(noProgressTimeoutMs || 60_000, {
        cwdPrefix: join(repoPath, '.devagent-worktrees'),
      });
      for (const s of stale) killStaleProcessTree(s.pid);
    } catch {}
    if (result.timedOut || result.exitCode !== 0) {
      const text = result.resultText ?? '';
      if (result.timedOut || (text && !isNonRetryable(text) && isTransient(text))) {
        infraRetries++;
        const { backoffDelay } = await import('../sessionguard/backoff.js');
        await new Promise((r) => setTimeout(r, backoffDelay(infraRetries)));
        attempt++;
        continue;
      }
      logicAttempts++;
      attempt++;
      if (logicAttempts >= maxAttempts && infraRetries >= 200) break;
      if (logicAttempts >= maxAttempts) break;
      continue;
    }
    const g1 = await runTestGate(worktreePath, timeoutMs);
    if (g1.passed) {
      // G5 browser evidence channel: runs only when devagent.json declares
      // browserCheck; returns null otherwise (behavior byte-identical).
      const { runBrowserGate } = await import('../validation/browser-gate.js');
      const g5 = await runBrowserGate({
        worktreePath,
        evidenceDir: join(repoPath, '.devagent', 'runs', sanitizeTicketId(task.id), 'g5'),
        timeoutMs,
      });
      if (g5 && !g5.passed) {
        log.warn('task', `${task.id} G5 failed on executor attempt ${attempt}`, {});
        gateFailureDetail = `browser gate failed: ${g5.detail ?? ''}`;
        logicAttempts++;
        attempt++;
        if (logicAttempts >= maxAttempts) break;
        continue;
      }
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
      // Surface G5 evidence to the auditor via the success detail (it flows
      // into the audit prompt as untrusted executor context).
      return { ok: true, worktreePath, ...(g5 ? { detail: g5.detail } : {}) };
    }
    log.warn('task', `${task.id} G1 failed on executor attempt ${attempt}`, {});
    gateFailureDetail = `test gate failed: ${g1.detail ?? ''}`;
    logicAttempts++;
    attempt++;
    if (logicAttempts >= maxAttempts) break;
  }
  return { ok: false, worktreePath, detail: 'test gate failed after executor attempts' };
}
