import { execFile } from 'node:child_process';

function run(
  cmd: string,
  args: string[],
  cwd: string,
): Promise<{ stdout: string; stderr: string }> {
  return new Promise((resolve, reject) => {
    execFile(cmd, args, { cwd }, (err, stdout, stderr) => {
      if (err) {
        // Attach raw output unless already present (real execFile sets it).
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

/**
 * Push a branch to origin so `gh pr create -H` can reference it.
 * Uses explicit refspec so local-only branches publish cleanly.
 */
export async function pushBranch(repoPath: string, branch: string): Promise<void> {
  await withRateLimitRetry(() =>
    new Promise<void>((resolve, reject) => {
      execFile('git', ['push', '-u', 'origin', `${branch}:${branch}`], { cwd: repoPath }, (err, _stdout, stderr) => {
        if (err) {
          reject(describeError(`git push ${branch} failed`, String(stderr)));
        } else {
          resolve();
        }
      });
    }),
  );
}

export interface CreatePrOptions {
  /** Absolute or relative path to the git repository */
  repoPath: string;
  branch: string;
  title: string;
  body: string;
  /** Base branch to open the PR against (defaults to gh's default) */
  baseBranch?: string;
}

function describeError(context: string, stderr?: string): Error {
  const detail = stderr && stderr.trim().length > 0 ? `: ${stderr.trim()}` : '';
  return new Error(`${context}${detail}`);
}

const RATE_LIMIT_PATTERN = /rate limit|secondary rate|abuse detection/i;

/**
 * Retry once after a pause when GitHub signals a (secondary) rate limit.
 * gh/git expose limits as stderr text rather than headers, so the wait is
 * fixed at 60s — long enough for the standard secondary window.
 */
export async function withRateLimitRetry<T>(fn: () => Promise<T>, opts: { sleep?: (ms: number) => Promise<void> } = {}): Promise<T> {
  try {
    return await fn();
  } catch (err) {
    const msg = String((err as Error).message ?? err);
    if (!RATE_LIMIT_PATTERN.test(msg)) throw err;
    await (opts.sleep ?? ((ms) => new Promise<void>((r) => setTimeout(r, ms))))(60_000);
    return fn();
  }
}

/**
 * Create a pull request via the `gh` CLI.
 * Equivalent to `gh pr create -t <title> -b <body> -B <base> -H <branch>` run
 * inside the repository. Resolves with the PR URL parsed from stdout.
 */
export async function createPr(opts: CreatePrOptions): Promise<string> {
  const args = ['pr', 'create', '-t', opts.title, '-b', opts.body];
  if (opts.baseBranch) {
    args.push('-B', opts.baseBranch);
  }
  args.push('-H', opts.branch);

  try {
    const { stdout, stderr } = await withRateLimitRetry(() => run('gh', args, opts.repoPath));

    // `gh pr create` prints the PR URL as the last non-empty stdout line.
    const url = stdout
      .split(/\r?\n/)
      .map((line) => line.trim())
      .filter((line) => line.length > 0)
      .pop();

    if (!url) {
      throw new Error(
        `gh pr create produced no PR URL${stderr ? ` (stderr: ${stderr.trim()})` : ''}`,
      );
    }

    return url;
  } catch (err) {
    const e = err as { stderr?: string; message?: string };
    if (/^gh pr create produced no PR URL/.test(e.message ?? '')) {
      throw e;
    }
    throw describeError(`gh pr create failed for branch "${opts.branch}"`, e.stderr ?? e.message);
  }
}

/**
 * Check whether a branch exists in the repository using
 * `git rev-parse --verify refs/heads/<branch>`.
 */
export async function branchExists(
  repoPath: string,
  branch: string,
): Promise<boolean> {
  try {
    await run('git', ['rev-parse', '--verify', `refs/heads/${branch}`], repoPath);
    return true;
  } catch {
    return false;
  }
}

/**
 * Auto-merge a PR after green gates via `gh pr merge --auto --squash`.
 * Best-effort: returns the gh output or throws with the stderr.
 */
export async function autoMergePr(
  repoPath: string,
  prRef: string,
  strategy: 'squash' | 'merge' | 'rebase' = 'squash',
): Promise<string> {
  const flag = strategy === 'merge' ? '--merge' : strategy === 'rebase' ? '--rebase' : '--squash';
  const { stdout } = await withRateLimitRetry(() => run('gh', ['pr', 'merge', prRef, '--auto', flag], repoPath));
  return stdout.trim();
}
