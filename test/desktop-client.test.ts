/**
 * Desktop FR-CTRL client tests (issue #181): unit coverage with a stub
 * transport plus an integration pass against the REAL in-process daemon
 * (src/server/daemon.ts) — the same pattern test/daemon.test.ts uses, so a
 * daemon contract change that breaks the desktop app fails here first.
 */
import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";
import { mkdtempSync, mkdirSync, appendFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { saveBoard } from "../src/orchestrator/store.js";
import type { ProjectBoard } from "../src/orchestrator/types.js";
import { startDaemon, type DaemonHandle } from "../src/server/daemon.js";
import { DaemonClient, parseEventLine, subscribeEvents, type FetchLike } from "../desktop/ui/src/lib/client.js";

/** Await until predicate passes or the deadline expires (poll, no fixed sleeps). */
async function until(fn: () => boolean, timeoutMs = 4000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (fn()) return;
    await new Promise((r) => setTimeout(r, 25));
  }
  throw new Error("condition not met before deadline");
}

// Stub herdr CLI: `agent list` returns an empty roster so the integration
// tests never observe the operator's real herdr session on this machine.
const STUB = `#!/usr/bin/env node
process.stdout.write(JSON.stringify({ id: 'x', result: { type: 'agent_list', agents: [] } }) + '\\n');
`;

describe("DaemonClient (stub transport)", () => {
  it("sends the bearer token and JSON body on POST /dispatch", async () => {
    const seen: Array<{ url: string; init: Parameters<FetchLike>[1] }> = [];
    const fetchImpl: FetchLike = async (url, init) => {
      seen.push({ url, init });
      return { status: 202, json: async () => ({ ok: true, taskId: "TASK-x1", pid: 7 }) };
    };
    const client = new DaemonClient({ baseUrl: "http://127.0.0.1:7788", token: "tok", fetchImpl });
    const r = await client.dispatch({ prompt: "hello", worker: "omp", role: "worker", budget: { maxLoops: 2 } });
    expect(r.status).toBe(202);
    expect(r.value).toEqual({ ok: true, taskId: "TASK-x1", pid: 7 });
    const call = seen[0]!;
    expect(call.url).toBe("http://127.0.0.1:7788/dispatch");
    expect(call.init.headers?.Authorization).toBe("Bearer tok");
    expect(JSON.parse(call.init.body ?? "{}")).toEqual({
      prompt: "hello",
      role: "worker",
      worker: "omp",
      budget: { maxLoops: 2 },
    });
  });

  it("reports unreachable as status 0 instead of throwing", async () => {
    const fetchImpl: FetchLike = async () => {
      throw new Error("ECONNREFUSED");
    };
    const client = new DaemonClient({ baseUrl: "http://127.0.0.1:1", token: "t", fetchImpl });
    const r = await client.status();
    expect(r.status).toBe(0);
    expect(r.value).toBeNull();
    expect(r.note).toContain("ECONNREFUSED");
  });

  it("surfaces error notes from non-2xx bodies", async () => {
    const fetchImpl: FetchLike = async () => ({
      status: 401,
      json: async () => ({ ok: false, note: "unauthorized" }),
    });
    const client = new DaemonClient({ baseUrl: "http://x", token: "t", fetchImpl });
    const r = await client.status();
    expect(r.status).toBe(401);
    expect(r.value).toBeNull();
    expect(r.note).toBe("unauthorized");
  });
});

