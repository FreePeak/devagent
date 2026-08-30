import { existsSync } from 'node:fs';
import { runCli } from '../workers/spawn-utils.js';

/**
 * Internal: spawn a git command. Routes through runCli so the child
 * inherits the fallback PATH (live-smoke lesson: a parent's minimal PATH
 * produced `spawn git ENOENT` for every worktree operation, killing the
 * selfbuild loop on loop 50 and tripping the circuit breaker). The 30s
 * timeout is generous enough for worktree add/remove but tight enough to
 * surface hung `git index-pack` calls.
 *
 * Translates non-zero exit / ENOENT into a thrown Error so the existing
 * `try { await run(...) } catch { return false }` call sites keep working.
 */
async function run(
  cmd: string,
  args: string[],
  cwd: string,
): Promise<{ stdout: string; stderr: string }> {
  const r = await runCli(cmd, args, { cwd, timeoutMs: 30_000 });
  if (r.exitCode !== 0) {
    const err = new Error(`${cmd} ${args.join(' ')} exited ${r.exitCode}: ${r.stderr.slice(0, 200)}`) as Error & {
      stdout?: string;
      stderr?: string;
      code?: number | string;
    };
    err.stdout = r.stdout;
    err.stderr = r.stderr;
    err.code = r.exitCode;
    throw err;
  }
  return { stdout: r.stdout, stderr: r.stderr };
}

export interface WorktreeInfo {
  worktreePath: string;
  branch: string;
}

/** Sanitize a ticket id so it is safe for branch and directory names. */
export function sanitizeTicketId(ticketId: string): string {
  return ticketId.replace(/[^A-Za-z0-9\-_]/g, '');
}

/** True when repoPath sits inside a git work tree. */
export async function isGitRepository(repoPath: string): Promise<boolean> {
  try {
    await run('git', ['rev-parse', '--is-inside-work-tree'], repoPath);
    return true;
  } catch {
    return false;
  }
}

/**
 * Create a git worktree for the given ticket:
 *   branch:  devagent/<sanitized ticket id>
 *   path:    <repoPath>/.devagent-worktrees/<sanitized ticket id>
 * Re-runs reuse prior work: when the branch already exists, the existing
 * worktree dir is returned as-is, or a new worktree is attached to the
 * existing branch.
 */
export async function createWorktree(
  repoPath: string,
  ticketId: string,
): Promise<WorktreeInfo> {
  const safeId = sanitizeTicketId(ticketId);
  const branch = `devagent/${safeId}`;
  const worktreePath = `${repoPath}/.devagent-worktrees/${safeId}`;

  try {
    await run('git', ['worktree', 'add', '-b', branch, worktreePath], repoPath);
  } catch (err) {
    const e = err as { stderr?: string; message?: string };
    const stderr = e.stderr ?? e.message ?? '';
    if (/already exists/.test(stderr)) {
      if (existsSync(worktreePath)) {
        return { worktreePath, branch };
      }
      try {
        await run('git', ['worktree', 'add', worktreePath, branch], repoPath);
      } catch {
        // Stale registration (directory removed without `git worktree remove`):
        // prune orphaned metadata and retry once.
        await run('git', ['worktree', 'prune'], repoPath);
        await run('git', ['worktree', 'add', worktreePath, branch], repoPath);
      }
      return { worktreePath, branch };
    }
    throw new Error(`git worktree add failed: ${stderr.trim()}`);
  }

  return { worktreePath, branch };
}

/**
 * Post-run disposal of a run's worktree (auto-cleanup stage).
 *
 * 'remove' mode snapshots any uncommitted worker output onto the run branch
 * first (nothing is ever lost), then removes the worktree registration and
 * directory. The branch itself is kept: it holds the snapshot and stays cheap.
 * 'preserve' keeps the tree untouched for inspection (failure debugging).
 */
export interface FinalizeWorktreeOptions {
  repoPath: string;
  worktreePath: string;
  ticketId: string;
  mode: 'remove' | 'preserve';
}

export interface FinalizeResult {
  action: 'removed' | 'preserved';
  /** True when uncommitted changes were snapshotted onto the branch pre-removal */
  committed: boolean;
  /** Present when removal was requested but failed (tree left in place) */
  error?: string;
}

export async function finalizeRunWorktree(opts: FinalizeWorktreeOptions): Promise<FinalizeResult> {
  if (opts.mode === 'preserve') {
    return { action: 'preserved', committed: false };
  }
  let committed = false;
  try {
    committed = await commitAllChanges(
      opts.worktreePath,
      `devagent(${sanitizeTicketId(opts.ticketId)}): auto-cleanup snapshot`,
    );
  } catch {
    // Snapshot is best-effort; removal below still proceeds for a clean tree.
  }
  try {
    await run('git', ['worktree', 'remove', '--force', opts.worktreePath], opts.repoPath);
    await run('git', ['worktree', 'prune'], opts.repoPath);
    return { action: 'removed', committed };
  } catch (err) {
    return { action: 'preserved', committed, error: (err as Error).message };
  }
}

/**
 * Remove a ticket's worktree with `git worktree remove --force`.
 * Best-effort: any failure (e.g. already removed) is swallowed.
 */
export async function removeWorktree(
  repoPath: string,
  ticketId: string,
): Promise<void> {
  const safeId = sanitizeTicketId(ticketId);
  const worktreePath = `${repoPath}/.devagent-worktrees/${safeId}`;

  try {
    await run('git', ['worktree', 'remove', '--force', worktreePath], repoPath);
  } catch {
    // best-effort cleanup
  }
}

/**
 * Stage every change (`git add -A`) and commit with --no-verify.
 * Returns true when a commit was created, false when the tree was clean
 * (nothing-to-commit is tolerated, not an error).
 */
