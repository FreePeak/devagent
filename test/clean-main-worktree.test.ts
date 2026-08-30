import { describe, expect, it, afterAll } from 'vitest';
import { execFileSync } from 'node:child_process';
import { existsSync, mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { assertCleanMainWorktree, popStashBySha, stashMainWorktree } from '../src/git/worktree.js';

const dirs: string[] = [];
afterAll(() => {
  for (const d of dirs) rmSync(d, { recursive: true, force: true });
});

function initRepo(): string {
  const dir = mkdtempSync(join(tmpdir(), 'da-cleanmain-'));
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

function porcelain(repo: string): string {
  return execFileSync('git', ['status', '--porcelain'], { cwd: repo }).toString();
}

describe('stashMainWorktree', () => {
  it('returns null on a clean tree without creating a stash', async () => {
    const repo = initRepo();
    expect(await stashMainWorktree(repo, 'test')).toBeNull();
    expect(execFileSync('git', ['stash', 'list'], { cwd: repo }).toString()).toBe('');
  });

  it('stashes tracked and untracked changes and restores them on pop by SHA', async () => {
    const repo = initRepo();
    writeFileSync(join(repo, 'f.txt'), 'modified\n');
    writeFileSync(join(repo, 'untracked.txt'), 'new\n');
    expect(porcelain(repo)).not.toBe('');

    const sha = await stashMainWorktree(repo, 'devagent auto-stash before merge');
    expect(sha).toMatch(/^[0-9a-f]{40}$/);
    expect(porcelain(repo)).toBe('');

    expect(await popStashBySha(repo, sha!)).toBe(true);
    expect(readFileSync(join(repo, 'f.txt'), 'utf8')).toBe('modified\n');
    expect(readFileSync(join(repo, 'untracked.txt'), 'utf8')).toBe('new\n');
    // pop consumed the stash entry: no stash refs remain
    expect(execFileSync('git', ['stash', 'list'], { cwd: repo }).toString()).toBe('');
  });
});

describe('assertCleanMainWorktree', () => {
  it('resolves on a clean tree checked out on main', async () => {
    const repo = initRepo();
    await expect(assertCleanMainWorktree(repo, 'main')).resolves.toBeUndefined();
  });

  it('rejects with a diagnostic message on detached HEAD', async () => {
    const repo = initRepo();
    execFileSync('git', ['checkout', '--detach'], { cwd: repo });
    await expect(assertCleanMainWorktree(repo, 'main')).rejects.toThrow(
      'main worktree is in detached-HEAD state; refusing to merge',
    );
  });

  it('rejects with the branch name on a non-main branch', async () => {
    const repo = initRepo();
    execFileSync('git', ['checkout', '-b', 'feature'], { cwd: repo });
    await expect(assertCleanMainWorktree(repo, 'main')).rejects.toThrow(
      /main worktree is on branch feature, expected main; refusing to merge/,
    );
  });

  it('rejects with a porcelain preview when the tree is dirty', async () => {
    const repo = initRepo();
    writeFileSync(join(repo, 'f.txt'), 'dirty\n');
    await expect(assertCleanMainWorktree(repo, 'main')).rejects.toThrow(
      /main worktree has uncommitted changes; refusing to merge:\n\s*M f\.txt/,
    );
  });
});
