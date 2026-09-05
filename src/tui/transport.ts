import { request as httpRequest, type ClientRequest, type RequestOptions } from 'node:http';
import { existsSync, readFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';
import type { TuiOptions } from './tui.js';

/**
 * Transport layer for the TUI (FR-TUI over FR-CTRL): bearer-token HTTP and
 * SSE against the daemon at 127.0.0.1 (or a Unix-domain socket). Everything
 * here is a pure client of the daemon API — the TUI never reads daemon state
 * files directly.
 */

export interface HttpResponse {
  status: number;
  body: string;
}

export function awaitResponse(req: ClientRequest): Promise<HttpResponse> {
  return new Promise<HttpResponse>((resolve) => {
    let settled = false;
    const done = (status: number, body: string) => {
      if (!settled) {
        settled = true;
        resolve({ status, body });
      }
    };
    req.on('error', () => done(0, ''));
    req.on('timeout', () => {
      req.destroy();
      done(0, '');
    });
    req.on('response', (res) => {
      let data = '';
      res.setEncoding('utf8');
      res.on('data', (chunk: string) => {
        data += chunk;
      });
      res.on('end', () => done(res.statusCode ?? 0, data));
      res.on('error', () => done(res.statusCode ?? 0, data));
    });
  });
}

/**
 * Path of the daemon's 0600 daemon-token file. The home directory comes from
 * the environment, so traversal-shaped values (any `..` component) are
 * refused outright — the file must live inside the devagent home, nowhere else.
 */
function tokenFilePath(): string {
  const home = process.env.DEVAGENT_HOME || join(process.env.HOME || homedir(), '.devagent');
  if (home.split(/[\\/]/).includes('..')) return '';
  return join(home, 'daemon-token');
}

/** Bearer token for daemon calls: opts > env > the 0600 daemon-token file. */
export function resolveToken(opts: TuiOptions): string {
  if (opts.token) return opts.token;
  if (process.env.DEVAGENT_DAEMON_TOKEN) return process.env.DEVAGENT_DAEMON_TOKEN;
  const file = tokenFilePath();
  if (!file || !existsSync(file)) return '';
  try {
    return readFileSync(file, 'utf8').trim();
  } catch {
    return '';
  }
}

/**
 * Build request options for one daemon call. Host header is bare "127.0.0.1"
 * over UDS (no port exists) and host:port over TCP — the daemon's
 * DNS-rebinding guard accepts both forms. Returns the http.request options
 * plus the headers to send (shared by one-shot and streaming callers).
 */
function requestParts(
  opts: TuiOptions,
  path: string,
  init: { method?: string; body?: string } = {},
): { headers: Record<string, string>; reqOptions: RequestOptions } {
  const headers: Record<string, string> = { Accept: 'application/json' };
  const token = resolveToken(opts);
  if (token) headers.Authorization = `Bearer ${token}`;
  if (init.body !== undefined) headers['Content-Type'] = 'application/json';
  const reqOptions: RequestOptions = { method: init.method ?? 'GET', headers };
  if (opts.udsPath) {
    headers.Host = '127.0.0.1';
    reqOptions.socketPath = opts.udsPath;
    reqOptions.path = path;
  } else {
    const raw = (opts.url ?? process.env.DEVAGENT_DAEMON_URL ?? 'http://127.0.0.1:7788').replace(/\/+$/, '');
    const u = new URL(raw + path);
    headers.Host = u.host;
    reqOptions.hostname = u.hostname;
    reqOptions.port = u.port || (u.protocol === 'https:' ? 443 : 80);
    reqOptions.path = `${u.pathname}${u.search}`;
  }
  return { headers, reqOptions };
}

/**
 * One request against the daemon. Never rejects: {status: 0, body: ''} means
 * unreachable.
 */
export async function daemonRequest(
  opts: TuiOptions,
  path: string,
  init: { method?: string; body?: string } = {},
  timeoutMs = 4_000,
): Promise<HttpResponse> {
  const { headers, reqOptions } = requestParts(opts, path, init);
  reqOptions.timeout = timeoutMs;
  const req = httpRequest(reqOptions);
  const pending = awaitResponse(req);
  if (init.body !== undefined) req.write(init.body);
  req.end();
  return pending;
}

export async function getJson<T>(opts: TuiOptions, path: string): Promise<{ status: number; value: T | null }> {
  const r = await daemonRequest(opts, path);
  let value: T | null = null;
  if (r.status === 200 && r.body) {
    try {
      value = JSON.parse(r.body) as T;
    } catch {
      value = null;
    }
  }
  return { status: r.status, value };
}

export async function postJson(
  opts: TuiOptions,
  path: string,
  body: unknown,
): Promise<{ status: number; ok: boolean; note: string }> {
  const r = await daemonRequest(opts, path, { method: 'POST', body: JSON.stringify(body ?? {}) });
  if (r.status === 0) return { status: 0, ok: false, note: 'daemon unreachable' };
  let parsed: { note?: string; ok?: boolean; error?: string } = {};
  try {
    parsed = JSON.parse(r.body) as { note?: string; ok?: boolean; error?: string };
  } catch {
    /* non-JSON error body */
  }
  const note = parsed.note ?? parsed.error ?? (r.status === 401 ? 'unauthorized (bad token?)' : `HTTP ${r.status}`);
  return { status: r.status, ok: r.status >= 200 && r.status < 300 && parsed.ok !== false, note };
}

/** Handle for a live /events subscription; stop() tears down and stops reconnecting. */
export interface EventsSubscription {
  stop(): void;
  /** Highest event id seen (or -1); feed back as lastEventId to resume without gaps. */
  lastEventId(): number;
}

export type EventsState = 'connecting' | 'live' | 'down';

const SSE_RECONNECT_MS = 3_000;
const SSE_AUTH_RETRY_MS = 15_000;

/**
 * Subscribe to the daemon's SSE /events stream (FR-CTRL run-log tail). Events
 * are dispatched as parsed `data` payloads; `Last-Event-ID` is sent on
 * (re)connect so the daemon's replay skips what we already have. Returns a
 * stop() handle — the caller owns the lifetime. Reconnects forever with
 * backoff until stopped; state callbacks let the UI show LIVE/RECONNECTING.
 */
export function subscribeEvents(
  opts: TuiOptions,
  onEvent: (id: number, data: string) => void,
  onState?: (state: EventsState) => void,
  lastEventId = -1,
): EventsSubscription {
  let stopped = false;
  let timer: NodeJS.Timeout | null = null;
  let lastId = lastEventId;
  let current: ClientRequest | null = null;

  const connect = () => {
    if (stopped) return;
    onState?.('connecting');
    const { headers, reqOptions } = requestParts(opts, '/events');
    headers.Accept = 'text/event-stream';
    if (lastId >= 0) headers['Last-Event-ID'] = String(lastId);
    // No idle timeout: SSE connections are long-lived by design.
    const req = httpRequest(reqOptions);
    current = req;
    let buf = '';

    const schedule = (ms: number) => {
      if (stopped) return;
      req.destroy();
      onState?.('down');
      timer = setTimeout(connect, ms);
      timer.unref?.();
    };

    req.on('error', () => schedule(SSE_RECONNECT_MS));
    req.on('response', (res) => {
      if (res.statusCode !== 200) {
        // 401 will not heal by hammering: retry slowly.
        schedule(res.statusCode === 401 ? SSE_AUTH_RETRY_MS : SSE_RECONNECT_MS);
        return;
      }
      onState?.('live');
      res.setEncoding('utf8');
      res.on('data', (chunk: string) => {
        buf += chunk;
        // Frames end at a blank line; parse as many complete frames as landed.
        let idx: number;
        while ((idx = buf.indexOf('\n\n')) !== -1) {
          const frame = buf.slice(0, idx);
          buf = buf.slice(idx + 2);
          let data = '';
          for (const lineRaw of frame.split('\n')) {
            const line = lineRaw.replace(/\r$/, '');
            if (line.startsWith(':')) continue; // comment / heartbeat
            if (line.startsWith('data:')) data += (data ? '\n' : '') + line.slice(5).trimStart();
            else if (line.startsWith('id:')) {
              const n = Number(line.slice(3).trim());
              if (Number.isFinite(n)) lastId = n;
            }
          }
          if (data) onEvent(lastId, data);
        }
      });
      res.on('end', () => schedule(SSE_RECONNECT_MS));
      res.on('error', () => schedule(SSE_RECONNECT_MS));
    });
    req.end();
  };

  connect();
  return {
    stop() {
      stopped = true;
      if (timer) clearTimeout(timer);
      current?.destroy();
    },
    lastEventId: () => lastId,
  };
}
