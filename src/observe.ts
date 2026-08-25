import { existsSync, readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
/**
 * Monitoring: render a zero-dependency static HTML status board from the
 * structured JSONL run logs plus any .devagent-project.json project boards.
 * Static file — open it or serve it anywhere. Inline CSS, small vanilla JS,
 * no external assets. Heavy per-run content (full timeline, raw events)
 * stays embedded as JSON and opens lazily in a new tab via Blob URLs.
 */

const MAX_INLINE_EVENTS = 50; // shown in the expandable row
const MAX_EMBED_EVENTS = 500; // embedded for the lazy new-tab detail view

export type CardStatus = 'todo' | 'inprogress' | 'done' | 'failed';

export interface RunEvent {
  ts: string;
  stage: string;
  level: string;
  message: string;
}

export interface RunSummary {
  runId: string;
  file: string;
  startedAt: string | null;
  lastAt: string | null;
  lastStage: string;
  lastLevel: string;
  lastMessage: string;
  eventCount: number;
  ok: boolean;
  /** Optional enrichment from structured `data` payloads when present. */
  title?: string;
  repo?: string;
  durationMs?: number;
  exitCode?: number;
  timedOut?: boolean;
  ticket?: string;
  prUrl?: string;
  /** Newest-last capped timeline for drill-down without opening the JSONL. */
  timeline?: RunEvent[];
}

export interface BoardTask {
  id: string;
  title: string;
  status: string;
  attempts?: number;
  failureDetail?: string;
  evidenceGaps?: string[];
  auditStatus?: string;
}

export interface LoadedBoard {
  path: string;
  goal: string;
  tasks: BoardTask[];
}

/** Kanban column for a run log: triage-first, PR presence means shipped. */
export function deriveRunStatus(s: RunSummary): CardStatus {
  if (!s.ok || s.timedOut) return 'failed';
  if (s.prUrl) return 'done';
  const implemented = s.timeline?.some((e) => e.stage === 'implement' || e.stage === 'validate');
  return implemented ? 'inprogress' : 'todo'; // plan-only runs are queued work
}

/**
 * Human-readable one-liner for a run: structured title when the pipeline
 * recorded one, then ticket reference, then the most informative-looking
 * plain message, so cards never open as bare UUIDs.
 */
export function deriveLabel(s: RunSummary): string {
  if (s.title?.trim()) return s.title.trim();
  if (s.ticket?.trim()) return s.ticket.trim();
  const events = s.timeline ?? [];
  const informative = events.find(
    (e) =>
      !/^run\s[0-9a-f-]+\sstarting$/i.test(e.message) &&
      !/^(task starting|dispatching\s\w+:\s*\w+|\w+\sdone(\s\(audited\))?)$/i.test(e.message),
  );
  if (informative) {
    const m = informative.message.length > 90 ? `${informative.message.slice(0, 87)}...` : informative.message;
    return m;
  }
  if (events.some((e) => /^dispatching\s/i.test(e.message))) return 'Orchestrator dispatch loop';
  return s.runId.slice(0, 8);
}

const BOARD_TO_CARD: Record<string, CardStatus> = {
  pending: 'todo',
  ready: 'todo',
  blocked: 'todo',
  ask: 'todo',
  dispatched: 'inprogress',
  untrusted: 'inprogress',
  done: 'done',
  failed: 'failed',
};

export function mapBoardStatus(status: string): CardStatus {
  return BOARD_TO_CARD[status] ?? 'todo';
}

export function collectRunSummaries(runsDir: string): RunSummary[] {
  if (!existsSync(runsDir)) return [];
  const summaries: RunSummary[] = [];
  for (const f of readdirSync(runsDir).sort()) {
    if (!f.endsWith('.jsonl')) continue;
    const raw = readFileSync(join(runsDir, f), 'utf8').trim();
    if (!raw) continue;
    const lines = raw.split('\n');
    const parsed: Array<Record<string, unknown>> = [];
    for (const line of lines) {
      try {
        parsed.push(JSON.parse(line) as Record<string, unknown>);
      } catch {
        continue;
      }
    }
    const first = parsed[0];
    const last = parsed[parsed.length - 1];
    if (!last || !first) continue;
    const level = String(last.level ?? 'info');
    const s: RunSummary = {
      runId: String(last.runId ?? f.replace(/\.jsonl$/, '')),
      file: f,
      startedAt: String(first.ts ?? '') || null,
      lastAt: String(last.ts ?? '') || null,
      lastStage: String(last.stage ?? ''),
      lastLevel: level,
      lastMessage: String(last.message ?? ''),
      eventCount: lines.length,
      ok: level !== 'error',
    };
    // Enrich from run-level metadata carried in event payloads.
    let prFromMessage: string | undefined;
    for (const e of parsed) {
      const msg = String(e.message ?? '');
      const m = msg.match(/https:\/\/[^\s"']+(?:\/pull\/|\/-\/merge_requests\/)[^\s"']*/);
      if (m && !s.prUrl) s.prUrl = m[0].replace(/[.)]+$/, '');
      const d = (e.data ?? {}) as Record<string, unknown>;
      if (!s.title && typeof d.title === 'string') s.title = d.title;
      if (!s.repo && typeof d.repo === 'string') s.repo = d.repo;
      if (!s.ticket && typeof d.ticket === 'string') s.ticket = d.ticket;
      if (s.durationMs === undefined && typeof d.durationMs === 'number') s.durationMs = d.durationMs;
      if (s.exitCode === undefined && typeof d.exitCode === 'number') s.exitCode = d.exitCode;
      if (d.timedOut === true) s.timedOut = true;
    }
    const events: RunEvent[] = parsed.map((e) => ({
      ts: String(e.ts ?? ''),
      stage: String(e.stage ?? ''),
      level: String(e.level ?? 'info'),
      message: String(e.message ?? ''),
    }));
    s.timeline = events.slice(-MAX_INLINE_EVENTS);
    summaries.push(s);
  }
  return summaries;
}

/** Scan directories for durable .devagent-project.json boards. */
export function loadBoards(dirs: string[]): LoadedBoard[] {
  const boards: LoadedBoard[] = [];
  for (const dir of dirs) {
    if (!dir) continue;
    const file = join(dir, '.devagent-project.json');
    if (!existsSync(file)) continue;
    try {
      const b = JSON.parse(readFileSync(file, 'utf8')) as { goal?: string; tasks?: Array<Record<string, unknown>> };
      if (!Array.isArray(b.tasks)) continue;
      boards.push({
        path: file,
        goal: String(b.goal ?? ''),
        tasks: b.tasks.map((t) => ({
          id: String(t.id ?? ''),
          title: String(t.title ?? t.prompt ?? ''),
          status: String(t.status ?? 'pending'),
          attempts: typeof t.attempts === 'number' ? t.attempts : undefined,
          failureDetail: typeof t.failureDetail === 'string' ? t.failureDetail : undefined,
          evidenceGaps: Array.isArray(t.evidenceGaps) ? t.evidenceGaps.map(String) : undefined,
          auditStatus: t.audit && typeof (t.audit as Record<string, unknown>).status === 'string'
            ? String((t.audit as Record<string, unknown>).status)
            : undefined,
        })),
      });
    } catch {
      continue; // corrupted board: skip, runs view still works
    }
  }
  return boards;
}

function esc(s: string): string {
  return s.replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]!);
}

