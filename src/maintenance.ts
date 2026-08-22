import { readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';

/**
 * Cleanup discipline: runs preserve worktrees for inspection, but stale ones
 * accumulate. Enumerate devagent worktrees older than a cutoff so callers can
 * remove them (removal stays explicit at the call site).
 */

export interface StaleWorktree {
  path: string;
  ageMs: number;
}

/** Find `.devagent-worktrees/<name>` dirs under repoPath older than cutoffMs. */
export function findStaleWorktrees(repoPath: string, cutoffMs: number, now = Date.now()): StaleWorktree[] {
  const wtRoot = join(repoPath, '.devagent-worktrees');
  let names: string[];
  try {
    names = readdirSync(wtRoot);
  } catch {
    return [];
  }
  const stale: StaleWorktree[] = [];
  for (const name of names) {
    try {
      const s = statSync(join(wtRoot, name));
      if (!s.isDirectory()) continue;
      const ageMs = now - s.mtimeMs;
      if (ageMs >= cutoffMs) stale.push({ path: join(wtRoot, name), ageMs });
    } catch {
      continue; // raced removal
    }
  }
  return stale;
}
