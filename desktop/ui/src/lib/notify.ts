/**
 * Native-notification decisions (FR-UI-04, issue #181): fold the event
 * stream + approval inbox into a minimal set of notifications. Pure and
 * DOM-free — the Rust shell (tauri-plugin-notification) does the actual
 * OS push; tested in test/desktop-pipeline.test.ts.
 */
import type { ApprovalQuestion, EventLine } from "./protocol.js";

export type NotificationKind = "approval-needed" | "failure" | "completion";

export interface DesiredNotification {
  kind: NotificationKind;
  /** taskId (or runId fallback) the notification deep-links into. */
  scope: string;
  title: string;
  body: string;
}

/**
 * Decisions for one poll of (events since last poll, current approvals).
 * Dedup is the caller's job via `seen` — this function is stateless on
 * purpose so a missed event on reconnect re-fires rather than re-renders.
 */
export function desiredNotifications(
  events: EventLine[],
  approvals: ApprovalQuestion[],
  seen: Record<string, true>,
): DesiredNotification[] {
  const out: DesiredNotification[] = [];
  const push = (n: DesiredNotification): void => {
    if (seen[n.scope]) return;
    seen[n.scope] = true;
    out.push(n);
  };
  for (const line of events) {
    const scope = line.taskId ?? line.runId ?? "";
    if (!scope) continue;
    const stage = line.stage ?? line.event ?? "";
    if (stage === "failed" || /\b(failed|rejected)\b/.test(line.message)) {
      push({ kind: "failure", scope, title: `Run failed: ${scope}`, body: line.message });
      continue;
    }
    if (stage === "publish" || /https:\/\/github\.com\/\S+\/pull\/\d+/.test(line.message)) {
      push({ kind: "completion", scope, title: `PR ready: ${scope}`, body: line.message });
    }
  }
  for (const a of approvals) {
    if (seen[a.taskId]) continue;
    seen[a.taskId] = true;
    out.push({
      kind: "approval-needed",
      scope: a.taskId,
      title: `Approval needed: ${a.title || a.taskId}`,
      body: a.question,
    });
  }
  return out;
}
