import { describe, expect, it, beforeEach } from 'vitest';
import { mkdtempSync, rmSync, existsSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { enqueueTask, listTasks, claimTask, claimNextPending, updateTask, setTaskStatus, readTask, writePrd, readPrd, pruneDone, taskCount, ensureQueueDirs, queueDir } from '../src/queue.js';

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
