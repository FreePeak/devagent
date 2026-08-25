import { execFile } from 'node:child_process';
import { existsSync } from 'node:fs';

function run(
  cmd: string,
  args: string[],
  cwd: string,
): Promise<{ stdout: string; stderr: string }> {
  return new Promise((resolve, reject) => {
    execFile(cmd, args, { cwd }, (err, stdout, stderr) => {
      if (err) {
        const e = err as { stdout?: unknown; stderr?: unknown };
        if (e.stdout === undefined) e.stdout = String(stdout);
        if (e.stderr === undefined) e.stderr = String(stderr);
        reject(err);
      } else {
        resolve({ stdout: String(stdout), stderr: String(stderr) });
      }
    });
  });
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
