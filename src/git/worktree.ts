import { execFile } from 'node:child_process';

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

/**
 * Create a git worktree for the given ticket:
 *   branch:  devagent/<sanitized ticket id>
 *   path:    <repoPath>/.devagent-worktrees/<sanitized ticket id>
 * Throws when the branch already exists (detected via git stderr).
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
      throw new Error(
        `Cannot create worktree for "${ticketId}": branch "${branch}" already exists`,
      );
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

/** Files changed on this branch vs its merge-base with the default branch. */
export async function listChangedFiles(repoPath: string, baseBranch: string): Promise<string[]> {
  const mergeBase = await run('git', ['merge-base', baseBranch, 'HEAD'], repoPath);
  const diff = await run('git', ['diff', '--name-only', mergeBase.stdout.trim(), 'HEAD'], repoPath);
  return diff.stdout.split('\n').map((s) => s.trim()).filter(Boolean);
}
