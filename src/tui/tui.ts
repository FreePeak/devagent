/**
 * FR-TUI (PRD §20.8): full-screen alternate-screen terminal dashboard over the
 * FR-CTRL daemon API (src/server/daemon.ts). The TUI is a pure HTTP + SSE
 * client of that API — no PTY parsing, no second event system (§20.3
 * anti-pattern); /status, /agents, /history, /sessions and the /events run-log
 * tail are its only data sources.
 *
 * v2 borrows from the reference dashboards the operator named:
 * - pilot — sparkline metric cards, the `u` upgrade hint (FR-TUI-05), chips.
 * - htop — proportional meters, j/k navigation with a selection cursor,
 *   always-fits-the-terminal layout, and flicker-free incremental redraw
 *   (src/tui/frame.ts diffs frames instead of clearing the screen).
 * - Claude Code — the live log tail view (FR-TUI-03), Enter-to-expand detail
 *   panels, a contextual footer hint bar, and a spinner only while running.
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
 * polling retries every 2s; the SSE tail reconnects on its own.
 */

import { decodeKeys, type Key } from './input.js';
import { renderFrame } from './frame.js';
import { daemonRequest, getJson, postJson, subscribeEvents, type EventsState } from './transport.js';
import { formatLogLine, meterBar, parseLogLine, sparkline, visibleLen, type LogLine } from './viz.js';
import { DEVAGENT_VERSION } from '../version.js';

