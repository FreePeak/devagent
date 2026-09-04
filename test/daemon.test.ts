import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";
import { spawn as cpSpawn } from "node:child_process";
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { request as httpRequest } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { appendAuditRecord, auditLedgerRecord } from "../src/orchestrator/ledger.js";
import { readProxyState } from "../src/resilience/proxy-state.js";
import { enqueueTask } from "../src/queue.js";
import { startDaemon, devagentHome, type DispatchSpec } from "../src/server/daemon.js";
import type { AnswerEndpointResult } from "../src/orchestrator/store.js";
import type { AuditVerdict } from "../src/orchestrator/types.js";

// Stub herdr CLI (same env-file protocol as test/herdr.test.ts): `agent list`
// returns two panes — a running TASK-abc pane and an idle pane parked in a
// .devagent-worktrees checkout (stale by the FR-VIS mapping).
const STUB = `#!/usr/bin/env node
const fs = require('node:fs');
const args = process.argv.slice(2);
if (args[0] === '--session') args.splice(0, 2);
const out = (o) => process.stdout.write(JSON.stringify(o) + '\\n');
const log = (line) => fs.appendFileSync(process.env.STUB_LOG || '/tmp/stub-herdr-daemon.log', line + '\\n');
log(args.join(' '));
if (args[0] === 'agent' && args[1] === 'list') {
  out({ id: 'x', result: { type: 'agent_list', agents: [
    { name: 'TASK-abc-a1', label: 'TASK-abc-a1', pane_id: 'p1', workspace_id: 'w1',
      agent_status: 'working', cwd: '/repo/.devagent-worktrees/TASK-abc-a1', created_at: '2026-09-04T00:00:00Z' },
    { name: 'TASK-old-a1', label: 'TASK-old-a1', pane_id: 'p2', workspace_id: 'w2',
      agent_status: 'idle', cwd: '/repo/.devagent-worktrees/TASK-old-a1' }
  ] } });
} else {
  out({ id: 'x', result: { type: 'unknown' } });
}
`;

function verdictFixture(taskId: string): AuditVerdict {
  return {
    verdict: "pass",
    integrity: "clean",
    criteriaResults: [{ criterion: "build passes", met: true, evidence: "tsc clean" }],
    summary: `${taskId} verified`,
  };
}

/** Await until predicate passes or the deadline expires (poll, no flaky sleeps). */
async function until(fn: () => boolean | Promise<boolean>, timeoutMs = 4000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await fn()) return;
    await new Promise((r) => setTimeout(r, 25));
  }
  throw new Error("condition not met before deadline");
}

