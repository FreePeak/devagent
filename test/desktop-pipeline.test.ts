/**
 * Desktop view-model tests (issue #181): aggregateStatus + rosterRows,
 * the FR-UI-08 pipeline DAG fold, and FR-UI-04 notification decisions.
 * These pin the desktop app's only logic; everything else is transport.
 */
import { describe, expect, it } from "vitest";
import { aggregateStatus, rosterRows } from "../desktop/ui/src/lib/state.js";
import { buildPipelines, stageBadge } from "../desktop/ui/src/lib/pipeline.js";
import { desiredNotifications } from "../desktop/ui/src/lib/notify.js";
import type { EventLine } from "../desktop/ui/src/lib/protocol.js";

describe("aggregateStatus (FR-UI-01, mirrors the TUI contract)", () => {
  it("running wins: active run, live pane, or claimed queue row", () => {
    const s = { runs: { active: 0, failed_recent: 3 }, queue: { pending: 1, claimed: 0, done: 5 }, circuit: "closed" };
    expect(aggregateStatus(s, [])).toBe("idle");
    expect(aggregateStatus({ ...s, runs: { active: 1, failed_recent: 3 } }, [])).toBe("running");
    expect(aggregateStatus({ ...s, queue: { pending: 0, claimed: 1, done: 5 } }, [])).toBe("running");
    expect(aggregateStatus(s, [{ taskId: "T", paneId: "p", state: "running" }])).toBe("running");
  });

  it("failed means live trouble (circuit open), not lifetime failures", () => {
    // failed_recent never decays — it must NOT pin the tray at failed.
    expect(aggregateStatus({ runs: { active: 0, failed_recent: 99 }, queue: { pending: 0, claimed: 0, done: 0 }, circuit: "closed" }, [])).toBe("idle");
    expect(aggregateStatus({ runs: { active: 0, failed_recent: 0 }, queue: { pending: 0, claimed: 0, done: 0 }, circuit: "open" }, [])).toBe("failed");
  });

  it("degrades to idle without a status payload", () => {
    expect(aggregateStatus(null, [])).toBe("idle");
  });
});

describe("rosterRows (FR-UI-03)", () => {
  it("merges panes and queued rows without double-listing pane-backed tasks", () => {
    const rows = rosterRows({
      panes: [{ taskId: "TASK-a", paneId: "p1", state: "running", role: "worker", worker: "omp" }],
      queued: [
        { id: "TASK-a", title: "dup", status: "claimed", createdAt: "t" },
        { id: "TASK-b", title: "queued only", status: "pending", createdAt: "t2" },
      ],
    });
    expect(rows).toHaveLength(2);
    expect(rows[0]).toMatchObject({ taskId: "TASK-a", kind: "pane", state: "running" });
    expect(rows[1]).toMatchObject({ taskId: "TASK-b", kind: "queued", state: "pending", title: "queued only" });
  });

  it("returns empty for a null payload (daemon unreachable)", () => {
    expect(rosterRows(null)).toEqual([]);
  });
});

describe("buildPipelines (FR-UI-08)", () => {
  it("folds the full flow into stage statuses, elapsed and retries", () => {
    const lines: EventLine[] = [
      { ts: "2026-09-08T00:00:00Z", stage: "plan", message: "plan ready", runId: "r1" },
      { ts: "2026-09-08T00:00:01Z", stage: "implement", message: "worker omp started", runId: "r1" },
      { ts: "2026-09-08T00:00:02Z", stage: "validate", message: "G1 failed: 2 tests broke", runId: "r1" },
      { ts: "2026-09-08T00:00:03Z", stage: "validate", message: "G1 passed: 12 tests green", runId: "r1" },
      { ts: "2026-09-08T00:00:04Z", stage: "publish", message: "https://github.com/o/r/pull/9", runId: "r1" },
    ];
    const [m] = buildPipelines(lines);
    expect(m?.scope).toBe("r1");
    const st = Object.fromEntries((m?.stages ?? []).map((s) => [s.id, s]));
    expect(st.plan?.status).toBe("pass");
    expect(st.implement?.status).toBe("pass");
    expect(st.G1?.status).toBe("pass");
    expect(st.G1?.retries).toBe(1);
    expect(st.pr?.status).toBe("pass");
    expect(m?.prUrl).toBe("https://github.com/o/r/pull/9");
    // plan saw a single event (elapsed 0); G1 spans a 1s retry gap.
    expect(st.plan?.elapsedMs).toBe(0);
    expect(st.G1?.elapsedMs).toBeGreaterThanOrEqual(1000);
  });

  it("maps clarify to G0 and numbered gate announcements to their stage", () => {
    const [m] = buildPipelines([
      { stage: "clarify", message: "G0 readiness gate rejected ticket", taskId: "TASK-x" },
    ]);
    const st = Object.fromEntries((m?.stages ?? []).map((s) => [s.id, s]));
    expect(st.G0?.status).toBe("fail");
    expect(m?.failedReason).toBe("G0 readiness gate rejected ticket");
    expect(stageBadge(m)).toBe("G0");
  });

  it("marks a running stage and skips via wording", () => {
    const [m] = buildPipelines([
      { stage: "validate", message: "G2 skipped: no migrations", taskId: "TASK-s" },
      { stage: "validate", message: "G3 passed", taskId: "TASK-s" },
    ]);
    const st = Object.fromEntries((m?.stages ?? []).map((s) => [s.id, s]));
    expect(st.G2?.status).toBe("skip");
    expect(st.G3?.status).toBe("pass");
  });

  it("scopes events per taskId/runId separately", () => {
    const models = buildPipelines([
      { stage: "plan", message: "a", taskId: "TASK-1" },
      { stage: "plan", message: "b", runId: "r2" },
    ]);
    expect(models).toHaveLength(2);
    expect(models.map((m) => m.scope).sort()).toEqual(["TASK-1", "r2"]);
  });
});

describe("desiredNotifications (FR-UI-04)", () => {
  it("fires once per scope for failure, completion and approval", () => {
    const seen: Record<string, true> = {};
    const events: EventLine[] = [
      { stage: "failed", message: "test gate failed: boom", taskId: "TASK-1" },
      { stage: "publish", message: "https://github.com/o/r/pull/3", taskId: "TASK-2" },
    ];
    const approvals = [{ taskId: "TASK-3", title: "pick db", question: "which provider?" }];
    const notes = desiredNotifications(events, approvals, seen);
    expect(notes.map((n) => n.kind)).toEqual(["failure", "completion", "approval-needed"]);
    // Same inputs again: deduped to nothing.
    expect(desiredNotifications(events, approvals, seen)).toEqual([]);
  });

  it("unscoped events never notify", () => {
    expect(desiredNotifications([{ stage: "failed", message: "x" }], [], {})).toEqual([]);
  });
});