/** Embed arbitrary data inside a <script type="application/json"> safely. */
function jsonEmbed(v: unknown): string {
  return JSON.stringify(v).replace(/</g, '\\u003c').replace(/>/g, '\\u003e').replace(/&/g, '\\u0026');
}

function fmtDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  const m = Math.floor(ms / 60000);
  const sec = Math.round((ms % 60000) / 1000);
  return m > 0 ? `${m}m${sec}s` : `${sec}s`;
}

function maxDay(summaries: RunSummary[]): string {
  let anchor = '1970-01-01';
  for (const s of summaries) {
    const day = (s.startedAt ?? '').slice(0, 10);
    if (/^\d{4}-\d{2}-\d{2}$/.test(day) && day > anchor) anchor = day;
  }
  return anchor;
}

/** GitHub-style per-day activity squares for the trailing N days. */
function renderHeatmap(summaries: RunSummary[], days = 14): string {
  const byDay = new Map<string, { count: number; errors: number }>();
  for (const s of summaries) {
    const day = (s.startedAt ?? '').slice(0, 10);
    if (!/^\d{4}-\d{2}-\d{2}$/.test(day)) continue;
    const cur = byDay.get(day) ?? { count: 0, errors: 0 };
    cur.count++;
    if (!s.ok) cur.errors++;
    byDay.set(day, cur);
  }
  const cells: string[] = [];
  const end = new Date(`${maxDay(summaries)}T00:00:00Z`).getTime();
  for (let i = days - 1; i >= 0; i--) {
    const d = new Date(end - i * 86400000);
    const key = d.toISOString().slice(0, 10);
    const v = byDay.get(key);
    const lvl = !v ? 0 : v.count < 5 ? 1 : v.count < 20 ? 2 : v.count < 60 ? 3 : 4;
    const cls = v?.errors ? 'hm err' : `hm l${lvl}`;
    const label = v ? `${key}: ${v.count} run(s)${v.errors ? `, ${v.errors} failed` : ''}` : `${key}: no runs`;
    cells.push(`<span class="${cls}" title="${esc(label)}"></span>`);
  }
  return `<div class="heatmap">${cells.join('')}<span class="hmlabel">${esc(maxDay(summaries))}, trailing ${days}d</span></div>`;
}