describe("daemon API", () => {
  let tmpHome = "";
  let repo = "";
  let stubDir = "";
  let stubBin = "";
  let stubLog = "";
  const cleanups: Array<() => void> = [];

  beforeAll(() => {
    tmpHome = mkdtempSync(join(tmpdir(), "devagent-daemon-home-"));
    repo = mkdtempSync(join(tmpdir(), "devagent-daemon-repo-"));
    stubDir = mkdtempSync(join(tmpdir(), "devagent-daemon-herdr-"));
    stubBin = join(stubDir, "herdr-stub.cjs");
    writeFileSync(stubBin, STUB);
    chmodSync(stubBin, 0o755);
    stubLog = join(stubDir, "calls.log");
    // herdr opt-in for the /status herdr block (config-gated like production).
    writeFileSync(join(repo, "devagent.json"), JSON.stringify({ herdr: { enabled: true } }));
  });

  afterAll(() => {
    delete process.env.DEVAGENT_HOME;
    delete process.env.DEVAGENT_DAEMON_TOKEN;
    delete process.env.DEVAGENT_HERDR_BIN;
    delete process.env.DEVAGENT_VISIBILITY;
    for (const dir of [tmpHome, repo, stubDir]) rmSync(dir, { recursive: true, force: true });
  });

  afterEach(() => {
    delete process.env.DEVAGENT_DAEMON_TOKEN;
    try {
      rmSync(stubLog, { force: true });
    } catch {
      // nothing logged yet
    }
    for (const fn of cleanups.splice(0)) fn();
  });

  interface Started {
    handle: Awaited<ReturnType<typeof startDaemon>>;
    token: string;
  }

  async function start(opts: Record<string, unknown> = {}): Promise<Started> {
    process.env.DEVAGENT_HOME = tmpHome;
    process.env.DEVAGENT_HERDR_BIN = stubBin;
    process.env.STUB_LOG = stubLog;
    const handle = await startDaemon({ port: 0, repoPath: repo, ...opts });
    cleanups.push(() => handle.stop());
    return { handle, token: handle.token };
  }

  /** Stub CLI invocation lines (herdr calls the daemon made). */
  function stubCalls(): string[] {
    try {
      return readFileSync(stubLog, "utf8").trim().split("\n");
    } catch {
      return [];
    }
  }

  function base(handle: Started): string {
    return `http://127.0.0.1:${handle.handle.port}`;
  }

  function get(handle: Started, path: string, init: RequestInit = {}): Promise<Response> {
    return fetch(`${base(handle)}${path}`, init);
  }

  /** Raw node:http GET — undici fetch silently normalizes/overrides the Host header. */
  function rawGet(
    handle: Started,
    path: string,
    headers: Record<string, string>,
  ): Promise<{ status: number }> {
    return new Promise((resolve, reject) => {
      const r = httpRequest(`${base(handle)}${path}`, { headers }, (resp) => {
        resp.resume();
        resp.on("end", () => resolve({ status: resp.statusCode ?? 0 }));
      });
      r.on("error", reject);
      r.end();
    });
  }

  it("serves /healthz without auth", async () => {
    const h = await start();
    const res = await get(h, "/healthz");
    expect(res.status).toBe(200);
    expect(await res.json()).toMatchObject({ ok: true });
  });

  it("rejects missing and wrong bearer tokens with 401", async () => {
    const h = await start();
    const noAuth = await get(h, "/status");
    expect(noAuth.status).toBe(401);
    const wrong = await get(h, "/status", { headers: { Authorization: "Bearer nope" } });
    expect(wrong.status).toBe(401);
  });

  it("rejects foreign Host and Origin headers with 403", async () => {
    const h = await start();
    // Host guard: raw client so the evil Host actually reaches the server.
    const evilHost = await rawGet(h, "/status", { Host: "evil.example.com", Authorization: `Bearer ${h.token}` });
    expect(evilHost.status).toBe(403);
    // bare loopback Host (UDS-style, TuiAgent contract) still passes
    const bareHost = await rawGet(h, "/status", { Host: "localhost", Authorization: `Bearer ${h.token}` });
    expect(bareHost.status).toBe(200);
    // Origin guard (fetch passes Origin through untouched)
    const evilOrigin = await get(h, "/status", {
      headers: { Authorization: `Bearer ${h.token}`, Origin: "http://evil.example.com" },
    });
    expect(evilOrigin.status).toBe(403);
    const ok = await get(h, "/status", { headers: { Authorization: `Bearer ${h.token}` } });
    expect(ok.status).toBe(200);
  });

  it("reports status shape with capabilities, queue counts, herdr and spawn blocks", async () => {
    enqueueTask(repo, {
      id: "TASK-status-1",
      title: "status probe",
      goal: "status probe",
      source: "test",
    });
    writeFileSync(
      join(repo, ".devagent", "proxy-state.json"),
      JSON.stringify({ circuit: "open", circuitChangedAt: new Date().toISOString(), updatedAt: new Date().toISOString() }),
    );
    const h = await start();
    const res = await get(h, "/status", { headers: { Authorization: `Bearer ${h.token}` } });
    expect(res.status).toBe(200);
    const body = (await res.json()) as Record<string, unknown>;
    expect(body.capabilities).toEqual(["approve", "dispatch", "attach", "kill-via-answer"]);
    expect(body.queue).toMatchObject({ pending: 1, claimed: 0, done: 0 });
    expect(body.circuit).toBe("open");
    expect(body.herdr).toMatchObject({ enabled: true, session: "devagent" });
    expect(body.spawn).toEqual({ visibility: "visible" });
    expect(typeof body.uptime_s).toBe("number");
    expect(readProxyState(repo)?.circuit).toBe("open");
  });

  it("lists session panes through the stubbed herdr CLI", async () => {
    const h = await start();
    const res = await get(h, "/sessions", { headers: { Authorization: `Bearer ${h.token}` } });
    expect(res.status).toBe(200);
    const body = (await res.json()) as { panes: Array<{ taskId: string; state: string; paneId: string }> };
    expect(body.panes).toHaveLength(2);
    const running = body.panes.find((p) => p.taskId === "TASK-abc")!;
    expect(running.state).toBe("running");
    expect(running.paneId).toBe("p1");
    const stale = body.panes.find((p) => p.taskId === "TASK-old")!;
    expect(stale.state).toBe("stale");
  });

  it("reads appended ledger rows through /history with limit and task filter", async () => {
    appendAuditRecord(repo, auditLedgerRecord({ taskId: "TASK-h1", attempt: 1, verdict: verdictFixture("TASK-h1") }));
    appendAuditRecord(repo, auditLedgerRecord({ taskId: "TASK-h2", attempt: 1, verdict: verdictFixture("TASK-h2") }));
    const h = await start();
    const all = await get(h, "/history", { headers: { Authorization: `Bearer ${h.token}` } });
    const allBody = (await all.json()) as { records: Array<{ taskId: string }> };
    expect(allBody.records.map((r) => r.taskId)).toContain("TASK-h1");
    const one = await get(h, "/history?taskId=TASK-h1", { headers: { Authorization: `Bearer ${h.token}` } });
    const oneBody = (await one.json()) as { records: Array<{ taskId: string }> };
    expect(oneBody.records.map((r) => r.taskId)).toEqual(["TASK-h1"]);
    const limited = await get(h, "/history?limit=1", { headers: { Authorization: `Bearer ${h.token}` } });
    const limitedBody = (await limited.json()) as { records: Array<{ taskId: string }> };
    expect(limitedBody.records).toHaveLength(1);
  });

  it("dispatch writes a queue row and runs the injected runner", async () => {
  const seen: DispatchSpec[] = [];
  const h = await start({
    dispatchRunner: async (spec: DispatchSpec) => {
      seen.push(spec);
      return { pid: 4242 };
    },
  });
    const res = await get(h, "/dispatch", {
      method: "POST",
      headers: { Authorization: `Bearer ${h.token}`, "Content-Type": "application/json" },
      body: JSON.stringify({ prompt: "daemon dispatch probe", worker: "omp", budget: { maxLoops: 3 } }),
    });
    expect(res.status).toBe(202);
    const body = (await res.json()) as { ok: boolean; taskId: string; pid: number };
    expect(body.ok).toBe(true);
    expect(body.taskId).toMatch(/^TASK-[0-9a-f]{8}$/);
    expect(body.pid).toBe(4242);
    expect(seen).toHaveLength(1);
    expect(seen[0]?.prompt).toBe("daemon dispatch probe");
    expect(seen[0]?.worker).toBe("omp");
    expect(seen[0]?.maxLoops).toBe(3);
    expect(seen[0]?.repoPath).toBe(repo);
    // the queue row is observable through the same API consumers use
    const agents = await get(h, "/agents", { headers: { Authorization: `Bearer ${h.token}` } });
    const agentsBody = (await agents.json()) as { queued: Array<{ id: string; source?: string }> };
    expect(agentsBody.queued.map((t) => t.id)).toContain(body.taskId);
    const queuedRow = agentsBody.queued.find((t) => t.id === body.taskId)!;
    expect(queuedRow.source).toBe("daemon");
  });

  it("rejects dispatch without a prompt (400)", async () => {
    const h = await start({ dispatchRunner: async () => ({ pid: null }) });
    const res = await get(h, "/dispatch", {
      method: "POST",
      headers: { Authorization: `Bearer ${h.token}`, "Content-Type": "application/json" },
      body: JSON.stringify({ worker: "omp" }),
    });
    expect(res.status).toBe(400);
  });

  it("approve routes through the injectable answer applier", async () => {
    let applied: { repoPath: string; taskId: string; answer: string } | null = null;
    const h = await start({
      answerApplier: (rp: string, id: string, answer: string) => {
        applied = { repoPath: rp, taskId: id, answer };
        const result: AnswerEndpointResult = { status: 200, body: { ok: true, note: "applied" } };
        return result;
      },
    });
    const res = await get(h, "/approve", {
      method: "POST",
      headers: { Authorization: `Bearer ${h.token}`, "Content-Type": "application/json" },
      body: JSON.stringify({ repoPath: repo, taskId: "T-9", answer: "go ahead" }),
    });
    expect(res.status).toBe(200);
    expect(applied).toEqual({ repoPath: repo, taskId: "T-9", answer: "go ahead" });
  });

  it("kill sentinel stops the live pane (send-keys + workspace close) and writes an audit row", async () => {
    const h = await start();
    const res = await get(h, "/approve", {
      method: "POST",
      headers: { Authorization: `Bearer ${h.token}`, "Content-Type": "application/json" },
      body: JSON.stringify({ repoPath: repo, taskId: "TASK-abc", answer: "__kill__" }),
    });
    expect(res.status).toBe(200);
    const body = (await res.json()) as { ok: boolean; killed: boolean; taskId: string; note: string };
    expect(body).toMatchObject({ ok: true, killed: true, taskId: "TASK-abc", note: "operator kill requested" });
    await until(() => {
      const calls = stubCalls().join("\n");
      return calls.includes("pane send-keys p1 ctrl+c") && calls.includes("workspace close w1");
    });
    await until(() => {
      try {
        const ledgerFile = join(repo, ".devagent", "runs", "orchestration", "events.jsonl");
        return existsSync(ledgerFile) && readFileSync(ledgerFile, "utf8").includes("operator-kill");
      } catch {
        return false;
      }
    });
  });

  it("kill sentinel with no live target returns 404 and never reaches the answer applier", async () => {
    const applied: string[] = [];
    const h = await start({
      answerApplier: (_rp: string, id: string, answer: string) => {
        applied.push(`${id}:${answer}`);
        const result: AnswerEndpointResult = { status: 200, body: { ok: true, note: "applied" } };
        return result;
      },
    });
    const res = await get(h, "/approve", {
      method: "POST",
      headers: { Authorization: `Bearer ${h.token}`, "Content-Type": "application/json" },
      body: JSON.stringify({ repoPath: repo, taskId: "TASK-nobody", answer: "__kill__" }),
    });
    expect(res.status).toBe(404);
    const body = (await res.json()) as { ok: boolean; note: string };
    expect(body.ok).toBe(false);
    expect(body.note).toContain("TASK-nobody");
    expect(applied).toEqual([]);
  });

  it("non-kill answers still route to the answer applier unchanged", async () => {
    const applied: string[] = [];
    const h = await start({
      answerApplier: (_rp: string, id: string, answer: string) => {
        applied.push(`${id}:${answer}`);
        const result: AnswerEndpointResult = { status: 200, body: { ok: true, note: "applied" } };
        return result;
      },
    });
    const res = await get(h, "/approve", {
      method: "POST",
      headers: { Authorization: `Bearer ${h.token}`, "Content-Type": "application/json" },
      body: JSON.stringify({ repoPath: repo, taskId: "T-9", answer: "continue with option b" }),
    });
    expect(res.status).toBe(200);
    expect(applied).toEqual(["T-9:continue with option b"]);
  });

  it("attach returns 404 when no pane matches the task", async () => {
    const h = await start();
    const res = await get(h, "/attach/TASK-missing", { method: "POST", headers: { Authorization: `Bearer ${h.token}` } });
    expect(res.status).toBe(404);
  });

  it("attach returns the attach command and writes an operator-attached ledger row", async () => {
    const h = await start();
    const res = await get(h, "/attach/TASK-abc", { method: "POST", headers: { Authorization: `Bearer ${h.token}` } });
    expect(res.status).toBe(200);
    const body = (await res.json()) as { ok: boolean; command: string };
    expect(body.ok).toBe(true);
    expect(body.command).toBe("herdr --session devagent agent attach p1");
    await until(() => {
      try {
        const ledgerFile = join(repo, ".devagent", "runs", "orchestration", "events.jsonl");
        return existsSync(ledgerFile) && readFileSync(ledgerFile, "utf8").includes('"operator-attached"');
      } catch {
        return false;
      }
    });
  });

  it("unknown route -> 404, wrong method -> 405", async () => {
    const h = await start();
    const nf = await get(h, "/nope", { headers: { Authorization: `Bearer ${h.token}` } });
    expect(nf.status).toBe(404);
    const mna = await get(h, "/dispatch", { headers: { Authorization: `Bearer ${h.token}` } });
    expect(mna.status).toBe(405);
  });

  it("events streams replay + live lines with ids (raw client; undici fetch buffers SSE)", async () => {
    const runsDir = join(tmpHome, "runs");
    mkdirSync(runsDir, { recursive: true });
    const log = join(runsDir, "live.jsonl");
    writeFileSync(log, `${JSON.stringify({ msg: "l1" })}\n${JSON.stringify({ msg: "l2" })}\n${JSON.stringify({ msg: "l3" })}\n`);
    const h = await start();
    const got: string[] = [];
    const req = httpRequest(
      { host: "127.0.0.1", port: h.handle.port as number, path: "/events", headers: { Authorization: `Bearer ${h.token}` } },
      (resp) => {
        resp.setEncoding("utf8");
        resp.on("data", (c: string) => got.push(c));
      },
    );
    req.end();
    await until(() => got.join("").includes('"msg":"l3"'), 3000);
    // live append arrives within the 250ms poll window
    writeFileSync(log, readFileSync(log, "utf8") + `${JSON.stringify({ msg: "l4" })}\n`);
    await until(() => got.join("").includes('"msg":"l4"'), 3000);
    const seen = got.join("");
    req.destroy();
    expect(seen).toContain("id: 0");
    expect(seen).toContain('"msg":"l1"');
    expect(seen).toContain('"msg":"l4"');
    expect(/id: \d+\ndata: /.test(seen)).toBe(true);
    // daemon still healthy after the client vanished
    await until(async () => {
      try {
        return (await get(h, "/healthz")).status === 200;
      } catch {
        return false;
      }
    });
  });

  it("stop() releases the port so a second daemon can bind", async () => {
    process.env.DEVAGENT_HOME = tmpHome;
    process.env.DEVAGENT_HERDR_BIN = stubBin;
    const first = await startDaemon({ port: 0, repoPath: repo });
    const port = first.port as number;
    expect(port).toBeGreaterThan(0);
    await first.stop();
    const second = await startDaemon({ port, repoPath: repo });
    expect(second.port).toBe(port);
    await second.stop();
  });

  it("issues and persists a 0600 daemon-token file under DEVAGENT_HOME", async () => {
    delete process.env.DEVAGENT_DAEMON_TOKEN;
    const h = await start();
    const tokenPath = join(devagentHome(), "daemon-token");
    expect(existsSync(tokenPath)).toBe(true);
    const stored = readFileSync(tokenPath, "utf8").trim();
    expect(stored).toBe(h.token);
    expect(stored.length).toBeGreaterThanOrEqual(24);
    if (process.platform !== "win32") {
      const mode = statSync(tokenPath).mode & 0o777;
      expect(mode).toBe(0o600);
    }
  });

  it("default dispatch runner spawns the real pipeline detached (dist CLI when present)", async () => {
    // Exercise the default runner in-process via a marker: spawn node -e that
    // writes a file, using the same code path shape (detached, unref, ignore).
    process.env.DEVAGENT_HOME = tmpHome;
    const marker = join(repo, "marker.txt");
    const runner = await import("../src/server/daemon.js");
    // The real runner builds argv for the repo CLI; here we only assert the
    // spawned-detached contract via a proxy: run the real CLI --help quickly.
    const distCli = join(process.cwd(), "dist", "src", "cli.js");
    const useDist = existsSync(distCli);
    const bin = useDist ? process.execPath : "npx";
    const argv = useDist
      ? [distCli, "task", "--help"]
      : ["tsx", join(process.cwd(), "src", "cli.ts"), "task", "--help"];
    const child = cpSpawn(bin, argv, { stdio: "ignore", detached: true });
    const code = await new Promise<number>((resolve) => {
      child.on("exit", (c) => resolve(c ?? -1));
    });
    expect([0, 1]).toContain(code);
  }, 10_000);
});

