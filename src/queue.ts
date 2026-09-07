import { existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync, rmSync, renameSync, linkSync } from 'node:fs';
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
  /**
   * Fencing token for the current claim (FR-VIS-09 extended from the loop
   * driver to queue claims). Every claim — including an expired-lease reclaim —
   * increments it and never reuses a value, so a write that carries an older
   * generation provably belongs to a worker that lost the task.
   */
  leaseGeneration?: number;
  /** ISO deadline after which a `claimed` task is reclaimable by another worker. */
  leaseExpiresAt?: string;
  /** Worker holding the current lease (stamped per claim beside claimedBy). */
  leaseOwner?: string;
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

/**
 * Claim machinery (FR-VIS-09 extended from the single-instance loop driver to
 * queue claims). The driver lock stops one repo running two drivers; this stops
 * two consumers claiming the same task, which the old read-check-write in
 * `claimTask` could not (last-writer-wins).
 */

/**
 * Default lease duration. Sized to the longest legitimate single claim:
 * `scripts/selfbuild-loop.sh` caps one task at SELFBUILD_TASK_TIMEOUT (7200s
 * default), so a healthy in-flight claim is never reclaimed under its owner
 * while a crashed holder's lease still lapses inside one iteration.
 */
export const DEFAULT_LEASE_MS = 2 * 60 * 60_000;

/**
 * A claim lock is held only for the read-modify-write of one task file, so a
 * lock older than this belongs to a process that died mid-claim: break it
 * (same stale-holder recovery the driver's mkdir lock runs on pid liveness).
 */
export const DEFAULT_CLAIM_LOCK_STALE_MS = 30_000;

export interface ClaimOptions {
  /** Lease lifetime before the claim becomes reclaimable (default DEFAULT_LEASE_MS). */
  leaseMs?: number;
  /** Injectable clock so lease behaviour is testable without sleeping. */
  now?: () => number;
  /** Age past which a wedged claim lock is broken (default DEFAULT_CLAIM_LOCK_STALE_MS). */
  lockStaleMs?: number;
}

export interface FencedWriteOptions {
  now?: () => number;
  lockStaleMs?: number;
}

function claimLockPath(repoPath: string, id: string): string {
  return join(queueDir(repoPath), `${sanitizeId(id)}.claim.lock`);
}

/**
 * True when a claimed task's lease has lapsed and another worker may take it.
 * A `claimed` task with no `leaseExpiresAt` predates leasing, so it is treated
 * as lapsed — otherwise legacy claims would wedge the queue head forever.
 */
export function leaseIsExpired(task: QueuedTask, now: number = Date.now()): boolean {
  if (!task.leaseExpiresAt) return true;
  const at = Date.parse(task.leaseExpiresAt);
  return !Number.isFinite(at) || at <= now;
}

/** Whether a task is claimable right now: pending, or claimed past its lease. */
function claimableNow(task: QueuedTask, now: number): boolean {
  if (task.status === 'pending') return true;
  return task.status === 'claimed' && leaseIsExpired(task, now);
}

/**
 * Acquire the per-task claim lock atomically with `link()`: creating a hard
 * link to an existing path fails EEXIST, so exactly one caller wins even when
 * several race at once — and unlike `flock` it is portable to macOS, where the
 * driver already avoids it (scripts/selfbuild-loop.sh:12). The holder writes
 * its identity into the link target first, so a wedged lock is diagnosable.
 * Returns the lock path on success, null when another claimant holds it.
 */