function statTile(label: string, value: string, cls = ''): string {
  return `<div class="tile ${cls}"><div class="tv">${esc(value)}</div><div class="tl">${esc(label)}</div></div>`;
}

function renderStats(summaries: RunSummary[]): string {
  const failed = summaries.filter((s) => !s.ok).length;
  const today = new Date(`${maxDay(summaries)}T00:00:00Z`).getTime();
  const runsToday = summaries.filter((s) => {
    const t = Date.parse(s.startedAt ?? '');
    return !Number.isNaN(t) && t >= today;
  }).length;
  const durations = summaries.map((s) => s.durationMs).filter((d): d is number => typeof d === 'number').sort((a, b) => a - b);
  const med = durations.length ? durations[Math.floor(durations.length / 2)]! : null;
  return [
    statTile('runs', String(summaries.length)),
    statTile('ok', String(summaries.length - failed), 'good'),
    statTile('failed', String(failed), failed ? 'bad' : ''),
    statTile(`runs since ${maxDay(summaries)}`, String(runsToday), 'good'),
    statTile('median duration', med !== null ? fmtDuration(med) : '-'),
  ].join('');
}

const STATUS_LABEL: Record<CardStatus, string> = {
  todo: 'todo',
  inprogress: 'in progress',
  done: 'done',
  failed: 'failed',
};

function statusPill(st: CardStatus): string {
  return `<span class="st st-${st}">${STATUS_LABEL[st]}</span>`;
}

function metaBits(s: RunSummary): string {
  const bits: string[] = [];
  if (typeof s.exitCode === 'number') bits.push(`exit ${s.exitCode}`);
  if (s.timedOut) bits.push('timed out');
  if (typeof s.durationMs === 'number') bits.push(fmtDuration(s.durationMs));
  if (s.repo) bits.push(s.repo.split('/').slice(-3).join('/'));
  if (s.ticket) bits.push(s.ticket);
  return bits.length ? `<span class="meta">${esc(bits.join(' · '))}</span>` : '';
}

function renderTimeline(timeline: RunEvent[]): string {
  return `<ol class="timeline">${timeline
    .map(
      (e) => `<li class="${e.level === 'error' ? 'tl-err' : ''}">
<span class="ts">${esc(e.ts.replace('T', ' ').replace(/\.\d+Z$/, ''))}</span>
<span class="pill">${esc(e.stage)}</span>
<span>${esc(e.message)}</span></li>`,
    )
    .join('')}</ol>`;
}

let embedSeq = 0;

