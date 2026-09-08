/**
 * View renderers for the DevAgent control app (FR-UI-02/03/04/08, issue #181).
 * Pure DOM builders: they take already-fetched state and return nodes — no
 * fetching, no timers (main.ts owns the polling/SSE wiring).
 */
import { clock, dur, el, render, type Child } from "./dom.js";
import { stageBadge, type PipelineModel, type StageState } from "./lib/pipeline.js";
import type {
  ApprovalQuestion,
  DaemonStatus,
  HistoryRow,
  RosterPayload,
} from "./lib/protocol.js";
import { rosterRows } from "./lib/state.js";

const STATE_LABEL: Record<string, string> = {
  running: "running",
  idle: "idle",
  failed: "failed (circuit open)",
};

/** Header strip: aggregate dot, counts, connection + approval badges. */
export function renderHeader(
  host: HTMLElement,
  st: DaemonStatus | null,
  agg: "running" | "idle" | "failed",
  approvalCount: number,
  sseState: string,
  onOpenApprovals: () => void,
  onRefresh: () => void,
): void {
  const q = st?.queue ?? {};
  const runs = st?.runs ?? {};
  render(
    host,
    el("span", { class: `dot ${agg}` }),
    el("h1", {}, `DevAgent — ${STATE_LABEL[agg] ?? agg}`),
    el(
      "span",
      { class: "meta" },
      el("span", {}, `queue ${q.pending ?? 0}p/${q.claimed ?? 0}c/${q.done ?? 0}d`),
      el("span", {}, `runs ${runs.active ?? 0}a/${runs.failed_recent ?? 0}f`),
      el("span", {}, `circuit ${st?.circuit ?? "?"}`),
      approvalCount > 0
        ? el("span", { class: "badge approvals", onclick: onOpenApprovals }, `${approvalCount} approval${approvalCount > 1 ? "s" : ""} needed`)
        : el("span", { class: "badge" }, "no approvals pending"),
      el("button", { onclick: onRefresh }, "Refresh"),
    ),
  );
  host.dataset.sse = sseState;
}

/** Left roster: live panes then queued tasks (FR-UI-03). */
export function renderRoster(
  host: HTMLElement,
  payload: RosterPayload | null,
  onAttach: (taskId: string) => void,
): void {
  const rows = rosterRows(payload);
  if (rows.length === 0) {
    render(host, el("p", { class: "none" }, "No live panes or queued tasks."));
    return;
  }
  render(
    host,
    rows.map((r) =>
      el(
        "div",
        { class: "row", "data-task": r.taskId },
        el("strong", {}, r.taskId),
        r.kind === "pane" ? el("span", { class: "badge" }, r.role ?? "worker") : null,
        el("span", { class: `state state-${r.state}` }, r.state),
        r.kind === "pane" && r.state === "running"
          ? el("button", { onclick: () => onAttach(r.taskId) }, "attach")
          : null,
      ),
    ),
  );
}

/** Dispatch sheet (FR-UI-02): prompt + role + worker + budget → POST /dispatch. */
export function buildDispatchForm(
  roles: string[],
  workers: string[],
  onSubmit: (req: {
    prompt: string;
    role?: string;
    worker?: string;
    budget?: { maxLoops?: number; timeoutMinutes?: number };
  }) => Promise<string>,
): HTMLElement {
  const prompt = el("textarea", { placeholder: "Goal / task prompt for the agent team…", required: true }) as HTMLTextAreaElement;
  const role = el("select", {}, roles.map((r) => el("option", { value: r }, r))) as HTMLSelectElement;
  const worker = el("select", {}, workers.map((w) => el("option", { value: w }, w))) as HTMLSelectElement;
  const maxLoops = el("input", { type: "number", min: 1, max: 20, placeholder: "loops (default)" }) as HTMLInputElement;
  const minutes = el("input", { type: "number", min: 5, max: 720, placeholder: "timeout min (default)" }) as HTMLInputElement;
  const note = el("span", { class: "note" }, "Dispatch goes through the daemon's pipeline/budget/gate machinery (FR-CTRL-03) — the app is a transport, not a bypass.");
  const form = el(
    "form",
    {
      class: "dispatch",
      onsubmit: async (ev: Event) => {
        ev.preventDefault();
        if (!prompt.value.trim()) {
          note.className = "err";
          note.textContent = "Prompt is required.";
          return;
        }
        note.className = "note";
        note.textContent = "Dispatching…";
        const msg = await onSubmit({
          prompt: prompt.value,
          role: role.value || undefined,
          worker: worker.value || undefined,
          budget: {
            maxLoops: maxLoops.value ? Number(maxLoops.value) : undefined,
            timeoutMinutes: minutes.value ? Number(minutes.value) : undefined,
          },
        });
        note.className = msg.startsWith("dispatched") ? "ok" : "err";
        note.textContent = msg;
        if (msg.startsWith("dispatched")) prompt.value = "";
      },
    },
    el("label", {}, "Prompt"),
    prompt,
    el("label", {}, "Role"),
    el("div", { class: "pickers" }, role, worker),
    el("label", {}, "Budget"),
    el("div", { class: "pickers" }, maxLoops, minutes),
    el("button", { class: "btn", type: "submit" }, "Dispatch"),
    note,
  );
  return form;
}

