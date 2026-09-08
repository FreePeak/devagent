/**
 * FR-CTRL client for the desktop control app (issue #181).
 *
 * The app is a thin renderer over the daemon API (FR-UI-07): every call here
 * maps 1:1 onto a documented endpoint (src/server/daemon.ts), authenticated
 * with the per-boot bearer token the Rust shell reads from
 * DEVAGENT_HOME/daemon-token. No business logic lives in this module beyond
 * transport shaping; no endpoint is bypassed or invented.
 *
 * DOM-free and dependency-free on purpose: the same code is unit-tested in
 * the root vitest suite (test/desktop-client.test.ts) against the real
 * in-process daemon.
 */
import type {
  ApprovalQuestion,
  AttachResult,
  DaemonStatus,
  DispatchRequest,
  DispatchResult,
  EventLine,
  HistoryRow,
  RosterPayload,
} from "./protocol.js";
export type { EventLine } from "./protocol.js";

/** Injectable fetch surface; `typeof fetch` in the webview, a stub in tests. */
export type FetchLike = (
  input: string,
  init: {
    method?: string;
    headers?: Record<string, string>;
    body?: string;
    signal?: AbortSignal;
  },
) => Promise<{
  status: number;
  json: () => Promise<unknown>;
  body?: { getReader: () => StreamReader } | null;
}>;

type StreamReader = {
  read: () => Promise<{ done: boolean; value?: Uint8Array | string }>;
  cancel?: () => Promise<unknown>;
};

export interface ClientConfig {
  /** Daemon base URL, e.g. `http://127.0.0.1:7788` (no trailing slash). */
  baseUrl: string;
  /** Bearer token from the daemon-token file / DEVAGENT_DAEMON_TOKEN. */
  token: string;
  fetchImpl?: FetchLike;
}

/** One API call outcome: status 0 means unreachable (never throws). */
export interface CallResult<T> {
  status: number;
  value: T | null;
  note?: string;
}

export class DaemonClient {
  private readonly fetchImpl: FetchLike;

  constructor(private readonly cfg: ClientConfig) {
    this.fetchImpl = cfg.fetchImpl ?? ((input, init) => globalThis.fetch(input, init) as never);
  }

  private async call<T>(
    path: string,
    init: { method?: string; body?: string } = {},
  ): Promise<CallResult<T>> {
    try {
      const res = await this.fetchImpl(`${this.cfg.baseUrl}${path}`, {
        method: init.method ?? "GET",
        headers: {
          Authorization: `Bearer ${this.cfg.token}`,
          ...(init.body !== undefined ? { "Content-Type": "application/json" } : {}),
        },
        body: init.body,
      });
      const parsed = (await res.json().catch(() => null)) as T | null;
      const note =
        parsed && typeof parsed === "object" && "note" in (parsed as Record<string, unknown>)
          ? String((parsed as Record<string, unknown>).note)
          : undefined;
      return { status: res.status, value: res.status >= 200 && res.status < 300 ? parsed : null, note };
    } catch (err) {
      return { status: 0, value: null, note: (err as Error)?.message ?? "unreachable" };
    }
  }

  /** GET /healthz — unauthenticated liveness probe. */
  async health(): Promise<CallResult<{ ok: boolean }>> {
    return this.call("/healthz");
  }

  /** GET /status — aggregate loop state (FR-UI-01 data source). */
  async status(): Promise<CallResult<DaemonStatus>> {
    return this.call<DaemonStatus>("/status");
  }

  /** GET /agents — roster: live panes + queued tasks (FR-UI-03). */
  async roster(): Promise<CallResult<RosterPayload>> {
    return this.call<RosterPayload>("/agents");
  }

  /** GET /approvals — tasks paused for a human decision (FR-UI-04 inbox). */
  async approvals(): Promise<CallResult<{ pending: ApprovalQuestion[] }>> {
    const r = await this.call<{ pending?: ApprovalQuestion[] }>("/approvals");
    return { ...r, value: r.value ? { pending: r.value.pending ?? [] } : null };
  }

  /** GET /history — ledger tail, newest `limit` rows (FR-UI-03 task history). */
  async history(opts: { limit?: number; taskId?: string } = {}): Promise<CallResult<{ records: HistoryRow[] }>> {
    const params = new URLSearchParams();
    if (opts.limit) params.set("limit", String(opts.limit));
    if (opts.taskId) params.set("taskId", opts.taskId);
    const qs = params.toString();
    const r = await this.call<{ records?: HistoryRow[] }>(`/history${qs ? `?${qs}` : ""}`);
    return { ...r, value: r.value ? { records: r.value.records ?? [] } : null };
  }

  /** POST /dispatch — same pipeline/budget/gate machinery as the CLI (FR-UI-02). */
  async dispatch(req: DispatchRequest): Promise<CallResult<DispatchResult>> {
    return this.call<DispatchResult>("/dispatch", { method: "POST", body: JSON.stringify(req) });
  }

  /** POST /approve — one gate decision (approve/deny/kill) for a paused task (FR-UI-04). */
  async approve(body: { taskId: string; answer: string; repoPath?: string }): Promise<CallResult<{ ok: boolean; note?: string }>> {
    return this.call("/approve", { method: "POST", body: JSON.stringify(body) });
  }

