import { describe, expect, it } from 'vitest';
import { applyHumanAnswer, createBoard, formatPlanOnly, loadBoard, saveBoard } from '../src/orchestrator/store.js';
import type { ProjectBoard } from '../src/orchestrator/types.js';
import { existsSync, mkdtempSync, rmSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

function tempRepo(): string {
  return mkdtempSync(join(tmpdir(), 'da-orch-'));
}

describe('board store', () => {
  it('saves and loads round-trip with readiness recomputed', () => {
    const repo = tempRepo();
    try {
      saveBoard(
        repo,
        createBoard(
          'goal',
          [
            { id: 'T1', title: 'a', prompt: 'p', dependsOn: [], status: 'done', attempts: 1 },
            { id: 'T2', title: 'b', prompt: 'q', dependsOn: ['T1'], status: 'pending', attempts: 0 },
          ],
          { planner: 'claude-code', executor: 'opencode' },
        ),
      );
      const loaded = loadBoard(repo)!;
      expect(loaded.goal).toBe('goal');
      // T2 was pending with done dep -> saved as ready
      expect(loaded.tasks[1]!.status).toBe('ready');
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });

  it('returns null for missing or corrupted boards', () => {
    expect(loadBoard(tempRepo())).toBeNull();
    const repo = tempRepo();
    try {
      writeFileSync(join(repo, '.devagent-project.json'), '{corrupt');
      expect(loadBoard(repo)).toBeNull();
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });

  it('writes atomically: no .tmp file left behind', () => {
    const repo = tempRepo();
    try {
      saveBoard(repo, createBoard('g', [], { planner: 'claude-code', executor: 'opencode' }));
      const raw = readFileSync(join(repo, '.devagent-project.json'), 'utf8');
      expect((JSON.parse(raw) as { goal: string }).goal).toBe('g');
      expect(existsSync(join(repo, '.devagent-project.json.tmp'))).toBe(false);
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });
});

describe('formatPlanOnly (orchestrate --plan-only)', () => {
  it('renders full contracts with criteria and constraints for review', () => {
    const board = createBoard('ship export button', [
      {
        id: 'T1',
        title: 'schema migration',
        prompt: 'add the exports table',
        acceptanceCriteria: ['migrations/001_exports.up.sql exists', 'down migration pairs'],
        boundaryConstraints: ['do not touch auth schema'],
        dependsOn: [],
        status: 'pending',
        attempts: 0,
      },
    ], { planner: 'claude-code', executor: 'claude-code' });
    const out = formatPlanOnly(board);
    expect(out).toContain('[T1] schema migration (pending)');
    expect(out).toContain('add the exports table');
    expect(out).toContain('criteria:');
    expect(out).toContain('    migrations/001_exports.up.sql exists');
    expect(out).toContain('constraints:');
    expect(out).toContain('    do not touch auth schema');
  });

  it('omits empty sections instead of printing headers with nothing under them', () => {
    const board = createBoard('g', [
      { id: 'T1', title: 't', prompt: 'p', dependsOn: [], status: 'pending', attempts: 0 },
    ], { planner: 'claude-code', executor: 'claude-code' });
    const out = formatPlanOnly(board);
    expect(out).not.toContain('criteria:');
    expect(out).not.toContain('constraints:');
  });
});

describe('applyHumanAnswer', () => {
  function askBoard(): ProjectBoard {
    return {
      goal: 'g',
      createdAt: '2026-08-24T00:00:00Z',
      updatedAt: '2026-08-24T00:00:00Z',
      roles: { planner: 'claude-code', executor: 'claude-code' },
      tasks: [
        {
          id: 'T1',
          title: 'paused',
          prompt: 'original contract',
          dependsOn: [],
          status: 'ask',
          attempts: 1,
          failureDetail: 'needs human input: which API key env var?',
        },
        { id: 'T2', title: 'done', prompt: 'p', dependsOn: [], status: 'done', attempts: 1 },
      ],
    };
  }

  it('folds the answer into the contract and requeues the task', () => {
    const b = askBoard();
    const r = applyHumanAnswer(b, 'T1', 'use ORG_API_KEY');
    expect(r.ok).toBe(true);
    expect(b.tasks[0]!.status).toBe('pending');
    expect(b.tasks[0]!.prompt).toContain('use ORG_API_KEY');
    expect(b.tasks[0]!.prompt).toContain('which API key env var?'); // question kept as context
    expect(b.tasks[0]!.evidenceGaps).toBeUndefined();
  });

  it('rejects unknown ids, non-ask tasks, and empty answers', () => {
    expect(applyHumanAnswer(askBoard(), 'TX', 'yes').ok).toBe(false);
    expect(applyHumanAnswer(askBoard(), 'T2', 'yes').ok).toBe(false);
    expect(applyHumanAnswer(askBoard(), 'T1', '   ').ok).toBe(false);
  });
});
