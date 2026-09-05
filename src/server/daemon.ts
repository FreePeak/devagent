import { spawn as cpSpawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import {
  existsSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  statSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { join } from "node:path";
import { herdrEnabled, herdrSessionName, loadConfig, spawnVisibility } from "../config.js";
import {
  attachCommandFor,
  herdrCli,
  listSessionPanes,
  type SessionPaneInfo,
} from "../integrations/herdr.js";
import { appendAuditRecord, appendOperatorAttachRecord, auditLedgerRecord, readLedgerTail } from "../orchestrator/ledger.js";
import { applyAnswerToRepo, type AnswerEndpointResult } from "../orchestrator/store.js";
import { enqueueTask, listTasks, readTask, taskCount, type QueuedTask } from "../queue.js";
import { readProxyState } from "../resilience/proxy-state.js";
import { findStaleWorkerPids, killStaleProcessTree } from "../resilience/reaper.js";

/**
 * Control-plane daemon (FR-CTRL): a loopback-only JSON API over the queue,
 * ledger, and board state so the TUI dashboard (FR-TUI) and remote operators
 * can observe runs and dispatch work — always through the same `devagent
 * task` pipeline the CLI uses (FR-CTRL-03, never a pipeline bypass).
 */

export interface DaemonOptions {
  /** TCP port (0 = ephemeral); ignored when udsPath is set; default 7788. */
  port?: number;
  /** Repo the API reads from and dispatches into (default cwd). */
  repoPath?: string;
  /** Bearer token; default DEVAGENT_DAEMON_TOKEN else a fresh random one persisted 0600 to daemon-token. */
  token?: string;
  /** Unix-socket path; when set, listens there instead of TCP (filesystem perms are the auth). */
  udsPath?: string;
  /** Test seam: dispatch runner (defaults to a detached spawn of the real `task` CLI). */
  dispatchRunner?: (spec: DispatchSpec) => Promise<{ pid: number | null }>;
  /** Test seam: answer applier (defaults to applyAnswerToRepo). */
  answerApplier?: (repoPath: string, taskId: string, answer: string) => AnswerEndpointResult;
}

/** Arguments for one dispatched run (mirrors `devagent task` flags). */
export interface DispatchSpec {
  repoPath: string;
  prompt: string;
  role?: string;
  worker?: string;
  maxLoops?: number;
  timeoutMinutes?: number;
}

/** Handle returned by startDaemon; stop() tears down every listener and SSE client. */
export interface DaemonHandle {
  stop(): Promise<void>;
  /** TCP port when listening on TCP, else null. */
  port: number | null;
  /** UDS path when listening on a socket, else null. */
  udsPath: string | null;
  /** Effective bearer token (always present; persisted for local clients on either transport). */
  token: string;
}

const DEVAGENT_CAPABILITIES = ["approve", "dispatch", "attach", "kill-via-answer"];
/** Exact answer sentinel that routes /approve into the operator-kill path. */
const KILL_SENTINEL = "__kill__";
/** Minimum age of a headless worker child before an operator kill may reap it. */
const KILL_MIN_CHILD_AGE_MS = 60_000;
const MAX_BODY_BYTES = 1 << 20; // 1 MiB
const EVENTS_REPLAY_LINES = 200;
const EVENTS_POLL_MS = 250;
const EVENTS_HEARTBEAT_MS = 15_000;
const HISTORY_DEFAULT_LIMIT = 100;
const HISTORY_MAX_LIMIT = 1000;
const RUN_LOCK_TTL_MS = 60 * 60_000;

interface RouteContext {
  repoPath: string;
  token: string;
  startedAt: number;
  follower: RunLogFollower;
  dispatchRunner: (spec: DispatchSpec) => Promise<{ pid: number | null }>;
  answerApplier: (repoPath: string, taskId: string, answer: string) => AnswerEndpointResult;
}

function sendJson(res: ServerResponse, status: number, body: Record<string, unknown>): void {
  if (res.writableEnded) return;
  const payload = JSON.stringify(body);
  res.statusCode = status;
  res.setHeader("Content-Type", "application/json");
  res.setHeader("Content-Length", Buffer.byteLength(payload));
  res.end(payload);
}

function readBody(req: IncomingMessage): Promise<string> {
  let resolveBody!: (v: string) => void;
  let rejectBody!: (e: Error) => void;
  const promise = new Promise<string>((resolve, reject) => {
    resolveBody = resolve;
    rejectBody = reject;
  });
  const chunks: Buffer[] = [];
  let size = 0;
  req.on("data", (chunk: Buffer) => {
    size += chunk.length;
    if (size > MAX_BODY_BYTES) {
      rejectBody(new Error("body too large"));
      req.destroy();
      return;
    }
    chunks.push(chunk);
  });
  req.on("end", () => resolveBody(Buffer.concat(chunks).toString("utf8")));
  req.on("error", (err) => rejectBody(err));
  return promise;
}

/** Resolve DEVAGENT_HOME the same way the CLI does. */
export function devagentHome(): string {
  return process.env.DEVAGENT_HOME || join(process.env.HOME || ".", ".devagent");
}

/**
 * Resolve the bearer token: explicit option, env, else generate + persist
 * (0600) under DEVAGENT_HOME/daemon-token so local TUIs can pick it up.
 */
function resolveToken(opts: DaemonOptions): string {
  const existing = opts.token ?? process.env.DEVAGENT_DAEMON_TOKEN;
  if (existing) return existing;
  const token = randomBytes(24).toString("base64url");
  try {
    const home = devagentHome();
    mkdirSync(home, { recursive: true });
    writeFileSync(join(home, "daemon-token"), `${token}\n`, { mode: 0o600 });
  } catch {
    // persistence is best-effort; the returned token still authenticates this run
  }
  return token;
}

/** Host guard (DNS-rebinding): only loopback hosts, bare or with any :port suffix. */
function hostAllowed(host: string | undefined): boolean {
  if (!host) return false;
  const bare = host.startsWith("[") ? host.slice(0, host.indexOf("]") + 1) : host.split(":")[0];
  return bare === "127.0.0.1" || bare === "localhost" || bare === "[::1]";
}

/** Origin guard: when a browser sends Origin, only loopback origins pass. */
function originAllowed(origin: string | undefined): boolean {
  if (!origin) return true;
  try {
    const url = new URL(origin);
    return url.hostname === "127.0.0.1" || url.hostname === "localhost" || url.hostname === "[::1]";
  } catch {
    return false;
  }
}

function timingSafeEq(a: string, b: string): boolean {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return diff === 0;
}

/** Newest run log under <home>/runs (null when none exist yet). */
function newestRunLog(home: string): string | null {
  try {
    const dir = join(home, "runs");
    if (!existsSync(dir)) return null;
    const files = readdirSync(dir).filter((f) => f.endsWith(".jsonl"));
    if (files.length === 0) return null;
    let newest = "";
    let newestM = -1;
    for (const f of files) {
      const m = statSync(join(dir, f)).mtimeMs;
      if (m > newestM) {
        newestM = m;
        newest = f;
      }
    }
    return join(dir, newest);
  } catch {
    return null;
  }
}

interface EventSink {
  send: (event: { id: number; data: string }) => void;
}

/**
 * Run-log follower: replays the tail of the newest DEVAGENT_HOME/runs/*.jsonl
 * as SSE events (ids = line index within the file, Last-Event-ID resumable)
 * and streams new lines via a 250ms poll. Bounded to the last 200 lines.
 */
class RunLogFollower {
  private readonly listeners = new Set<EventSink>();
  /** (path, linesRead) pairs — the run-log plus the repo orchestration stream. */
  private readonly sources: Array<{ path: string; loaded: number }> = [];
  private buffer: string[] = [];
  private loaded = 0;
  private pollTimer: NodeJS.Timeout | null = null;

  constructor(home: string, extraSources: string[] = []) {
    const runLog = newestRunLog(home);
    if (runLog) this.sources.push({ path: runLog, loaded: 0 });
    for (const p of extraSources) {
      // Skip duplicates (a repo may live under DEVAGENT_HOME).
      if (p !== runLog) this.sources.push({ path: p, loaded: 0 });
    }
  }

  hasLog(): boolean {
    return this.sources.length > 0;
  }

  /** Read new lines from every source and fan them out (ids shift-safe). */
  private load(): void {
    try {
      for (const s of this.sources) {
        if (!existsSync(s.path)) continue;
        const all = readFileSync(s.path, "utf8").split("\n").filter((l) => l.trim());
        while (s.loaded < all.length) {
          const data = all[s.loaded] ?? "";
          const id = this.loaded;
          this.loaded++;
          s.loaded++;
          this.buffer.push(data);
          if (this.buffer.length > EVENTS_REPLAY_LINES) this.buffer.shift();
          for (const l of this.listeners) l.send({ id, data });
        }
      }
    } catch {
      // a file may rotate under us; retry on the next tick
    }
  }

  /** Replay the tail to one client, skipping everything <= lastEventId. */
  replayTo(sink: EventSink, lastEventId: number | null): void {
    if (this.loaded === 0) this.load();
    const base = this.loaded - this.buffer.length;
    this.buffer.forEach((data, i) => {
      const id = base + i;
      if (lastEventId !== null && id <= lastEventId) return;
      sink.send({ id, data });
    });
  }

  add(sink: EventSink): void {
    this.listeners.add(sink);
  }

  remove(sink: EventSink): void {
    this.listeners.delete(sink);
  }

  /** Start the poll interval (idempotent). */
  start(): void {
    if (this.pollTimer || this.sources.length === 0) return;
    this.pollTimer = setInterval(() => this.load(), EVENTS_POLL_MS);
    this.pollTimer.unref();
  }

  stop(): void {
    if (this.pollTimer) clearInterval(this.pollTimer);
    this.pollTimer = null;
  }
}

/** Default dispatch: detached spawn of the real `devagent task` pipeline (FR-CTRL-03). */
async function defaultDispatchRunner(spec: DispatchSpec): Promise<{ pid: number | null }> {
  const repoRoot = process.cwd();
  const distCli = join(repoRoot, "dist", "src", "cli.js");
  const argv: string[] = existsSync(distCli)
    ? [process.execPath, distCli, "task", "--prompt", spec.prompt, "--repo", spec.repoPath]
    : ["npx", "tsx", join(repoRoot, "src", "cli.ts"), "task", "--prompt", spec.prompt, "--repo", spec.repoPath];
  if (spec.worker) argv.push("--worker", spec.worker);
  if (spec.maxLoops !== undefined) argv.push("--max-loops", String(spec.maxLoops));
  if (spec.timeoutMinutes !== undefined) argv.push("--timeout", String(spec.timeoutMinutes));
  const head = argv.shift();
  if (!head) return { pid: null };
  const child = cpSpawn(head, argv, {
    cwd: repoRoot,
    env: { ...process.env, DEVAGENT_VISIBILITY: process.env.DEVAGENT_VISIBILITY ?? "visible" },
    stdio: "ignore",
    detached: true,
  });
  child.unref();
  return { pid: typeof child.pid === "number" ? child.pid : null };
}

/** Dispatch body -> validated spec, or the JSON error to return. */
function parseDispatch(
  raw: string,
  defaultRepoPath: string,
): { spec: DispatchSpec; taskId: string } | { error: { status: number; body: Record<string, unknown> } } {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw || "{}");
  } catch {
    return { error: { status: 400, body: { ok: false, note: "invalid JSON body" } } };
  }
  const p = (parsed ?? {}) as Record<string, unknown>;
  if (typeof p.prompt !== "string" || !p.prompt.trim()) {
    return { error: { status: 400, body: { ok: false, note: "prompt must be a nonempty string" } } };
  }
  const spec: DispatchSpec = {
    repoPath: typeof p.repoPath === "string" && p.repoPath ? p.repoPath : defaultRepoPath,
    prompt: p.prompt.trim(),
  };
  if (typeof p.worker === "string" && p.worker) spec.worker = p.worker;
  if (typeof p.role === "string" && p.role) spec.role = p.role;
  const budget = p.budget as Record<string, unknown> | undefined;
  if (budget) {
    const loops = Number(budget.maxLoops);
    if (Number.isFinite(loops)) spec.maxLoops = loops;
    const minutes = Number(budget.timeoutMinutes);
    if (Number.isFinite(minutes)) spec.timeoutMinutes = minutes;
  }
  // Prefix-collision-safe: 8 hex chars of entropy + a uniqueness check against the queue.
  let taskId = `TASK-${randomBytes(4).toString("hex")}`;
  while (readTask(defaultRepoPath, taskId)) taskId = `TASK-${randomBytes(4).toString("hex")}`;
  return { spec, taskId };
}

