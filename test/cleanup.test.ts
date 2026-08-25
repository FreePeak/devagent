import { describe, expect, it, afterAll } from 'vitest';
import { execFileSync } from 'node:child_process';
import { existsSync, mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createWorktree, finalizeRunWorktree } from '../src/git/worktree.js';
import {
  matchOrcaWorktree,
  findOrcaWorktreeByPath,
  dropOrcaWorkspace,
} from '../src/integrations/orca.js';
import { loadConfig } from '../src/config.js';

const dirs: string[] = [];
afterAll(() => {
  for (const d of dirs) rmSync(d, { recursive: true, force: true });
});

function initRepo(): string {
  const dir = mkdtempSync(join(tmpdir(), 'da-clean-'));
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

function branchExists(repo: string, branch: string): boolean {
  const out = execFileSync('git', ['branch', '--list', branch], { cwd: repo }).toString();
  return out.includes(branch);
}

describe('finalizeRunWorktree (auto-cleanup stage)', () => {
  it('remove mode snapshots dirty changes onto the branch, then removes the tree', async () => {
    const repo = initRepo();
    const wt = await createWorktree(repo, 'CLEAN-1');
    writeFileSync(join(wt.worktreePath, 'wip.txt'), 'uncommitted worker output\n');

    const fin = await finalizeRunWorktree({
      repoPath: repo,
      worktreePath: wt.worktreePath,
      ticketId: 'CLEAN-1',
      mode: 'remove',
    });

    expect(fin.action).toBe('removed');
    expect(fin.committed).toBe(true);
    expect(fin.error).toBeUndefined();
    // Directory gone...
    expect(existsSync(wt.worktreePath)).toBe(false);
    // ...but nothing was lost: branch kept with the snapshot commit
    expect(branchExists(repo, wt.branch)).toBe(true);
    const subject = execFileSync('git', ['log', '-1', '--format=%s', wt.branch], { cwd: repo })
      .toString()
      .trim();
    expect(subject).toContain('auto-cleanup snapshot');
  });

  it('remove mode with a clean tree skips the snapshot commit but still removes', async () => {
    const repo = initRepo();
    const wt = await createWorktree(repo, 'CLEAN-2');

    const fin = await finalizeRunWorktree({
      repoPath: repo,
      worktreePath: wt.worktreePath,
      ticketId: 'CLEAN-2',
      mode: 'remove',
    });

    expect(fin.action).toBe('removed');
    expect(fin.committed).toBe(false);
    expect(existsSync(wt.worktreePath)).toBe(false);
  });

  it('preserve mode leaves the tree untouched (failure debugging)', async () => {
    const repo = initRepo();
    const wt = await createWorktree(repo, 'CLEAN-3');
    writeFileSync(join(wt.worktreePath, 'wip.txt'), 'broken state\n');

    const fin = await finalizeRunWorktree({
      repoPath: repo,
      worktreePath: wt.worktreePath,
      ticketId: 'CLEAN-3',
      mode: 'preserve',
    });

    expect(fin).toEqual({ action: 'preserved', committed: false });
    expect(existsSync(wt.worktreePath)).toBe(true);
  });
});

describe('orca workspace integration', () => {
  const psPayload = {
    ok: true,
    result: {
      worktrees: [
        { id: 'repo-1::/Users/me/orca/other', path: '/Users/me/orca/other' },
        { id: 'repo-2::/Users/me/orca/hackathon-c3', path: '/Users/me/orca/hackathon-c3/' },
      ],
    },
  };

  it('matches a repoPath against orca worktree ps output (slash-normalized)', () => {
    expect(matchOrcaWorktree(psPayload, '/Users/me/orca/hackathon-c3')).toBe(
      'repo-2::/Users/me/orca/hackathon-c3',
    );
    expect(matchOrcaWorktree(psPayload, '/Users/me/orca/nope')).toBeNull();
  });

  it('returns null on malformed ps output', () => {
    expect(matchOrcaWorktree(null, '/x')).toBeNull();
    expect(matchOrcaWorktree({ result: {} }, '/x')).toBeNull();
    expect(matchOrcaWorktree('garbage', '/x')).toBeNull();
  });

  it('findOrcaWorktreeByPath resolves through a successful ps call', async () => {
    const runner = async () => ({ exitCode: 0, stdout: JSON.stringify(psPayload), timedOut: false });
    const id = await findOrcaWorktreeByPath('/Users/me/orca/hackathon-c3', runner);
    expect(id).toBe('repo-2::/Users/me/orca/hackathon-c3');
  });

  it('degrades to null when orca is missing, failing, timing out, or emits garbage', async () => {
    const fail = async () => ({ exitCode: 127, stdout: '', timedOut: false });
    expect(await findOrcaWorktreeByPath('/x', fail)).toBeNull();

    const slow = async () => ({ exitCode: -1, stdout: '', timedOut: true });
    expect(await findOrcaWorktreeByPath('/x', slow)).toBeNull();

    const noise = async () => ({ exitCode: 0, stdout: 'SecCodeCheckValidity blah\n{not json', timedOut: false });
    expect(await findOrcaWorktreeByPath('/x', noise)).toBeNull();

    const boom = async () => {
      throw new Error('ENOENT');
    };
    expect(await findOrcaWorktreeByPath('/x', boom)).toBeNull();
  });

  it('dropOrcaWorkspace reports success/failure without throwing', async () => {
    const ok = async () => ({ exitCode: 0, stdout: '', timedOut: false });
    expect(await dropOrcaWorkspace('repo-2::/p', '/p', ok)).toBe(true);

    const fail = async () => ({ exitCode: 1, stdout: '', timedOut: false });
    expect(await dropOrcaWorkspace('repo-2::/p', '/p', fail)).toBe(false);

    const boom = async () => {
      throw new Error('nope');
    };
    expect(await dropOrcaWorkspace('repo-2::/p', '/p', boom)).toBe(false);
  });
});

describe('cleanup config surface', () => {
  it('accepts valid cleanup modes and dropOrcaWorkspace from devagent.json', () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-cfg-'));
    dirs.push(dir);
    writeFileSync(join(dir, 'devagent.json'), JSON.stringify({ cleanup: 'always', dropOrcaWorkspace: true }));
    const cfg = loadConfig(dir);
    expect(cfg.cleanup).toBe('always');
    expect(cfg.dropOrcaWorkspace).toBe(true);
  });

  it('defaults are auto / false and invalid modes are rejected', () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-cfg-'));
    dirs.push(dir);
    const cfg = loadConfig(dir);
    expect(cfg.cleanup).toBeUndefined(); // resolved at call sites to 'auto'
    expect(cfg.dropOrcaWorkspace).toBeUndefined();

    writeFileSync(join(dir, 'devagent.json'), JSON.stringify({ cleanup: 'sometimes' }));
    expect(() => loadConfig(dir)).toThrow(/Invalid cleanup/);
  });
});