describe("parseEventLine", () => {
  it("parses run-log rows and orchestration rows", () => {
    expect(parseEventLine(JSON.stringify({ ts: "t", level: "warn", stage: "plan", message: "Linear comment failed: HTTP 401", runId: "ffd7" }))).toEqual({
      ts: "t",
      level: "warn",
      stage: "plan",
      event: undefined,
      phase: undefined,
      detail: undefined,
      taskId: undefined,
      runId: "ffd7",
      message: "Linear comment failed: HTTP 401",
    });
    const orch = parseEventLine(JSON.stringify({ kind: "event", event: "loop-phase", loop: 82, phase: "research", detail: "timeout 900s" }));
    expect(orch.stage).toBe("loop-phase");
    expect(orch.message).toBe("phase: research — timeout 900s — (loop 82)");
  });

  it("degrades corrupt payloads to raw entries", () => {
    const l = parseEventLine("not json at all");
    expect(l.raw).toBe(true);
    expect(l.message).toBe("not json at all");
  });
});

describe("DaemonClient against the real daemon", () => {
  let tmpHome = "";
  let repo = "";
  let stubDir = "";
  let stubBin = "";
  let handle: DaemonHandle | null = null;
  let client: DaemonClient;

  beforeAll(async () => {
    tmpHome = mkdtempSync(join(tmpdir(), "devagent-dsk-home-"));
    repo = mkdtempSync(join(tmpdir(), "devagent-dsk-repo-"));
    stubDir = mkdtempSync(join(tmpdir(), "devagent-dsk-herdr-"));
    stubBin = join(stubDir, "herdr-stub.cjs");
    writeFileSync(stubBin, STUB);
    // Neutralize the operator's real herdr session: /agents must report an
    // empty roster, not the panes of whatever session is live on this Mac.
    process.env.DEVAGENT_HOME = tmpHome;
    process.env.DEVAGENT_HERDR_BIN = stubBin;
    handle = await startDaemon({ port: 0, repoPath: repo });
    client = new DaemonClient({ baseUrl: `http://127.0.0.1:${handle.port}`, token: handle.token });
  });

  afterAll(async () => {
    delete process.env.DEVAGENT_HOME;
    delete process.env.DEVAGENT_HERDR_BIN;
    await handle?.stop();
    rmSync(tmpHome, { recursive: true, force: true });
    rmSync(repo, { recursive: true, force: true });
    rmSync(stubDir, { recursive: true, force: true });
  });

  afterEach(() => {
    try {
      rmSync(join(repo, ".devagent-project.json"), { force: true });
    } catch {
      // no board written in this test
    }
  });

  it("reads status, roster and an empty approvals inbox", async () => {
    const st = await client.status();
    expect(st.status).toBe(200);
    expect(st.value?.capabilities).toContain("dispatch");
    const roster = await client.roster();
    expect(roster.status).toBe(200);
    expect(roster.value?.panes).toEqual([]);
    const approvals = await client.approvals();
    expect(approvals.value).toEqual({ pending: [] });
  });

  it("lists auditor-paused board tasks from /approvals and answers via /approve", async () => {
    const board: ProjectBoard = {
      goal: "desktop probe",
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
      tasks: [
        { id: "T-ask", title: "paused task", prompt: "p", dependsOn: [], status: "ask", attempts: 1, failureDetail: "needs human input: pick a provider" },
      ],
    };
    saveBoard(repo, board);
    const approvals = await client.approvals();
    expect(approvals.value?.pending).toHaveLength(1);
    expect(approvals.value?.pending[0]).toMatchObject({ taskId: "T-ask", question: "pick a provider" });

    const applied: Array<[string, string]> = [];
    const h2 = await startDaemon({
      port: 0,
      repoPath: repo,
      answerApplier: (rp, id, answer) => {
        applied.push([id, answer]);
        void rp;
        return { status: 200, body: { ok: true, note: "applied" } };
      },
    });
    const c2 = new DaemonClient({ baseUrl: `http://127.0.0.1:${h2.port}`, token: h2.token });
    const r = await c2.approve({ taskId: "T-ask", answer: "use omp" });
    expect(r.status).toBe(200);
    expect(applied).toEqual([["T-ask", "use omp"]]);
    await h2.stop();
  });

  it("dispatches through the injected runner and reads history", async () => {
    const h2 = await startDaemon({
      port: 0,
      repoPath: repo,
      dispatchRunner: async () => ({ pid: 4242 }),
    });
    const c2 = new DaemonClient({ baseUrl: `http://127.0.0.1:${h2.port}`, token: h2.token });
    const r = await c2.dispatch({ prompt: "desktop dispatch probe" });
    expect(r.status).toBe(202);
    expect(r.value?.ok).toBe(true);
    expect(r.value?.taskId).toMatch(/^TASK-/);
    const hist = await c2.history({ limit: 10 });
    expect(hist.status).toBe(200);
    expect(hist.value?.records).toEqual([]);
    await h2.stop();
  });

  it("attach returns 404 with a note when no pane matches", async () => {
    const r = await client.attach("TASK-none");
    expect(r.status).toBe(404);
    expect(r.value).toBeNull();
  });
});

