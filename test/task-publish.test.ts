import { describe, expect, it, afterAll } from 'vitest';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { RunLogger } from '../src/logger.js';
import { commitAllChanges, createWorktree, currentBranch, listChangedFiles } from '../src/git/worktree.js';
import { publishTaskBranch, type TaskPublishDeps, type TaskPublishOptions } from '../src/task.js';

const dirs: string[] = [];
afterAll(() => {
  for (const d of dirs) rmSync(d, { recursive: true, force: true });
});

function g(repo: string, ...args: string[]): string {
  return execFileSync('git', args, { cwd: repo }).toString().trim();
}

/** Real repo + real worktree on branch devagent/TASK, mirroring task mode. */
function fixture(): { repo: string; wt: string } {
  const repo = mkdtempSync(join(tmpdir(), 'da-publish-'));
  dirs.push(repo);
  g(repo, 'init', '-b', 'main');
  g(repo, 'config', 'user.email', 't@t');
  g(repo, 'config', 'user.name', 't');
  writeFileSync(join(repo, 'README.md'), 'base\n');
  g(repo, 'add', '-A');
  g(repo, 'commit', '-m', 'init');
  const wtInfo = createWorktreeSync(repo);
  return { repo, wt: wtInfo };
}

function createWorktreeSync(repo: string): string {
  const wt = join(repo, '.devagent-worktrees', 'TASK');
  g(repo, 'worktree', 'add', '-b', 'devagent/TASK', wt);
  return wt;
}

interface Captured {
  pushed?: string;
  pr?: { repoPath: string; branch: string; title: string };
  changedListed?: boolean;
}

/** Real git helpers, fake remote boundary — mirrors the production wiring. */
function makeDeps(capture: Captured): TaskPublishDeps {
  return {
    commitAllChanges,
    currentBranch,
    listChangedFiles,
    pushBranch: async (_repoPath, branch) => {
      capture.pushed = branch;
    },
    createPr: async (o) => {
      capture.pr = o;
      return `https://example/pr/${o.branch}`;
    },
  };
}

function makeOpts(repo: string, prompt = 'Fix the thing'): TaskPublishOptions {
  return { repoPath: repo, prompt, baseBranch: 'main', log: new RunLogger() };
}

describe('publishTaskBranch', () => {
  it('commits uncommitted worker output and pushes the ACTUAL worktree branch', async () => {
    const { repo, wt } = fixture();
    // Worker edited files but never committed — the dogfood failure mode.
    writeFileSync(join(wt, 'feature.txt'), 'hello\n');
    const capture: Captured = {};

    const url = await publishTaskBranch(makeOpts(repo), { ok: true, worker: 'claude-code', attempts: 1, worktreePath: wt }, makeDeps(capture));

    expect(url).toContain('https://example/pr/');
    // Ground truth branch name, NOT an invented devagent/task-<runId> refspec
    expect(capture.pushed).toBe('devagent/TASK');
    expect(capture.pr?.branch).toBe('devagent/TASK');
    // Nothing left uncommitted in the worktree
    expect(g(wt, 'status', '--porcelain')).toBe('');
    // And the pushed branch actually contains the new file
    expect(g(wt, 'show', 'devagent/TASK:feature.txt')).toBe('hello');
  });

  it('skips publishing entirely when the diff vs base is empty (no empty PRs)', async () => {
    const { repo, wt } = fixture();
    const capture: Captured = {};

    const url = await publishTaskBranch(makeOpts(repo), { ok: true, worker: 'claude-code', attempts: 1, worktreePath: wt }, makeDeps(capture));

    expect(url).toBeUndefined();
    expect(capture.pushed).toBeUndefined();
    expect(capture.pr).toBeUndefined();
  });

  it('publishes even when the tree is clean but the branch carries commits', async () => {
    const { repo, wt } = fixture();
    writeFileSync(join(wt, 'committed.txt'), 'x\n');
    await commitAllChanges(wt, 'worker already committed');
    const capture: Captured = {};

    const url = await publishTaskBranch(makeOpts(repo), { ok: true, worker: 'claude-code', attempts: 1, worktreePath: wt }, makeDeps(capture));

    expect(url).toBeDefined();
    expect(capture.pushed).toBe('devagent/TASK');
  });

  it('reports missing worktree as unpublishable without throwing', async () => {
    const capture: Captured = {};
    const url = await publishTaskBranch(makeOpts('.'), { ok: true, worker: 'c', attempts: 1 }, makeDeps(capture));
    expect(url).toBeUndefined();
  });
  // regression: loop 57-58 failure. cleanup=auto removed the worktree after
  // auto-cleanup snapshotted the changes onto devagent/TASK. Publishing must
  // then run from the main repo against that branch, not fail with
  // "git add -A exited -1" from a nonexistent cwd.
  it('publishes from the run branch after auto-cleanup removed the worktree', async () => {
    const { repo, wt } = fixture();
    writeFileSync(join(wt, 'feature.txt'), 'fix\n');
    await commitAllChanges(wt, 'worker changes');
    // simulate finalizeRunWorktree: remove the worktree registration + dir,
    // the branch devagent/TASK survives with the snapshot commit
    g(repo, 'worktree', 'remove', '--force', wt);
    g(repo, 'worktree', 'prune');
    const capture: Captured = {};

    const url = await publishTaskBranch(
      makeOpts(repo),
      { ok: true, worker: 'claude-code', attempts: 1, worktreePath: wt },
      makeDeps(capture),
    );

    expect(url).toBeDefined();
    expect(capture.pushed).toBe('devagent/TASK');
    expect(capture.pr?.branch).toBe('devagent/TASK');
  });
});

describe('currentBranch', () => {
  it('returns the checked-out branch of a worktree', async () => {
    const { wt } = fixture();
    expect(await currentBranch(wt)).toBe('devagent/TASK');
  });

  it('throws a clear error on a detached HEAD instead of returning HEAD', async () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-detach-'));
    dirs.push(repo);
    g(repo, 'init', '-b', 'main');
    g(repo, 'config', 'user.email', 't@t');
    g(repo, 'config', 'user.name', 't');
    writeFileSync(join(repo, 'f.txt'), '1\n');
    g(repo, 'add', '-A');
    g(repo, 'commit', '-m', 'one');
    g(repo, 'checkout', '--detach');
    await expect(currentBranch(repo)).rejects.toThrow(/detached/i);
  });
});

describe('listChangedFiles (publish evidence)', () => {
  it('lists only files changed on the branch vs merge-base', async () => {
    const { repo, wt } = fixture();
    writeFileSync(join(wt, 'a.txt'), 'a\n');
    await commitAllChanges(wt, 'change');
    const files = await listChangedFiles(wt, 'main');
    expect(files).toContain('a.txt');
    expect(files).not.toContain('README.md');
  });
});
