import { describe, expect, it, beforeEach } from 'vitest';
import { mkdtempSync, rmSync, existsSync, readdirSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { enqueueTask, listTasks, claimTask, claimNextPending, updateTask, setTaskStatus, readTask, writePrd, readPrd, pruneDone, taskCount, ensureQueueDirs, queueDir, completeTask, failTask, requeueTask, leaseIsExpired } from '../src/queue.js';

function tmpRepo(): string {
  const d = mkdtempSync(join(tmpdir(), 'da-queue-'));
  return d;
}

describe('queue: enqueue + list + read', () => {
  it('enqueues and lists tasks sorted by id', () => {
    const repo = tmpRepo();
    try {
      enqueueTask(repo, { id: 'TASK-1', title: 'First', goal: 'Goal: first' });
      enqueueTask(repo, { id: 'TASK-2', title: 'Second', goal: 'Goal: second' });
      const all = listTasks(repo);
      expect(all).toHaveLength(2);
      expect(all[0]!.id).toBe('TASK-1');
      expect(readTask(repo, 'TASK-1')!.title).toBe('First');
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('rejects duplicate id', () => {
    const repo = tmpRepo();
    try {
      enqueueTask(repo, { id: 'DUP', title: 'a', goal: 'Goal: a' });
      expect(() => enqueueTask(repo, { id: 'DUP', title: 'b', goal: 'Goal: b' })).toThrow(/already queued/);
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('sanitizes weird ids', () => {
    const repo = tmpRepo();
    try {
      const t = enqueueTask(repo, { id: 'TASK / 1 !!', title: 'x', goal: 'Goal: x' });
      expect(t.id).toMatch(/^[A-Za-z0-9._-]+$/);
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('writes PRD markdown when provided and reads it back', () => {
    const repo = tmpRepo();
    try {
      enqueueTask(repo, { id: 'PRD-1', title: 't', goal: 'Goal: t', prdMarkdown: '# PRD\nok' });
      expect(readPrd(repo, 'PRD-1')).toBe('# PRD\nok');
      expect(readTask(repo, 'PRD-1')!.prdPath).toContain('PRD-1.md');
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('writePrd + readPrd standalone', () => {
    const repo = tmpRepo();
    try {
      ensureQueueDirs(repo);
      writePrd(repo, 'X-1', 'hello');
      expect(readPrd(repo, 'X-1')).toBe('hello');
      expect(readPrd(repo, 'missing')).toBeNull();
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });
});

describe('queue: claim', () => {
  it('claimTask transitions pending->claimed and bumps attempts', () => {
    const repo = tmpRepo();
    try {
      enqueueTask(repo, { id: 'C-1', title: 'c', goal: 'Goal: c' });
      const claimed = claimTask(repo, 'C-1', 'w1');
      expect(claimed!.status).toBe('claimed');
      expect(claimed!.attempts).toBe(1);
      // second claim on same task fails
      expect(claimTask(repo, 'C-1', 'w2')).toBeNull();
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('claimNextPending picks oldest pending', () => {
    const repo = tmpRepo();
    try {
      enqueueTask(repo, { id: 'A', title: 'a', goal: 'Goal: a' });
      enqueueTask(repo, { id: 'B', title: 'b', goal: 'Goal: b' });
      const c = claimNextPending(repo, 'w1');
      expect(c!.id).toBe('A');
      expect(listTasks(repo, { status: 'pending' })).toHaveLength(1);
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('claimNextPending prefers clean tasks over failure-carrying ones (Q27)', () => {
    const repo = tmpRepo();
    try {
      enqueueTask(repo, { id: 'CARRY-OLD', title: 'carried old', goal: 'Goal: carried old', failureClass: 'test-gate' });
      enqueueTask(repo, { id: 'CARRY-NEW', title: 'carried new', goal: 'Goal: carried new', failureClass: 'worker-error' });
      enqueueTask(repo, { id: 'CLEAN', title: 'clean', goal: 'Goal: clean' });
      // Pin createdAt via direct queue-JSON writes: rapid enqueues can tie on
      // the wall clock, and tier order must be deterministic (updateTask
      // locks id|createdAt so the file is the only way to set it).
      for (const [id, at] of [['CARRY-OLD', '2026-09-01T00:00:00.000Z'], ['CARRY-NEW', '2026-09-02T00:00:00.000Z'], ['CLEAN', '2026-09-03T00:00:00.000Z']] as const) {
        const t = readTask(repo, id)!;
        writeFileSync(join(queueDir(repo), `${id}.json`), JSON.stringify({ ...t, createdAt: at }, null, 2) + '\n');
      }
      // carried tasks created first but a clean task exists: CLEAN must win
      expect(claimNextPending(repo, 'w1')!.id).toBe('CLEAN');
      // among carried tasks, oldest createdAt first
      expect(claimNextPending(repo, 'w1')!.id).toBe('CARRY-OLD');
      expect(claimNextPending(repo, 'w1')!.id).toBe('CARRY-NEW');
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('updateTask preserves id/createdAt and can clear failureClass via direct JSON write (Q27)', () => {
    const repo = tmpRepo();
    try {
      enqueueTask(repo, { id: 'JW-1', title: 'j', goal: 'Goal: j', failureClass: 'test-gate' });
      // Direct queue-JSON write with a stable createdAt (updateTask locks id|createdAt)
      const t = readTask(repo, 'JW-1')!;
      writeFileSync(join(queueDir(repo), 'JW-1.json'), JSON.stringify({ ...t, failureClass: undefined, status: 'pending', createdAt: '2026-09-01T00:00:00.000Z' }, null, 2) + '\n');
      const patched = updateTask(repo, 'JW-1', { title: 'jj' });
      expect(patched!.id).toBe('JW-1');
      expect(patched!.createdAt).toBe('2026-09-01T00:00:00.000Z');
      expect(patched!.failureClass).toBeUndefined();
      // clean-first order now claims it before a still-carried older task
      const carried = readTask(repo, 'STILL-CARRIED')!;
      writeFileSync(join(queueDir(repo), 'STILL-CARRIED.json'), JSON.stringify({ ...carried, createdAt: '2026-08-31T00:00:00.000Z' }, null, 2) + '\n');
      expect(claimNextPending(repo, 'w1')!.id).toBe('JW-1');
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('enqueueTask stamps carried failureClass onto the queued task (Q27)', () => {
    const repo = tmpRepo();
    try {
      const t = enqueueTask(repo, { id: 'STAMP-1', title: 's', goal: 'Goal: s', failureClass: 'test-gate' });
      expect(t.failureClass).toBe('test-gate');
      expect(readTask(repo, 'STAMP-1')!.failureClass).toBe('test-gate');
      const clean = enqueueTask(repo, { id: 'STAMP-2', title: 's2', goal: 'Goal: s2' });
      expect(clean.failureClass).toBeUndefined();
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });
});

/** Deterministic clock: leases are wall-clock behaviour, tests must not sleep. */
function fakeClock(start: number): { now: () => number; advance: (ms: number) => void } {
  let at = start;
  return { now: () => at, advance: (ms: number) => { at += ms; } };
}

const T0 = Date.parse('2026-09-07T00:00:00.000Z');

describe('queue: claim lease + fencing tokens (FR-VIS-09)', () => {
  it('double claim: exactly one worker wins the lease', () => {
    const repo = tmpRepo();
    try {
      const clock = fakeClock(T0);
      enqueueTask(repo, { id: 'L-1', title: 'l', goal: 'Goal: l' });
      const first = claimTask(repo, 'L-1', 'w1', { now: clock.now, leaseMs: 60_000 });
      const second = claimTask(repo, 'L-1', 'w2', { now: clock.now, leaseMs: 60_000 });
      expect(first!.status).toBe('claimed');
      expect(first!.leaseGeneration).toBe(1);
      expect(first!.leaseOwner).toBe('w1');
      expect(second).toBeNull();
      const stored = readTask(repo, 'L-1')!;
      expect(stored.leaseOwner).toBe('w1');
      expect(stored.claimedBy).toBe('w1');
      expect(stored.attempts).toBe(1);
      // the claim lock is released with the claim, not left behind
      expect(existsSync(join(queueDir(repo), 'L-1.claim.lock'))).toBe(false);
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('a live claim lock makes the loser give up without touching the task or the lock', () => {
    const repo = tmpRepo();
    try {
      const clock = fakeClock(T0);
      enqueueTask(repo, { id: 'L-2', title: 'l', goal: 'Goal: l' });
      // Another claimant mid-read-modify-write (link() already won the name).
      const lock = join(queueDir(repo), 'L-2.claim.lock');
      writeFileSync(lock, JSON.stringify({ workerId: 'other', pid: 999_999, acquiredAtMs: clock.now() }) + '\n');
      expect(claimTask(repo, 'L-2', 'w1', { now: clock.now })).toBeNull();
      expect(readTask(repo, 'L-2')!.status).toBe('pending');
      expect(existsSync(lock)).toBe(true);
      expect(readdirSync(queueDir(repo)).filter((f) => f.includes('.tmp.'))).toEqual([]);
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('a wedged claim lock past the stale window is broken and the claim proceeds', () => {
    const repo = tmpRepo();
    try {
      const clock = fakeClock(T0);
      enqueueTask(repo, { id: 'L-3', title: 'l', goal: 'Goal: l' });
      const lock = join(queueDir(repo), 'L-3.claim.lock');
      // Holder died between link() and unlink(): lock is 60s old, window is 30s.
      writeFileSync(lock, JSON.stringify({ workerId: 'dead', pid: 1, acquiredAtMs: clock.now() - 60_000 }) + '\n');
      expect(claimTask(repo, 'L-3', 'w1', { now: clock.now })).not.toBeNull();
      expect(readTask(repo, 'L-3')!.leaseOwner).toBe('w1');
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('expired lease is reclaimable and bumps the generation (never reused)', () => {
    const repo = tmpRepo();
    try {
      const clock = fakeClock(T0);
      enqueueTask(repo, { id: 'L-4', title: 'l', goal: 'Goal: l' });
      const first = claimTask(repo, 'L-4', 'w1', { now: clock.now, leaseMs: 1000 })!;
      expect(leaseIsExpired(first, clock.now())).toBe(false);
      // still inside the lease: nobody else gets in
      expect(claimTask(repo, 'L-4', 'w2', { now: clock.now, leaseMs: 1000 })).toBeNull();
      clock.advance(1000);
      const reclaimed = claimTask(repo, 'L-4', 'w2', { now: clock.now, leaseMs: 1000 })!;
      expect(reclaimed.leaseGeneration).toBe(first.leaseGeneration! + 1);
      expect(reclaimed.leaseOwner).toBe('w2');
      expect(reclaimed.attempts).toBe(2);
      expect(leaseIsExpired(reclaimed, clock.now() + 999)).toBe(false);
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('claimNextPending reclaims an expired lease instead of wedging on it', () => {
    const repo = tmpRepo();
    try {
      const clock = fakeClock(T0);
      enqueueTask(repo, { id: 'L-5', title: 'l', goal: 'Goal: l' });
      expect(claimNextPending(repo, 'w1', { now: clock.now, leaseMs: 1000 })!.leaseGeneration).toBe(1);
      expect(claimNextPending(repo, 'w2', { now: clock.now, leaseMs: 1000 })).toBeNull();
      clock.advance(1000);
      const reclaimed = claimNextPending(repo, 'w2', { now: clock.now, leaseMs: 1000 })!;
      expect(reclaimed.leaseGeneration).toBe(2);
      expect(reclaimed.leaseOwner).toBe('w2');
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('stale-generation writes are refused: complete, fail, and requeue', () => {
    const repo = tmpRepo();
    try {
      const clock = fakeClock(T0);
      enqueueTask(repo, { id: 'L-6', title: 'l', goal: 'Goal: l' });
      claimTask(repo, 'L-6', 'w1', { now: clock.now, leaseMs: 1000 });
      clock.advance(1000);
      claimTask(repo, 'L-6', 'w2', { now: clock.now, leaseMs: 1000 }); // generation 2
      // w1 woke up late and still holds generation 1
      expect(completeTask(repo, 'L-6', 1)).toBeNull();
      expect(failTask(repo, 'L-6', 1, 'boom')).toBeNull();
      expect(requeueTask(repo, 'L-6', 1, 'oops')).toBeNull();
      expect(setTaskStatus(repo, 'L-6', 'done', undefined, { expectedGeneration: 1 })).toBeNull();
      const held = readTask(repo, 'L-6')!;
      expect(held.status).toBe('claimed');
      expect(held.leaseOwner).toBe('w2');
      expect(held.leaseGeneration).toBe(2);
      expect(held.lastError).toBeUndefined();
      // the current token writes
      expect(completeTask(repo, 'L-6', 2)!.status).toBe('done');
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('requeue releases the lease and kills the releasing worker token', () => {
    const repo = tmpRepo();
    try {
      const clock = fakeClock(T0);
      enqueueTask(repo, { id: 'L-7', title: 'l', goal: 'Goal: l' });
      claimTask(repo, 'L-7', 'w1', { now: clock.now });
      const requeued = requeueTask(repo, 'L-7', 1, 'transient infra')!;
      expect(requeued.status).toBe('pending');
      expect(requeued.leaseGeneration).toBe(2);
      expect(requeued.leaseOwner).toBeUndefined();
      expect(requeued.lastError).toContain('transient infra');
      // a late completion from the worker that released it is refused
      expect(completeTask(repo, 'L-7', 1)).toBeNull();
      expect(readTask(repo, 'L-7')!.status).toBe('pending');
      // and the next claim continues the sequence rather than reusing 1
      expect(claimTask(repo, 'L-7', 'w2', { now: clock.now })!.leaseGeneration).toBe(3);
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('failTask records the detail and completeTask clears it under the current token', () => {
    const repo = tmpRepo();
    try {
      enqueueTask(repo, { id: 'L-8', title: 'l', goal: 'Goal: l' });
      const claimed = claimTask(repo, 'L-8', 'w1')!;
      const gen = claimed.leaseGeneration!;
      expect(failTask(repo, 'L-8', gen, 'gate red')!.lastError).toBe('gate red');
      expect(readTask(repo, 'L-8')!.status).toBe('failed');
      const retried = claimTask(repo, 'L-8', 'w2');
      // terminal failed is not claimable: it must be requeued by an
      // administrative write first
      expect(retried).toBeNull();
      setTaskStatus(repo, 'L-8', 'pending');
      const again = claimTask(repo, 'L-8', 'w2')!;
      expect(again.leaseGeneration).toBe(gen + 1);
      expect(completeTask(repo, 'L-8', again.leaseGeneration!)!.lastError).toBeUndefined();
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('a legacy claimed task with no lease fields is reclaimable, not wedged', () => {
    const repo = tmpRepo();
    try {
      const clock = fakeClock(T0);
      enqueueTask(repo, { id: 'L-9', title: 'l', goal: 'Goal: l' });
      claimTask(repo, 'L-9', 'w1', { now: clock.now });
      // Rewrite the record as a pre-lease build left it: claimed, no lease.
      const legacy = { ...readTask(repo, 'L-9')!, leaseGeneration: undefined, leaseOwner: undefined, leaseExpiresAt: undefined };
      writeFileSync(join(queueDir(repo), 'L-9.json'), JSON.stringify(legacy, null, 2) + '\n');
      const reclaimed = claimTask(repo, 'L-9', 'w2', { now: clock.now })!;
      expect(reclaimed.leaseGeneration).toBe(1);
      expect(reclaimed.leaseOwner).toBe('w2');
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });
});

describe('queue: status updates + prune', () => {
  it('setTaskStatus failed records lastError, done clears it', () => {
    const repo = tmpRepo();
    try {
      enqueueTask(repo, { id: 'S-1', title: 's', goal: 'Goal: s' });
      claimTask(repo, 'S-1', 'w1');
      setTaskStatus(repo, 'S-1', 'failed', 'boom');
      expect(readTask(repo, 'S-1')!.lastError).toContain('boom');
      setTaskStatus(repo, 'S-1', 'done');
      expect(readTask(repo, 'S-1')!.status).toBe('done');
      expect(readTask(repo, 'S-1')!.lastError).toBeUndefined();
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('pruneDone removes done tasks', () => {
    const repo = tmpRepo();
    try {
      enqueueTask(repo, { id: 'P-1', title: 'p', goal: 'Goal: p' });
      claimTask(repo, 'P-1', 'w1');
      setTaskStatus(repo, 'P-1', 'done');
      expect(pruneDone(repo, 0)).toBe(1);
      expect(listTasks(repo)).toHaveLength(0);
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('taskCount reflects totals per status', () => {
    const repo = tmpRepo();
    try {
      enqueueTask(repo, { id: 'T-1', title: 'a', goal: 'Goal: a' });
      enqueueTask(repo, { id: 'T-2', title: 'b', goal: 'Goal: b' });
      claimTask(repo, 'T-1', 'w1');
      const c = taskCount(repo);
      expect(c.total).toBe(2);
      expect(c.pending).toBe(1);
      expect(c.claimed).toBe(1);
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('updateTask patches fields', () => {
    const repo = tmpRepo();
    try {
      enqueueTask(repo, { id: 'U-1', title: 'u', goal: 'Goal: u' });
      updateTask(repo, 'U-1', { title: 'uu' });
      expect(readTask(repo, 'U-1')!.title).toBe('uu');
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('returns empty/null gracefully when dirs missing', () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-empty-'));
    try {
      expect(listTasks(repo)).toEqual([]);
      expect(readTask(repo, 'nope')).toBeNull();
      expect(claimNextPending(repo, 'w1')).toBeNull();
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });
});
