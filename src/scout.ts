import { existsSync, mkdirSync, readFileSync, writeFileSync, readdirSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { enqueueTask, ensureQueueDirs, listTasks, prdsDir, queueDir } from './queue.js';
import type { DevAgentConfig } from './config.js';
import { spawnCli } from './workers/spawn-utils.js';

export interface ScoutCycleOptions {
  repoPath: string;
  worker?: 'opencode' | 'claude-code' | 'omp' | 'pi';
  /** Override interval; not used in once mode but persisted to heartbeat */
  intervalMinutes?: number;
  dryRun?: boolean;
  timeoutMs?: number;
}

export interface ScoutCycleResult {
  ok: boolean;
  taskId?: string;
  prdPath?: string;
  queuePath?: string;
  heartbeatPath: string;
  detail: string;
  rawPrompt?: string;
  rawOutput?: string;
}

export interface ScoutHeartbeat {
  lastRunAt: string;
  lastTaskId?: string;
  lastStatus: 'ok' | 'failed' | 'skipped';
  lastDetail: string;
  worker: string;
  intervalMinutes: number;
}

function heartbeatPath(repoPath: string): string {
  return join(repoPath, '.devagent', 'scout.heartbeat.json');
}

function randomTaskId(): string {
  const d = new Date();
  const y = d.getUTCFullYear();
  const m = String(d.getUTCMonth() + 1).padStart(2, '0');
  const day = String(d.getUTCDate()).padStart(2, '0');
  const rand = Math.random().toString(36).slice(2, 6);
  return `SCOUT-${y}${m}${day}-${rand}`;
}

/**
 * Deterministic fallback task id: one per UTC day. The fallback fires when
 * scout LLM output is unparseable/failed — with a random id each cycle the
 * queue accumulated 12 same-goal duplicates in one day (2026-09-01), because
 * enqueueTask's exists-dedup never matched. A per-day id makes the dedup
 * catch repeats; a new day legitimately gets a fresh fallback slot.
 */
function fallbackTaskId(): string {
  const d = new Date();
  const y = d.getUTCFullYear();
  const m = String(d.getUTCMonth() + 1).padStart(2, '0');
  const day = String(d.getUTCDate()).padStart(2, '0');
  return `SCOUT-${y}${m}${day}-fallback`;
}

function sanitizeForFile(s: string): string {
  return s.replace(/[^A-Za-z0-9._-]/g, '-').replace(/-+/g, '-').slice(0, 80);
}

function readTextIfExists(path: string, maxChars = 4000): string {
  try {
    if (!existsSync(path)) return '';
    const raw = readFileSync(path, 'utf8');
    if (raw.length > maxChars) return raw.slice(0, maxChars) + '\n…[truncated]';
    return raw;
  } catch {
    return '';
  }
}

function queueCount(repoPath: string): number {
  try {
    if (!existsSync(queueDir(repoPath))) return 0;
    return readdirSync(queueDir(repoPath)).filter((f) => f.endsWith('.json')).length;
  } catch {
    return 0;
  }
}

function ledgerTail(repoPath: string): string {
  // Prefer .selfbuild/ledger.jsonl (durable), fallback to scout log
  const p = join(repoPath, '.selfbuild', 'ledger.jsonl');
  if (!existsSync(p)) return '(no ledger yet)';
  try {
    const lines = readFileSync(p, 'utf8').trim().split('\n').filter(Boolean);
    return lines.slice(-3).join('\n') || '(ledger empty)';
  } catch {
    return '(ledger unreadable)';
  }
}

export function buildScoutPrompt(repoPath: string, config: DevAgentConfig): string {
  const lessons = readTextIfExists(join(repoPath, '.selfbuild', 'lessons.md'), 2000);
  const tail = ledgerTail(repoPath);
  const qCount = queueCount(repoPath);
  const recentPrds = (() => {
    try {
      if (!existsSync(prdsDir(repoPath))) return '(none)';
      const files = readdirSync(prdsDir(repoPath)).filter((f) => f.endsWith('.md')).slice(-2);
      return files.length ? files.join(', ') : '(none)';
    } catch { return '(none)'; }
  })();

  return [
    `You are the DevAgent SCOUT. Repo: ${repoPath}.`,
    `Read docs/PRD.md section 4 (competitive landscape) and section 17 (roadmap), plus .selfbuild/ledger.jsonl tail and lessons below.`,
    `Queue depth: ${qCount} task(s). Recent PRDs: ${recentPrds}.`,
    lessons ? `Lessons (ratchet, do not re-derive):\n${lessons}` : '',
    `Ledger tail:\n${tail}`,
    '',
    `Select exactly ONE backlog item for a single iteration-sized improvement that is implementable + testable in one devagent task pass.`,
    `Output STRICTLY in this format (no extra prose):`,
    `---TASK---`,
    `id: <short id, e.g. FEAT-123 or IMPROVE-foo>`,
    `title: <max 80 chars>`,
    `goal: Goal: <one sentence goal starting with "Goal:">`,
    `criteria: <semicolon-separated acceptance criteria, or single line>`,
    `---PRD---`,
    `# <title>`,
    `## Goal`,
    `<2-3 sentences>`,
    `## Acceptance criteria`,
    `- <bullet>`,
    `## Notes`,
    `<optional notes>`,
  ].filter(Boolean).join('\n');
}

export function parseScoutOutput(text: string): { id: string; title: string; goal: string; criteria: string[]; prdMarkdown: string } | null {
  const taskIdx = text.indexOf('---TASK---');
  const prdIdx = text.indexOf('---PRD---');
  if (taskIdx < 0 || prdIdx < 0 || prdIdx <= taskIdx) return null;

  const taskBlock = text.slice(taskIdx + '---TASK---'.length, prdIdx).trim();
  const prdMarkdown = text.slice(prdIdx + '---PRD---'.length).trim();
  if (!prdMarkdown) return null;

  const lines = taskBlock.split('\n').map((l) => l.trim()).filter(Boolean);
  const get = (key: string): string | undefined => {
    const prefix = `${key}:`;
    const line = lines.find((l) => l.toLowerCase().startsWith(prefix));
    return line ? line.slice(prefix.length).trim() : undefined;
  };

  const id = get('id');
  const title = get('title');
  const goal = get('goal');
  const criteriaRaw = get('criteria') ?? '';
  if (!id || !title || !goal) return null;
  if (!/^Goal:/i.test(goal)) return null;

  const criteria = criteriaRaw ? criteriaRaw.split(';').map((s) => s.trim()).filter(Boolean) : [];
  return { id: sanitizeForFile(id), title: title.slice(0, 80), goal, criteria, prdMarkdown };
}

function fallbackTask(prompt: string, config: DevAgentConfig): { id: string; title: string; goal: string; criteria: string[]; prdMarkdown: string } {
  // Deterministic fallback when LLM output is unparseable or opencode unavailable
  const id = fallbackTaskId();
  const title = 'Scout fallback: improve devagent observability';
  const goal = 'Goal: Add a scout heartbeat status command so operators can verify the 24/7 scout is alive without reading files.';
  const prd = `# ${title}\n\n## Goal\nExpose \`devagent scout-status --repo <path>\` that prints heartbeat, queue depth, and last task.\n\n## Acceptance criteria\n- scout-status prints JSON or human table\n- heartbeat age is reported\n- missing heartbeat is reported cleanly\n\n## Notes\nFallback PRD generated when scout LLM output was unparseable.\n\nPrompt excerpt (first 400 chars): ${prompt.slice(0, 400)}`;
  return { id, title, goal, criteria: ['scout-status prints heartbeat + queue depth', 'missing heartbeat handled'], prdMarkdown: prd };
}

export async function runScoutOnce(opts: ScoutCycleOptions, config: DevAgentConfig): Promise<ScoutCycleResult> {
  const repoPath = opts.repoPath;
  const worker = opts.worker ?? (config.scout?.worker as 'opencode' | 'claude-code' | 'omp' | 'pi' | undefined) ?? 'omp';
  const intervalMinutes = opts.intervalMinutes ?? config.scout?.intervalMinutes ?? 30;
  const hbPath = heartbeatPath(repoPath);

  mkdirSync(join(repoPath, '.devagent'), { recursive: true });
  ensureQueueDirs(repoPath);

  const prompt = buildScoutPrompt(repoPath, config);

  // Optional queue-depth guard: skip when maxQueued reached
  const maxQueued = config.scout?.maxQueued;
  if (maxQueued !== undefined) {
    const pending = listTasks(repoPath, { status: 'pending' }).length;
    const claimed = listTasks(repoPath, { status: 'claimed' }).length;
    const total = pending + claimed;
    if (total >= maxQueued) {
      const detail = `skipped: queue depth ${total} >= maxQueued ${maxQueued}`;
      writeHeartbeat(repoPath, { lastStatus: 'skipped', lastDetail: detail, worker, intervalMinutes });
      return { ok: true, heartbeatPath: hbPath, detail, rawPrompt: prompt };
    }
  }

  if (opts.dryRun) {
    const parsed = fallbackTask(prompt, config);
    const prdPath = join(prdsDir(repoPath), `${parsed.id}.md`);
    mkdirSync(prdsDir(repoPath), { recursive: true });
    writeFileSync(prdPath, parsed.prdMarkdown);
    try {
      enqueueTask(repoPath, {
        id: parsed.id,
        title: parsed.title,
        goal: parsed.goal,
        acceptanceCriteria: parsed.criteria,
        prdMarkdown: parsed.prdMarkdown,
        source: `scout:${worker}:dry-run`,
      });
    } catch {
      // already exists (rerun) -> treat as success
    }
    writeHeartbeat(repoPath, { lastStatus: 'ok', lastDetail: `dry-run produced ${parsed.id}`, worker, intervalMinutes, lastTaskId: parsed.id });
    return { ok: true, taskId: parsed.id, prdPath, queuePath: join(queueDir(repoPath), `${parsed.id}.json`), heartbeatPath: hbPath, detail: `dry-run: ${parsed.id}`, rawPrompt: prompt, rawOutput: parsed.prdMarkdown };
  }

  // Live: dispatch to the configured worker. Worker-specific argv below must
  // match the matching WorkerAdapter's buildOmpArgs / baseArgs exactly so
  // sessionId/result parsing stays compatible.
  const timeoutMs = opts.timeoutMs ?? 5 * 60_000;
  const cli =
    worker === 'opencode' ? 'opencode' : worker === 'claude-code' ? 'claude' : worker === 'pi' ? 'pi' : 'omp';
  // claude-code only: same guard as the worker adapter's baseArgs — tier
  // aliases like "coding" 403 against the API key ("Combo "coding" is not
  // allowed"), so drop anything that is not a claude family id and let the
  // CLI fall back to ~/.claude/settings.json model.
  const scoutModelRaw = worker === 'claude-code' ? config.model?.trim() : undefined;
  const scoutModel =
    scoutModelRaw && (/^claude-/.test(scoutModelRaw) || /^(opus|sonnet|haiku)(-|$)/.test(scoutModelRaw))
      ? scoutModelRaw.split('#')[0]!
      : undefined;
  // pi only: provider-qualified ids ("provider/model") pass through; driver
  // tier aliases like "coding" are not pi ids and would fail model resolution.
  const scoutPiModelRaw = worker === 'pi' ? config.model?.trim() : undefined;
  const scoutPiModel =
    scoutPiModelRaw && scoutPiModelRaw.includes('/') ? scoutPiModelRaw.split('#')[0]! : undefined;
  const args =
    worker === 'opencode'
      ? ['run', '--format', 'json', prompt]
      : worker === 'omp'
        ? ['-p', prompt, '--mode', 'json']
        : worker === 'pi'
          ? ['--mode', 'json', '-p', prompt, ...(scoutPiModel ? ['--model', scoutPiModel] : [])]
          : ['-p', prompt, '--output-format', 'json', ...(scoutModel ? ['--model', scoutModel] : [])];

  let raw = '';
  try {
    const r = await spawnCli(cli, args, { cwd: repoPath, timeoutMs });
    raw = r.stdout || r.stderr || '';
    if (r.timedOut) {
      const detail = `scout timed out after ${timeoutMs}ms`;
      writeHeartbeat(repoPath, { lastStatus: 'failed', lastDetail: detail, worker, intervalMinutes });
      return { ok: false, heartbeatPath: hbPath, detail, rawPrompt: prompt, rawOutput: raw };
    }
    if (r.exitCode !== 0 && !raw.trim()) {
      const parsed = fallbackTask(prompt, config);
      const prdPath = join(prdsDir(repoPath), `${parsed.id}.md`);
      mkdirSync(prdsDir(repoPath), { recursive: true });
      writeFileSync(prdPath, parsed.prdMarkdown);
      try {
        enqueueTask(repoPath, { id: parsed.id, title: parsed.title, goal: parsed.goal, acceptanceCriteria: parsed.criteria, prdMarkdown: parsed.prdMarkdown, source: `scout:${worker}:fallback` });
      } catch { /* exists */ }
      const detail = `scout ${worker} failed (exit ${r.exitCode}), fallback task ${parsed.id} enqueued`;
      writeHeartbeat(repoPath, { lastStatus: 'ok', lastDetail: detail, worker, intervalMinutes, lastTaskId: parsed.id });
      return { ok: true, taskId: parsed.id, prdPath, queuePath: join(queueDir(repoPath), `${parsed.id}.json`), heartbeatPath: hbPath, detail, rawPrompt: prompt, rawOutput: raw };
    }
  } catch (err) {
    const detail = `scout spawn failed: ${(err as Error).message}`;
    writeHeartbeat(repoPath, { lastStatus: 'failed', lastDetail: detail, worker, intervalMinutes });
    return { ok: false, heartbeatPath: hbPath, detail, rawPrompt: prompt, rawOutput: raw };
  }

  // Try to extract scout payload from opencode NDJSON or claude single-JSON
  const extracted = extractScoutPayload(raw, worker);
  const parsed = extracted ? parseScoutOutput(extracted) : null;

  if (!parsed) {
    const fb = fallbackTask(prompt, config);
    const prdPath = join(prdsDir(repoPath), `${fb.id}.md`);
    mkdirSync(prdsDir(repoPath), { recursive: true });
    writeFileSync(prdPath, fb.prdMarkdown);
    try {
      enqueueTask(repoPath, { id: fb.id, title: fb.title, goal: fb.goal, acceptanceCriteria: fb.criteria, prdMarkdown: fb.prdMarkdown, source: `scout:${worker}:unparseable-fallback` });
    } catch { /* exists */ }
    const detail = `scout output unparseable, fallback ${fb.id} enqueued`;
    writeHeartbeat(repoPath, { lastStatus: 'ok', lastDetail: detail, worker, intervalMinutes, lastTaskId: fb.id });
    return { ok: true, taskId: fb.id, prdPath, queuePath: join(queueDir(repoPath), `${fb.id}.json`), heartbeatPath: hbPath, detail, rawPrompt: prompt, rawOutput: raw };
  }

  const prdPath = join(prdsDir(repoPath), `${parsed.id}.md`);
  mkdirSync(prdsDir(repoPath), { recursive: true });
  writeFileSync(prdPath, parsed.prdMarkdown);
  try {
    enqueueTask(repoPath, {
      id: parsed.id,
      title: parsed.title,
      goal: parsed.goal,
      acceptanceCriteria: parsed.criteria,
      prdMarkdown: parsed.prdMarkdown,
      source: `scout:${worker}`,
    });
  } catch {
    const detail = `task ${parsed.id} already queued`;
    writeHeartbeat(repoPath, { lastStatus: 'skipped', lastDetail: detail, worker, intervalMinutes, lastTaskId: parsed.id });
    return { ok: true, taskId: parsed.id, prdPath, queuePath: join(queueDir(repoPath), `${parsed.id}.json`), heartbeatPath: hbPath, detail, rawPrompt: prompt, rawOutput: raw };
  }

  const detail = `enqueued ${parsed.id}`;
  writeHeartbeat(repoPath, { lastStatus: 'ok', lastDetail: detail, worker, intervalMinutes, lastTaskId: parsed.id });
  return { ok: true, taskId: parsed.id, prdPath, queuePath: join(queueDir(repoPath), `${parsed.id}.json`), heartbeatPath: hbPath, detail, rawPrompt: prompt, rawOutput: raw };
}

export function extractScoutPayload(raw: string, worker: string): string | null {
  // opencode emits NDJSON lines with {type, text}; claude emits single JSON with {result}
  // or, with --output-format json, a single-line JSON ARRAY of message objects whose
  // final {type:"result"} element carries the answer in .result. Handle the array form
  // first: splitting on newlines would JSON.parse the whole array as one value and the
  // obj.result check would miss it (arrays have no .result), so scout fell back every cycle.
  const trimmed = raw.trim();
  if (trimmed.startsWith('[')) {
    try {
      const arr = JSON.parse(trimmed) as Array<Record<string, unknown>>;
      for (const obj of arr) {
        if (typeof obj.result === 'string' && obj.result.includes('---TASK---')) return obj.result;
        if (typeof obj.text === 'string' && obj.text.includes('---TASK---')) return obj.text;
      }
    } catch { /* not a JSON array; fall through to line scan */ }
  }
  for (const line of raw.split('\n')) {
    const t = line.trim();
    if (!t) continue;
    try {
      const obj = JSON.parse(t) as Record<string, unknown>;
      if (typeof obj.text === 'string' && obj.text.includes('---TASK---')) return obj.text as string;
      if (typeof obj.result === 'string' && (obj.result as string).includes('---TASK---')) return obj.result as string;
      if (typeof obj.part === 'string' && (obj.part as string).includes('---TASK---')) return obj.part as string;
    } catch { /* not JSON */ }
  }
  // Fallback: raw itself contains the markers
  if (raw.includes('---TASK---') && raw.includes('---PRD---')) return raw;
  return null;
}

export interface ScoutReplayResult {
  name: string;
  pass: boolean;
  expected: string | null;
  actual: string | null;
}

/** Replay every captured scout output fixture through extractScoutPayload and
 * compare against the golden.json expectations. Shared by the CLI --replay
 * flag and the golden test suite so both assert the exact same thing. */
export function replayScoutFixtures(fixturesDir?: string): ScoutReplayResult[] {
  if (!fixturesDir) {
    // tsc does not copy fixtures into the build output, so from compiled code
    // walk upward (dist/src/... or dist/...) to the nearest src/scout/__fixtures__.
    let dir = import.meta.dirname;
    for (let i = 0; i < 5; i++) {
      for (const candidate of [join(dir, 'scout', '__fixtures__'), join(dir, 'src', 'scout', '__fixtures__')]) {
        if (existsSync(join(candidate, 'golden.json'))) {
          fixturesDir = candidate;
          break;
        }
      }
      if (fixturesDir) break;
      dir = join(dir, '..');
    }
    if (!fixturesDir) {
      throw new Error('scout fixtures not found: no golden.json above ' + import.meta.dirname);
    }
  }
  const golden = JSON.parse(readFileSync(join(fixturesDir, 'golden.json'), 'utf8')) as Record<string, { worker: string; expected: string | null }>;
  const results: ScoutReplayResult[] = [];
  for (const [name, entry] of Object.entries(golden)) {
    const content = readFileSync(join(fixturesDir, name), 'utf8');
    const actual = extractScoutPayload(content, entry.worker);
    results.push({ name, pass: actual === entry.expected, expected: entry.expected, actual });
  }
  return results;
}

export function readHeartbeat(repoPath: string): ScoutHeartbeat | null {
  const p = heartbeatPath(repoPath);
  if (!existsSync(p)) return null;
  try {
    const hb = JSON.parse(readFileSync(p, 'utf8')) as Partial<ScoutHeartbeat>;
    // A heartbeat whose timestamp cannot be parsed is as good as missing:
    // scout-status reports an age from it, and a NaN age would mislead
    // operators about whether the 24/7 scout is alive.
    if (typeof hb.lastRunAt !== 'string' || Number.isNaN(Date.parse(hb.lastRunAt))) return null;
    return hb as ScoutHeartbeat;
  } catch {
    return null;
  }
}

export function writeHeartbeat(
  repoPath: string,
  patch: Omit<ScoutHeartbeat, 'lastRunAt'> & { lastRunAt?: string },
): ScoutHeartbeat {
  const hb: ScoutHeartbeat = {
    lastRunAt: patch.lastRunAt ?? new Date().toISOString(),
    lastTaskId: patch.lastTaskId,
    lastStatus: patch.lastStatus,
    lastDetail: patch.lastDetail,
    worker: patch.worker,
    intervalMinutes: patch.intervalMinutes,
  };
  mkdirSync(join(repoPath, '.devagent'), { recursive: true });
  const p = heartbeatPath(repoPath);
  writeFileSync(p, JSON.stringify(hb, null, 2) + '\n');
  return hb;
}

export async function runScoutLoop(
  opts: { repoPath: string; worker?: 'opencode' | 'claude-code' | 'omp' | 'pi'; intervalMinutes: number; timeoutMs?: number; signal?: AbortSignal },
  config: DevAgentConfig,
  onCycle?: (result: ScoutCycleResult) => void,
): Promise<void> {
  const lock = acquireScoutLock(opts.repoPath);
  if (!lock) {
    console.log(`[scout] another scout loop is already running for this repo (pid ${readScoutLockPid(opts.repoPath)}); exiting`);
    return;
  }
  try {
    while (!opts.signal?.aborted) {
      const result = await runScoutOnce({ repoPath: opts.repoPath, worker: opts.worker, intervalMinutes: opts.intervalMinutes, timeoutMs: opts.timeoutMs }, config);
      onCycle?.(result);
      if (opts.signal?.aborted) break;
      const sleepMs = opts.intervalMinutes * 60_000;
      await new Promise<void>((resolve) => {
        const t = setTimeout(resolve, sleepMs);
        opts.signal?.addEventListener('abort', () => { clearTimeout(t); resolve(); }, { once: true });
      });
    }
  } finally {
    releaseScoutLock(opts.repoPath);
  }
}

const scoutLockPath = (repoPath: string) => join(repoPath, '.devagent', 'scout.lock');

function processAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

/** Try to take the single-instance scout lock; false when a live holder exists. */
export function acquireScoutLock(repoPath: string, now: number = Date.now()): boolean {
  mkdirSync(join(repoPath, '.devagent'), { recursive: true });
  const p = scoutLockPath(repoPath);
  let current: { pid?: number; at?: number } = {};
  try { current = JSON.parse(readFileSync(p, 'utf8')) as { pid?: number; at?: number }; } catch { /* stale/corrupt */ }
  if (typeof current.pid === 'number' && processAlive(current.pid)) {
    // Liveness check passes for ANY live pid, including foreign namespaces;
    // combined with the mtime staleness window this is best-effort by design.
    return false;
  }
  writeFileSync(p, JSON.stringify({ pid: process.pid, at: now }) + '\n');
  return true;
}

export function releaseScoutLock(repoPath: string): void {
  try {
    const p = scoutLockPath(repoPath);
    const current = JSON.parse(readFileSync(p, 'utf8')) as { pid?: number };
    if (current.pid === process.pid) rmSync(p);
  } catch { /* already gone */ }
}

function readScoutLockPid(repoPath: string): number | null {
  try {
    const current = JSON.parse(readFileSync(scoutLockPath(repoPath), 'utf8')) as { pid?: number };
    return typeof current.pid === 'number' ? current.pid : null;
  } catch {
    return null;
  }
}
