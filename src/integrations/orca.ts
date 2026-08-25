import { spawnCli } from '../workers/spawn-utils.js';

/**
 * Orca workspace awareness for post-run cleanup.
 *
 * When a run's --repo is an Orca-managed worktree (e.g. ~/orca/workspaces/<name>),
 * plain `git worktree remove` would leave a ghost card in the Orca app. These
 * helpers detect that case and remove through orca-cli instead.
 *
 * Everything here is best-effort and gracefully degrades: missing binary,
 * app not running, malformed output -> "not an Orca workspace" / no-op.
 */

export interface OrcaWorktreeRef {
  /** Full Orca worktree id: `<repoId>::<worktreePath>` */
  id: string;
  path: string;
}

type CliRunner = (
  cmd: string,
  args: string[],
  opts: { cwd: string; timeoutMs: number },
) => Promise<{ exitCode: number; stdout: string; timedOut: boolean }>;

const defaultRunner: CliRunner = (cmd, args, opts) => spawnCli(cmd, args, opts);

/** Extract the JSON object from CLI output that may carry non-JSON noise lines. */
function extractJson(text: string): unknown {
  const start = text.indexOf('{');
  if (start < 0) return null;
  try {
    return JSON.parse(text.slice(start)) as unknown;
  } catch {
    return null;
  }
}

/** Pure matcher over parsed `orca worktree ps` output: exact path match (slash-normalized). */
export function matchOrcaWorktree(psOutput: unknown, repoPath: string): string | null {
  if (psOutput === null || typeof psOutput !== 'object') return null;
  const result = (psOutput as { result?: unknown }).result;
  if (result === null || typeof result !== 'object') return null;
  const worktrees = (result as { worktrees?: unknown }).worktrees;
  if (!Array.isArray(worktrees)) return null;
  const normalize = (p: string) => p.replace(/\/+$/, '');
  const target = normalize(repoPath);
  for (const wt of worktrees) {
    if (wt === null || typeof wt !== 'object') continue;
    const { id, path } = wt as { id?: unknown; path?: unknown };
    if (typeof id === 'string' && typeof path === 'string' && normalize(path) === target) {
      return id;
    }
  }
  return null;
}

/** Resolve the Orca worktree id for repoPath, or null when it is not Orca-managed. */
export async function findOrcaWorktreeByPath(
  repoPath: string,
  runner: CliRunner = defaultRunner,
): Promise<string | null> {
  try {
    const r = await runner('orca', ['worktree', 'ps', '--json'], { cwd: repoPath, timeoutMs: 15_000 });
    if (r.timedOut || r.exitCode !== 0) return null;
    return matchOrcaWorktree(extractJson(r.stdout), repoPath);
  } catch {
    return null;
  }
}

/** Remove an Orca-managed workspace (card + directory). Returns true on success. */
export async function dropOrcaWorkspace(
  worktreeId: string,
  cwd: string,
  runner: CliRunner = defaultRunner,
): Promise<boolean> {
  try {
    const r = await runner(
      'orca',
      ['worktree', 'rm', '--worktree', `id:${worktreeId}`, '--force'],
      { cwd, timeoutMs: 30_000 },
    );
    return !r.timedOut && r.exitCode === 0;
  } catch {
    return false;
  }
}

/** Best-effort Orca repo registration: `orca repo add --path <repoPath> --json`. */
export async function ensureOrcaRepo(
  repoPath: string,
  runner: CliRunner = defaultRunner,
): Promise<boolean> {
  try {
    const r = await runner('orca', ['repo', 'add', '--path', repoPath, '--json'], { cwd: repoPath, timeoutMs: 15_000 });
    return !r.timedOut && r.exitCode === 0;
  } catch {
    return false;
  }
}

/** Create an Orca worktree for a worker slot. Returns worktree path or null on any failure. */
export async function createOrcaWorktree(
  repoPath: string,
  name: string,
  runner: CliRunner = defaultRunner,
): Promise<string | null> {
  try {
    const r = await runner(
      'orca',
      ['worktree', 'create', '--name', name, '--repo', `path:${repoPath}`, '--json'],
      { cwd: repoPath, timeoutMs: 30_000 },
    );
    if (r.timedOut || r.exitCode !== 0) return null;
    const json = extractJson(r.stdout) as { result?: { path?: string; worktree?: { path?: string } } } | null;
    return json?.result?.path ?? json?.result?.worktree?.path ?? null;
  } catch {
    return null;
  }
}

/** List Orca worktrees that belong to the given repo (path prefix match). Best-effort. */
export async function listOrcaWorktrees(
  repoPath: string,
  runner: CliRunner = defaultRunner,
): Promise<string[]> {
  try {
    const r = await runner('orca', ['worktree', 'ps', '--json'], { cwd: repoPath, timeoutMs: 15_000 });
    if (r.timedOut || r.exitCode !== 0) return [];
    const parsed = extractJson(r.stdout) as { result?: { worktrees?: Array<{ path?: string }> } } | null;
    const wts = parsed?.result?.worktrees;
    if (!Array.isArray(wts)) return [];
    return wts.map((w) => w.path).filter((p): p is string => typeof p === 'string' && p.startsWith(repoPath));
  } catch {
    return [];
  }
}
