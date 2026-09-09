// DevAgent Control UI (FR-UI-02/03/04/08 webview half).
// Thin by contract (FR-UI-07): this code renders state and forwards intent to
// the Rust core via Tauri commands; it holds no business logic and no I/O of
// its own. All daemon traffic (HTTP + SSE) lives in src-tauri.

interface TauriEventApi {
  event: {
    listen(event: string, handler: (e: { payload: unknown }) => void): void;
  };
}

type Cmd = (cmd: string, args?: Record<string, unknown>) => Promise<unknown>;

declare global {
  interface Window {
    // Populated by tauri.conf.json app.withGlobalTauri (enabled).
    __TAURI_INTERNALS__?: { invoke: Cmd };
    __TAURI__?: TauriEventApi;
  }
}

export {};

declare global {
  interface Window {
    // Populated by tauri.conf.json app.withGlobalTauri (enabled).
    __TAURI_INTERNALS__?: { invoke: Cmd };
    __TAURI__?: TauriEventApi;
  }
}


let invoke: Cmd;
const internals = window.__TAURI_INTERNALS__;
if (internals) {
  // Real Tauri runtime: the same module loads in a plain browser for layout
  // smoke-testing (renders "daemon unreachable").
  invoke = (cmd, args) => internals.invoke(cmd, args);
} else {
  console.warn("[devagent-control] not running inside Tauri — UI shell only");
  invoke = async () => null;
}


interface StatusBody {
  now: string;
  uptime_s: number;
  runs: { active: number; failed_recent: number };
  queue: { pending: number; claimed: number; done: number };
  circuit: string;
  herdr: { enabled: boolean; session: string };
  spawn: { visibility: string };
  capabilities: string[];
}

interface PaneInfo {
  taskId: string;
  role: string;
  worker: string;
  paneId: string;
  label: string;
  cwd: string;
  agentStatus: string;
  state: "running" | "idle" | "stale";
  startedAt: string;
}

interface QueuedTask {
  id: string;
  title: string;
  goal: string;
  status: "pending" | "claimed" | "done" | "failed";
  createdAt: string;
  updatedAt: string;
}

interface AgentsBody {
  panes: PaneInfo[];
  queued: QueuedTask[];
}

interface HistoryRow {
  ts: string;
  taskId?: string;
  kind?: string;
  event?: string;
  verdict?: string;
  goal?: string;
  summary?: string;
  [k: string]: unknown;
}

interface StageNode {
  name: string;
  status: "pending" | "running" | "pass" | "fail" | "skip";
  elapsed_ms: number;
  retries: number;
}

interface RunTimeline {
  run_id: string;
  stages: StageNode[];
}

// ---------------------------------------------------------------------------
// Small helpers.

function esc(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

function fmtElapsed(ms: number): string {
  if (ms <= 0) return "";
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m${s % 60 ? ` ${s % 60}s` : ""}`;
  return `${Math.floor(m / 60)}h${m % 60 ? ` ${m % 60}m` : ""}`;
}

function el<T extends HTMLElement>(tag: string, cls?: string, html?: string): T {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (html !== undefined) e.innerHTML = html;
  return e as T;
}

async function daemonJson<T>(cmd: string, args?: Record<string, unknown>): Promise<{ code: number; body: T | null; err: boolean }> {
  try {
    const [code, body] = (await invoke(cmd, args)) as [number, T];

    return { code, body, err: false };
  } catch {
    return { code: 0, body: null, err: true };
  }
}

const $ = <T extends HTMLElement = HTMLElement>(id: string) =>
  document.getElementById(id) as T | null ?? (() => {
    throw new Error(`missing #${id}`);
  })();

// ---------------------------------------------------------------------------
// Tabs

const tabs = ["roster", "approvals", "pipeline", "history"] as const;
type Tab = (typeof tabs)[number];

function selectTab(tab: Tab) {
  for (const t of tabs) {
    $(`tab-${t}`).classList.toggle("active", t === tab);
    document
      .querySelector(`#tabs button[data-tab="${t}"]`)!
      .classList.toggle("active", t === tab);
  }
}

for (const b of document.querySelectorAll<HTMLButtonElement>("#tabs button")) {
  b.addEventListener("click", () => selectTab(b.dataset.tab as Tab));
}

// ---------------------------------------------------------------------------
// FR-UI-03: roster + queue + live log tail.

const MAX_LOG_LINES = 500;

