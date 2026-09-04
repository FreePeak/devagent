import { existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync, rmSync, renameSync } from 'node:fs';
import { join } from 'node:path';

export type QueuedTaskStatus = 'pending' | 'claimed' | 'done' | 'failed';

export interface QueuedTask {
  id: string;
  title: string;
  /** Full goal/prompt text passed to devagent task */
  goal: string;
  description?: string;
  acceptanceCriteria: string[];
  status: QueuedTaskStatus;
  createdAt: string;
  updatedAt: string;
  /** Worker that claimed the task */
  claimedBy?: string;
  claimedAt?: string;
  prdPath?: string;
  source?: string;
  /** Last failure detail for retry visibility */
  lastError?: string;
  attempts?: number;
  /**
   * Cross-board retry memory (Q27): executor failure class carried from an
   * archived board for a re-bridged goal. Tasks carrying one claim after all
   * clean tasks (claimNextPending two-tier order).
   */
  failureClass?: string;
}

export interface EnqueueInput {
  id: string;
  title: string;
  goal: string;
  description?: string;
  acceptanceCriteria?: string[];
  prdMarkdown?: string;
  source?: string;
  /** Carried executor failure class from a prior archived board for this goal (Q27). */
  failureClass?: string;
}

function sanitizeId(id: string): string {
  const s = id.replace(/[^A-Za-z0-9._-]/g, '-').replace(/-+/g, '-').replace(/^-|-$/g, '');
  if (!s) throw new Error(`Invalid task id "${id}"`);
  return s;
}

export function queueDir(repoPath: string): string {
  return join(repoPath, '.devagent', 'queue');
}

export function prdsDir(repoPath: string): string {
  return join(repoPath, '.devagent', 'prds');
}

export function ensureQueueDirs(repoPath: string): void {
  mkdirSync(queueDir(repoPath), { recursive: true });
  mkdirSync(prdsDir(repoPath), { recursive: true });
}

function taskPath(repoPath: string, id: string): string {
  return join(queueDir(repoPath), `${sanitizeId(id)}.json`);
}

function prdPath(repoPath: string, id: string): string {
  return join(prdsDir(repoPath), `${sanitizeId(id)}.md`);
}

function nowIso(): string {
  return new Date().toISOString();
}

function readTaskFile(path: string): QueuedTask | null {
  if (!existsSync(path)) return null;
  try {
    return JSON.parse(readFileSync(path, 'utf8')) as QueuedTask;
  } catch {
    return null;
  }
}

function writeTaskFile(path: string, task: QueuedTask): void {
  mkdirSync(join(path, '..'), { recursive: true });
  const tmp = `${path}.tmp.${process.pid}`;
  writeFileSync(tmp, JSON.stringify(task, null, 2) + '\n');
  try {
    renameSync(tmp, path);
  } catch {
    writeFileSync(path, JSON.stringify(task, null, 2) + '\n');
    try { rmSync(tmp, { force: true }); } catch { /* ignore */ }
  }
}

/** Enqueue a new task; throws if id already exists. Writes PRD markdown when provided. */
export function enqueueTask(repoPath: string, input: EnqueueInput): QueuedTask {
  ensureQueueDirs(repoPath);
  const id = sanitizeId(input.id);
  const path = taskPath(repoPath, id);
  if (existsSync(path)) throw new Error(`Task ${id} already queued`);
  const ts = nowIso();
  const task: QueuedTask = {
    id,
    title: input.title.slice(0, 120),
    goal: input.goal,
    description: input.description,
    acceptanceCriteria: input.acceptanceCriteria ?? [],
    status: 'pending',
    createdAt: ts,
    updatedAt: ts,
    source: input.source ?? 'scout',
    attempts: 0,
    failureClass: input.failureClass,
  };
  if (input.prdMarkdown) {
    const p = prdPath(repoPath, id);
    writeFileSync(p, input.prdMarkdown);
    task.prdPath = p;
  }
  writeTaskFile(path, task);
  return task;
}

/** List tasks, optionally filtered by status. Sorted by createdAt ascending. */
export function listTasks(
  repoPath: string,
  filter?: { status?: QueuedTaskStatus },
): QueuedTask[] {
  const dir = queueDir(repoPath);
  if (!existsSync(dir)) return [];
  const files = readdirSync(dir).filter((f) => f.endsWith('.json'));
  const tasks: QueuedTask[] = [];
  for (const f of files) {
    const t = readTaskFile(join(dir, f));
    if (!t) continue;
    if (filter?.status && t.status !== filter.status) continue;
    tasks.push(t);
  }
  tasks.sort((a, b) => a.createdAt.localeCompare(b.createdAt));
  return tasks;
}

