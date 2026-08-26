import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { listTasks, taskCount, type QueuedTask } from './queue.js';
import { readHeartbeat } from './scout.js';
import { spawnCli } from './workers/spawn-utils.js';

/**
 * Progress-tracker role (self-build factory): one long-lived agent that
 * observes the other two (scout writes the queue, builder consumes it) and
 * publishes a durable progress snapshot. Read-only over the repo; writes only
 * .devagent/tracker.heartbeat.json + .selfbuild/progress.{md,json}.
 */

export interface TrackerOptions {
  repoPath: string;
  /** Max entries per section in the markdown snapshot */
  limit?: number;
}

export interface TrackerSnapshot {
  generatedAt: string;
  queue: ReturnType<typeof taskCount>;
  recentTasks: Array<Pick<QueuedTask, 'id' | 'title' | 'status' | 'updatedAt' | 'lastError'>>;
  scout: { alive: boolean; lastRunAt?: string; lastTaskId?: string; lastStatus?: string; worker?: string } | null;
  board: { goal: string; counts: Record<string, number>; blockedReason?: string } | null;
  ledgerTail: string[];
  recentCommits: string[];
  openPrs: string[];
}

export type CliRunner = (
  cmd: string,
  args: string[],
  opts: { cwd: string; timeoutMs: number },
) => Promise<{ exitCode: number; stdout: string; stderr: string; timedOut: boolean }>;

const defaultRunner: CliRunner = (cmd, args, opts) => spawnCli(cmd, args, opts);

function readLedgerTail(repoPath: string, n: number): string[] {
  const p = join(repoPath, '.selfbuild', 'ledger.jsonl');
  if (!existsSync(p)) return [];
  try {
    return readFileSync(p, 'utf8').trim().split('\n').filter(Boolean).slice(-n);
  } catch {
    return [];
  }
}

function readBoardSnapshot(repoPath: string): TrackerSnapshot['board'] {
  try {
    const file = join(repoPath, '.devagent-project.json');
    if (!existsSync(file)) return null;
    const boardMod = JSON.parse(readFileSync(file, 'utf8')) as
      | { goal?: string; tasks?: Array<{ status: string; failureDetail?: string; audit?: { summary?: string } }> }
      | null;
    if (!boardMod?.tasks || typeof boardMod.goal !== 'string') return null;
    const counts: Record<string, number> = {};
    for (const t of boardMod.tasks) counts[t.status] = (counts[t.status] ?? 0) + 1;
    const blocked = boardMod.tasks.find((t) => t.status === 'blocked' || t.status === 'ask') as
      | { failureDetail?: string; audit?: { summary?: string } }
      | undefined;
    return { goal: boardMod.goal, counts, blockedReason: blocked?.failureDetail ?? blocked?.audit?.summary };
  } catch { return null; }
}

async function recentCommits(repoPath: string, n: number, runner: CliRunner): Promise<string[]> {
  try {
    const r = await runner('git', ['log', '--oneline', `-${n}`], { cwd: repoPath, timeoutMs: 10_000 });
    if (r.timedOut || r.exitCode !== 0) return [];
    return r.stdout.trim().split('\n').filter(Boolean);
  } catch {
    return [];
  }
}

/** Open PRs via gh; best-effort — empty list when gh missing / no remote. */
async function openPrs(repoPath: string, n: number, runner: CliRunner): Promise<string[]> {
  try {
    const r = await runner('gh', ['pr', 'list', '--limit', String(n), '--json', 'number,title,url,state'], {
      cwd: repoPath,
      timeoutMs: 15_000,
    });
    if (r.timedOut || r.exitCode !== 0) return [];
    const start = r.stdout.indexOf('[');
    if (start < 0) return [];
    const parsed = JSON.parse(r.stdout.slice(start)) as Array<{ number: number; title: string; url: string; state: string }>;
    return parsed.map((p) => `#${p.number} [${p.state}] ${p.title} — ${p.url}`);
  } catch {
    return [];
  }
}

export function renderProgressMarkdown(snap: TrackerSnapshot): string {
  const lines: string[] = [];
  lines.push(`# DevAgent Self-Build Progress`);
  lines.push('');
  lines.push(`Generated: ${snap.generatedAt}`);
  lines.push('');
  lines.push(`## Queue`);
  lines.push(`total ${snap.queue.total} — pending:${snap.queue.pending} claimed:${snap.queue.claimed} done:${snap.queue.done} failed:${snap.queue.failed}`);
  for (const t of snap.recentTasks) {
    lines.push(`- [${t.status}] ${t.id}: ${t.title}${t.lastError ? ` — ${t.lastError.slice(0, 80)}` : ''}`);
  }
  lines.push('');
  if (snap.board) {
    lines.push(`## Orchestrator board`);
    const cs = snap.board.counts;
    lines.push(`goal: ${snap.board.goal.slice(0, 120)}`);
    lines.push(`tasks: ${Object.entries(cs).map(([k, v]) => `${k}:${v}`).join(' ')}`);
    if (snap.board.blockedReason) lines.push(`blocked: ${snap.board.blockedReason.slice(0, 200)}`);
    lines.push('');
  }
  lines.push(`## Scout (PRD writer)`);
  if (snap.scout) {
    const state = snap.scout.alive ? 'alive' : 'stale (>6h)';
    lines.push(`${state}: worker=${snap.scout.worker} lastRunAt=${snap.scout.lastRunAt} lastTask=${snap.scout.lastTaskId ?? 'none'} (${snap.scout.lastStatus})`);
  } else {
    lines.push(`no heartbeat yet`);
  }
  lines.push('');
  lines.push(`## Builder ledger tail`);
  lines.push(...(snap.ledgerTail.length ? snap.ledgerTail.map((l) => `- ${l}`) : ['- (empty)']));
  lines.push('');
  lines.push(`## Recent commits`);
  lines.push(...(snap.recentCommits.length ? snap.recentCommits.map((c) => `- ${c}`) : ['- (none)']));
  lines.push('');
  lines.push(`## Open PRs`);
  lines.push(...(snap.openPrs.length ? snap.openPrs.map((p) => `- ${p}`) : ['- (none or gh unavailable)']));
  lines.push('');
  return lines.join('\n');
}