describe("subscribeEvents against the real daemon SSE stream", () => {
  let tmpHome = "";
  let repo = "";
  let handle: DaemonHandle | null = null;
  let runLog = "";

  beforeAll(async () => {
    tmpHome = mkdtempSync(join(tmpdir(), "devagent-dsk-sse-"));
    repo = mkdtempSync(join(tmpdir(), "devagent-dsk-sse-repo-"));
    process.env.DEVAGENT_HOME = tmpHome;
    runLog = join(tmpHome, "runs", "sse-run.jsonl");
    // The follower snapshots DEVAGENT_HOME/runs at daemon start: the run log
    // must exist before startDaemon or it is never followed.
    mkdirSync(join(tmpHome, "runs"), { recursive: true });
    writeFileSync(runLog, `${JSON.stringify({ ts: "2026-09-08T00:00:01Z", level: "info", stage: "plan", message: "plan ready" })}\n`);
    handle = await startDaemon({ port: 0, repoPath: repo });
  });

  afterAll(async () => {
    delete process.env.DEVAGENT_HOME;
    await handle?.stop();
    rmSync(tmpHome, { recursive: true, force: true });
    rmSync(repo, { recursive: true, force: true });
  });

  it("streams run-log lines and resumes from Last-Event-ID without gaps", async () => {
    const msgs: string[] = [];
    let lastId = -1;
    const sub = subscribeEvents(
      { baseUrl: `http://127.0.0.1:${handle!.port}`, token: handle!.token },
      { onEvent: (id, line) => { lastId = id; msgs.push(line.message); } },
    );
    // Replay of the existing tail + a live line appended now.
    appendFileSync(runLog, `${JSON.stringify({ ts: "2026-09-08T00:00:02Z", level: "info", stage: "implement", message: "implementing" })}\n`);
    await until(() => msgs.includes("implementing"));
    sub.stop();
    expect(msgs).toContain("plan ready");
    expect(lastId).toBeGreaterThanOrEqual(1);

    // Resume from the last seen id: only newer events arrive, no replay.
    const resumed: string[] = [];
    const sub2 = subscribeEvents(
      { baseUrl: `http://127.0.0.1:${handle!.port}`, token: handle!.token },
      { onEvent: (_id, line) => resumed.push(line.message) },
      { lastEventId: lastId },
    );
    appendFileSync(runLog, `${JSON.stringify({ ts: "2026-09-08T00:00:03Z", level: "info", stage: "publish", message: "pr opened" })}\n`);
    await until(() => resumed.includes("pr opened"));
    sub2.stop();
    expect(resumed).toEqual(["pr opened"]);
  });

  it("rejects a wrong token by staying down (never live)", async () => {
    const states: string[] = [];
    let sawEvent = false;
    const sub = subscribeEvents(
      { baseUrl: `http://127.0.0.1:${handle!.port}`, token: "wrong-token" },
      { onEvent: () => { sawEvent = true; }, onState: (s) => states.push(s) },
    );
    await until(() => states.includes("down"));
    sub.stop();
    expect(sawEvent).toBe(false);
    expect(states).not.toContain("live");
  });
});