function acquireClaimLock(
  repoPath: string,
  id: string,
  workerId: string,
  now: number,
  staleMs: number,
): string | null {
  const lock = claimLockPath(repoPath, id);
  const tmp = `${lock}.tmp.${process.pid}.${now}`;
  for (let attempt = 0; attempt < 2; attempt++) {
    writeFileSync(tmp, JSON.stringify({ workerId, pid: process.pid, acquiredAtMs: now }) + '\n');
    try {
      linkSync(tmp, lock);
      rmSync(tmp, { force: true });
      return lock;
    } catch (err) {
      rmSync(tmp, { force: true });
      // EEXIST is the only contention signal; any other errno (missing dir,
      // EPERM on a foreign filesystem) is ours to abort on, not to retry.
      const code = (err as NodeJS.ErrnoException).code;
      if (code !== 'EEXIST') return null;
      if (attempt === 0 && claimLockIsStale(lock, now, staleMs)) {
        try { rmSync(lock, { force: true }); } catch { /* ignore */ }
        continue;
      }
      return null;
    }
  }
  return null;
}

/** A lock with no readable timestamp is corrupt or legacy: break it (latest-wins). */
function claimLockIsStale(lock: string, now: number, staleMs: number): boolean {
  let acquiredAtMs: number | null = null;
  try {
    const raw: unknown = JSON.parse(readFileSync(lock, 'utf8'));
    if (raw && typeof raw === 'object' && 'acquiredAtMs' in raw && typeof raw.acquiredAtMs === 'number') {
      acquiredAtMs = raw.acquiredAtMs;
    }
  } catch {
    acquiredAtMs = null;
  }
  return acquiredAtMs === null || now - acquiredAtMs > staleMs;
}