function runRowHtml(s: RunSummary): string {
  const id8 = esc(s.runId.slice(0, 8));
  const status = deriveRunStatus(s);
  const heading = `${esc(deriveLabel(s))} <small class="rid-id">${id8}</small>`;
  const prLink = s.prUrl ? ` <a href="${esc(s.prUrl)}" target="_blank" rel="noopener">PR ↗</a>` : '';
  const embedId = `ev${embedSeq++}`;
  // Full history opens lazily in a new tab so the main page stays light to scroll.
  const openBtn = `<button class="openbtn" onclick="openDetail('${embedId}','${esc(s.runId)}')">open</button>`;
  return `<details class="run ${status}" data-status="${status}" data-search="${esc(
    `${s.runId} ${s.lastMessage} ${s.title ?? ''} ${s.repo ?? ''} ${s.lastStage} ${s.ticket ?? ''}`,
  ).toLowerCase()}">
<summary>
<span class="rid">${heading}</span>${statusPill(status)}
<span class="msg">${esc(s.lastMessage)}${prLink}</span>
${metaBits(s)}
<span class="when">${esc((s.startedAt ?? '').replace('T', ' ').replace(/\.\d+Z$/, ''))}</span>
${openBtn}
</summary>
${s.timeline ? renderTimeline(s.timeline) : ''}
<script type="application/json" id="${embedId}">${jsonEmbed({ run: s, events: s.timeline })}</script>
</details>`;
}

function renderBoardTab(boards: LoadedBoard[], summaries: RunSummary[]): string {
  const cols: CardStatus[] = ['todo', 'inprogress', 'done', 'failed'];
  const cards = new Map<CardStatus, string[]>([['todo', []], ['inprogress', []], ['done', []], ['failed', []]]);
  for (const b of boards) {
    for (const t of b.tasks) {
      const badges: string[] = [];
      if (t.status === 'ask') badges.push('<span class="badge ask">needs input</span>');
      if (t.status === 'blocked') badges.push('<span class="badge blocked">blocked</span>');
      if (t.status === 'untrusted') badges.push('<span class="badge">unaudited</span>');
      if (typeof t.attempts === 'number' && t.attempts > 1) badges.push(`<span class="badge">try ${t.attempts}</span>`);
      if (t.evidenceGaps?.length) badges.push(`<span class="badge gap">${t.evidenceGaps.length} gap(s)</span>`);
      cards.get(mapBoardStatus(t.status))!.push(
        `<div class="card"><div class="cid">${esc(t.id)}</div><div class="ctitle">${esc(t.title.slice(0, 140))}</div>${badges.join('')}</div>`,
      );
    }
  }
  for (const s of summaries) cards.get(deriveRunStatus(s))!.push(runCardHtml(s));
  const colHtml = cols
    .map((c) => {
      const list = cards.get(c)!;
      return `<div class="col"><h3>${STATUS_LABEL[c]} <small>${list.length}</small></h3>${list.join('') || '<p class="empty">—</p>'}</div>`;
    })
    .join('');
  const note = boards.length
    ? ''
    : '<p class="note">No .devagent-project.json boards found — board shows run-derived state only. Pass board dirs via DEVAGENT_BOARD_DIRS.</p>';
  return `${note}<div class="boardcols">${colHtml}</div>`;
}

function runCardHtml(s: RunSummary): string {
  const label = esc(deriveLabel(s).slice(0, 100));
  const prLink = s.prUrl ? ` <a href="${esc(s.prUrl)}" target="_blank" rel="noopener">PR ↗</a>` : '';
  const fail = !s.ok ? `<div class="cfail">${esc(s.lastMessage.slice(0, 120))}</div>` : '';
  return `<div class="card runcard" title="${esc(s.runId)}"><div class="cid">${label}${prLink}</div>${fail}<div class="cmeta">${esc((s.startedAt ?? '').slice(0, 16).replace('T', ' '))} · ${esc(s.lastStage)}</div></div>`;
}

function renderRunsTab(summaries: RunSummary[]): string {
  // Group by calendar day of startedAt, newest first.
  const byDay = new Map<string, RunSummary[]>();
  for (const s of summaries) {
    const day = (s.startedAt ?? '').slice(0, 10) || 'unknown';
    const list = byDay.get(day) ?? [];
    list.push(s);
    byDay.set(day, list);
  }
  const days = [...byDay.keys()].sort().reverse();
  return days
    .map((day) => {
      const list = [...byDay.get(day)!].reverse(); // newest first within the day
      const failed = list.filter((s) => !s.ok).length;
      return `<h2 class="dayhdr">${esc(day)} <small>${list.length} run(s)${failed ? `, ${failed} failed` : ''}</small></h2>
<div class="list">${list.map(runRowHtml).join('')}</div>`;
    })
    .join('');
}