export function readTask(repoPath: string, id: string): QueuedTask | null {
  return readTaskFile(taskPath(repoPath, sanitizeId(id)));
}

/** Update a task's fields; returns the updated task or null if missing. */
export function updateTask(
  repoPath: string,
  id: string,
  patch: Partial<Omit<QueuedTask, 'id' | 'createdAt'>> & { lastError?: string },
): QueuedTask | null {
  const path = taskPath(repoPath, sanitizeId(id));
  const cur = readTaskFile(path);
  if (!cur) return null;
  const next: QueuedTask = { ...cur, ...patch, id: cur.id, createdAt: cur.createdAt, updatedAt: nowIso() } as QueuedTask;
  writeTaskFile(path, next);
  return next;
}

export function setTaskStatus(
  repoPath: string,
  id: string,
  status: QueuedTaskStatus,
  detail?: string,
): QueuedTask | null {
  const patch: Partial<QueuedTask> = { status };
  if (status === 'failed' && detail) patch.lastError = detail.slice(0, 2000);
  if (status === 'done') patch.lastError = undefined;
  return updateTask(repoPath, id, patch);
}

/** Claim a specific task if it is pending; returns null if not pending or missing. */
export function claimTask(repoPath: string, id: string, workerId: string): QueuedTask | null {
  const path = taskPath(repoPath, sanitizeId(id));
  const cur = readTaskFile(path);
  if (!cur || cur.status !== 'pending') return null;
  const next: QueuedTask = {
    ...cur,
    status: 'claimed',
    claimedBy: workerId,
    claimedAt: nowIso(),
    updatedAt: nowIso(),
    attempts: (cur.attempts ?? 0) + 1,
  };
  writeTaskFile(path, next);
  // re-read to detect race: if another worker raced, the file we just wrote wins (last-writer-wins is acceptable for single-consumer fleet; for multi-consumer add file locking)
  return next;
}

/**
 * Claim the oldest pending task; returns null when none pending.
 * Two-tier order (Q27 cross-board retry memory): tasks without a carried
 * failureClass first, then tasks carrying one — oldest createdAt first within
 * each tier — so re-bridged failures wait until fresh work drains.
 */
export function claimNextPending(repoPath: string, workerId: string): QueuedTask | null {
  const pendings = listTasks(repoPath, { status: 'pending' });
  const clean = pendings.filter((t) => !t.failureClass);
  const carried = pendings.filter((t) => t.failureClass);
  for (const t of [...clean, ...carried]) {
    const claimed = claimTask(repoPath, t.id, workerId);
    if (claimed) return claimed;
  }
  return null;
}

export function writePrd(repoPath: string, id: string, markdown: string): string {
  ensureQueueDirs(repoPath);
  const p = prdPath(repoPath, sanitizeId(id));
  writeFileSync(p, markdown);
  const existing = readTask(repoPath, id);
  if (existing && !existing.prdPath) updateTask(repoPath, id, { prdPath: p });
  return p;
}

export function readPrd(repoPath: string, id: string): string | null {
  const p = prdPath(repoPath, sanitizeId(id));
  if (!existsSync(p)) return null;
  return readFileSync(p, 'utf8');
}

/** Remove done tasks older than cutoffMs (0 = all done). Returns count removed. */
export function pruneDone(repoPath: string, cutoffMs: number = 0): number {
  const tasks = listTasks(repoPath, { status: 'done' });
  let removed = 0;
  const now = Date.now();
  for (const t of tasks) {
    const age = now - Date.parse(t.updatedAt);
    if (age >= cutoffMs) {
      try {
        rmSync(taskPath(repoPath, t.id), { force: true });
        removed++;
      } catch { /* ignore */ }
    }
  }
  return removed;
}

export function taskCount(repoPath: string): Record<QueuedTaskStatus | 'total', number> {
  const all = listTasks(repoPath);
  const c: Record<string, number> = { total: all.length, pending: 0, claimed: 0, done: 0, failed: 0 };
  for (const t of all) c[t.status] = (c[t.status] ?? 0) + 1;
  return c as Record<QueuedTaskStatus | 'total', number>;
}
