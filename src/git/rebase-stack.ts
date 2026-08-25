import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnCli } from '../workers/spawn-utils.js';
/**
 * Merge-queue rebase automation (PRD Phase 4): stacked loop branches drift
 * behind main as parents land, and every refresh used to be a manual
 * `git rebase` dance per branch. This walks a stack bottom-up and rebases
 * each branch onto its updated parent inside a throwaway detached worktree,
 * so the caller's checkout and any task worktrees are never touched.
 *
 * Divergence-guard discipline (mirrors orchestrator/merge.ts): a conflict
 * stops the walk with a clear report instead of force-continuing — children
 * of a conflicted branch are left untouched because their parent just moved.
 */
async function git(args: string[], cwd: string, timeoutMs = 60_000): Promise<{ exitCode: number; stdout: string; stderr: string }> {
  const r = await spawnCli('git', args, { cwd, timeoutMs });
  return { exitCode: r.exitCode, stdout: r.stdout, stderr: r.stderr };
}

export type RebaseOutcome = 'up-to-date' | 'rebased' | 'pushed' | 'conflict' | 'error';

export interface StackBranchResult {
  branch: string;
  outcome: RebaseOutcome;
  detail?: string;
}

export interface RebaseStackResult {
  ok: boolean;
  results: StackBranchResult[];
}

/** Refs currently checked out in any worktree (rebase would fork them). */
async function checkedOutBranches(repoPath: string): Promise<Set<string>> {
  const r = await git(['worktree', 'list', '--porcelain'], repoPath);
  const out = new Set<string>();
  for (const line of r.stdout.split('\n')) {
    if (line.startsWith('branch ')) out.add(line.slice('branch '.length).trim().replace(/^refs\/heads\//, ''));
  }
  return out;
}

export async function rebaseStack(
  repoPath: string,
  branches: string[],
  opts: { onto?: string; push?: boolean } = {},
): Promise<RebaseStackResult> {
  const onto = opts.onto ?? 'main';
  const results: StackBranchResult[] = [];
  if (branches.length === 0) return { ok: true, results };

  // Validate before touching anything: all branches exist, form a chain over
  // the base, and none is checked out in a worktree (a rebased ref would
  // silently diverge from the checked-out copy).
  for (const b of branches) {
    const v = await git(['rev-parse', '--verify', '--quiet', `${b}^{commit}`], repoPath);
    if (v.exitCode !== 0) {
      return { ok: false, results: [{ branch: b, outcome: 'error', detail: `branch not found` }] };
    }
  }
  const baseCheck = await git(['rev-parse', '--verify', '--quiet', `${onto}^{commit}`], repoPath);
  if (baseCheck.exitCode !== 0) {
    return { ok: false, results: [{ branch: onto, outcome: 'error', detail: 'onto branch not found' }] };
  }
  const chain: string[] = [onto, ...branches];
  for (let i = 0; i < branches.length; i++) {
    // Shared ancestry, not containment: a stacked child forked from its
    // parent's pre-rebase tip, so the moved parent ref cannot be an
    // ancestor of the child anymore.
    const base = chain[i]!;
    const branch = branches[i]!;
    const a = await git(['merge-base', base, branch], repoPath);
    if (a.exitCode !== 0) {
      return {
        ok: false,
        results: [{ branch, outcome: 'error', detail: `not stacked on ${base} (no common ancestry)` }],
      };
    }
  }
  const busy = await checkedOutBranches(repoPath);
  for (const b of branches) {
    if (busy.has(b)) {
      return {
        ok: false,
        results: [{ branch: b, outcome: 'error', detail: 'checked out in a worktree; run from a workspace that does not hold it' }],
      };
    }
  }

  let parent = onto;
  for (const branch of branches) {
    const tipBefore = (await git(['rev-parse', branch], repoPath)).stdout.trim();
    // Up-to-date when the parent tip is already an ancestor of the branch.
    const fresh = await git(['merge-base', '--is-ancestor', parent, branch], repoPath);
    if (fresh.exitCode === 0) {
      results.push({ branch, outcome: 'up-to-date' });
      parent = branch;
      continue;
    }
    // Rebase in a throwaway detached worktree so no live checkout moves.
    const tmp = mkdtempSync(join(tmpdir(), 'da-rebase-'));
    try {
      const add = await git(['worktree', 'add', '--detach', tmp, branch], repoPath);
      if (add.exitCode !== 0) {
        results.push({ branch, outcome: 'error', detail: `worktree add failed: ${(add.stderr || add.stdout).trim().slice(0, 200)}` });
        return { ok: false, results };
      }
      const rb = await git(['rebase', parent], tmp);
      if (rb.exitCode !== 0) {
        await git(['rebase', '--abort'], tmp); // leave the tree clean; branch ref untouched
        results.push({
          branch,
          outcome: 'conflict',
          detail: `rebase onto ${parent} conflicts; resolve manually (children untouched)`,
        });
        return { ok: false, results };
      }
      const move = await git(['branch', '-f', branch, 'HEAD'], tmp);
      if (move.exitCode !== 0) {
        results.push({ branch, outcome: 'error', detail: `ref update failed: ${(move.stderr || move.stdout).trim().slice(0, 200)}` });
        return { ok: false, results };
      }
      results.push({ branch, outcome: 'rebased' });
      parent = branch;
      if (opts.push) {
        // Lease pins the expected remote sha to the pre-rebase tip: if the
        // remote moved while we rebased (another session refreshed the same
        // PR), the push refuses instead of clobbering their update. The
        // explicit-sha form works even with no stale remote-tracking ref.
        const pushed = await git(
          ['push', `--force-with-lease=refs/heads/${branch}:${tipBefore}`, 'origin', `${branch}:${branch}`],
          repoPath,
        );
        const entry = results[results.length - 1]!;
        if (pushed.exitCode !== 0) {
          entry.outcome = 'error';
          entry.detail = `push failed: ${(pushed.stderr || pushed.stdout).trim().slice(0, 200)}`;
          return { ok: false, results };
        }
        entry.outcome = 'pushed';
      }
    } finally {
      rmSync(tmp, { recursive: true, force: true });
      await git(['worktree', 'prune'], repoPath);
    }
  }
  return { ok: true, results };
}
