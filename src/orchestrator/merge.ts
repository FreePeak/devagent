import { join } from 'node:path';
import { spawnCli } from '../workers/spawn-utils.js';
import type { ProjectBoard } from './types.js';
import type { RunLogger } from '../logger.js';

/**
 * Merge-back: after all tasks are done, integrate each task branch into the
 * base branch in dependency order (topological), re-running the test gate on
 * the merged tree. A merge conflict or gate failure stops integration with a
 * clear report rather than force-continuing (divergence-guard discipline).
 */

async function git(args: string[], cwd: string, timeoutMs = 60_000): Promise<{ exitCode: number; stdout: string; stderr: string }> {
  const r = await spawnCli('git', args, { cwd, timeoutMs });
  return { exitCode: r.exitCode, stdout: r.stdout, stderr: r.stderr };
}

/** Topological order over dependsOn (board is validated cycle-free at plan time). */
export function topoOrder(board: ProjectBoard): string[] {
  const byId = new Map(board.tasks.map((t) => [t.id, t]));
  const order: string[] = [];
  const seen = new Set<string>();
  const visit = (id: string): void => {
    if (seen.has(id)) return;
    seen.add(id);
    for (const d of byId.get(id)?.dependsOn ?? []) visit(d);
    order.push(id);
  };
  board.tasks.forEach((t) => visit(t.id));
  return order;
}

export interface MergeResult {
  ok: boolean;
  merged: string[];
  failure?: { taskId: string; stage: 'checkout' | 'merge' | 'gate'; detail: string };
}

export async function mergeProjectBranches(
  repoPath: string,
  board: ProjectBoard,
  baseBranch: string,
  log: RunLogger,
): Promise<MergeResult> {
  const doneIds = new Set(board.tasks.filter((t) => t.status === 'done').map((t) => t.id));
  // Ensure base exists locally and is checked out in the main worktree
  const checkout = await git(['checkout', baseBranch], repoPath);
  if (checkout.exitCode !== 0) {
    return {
      ok: false,
      merged: [],
      failure: { taskId: '-', stage: 'checkout', detail: `cannot checkout ${baseBranch}: ${checkout.stderr.trim().slice(0, 200)}` },
    };
  }

  const merged: string[] = [];
  for (const id of topoOrder(board)) {
    if (!doneIds.has(id)) continue;
    const task = board.tasks.find((t) => t.id === id)!;
    const branch = `devagent/${id}-a${task.attempts}`;
    const m = await git(['merge', '--no-ff', '--no-edit', branch], repoPath);
    if (m.exitCode !== 0) {
      await git(['merge', '--abort'], repoPath); // leave the tree clean
      return {
        ok: false,
        merged,
        failure: { taskId: id, stage: 'merge', detail: m.stderr.trim().slice(0, 300) || `merge ${branch} failed` },
      };
    }
    // Gate the integrated tree after every merge
    const { runTestGate } = await import('../validation/test-gate.js');
    const g1 = await runTestGate(repoPath, 10 * 60_000);
    if (!g1.passed) {
      // roll back this merge; earlier merges stay (they passed their gates)
      const rollback = await git(['reset', '--hard', 'HEAD@{1}'], repoPath);
      return {
        ok: false,
        merged,
        failure: {
          taskId: id,
          stage: 'gate',
          detail: rollback.exitCode === 0 ? `integrated tests failed: ${g1.detail?.slice(0, 200) ?? 'unknown'}` : `tests failed and rollback failed: ${g1.detail?.slice(0, 150)}`,
        },
      };
    }
    merged.push(id);
    log.info('task', `Merged ${branch} into ${baseBranch}`, {});
  }
  return { ok: true, merged };
}