export interface TuiOptions {
  /** Daemon base URL; default http://127.0.0.1:7788 (ignored with udsPath). */
  url?: string;
  /** Bearer token; default DEVAGENT_DAEMON_TOKEN or the daemon-token file. */
  token?: string;
  /** Unix-domain socket path; when set, requests go over the socket. */
  udsPath?: string;
  /** Repo path echoed into the kill (approve) call; default process.cwd(). */
  repoPath?: string;
  /**
   * Never start an embedded daemon — attach to a running one or degrade to
   * the UNREACHABLE header (glances `-c` analog). Default: probe, attach if
   * reachable, else embed a daemon for this session.
   */
  attachOnly?: boolean;
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
/** Sparkline memory: one sample per completed poll → ~2 minutes of activity. */
const SPARK_SAMPLES = 60;
/** Live-log ring buffer bound (lines kept from /events). */
const LOG_CAP = 1_000;
const SPINNER = '⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏';
const TICKER_MS = 150;

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

/** The three dashboard views; 1/2/3 switch, s and l toggle (htop-like tabs). */
export type TuiView = 'workers' | 'sessions' | 'log';

/** Modal panels: per-item detail (Claude Code's expand) or the upgrade hint. */
export interface OverlayState {
  kind: 'detail' | 'upgrade';
  item?: TuiPane | TuiQueuedTask;
}

/** Client-sampled activity series for the header sparkline (pilot cue). */
export interface MetricsState {
  /** Active-worker counts, one per completed poll (newest last). */
  samples: number[];
  /** Ms between samples (the poll interval) — for the window label. */
  sampleMs: number;
}

/** Everything the log view needs; the interactive loop owns the buffer. */
export interface LogViewState {
  lines: LogLine[];
  /** Lines scrolled back from the tail; 0 = following the newest output. */
  scroll: number;
  follow: boolean;
  state: EventsState | 'off';
  /** runId of the newest structured line, when known. */
  source?: string;
}

export interface RenderOptions {
  /** Legacy alias: renders the sessions view (superseded by view). */
  showSessions?: boolean;
  /** Help overlay above the cards. */
  showHelp?: boolean;
  /** One-line transient note (kill confirm, errors) in the footer. */
  note?: string;
  /** TaskId awaiting a y/n confirm for the kill flow. */
  pendingKill?: string | null;
  view?: TuiView;
  /** Selection cursor index into the current view's item list. */
  selection?: number;
  overlay?: OverlayState | null;
  metrics?: MetricsState;
  log?: LogViewState;
  /** Terminal row budget; interactive passes stdout.rows, tests pass exact. */
  rows?: number;
  /** Spinner animation frame (interactive only; animated while RUNNING). */
  spinnerFrame?: number;
  /** When the dashboard embedded its own daemon for this session, say so. */
  daemonMode?: 'attach' | 'embedded';
}

const EMPTY_SNAPSHOT: Snapshot = { status: null, agents: null, history: [], sessions: null, reachable: false, fetchedAt: 0 };

/**
 * Runtime shape guards for daemon payloads. The endpoints' envelopes are part
 * of no shared schema — the TUI must trust only what it re-validates here.
 * The `/sessions` crash (2026-09-05): the endpoint answers {panes:[...]} but
 * the raw value was assigned into a TuiPane[]-typed field, and the first
 * spread of it threw "not iterable" inside the raw-mode key handler, killing
 * the alternate-screen app.
 */

/** `/sessions` payload: bare array or {panes:[...]}; anything else → null. */
function normalizeSessions(value: unknown): TuiPane[] | null {
  if (Array.isArray(value)) return value;
  if (value !== null && typeof value === 'object' && Array.isArray((value as { panes?: unknown }).panes)) {
    return (value as { panes: TuiPane[] }).panes;
  }
  return null;
}

/** `/agents` payload: keep panes/queued only when they are actually arrays. */
function normalizeAgents(value: unknown): AgentPayload | null {
  if (value === null || typeof value !== 'object') return null;
  const raw = value as { panes?: unknown; queued?: unknown };
  return {
    panes: Array.isArray(raw.panes) ? (raw.panes as TuiPane[]) : [],
    queued: Array.isArray(raw.queued) ? (raw.queued as TuiQueuedTask[]) : [],
  };
}

/**
 * Pane roster for rendering, status aggregation and selection: the agents
 * payload first, the sessions roster as fallback. Runtime-array-checked —
 * hand-built or future-shaped snapshots must degrade to empty, never throw.
 */
function rosterPanes(snap: Snapshot): TuiPane[] {
  if (Array.isArray(snap.agents?.panes)) return snap.agents.panes;
  if (Array.isArray(snap.sessions)) return snap.sessions;
  return [];
}

/** Queued rows for rendering/selection; empty when the payload is not an array. */
function queueRows(snap: Snapshot): TuiQueuedTask[] {
  return Array.isArray(snap.agents?.queued) ? snap.agents.queued : [];
}

/** Poll /status + /agents + /history + /sessions concurrently. Never throws. */
export async function fetchSnapshot(opts: TuiOptions): Promise<Snapshot> {
  try {
    const [st, ag, hi, se] = await Promise.all([
      getJson<StatusPayload>(opts, '/status'),
      getJson<unknown>(opts, '/agents'),
      getJson<HistoryRow[] | { records?: HistoryRow[] }>(opts, `/history?limit=${HISTORY_ROWS}`),
      getJson<unknown>(opts, '/sessions'),
    ]);
    return {
      status: st.value,
      agents: normalizeAgents(ag.value),
      // /history answers {records:[...]}; accept either shape but unwrap the
      // envelope so the history panel renders (the bare-array expectation
      // silently yielded [] forever).
      history: Array.isArray(hi.value)
        ? hi.value
        : hi.value !== null && typeof hi.value === "object" && Array.isArray(hi.value.records)
          ? hi.value.records
          : [],
      // /sessions answers {panes:[...]}; same envelope treatment as /history.
      sessions: normalizeSessions(se.value),
      reachable: st.status !== 0,
      authFailed: st.status === 401,
      fetchedAt: Date.now(),
    };
  } catch {
    // getJson never rejects, but a snapshot is never worth crashing over.
    return { ...EMPTY_SNAPSHOT, fetchedAt: Date.now() };
  }
}

export function aggregateStatus(status: Snapshot['status'], panes: TuiPane[]): 'RUNNING' | 'IDLE' | 'FAILED' {
  if (!status) return 'IDLE';
  const runningPanes = panes.filter((p) => p.state === 'running').length;
  if (
    (status.runs?.active ?? 0) > 0 ||
    runningPanes > 0 ||
    (status.queue?.claimed ?? 0) > 0
  ) {
    return 'RUNNING';
  }
  // FAILED means live trouble (circuit open — the factory cannot dispatch),
  // not "some task failed at some point": runs.failed_recent is a lifetime
  // queue-failed count that never decays, so it pinned the header at FAILED
  // permanently (2026-09-05: healthy closed-circuit factory showed FAILED).
  if (status.circuit === 'open') return 'FAILED';
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

/** uptime seconds → compact "3h12m" / "45s". */
function fmtUptime(s: number | undefined): string {
  if (typeof s !== 'number' || !Number.isFinite(s) || s < 0) return '-';
  if (s < 60) return `${Math.floor(s)}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  return `${h}h${m % 60}m`;
}

type CardLines = string[];

/** Status chip: colored dot + label, e.g. "● running" (Pilot-style). */
export function chipFor(state: string, label?: string): string {
  const dot = state === 'running' || state === 'ok' ? C.green : state === 'failed' ? C.red : state === 'stale' ? C.magenta : C.yellow;
  return `${dot}●${C.reset} ${statusColor(state)}${truncate(label || state, 18)}${C.reset}`;
}

/** Boxed worker card (Pilot-style panel): title bar + status/cwd body. */
function paneCardLines(p: TuiPane, inner: number, selected: boolean): CardLines {
  const id = p.taskId || p.label || '?';
  const el = fmtElapsed(p.startedAt);
  const body = [
    ` ${chipFor(p.state, p.agentStatus || p.state)} ${el ? `${C.dim}· ${el}${C.reset}` : ''} ${C.dim}${truncate(p.worker || '-', 12)}${C.reset}`,
    ` ${C.dim}cwd ${truncate(p.cwd, Math.max(10, inner - 8))}${C.reset}`,
    ` ${C.cyan}devagent attach ${truncate(id, Math.max(8, inner - 18))}${C.reset}`,
  ];
  return boxLines(selected ? `${C.cyan}▸${C.reset} ${truncate(id, inner - 8)}` : truncate(id, inner - 6), body, inner);
}

function queuedCardLines(t: TuiQueuedTask, inner: number, selected: boolean): CardLines {
  const body = [
    ` ${chipFor('queued', 'queued')}  ${C.dim}${truncate(t.title ?? '', Math.max(10, inner - 14))}${C.reset}`,
    ` ${C.dim}waiting for a worker claim${C.reset}`,
  ];
  return boxLines(selected ? `${C.cyan}▸${C.reset} ${truncate(t.id || '?', inner - 8)}` : truncate(t.id || '?', inner - 6), body, inner);
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

/** Terminal width/rows; ptys can report 0 (no size) — treat that as unknown. */
function termColumns(): number {
  const c = process.stdout?.columns;
  return typeof c === 'number' && c >= 20 ? c : 100;
}
function termRows(): number {
  const r = process.stdout?.rows;
  return typeof r === 'number' && r >= 10 ? r : 40;
}

/** Header strip + metrics line + iteration card (from loop-phase ledger rows). */
function headerLines(snap: Snapshot, ropts: RenderOptions): string[] {
  const width = termColumns();
  const status = snap.status;
  if (snap.authFailed) {
    return [
      `${C.bold}${C.red} DevAgent — DAEMON AUTH REJECTED ${C.reset}${C.dim} token invalid (DEVAGENT_DAEMON_TOKEN / daemon-token file)${C.reset}`,
      '',
    ];
  }
  if (!snap.reachable || !status) {
    return [
      `${C.bold}${C.red} DevAgent — DAEMON UNREACHABLE ${C.reset}${C.dim} retrying every ${POLL_MS / 1000}s · start the daemon${C.reset}`,
      '',
    ];
  }
  const panes = rosterPanes(snap);
  const agg = aggregateStatus(status, panes);
  const q = status.queue ?? {};
  // Spinner (Claude Code cue) animates only while work is live.
  const spin = agg === 'RUNNING' ? `${C.green}${SPINNER[ropts.spinnerFrame ?? 0] ?? SPINNER[0]}${C.reset} ` : '';
  const chip = `${statusColor(agg)}● ${agg}${C.reset}`;
  // Embedded-daemon cue in the title bar (cyan = "the TUI started this one
  // for you") — the bar is short, so the marker survives narrow terminals
  // (the meta line gets width-clamped first).
  const barBody = ` DevAgent  ${spin}${chip}` + (ropts.daemonMode === 'embedded' ? `  ${C.cyan}· daemon:embedded${C.reset}` : '');
  // Metrics line (htop meters + pilot sparkline): uptime, runs, queue meter,
  // activity sparkline, environment. One dense dim line under the bar.
  const pending = q.pending ?? 0;
  const claimed = q.claimed ?? 0;
  const done = q.done ?? 0;
  const openTasks = pending + claimed;
  const meter = `${C.dim}[${C.reset}${meterBar(openTasks, openTasks + done, 10, `${C.yellow}█${C.reset}`, `${C.dim}░${C.reset}`)}${C.dim}]${C.reset}`;
  const samples = ropts.metrics?.samples ?? [];
  const spark = samples.length
    ? ` · ${C.dim}activity(${fmtUptime((samples.length * (ropts.metrics?.sampleMs ?? POLL_MS)) / 1000)}) ${C.cyan}${sparkline(samples)}${C.reset}${C.dim} ${samples[samples.length - 1] ?? 0}${C.reset}`
    : '';
  const circuit =
    status.circuit && status.circuit !== 'closed'
      ? ` · ${status.circuit === 'open' ? C.red : C.yellow}circuit:${status.circuit}${C.reset}`
      : '';
  const meta =
    `${C.dim} up ${fmtUptime(status.uptime_s)} · runs ${status.runs?.active ?? 0}a/${status.runs?.failed_recent ?? 0}f · queue ${meter} ${pending}p/${claimed}c/${done}d${C.reset}` +
    spark +
    circuit +
    `${C.dim} · herdr:${status.herdr?.session ?? '-'} · vis:${status.spawn?.visibility ?? 'visible'}${C.reset}`;
  return [
    `${C.bold}${C.inverse}${padTo(barBody, Math.max(width, visibleLen(barBody) + 1))}${C.reset}`,
    meta,
    '',
    ...iterationLines(snap),
  ];
}

/**
 * Current loop progress (human jump-in cue, PR #140): iteration + phase from
 * the newest loop-phase row in the ledger tail; nothing when none yet.
 */
function iterationLines(snap: Snapshot): string[] {
  const phaseRows = snap.history.filter((r) => r.event === 'loop-phase');
  const latest = phaseRows[phaseRows.length - 1] as Record<string, unknown> | undefined;
  if (!latest || typeof latest.phase !== 'string') return [];
  const det = typeof latest.detail === 'string' && latest.detail ? ` — ${latest.detail}` : '';
  return [
    `${C.dim}iteration ${String(latest.loop ?? '?')} · phase: ${C.reset}${C.cyan}${latest.phase}${C.reset}${C.dim}${det}${C.reset}`,
    '',
  ];
}

function helpLines(): string[] {
  return [
    `${C.bold}Keys${C.reset}`,
    '  1 / 2 / 3  switch view: workers / sessions / live log   (s and l toggle back)',
    '  ↑ ↓ / PgUp PgDn  move the selection (workers, sessions) · scroll (log)',
    '  g / G      jump to first / last item (log: oldest / newest)',
    '  f         toggle follow-tail in the log view',
    '  Enter / o  expand the selected worker into a detail panel',
    '  u         upgrade hint (pilot-style self-update recipe)',
    '  r  refresh now',
    '  k  kill the running task via POST /approve (answer __kill__); daemon must advertise kill-via-answer',
    '  y  confirm the pending kill — any other key cancels',
    '  ?  toggle this help',
    '  q or Ctrl+C  quit',
    '',
  ];
}

/** The log view: dense structured tail (Claude Code transcript feel). */
function logViewLines(ropts: RenderOptions, width: number, bodyBudget: number): string[] {
  const log = ropts.log;
  const titleState =
    !log || log.state === 'off'
      ? dim('● tail off')
      : log.state === 'live'
        ? `${C.green}● live${C.reset}`
        : log.state === 'down'
          ? `${C.yellow}● reconnecting…${C.reset}`
          : dim('● connecting…');
  const src = log?.source ? dim(` · run ${truncate(log.source, 8)}`) : '';
  const pos = log && log.scroll > 0 ? dim(`  [${log.scroll} older ↑ · f to follow]`) : dim('  [following tail]');
  const lines: string[] = [
    `${C.bold}▌Live log${C.reset} ${titleState}${dim(` · ${log?.lines.length ?? 0} line(s) buffered`)}${src}${pos}`,
    '',
  ];
  if (!log || !log.lines.length) {
    lines.push(dim('  no events yet — waiting for worker / daemon run-log output'));
    return lines;
  }
  // Chrome inside the body: the title + blank above the rows. The title must
  // never be cut by fitting, so the viewport derives from the body budget the
  // caller computed (rows - page header - footer), not its own guess.
  const viewport = Math.max(3, bodyBudget - 2);
  const start = Math.max(0, log.lines.length - viewport - (log.follow ? 0 : log.scroll));
  const slice = log.lines.slice(start, start + viewport);
  for (const l of slice) lines.push(formatLogLine(l, width));
  return lines;
}

/** Detail panel (Claude Code expand): everything known about one item. */
function detailOverlayLines(item: TuiPane | TuiQueuedTask, width: number): string[] {
  const inner = Math.max(30, width - 4);
  const body: string[] = [];
  if ('paneId' in item) {
    const p = item as TuiPane;
    body.push(
      ` ${chipFor(p.state, p.agentStatus || p.state)}  ${C.dim}role ${p.role || '-'} · engine ${p.worker || '-'}${C.reset}`,
      ` ${C.dim}pane ${p.paneId || '-'} · workspace ${p.workspaceId || '-'}${C.reset}`,
      ` ${C.dim}up ${fmtElapsed(p.startedAt) || '-'} · since ${truncate(p.startedAt || '-', inner - 16)}${C.reset}`,
      ` ${C.dim}cwd ${truncate(p.cwd, inner - 6)}${C.reset}`,
      '',
      ` ${C.cyan}devagent attach ${truncate(p.taskId || p.label || '?', Math.max(8, inner - 18))}${C.reset}`,
      dim(' jump into this worker pane and steer it live'),
    );
    return boxLines(`${C.cyan}▸${C.reset} ${truncate(p.taskId || p.label || '?', inner - 8)}`, body, inner);
  }
  const t = item as TuiQueuedTask;
  body.push(
    ` ${chipFor('queued', 'queued')}  ${C.dim}${t.status || 'pending'}${C.reset}`,
    ` ${C.dim}created ${truncate(t.createdAt || '-', inner - 10)}${C.reset}`,
    '',
    ` ${truncate(t.title || '', inner - 2)}`,
    dim(' waiting for a worker claim'),
  );
  return boxLines(`${C.cyan}▸${C.reset} ${truncate(t.id || '?', inner - 8)}`, body, inner);
}

/** Pilot's `u` recipe (FR-TUI-05): the self-hosted upgrade/rollback hint. */
function upgradeOverlayLines(width: number): string[] {
  const inner = Math.max(34, width - 4);
  const body = [
    ` ${C.bold}devagent v${DEVAGENT_VERSION}${C.reset} ${C.dim}(self-hosted checkout)${C.reset}`,
    '',
    ` ${C.dim}upgrade — clean worktree only:${C.reset}`,
    `   ${C.cyan}git pull --ff-only${C.reset}`,
    `   ${C.cyan}npm ci && npm run build${C.reset}`,
    '',
    ` ${C.dim}rollback:${C.reset}`,
    `   ${C.cyan}git checkout <previous-commit> && npm run build${C.reset}`,
    '',
    ` ${C.dim}the daemon dispatches dist/src/cli.js — rebuild, then restart${C.reset}`,
    ` ${C.dim}devagent tui so new tasks run the fresh build${C.reset}`,
  ];
  return boxLines('Upgrade', body, inner);
}

/** Trim `body` so header+body+footer fit `rows` (htop always fits). */
function fitLines(header: string[], body: string[], footer: string[], rows: number, keep: 'top' | 'bottom'): string[] {
  const budget = rows - header.length - footer.length;
  if (budget >= body.length) return [...header, ...body, ...footer];
  const cut = Math.max(1, budget);
  const trimmed = keep === 'top' ? body.slice(0, cut) : body.slice(body.length - cut);
  return [...header, ...trimmed, ...footer];
}

/** Full frame as lines (interactive diffs these; one-shot joins them). */
export function renderLines(snap: Snapshot, ropts: RenderOptions = {}): string[] {
  const width = termColumns();
  const rows = ropts.rows ?? 100;
  const view: TuiView = ropts.view ?? (ropts.showSessions ? 'sessions' : 'workers');
  const panes = rosterPanes(snap);
  const queued = queueRows(snap);

  const header = headerLines(snap, ropts);
  if (ropts.showHelp) header.push(...helpLines());

  // Footer (htop function-bar cue): contextual keys + transient notes.
  const notes: string[] = [];
  if (ropts.pendingKill) notes.push(`kill ${ropts.pendingKill}: y confirm · other key cancels`);
  if (ropts.note) notes.push(ropts.note);
  const keysHint =
    view === 'log'
      ? `${C.inverse} [1] workers [2] sessions [3] log · ↑↓ scroll · f follow · r refresh [?] help [q] quit ${C.reset}`
      : `${C.inverse} [1] workers [2] sessions [3] log · ↑↓ select · ⏎ detail · k kill · r refresh [?] help [q] quit ${C.reset}`;
  const footer = [
    keysHint + (notes.length ? `  ${C.yellow}${truncate(notes.join(' · '), Math.max(20, width - 62))}${C.reset}` : ''),
  ];

  if (ropts.overlay?.kind === 'upgrade') {
    return fitLines(header, [...upgradeOverlayLines(width), ''], footer, rows, 'top');
  }
  if (ropts.overlay?.kind === 'detail' && ropts.overlay.item) {
    return fitLines(header, [...detailOverlayLines(ropts.overlay.item, width), ''], footer, rows, 'top');
  }

  if (view === 'log') {
    const bodyBudget = rows - header.length - footer.length;
    return fitLines(header, logViewLines(ropts, width, bodyBudget), footer, rows, 'bottom');
  }

  if (view === 'sessions') {
    const body: string[] = [`${C.bold}▌Sessions${C.reset} ${C.dim}herdr panes${C.reset}`, ''];
    if (!panes.length) body.push(dim('  no live sessions'));
    panes.forEach((p, i) => {
      const el = fmtElapsed(p.startedAt);
      const mark = i === ropts.selection ? `${C.cyan}▸${C.reset}` : ' ';
      body.push(
        `${mark} ${C.cyan}${truncate(p.paneId || '-', 18)}${C.reset}  ${C.bold}${truncate(p.taskId || '?', 24)}${C.reset}  ${chipFor(p.state, p.agentStatus || p.state)}${el ? ` ${C.dim}· ${el}${C.reset}` : ''}  ${C.dim}${truncate(p.cwd, Math.max(20, width - 74))}${C.reset}`,
      );
    });
    return fitLines(header, body, footer, rows, 'top');
  }

  // Workers view: cards (2-up) + history tail.
  const half = Math.max(34, Math.floor(width / 2));
  const body: string[] = [`${C.bold}▌Workers${C.reset} ${C.dim}${panes.length} pane(s) · ${queued.length} queued${C.reset}`, ''];
  const cards: CardLines[] = [
    ...panes.map((p, i) => paneCardLines(p, half - 2, i === ropts.selection)),
    ...queued.map((t, i) => queuedCardLines(t, half - 2, panes.length + i === ropts.selection)),
  ];
  if (!cards.length) body.push(dim('  no workers, queue empty'));
  for (let i = 0; i < cards.length; i += 2) {
    const a = cards[i]!;
    const b = cards[i + 1];
    const rws = Math.max(a.length, b?.length ?? 0);
    for (let r = 0; r < rws; r++) {
      body.push(padTo(a[r] ?? '', half) + (b ? (b[r] ?? '') : ''));
    }
    body.push('');
  }
  if (cards.length) body.pop(); // single blank between cards and history

  body.push(`${C.bold}▌History${C.reset} ${C.dim}ledger tail${C.reset}`, '');
  const history = snap.history.slice(-HISTORY_ROWS);
  if (!history.length) {
    body.push(dim('  no ledger rows'));
  } else {
    for (const row of history) {
      // Row shapes vary by producer: loop-result rows carry {event, loop,
      // status, goal}; watchdog-health rows carry {taskId, watchdogFired};
      // audit rows carry {taskId, verdict}. Columns: clock, kind, taskId
      // (loop-result rows show their loop number instead), short verdict,
      // goal prose — taskId must win over prose so every row is identifiable.
      const rec = row as Record<string, unknown>;
      const ev = typeof rec.event === 'string' && rec.event
        ? rec.event
        : typeof rec.status === 'string' && rec.status
          ? rec.status
          : typeof rec.kind === 'string' ? rec.kind : '';
      const task = typeof rec.taskId === 'string' && rec.taskId
        ? rec.taskId
        : typeof rec.loop === 'number' ? `loop:${rec.loop}` : '';
      const statusTxt = typeof rec.status === 'string' && rec.status && rec.status !== ev
        ? rec.status
        : typeof rec.verdict === 'string' && rec.verdict
          ? rec.verdict
          : rec.watchdogFired === true ? 'fired' : rec.watchdogFired === false ? 'pass' : '';
      const goal = typeof rec.goal === 'string' ? rec.goal : '';
      const goalW = Math.min(60, Math.max(30, width - 66));
      const statusCell = statusTxt
        ? `${statusColor(statusTxt)}${truncate(statusTxt, 8)}${C.reset}`
        : '        ';
      body.push(
        `  ${C.dim}${fmtClock(row.ts)}${C.reset}  ${C.cyan}${truncate(ev, 18)}${C.reset}  ${truncate(task, 18)}  ${statusCell}  ${truncate(goal, goalW)}`,
      );
    }
  }
  return fitLines(header, body, footer, rows, 'top');
}

/** Full frame (multi-line, no screen-control codes) for the current snapshot. */
export function renderDashboard(snap: Snapshot, ropts: RenderOptions = {}): string {
  return renderLines(snap, ropts).join('\n');
}

/** Flat item list of the current view — what the selection cursor walks. */
function viewItems(snap: Snapshot, view: TuiView): (TuiPane | TuiQueuedTask)[] {
  if (view === 'log') return [];
  if (view === 'sessions') return rosterPanes(snap);
  return [...rosterPanes(snap), ...queueRows(snap)];
}

/** Kill target: the selection, else a running pane, else any pane, else first queued row. */
function pickKillTarget(snap: Snapshot, view: TuiView, selection: number): string | null {
  const sel = viewItems(snap, view)[selection];
  const id = sel && ('paneId' in sel ? sel.taskId : sel.id);
  if (id) return id;
  const panes = rosterPanes(snap);
  const running = panes.find((p) => p.state === 'running');
  if (running?.taskId) return running.taskId;
  if (panes[0]?.taskId) return panes[0].taskId;
  return queueRows(snap).find((q) => q.status === 'pending')?.id ?? null;
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

/**
 * One resolved daemon session: the effective client options (rewritten when
 * we embedded our own daemon), the display mode, and the teardown no-op'd
 * unless we own the daemon.
 */
interface DaemonSession {
  opts: TuiOptions;
  mode: 'attach' | 'embedded';
  stop(): Promise<void>;
}

const DEFAULT_DAEMON_URL = 'http://127.0.0.1:7788';

/** Unauthenticated liveness probe (the daemon's /healthz). Never throws. */
async function probeDaemon(opts: TuiOptions): Promise<boolean> {
  try {
    const r = await daemonRequest(opts, '/healthz', {}, 900);
    return r.status === 200;
  } catch {
    return false;
  }
}

/**
 * Resolve which daemon the TUI talks to (the glances standalone pattern):
 * an explicit target (url/token/udsPath — tests, remote, socket) or
 * `--attach-only` are pure clients; otherwise probe the default daemon and
 * attach when it answers, else embed one in-process on an ephemeral port —
 * no port conflicts, stopped when the TUI exits. `devagent daemon` remains
 * the way to run a long-lived shared daemon.
 *
 * The embedded daemon gets an explicit in-memory token on purpose: without
 * one, startDaemon persists a fresh token into the shared daemon-token file
 * and every later attach to the long-lived daemon 401s until its next boot
 * (the 2026-09-05 AUTH REJECTED incident). An ephemeral daemon must not
 * mutate shared on-disk auth state.
 */
export async function ensureDaemon(opts: TuiOptions): Promise<DaemonSession> {
  const explicitTarget = Boolean(opts.url || opts.token || opts.udsPath);
  if (explicitTarget || (await probeDaemon(opts)) || opts.attachOnly) {
    return { opts, mode: 'attach', stop: async () => {} };
  }
  const { startDaemon } = await import('../server/daemon.js');
  const { randomBytes } = await import('node:crypto');
  const handle = await startDaemon({
    port: 0,
    repoPath: opts.repoPath ?? process.cwd(),
    token: randomBytes(24).toString('base64url'),
  });
  return {
    opts: { ...opts, url: `http://127.0.0.1:${handle.port}`, token: handle.token },
    mode: 'embedded',
    stop: () => handle.stop(),
  };
}

/** One-shot mode: single snapshot render to stdout, exit 0. Never throws. */
async function runOneShot(session: DaemonSession): Promise<void> {
  const snap = await fetchSnapshot(session.opts);
  const out = process.stdout;
  const flushed = new Promise<void>((resolve) => {
    if (out.write(renderDashboard(snap, { daemonMode: session.mode }) + '\n')) resolve();
    else out.once('drain', resolve);
  });
  await flushed;
}

/** Interactive mode: alternate screen + raw mode; resolves on quit. */
async function runInteractive(opts: TuiOptions, daemonMode: 'attach' | 'embedded'): Promise<void> {
  const out = process.stdout;
  const stdin = process.stdin;
  let quitResolve!: () => void;
  const untilQuit = new Promise<void>((resolve) => {
    quitResolve = resolve;
  });

  let stopped = false;
  let polling = false;
  let timer: ReturnType<typeof setTimeout> | null = null;
  let view: TuiView = 'workers';
  let showHelp = false;
  let selection = 0;
  let overlay: OverlayState | null = null;
  let pendingKill: string | null = null;
  let note = 'connecting…';
  let snap: Snapshot = { ...EMPTY_SNAPSHOT, fetchedAt: Date.now() };
  let pendingInput = ''; // partial escape sequence carried across chunks

  // Live tail (FR-TUI-03): the daemon's SSE /events stream, buffered locally.
  const logLines: LogLine[] = [];
  let logScroll = 0;
  let logFollow = true;
  let sseState: EventsState | 'off' = 'off';
  let lastLogId = -1;
  let logSource: string | undefined;
  let redrawTimer: ReturnType<typeof setTimeout> | null = null;
  const drawSoon = () => {
    if (redrawTimer || stopped) return;
    redrawTimer = setTimeout(() => {
      redrawTimer = null;
      if (!stopped) safeDraw();
    }, 250);
    redrawTimer.unref?.();
  };

  // Activity sparkline samples (pilot's metric card), one per completed poll.
  const samples: number[] = [];
  let spinnerFrame = 0;

  let prevFrame: string[] | null = null;
  let prevWidth = termColumns();

  const draw = () => {
    const width = termColumns();
    const rows = Math.max(12, termRows() - 1); // headroom: never scroll
    if (width !== prevWidth) {
      prevWidth = width;
      prevFrame = null;
      out.write('\x1b[H\x1b[2J'); // a reflow needs one full clear
    }
    const next = renderLines(snap, {
      view,
      showHelp,
      daemonMode,
      selection,
      overlay,
      note,
      pendingKill,
      metrics: { samples, sampleMs: POLL_MS },
      log: {
        lines: logLines,
        scroll: logFollow ? 0 : logScroll,
        follow: logFollow,
        state: sseState,
        source: logSource,
      },
      rows,
      spinnerFrame,
    });
    out.write(`${renderFrame(prevFrame, next, width)}\n`);
    prevFrame = next;
  };

  // A draw must never take the app down (a crash on the alternate screen
  // leaves the operator's terminal broken): surface the failure in the
  // footer's note and keep running.
  const safeDraw = () => {
    try {
      draw();
    } catch (err) {
      note = `internal: ${err instanceof Error ? err.message : String(err)}`.slice(0, 80);
      try {
        draw();
      } catch {
        /* the next poll/tick retries with fresh state */
      }
    }
  };

  const events = subscribeEvents(
    opts,
    (id, data) => {
      if (id <= lastLogId) return; // replayed after reconnect
      lastLogId = id;
      const line = parseLogLine(data);
      if (line.runId) logSource = line.runId;
      logLines.push(line);
      if (logLines.length > LOG_CAP) logLines.splice(0, logLines.length - LOG_CAP);
      if (logFollow) logScroll = 0;
      drawSoon();
    },
    (st) => {
      sseState = st;
      drawSoon();
    },
  );

  // Spinner ticker: animate only while work is live — the incremental
  // renderer makes a 1-line header rewrite per tick effectively free.
  const ticker: ReturnType<typeof setInterval> = setInterval(() => {
    if (stopped) return;
    const agg = aggregateStatus(snap.status, rosterPanes(snap));
    if (agg !== 'RUNNING' && !pendingKill) return;
    spinnerFrame = (spinnerFrame + 1) % SPINNER.length;
    safeDraw();
  }, TICKER_MS);
  ticker.unref?.();

  const quit = () => {
    if (stopped) return;
    stopped = true;
    if (timer) clearTimeout(timer);
    if (redrawTimer) clearTimeout(redrawTimer);
    events.stop();
    clearInterval(ticker);
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

  const clampSelection = () => {
    const n = viewItems(snap, view).length;
    if (selection >= n) selection = Math.max(0, n - 1);
  };

  const handleKey = (key: Key) => {
    if (pendingKill) {
      if (key.kind === 'char' && (key.ch === 'y' || key.ch === 'Y')) {
        const target = pendingKill;
        pendingKill = null;
        note = `killing ${target}…`;
        void executeKill(opts, snap, target)
          .then((msg) => {
            note = msg;
            if (!stopped) safeDraw();
          })
          .catch((err: unknown) => {
            note = `kill failed: ${err instanceof Error ? err.message : String(err)}`.slice(0, 80);
            if (!stopped) safeDraw();
          });
      } else {
        pendingKill = null;
        note = 'kill cancelled';
      }
      draw();
      return;
    }
    if (key.kind === 'ctrl' && key.ch === '\x03') {
      quit();
      return;
    }
    if (key.kind === 'esc') {
      if (overlay) overlay = null;
      else if (showHelp) showHelp = false;
      else if (view === 'log' && (logScroll > 0 || !logFollow)) {
        logScroll = 0;
        logFollow = true;
      }
      draw();
      return;
    }
    // Modal overlays swallow every key except Ctrl+C (handled above).
    if (overlay) {
      overlay = null;
      draw();
      return;
    }
    const ch = key.kind === 'char' ? key.ch : '';
    switch (ch) {
      case '1':
        view = 'workers';
        clampSelection();
        break;
      case '2':
      case 's':
        view = view === 'sessions' ? 'workers' : 'sessions';
        clampSelection();
        break;
      case '3':
      case 'l':
        view = view === 'log' ? 'workers' : 'log';
        break;
      case 'r':
        void poll();
        return;
      case 'k':
        beginKill();
        break;
      case 'u':
        overlay = { kind: 'upgrade' };
        break;
      case 'f':
        if (view === 'log') {
          logFollow = !logFollow;
          if (logFollow) logScroll = 0;
        }
        break;
      case '?':
        showHelp = !showHelp;
        break;
      case 'q':
        quit();
        return;
      default:
        break;
    }
    if (key.kind === 'enter' || ch === 'o' || ch === 'O') {
      const item = viewItems(snap, view)[selection];
      if (item) overlay = { kind: 'detail', item };
      else if (view !== 'log') note = 'nothing selected';
      draw();
      return;
    }
    // Navigation: arrows move the selection (lists) or scroll (log); g/G
    // home/end, PgUp/PgDn page. ('k' stays kill per FR-TUI-05, so lists use
    // arrows — the htop default — instead of vi keys.)
    const down = key.kind === 'down';
    const up = key.kind === 'up';
    if (down || up) {
      if (view === 'log') {
        const max = Math.max(0, logLines.length - 1);
        logScroll = Math.min(max, Math.max(0, logScroll + (up ? 1 : -1)));
        logFollow = logScroll === 0;
      } else {
        const n = viewItems(snap, view).length;
        selection = Math.min(Math.max(0, n - 1), Math.max(0, selection + (down ? 1 : -1)));
      }
      draw();
      return;
    }
    if (key.kind === 'pgup' || key.kind === 'pgdn') {
      if (view === 'log') {
        const max = Math.max(0, logLines.length - 1);
        logScroll = Math.min(max, Math.max(0, logScroll + (key.kind === 'pgup' ? 10 : -10)));
        logFollow = logScroll === 0;
      } else {
        selection = key.kind === 'pgup' ? 0 : Math.max(0, viewItems(snap, view).length - 1);
      }
      draw();
      return;
    }
    if (key.kind === 'home' || ch === 'g') {
      if (view === 'log') {
        logScroll = Math.max(0, logLines.length - 1);
        logFollow = logScroll === 0;
      } else {
        selection = 0;
      }
      draw();
      return;
    }
    if (key.kind === 'end' || ch === 'G') {
      if (view === 'log') {
        logScroll = 0;
        logFollow = true;
      } else {
        selection = Math.max(0, viewItems(snap, view).length - 1);
      }
      draw();
      return;
    }
    draw();
  };

  const onData = (buf: Buffer) => {
    // DEVAGENT_TUI_DEBUG=chunks surfaces raw input in the footer (pty bring-up).
    if (process.env.DEVAGENT_TUI_DEBUG) note = `in:${JSON.stringify(buf.toString('utf8')).slice(0, 40)}`;
    try {
      const { keys, pending } = decodeKeys(pendingInput + buf.toString('utf8'));
      pendingInput = pending;
      for (const key of keys) handleKey(key);
    } catch (err) {
      // A throw here is otherwise fatal in raw mode (nothing upstream can
      // catch it) — degrade to a footer note instead of a broken terminal.
      note = `internal: ${err instanceof Error ? err.message : String(err)}`.slice(0, 80);
      safeDraw();
    }
  };

  const beginKill = () => {
    const caps = snap.status?.capabilities ?? [];
    if (!caps.includes('kill-via-answer')) {
      note = 'kill: not supported by this daemon';
      return;
    }
    const target = pickKillTarget(snap, view, selection);
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
      clampSelection();
      // Sparkline sample: the truthier of running panes and run-registry locks.
      const runningPanes = rosterPanes(next).filter((p) => p.state === 'running').length;
      const active = Math.max(runningPanes, next.status?.runs?.active ?? 0);
      samples.push(active);
      if (samples.length > SPARK_SAMPLES) samples.shift();
      if (!stopped) safeDraw();
    } catch (err) {
      // Never let a poll failure become an unhandled rejection (it would kill
      // the process): show it, and keep the poll loop alive below.
      note = `internal: ${err instanceof Error ? err.message : String(err)}`.slice(0, 80);
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
 * Run the dashboard (FR-TUI-01): resolve the daemon first (attach to a
 * running one, or embed an ephemeral one for this session — see
 * ensureDaemon). Non-TTY stdin degrades to a one-shot snapshot + exit 0; a
 * TTY gets the full-screen app. Never throws into the caller — the alternate
 * screen and cursor are restored, and an embedded daemon is stopped, on
 * every exit path.
 */
export async function runTui(opts: TuiOptions = {}): Promise<void> {
  const out = process.stdout;
  let entered = false;
  let session: DaemonSession | null = null;
  try {
    session = await ensureDaemon(opts);
    if (!process.stdin.isTTY) {
      await runOneShot(session);
      return;
    }
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
      await runInteractive(session.opts, session.mode);
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
    // Single teardown owner: an embedded daemon dies with the TUI; attached
    // sessions are no-ops. A failed stop must not mask the exit path.
    try {
      await session?.stop();
    } catch {
      /* daemon already gone */
    }
  }
}