export async function commitAllChanges(worktreePath: string, message: string): Promise<boolean> {
  await run('git', ['add', '-A'], worktreePath);
  try {
    await run('git', ['commit', '--no-verify', '-m', message], worktreePath);
    return true;
  } catch (err) {
    const e = err as { stdout?: string; stderr?: string };
    if (/nothing to commit/i.test(`${e.stdout ?? ''}${e.stderr ?? ''}`)) return false;
    throw err;
  }
}

/**
 * Name of the branch checked out in worktreePath — ground truth for
 * publishing (never guess a refspec). Throws on a detached HEAD, where
 * "branch" is meaningless and pushing would silently do the wrong thing.
 */
export async function currentBranch(worktreePath: string): Promise<string> {
  const r = await run('git', ['rev-parse', '--abbrev-ref', 'HEAD'], worktreePath);
  const name = r.stdout.trim();
  if (!name || name === 'HEAD') {
    throw new Error(`detached HEAD in ${worktreePath}: no branch to publish`);
  }
  return name;
}

/** Rename the currently checked-out branch inside a worktree. */
export async function renameCurrentBranch(worktreePath: string, newBranch: string): Promise<void> {
  await run('git', ['branch', '-m', newBranch], worktreePath);
}

/**
 * Hard-delete a branch from the repo. Best-effort: any failure
 * (unknown branch, checked out elsewhere) is swallowed.
 */
export async function deleteBranch(repoPath: string, branch: string): Promise<void> {
  try {
    await run('git', ['branch', '-D', branch], repoPath);
  } catch {
    // best-effort cleanup
  }
}

/** Files changed on this branch vs its merge-base with the default branch. */
export async function listChangedFiles(repoPath: string, baseBranch: string): Promise<string[]> {
  const mergeBase = await run('git', ['merge-base', baseBranch, 'HEAD'], repoPath);
  const diff = await run('git', ['diff', '--name-only', mergeBase.stdout.trim(), 'HEAD'], repoPath);
  return diff.stdout.split('\n').map((s) => s.trim()).filter(Boolean);
}

/**
 * Stash uncommitted changes (including untracked files) in the worktree at
 * repoPath. Returns null when the tree is already clean (no empty stash is
 * created); otherwise returns the concrete stash SHA.
 *
 * The SHA (not `stash@{0}`) is what callers must pop: stash indices shift
 * whenever anything else stashes concurrently, which is exactly how the
 * selfbuild automation clobbered work in earlier loops.
 */
export async function stashMainWorktree(repoPath: string, message: string): Promise<string | null> {
  const statusBefore = await run('git', ['status', '--porcelain'], repoPath);
  if (statusBefore.stdout.trim() === '') return null;

  const stashListBefore = await run('git', ['stash', 'list', '--format=%H'], repoPath);
  await run('git', ['stash', 'push', '--include-untracked', '-m', message], repoPath);
  const rev = await run('git', ['rev-parse', '-q', '--verify', 'stash@{0}'], repoPath);
  const sha = rev.stdout.trim();
  if (!sha) throw new Error('git stash push succeeded but stash@{0} could not be resolved');

  const stashListAfter = await run('git', ['stash', 'list', '--format=%H'], repoPath);
  if (stashListBefore.stdout.includes(sha)) {
    throw new Error(`stash push created no new stash entry (sha ${sha} already present)`);
  }
  return sha;
}

/**
 * Restore a stash created by stashMainWorktree, addressed by its concrete
 * SHA rather than a shifting `stash@{n}` index. `git stash pop` rejects raw
 * SHAs, so this applies the commit then drops the exact stash entry it
 * still points at. Returns false when the apply fails (conflict, missing
 * stash) and leaves the stash intact — never drops user work.
 */
export async function popStashBySha(repoPath: string, sha: string): Promise<boolean> {
  try {
    await run('git', ['stash', 'apply', sha], repoPath);
  } catch {
    return false;
  }
  try {
    const list = await run('git', ['stash', 'list', '--format=%H'], repoPath);
    const idx = list.stdout.split('\n').filter(Boolean).indexOf(sha);
    if (idx < 0) return true; // already consumed by a concurrent pop
    const current = await run('git', ['rev-parse', '-q', '--verify', `stash@{${idx}}`], repoPath);
    if (current.stdout.trim() === sha) {
      await run('git', ['stash', 'drop', `stash@{${idx}}`], repoPath);
    }
  } catch {
    // apply succeeded; a failed drop only leaves the stash in place
  }
  return true;
}

/**
 * Fail-fast guard for the merge-back path: refuses to run when the main
 * worktree's HEAD is detached or on a branch other than baseBranch, or when
 * it carries uncommitted changes. The branch check runs before the status
 * check so the reported error is always the most actionable one.
 */
export async function assertCleanMainWorktree(repoPath: string, baseBranch = 'main'): Promise<void> {
  const head = await run('git', ['rev-parse', '--abbrev-ref', 'HEAD'], repoPath);
  const branch = head.stdout.trim();
  if (branch === 'HEAD') {
    throw new Error('main worktree is in detached-HEAD state; refusing to merge');
  }
  if (branch !== baseBranch) {
    throw new Error(`main worktree is on branch ${branch}, expected ${baseBranch}; refusing to merge`);
  }
  const status = await run('git', ['status', '--porcelain'], repoPath);
  if (status.stdout.trim() !== '') {
    const preview = status.stdout.split('\n').filter(Boolean).slice(0, 20).join('\n');
    throw new Error(`main worktree has uncommitted changes; refusing to merge:\n${preview}`);
  }
}
