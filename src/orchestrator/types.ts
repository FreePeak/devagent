import type { WorkerName } from '../types.js';

/**
 * Orchestrator workflow types: a planner agent decomposes a goal into a
 * dependency DAG of small precise tasks; executor agents implement them in
 * isolated worktrees; the board persists so runs survive restarts.
 */

export type TaskStatus = 'pending' | 'ready' | 'dispatched' | 'done' | 'failed' | 'blocked';

export interface OrchestratorTask {
  id: string;
  title: string;
  /** Precise implementation instructions for the executor worker */
  prompt: string;
  /** Machine-verifiable completion signal (CrewAI expected_output lesson) */
  expectedOutput?: string;
  /** Task ids that must be done before this one becomes ready */
  dependsOn: string[];
  status: TaskStatus;
  attempts: number;
  worktreePath?: string;
  failureDetail?: string;
}

export interface ProjectBoard {
  goal: string;
  createdAt: string;
  updatedAt: string;
  roles: {
    planner: WorkerName;
    executor: WorkerName;
  };
  tasks: OrchestratorTask[];
}

export const BOARD_FILE = '.devagent-project.json';

/** Tasks whose dependencies are all done become ready. */
export function recomputeReadiness(tasks: OrchestratorTask[]): OrchestratorTask[] {
  const byId = new Map(tasks.map((t) => [t.id, t]));
  return tasks.map((t) => {
    if (t.status !== 'pending') return t;
    const deps = t.dependsOn.map((d) => byId.get(d));
    if (deps.some((d) => !d)) {
      // dangling dependency reference: block rather than run blind
      return { ...t, status: 'blocked' as const };
    }
    if (deps.every((d) => d!.status === 'done')) {
      return { ...t, status: 'ready' as const };
    }
    // upstream failed permanently -> this can never run
    if (deps.some((d) => d!.status === 'failed' || d!.status === 'blocked')) {
      return { ...t, status: 'blocked' as const };
    }
    return t;
  });
}