/** Write the queue row the pipeline consumers (consume.ts, selfbuild claim) expect. */
function enqueueFromSpec(repoPath: string, taskId: string, spec: DispatchSpec): QueuedTask {
  const firstLine = spec.prompt.split("\n")[0] ?? spec.prompt;
  return enqueueTask(repoPath, {
    id: taskId,
    title: firstLine.slice(0, 120),
    goal: spec.prompt,
    source: "daemon",
  });
}

/** Count live run locks under DEVAGENT_HOME/locks (the runregistry's on-disk state). */
function countActiveRuns(home: string): number {
  const locksDir = join(home, "locks");
  let active = 0;
  try {
    for (const f of readdirSync(locksDir)) {
      if (!f.endsWith(".lock")) continue;
      try {
        const holder = JSON.parse(readFileSync(join(locksDir, f), "utf8")) as { startedAt?: number };
        if (Date.now() - (holder.startedAt ?? 0) <= RUN_LOCK_TTL_MS) active++;
      } catch {
        // corrupt lock file does not count as an active run
      }
    }
  } catch {
    // no locks dir -> zero active runs
  }
  return active;
}

async function statusEndpoint(res: ServerResponse, ctx: RouteContext): Promise<void> {
  const cfg = loadConfig(ctx.repoPath);
  const counts = taskCount(ctx.repoPath);
  const proxy = readProxyState(ctx.repoPath);
  sendJson(res, 200, {
    now: new Date().toISOString(),
    uptime_s: Math.floor((Date.now() - ctx.startedAt) / 1000),
    runs: { active: countActiveRuns(devagentHome()), failed_recent: counts.failed },
    queue: { pending: counts.pending, claimed: counts.claimed, done: counts.done },
    circuit: proxy?.circuit ?? "closed",
    herdr: { enabled: herdrEnabled(cfg), session: herdrSessionName(cfg) },
    spawn: { visibility: spawnVisibility(cfg) },
    capabilities: DEVAGENT_CAPABILITIES,
  });
}