/** Approval inbox (FR-UI-04): pending gates with custom answer / approve / deny. */
export function renderApprovals(
  host: HTMLElement,
  pending: ApprovalQuestion[],
  onAnswer: (taskId: string, answer: string) => Promise<void>,
): void {
  if (pending.length === 0) {
    render(host, el("p", { class: "none" }, "Nothing paused for a human decision."));
    return;
  }
  render(
    host,
    pending.map((a) => {
      const input = el("input", { placeholder: "answer to fold into the task contract (optional)" }) as HTMLInputElement;
      return el(
        "div",
        { class: "approval", "data-task": a.taskId },
        el("strong", {}, `${a.taskId} — ${a.title}`),
        el("div", { class: "q" }, a.question || "(no question text)"),
        el("div", { class: "actions" }, input,
          el("button", { class: "btn approve", onclick: () => onAnswer(a.taskId, input.value || "yes") }, "Approve"),
          el("button", { class: "btn deny", onclick: () => onAnswer(a.taskId, "no") }, "Deny"),
        ),
        a.attempts ? el("span", { class: "note" }, `attempts: ${a.attempts}`) : null,
      );
    }),
  );
}

/** Log tail (FR-UI-03): newest at the bottom, auto-scrolled. */
export function renderLog(
  host: HTMLElement,
  lines: Array<{ ts?: string; level?: string; stage?: string; message: string; raw?: boolean }>,
): void {
  const rows = lines.slice(-400).map((l) =>
    el(
      "div",
      {},
      el("span", { class: "ts" }, clock(l.ts)),
      el("span", { class: `lvl lvl-${l.level ?? "evt"}` }, l.raw ? "raw" : (l.level ?? "evt").slice(0, 5)),
      l.stage ? el("span", { class: "stage" }, l.stage.slice(0, 12)) : null,
      document.createTextNode(l.message),
    ),
  );
  render(host, rows);
  host.scrollTop = host.scrollHeight;
}

function stageChip(s: StageState): Child {
  return el(
    "span",
    { class: `stage stage-${s.status}` },
    `${s.id}${s.status === "pending" ? "" : ` ${s.status}`}`,
    s.elapsedMs > 0 ? el("span", { class: "t" }, dur(s.elapsedMs)) : null,
    s.retries > 0 ? el("span", { class: "r" }, `↻${s.retries}`) : null,
  );
}

/** Pipeline DAG (FR-UI-08): one row per scope, stage chips G0–G5 included. */
export function renderPipelines(host: HTMLElement, models: PipelineModel[]): void {
  if (models.length === 0) {
    render(host, el("p", { class: "none" }, "No pipeline events yet — stages appear as the scout → plan → implement → gates → PR flow emits them."));
    return;
  }
  render(
    host,
    models.map((m) => {
      const badge = stageBadge(m);
      return el(
        "div",
        { class: "pipeline", "data-scope": m.scope },
        el("strong", {}, m.scope || "(current run)"),
        badge ? el("span", { class: "badge" }, `stage: ${badge}`) : null,
        m.failedReason ? el("div", { class: "failed-reason" }, `failed: ${m.failedReason}`) : null,
        m.prUrl ? el("div", { class: "ok" }, el("a", { href: m.prUrl, target: "_blank", rel: "noreferrer" }, m.prUrl)) : null,
        el(
          "div",
          { class: "dag" },
          m.stages.flatMap((s, i) =>
            i === 0 ? [stageChip(s)] : [el("span", { class: "arrow" }, "→"), stageChip(s)],
          ),
        ),
        el("div", { class: "stage-detail" },
          m.stages
            .filter((s) => s.detail && s.status !== "pending")
            .map((s) => `${s.id}: ${s.detail}`)
            .join(" · ") || "no stage detail yet",
        ),
      );
    }),
  );
}

/** History table (FR-UI-03): newest ledger rows first. */
export function renderHistory(host: HTMLElement, records: HistoryRow[]): void {
  if (records.length === 0) {
    render(host, el("p", { class: "none" }, "No ledger records yet."));
    return;
  }
  render(
    host,
    el(
      "table",
      {},
      el("thead", {}, el("tr", {}, el("th", {}, "time"), el("th", {}, "task"), el("th", {}, "kind"), el("th", {}, "verdict / event"), el("th", {}, "summary"))),
      el(
        "tbody",
        {},
        [...records].reverse().slice(0, 200).map((r) =>
          el(
            "tr",
            {},
            el("td", {}, clock(typeof r.ts === "string" ? r.ts : undefined)),
            el("td", {}, String(r.taskId ?? "")),
            el("td", {}, String(r.kind ?? "")),
            el("td", {}, String(r.verdict ?? r.event ?? "")),
            el("td", {}, String(r.summary ?? r.detail ?? r.goal ?? "")),
          ),
        ),
      ),
    ),
  );
}
