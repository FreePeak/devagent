import { afterAll, describe, expect, it } from 'vitest';
import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { LEDGER_DIR } from '../src/orchestrator/ledger.js';
import { perTaskPrPublished, restoreAutoStash, topoOrder } from '../src/orchestrator/merge.js';
import { stashMainWorktree } from '../src/git/worktree.js';
import type { ProjectBoard } from '../src/orchestrator/types.js';

function board(tasks: ProjectBoard['tasks']): ProjectBoard {
  return { goal: 'g', createdAt: '', updatedAt: '', roles: { planner: 'claude-code', executor: 'opencode' }, tasks };
}

describe('topoOrder', () => {
  it('emits dependencies before dependents', () => {
    const b = board([
      { id: 'T3', title: '', prompt: '', dependsOn: ['T1', 'T2'], status: 'done', attempts: 1 },
      { id: 'T1', title: '', prompt: '', dependsOn: [], status: 'done', attempts: 1 },
      { id: 'T2', title: '', prompt: '', dependsOn: ['T1'], status: 'done', attempts: 2 },
    ]);
    const order = topoOrder(b);
    expect(order.indexOf('T1')).toBeLessThan(order.indexOf('T2'));
    expect(order.indexOf('T2')).toBeLessThan(order.indexOf('T3'));
  });

  it('includes only done tasks in merge candidates when filtered by caller', () => {
    // mergeProjectBranches filters to done; topoOrder covers all — contract check
    const b = board([
      { id: 'A', title: '', prompt: '', dependsOn: [], status: 'done', attempts: 1 },
      { id: 'B', title: '', prompt: '', dependsOn: ['A'], status: 'failed', attempts: 1 },
    ]);
    const done = new Set(b.tasks.filter((t) => t.status === 'done').map((t) => t.id));
    expect(topoOrder(b).filter((id) => done.has(id))).toEqual(['A']);
  });
});

describe('perTaskPrPublished (legacy merge-back gate, PRD Q20)', () => {
  it('is true when a done task published its per-task PR', () => {
    const b = board([{ id: 'T1', title: '', prompt: '', dependsOn: [], status: 'done', attempts: 1, prUrl: 'https://github.com/x/y/pull/7' }]);
    expect(perTaskPrPublished(b)).toBe(true);
  });

  it('is false when no done task has a PR URL (legacy merge-back still owns integration)', () => {
    const b = board([{ id: 'T1', title: '', prompt: '', dependsOn: [], status: 'done', attempts: 1 }]);
    expect(perTaskPrPublished(b)).toBe(false);
  });

  it('ignores empty PR URLs', () => {
    const b = board([{ id: 'T1', title: '', prompt: '', dependsOn: [], status: 'done', attempts: 1, prUrl: '' }]);
    expect(perTaskPrPublished(b)).toBe(false);
  });

  it('ignores PR URLs on tasks that are not done', () => {
    const b = board([
      { id: 'T1', title: '', prompt: '', dependsOn: [], status: 'failed', attempts: 1, prUrl: 'https://github.com/x/y/pull/7' },
      { id: 'T2', title: '', prompt: '', dependsOn: [], status: 'blocked', attempts: 1 },
    ]);
    expect(perTaskPrPublished(b)).toBe(false);
  });

  it('is false for an empty board', () => {
    expect(perTaskPrPublished(board([]))).toBe(false);
  });
});

describe('restoreAutoStash (Q26 merge-back auto-stash ledger warnings)', () => {
  const dirs: string[] = [];
  afterAll(() => {
    for (const d of dirs) rmSync(d, { recursive: true, force: true });
  });

  function initRepo(): string {
    const dir = mkdtempSync(join(tmpdir(), 'da-merge-stash-'));
    dirs.push(dir);
    const repo = join(dir, 'repo');
    mkdirSync(repo);
    execFileSync('git', ['init', '-b', 'main'], { cwd: repo });
    execFileSync('git', ['config', 'user.email', 'test@example.com'], { cwd: repo });
    execFileSync('git', ['config', 'user.name', 'test'], { cwd: repo });
    writeFileSync(join(repo, 'f.txt'), 'x\n');
    execFileSync('git', ['add', '.'], { cwd: repo });
    execFileSync('git', ['commit', '-m', 'init'], { cwd: repo });
    return repo;
  }

  function stashRows(repo: string): Array<Record<string, unknown>> {
    const file = join(repo, LEDGER_DIR, 'events.jsonl');
    if (!existsSync(file)) return [];
    return readFileSync(file, 'utf8')
      .trim()
      .split('\n')
      .filter(Boolean)
      .map((l) => JSON.parse(l) as Record<string, unknown>)
      .filter((r) => r.event === 'merge-back-stash');
  }

  it('emits a restored row when the stash pops back cleanly', async () => {
    const repo = initRepo();
    writeFileSync(join(repo, 'f.txt'), 'local work\n');
    const sha = (await stashMainWorktree(repo, 'devagent auto-stash before merge'))!;

    expect(await restoreAutoStash(repo, sha)).toBe(true);
    expect(readFileSync(join(repo, 'f.txt'), 'utf8')).toBe('local work\n');

    const rows = stashRows(repo);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({
      kind: 'event',
      event: 'merge-back-stash',
      taskId: 'merge-back',
      attempt: 1,
      stashSha: sha,
      outcome: 'restored',
    });
    expect(typeof rows[0]!.ts).toBe('string');
    expect(typeof rows[0]!.detail).toBe('string');
  });

  it('emits a retained row and keeps the stash when the pop conflicts', async () => {
    const repo = initRepo();
    writeFileSync(join(repo, 'f.txt'), 'local work\n');
    const sha = (await stashMainWorktree(repo, 'devagent auto-stash before merge'))!;
    // Merge-back lands a conflicting change to the same file: apply fails.
    writeFileSync(join(repo, 'f.txt'), 'merged work\n');
    execFileSync('git', ['add', '.'], { cwd: repo });
    execFileSync('git', ['commit', '-m', 'merge-back'], { cwd: repo });

    expect(await restoreAutoStash(repo, sha)).toBe(false);
    // User work is never dropped: the stash entry survives for manual recovery
    expect(execFileSync('git', ['stash', 'list', '--format=%H'], { cwd: repo }).toString().trim()).toBe(sha);

    const rows = stashRows(repo);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({
      kind: 'event',
      event: 'merge-back-stash',
      taskId: 'merge-back',
      attempt: 1,
      stashSha: sha,
      outcome: 'retained',
    });
  });
});