async function agentsEndpoint(res: ServerResponse, ctx: RouteContext): Promise<void> {
  let panes: SessionPaneInfo[] = [];
  try {
    panes = await listSessionPanes(herdrSessionName());
  } catch {
    panes = [];
  }
  const queued = listTasks(ctx.repoPath, { status: "pending" }).concat(
    listTasks(ctx.repoPath, { status: "claimed" }),
  );
  sendJson(res, 200, { panes, queued });
}

async function agentEndpoint(res: ServerResponse, repoPath: string, id: string): Promise<void> {
  const task = readTask(repoPath, id);
  if (!task) {
    sendJson(res, 404, { ok: false, note: `no task ${id}` });
    return;
  }
  sendJson(res, 200, { task });
}

async function dispatchEndpoint(req: IncomingMessage, res: ServerResponse, ctx: RouteContext): Promise<void> {
  const raw = await readBody(req);
  const parsed = parseDispatch(raw, ctx.repoPath);
  if ("error" in parsed) {
    sendJson(res, parsed.error.status, parsed.error.body);
    return;
  }
  const { spec, taskId } = parsed;
  let queued: QueuedTask;
  try {
    queued = enqueueFromSpec(ctx.repoPath, taskId, spec);
  } catch (err) {
    sendJson(res, 409, { ok: false, note: (err as Error).message });
    return;
  }
  const { pid } = await ctx.dispatchRunner(spec);
  sendJson(res, 202, { ok: true, taskId: queued.id, pid });
}

