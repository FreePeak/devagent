import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { QueuedTask } from '../queue.js';
import { listTasks, setTaskStatus } from '../queue.js';
import type { ProjectBoard, OrchestratorTask } from './types.js';
import { loadBoard, saveBoard, createBoard } from './store.js';
import { fallbackPlan, parsePlan } from './planner.js';
import type { WorkerName } from '../types.js';

/**
 * Bridge: turn a queued idea (goal string) into a board DAG via the planner,
 * or flatten board tasks back to queue items for Orca workers watching the queue.
 * Mirrors store.ts atomic writes, never logs secrets.
 */

export interface BridgeOptions {
  repoPath: string;
  /** Boards are keyed by the queued goal so re-runs are idempotent. */
  boardGoalKey?: (task: QueuedTask) => string;
  /** How the planner is invoked for a goal; tests inject a stub. */
  planner?: (goal: string) => Promise<OrchestratorTask[] | null>;
}

function defaultPlanner(goal: string): Promise<OrchestratorTask[] | null> {
  // Synchronous fallback-parse never needs a worker: wrap deterministic parse + fallback
  return Promise.resolve(fallbackPlan(goal));
}

export function queuedGoalString(t: QueuedTask): string {
  const ac = t.acceptanceCriteria?.length ? `\nAcceptance criteria:\n${t.acceptanceCriteria.map((c) => `- ${c}`).join('\n')}` : '';
  return `${t.goal}${ac}`;
}

export interface BridgeResult {
  boardPath: string;
  tasksWritten: number;
  created: boolean;
  /** Tasks were already present for this goal-backed board (idempotency) */
  idempotent: boolean;
}

/** Idempotent: if a board already exists for this repo, reuse it; else plan one queued goal into a board. */
export async function bridgeQueueToBoard(
  oldestPending: QueuedTask,
  opts: BridgeOptions,
): Promise<BridgeResult> {
  const repoPath = opts.repoPath;
  const boardPath = join(repoPath, '.devagent-project.json');
  const existing = loadBoard(repoPath);
  if (existing) {
    return { boardPath, tasksWritten: existing.tasks.length, created: false, idempotent: true };
  }
  const goal = queuedGoalString(oldestPending);
  const planner = opts.planner ?? defaultPlanner;
  const planned = await planner(goal);
  let tasks: OrchestratorTask[] = planned && Array.isArray(planned) && planned.length > 0 ? planned : fallbackPlan(goal);
  if (tasks.length > 12) tasks = tasks.slice(0, 12);

  const roles: ProjectBoard['roles'] = { planner: 'pi' as WorkerName, executor: 'pi' as WorkerName };
  const board = createBoard(goal, tasks, roles);
  saveBoard(repoPath, board);
  // Retire the source queue item: the board now owns this goal, and leaving it
  // pending would make the builder lane double-build the same idea.
  setTaskStatus(repoPath, oldestPending.id, 'done');
  return { boardPath, tasksWritten: tasks.length, created: true, idempotent: false };
}

/** Sync dry path over the live queue: pick oldest pending and bridge it (no-op when board exists). */
export async function bridgeIfQueued(repoPath: string, planner?: BridgeOptions['planner']): Promise<BridgeResult | null> {
  const pending = listTasks(repoPath).filter((t: QueuedTask) => t.status === 'pending').sort((a: QueuedTask, b: QueuedTask) => a.createdAt.localeCompare(b.createdAt));
  if (pending.length === 0) return null;
  return bridgeQueueToBoard(pending[0]!, { repoPath, planner });
}

/** Evidence source check for tests: does progress.md currently want a board section? */
export function hasBoard(repoPath: string): boolean {
  return existsSync(join(repoPath, '.devagent-project.json'));
}
