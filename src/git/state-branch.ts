import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { runCli } from '../workers/spawn-utils.js';

/**
 * Internal: spawn a git command with stderr attached on failure. Routes
 * through runCli so the child inherits the fallback PATH (a parent's
 * minimal PATH produced `spawn git ENOENT` for every git operation and
 * killed the selfbuild loop; see src/git/worktree.ts run()).
 */
async function run(
  cmd: string,
  args: string[],
  cwd: string,
  timeoutMs: number,
  extraEnv?: Record<string, string>,
): Promise<{ stdout: string; stderr: string }> {
  const r = await runCli(cmd, args, { cwd, timeoutMs, ...(extraEnv ? { env: extraEnv } : {}) });
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

export interface EnsureStateBranchOpts {
  /** Remote to check and push to. Default 'origin'. */
  remote?: string;
  /** Branch to create on the remote. Default 'selfbuild/state'. */
  branch?: string;
  /** Repo-relative lessons file to seed the branch with. Default '.devagent/lessons.md'. */
  lessonsFile?: string;
}

export interface EnsureStateBranchResult {
  action: 'created' | 'exists';
}

/**
 * Ensure the durable-state branch exists on the remote, seeding it with the
 * lessons file on first creation.
 *
 * Why plumbing-only: the selfbuild automation races on HEAD switches, so
 * porcelain orphan-branch workflows (checkout/switch to a new parentless
 * root) would clobber the live worktree and are the main failure mode this
 * module must avoid. The orphan commit is instead built in a temporary index
 * file (GIT_INDEX_FILE) and committed with `git commit-tree` with no parent,
 * leaving the current HEAD, index, and working tree completely untouched.
 *
 * The existence check is remote-side (`git ls-remote`) because the contract
 * is about the upstream branch, not any local ref. No fetch is performed.
 */
export async function ensureStateBranch(
  repoPath: string,
  opts: EnsureStateBranchOpts = {},
): Promise<EnsureStateBranchResult> {
  const remote = opts.remote ?? 'origin';
  const branch = opts.branch ?? 'selfbuild/state';
  const lessonsFile = opts.lessonsFile ?? '.devagent/lessons.md';

  // Remote-side existence check: any ref line means the branch already has
  // an upstream tip, so this is a no-op.
  const ls = await run('git', ['ls-remote', '--heads', remote, branch], repoPath, 30_000);
  if (ls.stdout.trim() !== '') return { action: 'exists' };

  // Build the orphan commit in a temporary index so the caller's index is
  // never modified.
  const tmp = mkdtempSync(join(tmpdir(), 'da-statebranch-'));
  const indexFile = join(tmp, 'index');
  try {
    await run('git', ['read-tree', '--empty'], repoPath, 30_000, { GIT_INDEX_FILE: indexFile });

    // Seed the lessons file: real content when present locally, empty blob
    // otherwise, so the branch always contains the file.
    const localPath = join(repoPath, lessonsFile);
    const content =
      existsSync(localPath) && readFileSync(localPath).length > 0 ? readFileSync(localPath) : Buffer.alloc(0);
    const blobFile = join(tmp, 'blob');
    writeFileSync(blobFile, content);
    const blob = await run('git', ['hash-object', '-w', blobFile], repoPath, 30_000);
    const blobSha = blob.stdout.trim();

    await run(
      'git',
      ['update-index', '--add', '--cacheinfo', `100644,${blobSha},${lessonsFile}`],
      repoPath,
      30_000,
      { GIT_INDEX_FILE: indexFile },
    );

    const tree = await run('git', ['write-tree'], repoPath, 30_000, { GIT_INDEX_FILE: indexFile });
    const treeSha = tree.stdout.trim();

    const commit = await run(
      'git',
      ['commit-tree', treeSha, '-m', 'chore: seed selfbuild/state durable branch'],
      repoPath,
      30_000,
    );
    const commitSha = commit.stdout.trim();

    // Exactly one push, no -f: the branch was just verified missing, so the
    // ref cannot have moved underneath us unless someone else created it
    // concurrently; in that rare race the push fails and the caller's
    // try/catch-and-continue handles it.
    await run('git', ['push', remote, `${commitSha}:refs/heads/${branch}`], repoPath, 120_000);

    return { action: 'created' };
  } finally {
    rmSync(tmp, { recursive: true, force: true });
  }
}
