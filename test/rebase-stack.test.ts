import { describe, expect, it, afterAll } from 'vitest';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { rebaseStack } from '../src/git/rebase-stack.js';

const dirs: string[] = [];
afterAll(() => {
  for (const d of dirs) rmSync(d, { recursive: true, force: true });
});

function g(repo: string, ...args: string[]): string {
  return execFileSync('git', args, { cwd: repo }).toString().trim();
}

/** Repo with main@base, plus stacked branches bottom->top each one commit ahead. */
function initStackedRepo(): { repo: string; branches: string[] } {
  const dir = mkdtempSync(join(tmpdir(), 'da-rb-'));
  dirs.push(dir);
  const repo = join(dir, 'repo');
  mkdirSync(repo);
  g(repo, 'init', '-b', 'main');
  g(repo, 'config', 'user.email', 'test@example.com');
  g(repo, 'config', 'user.name', 'test');
  writeFileSync(join(repo, 'f.txt'), 'base\n');
  g(repo, 'add', '.');
  g(repo, 'commit', '-m', 'base');

  // loop60 branches off main and adds its own file
  g(repo, 'checkout', '-b', 'devagent/loop60');
  writeFileSync(join(repo, 'l60.txt'), '60\n');
  g(repo, 'add', '.');
  g(repo, 'commit', '-m', 'loop60 work');

  // loop61 stacks on loop60
  g(repo, 'checkout', '-b', 'devagent/loop61');
  writeFileSync(join(repo, 'l61.txt'), '61\n');
  g(repo, 'add', '.');
  g(repo, 'commit', '-m', 'loop61 work');

  // main moves forward (the "parent landed" event)
  g(repo, 'checkout', 'main');
  writeFileSync(join(repo, 'main.txt'), 'new\n');
  g(repo, 'add', '.');
  g(repo, 'commit', '-m', 'main moved');

  return { repo, branches: ['devagent/loop60', 'devagent/loop61'] };
}

describe('rebaseStack', () => {
  it('rebases drifted children onto the moved base in order', async () => {
    const { repo, branches } = initStackedRepo();
    const r = await rebaseStack(repo, branches, { onto: 'main' });
    expect(r.ok).toBe(true);
    expect(r.results.map((x) => x.outcome)).toEqual(['rebased', 'rebased']);
    // each branch now contains the new main commit
    for (const b of branches) {
      expect(g(repo, 'merge-base', '--is-ancestor', 'main', b)).toBeDefined();
      // own work survives the rebase
      expect(g(repo, 'show', `${b}:l${b.slice(-2)}.txt`)).toMatch(/6[01]/);
    }
    // stack order preserved: loop61 still contains loop60's tip
    expect(g(repo, 'merge-base', '--is-ancestor', branches[0], branches[1])).toBeDefined();
  });

  it('reports up-to-date when nothing drifted', async () => {
    const { repo, branches } = initStackedRepo();
    await rebaseStack(repo, branches, { onto: 'main' }); // first pass fixes drift
    const r = await rebaseStack(repo, branches, { onto: 'main' }); // second is a no-op
    expect(r.ok).toBe(true);
    expect(r.results.map((x) => x.outcome)).toEqual(['up-to-date', 'up-to-date']);
  });

  it('stops at a conflicting branch with earlier siblings rebased and children untouched', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-rb-c-'));
    dirs.push(dir);
    const repo = join(dir, 'repo');
    mkdirSync(repo);
    g(repo, 'init', '-b', 'main');
    g(repo, 'config', 'user.email', 'test@example.com');
    g(repo, 'config', 'user.name', 'test');
    writeFileSync(join(repo, 'shared.txt'), 'base\n');
    g(repo, 'add', '.');
    g(repo, 'commit', '-m', 'base');

    g(repo, 'checkout', '-b', 'b1');
    writeFileSync(join(repo, 'shared.txt'), 'b1 edit\n');
    g(repo, 'add', '.');
    g(repo, 'commit', '-m', 'b1 work');

    g(repo, 'checkout', '-b', 'b2');
    writeFileSync(join(repo, 'other.txt'), 'b2\n');
    g(repo, 'add', '.');
    g(repo, 'commit', '-m', 'b2 work');

    // conflicting change on main to the same line b1 touched
    g(repo, 'checkout', 'main');
    writeFileSync(join(repo, 'shared.txt'), 'main edit\n');
    g(repo, 'add', '.');
    g(repo, 'commit', '-m', 'main conflicts with b1');

    const r = await rebaseStack(repo, ['b1', 'b2'], { onto: 'main' });
    expect(r.ok).toBe(false);
    expect(r.results).toHaveLength(1); // walk stops at the conflict
    expect(r.results[0]).toMatchObject({ branch: 'b1', outcome: 'conflict' });
    // conflicted branch ref untouched; child still based on old parent
    // (merge-base --is-ancestor exits non-zero when false)
    expect(() => g(repo, 'merge-base', '--is-ancestor', 'main', 'b1')).toThrow();
    expect(() => g(repo, 'merge-base', '--is-ancestor', 'main', 'b2')).toThrow();
    // no leftover rebase state in any worktree
    expect(g(repo, 'worktree', 'list')).not.toContain('da-rebase-');
  });

  it('rejects a disconnected history before touching anything', async () => {
    const { repo } = initStackedRepo();
    // orphan branch shares no ancestry with the stack
    execFileSync('git', ['checkout', '--orphan', 'solo'], { cwd: repo });
    writeFileSync(join(repo, 'orphan.txt'), 'orphan\n');
    g(repo, 'add', '.');
    g(repo, 'commit', '-m', 'orphan root');
    g(repo, 'checkout', 'main');
    const r = await rebaseStack(repo, ['devagent/loop60', 'solo'], { onto: 'main' });
    expect(r.ok).toBe(false);
    expect(r.results[0]).toMatchObject({ branch: 'solo', outcome: 'error' });
    // validation ran before any rebase: loop60 never moved
    expect(r.results.filter((x) => x.branch === 'devagent/loop60')).toEqual([]);
  });

  it('refuses branches checked out in a worktree', async () => {
    const { repo, branches } = initStackedRepo();
    const wt = join(dirs[dirs.length - 1], 'held');
    g(repo, 'worktree', 'add', wt, 'devagent/loop60');
    const r = await rebaseStack(repo, branches, { onto: 'main' });
    expect(r.ok).toBe(false);
    expect(r.results[0]).toMatchObject({ branch: 'devagent/loop60', outcome: 'error', detail: expect.stringContaining('checked out') });
  });

  it('errors on unknown branch or missing onto', async () => {
    const { repo } = initStackedRepo();
    expect((await rebaseStack(repo, ['nope'], {})).results[0].outcome).toBe('error');
    expect((await rebaseStack(repo, ['devagent/loop60'], { onto: 'ghost' })).results[0].outcome).toBe('error');
  });

  it('handles an empty stack as a no-op success', async () => {
    const { repo } = initStackedRepo();
    const r = await rebaseStack(repo, [], { onto: 'main' });
    expect(r).toEqual({ ok: true, results: [] });
  });
});
