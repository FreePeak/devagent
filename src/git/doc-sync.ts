import { execFile } from 'node:child_process';
import { statSync } from 'node:fs';
import { promisify } from 'node:util';
import { join } from 'node:path';

const execFileP = promisify(execFile);

/**
 * Repo doc-sync (operator PRD-freshness fix, 2026-09-04): the 24/7 scout and
 * the selfbuild loop select work from docs/PRD.md, but nothing refreshed the
 * working tree from origin before reading it — a manual PRD update pushed from
 * another machine (or committed locally by the operator) was invisible until
 * some unrelated `git pull` happened to land, so the scout enqueued stale
 * backlog items and the PO built the older doc version indefinitely.
 */

export interface RepoSyncResult {
  ok: boolean;
  /** true when the tree already matched origin (no update pulled) */
  alreadyUpToDate?: boolean;
  detail: string;
}

/** Files that gate work selection; a sync is only meaningful when they exist. */
export const WORK_SELECTION_DOCS = ['docs/PRD.md'];

/**
 * Fetch origin and fast-forward the current branch so work-selection docs are
 * fresh before any scout/PO read. Refuses to run on a dirty tree for the
 * tracked work-selection files (an operator mid-edit must never be clobbered);
 * untracked files and state dirs (.devagent/.selfbuild) do not block the sync.
 * Network/merge failures are reported, never thrown — callers decide whether a
 * stale read is fatal (loop) or best-effort (scout heartbeat).
 */
export async function syncWorkSelectionDocs(
  repoPath: string,
  opts: { branch?: string; timeoutMs?: number } = {},
): Promise<RepoSyncResult> {
  const branch = opts.branch ?? 'main';
  const timeoutMs = opts.timeoutMs ?? 30_000;
  try {
    const fetch = await execFileP('git', ['fetch', 'origin', branch], { cwd: repoPath, timeout: timeoutMs, encoding: 'utf8' });
    if (fetch.stderr.includes('fatal:')) {
      return { ok: false, detail: `git fetch failed: ${fetch.stderr.trim().slice(0, 300)}` };
    }
  } catch (err) {
    return { ok: false, detail: `git fetch failed: ${(err as Error).message.slice(0, 300)}` };
  }

  let local: string;
  let remote: string;
  let dirty: string;
  try {
    const [localRes, remoteRes, statusRes] = await Promise.all([
      execFileP('git', ['rev-parse', 'HEAD'], { cwd: repoPath, timeout: timeoutMs, encoding: 'utf8' }),
      execFileP('git', ['rev-parse', `origin/${branch}`], { cwd: repoPath, timeout: timeoutMs, encoding: 'utf8' }),
      execFileP('git', ['status', '--porcelain', '--', ...WORK_SELECTION_DOCS], { cwd: repoPath, timeout: timeoutMs, encoding: 'utf8' }),
    ]);
    local = localRes.stdout.trim();
    remote = remoteRes.stdout.trim();
    dirty = statusRes.stdout;
  } catch (err) {
    return { ok: false, detail: `git rev-parse/status failed: ${(err as Error).message.slice(0, 300)}` };
  }

  if (local === remote) return { ok: true, alreadyUpToDate: true, detail: 'work-selection docs already at origin' };
  if (dirty.trim()) {
    return {
      ok: false,
      detail: `refusing sync: ${WORK_SELECTION_DOCS.join(', ')} locally modified — commit or stash first (${dirty.trim().split('\n').length} file(s))`,
    };
  }

  try {
    const merge = await execFileP('git', ['merge', '--ff-only', `origin/${branch}`], { cwd: repoPath, timeout: timeoutMs, encoding: 'utf8' });
    return { ok: true, alreadyUpToDate: false, detail: merge.stdout.trim().slice(0, 200) || 'fast-forwarded' };
  } catch (err) {
    const msg = (err as Error & { stderr?: string }).stderr ?? (err as Error).message;
    return { ok: false, detail: `fast-forward to origin/${branch} failed (diverged?): ${msg.trim().slice(0, 300)}` };
  }
}

/** PRD.md stat as the staleness join key (doc-level, not tree-level). */
export function prdStat(repoPath: string): { size: number; mtimeMs: number } | null {
  try {
    const st = statSync(join(repoPath, 'docs', 'PRD.md'));
    return { size: st.size, mtimeMs: st.mtimeMs };
  } catch {
    return null;
  }
}
