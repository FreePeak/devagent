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
  /** Variant override forwarded to opencode (--variant or #variant). */
  variant?: string;
  env?: Record<string, string>;
  /**
   * Total launches allowed when the worker auto-resumes a session killed by
   * an API failure (claude-code adapter). Includes the first launch.
   */
  apiMaxAttempts?: number;
  /**
   * Watchdog: kill the child when no output arrives for this long. 0 disables.
   * When set, `timedOut` from the watchdog is treated as a transient provider
   * failure and retried forever (until wall-clock budget or non-retryable error).
   * Env `DEVAGENT_NO_PROGRESS_TIMEOUT_MS` provides a default when unset.
   */
  noProgressTimeoutMs?: number;
  /**
   * Run this worker launch inside a herdr pane (persistent terminal runtime)
   * instead of a direct child process. Set by the executor from config
   * `herdr.enabled`; see src/integrations/herdr.ts.
   */
  herdr?: boolean;
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

/** Opt-in browser evidence channel (gate G5), configured in devagent.json. */
export interface BrowserCheckConfig {
  /** Shell command that boots the repo's dev server in the worktree. */
  start: string;
  /** Absolute URL the gate loads headlessly (must be http/https). */
  url: string;
  /** Clauses of the form "text:<substring>" or "selector:<css>"; all must hold. */
  expect: string[];
  /** Save a full-page PNG into the run's evidence directory (default true). */
  screenshot?: boolean;
}

export interface GateResult {
  gate: 'G1-tests' | 'G2-migration-apply' | 'G3-migration-static' | 'G4-async-review' | 'G5-browser';
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
  | 'audit'
  | 'consume'
  | 'self-update'
  | 'scout'
  | 'queue'
  | 'create';

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
  /** Variant override for opencode (e.g. max). Encoded as --variant or #variant. */
  variant?: string;
  /** Post-run worktree disposal policy; default 'auto' (remove on success). */
  cleanup?: 'auto' | 'keep' | 'always';
  /** Drop the enclosing Orca workspace via orca-cli when repoPath is Orca-managed. */
  dropOrcaWorkspace?: boolean;
}

export interface LogEntry {
  ts: string;
  runId: string;
  stage: RunStage;
  level: 'info' | 'warn' | 'error';
  message: string;
  data?: Record<string, unknown>;
}