export function collectProgress(opts: TrackerOptions, runner: CliRunner = defaultRunner): TrackerSnapshot {
  const repoPath = opts.repoPath;
  const limit = opts.limit ?? 10;

  const tasks = listTasks(repoPath).slice(-limit).reverse();
  const hb = readHeartbeat(repoPath);
  const scoutAlive = hb ? Date.now() - Date.parse(hb.lastRunAt) < 6 * 3_600_000 : false;

  return {
    generatedAt: new Date().toISOString(),
    queue: taskCount(repoPath),
    recentTasks: tasks.map((t) => ({ id: t.id, title: t.title, status: t.status, updatedAt: t.updatedAt, lastError: t.lastError })),
    scout: hb
      ? {
          alive: scoutAlive,
          lastRunAt: hb.lastRunAt,
          lastTaskId: hb.lastTaskId,
          lastStatus: hb.lastStatus,
          worker: hb.worker,
        }
      : null,
    board: readBoardSnapshot(repoPath),
    ledgerTail: readLedgerTail(repoPath, limit),
    recentCommits: [], // filled by collectProgressAsync below when runner available
    openPrs: [],
  };
}

/** Async variant that also gathers git + gh evidence through the injectable runner. */
export async function collectProgressAsync(
  opts: TrackerOptions,
  runner: CliRunner = defaultRunner,
): Promise<TrackerSnapshot> {
  const base = collectProgress(opts, runner);
  const limit = opts.limit ?? 10;
  const [commits, prs] = await Promise.all([
    recentCommits(opts.repoPath, limit, runner),
    openPrs(opts.repoPath, limit, runner),
  ]);
  return { ...base, recentCommits: commits, openPrs: prs };
}

function heartbeatPath(repoPath: string): string {
  return join(repoPath, '.devagent', 'tracker.heartbeat.json');
}

export interface TrackerHeartbeat {
  lastRunAt: string;
  lastStatus: 'ok' | 'failed';
  lastDetail: string;
}

export function readTrackerHeartbeat(repoPath: string): TrackerHeartbeat | null {
  const p = heartbeatPath(repoPath);
  if (!existsSync(p)) return null;
  try {
    return JSON.parse(readFileSync(p, 'utf8')) as TrackerHeartbeat;
  } catch {
    return null;
  }
}

function writeTrackerHeartbeat(repoPath: string, hb: TrackerHeartbeat): void {
  mkdirSync(join(repoPath, '.devagent'), { recursive: true });
  writeFileSync(heartbeatPath(repoPath), JSON.stringify(hb, null, 2) + '\n');
}

export interface TrackOnceResult {
  ok: boolean;
  detail: string;
  snapshot?: TrackerSnapshot;
  progressMdPath?: string;
  progressJsonPath?: string;
  heartbeatPath: string;
}

/** One tracker cycle: gather -> write .selfbuild/progress.{md,json} + heartbeat. */
export async function trackOnce(opts: TrackerOptions, runner: CliRunner = defaultRunner): Promise<TrackOnceResult> {
  const repoPath = opts.repoPath;
  const hbPath = heartbeatPath(repoPath);
  try {
    const snap = await collectProgressAsync({ repoPath, limit: opts.limit }, runner);
    mkdirSync(join(repoPath, '.selfbuild'), { recursive: true });
    const mdPath = join(repoPath, '.selfbuild', 'progress.md');
    const jsonPath = join(repoPath, '.selfbuild', 'progress.json');
    writeFileSync(mdPath, renderProgressMarkdown(snap));
    writeFileSync(jsonPath, JSON.stringify(snap, null, 2) + '\n');
    const detail = `queue ${snap.queue.total} (${snap.queue.pending} pending, ${snap.queue.failed} failed), scout ${snap.scout?.alive ? 'alive' : 'down'}, ${snap.openPrs.length} open PR(s)`;
    writeTrackerHeartbeat(repoPath, { lastRunAt: new Date().toISOString(), lastStatus: 'ok', lastDetail: detail });
    return { ok: true, detail, snapshot: snap, progressMdPath: mdPath, progressJsonPath: jsonPath, heartbeatPath: hbPath };
  } catch (err) {
    writeTrackerHeartbeat(repoPath, { lastRunAt: new Date().toISOString(), lastStatus: 'failed', lastDetail: (err as Error).message });
    return { ok: false, detail: `track failed: ${(err as Error).message}`, heartbeatPath: hbPath };
  }
}

export async function trackLoop(
  opts: { repoPath: string; intervalMinutes: number; signal?: AbortSignal },
  onCycle?: (r: TrackOnceResult) => void,
): Promise<void> {
  while (!opts.signal?.aborted) {
    const r = await trackOnce({ repoPath: opts.repoPath });
    onCycle?.(r);
    if (opts.signal?.aborted) break;
    await new Promise<void>((resolve) => {
      const t = setTimeout(resolve, opts.intervalMinutes * 60_000);
      opts.signal?.addEventListener('abort', () => {
        clearTimeout(t);
        resolve();
      }, { once: true });
    });
  }
}
