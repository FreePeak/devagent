import type { WorkerName } from '../types.js';

/**
 * Orchestrator workflow types: a planner agent decomposes a goal into a
 * dependency DAG of small precise tasks; executor agents implement them in
 * isolated worktrees; the board persists so runs survive restarts.
 */

/**
 * 'untrusted': executor finished and its own gates passed, but no independent
 * audit has confirmed completion yet (LongHorizon-Harness lesson: executor
 * claims never directly become trusted state). 'ask': a decision needs human
 * input before this branch can proceed.
 */
export type TaskStatus =
  | 'pending'
  | 'ready'
  | 'dispatched'
  | 'untrusted'
  | 'done'
  | 'failed'
  | 'blocked'
  | 'ask';

/** Per-criterion evidence from an independent auditor (read-only). */
export interface CriterionResult {
  criterion: string;
  met: boolean;
  /** Environmental proof: command output, file content, test result */
  evidence: string;
}

/**
 * Independent audit verdict. A task may only flip to 'done' when verdict is
 * 'pass' AND integrity is 'clean' (LH-Harness rule: a mutated-workspace
 * report can never support a completed record).
 */
export interface AuditVerdict {
  verdict: 'pass' | 'fail';
  integrity: 'clean' | 'suspect' | 'violation';
  criteriaResults: CriterionResult[];
  /** Auditor's one-paragraph account of what it actually checked */
  summary: string;
}

export interface OrchestratorTask {
  id: string;
  title: string;
  /** Precise implementation instructions for the executor worker */
  prompt: string;
  /**
   * Machine-checkable acceptance criteria. The auditor verifies each
   * criterion independently; itemized criteria make "confidently verified
   * wrong answer" less likely than one freeform expected-output string.
   */
  acceptanceCriteria?: string[];
  /** Things the executor must NOT do inside its bounded contract */
  boundaryConstraints?: string[];
  /** Machine-verifiable completion signal (CrewAI expected_output lesson) */
  expectedOutput?: string;
  /** Task ids that must be done before this one becomes ready */
  dependsOn: string[];
  status: TaskStatus;
  attempts: number;
  worktreePath?: string;
  failureDetail?: string;
  /** Latest independent audit; set once the task leaves 'untrusted' via audit */
  audit?: AuditVerdict;
  /**
   * Targeted re-contracting: what the previous attempt failed to prove, so
   * the retry closes the gap instead of redoing blind work.
   */
  evidenceGaps?: string[];
}

export interface ProjectBoard {
  goal: string;
  createdAt: string;
  updatedAt: string;
  roles: {
    planner: WorkerName;
    executor: WorkerName;
    /** Independent auditor role; absent boards predate evidence gating */
    auditor?: WorkerName;
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
    // upstream failed permanently or paused on human input -> this can never run
    if (deps.some((d) => d!.status === 'failed' || d!.status === 'blocked' || d!.status === 'ask')) {
      return { ...t, status: 'blocked' as const };
    }
    return t;
  });
}
