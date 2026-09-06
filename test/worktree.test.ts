import { describe, expect, it, afterAll } from 'vitest';
import { execFileSync } from 'node:child_process';
import { existsSync, mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { commitAllChanges, createWorktree, deleteBranch, finalizeRunWorktree, renameCurrentBranch } from '../src/git/worktree.js';

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

/**
 * Repo wired to a local bare `origin` over the file transport: exercises the
 * real push path (snapshot-then-push) without touching the network.
 */
function initRepoWithRemote(): { repo: string; remoteDir: string; dir: string } {
  const dir = mkdtempSync(join(tmpdir(), 'da-wt-push-'));
  dirs.push(dir);
  const remoteDir = join(dir, 'origin.git');
  execFileSync('git', ['init', '--bare', remoteDir]);
  const repo = join(dir, 'repo');
  mkdirSync(repo);
  execFileSync('git', ['init'], { cwd: repo });
  execFileSync('git', ['config', 'user.email', 'test@example.com'], { cwd: repo });
  execFileSync('git', ['config', 'user.name', 'test'], { cwd: repo });
  writeFileSync(join(repo, 'f.txt'), 'x\n');
  execFileSync('git', ['add', '.'], { cwd: repo });
  execFileSync('git', ['commit', '-m', 'init'], { cwd: repo });
  execFileSync('git', ['remote', 'add', 'origin', remoteDir], { cwd: repo });
  return { repo, remoteDir, dir };
}

/** Tip of a branch in the bare remote ('' when the remote never saw it). */
function remoteTip(remoteDir: string, branch: string): string {
  try {
    return execFileSync('git', ['--git-dir', remoteDir, 'rev-parse', '--verify', `refs/heads/${branch}`], {
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    }).trim();
  } catch {
    return '';
  }
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

describe('commitAllChanges / renameCurrentBranch / deleteBranch (merge-assist primitives)', () => {
  it('commits untracked and modified files with --no-verify', async () => {
    const repo = initRepo();
    const wt = await createWorktree(repo, 'ENG-9');
    try {
      writeFileSync(join(wt.worktreePath, 'new.txt'), 'added\n');
      writeFileSync(join(wt.worktreePath, 'f.txt'), 'edited\n');

      const created = await commitAllChanges(wt.worktreePath, 'wip commit');

      expect(created).toBe(true);
      const status = execFileSync('git', ['status', '--porcelain'], { cwd: wt.worktreePath }).toString();
      expect(status).toBe('');
      const subject = execFileSync('git', ['log', '-1', '--format=%s'], { cwd: wt.worktreePath }).toString().trim();
      expect(subject).toBe('wip commit');
    } finally {
      rmSync(`${repo}/.devagent-worktrees`, { recursive: true, force: true });
    }
  });

  it('tolerates nothing-to-commit on a clean tree', async () => {
    const repo = initRepo();
    const wt = await createWorktree(repo, 'ENG-11');
    try {
      const created = await commitAllChanges(wt.worktreePath, 'should not appear');
      expect(created).toBe(false);
      const subject = execFileSync('git', ['log', '-1', '--format=%s'], { cwd: wt.worktreePath }).toString().trim();
      expect(subject).toBe('init');
    } finally {
      rmSync(`${repo}/.devagent-worktrees`, { recursive: true, force: true });
    }
  });

  it('renames the checked-out branch and deletes branches best-effort', async () => {
    const repo = initRepo();
    const wt = await createWorktree(repo, 'ENG-12');
    try {
      await renameCurrentBranch(wt.worktreePath, 'devagent/ENG-12-canonical');
      const current = execFileSync('git', ['rev-parse', '--abbrev-ref', 'HEAD'], {
        cwd: wt.worktreePath,
      }).toString().trim();
      expect(current).toBe('devagent/ENG-12-canonical');

      // Checked-out branch cannot be deleted from the repo root: must not throw
      await deleteBranch(repo, 'devagent/ENG-12-canonical');
      execFileSync('git', ['branch', 'spare'], { cwd: repo });
      await deleteBranch(repo, 'spare');
      const branches = execFileSync('git', ['branch', '--list'], { cwd: repo }).toString();
      expect(branches).not.toContain('spare');
      // Unknown branch: swallowed
      await deleteBranch(repo, 'nope');
    } finally {
      rmSync(`${repo}/.devagent-worktrees`, { recursive: true, force: true });
    }
  });
});

describe('finalizeRunWorktree snapshot-then-push (PRD §18 Q29)', () => {
  it('pushes the snapshot to the remote before the worktree dies', async () => {
    const { repo, remoteDir } = initRepoWithRemote();
    const wt = await createWorktree(repo, 'PUSH-1');
    writeFileSync(join(wt.worktreePath, 'wip.txt'), 'uncommitted worker output\n');

    const fin = await finalizeRunWorktree({
      repoPath: repo,
      worktreePath: wt.worktreePath,
      ticketId: 'PUSH-1',
      mode: 'remove',
    });

    expect(fin).toEqual({ action: 'removed', committed: true, pushed: true });
    expect(existsSync(wt.worktreePath)).toBe(false);
    // Remote persistence precedes removal: the remote holds exactly the
    // snapshot commit, not a stale pre-run tip.
    expect(remoteTip(remoteDir, wt.branch)).toBe(
      execFileSync('git', ['rev-parse', '--verify', wt.branch], { cwd: repo, encoding: 'utf8' }).trim(),
    );
    const subject = execFileSync('git', ['--git-dir', remoteDir, 'log', '-1', '--format=%s', wt.branch], {
      encoding: 'utf8',
    }).trim();
    expect(subject).toContain('auto-cleanup snapshot');
    // publish's later push is idempotent: same refspec, nothing to update
    execFileSync('git', ['push', '-u', 'origin', `${wt.branch}:${wt.branch}`], { cwd: repo });
    expect(remoteTip(remoteDir, wt.branch)).toBe(
      execFileSync('git', ['rev-parse', '--verify', wt.branch], { cwd: repo, encoding: 'utf8' }).trim(),
    );
  });

  it('holds the worktree when the push fails, so no snapshot is stranded', async () => {
    const { repo, dir } = initRepoWithRemote();
    const wt = await createWorktree(repo, 'PUSH-2');
    writeFileSync(join(wt.worktreePath, 'wip.txt'), 'uncommitted worker output\n');
    // Unreachable origin: the exact failure class publish swallows as non-fatal
    execFileSync('git', ['remote', 'set-url', 'origin', join(dir, 'missing.git')], { cwd: repo });

    const fin = await finalizeRunWorktree({
      repoPath: repo,
      worktreePath: wt.worktreePath,
      ticketId: 'PUSH-2',
      mode: 'remove',
    });

    expect(fin.action).toBe('preserved');
    expect(fin.committed).toBe(true);
    expect(fin.pushed).toBe(false);
    expect(fin.error).toContain('not pushed');
    expect(remoteTip(join(dir, 'origin.git'), wt.branch)).toBe('');
    // Still recoverable: the tree and its output survive the failed push
    expect(existsSync(wt.worktreePath)).toBe(true);
    expect(readFileSync(join(wt.worktreePath, 'wip.txt'), 'utf8')).toContain('uncommitted');
  });

  it('persists worker commits made before cleanup on an otherwise clean tree', async () => {
    const { repo, remoteDir } = initRepoWithRemote();
    const wt = await createWorktree(repo, 'PUSH-3');
    writeFileSync(join(wt.worktreePath, 'work.txt'), 'committed by the worker\n');
    await commitAllChanges(wt.worktreePath, 'worker commit');

    const fin = await finalizeRunWorktree({
      repoPath: repo,
      worktreePath: wt.worktreePath,
      ticketId: 'PUSH-3',
      mode: 'remove',
    });

    // Nothing left to snapshot, yet the unpushed branch is still persisted
    expect(fin).toEqual({ action: 'removed', committed: false, pushed: true });
    expect(remoteTip(remoteDir, wt.branch)).not.toBe('');
    expect(existsSync(wt.worktreePath)).toBe(false);
  });

  it('skips the push and still removes on a repo with no remote configured', async () => {
    const repo = initRepo();
    const wt = await createWorktree(repo, 'PUSH-4');
    writeFileSync(join(wt.worktreePath, 'wip.txt'), 'local-only work\n');

    const fin = await finalizeRunWorktree({
      repoPath: repo,
      worktreePath: wt.worktreePath,
      ticketId: 'PUSH-4',
      mode: 'remove',
    });

    expect(fin).toEqual({ action: 'removed', committed: true, pushed: false });
    expect(fin.error).toBeUndefined();
    expect(existsSync(wt.worktreePath)).toBe(false);
  });
});