function renderRoster(body: AgentsBody | null) {
  const roster = $("roster");
  roster.innerHTML = "";
  if (!body || body.panes.length === 0) {
    roster.appendChild(el("li", "muted", "no live agent panes"));
  } else {
    for (const p of body.panes) {
      const li = el(
        "li",
        undefined,
        `<span class="agent-status ${esc(p.state)}">${esc(p.state)}</span> ` +
          `<strong>${esc(p.taskId || p.label)}</strong>` +
          `<span class="muted"> · ${esc(p.worker || "?")} · ${esc(p.role || "?")}</span>`,
      );
      roster.appendChild(li);
    }
  }
  const queue = $("queue");
  queue.innerHTML = "";
  if (!body || body.queued.length === 0) {
    queue.appendChild(el("li", "muted", "queue empty"));
  } else {
    for (const q of body.queued) {
      queue.appendChild(
        el(
          "li",
          undefined,
          `<strong>${esc(q.id)}</strong> <span class="muted">${esc(q.status)}</span><br/>${esc(q.title || q.goal.slice(0, 90))}`,
        ),
      );
    }
  }
}

function renderLogRow(data: string) {
  const tail = $("log-tail");
  const row = el("div", "log-row");
  try {
    const obj = JSON.parse(data) as Record<string, unknown>;
    const ts = typeof obj.ts === "string" ? obj.ts.slice(11, 19) : "";
    const stage = (obj.stage as string) || (obj.event as string) || "";
    const level = ((obj.level as string) || "info").toLowerCase();
    let msg = (obj.message as string) || "";
    if (!msg && obj.phase) {
      const bits: string[] = [];
      if (obj.phase) bits.push(`phase: ${obj.phase}`);
      if (obj.detail) bits.push(String(obj.detail));
      if (obj.loop !== undefined) bits.push(`(loop ${obj.loop})`);
      msg = bits.join(" — ");
    }
    if (!msg) msg = data;
    row.innerHTML =
      `<span class="log-ts">${esc(ts)}</span>` +
      (stage ? `<span class="log-stage">${esc(stage)}</span>` : "") +
      `<span class="level-${esc(level)}">${esc(msg)}</span>`;
  } catch {
    row.textContent = data;
  }
  const stick = tail.scrollHeight - tail.scrollTop - tail.clientHeight < 40;
  tail.appendChild(row);
  while (tail.childElementCount > MAX_LOG_LINES) tail.firstElementChild!.remove();
  if (stick) tail.scrollTop = tail.scrollHeight;
}

// ---------------------------------------------------------------------------
// FR-UI-04: approval inbox.

interface ApprovalItem {
  taskId: string;
  title?: string;
  detail?: string;
}

let approvals: ApprovalItem[] = [];

function renderApprovals() {
  const list = $("approvals");
  list.innerHTML = "";
  const badge = $("approval-count");
  badge.hidden = approvals.length === 0;
  badge.textContent = String(approvals.length);
  if (approvals.length === 0) {
    list.appendChild(el("li", "muted", "nothing waiting on you"));
    return;
  }
  for (const item of approvals) {
    const li = el(
      "li",
      undefined,
      `<strong>${esc(item.taskId)}</strong> ${item.title ? `— ${esc(item.title)}` : ""}` +
        `<div class="approval-actions">` +
        `<input type="text" placeholder='answer (y / n / free text)' data-answer="${esc(item.taskId)}" />` +
        `<button class="primary" data-approve="${esc(item.taskId)}">Approve</button>` +
        `<button data-deny="${esc(item.taskId)}">Deny</button>` +
        `</div>`,
    );
    list.appendChild(li);
  }
}

document.addEventListener("click", async (ev) => {
  const t = ev.target as HTMLElement;
  const approveId = t.dataset?.approve;
  const denyId = t.dataset?.deny;
  if (!approveId && !denyId) return;
  const taskId = approveId ?? denyId!;
  const input = document.querySelector<HTMLInputElement>(`input[data-answer="${taskId}"]`);
  let answer = input?.value.trim() ?? "";
  if (denyId) answer = "no";
  else if (answer === "") answer = "yes";
  if (answer.toLowerCase() === "y") answer = "yes";
  if (answer.toLowerCase() === "n") answer = "no";
  try {
    await invoke("approve", { taskId, answer });
    approvals = approvals.filter((a) => a.taskId !== taskId);
    renderApprovals();
  } catch (e) {
    alert(`approve failed: ${e}`);
  }
});

