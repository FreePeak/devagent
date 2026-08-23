import { describe, expect, it, afterAll } from 'vitest';
import { execFileSync } from 'node:child_process';
import { existsSync, mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createWorktree } from '../src/git/worktree.js';

const dirs: string[] = [];
afterAll(() => {
  for (const d of dirs) rmSync(d, { recursive: true, force: true });
});

function initRepo(): string {
  const dir = mkdtempSync(join(tmpdir(), 'da-wt-'));
  dirs.push(dir);
  const repo = join(dir, 'repo');
  mkdirSync(repo);
  execFileSync('git', ['init'], { cwd: repo });
  execFileSync('git', ['config', 'user.email', 'test@example.com'], { cwd: repo });
  execFileSync('git', ['config', 'user.name', 'test'], { cwd: repo });
  writeFileSync(join(repo, 'f.txt'), 'x\n');
  execFileSync('git', ['add', '.'], { cwd: repo });
  execFileSync('git', ['commit', '-m', 'init'], { cwd: repo });
  return repo;
}

describe('createWorktree', () => {
  it('creates an isolated worktree and branch for a fresh ticket', async () => {
    const repo = initRepo();
    const info = await createWorktree(repo, 'ENG-1');
    try {
      expect(info).toEqual({
        worktreePath: `${repo}/.devagent-worktrees/ENG-1`,
        branch: 'devagent/ENG-1',
      });
      expect(existsSync(info.worktreePath)).toBe(true);
      const branch = execFileSync('git', ['rev-parse', '--abbrev-ref', 'HEAD'], {
        cwd: info.worktreePath,
      }).toString().trim();
      expect(branch).toBe('devagent/ENG-1');
    } finally {
      rmSync(`${repo}/.devagent-worktrees`, { recursive: true, force: true });
    }
  });

  it('reuses the existing worktree on re-run for the same ticket', async () => {
    const repo = initRepo();
    try {
      const first = await createWorktree(repo, 'ENG-2');
      // Simulate prior work landing on the branch between runs
      writeFileSync(join(first.worktreePath, 'progress.txt'), 'wip\n');
      execFileSync('git', ['add', '.'], { cwd: first.worktreePath });
      execFileSync('git', ['commit', '-m', 'wip'], { cwd: first.worktreePath });

      const second = await createWorktree(repo, 'ENG-2');

      expect(second).toEqual(first);
      // Prior work survives the re-run
      expect(existsSync(join(second.worktreePath, 'progress.txt'))).toBe(true);
    } finally {
      rmSync(`${repo}/.devagent-worktrees`, { recursive: true, force: true });
    }
  });

  it('attaches a new worktree to the existing branch when the dir was removed', async () => {
    const repo = initRepo();
    try {
      const first = await createWorktree(repo, 'ENG-3');
      execFileSync('git', ['worktree', 'remove', first.worktreePath], { cwd: repo });
      expect(existsSync(first.worktreePath)).toBe(false);

      const second = await createWorktree(repo, 'ENG-3');

      expect(second.branch).toBe('devagent/ENG-3');
      expect(second.worktreePath).toBe(first.worktreePath);
      expect(existsSync(second.worktreePath)).toBe(true);
      const branch = execFileSync('git', ['rev-parse', '--abbrev-ref', 'HEAD'], {
        cwd: second.worktreePath,
      }).toString().trim();
      expect(branch).toBe('devagent/ENG-3');
    } finally {
      rmSync(`${repo}/.devagent-worktrees`, { recursive: true, force: true });
    }
  });
});