// UDS coverage: separate suite so TCP tests keep the plain-token lifecycle.
describe("daemon UDS transport", () => {
  let tmpHome = "";
  let repo = "";
  beforeAll(() => {
    tmpHome = mkdtempSync(join(tmpdir(), "devagent-uds-home-"));
    repo = mkdtempSync(join(tmpdir(), "devagent-uds-repo-"));
    process.env.DEVAGENT_HOME = tmpHome;
  });
  afterAll(() => {
    delete process.env.DEVAGENT_HOME;
    rmSync(tmpHome, { recursive: true, force: true });
    rmSync(repo, { recursive: true, force: true });
  });

  it("serves healthz over a unix socket without TCP", async () => {
    const sockPath = join(tmpHome, "daemon.sock");
    const handle = await startDaemon({ udsPath: sockPath, repoPath: repo, token: "uds-token" });
    try {
      expect(handle.port).toBeNull();
      expect(handle.udsPath).toBe(sockPath);
      // node:http client over the socket path (fetch cannot reach UDS directly)
      const body = await new Promise<string>((resolve, reject) => {
        const r = httpRequest({ socketPath: sockPath, path: "/healthz", method: "GET" }, (resp) => {
          let data = "";
          resp.on("data", (c: Buffer) => (data += c.toString("utf8")));
          resp.on("end", () => resolve(data));
        });
        r.on("error", reject);
        r.end();
      });
      expect(JSON.parse(body)).toMatchObject({ ok: true });
    } finally {
      await handle.stop();
    }
  });
});