function pushApproval(item: ApprovalItem) {
  if (!approvals.some((a) => a.taskId === item.taskId)) {
    approvals.push(item);
    renderApprovals();
  }
}

// ---------------------------------------------------------------------------
// FR-UI-08: pipeline view.

const CANON: readonly string[] = ["scout", "plan", "implement", "gates", "pr"];

function renderPipeline(t: RunTimeline | null) {
  const view = $("pipeline-view");
  view.innerHTML = "";
  if (!t || t.stages.length === 0) {
    view.appendChild(el("p", "muted", "no pipeline rows yet for this run"));
    return;
  }
  const rank = (n: string) => {
    const i = CANON.indexOf(n);
    return i === -1 ? CANON.length : i;
  };
  const stages = [...t.stages].sort(
    (a, b) => rank(a.name) - rank(b.name) || a.name.localeCompare(b.name),
  );
  stages.forEach((s, i) => {
    if (i > 0) view.appendChild(el("span", "pipeline-arrow", "→ "));
    const chip = el(
      "span",
      `stage-chip ${s.status}`,
      `${esc(s.name)} · ${s.status}`,
    );
    chip.title = `elapsed ${s.elapsed_ms}ms, retries ${s.retries}`;
    const meta = el(
      "span",
      "stage-meta",
      `${fmtElapsed(s.elapsed_ms)}${s.retries ? ` · ${s.retries} retry${s.retries > 1 ? "s" : ""}` : ""}`,
    );
    const row = el("div", "pipeline-stage");
    row.appendChild(chip);
    row.appendChild(meta);
    view.appendChild(row);
  });
}

async function refreshPipeline() {
  const runId = ($("run-select") as HTMLSelectElement).value;
  if (runId) {
    const t = (await invoke("get_pipeline", { runId })) as RunTimeline | null;
    renderPipeline(t);
  } else {
    try {
      const t = (await invoke("get_pipeline_from_history", {})) as RunTimeline;
      renderPipeline(t);
    } catch {
      renderPipeline(null);
    }
  }
}

async function refreshRunList() {
  try {
    const runs = (await invoke("list_runs", {})) as string[];
    const sel = $("run-select") as HTMLSelectElement;
    const current = sel.value;
    sel.innerHTML = "";
    sel.appendChild(new Option("(history fallback)", ""));
    for (const r of runs.sort()) sel.appendChild(new Option(r, r));
    sel.value = current;
    if (!sel.value) sel.value = "";
  } catch {
    // not in Tauri; ignore
  }
}

$("pipeline-refresh").addEventListener("click", refreshPipeline);
$("run-select").addEventListener("change", refreshPipeline);

// ---------------------------------------------------------------------------
// FR-UI-02: dispatch sheet.

const dispatchSheet = $("dispatch-sheet") as HTMLDialogElement;
$("open-dispatch").addEventListener("click", () => dispatchSheet.showModal());
$("dispatch-cancel").addEventListener("click", () => dispatchSheet.close());

$("dispatch-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = ev.target as HTMLFormElement;
  const fd = new FormData(f);
  const result = $("dispatch-result");
  result.textContent = "dispatching…";
  try {
    const [, body] = (await invoke("dispatch", {
      prompt: String(fd.get("prompt") || ""),
      role: String(fd.get("role") || ""),
      worker: String(fd.get("worker") || ""),
      repoPath: String(fd.get("repoPath") || ""),
      autoPr: fd.get("autoPr") === "on",
      maxLoops: fd.get("maxLoops") ? Number(fd.get("maxLoops")) : null,
      timeoutMinutes: fd.get("timeoutMinutes") ? Number(fd.get("timeoutMinutes")) : null,
    })) as [number, { taskId: string; pid: number | null }];
    result.textContent = `dispatched as ${body?.taskId ?? "?"} ✓`;
    setTimeout(() => dispatchSheet.close(), 900);
  } catch (e) {
    result.textContent = `dispatch failed: ${e}`;
  }
});

// ---------------------------------------------------------------------------
// Settings.

