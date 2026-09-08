/**
 * DevAgent control app entrypoint (issue #181). Wires the FR-CTRL client to
 * the views: 2s REST polling (status/agents/approvals/history) + one SSE
 * subscription for the live tail (FR-CTRL-04). All rendering goes through
 * views.ts; all daemon knowledge through lib/client.ts (FR-UI-07: thin
 * client — the app holds no credentials besides the daemon token and no
 * business logic; losing it degrades to CLI-only operation).
 */
import { DaemonClient, subscribeEvents, type EventsState, type EventLine } from "./lib/client.js";
import type { DaemonStatus, RosterPayload, ApprovalQuestion, HistoryRow, DispatchRequest } from "./lib/protocol.js";
import { aggregateStatus } from "./lib/state.js";
import { buildPipelines } from "./lib/pipeline.js";
import { desiredNotifications } from "./lib/notify.js";
import { el, render } from "./dom.js";
import {
  buildDispatchForm,
  renderApprovals,
  renderHeader,
  renderHistory,
  renderLog,
  renderPipelines,
  renderRoster,
} from "./views.js";

/// Tauri command bridge (withGlobalTauri); absent when served by plain vite.
interface TauriBridge {
  invoke(cmd: string, args?: Record<string, unknown>): Promise<unknown>;
}
function tauri(): TauriBridge | null {
  const g = globalThis as Record<string, unknown>;
  return g.__TAURI_INTERNALS__ ? (g.__TAURI__ as TauriBridge) : null;
}

const POLL_MS = 2_000;
const ROLES = ["worker", "scout", "reviewer", "curator", "po", "prd-curator"] as const;
const WORKERS = ["", "omp", "opencode", "claude-code", "pi", "grok"] as const;

type View = "dashboard" | "approvals" | "dispatch" | "history";

interface AppState {
  view: View;
  status: DaemonStatus | null;
  roster: RosterPayload | null;
  approvals: ApprovalQuestion[];
  history: HistoryRow[];
  events: EventLine[];
  sse: EventsState;
  sseLastId: number;
  authFailed: boolean;
  connNote: string;
}

const app = document.getElementById("app")!;
const headerEl = el("header");
const navEl = el("nav");
const mainEl = el("section");
const footerEl = el("footer", { class: "footer" });
app.append(headerEl, el("main", {}, navEl, mainEl), footerEl);

let client: DaemonClient | null = null;
let cfg: { baseUrl: string; token: string } = { baseUrl: "", token: "" };

const state: AppState = {
  view: "dashboard",
  status: null,
  roster: null,
  approvals: [],
  history: [],
  events: [],
  sse: "connecting",
  sseLastId: -1,
  authFailed: false,
  connNote: "",
};

const notifiedScopes: Record<string, true> = {};

/// Browser fast-path: 127.0.0.1 origin means the vite dev server, which
/// cannot read DEVAGENT_HOME — require explicit URL+token via query params
/// so local UI iteration stays possible without the shell.
function browserConfigFallback(): { baseUrl: string; token: string } | null {
  if (typeof location !== "undefined" && location.hostname === "127.0.0.1") {
    const q = new URLSearchParams(location.search);
    const url = q.get("daemon");
    const token = q.get("token");
    if (url && token) return { baseUrl: url.replace(/\/+$/, ""), token };
  }
  return null;
}

async function initClient(): Promise<void> {
  const t = tauri();
  if (t) {
    try {
      const dc = (await t.invoke("daemon_config")) as { url: string; token: string; uds_path: string | null };
      if (dc.uds_path) {
        state.connNote = `daemon over UDS ${dc.uds_path} — the webview cannot reach a unix socket directly; set DEVAGENT_DAEMON_URL to the TCP port for UI traffic`;
        return;
      }
      cfg = { baseUrl: dc.url, token: dc.token };
    } catch (err) {
      state.connNote = (err as Error).message ?? String(err);
      return;
    }
  } else {
    const fb = browserConfigFallback();
    if (!fb) {
      state.connNote = "no daemon config — run the packaged app, or pass ?daemon=<url>&token=<token> for vite dev";
      return;
    }
    cfg = fb;
  }
  client = new DaemonClient(cfg);
}

function renderNav(): void {
  const items: Array<[View, string]> = [
    ["dashboard", "Dashboard"],
    ["dispatch", "Dispatch"],
    ["approvals", `Approvals${state.approvals.length ? ` (${state.approvals.length})` : ""}`],
    ["history", "History"],
  ];
  render(
    navEl,
    items.map(([v, label]) =>
      el("div", { class: `row${state.view === v ? " selected" : ""}`, onclick: () => { state.view = v; draw(); } }, el("strong", {}, label)),
    ),
  );
}