async function approveEndpoint(req: IncomingMessage, res: ServerResponse, ctx: RouteContext): Promise<void> {
  const raw = await readBody(req);
  let parsed: Record<string, unknown>;
  try {
    parsed = JSON.parse(raw || "{}") as Record<string, unknown>;
  } catch {
    sendJson(res, 400, { ok: false, note: "invalid JSON body" });
    return;
  }
  const repoPath = typeof parsed.repoPath === "string" && parsed.repoPath ? parsed.repoPath : ctx.repoPath;
  const taskId = typeof parsed.taskId === "string" ? parsed.taskId : "";
  const answer = typeof parsed.answer === "string" ? parsed.answer : "";
  if (!taskId || !answer) {
    sendJson(res, 400, { ok: false, note: "taskId and answer are required" });
    return;
  }
  // The kill sentinel never reaches the answer pipeline: an injected human
  // answer would re-queue the task it is meant to stop (integration review).
  if (answer === KILL_SENTINEL) {
    await killViaAnswerEndpoint(res, ctx, repoPath, taskId);
    return;
  }
  const r = ctx.answerApplier(repoPath, taskId, answer);
  sendJson(res, r.status, r.body);
}

/**
 * Operator kill (kill-via-answer capability): stop a live worker for taskId
 * without touching the answer pipeline. Best-effort herdr pane stop
 * (ctrl+c + workspace close), then a guarded headless-child reap scoped to
 * this repo's .devagent-worktrees (same isDevagentWorkerCmd guard the reaper
 * uses everywhere — never user sessions). No live target -> 404.
 */