const settingsSheet = $("settings-sheet") as HTMLDialogElement;
$("open-settings").addEventListener("click", async () => {
  try {
    const [baseUrl] = (await invoke("get_daemon_config", {})) as [string, string];
    (settingsSheet.querySelector('input[name="baseUrl"]') as HTMLInputElement).value = baseUrl;
  } catch {
    // not in Tauri
  }
  settingsSheet.showModal();
});
$("settings-cancel").addEventListener("click", () => settingsSheet.close());
$("settings-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const fd = new FormData(ev.target as HTMLFormElement);
  try {
    await invoke("set_daemon_config", {
      baseUrl: String(fd.get("baseUrl") || ""),
      token: String(fd.get("token") || "") || null,
    });
  } catch (e) {
    alert(`settings failed: ${e}`);
  }
  settingsSheet.close();
});

// ---------------------------------------------------------------------------
// Poll loop + SSE subscriptions (data arrives from the Rust core).

let lastAggregate = "";

async function pollOnce() {
  const { code, body, err } = await daemonJson<StatusBody>("get_status");
  const dot = $("conn-dot");
  const label = $("conn-label");
  if (err || code >= 500) {
    dot.className = "dot offline";
    label.textContent = "daemon unreachable — degrade to CLI (devagent …)";
  } else if (code === 401) {
    dot.className = "dot auth";
    label.textContent = "auth failed — set the token in Settings";
  } else if (code === 0) {
    dot.className = "dot offline";
    label.textContent = "daemon unreachable";
  } else {
    dot.className = "dot ok";
    const s = body!;
    label.textContent = `up ${fmtElapsed(s.uptime_s * 1000)} · runs ${s.runs.active} active / ${s.runs.failed_recent} failed · queue ${s.queue.pending}+${s.queue.claimed}/${s.queue.done} · circuit ${s.circuit}`;
  }

  const agents = await daemonJson<AgentsBody>("get_agents");
  renderRoster(agents.err ? null : agents.body);

  const hist = await daemonJson<{ records: HistoryRow[] }>("get_history", { limit: 100 });
  renderHistory(hist.err ? null : hist.body?.records ?? null);
}

function renderHistory(records: HistoryRow[] | null) {
  const list = $("history-list");
  list.innerHTML = "";
  if (!records || records.length === 0) {
    list.appendChild(el("li", "muted", "no ledger records"));
    return;
  }
  // newest last per the ledger tail; render newest first
  for (const r of [...records].reverse()) {
    const what =
      r.summary ||
      r.goal ||
      (r.verdict ? `verdict ${r.verdict}` : r.event || r.kind || "record");
    list.appendChild(
      el(
        "li",
        undefined,
        `<span class="muted">${esc(String(r.ts ?? "").slice(0, 19))}</span> ` +
          `${r.taskId ? `<strong>${esc(r.taskId)}</strong> ` : ""}${esc(what)}`,
      ),
    );
  }
}

function wireTauriEvents() {
  // withGlobalTauri exposes window.__TAURI__.event.listen; the core emits
  // daemon://* events from its poll and SSE threads.
  const tauri = window.__TAURI__;
  if (!tauri?.event?.listen) {
    console.warn("[devagent-control] event API not present; live tail disabled");
    return;
  }
  tauri.event.listen("daemon://event", (e) => {
    const payload = String(e.payload ?? "");
    renderLogRow(payload);
    const row = parseJsonRecord(payload);
    if (row?.verdict === "ask" && typeof row.taskId === "string") {
      pushApproval({
        taskId: row.taskId,
        title: typeof row.summary === "string" ? row.summary : undefined,
      });
    }
  });
  tauri.event.listen("daemon://aggregate", (e) => {
    const label = String(e.payload ?? "");
    if (label !== lastAggregate) {
      lastAggregate = label;
      $("aggregate-label").textContent = `aggregate: ${label}`;
    }
  });
  tauri.event.listen("daemon://approval-needed", (e) => {
    pushApproval({ taskId: String(e.payload ?? "") });
    selectTab("approvals");
  });
  tauri.event.listen("daemon://run", () => {
    void refreshRunList();
  });
}

/// External JSON (the SSE data line): validate shape, never blind-cast.
function parseJsonRecord(text: string): Record<string, unknown> | null {
  try {
    const v: unknown = JSON.parse(text);
    return v && typeof v === "object" && !Array.isArray(v)
      ? (v as Record<string, unknown>)
      : null;
  } catch {
    return null;
  }
}

async function main() {
  wireTauriEvents();
  renderApprovals();
  await refreshRunList();
  await pollOnce();
  setInterval(pollOnce, 5000);
  setInterval(refreshPipeline, 5000);
}

void main();
