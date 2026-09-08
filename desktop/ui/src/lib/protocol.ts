/**
 * Wire types of the FR-CTRL daemon API (src/server/daemon.ts) that the
 * desktop control app consumes (FR-UI-07: thin client — no business logic,
 * no second transport; every field tolerates absence).
 */

/** GET /status — aggregate loop state (FR-CTRL-01). */
export interface DaemonStatus {
  now?: string;
  uptime_s?: number;
  runs?: { active?: number; failed_recent?: number };
  queue?: { pending?: number; claimed?: number; done?: number };
  circuit?: string;
  herdr?: { enabled?: boolean; session?: string };
  spawn?: { visibility?: string };
  capabilities?: string[];
}

/** One operator-observable worker pane (mirrors herdr's SessionPaneInfo). */
export interface SessionPane {
  taskId: string;
  role?: string;
  worker?: string;
  paneId: string;
  state?: string;
  cwd?: string;
  startedAt?: string;
}

/** One queue row as listed by GET /agents. */
export interface QueuedTaskRow {
  id?: string;
  title?: string;
  status?: string;
  createdAt?: string;
}

/** GET /agents — roster: live panes + queued tasks. */
export interface RosterPayload {
  panes?: SessionPane[];
  queued?: QueuedTaskRow[];
}

/** One task the auditor paused for human input (GET /approvals). */
export interface ApprovalQuestion {
  taskId: string;
  title: string;
  question: string;
  attempts?: number;
  updatedAt?: string | null;
}

/** POST /dispatch body — mirrors the daemon's DispatchSpec (FR-CTRL-03). */
export interface DispatchRequest {
  prompt: string;
  repoPath?: string;
  role?: string;
  worker?: string;
  budget?: { maxLoops?: number; timeoutMinutes?: number };
}

export interface DispatchResult {
  ok: boolean;
  taskId?: string;
  pid?: number | null;
  note?: string;
}

export interface AttachResult {
  ok: boolean;
  command?: string;
  note?: string;
}

/** GET /history rows: sparse ledger records (audit verdicts + lifecycle events). */
export type HistoryRow = Record<string, unknown>;

/** Aggregate tray/header state (FR-UI-01), mirroring the TUI's aggregateStatus. */
export type AggregateState = "running" | "idle" | "failed";

/**
 * One parsed SSE `data:` payload from /events — a DEVAGENT_HOME/runs/*.jsonl
 * line ({ts,level,stage,message,runId}) or a repo orchestration row
 * ({kind:'event',event,phase,detail,taskId}). Malformed lines are surfaced as
 * raw entries by the parser, never thrown.
 */
export interface EventLine {
  ts?: string;
  level?: string;
  stage?: string;
  event?: string;
  phase?: string;
  detail?: string;
  taskId?: string;
  runId?: string;
  message: string;
  /** True when the payload was not JSON and is shown verbatim. */
  raw?: boolean;
}
