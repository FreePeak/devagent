import { claimNextPending, readTask, setTaskStatus, updateTask, type QueuedTask } from './queue.js';
import { RunLogger } from './logger.js';
import { syntheticTicketFromPrompt } from './task.js';
import type { PipelineDeps } from './pipeline.js';
import { loadConfig } from './config.js';
import { runPipeline } from './pipeline.js';

export interface ConsumeOptions {
  repoPath: string;
  autoPr: boolean;
  autoMerge: boolean;
  maxLoops: number;
  timeoutMs: number;
  /** Claim identity for queue file */
  workerId?: string;
}

export interface ConsumeResult {
  ok: boolean;
  taskId?: string;
  detail: string;
  prUrl?: string;
  merged?: boolean;
}

/** Claim one pending task and run it through the pipeline -> PR -> optional auto-merge. */
export async function consumeOnce(opts: ConsumeOptions): Promise<ConsumeResult> {
  const workerId = opts.workerId ?? `consume-${process.pid}`;
  const task: QueuedTask | null = claimNextPending(opts.repoPath, workerId);
  if (!task) {
    return { ok: true, detail: 'no pending tasks' };
  }

  const log = new RunLogger();
  log.info('consume', `Claimed ${task.id}: ${task.title}`, { workerId });

  try {
    const result = await runQueuedTask(task, opts, log);
    if (result.ok) {
      setTaskStatus(opts.repoPath, task.id, 'done');
      updateTask(opts.repoPath, task.id, { lastError: undefined });
    } else {
      setTaskStatus(opts.repoPath, task.id, 'failed', result.detail);
    }
    return { ok: result.ok, taskId: task.id, detail: result.detail, prUrl: result.prUrl, merged: result.merged };
  } catch (err) {
    const msg = (err as Error).message;
    setTaskStatus(opts.repoPath, task.id, 'failed', msg);
    log.error('consume', `Task ${task.id} crashed: ${msg}`);
    return { ok: false, taskId: task.id, detail: `crashed: ${msg}` };
  }
}

async function runQueuedTask(
  task: QueuedTask,
  opts: ConsumeOptions,
  log: RunLogger,
): Promise<{ ok: boolean; detail: string; prUrl?: string; merged?: boolean }> {
  const ticket = {
    id: task.id,
    title: task.title,
    description: task.goal + (task.description ? `\n\n${task.description}` : ''),
    labels: [] as string[],
    acceptanceCriteria: task.acceptanceCriteria,
    url: '',
    trackerInternalId: task.id,
  };

  // Build PipelineDeps that use the queued goal as synthetic ticket (no Linear fetch).
  // We reuse the task prompt path: implementStage + gates come from deps-style.
  const cfg = {
    ticketId: task.id,
    repoPath: opts.repoPath,
    worker: loadConfig(opts.repoPath).worker,
    autoPr: opts.autoPr,
    interactive: false,
    maxLoops: opts.maxLoops,
    timeoutMs: opts.timeoutMs,
    dryRun: false,
    cleanup: (loadConfig(opts.repoPath).cleanup ?? 'auto') as 'auto' | 'keep' | 'always',
    dropOrcaWorkspace: Boolean(loadConfig(opts.repoPath).dropOrcaWorkspace),
  };

  // Use buildDeps-equivalent but with fetchTicket stubbed to the queued ticket
  const creds = (await import('./config.js')).loadCredentials();
  const { buildDeps } = await import('./deps.js');
  const deps: PipelineDeps = {
    ...buildDeps(creds as never, cfg, log),
    fetchTicket: async () => ticket,
  };

  const outcomes = await runPipeline(cfg, deps, log);
  const failed = outcomes.find((o) => o.stage === 'failed') as { stage: 'failed'; reason: string } | undefined;
  if (failed) {
    return { ok: false, detail: failed.reason };
  }

  const publish = outcomes.find((o) => o.stage === 'publish') as { stage: 'publish'; prUrl?: string; note: string } | undefined;
  const prUrl = publish?.prUrl;

  if (opts.autoMerge && prUrl) {
    try {
      const { autoMergePr } = await import('./integrations/github.js');
      await autoMergePr(opts.repoPath, prUrl);
      return { ok: true, detail: `done: ${task.id} -> ${prUrl} (auto-merged)`, prUrl, merged: true };
    } catch (err) {
      return { ok: true, detail: `done: ${task.id} -> ${prUrl} (auto-merge failed: ${(err as Error).message})`, prUrl, merged: false };
    }
  }

  // Self-update hook: after green merge, optionally pull+build
  if (prUrl) {
    const config = loadConfig(opts.repoPath);
    if (config.selfUpdate) {
      try {
        const { runSelfUpdate } = await import('./self-update.js');
        await runSelfUpdate(opts.repoPath, log);
      } catch (err) {
        log.warn('consume', `self-update skipped: ${(err as Error).message}`);
      }
    }
  }

  if (prUrl) return { ok: true, detail: `done: ${task.id} -> ${prUrl}`, prUrl };
  const implement = outcomes.find((o) => o.stage === 'implement') as { ok: boolean } | undefined;
  if (implement && !implement.ok) return { ok: false, detail: `implementation failed for ${task.id}` };
  return { ok: true, detail: `done: ${task.id} (no PR: missing GITHUB_TOKEN or branch)`, prUrl: undefined };
}