function renderFeaturesTab(summaries: RunSummary[]): string {
  // A feature = distinct title or ticket; runs without either collapse onto their repo+day.
  const groups = new Map<string, RunSummary[]>();
  for (const s of summaries) {
    const key = (s.title ?? s.ticket ?? '').trim() || `${s.repo ?? 'no-repo'} ${(s.startedAt ?? '').slice(0, 10)}`;
    const list = groups.get(key) ?? [];
    list.push(s);
    groups.set(key, list);
  }
  const lastStarted = (runs: RunSummary[]): string => runs[runs.length - 1]?.startedAt ?? '';
  const entries = [...groups.entries()].sort((a, b) => lastStarted(b[1]).localeCompare(lastStarted(a[1])));
  return entries
    .map(([key, runs]) => {
      const counts = new Map<CardStatus, number>();
      for (const r of runs) counts.set(deriveRunStatus(r), (counts.get(deriveRunStatus(r)) ?? 0) + 1);
      const pills = [...counts.entries()].map(([st, n]) => `${statusPill(st)} <small>${n}</small>`).join(' ');
      const prLink = runs.find((r) => r.prUrl)?.prUrl;
      return `<details class="feature">
<summary><span class="rid">${esc(key)}</span>${pills}${prLink ? ` <a href="${esc(prLink)}" target="_blank" rel="noopener">PR ↗</a>` : ''}<span class="meta">${runs.length} run(s)</span></summary>
<div class="list">${[...runs].reverse().map(runRowHtml).join('')}</div>
</details>`;
    })
    .join('') || '<p class="empty">No features yet.</p>';
}