function releaseClaimLock(lock: string): void {
  try { rmSync(lock, { force: true }); } catch { /* ignore */ }
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

/**
 * Update a task's fields; returns the updated task or null if missing.
 *
 * Pass `expectedGeneration` to fence the write to the worker that currently
 * holds the claim: the patch lands only while it matches the stored
 * `leaseGeneration`, and a stale token is refused (null, no write). Without it
 * the write stays unfenced — correct for out-of-band administration of a task
 * nobody has claimed (the scout's PRD backfill, the bridge retiring a pending
 * goal), never for claim-lifecycle writes.
 */
export function updateTask(
  repoPath: string,
  id: string,
  patch: Partial<Omit<QueuedTask, 'id' | 'createdAt'>> & { lastError?: string },
  opts: FencedWriteOptions & { expectedGeneration?: number } = {},
): QueuedTask | null {
  if (opts.expectedGeneration !== undefined) {
    return fencedUpdate(repoPath, id, opts.expectedGeneration, patch, opts);
  }
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
  opts: FencedWriteOptions & { expectedGeneration?: number } = {},
): QueuedTask | null {
  const patch: Partial<QueuedTask> = { status };
  if (status === 'failed' && detail) patch.lastError = detail.slice(0, 2000);
  if (status === 'done') patch.lastError = undefined;
  return updateTask(repoPath, id, patch, opts);
}

/**
 * Read-modify-write a task under the claim lock, refusing the write unless
 * `generation` is the task's current fencing token. Returns null when the task
 * is missing, another claimant holds the lock, or the token is stale — a stale
 * token means the lease moved to a different worker, so this writer must not
 * touch the record.
 */
function fencedUpdate(
  repoPath: string,
  id: string,
  generation: number,
  patch: Partial<QueuedTask>,
  opts: FencedWriteOptions = {},
): QueuedTask | null {
  const nowFn = opts.now ?? Date.now;
  const path = taskPath(repoPath, sanitizeId(id));
  if (!existsSync(path)) return null;
  const lock = acquireClaimLock(repoPath, id, `fenced-${process.pid}`, nowFn(), opts.lockStaleMs ?? DEFAULT_CLAIM_LOCK_STALE_MS);
  if (!lock) return null;
  try {
    const cur = readTaskFile(path);
    if (!cur) return null;
    if ((cur.leaseGeneration ?? 0) !== generation) return null;
    const next: QueuedTask = {
      ...cur,
      ...patch,
      id: cur.id,
      createdAt: cur.createdAt,
      updatedAt: new Date(nowFn()).toISOString(),
    };
    writeTaskFile(path, next);
    return next;
  } finally {
    releaseClaimLock(lock);
  }
}

/** Fenced completion: refused once the lease is no longer this worker's. */
export function completeTask(
  repoPath: string,
  id: string,
  generation: number,
  opts: FencedWriteOptions = {},
): QueuedTask | null {
  return fencedUpdate(repoPath, id, generation, { status: 'done', lastError: undefined }, opts);
}

/** Fenced terminal failure. */
export function failTask(
  repoPath: string,
  id: string,
  generation: number,
  detail?: string,
  opts: FencedWriteOptions = {},
): QueuedTask | null {
  const patch: Partial<QueuedTask> = { status: 'failed' };
  if (detail) patch.lastError = detail.slice(0, 2000);
  return fencedUpdate(repoPath, id, generation, patch, opts);
}

/**
 * Fenced requeue back to pending. Releases the lease by BUMPING the generation
 * (never reusing a token), so the releasing worker's own writes are dead from
 * the moment it hands the task back and a late completion cannot resurrect it.
 */
export function requeueTask(
  repoPath: string,
  id: string,
  generation: number,
  detail?: string,
  opts: FencedWriteOptions = {},
): QueuedTask | null {
  const patch: Partial<QueuedTask> = {
    status: 'pending',
    leaseGeneration: generation + 1,
    leaseOwner: undefined,
    leaseExpiresAt: undefined,
  };
  if (detail) patch.lastError = detail.slice(0, 2000);
  return fencedUpdate(repoPath, id, generation, patch, opts);
}

/**
 * Claim a task under the atomic `link()` claim lock and stamp a fresh fencing
 * token. Claimable when pending or when a previous claim's lease has lapsed —
 * a reclaim bumps the generation, so the dead holder's writes are refused from
 * then on. Returns null when the task is missing, leased to a live worker, or
 * another claimant currently holds the lock.
 */
export function claimTask(
  repoPath: string,
  id: string,
  workerId: string,
  opts: ClaimOptions = {},
): QueuedTask | null {
  const nowFn = opts.now ?? Date.now;
  const path = taskPath(repoPath, sanitizeId(id));
  if (!existsSync(path)) return null;
  const lock = acquireClaimLock(repoPath, id, workerId, nowFn(), opts.lockStaleMs ?? DEFAULT_CLAIM_LOCK_STALE_MS);
  if (!lock) return null;
  try {
    const cur = readTaskFile(path);
    if (!cur) return null;
    const at = nowFn();
    if (!claimableNow(cur, at)) return null;
    const ts = new Date(at).toISOString();
    const generation = (cur.leaseGeneration ?? 0) + 1;
    const next: QueuedTask = {
      ...cur,
      status: 'claimed',
      claimedBy: workerId,
      claimedAt: ts,
      leaseOwner: workerId,
      leaseGeneration: generation,
      leaseExpiresAt: new Date(at + (opts.leaseMs ?? DEFAULT_LEASE_MS)).toISOString(),
      updatedAt: ts,
      attempts: (cur.attempts ?? 0) + 1,
    };
    writeTaskFile(path, next);
    return next;
  } finally {
    releaseClaimLock(lock);
  }
}

/**
 * Claim the next claimable task; returns null when none is.
 * Two-tier order (Q27 cross-board retry memory): tasks without a carried
 * failureClass first, then tasks carrying one — oldest createdAt first within
 * each tier — so re-bridged failures wait until fresh work drains. Expired
 * leases join the candidate set, so a crashed worker's task is reclaimed by the
 * next cycle instead of wedging.
 */
export function claimNextPending(
  repoPath: string,
  workerId: string,
  opts: ClaimOptions = {},
): QueuedTask | null {
  const now = (opts.now ?? Date.now)();
  const claimable = listTasks(repoPath).filter((t) => claimableNow(t, now));
  const clean = claimable.filter((t) => !t.failureClass);
  const carried = claimable.filter((t) => t.failureClass);
  for (const t of [...clean, ...carried]) {
    const claimed = claimTask(repoPath, t.id, workerId, opts);
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