async function killViaAnswerEndpoint(
  res: ServerResponse,
  ctx: RouteContext,
  repoPath: string,
  taskId: string,
): Promise<void> {
  const session = herdrSessionName();
  let paneStopped = false;
  let childKilled = false;
  try {
    const panes = await listSessionPanes(session);
    const pane = panes.find((p) => p.taskId === taskId && p.state === "running");
    if (pane) {
      try {
        const keys = await herdrCli(["--session", session, "pane", "send-keys", pane.paneId, "ctrl+c"], { timeoutMs: 5_000 });
        if (keys.code === 0) paneStopped = true;
      } catch {
        // best-effort
      }
      if (pane.workspaceId) {
        try {
          const close = await herdrCli(["--session", session, "workspace", "close", pane.workspaceId], { timeoutMs: 10_000 });
          if (close.code === 0) paneStopped = true;
        } catch {
          // best-effort
        }
      }
    }
  } catch {
    // roster unavailable: fall through to the headless-child reap
  }
  try {
    const stale = findStaleWorkerPids(KILL_MIN_CHILD_AGE_MS, { cwdPrefix: join(repoPath, ".devagent-worktrees") });
    for (const s of stale) killStaleProcessTree(s.pid, "operator-kill");
    childKilled = stale.length > 0;
  } catch {
    // best-effort
  }
  if (!paneStopped && !childKilled) {
    sendJson(res, 404, { ok: false, note: `no live worker pane or child for ${taskId}` });
    return;
  }
  // AuditLedgerRecord has no free-form event field; the operator kill is
  // recorded as a failed audit whose sole unmet criterion is the explicit
  // "operator-kill" token (filterable, visible to readLedger / /history).
  appendAuditRecord(
    ctx.repoPath,
    auditLedgerRecord({
      taskId,
      attempt: 0,
      verdict: {
        verdict: "fail",
        integrity: "clean",
        criteriaResults: [{ criterion: "operator-kill", met: false, evidence: "daemon /approve __kill__" }],
        summary: "operator kill via daemon /approve __kill__ (kill-via-answer)",
      },
    }),
  );
  sendJson(res, 200, { ok: true, killed: true, taskId, note: "operator kill requested" });
}