  /** POST /attach/:taskId — the herdr attach command for jump-in (FR-VIS). */
  async attach(taskId: string): Promise<CallResult<AttachResult>> {
    return this.call<AttachResult>(`/attach/${encodeURIComponent(taskId)}`, { method: "POST" });
  }
}

/**
 * Parse one SSE `data:` payload (same tolerant semantics as the TUI's
 * src/tui/viz.ts: a corrupt line degrades to a raw entry, never throws).
 */
export function parseEventLine(text: string): EventLine {
  try {
    const obj = JSON.parse(text) as Record<string, unknown>;
    const stage = typeof obj.stage === "string" ? obj.stage : typeof obj.event === "string" ? obj.event : undefined;
    let message = typeof obj.message === "string" && obj.message ? obj.message : "";
    if (!message) {
      const bits: string[] = [];
      if (typeof obj.phase === "string") bits.push(`phase: ${obj.phase}`);
      if (typeof obj.detail === "string" && obj.detail) bits.push(obj.detail);
      if (typeof obj.loop === "number") bits.push(`(loop ${obj.loop})`);
      message = bits.join(" — ");
    }
    return {
      ts: typeof obj.ts === "string" ? obj.ts : undefined,
      level: typeof obj.level === "string" ? obj.level.toLowerCase() : undefined,
      stage,
      event: typeof obj.event === "string" ? obj.event : undefined,
      phase: typeof obj.phase === "string" ? obj.phase : undefined,
      detail: typeof obj.detail === "string" ? obj.detail : undefined,
      taskId: typeof obj.taskId === "string" ? obj.taskId : undefined,
      runId: typeof obj.runId === "string" ? obj.runId : undefined,
      message: message || text,
    };
  } catch {
    return { message: text, raw: true };
  }
}

export type EventsState = "connecting" | "live" | "down";

export interface EventsSubscription {
  stop(): void;
  /** Highest event id seen (or -1); resume from it to replay without gaps. */
  lastEventId(): number;
}

export interface EventsHandlers {
  onEvent: (id: number, line: EventLine) => void;
  onState?: (state: EventsState) => void;
}

const SSE_RECONNECT_MS = 3_000;
const SSE_AUTH_RETRY_MS = 15_000;

/**
 * Subscribe to GET /events (FR-CTRL-04 SSE tail) over fetch + ReadableStream.
 * Reconnects with backoff until stopped; `Last-Event-ID` is sent on every
 * (re)connect so the daemon's replay skips what the UI already rendered.
 */
export function subscribeEvents(
  cfg: ClientConfig,
  handlers: EventsHandlers,
  opts: { lastEventId?: number; fetchImpl?: FetchLike; setTimeoutImpl?: (fn: () => void, ms: number) => unknown } = {},
): EventsSubscription {
  const fetchImpl = opts.fetchImpl ?? cfg.fetchImpl ?? ((input, init) => globalThis.fetch(input, init) as never);
  const later = opts.setTimeoutImpl ?? ((fn, ms) => setTimeout(fn, ms));
  let stopped = false;
  let lastId = opts.lastEventId ?? -1;
  let controller: AbortController | null = null;

  const connect = async (): Promise<void> => {
    if (stopped) return;
    handlers.onState?.("connecting");
    try {
      const res = await fetchImpl(`${cfg.baseUrl}/events`, {
        method: "GET",
        headers: {
          Accept: "text/event-stream",
          Authorization: `Bearer ${cfg.token}`,
          ...(lastId >= 0 ? { "Last-Event-ID": String(lastId) } : {}),
        },
        signal: controller?.signal,
      });
      if (stopped) return;
      if (res.status !== 200 || !res.body) {
        handlers.onState?.("down");
        if (!stopped) later(connect, res.status === 401 ? SSE_AUTH_RETRY_MS : SSE_RECONNECT_MS);
        return;
      }
      handlers.onState?.("live");
      const reader = res.body.getReader();
      let buf = "";
      for (;;) {
        const { done, value } = await reader.read();
        if (done || stopped) break;
        buf += typeof value === "string" ? value : new TextDecoder().decode(value);
        let idx: number;
        while ((idx = buf.indexOf("\n\n")) !== -1) {
          const frame = buf.slice(0, idx);
          buf = buf.slice(idx + 2);
          let data = "";
          for (const lineRaw of frame.split("\n")) {
            const line = lineRaw.replace(/\r$/, "");
            if (line.startsWith(":")) continue; // comment / heartbeat
            if (line.startsWith("data:")) data += (data ? "\n" : "") + line.slice(5).trimStart();
            else if (line.startsWith("id:")) {
              const n = Number(line.slice(3).trim());
              if (Number.isFinite(n)) lastId = n;
            }
          }
          if (data) handlers.onEvent(lastId, parseEventLine(data));
        }
      }
    } catch {
      // transport error: fall through to the reconnect below
    }
    if (stopped) return;
    handlers.onState?.("down");
    later(connect, SSE_RECONNECT_MS);
  };

  controller = new AbortController();
  void connect();
  return {
    stop() {
      stopped = true;
      controller?.abort();
    },
    lastEventId: () => lastId,
  };
}
