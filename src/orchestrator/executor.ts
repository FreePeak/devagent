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
  lessonsMaxChars?: number;
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
  const lessons = loadLessons(repoPath, args.lessonsFile, args.lessonsMaxChars);
  const prompt = buildImplementationPrompt(plan, lessons);
  const maxAttempts = 2; // in-worker repair loop; scheduler owns cross-wave retries
  const { loadConfig, herdrEnabled } = await import('../config.js');
  const fullCfg = loadConfig(repoPath);
  const resilienceCfg = fullCfg.resilience;
  const noProgressTimeoutMs = resilienceCfg?.noProgressTimeoutMs ?? 10 * 60_000;
  const apiMaxAttempts = resilienceCfg?.apiMaxAttempts;
  const useHerdr = herdrEnabled(fullCfg);
  const model = fullCfg.model;
  const variant = fullCfg.variant;

  // Transient check uses classify module (sync after first import)
  const { isTransientProviderError: isTransient } = await import('../resilience/classify.js');
  const { isNonRetryableApiError: isNonRetryable } = await import('../sessionguard/events.js');

  let attempt = 1;
  let logicAttempts = 0;
  let infraRetries = 0;
  let lastGateDetail: string | undefined;
  while (logicAttempts < maxAttempts || infraRetries < 200) {
    const spawnOpts: Record<string, unknown> = {
      prompt: attempt === 1 ? prompt : buildRepairPrompt(plan, attempt - 1, 'previous attempt failed the test gate', lessons),
      cwd: worktreePath,
      timeoutMs,
      ...(apiMaxAttempts !== undefined ? { apiMaxAttempts } : {}),
      ...(noProgressTimeoutMs !== undefined ? { noProgressTimeoutMs } : {}),
      ...(useHerdr ? { herdr: true } : {}),
      ...(model ? { model } : {}),
      ...(variant ? { variant } : {}),
    };
    const result = await worker.spawn(spawnOpts as never);
    try {
      const { findStaleWorkerPids, killStaleProcessTree } = await import('../resilience/reaper.js');
      // Repo-scoped: only headless workers whose cwd is inside this repo's
      // worktrees may be reaped — never the user's interactive sessions.
      const stale = findStaleWorkerPids(noProgressTimeoutMs || 60_000, {
        cwdPrefix: join(repoPath, '.devagent-worktrees'),
      });
      // cwdPrefix guard already filters — skip isDevagentWorker false above
      // For herdr panes the cwd is remote pane cwd, which lsof may not resolve;
      // skip kill for herdr-detected panes (herdr.ts watchdog handles those).
      for (const s of stale) if (!useHerdr) killStaleProcessTree(s.pid);
    } catch {}
    if (result.timedOut || result.exitCode !== 0 || result.errorText) {
      // Inspect both resultText and errorText: the proxy surfaces transient
      // provider outages (rate-limited empty streams) as stderr-only with no
      // .result field, so resultText alone misses them and the task used to
      // false-fail after 2 attempts instead of retrying.
      const text = [result.resultText, result.errorText].filter(Boolean).join('\n');
      if (
        result.timedOut ||
        (text && !isNonRetryable(text) && (isTransient(text) || isTransient(result.errorText ?? null)))
      ) {
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
    lastGateDetail = g1.detail?.slice(0, 600);
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
    logicAttempts++;
    attempt++;
    if (logicAttempts >= maxAttempts) break;
  }
  return { ok: false, worktreePath, detail: lastGateDetail ?? 'test gate failed after executor attempts' };
}