export function renderDashboard(summaries: RunSummary[], title = 'DevAgent Runs', boards: LoadedBoard[] = []): string {
  // Newest-first display everywhere; callers may hand us either order.
  const display = [...summaries].sort((a, b) => (b.startedAt ?? '').localeCompare(a.startedAt ?? ''));
  return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>${esc(title)}</title>
<style>
body{font-family:ui-sans-serif,system-ui,sans-serif;margin:1.5rem;background:#111;color:#eee}
h1 small,h2 small,h3 small{color:#888;font-weight:normal}
small{color:#888}
.tabs{display:flex;gap:.3rem;margin:1rem 0;border-bottom:1px solid #2a2a2a}
.tabbtn{background:none;border:none;color:#999;padding:.5rem .9rem;font-size:.9rem;cursor:pointer;border-bottom:2px solid transparent}
.tabbtn.on{color:#eee;border-bottom-color:#46a758}
.tabpane{display:none}.tabpane.on{display:block}
.tiles{display:flex;gap:.75rem;margin:1rem 0;flex-wrap:wrap}
.tile{background:#1a1a1a;border:1px solid #2a2a2a;border-radius:8px;padding:.6rem 1rem;min-width:7rem}
.tv{font-size:1.4rem;font-weight:600}.tl{font-size:.7rem;color:#999;text-transform:uppercase;letter-spacing:.05em}
.tile.good .tv{color:#46a758}.tile.bad .tv{color:#e5484d}
.heatmap{display:flex;align-items:center;gap:3px;margin:.5rem 0 1.5rem;flex-wrap:wrap}
.hm{width:14px;height:14px;border-radius:3px;background:#222;display:inline-block}
.hm.l1{background:#1b3a2a}.hm.l2{background:#256a43}.hm.l3{background:#37a06a}.hm.l4{background:#46a758}
.hm.err{background:#e5484d}
.hmlabel{font-size:.7rem;color:#888;margin-left:.5rem}
.boardcols{display:grid;grid-template-columns:repeat(4,minmax(15rem,1fr));gap:.75rem;align-items:start}
.col{background:#151515;border:1px solid #262626;border-radius:8px;padding:.6rem}
.col h3{margin:.2rem .2rem .6rem;font-size:.8rem;text-transform:uppercase;color:#aaa}
.card{background:#1d1d1d;border:1px solid #303030;border-radius:6px;padding:.5rem .6rem;margin-bottom:.4rem;font-size:.78rem}
.card .cid{font-family:ui-monospace,monospace;color:#9ecbff;font-size:.75rem;margin-bottom:.2rem}
.card .ctitle{color:#ddd}
.card .cfail{color:#e5484d;margin-top:.2rem}
.card .cmeta{color:#666;font-size:.68rem;margin-top:.3rem}
.st{padding:.05rem .45rem;border-radius:99px;font-size:.65rem;text-transform:uppercase}
.st-todo{background:#2a2a2a;color:#bbb}.st-inprogress{background:#14406b;color:#7cc0ff}
.st-done{background:#173b26;color:#46a758}.st-failed{background:#47191c;color:#ff7b81}
.badge{display:inline-block;background:#2a2a2a;color:#ccc;border-radius:4px;padding:.05rem .35rem;font-size:.62rem;margin-right:.25rem}
.badge.ask{background:#4a3a14;color:#ffd166}.badge.blocked{background:#47191c;color:#ff7b81}.badge.gap{background:#3a2a14;color:#ffb066}
.dayhdr{border-bottom:1px solid #262626;padding-bottom:.3rem}
input#f{background:#1a1a1a;border:1px solid #333;color:#eee;border-radius:6px;padding:.4rem .6rem;width:18rem;font-size:.85rem}
button.fbtn,.openbtn{background:#1a1a1a;border:1px solid #333;color:#ccc;border-radius:6px;padding:.3rem .7rem;font-size:.72rem;cursor:pointer;margin-left:.3rem}
button.fbtn.on{border-color:#46a758;color:#46a758}
.openbtn:hover{border-color:#7cc0ff;color:#7cc0ff}
.list{margin:.5rem 0 1.5rem}
details.run,details.feature{background:#161616;border:1px solid #262626;border-radius:6px;margin-bottom:.35rem}
details.run.err,details.feature:has(details.run.err){border-left:3px solid #e5484d}
details.run.done{border-left:3px solid #256a43}
details.run.todo,details.run.inprogress{border-left:3px solid #444}
summary{display:flex;align-items:center;gap:.6rem;padding:.45rem .7rem;cursor:pointer;flex-wrap:wrap}
details.feature>summary{font-weight:600}
summary:hover{background:#1c1c1c}
.rid{font-family:ui-monospace,monospace;font-size:.8rem;color:#9ecbff;min-width:14rem}
.rid-id{color:#666;font-size:.68rem;font-weight:normal;font-family:ui-monospace,monospace}
details.run .rid{min-width:auto;max-width:28rem}
.msg{font-size:.8rem;color:#ccc;flex:1;min-width:12rem}
.meta,.when{font-size:.7rem;color:#777}
.pill{background:#2a2a2a;color:#bbb;padding:.1rem .5rem;border-radius:99px;font-size:.7rem;text-transform:uppercase}
.timeline{list-style:none;margin:.4rem .8rem .8rem;padding:.4rem 0 0;border-top:1px dashed #2a2a2a}
.timeline li{font-size:.75rem;color:#aaa;padding:.15rem 0;display:flex;gap:.6rem;flex-wrap:wrap}
.timeline li.tl-err{color:#e5484d}
.ts{font-family:ui-monospace,monospace;color:#666}
.empty{color:#666;font-style:italic;padding:.5rem 0}
.note{color:#ffb066;font-size:.75rem;background:#241c10;border:1px solid #3a2a14;border-radius:6px;padding:.4rem .7rem}
@media print{body{background:#fff;color:#111}}
</style></head><body>
<h1>${esc(title)} <small>(${summaries.length} runs)</small></h1>
<div class="tabs">
<button class="tabbtn on" id="tb-board" onclick="showTab('board')">Board</button>
<button class="tabbtn" id="tb-runs" onclick="showTab('runs')">Runs by date</button>
<button class="tabbtn" id="tb-features" onclick="showTab('features')">Features</button>
</div>
<div class="tiles">${renderStats(display)}</div>
<h2 style="margin-top:0">Activity</h2>
${renderHeatmap(display)}
<div class="tabpane on" id="tp-board">${renderBoardTab(boards, display)}</div>
<div class="tabpane" id="tp-runs">
<p><input id="f" type="search" placeholder="filter runs..." oninput="applyFilter()">
<button class="fbtn on" id="b-all" onclick="setStatus('all')">all</button>
<button class="fbtn" id="b-ok" onclick="setStatus('ok')">ok</button>
<button class="fbtn" id="b-err" onclick="setStatus('err')">failed</button></p>
<div id="runs">${renderRunsTab(display)}</div>
</div>
<div class="tabpane" id="tp-features">${renderFeaturesTab(display)}</div>
<script>
function showTab(name){
  ['board','runs','features'].forEach(function(k){
    document.getElementById('tb-'+k).classList.toggle('on',k===name);
    document.getElementById('tp-'+k).classList.toggle('on',k===name);
  });
}
var statusFilter='all';
function applyFilter(){
  var q=document.getElementById('f').value.toLowerCase();
  document.querySelectorAll('#tp-runs details.run').forEach(function(el){
    var okStatus=statusFilter==='all'||el.dataset.status===statusFilter;
    var okText=!q||(el.dataset.search||'').indexOf(q)!==-1;
    el.style.display=okStatus&&okText?'':'none';
  });
  document.querySelectorAll('#tp-runs .dayhdr').forEach(function(h){
    var vis=0,nxt=h.nextElementSibling;
    if(nxt){nxt.querySelectorAll('details.run').forEach(function(el){if(el.style.display!=='none')vis++;});}
    h.style.display=vis?'':'none';
  });
}
function setStatus(s){
  statusFilter=s;
  ['all','ok','err'].forEach(function(k){
    document.getElementById('b-'+k).classList.toggle('on',k===s);
  });
  applyFilter();
}
/* Lazy detail view: renders the embedded run JSON into a fresh tab. */
function openDetail(embedId,runId){
  var node=document.getElementById(embedId);
  var payload=node?JSON.parse(node.textContent):null;
  var w=window.open('','_blank');
  if(!w)return;
  var evs=(payload&&payload.events)||[];
  var rows=evs.map(function(e){
    return '<tr class="'+(e.level==='error'?'err':'')+'"><td>'+escH(e.ts)+'</td><td>'+escH(e.stage)+'</td><td>'+escH(e.message)+'</td></tr>';
  }).join('');
  var raw='<pre>'+escH(JSON.stringify(payload&&(payload.run||payload),null,2))+'</pre>';
  w.document.write('<!doctype html><html><head><meta charset="utf-8"><title>'+escH(runId)+' — DevAgent run detail</title>'+
  '<style>body{font-family:ui-sans-serif,system-ui,sans-serif;margin:1.5rem;background:#111;color:#eee}'+
  'table{border-collapse:collapse;width:100%}th,td{text-align:left;padding:.35rem .5rem;border-bottom:1px solid #2a2a2a;font-size:.8rem}'+
  'th{color:#999;text-transform:uppercase;font-size:.65rem}tr.err td{color:#ff7b81}'+
  '.pill{background:#2a2a2a;padding:.1rem .5rem;border-radius:99px;font-size:.7rem}'+
  'pre{background:#181818;border:1px solid #2a2a2a;border-radius:6px;padding:.8rem;font-size:.72rem;overflow:auto;color:#bbb}'+
  'h1{font-size:1.1rem}a{color:#7cc0ff}</style></head><body>'+
  '<h1>'+escH(runId)+'</h1><p><a href="#" onclick="window.close()">close tab</a></p>'+
  '<h2>Timeline ('+evs.length+' events)</h2><table><thead><tr><th>time</th><th>stage</th><th>message</th></tr></thead><tbody>'+(rows||'<tr><td colspan=3>none</td></tr>')+'</tbody></table>'+
  '<h2>Raw summary</h2>'+raw+
  '</body></html>');
  w.document.close();
}
function escH(s){return String(s==null?'':s).replace(/[&<>"]/g,function(c){return{'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c];});}
</script>
</body></html>`;
}

export function writeDashboard(homeDir: string, boardDirs: string[] = []): { path: string; runs: number; boards: number } {
  const summaries = collectRunSummaries(join(homeDir, 'runs'));
  const boards = loadBoards(boardDirs);
  const html = renderDashboard(summaries, 'DevAgent Runs', boards);
  const path = join(homeDir, 'dashboard.html');
  writeFileSync(path, html);
  return { path, runs: summaries.length, boards: boards.length };
}
