import { describe, expect, it, vi } from 'vitest';
import { mkdtempSync, rmSync, mkdirSync, writeFileSync, existsSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { enqueueTask } from '../src/queue.js';
import { bridgeQueueToBoard, bridgeIfQueued } from '../src/orchestrator/queue-bridge.js';
import type { QueuedTask } from '../src/queue.js';
import type { OrchestratorTask } from '../src/orchestrator/types.js';

function tmpRepo(): string {
  const d = mkdtempSync(join(tmpdir(), 'da-qb-'));
  return d;
}

describe('queue-bridge', () => {
  it('bridges a queued goal into a board via planner (idempotent)', async () => {
    const repo = tmpRepo();
    try {
      const q: QueuedTask = enqueueTask(repo, { id: 'SCOUT-2026-01-01-aaaa', title: 'Add audit trail', goal: 'Goal: add audit table', acceptanceCriteria: ['migration applies'] });
      const planner = async (goal: string): Promise<OrchestratorTask[]> => [
        { id: 'T1', title: 'Draft migration', prompt: 'create migration', dependsOn: [], status: 'pending', attempts: 0 },
        { id: 'T2', title: 'Add API', prompt: 'add endpoint', dependsOn: ['T1'], status: 'pending', attempts: 0 },
      ];
      const r = await bridgeQueueToBoard(q, { repoPath: repo, planner });
      expect(r.created).toBe(true);
      expect(existsSync(join(repo, '.devagent-project.json'))).toBe(true);
      const board = JSON.parse(readFileSync(join(repo, '.devagent-project.json'), 'utf8'));
      expect(board.tasks).toHaveLength(2);
      // source queue item retired so the builder lane never double-builds it
      const { readTask } = await import('../src/queue.js');
      expect(readTask(repo, q.id)!.status).toBe('done');

      // idemp: second call returns existing board without wiping tasks
      const r2 = await bridgeQueueToBoard(q, { repoPath: repo, planner });
      expect(r2.idempotent).toBe(true);
      expect(r2.tasksWritten).toBe(2);
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });

  it('falls back to single-task plan when planner fails', async () => {
    const repo = tmpRepo();
    try {
      const q: QueuedTask = enqueueTask(repo, { id: 'G-1', title: 'x', goal: 'Goal: do a tiny edit' });
      const r = await bridgeQueueToBoard(q, { repoPath: repo, planner: async () => null });
      expect(r.tasksWritten).toBe(1);
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });

  it('carries the prior archived board failure class onto a re-bridged goal (Q27)', async () => {
    const repo = tmpRepo();
    try {
      // A stuck board archived by orchestrate-loop (PR #68/#102 path): its goal is
      // the composed queued-goal string, and the interrupted task carries the
      // executor failure class (taskInterrupt post-mortem, scheduler.ts:302).
      const q: QueuedTask = enqueueTask(repo, { id: 'REBRIDGE-1', title: 'Add audit trail', goal: 'Goal: add audit table', acceptanceCriteria: ['migration applies'] });
      const goal = 'Goal: add audit table\nAcceptance criteria:\n- migration applies';
      const archiveDir = join(repo, '.devagent', 'archive');
      mkdirSync(archiveDir, { recursive: true });
      writeFileSync(
        join(archiveDir, 'board-stuck-20260903-120000.json'),
        JSON.stringify({
          goal,
          createdAt: '2026-09-03T04:00:00.000Z',
          updatedAt: '2026-09-03T05:00:00.000Z',
          roles: { planner: 'omp', executor: 'omp' },
          tasks: [
            {
              id: 'T1',
              title: 'Draft migration',
              prompt: 'create migration',
              dependsOn: [],
              status: 'failed',
              attempts: 3,
              interrupt: {
                failureClass: 'test-gate',
                lastGateExcerpt: '1 failed',
                attempts: 3,
                trailHash: 'abc123',
              },
            },
          ],
        }),
      );

      const planner = async (g: string): Promise<OrchestratorTask[]> => [
        { id: 'T1', title: 'Draft migration', prompt: 'create migration', dependsOn: [], status: 'pending', attempts: 0 },
      ];
      const r = await bridgeQueueToBoard(q, { repoPath: repo, planner });
      expect(r.created).toBe(true);
      const board = JSON.parse(readFileSync(join(repo, '.devagent-project.json'), 'utf8'));
      // Re-bridged goal carries the prior board's failure class instead of a
      // fresh attempt budget with no memory of why the last board died.
      expect(board.failureClass).toBe('test-gate');
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });

  it('leaves failureClass unset when no archived board matches the goal', async () => {
    const repo = tmpRepo();
    try {
      const q: QueuedTask = enqueueTask(repo, { id: 'FRESH-1', title: 'x', goal: 'Goal: brand new work' });
      const r = await bridgeQueueToBoard(q, { repoPath: repo, planner: async () => null });
      expect(r.created).toBe(true);
      const board = JSON.parse(readFileSync(join(repo, '.devagent-project.json'), 'utf8'));
      expect(board.failureClass).toBeUndefined();
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });
  it('bridgeIfQueued picks oldest pending and is no-op when no pending', async () => {
    const repo = tmpRepo();
    try {
      expect(await bridgeIfQueued(repo)).toBeNull();
      enqueueTask(repo, { id: 'A-1', title: 'first', goal: 'Goal: first' });
      enqueueTask(repo, { id: 'B-1', title: 'second', goal: 'Goal: second' });
      const r = await bridgeIfQueued(repo, async (g) => [
        { id: 'T1', title: g.slice(0, 30), prompt: g, dependsOn: [], status: 'pending', attempts: 0 },
      ]);
      expect(r!.tasksWritten).toBe(1);
      // board exists now, second bridgeIfQueued returns idempotent board, not a new goal
      const r2 = await bridgeIfQueued(repo);
      expect(r2!.idempotent).toBe(true);
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });
});
