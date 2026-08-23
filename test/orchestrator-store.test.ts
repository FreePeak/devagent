import { describe, expect, it } from 'vitest';
import { createBoard, loadBoard, saveBoard } from '../src/orchestrator/store.js';
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
