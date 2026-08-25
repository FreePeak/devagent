import type { RunLogger } from './logger.js';
import { runPipeline } from './pipeline.js';
import type { PipelineDeps } from './pipeline.js';
import type { TicketSpec } from './types.js';

/**
 * Orchestrator-facing one-shot task mode: any external harness (Orca,
 * CI, another DevAgent) can drive DevAgent with a raw prompt and a cwd,
 * the same contract it uses for claude-code/opencode workers:
 *   devagent task --prompt "add rate limiting to /api" [--auto-pr]
 * The prompt becomes a synthetic ticket; fetch/post stages are local.
 */

export interface TaskOptions {
  prompt: string;
  repoPath: string;
  autoPr: boolean;
  autoMerge?: boolean;
  maxLoops: number;
  timeoutMs: number;
  log: RunLogger;
  /**
   * Task identity: names the synthetic ticket, worktree (.devagent-worktrees/<id>)
   * and branch (devagent/<id>). Concurrent dispatches (pooled remote hosts,
   * parallel Orca runs) must not share one id — git refuses to check a branch
   * out twice across worktrees of the same repo.
   */
  taskId?: string;
}

/**
 * Collision-free default task id (loop 66): the previous constant `TASK` made
 * every concurrent run fight over `.devagent-worktrees/TASK` and the branch
 * `devagent/TASK` ("already used by worktree at ..."). Epoch36 + random suffix
 * keeps ids unique per invocation while staying sanitize-safe.
 */
export function defaultTaskId(now: () => number = Date.now): string {
  const epoch36 = now().toString(36);
  const rand = Math.random().toString(36).slice(2, 6);
  return `TASK-${epoch36}-${rand}`;
}

export function syntheticTicketFromPrompt(prompt: string, taskId?: string): TicketSpec {
  // First line becomes the title; whole prompt stays as description
  const [firstLine, ...rest] = prompt.trim().split('\n');
  const id = taskId ?? defaultTaskId();
  return {
    id,
    title: firstLine!.slice(0, 80),
    description: rest.join('\n').trim() || firstLine!,
    labels: ['orchestrated'],
    acceptanceCriteria: [],
    url: '',
    trackerInternalId: id,
  };
}

export interface TaskDeps {
  runPipelineDeps: PipelineDeps;
  /** Worker dispatch identical to deps.ts implementStage; injected by caller. */
  implementStage(cfg: TaskOptions, ticket: TicketSpec, log: RunLogger): Promise<{ ok: boolean; worker: string; attempts: number; worktreePath?: string }>;
  publishStage?(cfg: TaskOptions, ticket: TicketSpec, impl: { ok: boolean; worktreePath?: string }): Promise<string | undefined>;
}

/** Remote boundary of task publishing, injected so the logic stays testable over real git fixtures. */
export interface TaskPublishDeps {
  commitAllChanges(worktreePath: string, message: string): Promise<boolean>;
  currentBranch(worktreePath: string): Promise<string>;
  listChangedFiles(worktreePath: string, baseBranch: string): Promise<string[]>;
  pushBranch(repoPath: string, branch: string): Promise<void>;
  createPr(o: { repoPath: string; branch: string; title: string; body: string }): Promise<string>;
}

export interface TaskPublishOptions {
  repoPath: string;
  prompt: string;
  /** Base used for the empty-diff guard and PR evidence. */
  baseBranch: string;
  log: RunLogger;
}

/**
 * Publish a finished task worktree as a PR (dogfood loops 7-9 lesson):
 * - commits whatever the worker left uncommitted — agents routinely edit
 *   without committing, and an uncommitted change silently ships as an empty PR;
 * - pushes the branch the worktree ACTUALLY has checked out. The previous
 *   implementation invented `devagent/task-<runId>`, a ref nobody ever
 *   created, so every push died with "src refspec does not match any";
 * - refuses to open a PR when the diff vs base is empty.
 */
export async function publishTaskBranch(
  opts: TaskPublishOptions,
  impl: { ok: boolean; worktreePath?: string },
  io: TaskPublishDeps,
): Promise<string | undefined> {
  if (!impl.worktreePath) return undefined;

  const title = opts.prompt.split('\n')[0]!.slice(0, 80);
  await io.commitAllChanges(impl.worktreePath, `devagent(task): ${title}`);

  const branch = await io.currentBranch(impl.worktreePath);
  const changed = await io.listChangedFiles(impl.worktreePath, opts.baseBranch);
  if (changed.length === 0) {
    opts.log.warn('task', 'nothing changed vs base; skipping PR', { branch, baseBranch: opts.baseBranch });
    return undefined;
  }

  await io.pushBranch(opts.repoPath, branch);
  return io.createPr({
    repoPath: opts.repoPath,
    branch,
    title,
    body: `Automated task via \`devagent task\`.\n\n## Prompt\n${opts.prompt}`,
  });
}

/** Minimal pipeline execution for prompt-driven tasks (no tracker round-trip). */
export async function runTask(opts: TaskOptions, deps: TaskDeps): Promise<{ ok: boolean; prUrl?: string; note: string }> {
  const ticket = syntheticTicketFromPrompt(opts.prompt, opts.taskId);
  opts.log.info('task', `Task starting`, { title: ticket.title, taskId: ticket.id });

  const impl = await deps.implementStage(opts, ticket, opts.log);
  if (!impl.ok) {
    return { ok: false, note: 'implementation failed validation' };
  }

  if (!opts.autoPr) {
    return { ok: true, note: `worktree ready for review: ${impl.worktreePath ?? '(repo root)'}` };
  }
  const prUrl = await deps.publishStage?.(opts, ticket, impl);
  return { ok: true, prUrl, note: prUrl ? `PR opened: ${prUrl}` : 'no remote credentials; branch preserved locally' };
}