function renderFooter(): void {
  render(
    footerEl,
    el("span", { class: state.sse === "live" ? "sse-live" : "sse-down" }, `events: ${state.sse}`),
    state.authFailed ? el("span", { class: "sse-down" }, "auth failed (401) — token mismatch with the running daemon") : null,
    state.connNote ? el("span", {}, state.connNote) : null,
    el("span", {}, client ? "thin client of `devagent daemon` (FR-CTRL)" : "disconnected"),
  );
}

function renderMain(): void {
  switch (state.view) {
    case "dashboard": {
      mainEl.replaceChildren(
        el("h2", {}, "Agent roster"),
        el("div", { class: "roster" }),
        el("h2", {}, "Pipeline"),
        el("div", { class: "pipelines" }),
        el("h2", {}, "Live log"),
        el("div", { class: "log" }),
      );
      renderRoster(mainEl.querySelector<HTMLElement>(".roster")!, state.roster, attachTo);
      renderPipelines(mainEl.querySelector<HTMLElement>(".pipelines")!, buildPipelines(state.events));
      renderLog(mainEl.querySelector<HTMLElement>(".log")!, state.events);
      break;
    }
    case "dispatch": {
      mainEl.replaceChildren(
        el("h2", {}, "Dispatch a run"),
        buildDispatchForm([...ROLES], [...WORKERS], dispatchSubmit),
      );
      break;
    }
    case "approvals": {
      const host = el("div");
      renderApprovals(host, state.approvals, async (taskId, answer) => {
        if (!client) return;
        await client.approve({ taskId, answer });
        await pollOnce();
        draw();
      });
      mainEl.replaceChildren(el("h2", {}, "Approval inbox (FR-UI-04)"), host);
      break;
    }
    case "history": {
      const host = el("div");
      renderHistory(host, state.history);
      mainEl.replaceChildren(el("h2", {}, "Task history (ledger)"), host);
      break;
    }
  }
}

async function dispatchSubmit(req: DispatchRequest): Promise<string> {
  if (!client) return "no daemon connection";
  const r = await client.dispatch(req);
  if (r.status === 202 && r.value?.ok) {
    state.history = [];
    return `dispatched as ${r.value.taskId} (pid ${r.value.pid ?? "?"})`;
  }
  return `dispatch failed (${r.status}): ${r.note ?? "unknown error"}`;
}

function draw(): void {
  const agg = aggregateStatus(state.status, state.roster?.panes ?? []);
  renderHeader(
    headerEl,
    state.status,
    agg,
    state.approvals.length,
    state.sse,
    () => { state.view = "approvals"; draw(); },
    () => { void pollOnce().then(draw).catch(() => undefined); },
  );
  void setTray(agg);
  renderNav();
  renderFooter();
  renderMain();
}

async function setTray(agg: "running" | "idle" | "failed"): Promise<void> {
  const t = tauri();
  if (!t) return;
  try {
    await t.invoke("set_tray_state", { state: agg });
  } catch {
    // tray surface is best-effort; never blocks the UI
  }
}

async function pushNotifications(): Promise<void> {
  const t = tauri();
  if (!t) return;
  const notes = desiredNotifications(state.events.slice(-50), state.approvals, notifiedScopes);
  for (const n of notes) {
    try {
      await t.invoke("notify", { title: n.title, body: n.body.slice(0, 200) });
    } catch {
      // notifications are best-effort (permission can be denied per-OS)
    }
  }
}

async function pollOnce(): Promise<void> {
  if (!client) return;
  const [status, roster, approvals, history] = await Promise.all([
    client.status(),
    client.roster(),
    client.approvals(),
    state.history.length === 0 ? client.history({ limit: 100 }) : Promise.resolve(null),
  ]);
  state.authFailed = status.status === 401;
  state.status = status.value;
  if (roster.value) state.roster = roster.value;
  if (approvals.value) state.approvals = approvals.value.pending;
  if (history?.value) state.history = history.value.records;
}

async function attachTo(taskId: string): Promise<void> {
  if (!client) return;
  const r = await client.attach(taskId);
  state.connNote = r.value?.command
    ? `attach with: ${r.value.command}`
    : `attach failed (${r.status}): ${r.note ?? "no live pane"}`;
  renderFooter();
}

function wireEvents(): void {
  if (!client) return;
  subscribeEvents(cfg, {
    onEvent: (id, line) => {
      state.events.push(line);
      if (state.events.length > 400) state.events.splice(0, state.events.length - 400);
      state.sseLastId = id;
      if (state.view === "dashboard") renderMain();
      void pushNotifications();
    },
    onState: (s) => {
      state.sse = s;
      renderFooter();
    },
  }, { lastEventId: state.sseLastId });
}

async function main(): Promise<void> {
  await initClient();
  if (client) {
    wireEvents();
    await pollOnce();
    void pushNotifications();
    setInterval(() => { void pollOnce().then(draw).catch(() => undefined); }, POLL_MS);
  }
  draw();
}

void main();
