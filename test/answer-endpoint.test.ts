import { afterEach, describe, expect, it } from 'vitest';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { applyAnswerToRepo, loadBoard } from '../src/orchestrator/store.js';

/**
 * POST /api/answer semantics (serve command). The HTTP layer is thin
 * (auth + JSON parse); these pin the status-code contract of the handler.
 * LangGraph interrupt/resume lessons: explicit taskId binding, clean
 * rejection on stale/duplicate decisions, no mutation without validation.
 */
describe('applyAnswerToRepo (POST /api/answer handler)', () => {
  const dirs: string[] = [];

  function repoWithAskTask(): string {
    const dir = mkdtempSync(join(tmpdir(), 'da-answer-'));
    dirs.push(dir);
    writeFileSync(
      join(dir, '.devagent-project.json'),
      JSON.stringify({
        goal: 'needs input',
        createdAt: '2026-08-24T00:00:00Z',
        updatedAt: '2026-08-24T01:00:00Z',
        roles: { planner: 'claude-code', executor: 'claude-code' },
        tasks: [
          {
            id: 'T1',
            title: 'blocked on human',
            prompt: 'p',
            dependsOn: [],
            status: 'ask',
            attempts: 1,
            failureDetail: 'needs human input: which DB should the migration target?',
          },
          { id: 'T2', title: 'done', prompt: 'p', dependsOn: [], status: 'done', attempts: 1 },
        ],
      }),
    );
    return dir;
  }

  afterEach(() => {
    for (const d of dirs) rmSync(d, { recursive: true, force: true });
    dirs.length = 0;
  });

  it('answers an ask task with 200 and persists the requeue', () => {
    const repo = repoWithAskTask();
    const r = applyAnswerToRepo(repo, 'T1', 'target the analytics replica');
    expect(r.status).toBe(200);
    expect(r.body.ok).toBe(true);
    // reload from disk: the answer context survived persistence
    const board = loadBoard(repo)!;
    expect(board.tasks[0]!.status).toBe('ready'); // readiness recomputed on save
    expect(board.tasks[0]!.prompt).toContain('analytics replica');
  });

  it('returns 400 for an empty answer before touching state', () => {
    const repo = repoWithAskTask();
    expect(applyAnswerToRepo(repo, 'T1', '   ').status).toBe(400);
  });

  it('returns 404 when there is no board', () => {
    const empty = mkdtempSync(join(tmpdir(), 'da-answer-empty-'));
    dirs.push(empty);
    const r = applyAnswerToRepo(empty, 'T1', 'yes');
    expect(r.status).toBe(404);
  });

  it('returns 409 for unknown ids and stale decisions (idempotent replays)', () => {
    const repo = repoWithAskTask();
    expect(applyAnswerToRepo(repo, 'TX', 'yes').status).toBe(409);
    expect(applyAnswerToRepo(repo, 'T2', 'yes').status).toBe(409); // not in ask
    // a successful decision, then a replayed one: conflicts instead of double-applying
    expect(applyAnswerToRepo(repo, 'T1', 'target analytics').status).toBe(200);
    expect(applyAnswerToRepo(repo, 'T1', 'again').status).toBe(409);
  });
});
