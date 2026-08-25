/**
 * Shared type contracts for DevAgent.
 * Every module (workers, validation, integrations, pipeline) depends on these.
 */

// ---------- Tickets ----------

export interface TicketSpec {
  /** Tracker-native identifier, e.g. "LINEAR-204" */
  id: string;
  title: string;
  description: string;
  labels: string[];
  acceptanceCriteria: string[];
  /** Raw tracker URL for linking back */
  url?: string;
  /** Tracker-internal object id (e.g. Linear UUID) required for mutations like comments */
  trackerInternalId?: string;
}

export type TicketClass = 'endpoint-only' | 'migration-required' | 'consumer-only';

// ---------- Workers ----------

export type WorkerName = 'claude-code' | 'opencode';

export interface WorkerSpawnOptions {
  prompt: string;
  cwd: string;
  timeoutMs: number;
  maxSteps?: number;
  /** Model override forwarded to the worker CLI (provider/model). */
  model?: string;
  env?: Record<string, string>;
  /**
   * Total launches allowed when the worker auto-resumes a session killed by
   * an API failure (claude-code adapter). Includes the first launch.
   */
  apiMaxAttempts?: number;
}

export interface WorkerEvent {
  type: string;
  [key: string]: unknown;
}

export interface WorkerResult {
  exitCode: number;
  events: WorkerEvent[];
  /** Final text output from the worker, if any */
  resultText: string | null;
  sessionId: string | null;
  durationMs: number;
  timedOut: boolean;
}

/** Uniform contract over heterogeneous headless coding-agent CLIs. */
export interface WorkerAdapter {
  readonly name: WorkerName;
  spawn(opts: WorkerSpawnOptions): Promise<WorkerResult>;
}

// ---------- Validation ----------

export type Severity = 'critical' | 'high' | 'medium' | 'low';

export interface Finding {
  ruleId: string;
  severity: Severity;
  message: string;
  /** File the finding applies to, when known */
  file?: string;
  line?: number;
}

export interface GateResult {
  gate: 'G1-tests' | 'G2-migration-apply' | 'G3-migration-static' | 'G4-async-review';
  passed: boolean;
  /**
   * True when the gate did not actually run (missing prerequisites) — evidence
   * consumers must not treat `passed` as verified-green when this is set.
   */
  skipped?: boolean;
  findings: Finding[];
  detail?: string;
}

// ---------- Run ----------

export type RunStage =
  | 'fetch'
  | 'plan'
  | 'implement'
  | 'validate'
  | 'publish'
  | 'failed'
  | 'clarify'
  | 'task'
  | 'audit';

export interface RunConfig {
  ticketId: string;
  repoPath: string;
  worker: WorkerName | 'both';
  autoPr: boolean;
  interactive: boolean;
  maxLoops: number;
  timeoutMs: number;
  dryRun: boolean;
  /** Model override forwarded to worker CLIs (provider/model). */
  model?: string;
}

export interface LogEntry {
  ts: string;
  runId: string;
  stage: RunStage;
  level: 'info' | 'warn' | 'error';
  message: string;
  data?: Record<string, unknown>;
}