function eventsEndpoint(req: IncomingMessage, res: ServerResponse, ctx: RouteContext): void {
  res.writeHead(200, {
    "Content-Type": "text/event-stream",
    "Cache-Control": "no-cache",
    Connection: "keep-alive",
  });
  res.write("retry: 3000\n\n");
  const sink: EventSink = {
    send: ({ id, data }) => {
      try {
        res.write(`id: ${id}\ndata: ${data}\n\n`);
      } catch {
        // client vanished mid-write; the close sweep cleans up
      }
    },
  };
  const raw = req.headers["last-event-id"];
  const lastEventId =
    typeof raw === "string" && raw !== "" && Number.isFinite(Number(raw)) ? Number(raw) : null;
  ctx.follower.replayTo(sink, lastEventId);
  ctx.follower.add(sink);

  const heartbeat = setInterval(() => {
    try {
      res.write(": heartbeat\n\n");
    } catch {
      // ignore
    }
  }, EVENTS_HEARTBEAT_MS);
  heartbeat.unref();

  res.on("close", () => {
    clearInterval(heartbeat);
    ctx.follower.remove(sink);
  });
}

async function historyEndpoint(res: ServerResponse, ctx: RouteContext, url: URL): Promise<void> {
  const limitRaw = Number(url.searchParams.get("limit") ?? HISTORY_DEFAULT_LIMIT);
  const limit =
    Number.isFinite(limitRaw) && limitRaw > 0
      ? Math.min(Math.floor(limitRaw), HISTORY_MAX_LIMIT)
      : HISTORY_DEFAULT_LIMIT;
  const taskId = url.searchParams.get("taskId") ?? undefined;
  const records = readLedgerTail(ctx.repoPath, { taskId, limit: HISTORY_MAX_LIMIT });
  sendJson(res, 200, { records: records.slice(-limit) });
}

async function sessionsEndpoint(res: ServerResponse): Promise<void> {
  let panes: SessionPaneInfo[] = [];
  try {
    panes = await listSessionPanes(herdrSessionName());
  } catch {
    panes = [];
  }
  sendJson(res, 200, { panes });
}

async function attachEndpoint(res: ServerResponse, ctx: RouteContext, taskId: string): Promise<void> {
  let command: string | null = null;
  let paneId = "";
  try {
    const panes = await listSessionPanes(herdrSessionName());
    const pane = panes.find((p) => p.taskId === taskId);
    if (pane) {
      command = await attachCommandFor(taskId);
      paneId = pane.paneId;
    }
  } catch {
    command = null;
  }
  if (!command) {
    sendJson(res, 404, { ok: false, note: `no live pane for ${taskId}` });
    return;
  }
  appendOperatorAttachRecord(ctx.repoPath, {
    ts: new Date().toISOString(),
    kind: "event",
    event: "operator-attached",
    taskId,
    attempt: 0,
    paneId,
    session: herdrSessionName(),
  });
  sendJson(res, 200, { ok: true, command });
}

