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

// ---------- Executor failures ----------

/**
 * Executor failure class (PRD:775 / Q24 taxonomy mirror from PR #100). A
 * structured classifier for the compact post-mortem written to the ledger on
 * taskInterrupt, so operators see WHY a task died, not just `attempts: 3`.
 * Mirrors the Q24 error taxonomy used by the CI-Fixer
 * (`src/integrations/autopr.ts`): one bounded vocabulary of terminal failure
 * modes instead of freeform detail strings.
 */
export type ExecutorFailureClass =
  /** Repo test suite failed on the gate (G1) after the worker reported done. */
  | 'test-gate'
  /** Worker exited non-zero or produced an error the classifier cannot retry. */
  | 'worker-error'
  /** Worker timed out (wall-clock budget exhausted, no progress). */
  | 'timeout'
  /** Git commit of gate-passed work failed. */
  | 'commit'
  /** Worktree creation failed (repo/branch-level problem). */
  | 'worktree'
  /** Transient provider failure that would normally be retried, not terminal. */
  | 'transient-provider'
  /** Dispatch preflight rejected the run config (e.g. model id invalid for the adapter). */
  | 'config'
  /** Failure we could not classify into a known class. */
  | 'unknown';

// ---------- Workers ----------

export type WorkerName = 'claude-code' | 'opencode' | 'omp' | 'pi';

/**
 * Q34 observability context for watchdog-health ledger rows. Dispatchers
 * (executor, deps) populate it for implement attempts; when present and a
 * no-progress clock is armed (noProgressTimeoutMs > 0), each worker-CLI
 * launch appends one event row to the orchestration ledger so never-firing
 * watchdogs are visible in analytics instead of inferred from long
 * wall-clock timeouts.
 */
export interface WatchdogLedgerContext {
  /** Repo root the ledger lives under — never the (ephemeral) worktree cwd. */
  repoPath: string;
  taskId: string;
  attempt: number;
  worker: string;
  /**
   * FR-VIS: spawn runtime that owned the launch — "herdr-pane" when routed
   * through herdr, "direct" for a plain child process. Populated by the
   * spawn sites; undefined on legacy callers.
   */
  runtime?: 'herdr-pane' | 'direct';
  /** FR-VIS: true when the worker ran in an operator-visible herdr pane. */
  visible?: boolean;
}

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
  /** Q34: structured watchdog-health ledger context; see WatchdogLedgerContext. */
  watchdogLedger?: WatchdogLedgerContext;
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
  /**
   * Last error text from the worker (stitched from stderr/parsed result).
   * Surfaced so the executor can classify transient vs non-retryable
   * failures even when resultText is null (e.g. proxy returned
   * [claude-code:unrecognized_model] with no .result element).
   */
  errorText?: string;
  /**
   * True when the worker exited cleanly (exit 0) but produced zero events and
   * no result text — the hung-worker/empty-output signature (loop 59 run-11:
   * 30-minute burn on silent opencode runs). Distinct from both success and a
   * normal failure so callers can fall back (branch + gh pr create) without
   * burning the full retry budget on a dead endpoint.
   */
  noProgress?: boolean;
}

/** Uniform contract over heterogeneous headless coding-agent CLIs. */
export interface WorkerAdapter {
  readonly name: WorkerName;
  /** Optional per-adapter progress classifier (PRD Q33). When present, a
   * JSON delta of the worker's event stream that satisfies it counts as
   * progress for the no-progress watchdog; otherwise it is pure thinking
   * and must not reset the clock. When absent, the runtime falls back to
   * the shared 'meaningfulBytes' heuristic helpers (herdr/spawn-utils). An
   * adapter that redeclares nothing still benefits from the watchdog — this
   * hook only moves the decision closer to the adapter's stream shape.
   */
  isProgress?(line: string): boolean;
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
  | 'audit'
  | 'consume'
  | 'self-update'
  | 'scout'
  | 'queue'
  | 'create'
  | 'governor';

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
