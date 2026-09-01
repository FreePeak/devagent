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
  /** 'ask': completion cannot be judged without human input/authorization */
  verdict: 'pass' | 'fail' | 'ask';
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
  /** How many planner-written recovery contracts this task has been granted */
  recoveries?: number;
  /**
   * Consecutive audit failures repeating the same primary gap (SWE-agent
   * lesson: recovery odds decay after repeated failures — escalate to a
   * recovery re-contract instead of burning retries on the same wall).
   */
  repeatGaps?: number;
  /**
   * Executor failure surface (PRD:775): set when the executor aborted this
   * task via taskInterrupt — N+ identical trailing trail.jsonl failure
   * signatures caused the worker to be aborted and the post-mortem threaded
   * into the ledger. The compact evidence (failure class, gate excerpt,
   * attempts, trail hash) survives board archive for the next bridge.
   */
  interrupt?: {
    failureClass: string;
    lastGateExcerpt: string;
    attempts: number;
    trailHash: string;
  };
}

/** Branch/worktree attempt suffix; recovery grants extend it to stay collision-free. */
export function attemptSuffix(attempts: number, recoveries = 0): string {
  return `a${attempts}${recoveries > 0 ? `r${recoveries}` : ''}`;
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
    // `pending` and dependency-induced `blocked` are both derived states:
    // re-derive every pass so a task blocked by an upstream `ask`/`blocked`
    // is promoted once that upstream reaches `done` (loop-55: T2 answered ->
    // T3 stayed blocked forever because only `pending` was recomputed).
    if (t.status !== 'pending' && t.status !== 'blocked') return t;
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

function byIdValue(t: OrchestratorTask): OrchestratorTask {
  return t;
}