/** Route one request: guards first (healthz, host, origin, auth), then endpoints. */
async function route(
  req: IncomingMessage,
  res: ServerResponse,
  url: URL,
  ctx: RouteContext,
): Promise<void> {
  const path = url.pathname.replace(/\/+$/, "") || "/";
  const method = (req.method ?? "GET").toUpperCase();

  // /healthz is deliberately unauthenticated (liveness probe).
  if (path === "/healthz" && method === "GET") {
    sendJson(res, 200, { ok: true });
    return;
  }

  if (!hostAllowed(req.headers.host)) {
    sendJson(res, 403, { ok: false, note: "forbidden host" });
    return;
  }
  if (!originAllowed(req.headers.origin)) {
    sendJson(res, 403, { ok: false, note: "forbidden origin" });
    return;
  }

  const auth = req.headers.authorization ?? "";
  const expected = `Bearer ${ctx.token}`;
  if (!timingSafeEq(auth, expected)) {
    res.setHeader("WWW-Authenticate", 'Bearer realm="devagent-daemon"');
    sendJson(res, 401, { ok: false, note: "unauthorized" });
    return;
  }

  if (method === "GET" && path === "/status") return statusEndpoint(res, ctx);
  if (method === "GET" && path === "/agents") return agentsEndpoint(res, ctx);
  if (method === "GET" && path.startsWith("/agents/")) {
    return agentEndpoint(res, ctx.repoPath, decodeURIComponent(path.slice("/agents/".length)));
  }
  if (method === "POST" && path === "/dispatch") return dispatchEndpoint(req, res, ctx);
  if (method === "POST" && path === "/approve") return approveEndpoint(req, res, ctx);
  if (method === "GET" && path === "/events") return eventsEndpoint(req, res, ctx);
  if (method === "GET" && path === "/history") return historyEndpoint(res, ctx, url);
  if (method === "GET" && path === "/sessions") return sessionsEndpoint(res);
  if (method === "POST" && path.startsWith("/attach/")) {
    return attachEndpoint(res, ctx, decodeURIComponent(path.slice("/attach/".length)));
  }

  // A known path hit with the wrong method -> 405; everything else -> 404.
  const known =
    ["/status", "/agents", "/dispatch", "/approve", "/events", "/history", "/sessions"].includes(path) ||
    ["/agents/", "/attach/"].some((p) => path.startsWith(p));
  sendJson(res, known ? 405 : 404, { ok: false, note: known ? "method not allowed" : "not found" });
}

/**
 * Start the FR-CTRL daemon. Resolves once the listener is ready.
 * TCP binds 127.0.0.1 only; UDS (udsPath) replaces the transport and relies
 * on filesystem permissions (the token is still issued for client compat).
 */
export async function startDaemon(opts: DaemonOptions = {}): Promise<DaemonHandle> {
  const ctx: RouteContext = {
    repoPath: opts.repoPath ?? process.cwd(),
    token: resolveToken(opts),
    startedAt: Date.now(),
    // SSE sources: the per-run run-log AND the repo orchestration stream the
    // selfbuild loop writes phase/result rows to (without the latter the
    // stream never sees loop progress — the two trees were disjoint).
    follower: new RunLogFollower(devagentHome(), [
      join(opts.repoPath ?? process.cwd(), ".devagent", "runs", "orchestration", "events.jsonl"),
    ]),
    dispatchRunner: opts.dispatchRunner ?? defaultDispatchRunner,
    answerApplier: opts.answerApplier ?? applyAnswerToRepo,
  };

  const server: Server = createServer((req, res) => {
    let url: URL;
    try {
      url = new URL(req.url ?? "/", `http://${req.headers.host ?? "127.0.0.1"}`);
    } catch {
      sendJson(res, 400, { ok: false, note: "bad request URL" });
      return;
    }
    route(req, res, url, ctx).catch((err) => {
      sendJson(res, 500, { ok: false, note: (err as Error).message });
    });
  });

  let resolveListen!: () => void;
  let rejectListen!: (e: Error) => void;
  const listening = new Promise<void>((resolve, reject) => {
    resolveListen = resolve;
    rejectListen = reject;
  });
  server.once("error", (err) => rejectListen(err));
  server.once("listening", () => resolveListen());
  if (opts.udsPath) {
    try {
      unlinkSync(opts.udsPath);
    } catch {
      // no stale socket; bind will fail loudly if the path is stuck
    }
    server.listen(opts.udsPath);
  } else {
    server.listen(opts.port ?? 7788, "127.0.0.1");
  }
  await listening;

  ctx.follower.start();

  const address = server.address();
  const handle: DaemonHandle = {
    port: !opts.udsPath && address && typeof address === "object" ? address.port : null,
    udsPath: opts.udsPath ?? null,
    token: ctx.token,
    stop: () => {
      let resolveStop!: () => void;
      const done = new Promise<void>((resolve) => {
        resolveStop = resolve;
      });
      ctx.follower.stop();
      server.close(() => resolveStop());
      // safety net: linger no longer than 500ms even with keep-alive sockets
      setTimeout(resolveStop, 500).unref();
      return done;
    },
  };
  return handle;
}
