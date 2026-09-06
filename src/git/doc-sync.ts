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
  /**
   * True when local and origin histories share no fork point progress — the
   * merge-base is neither HEAD nor origin/<branch> — so a fast-forward is
   * impossible. A clean tree is rebased onto origin (loops 95–99 burned on
   * misclassifying this as a generic provider outage); a dirty tree refuses.
   */
  diverged?: boolean;
  /**
   * True when the tracked work-selection docs are locally modified. With
   * `diverged` this is the distinct refusal class: an autostash would lift
   * the operator's edit out of the tree, so the sync refuses instead of
   * resolving.
   */
  dirty?: boolean;
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
 *
 * Divergence (Q41 degradation surface): when histories diverge and the tracked
 * docs are clean, the local branch is rebased onto origin/<branch> with
 * --autostash (clean abort on conflict, tree untouched). When the tracked docs
 * are dirty AND histories diverged, the sync refuses with ok:false +
 * diverged:true — a stash/autostash would momentarily lift the operator's
 * edit out of the tree, so the operator must reconcile by hand.
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

  const dirtyDocs = dirty.trim().length > 0;
  if (local === remote) {
    return { ok: true, alreadyUpToDate: true, dirty: dirtyDocs, detail: 'work-selection docs already at origin' };
  }

  // Divergence classification (PRD §17 defect + Q41) BEFORE the dirty gate:
  // merge-base prints the fork point A. base === local → strictly behind, a
  // plain fast-forward still applies; base === remote → strictly ahead,
  // nothing to pull; anything else (including merge-base exit 1 = unrelated
  // histories) means the histories diverged and a fast-forward is impossible.
  let base = '';
  try {
    base = (
      await execFileP('git', ['merge-base', 'HEAD', `origin/${branch}`], { cwd: repoPath, timeout: timeoutMs, encoding: 'utf8' })
    ).stdout.trim();
  } catch {
    base = ''; // exit 1: unrelated histories
  }
  const diverged = base !== local && base !== remote;

  if (dirtyDocs) {
    return diverged
      ? {
          ok: false,
          diverged: true,
          dirty: true,
          detail: `refusing sync: histories diverged from origin/${branch} and ${WORK_SELECTION_DOCS.join(', ')} locally modified — reconcile by hand (an autostash would lift the operator edit out of the tree)`,
        }
      : {
          ok: false,
          dirty: true,
          detail: `refusing sync: ${WORK_SELECTION_DOCS.join(', ')} locally modified — commit or stash first (${dirty.trim().split('\n').length} file(s))`,
        };
  }

  if (!diverged) {
    // Linear history: the old fast-forward path (covers strictly-behind and
    // strictly-ahead — an ff-only merge of an ancestor is a no-op).
    try {
      const merge = await execFileP('git', ['merge', '--ff-only', `origin/${branch}`], { cwd: repoPath, timeout: timeoutMs, encoding: 'utf8' });
      return { ok: true, alreadyUpToDate: false, dirty: false, detail: merge.stdout.trim().slice(0, 200) || 'fast-forwarded' };
    } catch (err) {
      const msg = (err as Error & { stderr?: string }).stderr ?? (err as Error).message;
      return { ok: false, dirty: false, detail: `fast-forward to origin/${branch} failed: ${msg.trim().slice(0, 300)}` };
    }
  }

  // Diverged + clean tracked docs: rebase onto origin with --autostash.
  // On conflict, abort cleanly — a checkout left mid-rebase
  // (.git/rebase-merge) would poison every later status/merge/worktree op.
  try {
    const rebase = await execFileP('git', ['rebase', '--autostash', `origin/${branch}`], { cwd: repoPath, timeout: timeoutMs, encoding: 'utf8' });
    return {
      ok: true,
      alreadyUpToDate: false,
      diverged: true,
      dirty: false,
      detail: (rebase.stdout.trim().slice(0, 200) || `rebased onto origin/${branch}`) + ' (diverged history reconciled)',
    };
  } catch (err) {
    await execFileP('git', ['rebase', '--abort'], { cwd: repoPath, timeout: timeoutMs }).catch(() => {});
    const msg = (err as Error & { stderr?: string }).stderr ?? (err as Error).message;
    return {
      ok: false,
      diverged: true,
      dirty: false,
      detail: `diverged from origin/${branch}: rebase failed, aborted cleanly — reconcile by hand: ${msg.trim().slice(0, 300)}`,
    };
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
