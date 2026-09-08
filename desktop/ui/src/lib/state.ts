/**
 * View-model helpers for the desktop control app (issue #181). Pure and
 * DOM-free: tested in test/desktop-pipeline.test.ts alongside the pipeline
 * and notifications modules.
 */
import type { AggregateState, DaemonStatus, QueuedTaskRow, SessionPane } from "./protocol.js";

/**
 * Aggregate tray/header state (FR-UI-01). Mirrors src/tui/tui.ts
 * aggregateStatus so the tray and the TUI never disagree: live work wins
 * over everything; FAILED means the circuit is open (the factory cannot
 * dispatch), not a lifetime failure count — runs.failed_recent never decays
 * and pinned the TUI header FAILED permanently (2026-09-05 incident).
 */
export function aggregateStatus(status: DaemonStatus | null, panes: SessionPane[]): AggregateState {
  if (!status) return "idle";
  const runningPanes = panes.filter((p) => p.state === "running").length;
  if ((status.runs?.active ?? 0) > 0 || runningPanes > 0 || (status.queue?.claimed ?? 0) > 0) {
    return "running";
  }
  if (status.circuit === "open") return "failed";
  return "idle";
}

/** One roster row: a live worker pane or a queued task not yet pane-backed. */
export interface RosterRow {
  taskId: string;
  kind: "pane" | "queued";
  /** Live panes carry the herdr state; queued tasks their queue status. */
  state: string;
  role?: string;
  worker?: string;
  paneId?: string;
  cwd?: string;
  title?: string;
  createdAt?: string;
}

/**
 * Merge /agents panes + queued rows into one roster (FR-UI-03). A queued
 * task that already has a pane is represented by the pane row only, so a
 * mid-run task never appears twice.
 */
export function rosterRows(payload: { panes?: SessionPane[]; queued?: QueuedTaskRow[] } | null): RosterRow[] {
  if (!payload) return [];
  const paneTaskIds = new Set((payload.panes ?? []).map((p) => p.taskId));
  const rows: RosterRow[] = (payload.panes ?? []).map((p) => ({
    taskId: p.taskId,
    kind: "pane" as const,
    state: p.state ?? "unknown",
    role: p.role,
    worker: p.worker,
    paneId: p.paneId,
    cwd: p.cwd,
  }));
  for (const t of payload.queued ?? []) {
    if (!t.id || paneTaskIds.has(t.id)) continue;
    rows.push({
      taskId: t.id,
      kind: "queued" as const,
      state: t.status ?? "pending",
      title: t.title,
      createdAt: t.createdAt,
    });
  }
  return rows;
}
