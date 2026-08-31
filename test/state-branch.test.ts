import { describe, expect, it, afterAll } from 'vitest';
import { execFileSync } from 'node:child_process';
import { existsSync, mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { ensureStateBranch } from '../src/git/state-branch.js';

const dirs: string[] = [];
afterAll(() => {
  for (const d of dirs) rmSync(d, { recursive: true, force: true });
});

/**
 * Fixture: a temp bare repo as the remote, plus a work repo with one
 * initial commit on main so HEAD exists. All pushes go to the local bare
 * path — no network.
 */
interface Fixture {
  repo: string;
  bare: string;
  git(args: string[], cwd?: string): string;
}
function initFixture(): Fixture {
  const dir = mkdtempSync(join(tmpdir(), 'da-statebranch-'));
  dirs.push(dir);
  const repo = join(dir, 'repo');
  const bare = join(dir, 'remote.git');
  mkdirSync(repo);
  execFileSync('git', ['init', '--bare', '-b', 'main', bare]);
  execFileSync('git', ['init', '-b', 'main'], { cwd: repo });
  execFileSync('git', ['config', 'user.email', 'test@example.com'], { cwd: repo });
  execFileSync('git', ['config', 'user.name', 'test'], { cwd: repo });
  execFileSync('git', ['remote', 'add', 'origin', bare], { cwd: repo });
  writeFileSync(join(repo, 'f.txt'), 'x\n');
  execFileSync('git', ['add', '.'], { cwd: repo });
  execFileSync('git', ['commit', '-m', 'init'], { cwd: repo });

  const git = (args: string[], cwd: string = repo): string =>
    execFileSync('git', args, { cwd }).toString();
  return { repo, bare, git };
}

function lsRemoteStateRef(f: Fixture): string {
  return f.git(['ls-remote', '--heads', 'origin', 'selfbuild/state']);
}

describe('ensureStateBranch', () => {
  it('creates the orphan branch on the remote seeded with local lessons content', async () => {
    const f = initFixture();
    const lessons = 'lesson one: verify before claiming completion\n';
    mkdirSync(join(f.repo, '.devagent'));
    writeFileSync(join(f.repo, '.devagent', 'lessons.md'), lessons);
    // Track the lessons file so the worktree starts clean and the
    // status-unchanged assertion below is meaningful.
    f.git(['add', '.devagent']);
    f.git(['commit', '-m', 'lessons']);

    const r = await ensureStateBranch(f.repo);
    expect(r).toEqual({ action: 'created' });

    // Ref present on the remote.
    const ls = lsRemoteStateRef(f);
    expect(ls).toContain('refs/heads/selfbuild/state');
    const tip = ls.trim().split(/\s+/)[0]!;

    // Branch contains the lessons file with the local content.
    expect(f.git(['--git-dir', f.bare, 'show', `refs/heads/selfbuild/state:.devagent/lessons.md`])).toBe(lessons);

    // Commit is parentless (orphan): rev-list --parents -n1 has exactly one token.
    const parents = f.git(['rev-list', '--parents', '-n', '1', tip]).trim();
    expect(parents.split(/\s+/)).toHaveLength(1);
  });

  it('leaves HEAD and the worktree untouched', async () => {
    const f = initFixture();
    const lessons = 'lesson one: verify before claiming completion\n';
    mkdirSync(join(f.repo, '.devagent'));
    writeFileSync(join(f.repo, '.devagent', 'lessons.md'), lessons);
    f.git(['add', '.devagent']);
    f.git(['commit', '-m', 'lessons']);

    const headBefore = f.git(['rev-parse', 'HEAD']).trim();
    const statusBefore = f.git(['status', '--porcelain']);
    expect(statusBefore).toBe('');

    const r = await ensureStateBranch(f.repo);
    expect(r).toEqual({ action: 'created' });

    // Work repo untouched.
    expect(f.git(['rev-parse', 'HEAD']).trim()).toBe(headBefore);
    expect(f.git(['status', '--porcelain'])).toBe(statusBefore);
  });

  it('creates the branch with an empty lessons.md when no local lessons file exists', async () => {
    const f = initFixture();
    expect(existsSync(join(f.repo, '.devagent', 'lessons.md'))).toBe(false);

    const r = await ensureStateBranch(f.repo);
    expect(r).toEqual({ action: 'created' });

    expect(lsRemoteStateRef(f)).toContain('refs/heads/selfbuild/state');
    expect(f.git(['--git-dir', f.bare, 'show', `refs/heads/selfbuild/state:.devagent/lessons.md`])).toBe('');
  });

  it('is a no-op when the remote branch already exists', async () => {
    const f = initFixture();
    mkdirSync(join(f.repo, '.devagent'));
    writeFileSync(join(f.repo, '.devagent', 'lessons.md'), 'seed\n');
    await ensureStateBranch(f.repo);
    const tipBefore = f
      .git(['--git-dir', f.bare, 'rev-parse', 'refs/heads/selfbuild/state'])
      .trim();

    const r = await ensureStateBranch(f.repo);
    expect(r).toEqual({ action: 'exists' });
    expect(f.git(['--git-dir', f.bare, 'rev-parse', 'refs/heads/selfbuild/state']).trim()).toBe(tipBefore);
  });
});
