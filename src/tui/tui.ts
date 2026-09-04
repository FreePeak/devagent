import { request as httpRequest, type ClientRequest, type RequestOptions } from 'node:http';
import { existsSync, readFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';

/**
 * FR-TUI (PRD §20.8): full-screen alternate-screen terminal dashboard over the
 * FR-CTRL daemon API (src/server/daemon.ts). The TUI is a pure HTTP client of
 * that API — no PTY parsing, no second event system (§20.3 anti-pattern);
 * /status, /agents, /history and /sessions are its only data sources.
 *
 * Transport: bearer token (opts.token > DEVAGENT_DAEMON_TOKEN >
 * $DEVAGENT_HOME/daemon-token — the 0600 file the daemon writes at boot) over
 * 127.0.0.1 HTTP, or a Unix-domain socket via opts.udsPath (FR-CTRL-05;
 * filesystem perms replace the token there, but a known token is still sent).
 *
 * Non-TTY stdin degrades to a one-shot snapshot on stdout + exit 0 — the
 * smoke-testable path. A TTY gets the alternate screen, raw mode, hidden
 * cursor and single-key handling; q / Ctrl+C always restore the screen. A
 * daemon outage never throws: the header degrades to DAEMON UNREACHABLE and
 * polling retries every 2s.
 */

export interface TuiOptions {
  /** Daemon base URL; default http://127.0.0.1:7788 (ignored with udsPath). */
  url?: string;
  /** Bearer token; default DEVAGENT_DAEMON_TOKEN or the daemon-token file. */
  token?: string;
  /** Unix-domain socket path; when set, requests go over the socket. */
  udsPath?: string;
  /** Repo path echoed into the kill (approve) call; default process.cwd(). */
  repoPath?: string;
}

/** Subset of the herdr pane roster the daemon exposes on /agents + /sessions. */
export interface TuiPane {
  taskId: string;
  role: string;
  worker: string;
  paneId: string;
  workspaceId: string;
  label: string;
  cwd: string;
  agentStatus: string;
  state: 'running' | 'idle' | 'stale';
  startedAt: string;
}

/** Queue-row subset of what the daemon exposes on /agents.queued. */
export interface TuiQueuedTask {
  id: string;
  title: string;
  status: string;
  createdAt: string;
}

interface StatusPayload {
  now?: string;
  uptime_s?: number;
  runs?: { active?: number; failed_recent?: number };
  queue?: { pending?: number; claimed?: number; done?: number };
  circuit?: string;
  herdr?: { enabled?: boolean; session?: string };
  spawn?: { visibility?: string };
  capabilities?: string[];
}

interface AgentPayload {
  panes?: TuiPane[];
  queued?: TuiQueuedTask[];
}

type HistoryRow = Record<string, unknown>;

/** What one poll cycle produced; every field tolerates a partial failure. */
export interface Snapshot {
  status: StatusPayload | null;
  agents: AgentPayload | null;
  history: HistoryRow[];
  sessions: TuiPane[] | null;
  /** False only when /status got no HTTP response at all (conn refused/timeout). */
  reachable: boolean;
  /** True when /status answered 401 — token present but wrong. */
  authFailed?: boolean;
  fetchedAt: number;
}

const POLL_MS = 2_000;
const HISTORY_ROWS = 8;

/** Colors: dim lines are the quiet majority (pilot-style dashboard). */
const C = {
  reset: '\x1b[0m',
  dim: '\x1b[2m',
  bold: '\x1b[1m',
  green: '\x1b[32m',
  yellow: '\x1b[33m',
  red: '\x1b[31m',
  cyan: '\x1b[36m',
  magenta: '\x1b[35m',
  inverse: '\x1b[7m',
} as const;

function statusColor(status: string): string {
  switch (status.toLowerCase()) {
    case 'running':
      return C.green;
    case 'idle':
      return C.yellow;
    case 'stale':
      return C.magenta;
    case 'failed':
      return C.red;
    default:
      return C.dim;
  }
}

export function truncate(s: string, n: number): string {
  return s.length <= n ? s : `${s.slice(0, Math.max(0, n - 1))}…`;
}

function visibleLen(s: string): number {
  return s.replace(/\x1b\[[0-9;]*m/g, '').length;
}

/** Pad to `n` visible columns — ANSI color codes must not count toward width. */
function padTo(s: string, n: number): string {
  return s + ' '.repeat(Math.max(1, n - visibleLen(s)));
}

export function dim(s: string): string {
  return `${C.dim}${s}${C.reset}`;
}
/** Cyan accent: attach hints and other §20.8 emphasis. */
export function cyan(s: string): string {
  return `${C.cyan}${s}${C.reset}`;
}

/** Bearer token for daemon calls: opts > env > the 0600 daemon-token file. */
function resolveToken(opts: TuiOptions): string {
  if (opts.token) return opts.token;
  if (process.env.DEVAGENT_DAEMON_TOKEN) return process.env.DEVAGENT_DAEMON_TOKEN;
  const file = join(process.env.DEVAGENT_HOME || join(process.env.HOME || homedir(), '.devagent'), 'daemon-token');
  if (!existsSync(file)) return '';
  try {
    return readFileSync(file, 'utf8').trim();
  } catch {
    return '';
  }
}

interface HttpResponse {
  status: number;
  body: string;
}

function awaitResponse(req: ClientRequest): Promise<HttpResponse> {
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
 * One request against the daemon. Never rejects: {status: 0, body: ''} means
 * unreachable. Host header is bare "127.0.0.1" over UDS (no port exists) and
 * host:port over TCP — the daemon's DNS-rebinding guard accepts both forms.
 */
async function daemonRequest(
  opts: TuiOptions,
  path: string,
  init: { method?: string; body?: string } = {},
  timeoutMs = 4_000,
): Promise<HttpResponse> {
  const headers: Record<string, string> = { Accept: 'application/json' };
  const token = resolveToken(opts);
  if (token) headers.Authorization = `Bearer ${token}`;
  if (init.body !== undefined) headers['Content-Type'] = 'application/json';
  const reqOptions: RequestOptions = { method: init.method ?? 'GET', headers, timeout: timeoutMs };
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
  const req = httpRequest(reqOptions);
  const pending = awaitResponse(req);
  if (init.body !== undefined) req.write(init.body);
  req.end();
  return pending;
}

async function getJson<T>(opts: TuiOptions, path: string): Promise<{ status: number; value: T | null }> {
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

async function postJson(
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

const EMPTY_SNAPSHOT: Snapshot = { status: null, agents: null, history: [], sessions: null, reachable: false, fetchedAt: 0 };

/** Poll /status + /agents + /history + /sessions concurrently. Never throws. */
export async function fetchSnapshot(opts: TuiOptions): Promise<Snapshot> {
  try {
    const [st, ag, hi, se] = await Promise.all([
      getJson<StatusPayload>(opts, '/status'),
      getJson<AgentPayload>(opts, '/agents'),
      getJson<HistoryRow[] | { records?: HistoryRow[] }>(opts, `/history?limit=${HISTORY_ROWS}`),
      getJson<TuiPane[]>(opts, '/sessions'),
    ]);
    return {
      status: st.value,
      agents: ag.value,
      // /history answers {records:[...]}; accept either shape but unwrap the
      // envelope so the history panel renders (the bare-array expectation
      // silently yielded [] forever).
      history: Array.isArray(hi.value)
        ? hi.value
        : hi.value !== null && typeof hi.value === "object" && Array.isArray(hi.value.records)
          ? hi.value.records
          : [],
      sessions: se.value,
      reachable: st.status !== 0,
      authFailed: st.status === 401,
      fetchedAt: Date.now(),
    };
  } catch {
    // getJson never rejects, but a snapshot is never worth crashing over.
    return { ...EMPTY_SNAPSHOT, fetchedAt: Date.now() };
  }
}

/**
 * Aggregate header status: FAILED when recent failures exist, RUNNING when
 * the daemon reports active runs or any pane is live, else IDLE.
 */
export function aggregateStatus(status: StatusPayload | null, panes: TuiPane[]): 'RUNNING' | 'IDLE' | 'FAILED' {
  if (!status) return 'IDLE';
  if ((status.runs?.failed_recent ?? 0) > 0) return 'FAILED';
  const runningPanes = panes.filter((p) => p.state === 'running').length;
  if ((status.runs?.active ?? 0) > 0 || runningPanes > 0) return 'RUNNING';
  return 'IDLE';
}

/** startedAt → compact elapsed ("12m", "3h", "2d"); '' when absent/unparseable. */
function fmtElapsed(startedAt: unknown): string {
  if (!startedAt) return '';
  const t = Date.parse(String(startedAt));
  if (!Number.isFinite(t)) return '';
  const mins = Math.max(0, Math.floor((Date.now() - t) / 60_000));
  if (mins < 60) return `${mins}m`;
  const hours = Math.floor(mins / 60);
  if (hours < 48) return `${hours}h`;
  return `${Math.floor(hours / 24)}d`;
}

/** ISO ts → local HH:MM:SS for the history rows; blanks when unparseable. */
function fmtClock(ts: unknown): string {
  if (typeof ts !== 'string' || !ts) return '         ';
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return '         ';
  return d.toTimeString().slice(0, 8);
}

type CardLines = string[];

/** Status chip: colored dot + label, e.g. "● running" (Pilot-style). */
export function chipFor(state: string, label?: string): string {
  const dot = state === 'running' || state === 'ok' ? C.green : state === 'failed' ? C.red : state === 'stale' ? C.magenta : C.yellow;
  return `${dot}●${C.reset} ${statusColor(state)}${truncate(label || state, 18)}${C.reset}`;
}

/** Boxed worker card (Pilot-style panel): title bar + status/cwd body. */
function paneCardLines(p: TuiPane, inner: number): CardLines {
  const id = p.taskId || p.label || '?';
  const el = fmtElapsed(p.startedAt);
  const body = [
    ` ${chipFor(p.state, p.agentStatus || p.state)} ${el ? `${C.dim}· ${el}${C.reset}` : ''} ${C.dim}${truncate(p.worker || '-', 12)}${C.reset}`,
    ` ${C.dim}cwd ${truncate(p.cwd, Math.max(10, inner - 8))}${C.reset}`,
    ` ${C.cyan}devagent attach ${truncate(id, Math.max(8, inner - 18))}${C.reset}`,
  ];
  return boxLines(truncate(id, inner - 6), body, inner);
}

function queuedCardLines(t: TuiQueuedTask, inner: number): CardLines {
  const body = [
    ` ${chipFor('queued', 'queued')}  ${C.dim}${truncate(t.title ?? '', Math.max(10, inner - 14))}${C.reset}`,
    ` ${C.dim}waiting for a worker claim${C.reset}`,
  ];
  return boxLines(truncate(t.id || '?', inner - 6), body, inner);
}

/**
 * Rounded-box panel lines for visible width `w`: ╭─ title ─╮ / body /
 * ╰──╯. visibleLen measures title+body so ANSI colors never skew borders.
 */
export function boxLines(title: string, body: string[], w: number): string[] {
  const tl = visibleLen(title);
  const head = `${C.dim}╭─${C.reset} ${title} ${C.dim}${'─'.repeat(Math.max(1, w - tl - 5))}╮${C.reset}`;
  const rows = body.map((b) => `${padTo(b + ' ', w - 1)}${C.dim}│${C.reset}`);
  const foot = `${C.dim}╰${'─'.repeat(Math.max(1, w - 2))}╯${C.reset}`;
  return [head, ...rows, foot];
}

export interface RenderOptions {
  /** /sessions view instead of the worker cards. */
  showSessions?: boolean;
  /** Help overlay above the cards. */
  showHelp?: boolean;
  /** One-line transient note (kill confirm, errors) in the footer. */
  note?: string;
  /** TaskId awaiting a y/n confirm for the kill flow. */
  pendingKill?: string | null;
}

/** Full frame (multi-line, no screen-control codes) for the current snapshot. */
export function renderDashboard(snap: Snapshot, ropts: RenderOptions = {}): string {
  const width = process.stdout?.columns ?? 100;
  const half = Math.max(34, Math.floor(width / 2));
  const status = snap.status;
  const panes = snap.agents?.panes ?? snap.sessions ?? [];
  const queued = snap.agents?.queued ?? [];
  const lines: string[] = [];

  // Header: inverse-video full-width title strip with the aggregate status
  // embedded — the Pilot "toolbar" cue. Auth/offline variants stay red-on-normal.
  if (snap.authFailed) {
    lines.push(`${C.bold}${C.red} DevAgent — DAEMON AUTH REJECTED ${C.reset}${C.dim} token invalid (DEVAGENT_DAEMON_TOKEN / daemon-token file)${C.reset}`);
  } else if (!snap.reachable || !status) {
    lines.push(`${C.bold}${C.red} DevAgent — DAEMON UNREACHABLE ${C.reset}${C.dim} retrying every ${POLL_MS / 1000}s · start the daemon${C.reset}`);
  } else {
    const agg = aggregateStatus(status, panes);
    const q = status.queue ?? {};
    const circ = status.circuit ? ` · circuit:${status.circuit}` : '';
    const meta = `${q.pending ?? 0}p/${q.claimed ?? 0}c/${q.done ?? 0}d · herdr:${status.herdr?.session ?? '-'} · vis:${status.spawn?.visibility ?? 'visible'}${circ}`;
    const chip = `${statusColor(agg)}● ${agg}${C.reset}`;
    const barBody = ` DevAgent  ${chip}  ${C.dim}${meta}${C.reset} `;
    lines.push(`${C.bold}${C.inverse}${padTo(barBody, Math.max(width, visibleLen(barBody) + 1))}${C.reset}`);
  }
  lines.push('');

  if (ropts.showHelp) {
    lines.push(
      `${C.bold}Keys${C.reset}`,
      '  r  refresh now',
      '  s  toggle sessions view (herdr panes)',
      '  k  kill the running task via POST /approve (answer __kill__); daemon must advertise kill-via-answer',
      '  y  confirm the pending kill — any other key cancels',
      '  ?  toggle this help',
      '  q or Ctrl+C  quit',
      '',
    );
  }

  lines.push(
    ropts.showSessions
      ? `${C.bold}▌Sessions${C.reset} ${C.dim}herdr panes${C.reset}`
      : `${C.bold}▌Workers${C.reset} ${C.dim}${panes.length} pane(s) · ${queued.length} queued${C.reset}`,
    '',
  );

  if (ropts.showSessions) {
    if (!panes.length) lines.push(dim('  no live sessions'));
    for (const p of panes) {
      const el = fmtElapsed(p.startedAt);
      lines.push(
        `  ${C.cyan}${truncate(p.paneId || '-', 18)}${C.reset}  ${C.bold}${truncate(p.taskId || '?', 24)}${C.reset}  ${chipFor(p.state, p.agentStatus || p.state)}${el ? ` ${C.dim}· ${el}${C.reset}` : ''}  ${C.dim}${truncate(p.cwd, Math.max(20, width - 72))}${C.reset}`,
      );
    }
  } else {
    const cards: CardLines[] = [
      ...panes.map((p) => paneCardLines(p, half - 2)),
      ...queued.map((t) => queuedCardLines(t, half - 2)),
    ];
    if (!cards.length) lines.push(dim('  no workers, queue empty'));
    for (let i = 0; i < cards.length; i += 2) {
      const a = cards[i]!;
      const b = cards[i + 1];
      const rows = Math.max(a.length, b?.length ?? 0);
      for (let r = 0; r < rows; r++) {
        lines.push(padTo(a[r] ?? '', half) + (b ? (b[r] ?? '') : ''));
      }
      lines.push('');
    }
    if (cards.length) lines.pop(); // single blank between cards and history
  }

  lines.push(`${C.bold}▌History${C.reset} ${C.dim}ledger tail${C.reset}`, '');
  const history = snap.history.slice(-HISTORY_ROWS);
  if (!history.length) {
    lines.push(dim('  no ledger rows'));
  } else {
    for (const row of history) {
      // Row shapes vary by producer: loop-result rows carry {status, loop,
      // goal}; audit/attach rows carry {event, taskId}. Surface whichever
      // verdict + subject the row has (narrowed reads, no blind casts).
      const rec = row as Record<string, unknown>;
      const ev = typeof rec.event === 'string' && rec.event
        ? rec.event
        : typeof rec.status === 'string' && rec.status
          ? rec.status
          : typeof rec.kind === 'string' ? rec.kind : '';
      const subject = typeof rec.goal === 'string' && rec.goal
        ? rec.goal
        : typeof rec.taskId === 'string' ? rec.taskId : '';
      lines.push(
        `  ${C.dim}${fmtClock(row.ts)}${C.reset}  ${C.cyan}${truncate(ev, 14)}${C.reset}  ${truncate(subject, Math.max(20, width - 26))}`,
      );
    }
  }

  const notes: string[] = [];
  if (ropts.pendingKill) notes.push(`kill ${ropts.pendingKill}: press y to confirm, any other key cancels`);
  if (ropts.note) notes.push(ropts.note);
  lines.push(
    '',
    `${C.inverse} [r] refresh [s] sessions [k] kill [?] help [q] quit ${C.reset}` +
      (notes.length ? `  ${C.yellow}${truncate(notes.join(' · '), Math.max(20, width - 48))}${C.reset}` : ''),
  );
  return lines.join('\n');
}

/** First kill candidate: running pane, else any pane, else first queued row. */
function pickKillTarget(snap: Snapshot): string | null {
  const panes = snap.agents?.panes ?? [];
  const running = panes.find((p) => p.state === 'running');
  if (running?.taskId) return running.taskId;
  if (panes[0]?.taskId) return panes[0].taskId;
  return snap.agents?.queued?.find((q) => q.status === 'pending')?.id ?? null;
}

/**
 * Kill via the same gate machinery as the CLI (FR-CTRL-03): POST /approve with
 * answer __kill__, only when the daemon advertises the capability. Returns the
 * operator-facing note; never throws.
 */
async function executeKill(opts: TuiOptions, snap: Snapshot, taskId: string): Promise<string> {
  const caps = snap.status?.capabilities ?? [];
  if (!caps.includes('kill-via-answer')) return 'kill: not supported by this daemon';
  const r = await postJson(opts, '/approve', { repoPath: opts.repoPath ?? process.cwd(), taskId, answer: '__kill__' });
  return r.ok ? `kill: ${taskId} accepted (${r.note})` : `kill ${taskId} failed: ${r.note}`;
}

/** One-shot mode: single snapshot render to stdout, exit 0. Never throws. */
async function runOneShot(opts: TuiOptions): Promise<void> {
  const snap = await fetchSnapshot(opts);
  const out = process.stdout;
  const flushed = new Promise<void>((resolve) => {
    if (out.write(renderDashboard(snap) + '\n')) resolve();
    else out.once('drain', resolve);
  });
  await flushed;
}

/** Interactive mode: alternate screen + raw mode; resolves on quit. */
async function runInteractive(opts: TuiOptions): Promise<void> {
  const out = process.stdout;
  const stdin = process.stdin;
  let quitResolve!: () => void;
  const untilQuit = new Promise<void>((resolve) => {
    quitResolve = resolve;
  });

  let stopped = false;
  let polling = false;
  let timer: ReturnType<typeof setTimeout> | null = null;
  let showSessions = false;
  let showHelp = false;
  let pendingKill: string | null = null;
  let note = 'connecting…';
  let snap: Snapshot = { ...EMPTY_SNAPSHOT, fetchedAt: Date.now() };

  const draw = () => {
    out.write(`\x1b[H\x1b[2J${renderDashboard(snap, { showSessions, showHelp, note, pendingKill })}\n`);
  };

  const quit = () => {
    if (stopped) return;
    stopped = true;
    if (timer) clearTimeout(timer);
    stdin.removeListener('data', onData);
    try {
      stdin.setRawMode(false);
    } catch {
      /* already non-raw */
    }
    stdin.pause();
    out.write('\x1b[?1049l\x1b[?25h');
    quitResolve();
  };

  const onData = (buf: Buffer) => {
    const ch = buf.toString('utf8')[0];
    if (pendingKill) {
      if (ch === 'y' || ch === 'Y') {
        const target = pendingKill;
        pendingKill = null;
        note = `killing ${target}…`;
        void executeKill(opts, snap, target).then((msg) => {
          note = msg;
          if (!stopped) draw();
        });
      } else {
        pendingKill = null;
        note = 'kill cancelled';
      }
      draw();
      return;
    }
    switch (ch) {
      case 'r':
        void poll();
        return;
      case 's':
        showSessions = !showSessions;
        break;
      case 'k':
        beginKill();
        break;
      case '?':
        showHelp = !showHelp;
        break;
      case '\x1b':
        showHelp = false;
        break;
      case 'q':
      case '\x03':
        quit();
        return;
      default:
        return; // ignore unhandled keys
    }
    draw();
  };

  const beginKill = () => {
    const caps = snap.status?.capabilities ?? [];
    if (!caps.includes('kill-via-answer')) {
      note = 'kill: not supported by this daemon';
      return;
    }
    const target = pickKillTarget(snap);
    if (!target) {
      note = 'kill: no running task';
      return;
    }
    pendingKill = target;
  };

  const poll = async () => {
    if (polling || stopped) return;
    polling = true;
    try {
      const next = await fetchSnapshot(opts);
      snap = next;
      note = next.reachable ? '' : 'daemon unreachable — retrying';
      if (!stopped) draw();
    } finally {
      polling = false;
    }
    if (!stopped) timer = setTimeout(() => void poll(), POLL_MS);
  };

  stdin.on('data', onData);
  void poll();
  await untilQuit;
}

/**
 * Run the dashboard (FR-TUI-01). Non-TTY stdin degrades to a one-shot
 * snapshot + exit 0; a TTY gets the full-screen app. Never throws into the
 * caller — the alternate screen and cursor are restored on every exit path.
 */
export async function runTui(opts: TuiOptions = {}): Promise<void> {
  if (!process.stdin.isTTY) {
    await runOneShot(opts);
    return;
  }
  const out = process.stdout;
  let entered = false;
  try {
    out.write('\x1b[?1049h\x1b[?25l');
    entered = true;
    process.stdin.setRawMode(true);
    process.stdin.resume();
    // Ctrl+C in raw mode never delivers SIGINT (the terminal has the TTY in
    // raw mode), but a signal from outside (kill -INT, PTY teardown) must
    // still quit cleanly — restore happens in runInteractive's quit().
    const onSigint = () => {
      process.stdin.write('q');
    };
    process.once('SIGINT', onSigint);
    try {
      await runInteractive(opts);
    } finally {
      process.removeListener('SIGINT', onSigint);
    }
  } catch (err) {
    // Best-effort surface, then restore the user's terminal; never crash.
    try {
      process.stderr.write(`tui: ${err instanceof Error ? err.message : String(err)}\n`);
    } catch {
      /* stderr gone */
    }
    process.exitCode = 0;
  } finally {
    if (entered) {
      try {
        process.stdin.setRawMode(false);
      } catch {
        /* not raw */
      }
      out.write('\x1b[?1049l\x1b[?25h');
    }
  }
}
