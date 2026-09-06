import { accessSync, constants, existsSync, mkdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { join } from 'node:path';
import { BOARD_FILE } from './types.js';

/**
 * Orchestrator board recovery (PRD:888 Q19) — the board-recovery decisions
 * folded out of scripts/orchestrate-loop.sh into typed, tested code. The
 * shell is a thin caller via `devagent board-recovery` (src/cli.ts), using
 * the same seam pattern as selfbuild-gate.ts (loops 121/122): the gate
 * PERFORMS the action it verdicts (requeue write, board archive, merged-
 * worktree prune) and prints exactly one verdict line
 * `<wait|requeue|archive>: <reason>`; the shell only maps the verdict to
 * loop control:
 *
 *   wait     sleep POLL_SECS, continue (parked below threshold, requeue
 *            disabled, or a completed board already archived by the gate)
 *   requeue  parked tasks reset to pending; the shell clears its poll
 *            counter, sleeps, and lets the next cycle dispatch them
 *   archive  stuck board archived; the shell falls through to the queue
 *            bridge THIS cycle (#73: previously the parked block always did
 *            `sleep POLL_SECS; continue`, leaving the board absent and the
 *            factory idle for a whole poll interval)
 *
 * Exit-code contract is backlog-check's (PRD:889): 0 = verdict printed,
 * 2 = unresolved (caller misuse / board vanished mid-cycle). A crashed CLI
 * also exits nonzero, so the shell only honors a verdict word it can read
 * back from the output — the fallback is wait: a crashed gate must never
 * archive a board or hot-loop the factory.
 *
 * Decision table (ported verbatim from the shell, scripts/orchestrate-loop.sh
 * :138-196): the gate is consulted only when the board has no open tasks
 * (open = not done/failed/blocked) and at least one task.
 *   - every task done           → archive the completed board so the next
 *                                 iteration re-bridges scouted queue items
 *                                 and plans a fresh board from the goal
 *                                 (infinity cycle, #42); merged worktrees
 *                                 are pruned alongside. Verdict: wait.
 *   - all failed/blocked,
 *     requeue disabled          → wait (parked, logged)
 *   - all failed/blocked,
 *     parked polls < threshold  → wait (parked, logged with P/Q counter)
 *   - threshold reached         → requeue (failed/blocked → pending,
 *                                 attempts reset to 0), then re-inspect:
 *     still failed/blocked      → archive board-stuck (defensive: requeue
 *                                 clears every dispatch-dead status, so this
 *                                 only fires if the reset write failed)
 *     every task pending        → the scheduler still cannot dispatch
 *                                 (readiness never recomputes for a board
 *                                 with no done tasks to unblock deps), so
 *                                 archive board-stuck and let the queue
 *                                 bridge take over
 *     otherwise                 → requeue verdict (some tasks done; the
 *                                 reset pending tasks dispatch next cycle)
 *
 * The parked-poll counter is LOOP state, not board state: the shell owns it
 * (incremented per parked cycle, cleared on any action verdict or when the
 * board has open work) and passes the post-increment value via
 * --parked-polls. Folding it into the gate would require persisting driver
 * state the board file does not carry.
 */

/** Statuses the scheduler can never dispatch again (the dispatch-dead pair). */
const DEAD_STATUSES: readonly string[] = ['failed', 'blocked'];

/** Board task shape as read back from JSON (statuses are validated loosely, mirroring the shell's `node -e` leniency). */
interface BoardTaskLike {
  status?: unknown;
  attempts?: unknown;
}

/** Parsed board file: the raw object (written back verbatim after a requeue)
 * plus its validated tasks array — the same array reference `raw.tasks` holds. */
interface BoardFile {
  raw: object;
  tasks: BoardTaskLike[];
}

/** Raw task counts the decision table reads, mirroring the shell's board_* helpers. */
export interface BoardCounts {
  total: number;
  done: number;
  /** Not done/failed/blocked — everything the loop may still dispatch. */
  open: number;
  /** failed/blocked — the dispatch-dead states. */
  stuck: number;
  pending: number;
}

export interface RecoveryOptions {
  /** Parked cycles including this one (the shell's post-increment counter). */
  parkedPolls: number;
  /** Requeue threshold in parked polls; 0 = never requeue. */
  requeueAfter: number;
  /** Loop sleep seconds, quoted in wait/requeue verdict text. */
  pollSecs: number;
}

export type RecoveryAction = 'wait' | 'requeue' | 'archive';

export interface BoardRecoveryVerdict {
  action: RecoveryAction;
  reason: string;
}

/** First-cycle intent: what the parked board needs before any mutation. */
export type RecoveryIntent =
  | { kind: 'archive-complete' }
  | { kind: 'requeue-now' }
  | { kind: 'wait'; reason: string };

/** Post-requeue intent: whether the reset board is still undispatchable. */
export type PostRequeueIntent =
  | { kind: 'archive'; detail: string }
  | { kind: 'requeue' };

/** Count the board the way the shell's board_* helpers did. */
export function countBoard(tasks: readonly BoardTaskLike[]): BoardCounts {
  let done = 0;
  let stuck = 0;
  let pending = 0;
  for (const t of tasks) {
    const status = t?.status;
    if (status === 'done') done++;
    else if (DEAD_STATUSES.includes(String(status))) stuck++;
    if (status === 'pending') pending++;
  }
  const total = tasks.length;
  return { total, done, open: total - done - stuck, stuck, pending };
}

/**
 * Pre-mutation decision: completed board, parked wait, or threshold reached.
 * `open > 0` / `total === 0` are defensive (the shell guard keeps the gate
 * out of those paths); they verdict wait, never an action.
 */
export function decideBoardRecovery(counts: BoardCounts, opts: RecoveryOptions): RecoveryIntent {
  if (counts.total === 0) return { kind: 'wait', reason: 'board has no tasks' };
  if (counts.open > 0) return { kind: 'wait', reason: `board has ${counts.open} open task(s)` };
  if (counts.done === counts.total) return { kind: 'archive-complete' };
  const sleep = `; sleeping ${opts.pollSecs}s`;
  if (opts.requeueAfter <= 0) {
    return { kind: 'wait', reason: `${counts.stuck} task(s) failed/blocked${sleep} (requeue disabled)` };
  }
  if (opts.parkedPolls < opts.requeueAfter) {
    return {
      kind: 'wait',
      reason: `${counts.stuck} task(s) failed/blocked (${opts.parkedPolls}/${opts.requeueAfter})${sleep}`,
    };
  }
  return { kind: 'requeue-now' };
}

/**
 * Post-requeue decision, over the counts recomputed AFTER the reset write.
 * The stuck branch is defensive table parity — requeue clears every
 * dispatch-dead status (and a failed reset write throws, exiting the gate
 * unresolved), so it cannot fire in practice. The all-pending branch is the
 * real one: a board with no done task never recomputes readiness, so
 * requeue cannot unstick it; archive and let the queue bridge take over.
 */
export function decidePostRequeue(counts: BoardCounts): PostRequeueIntent {
  if (counts.stuck > 0) return { kind: 'archive', detail: `board stuck (${counts.stuck} failed/blocked)` };
  if (counts.total > 0 && counts.pending === counts.total) {
    return { kind: 'archive', detail: 'board all-pending but undispatchable' };
  }
  return { kind: 'requeue' };
}

/** Render a verdict the way the CLI prints it: the shell parses the word before the first colon. */
export function formatVerdict(verdict: BoardRecoveryVerdict): string {
  return `${verdict.action}: ${verdict.reason}`;
}

/** `date +%Y%m%d-%H%M%S` in local time — the archive filename stamp the shell used. */
export function formatTimestamp(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}${pad(d.getMonth() + 1)}${pad(d.getDate())}-${pad(d.getHours())}${pad(d.getMinutes())}${pad(d.getSeconds())}`;
}

function readBoard(boardPath: string): BoardFile | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(readFileSync(boardPath, 'utf8'));
  } catch {
    return null;
  }
  if (!parsed || typeof parsed !== 'object' || !('tasks' in parsed)) return null;
  if (!Array.isArray(parsed.tasks)) return null;
  // Elements stay unknown-shaped: every access below is optional-guarded,
  // mirroring the shell's `t.status` leniency on hand-edited boards.
  const tasks = parsed.tasks as BoardTaskLike[];
  return { raw: parsed, tasks };
}

/**
 * Port of requeue_parked(): failed/blocked → pending with the attempts
 * budget reset; writes only when something changed, preserving the shell's
 * byte-for-byte JSON shape (2-space indent, trailing newline). tmp+rename
 * so a crash mid-write cannot corrupt the board.
 */
export function requeueParked(boardPath: string, board: BoardFile): number {
  let n = 0;
  for (const t of board.tasks) {
    if (t && DEAD_STATUSES.includes(String(t.status))) {
      t.status = 'pending';
      t.attempts = 0;
      n++;
    }
  }
  if (n > 0) {
    const tmp = `${boardPath}.tmp`;
    writeFileSync(tmp, JSON.stringify(board.raw, null, 2) + '\n');
    renameSync(tmp, boardPath);
  }
  return n;
}

/** Move the board into .devagent/archive/ under `prefix-<stamp>.json`; returns the repo-relative path. */
export function archiveBoard(
  repoPath: string,
  boardPath: string,
  prefix: 'board' | 'board-stuck',
  stamp: string,
): string {
  const dir = join(repoPath, '.devagent', 'archive');
  mkdirSync(dir, { recursive: true });
  const rel = join('.devagent', 'archive', `${prefix}-${stamp}.json`);
  renameSync(boardPath, join(repoPath, rel));
  return rel;
}

/** Safe-gated merged-worktree prune (the shell's cleanup_merged_worktrees): only an executable repo script, never fatal. */
function pruneMergedWorktrees(repoPath: string): void {
  const script = join(repoPath, 'scripts', 'git-cleanup-merged.sh');
  if (!existsSync(script)) return;
  try {
    accessSync(script, constants.X_OK);
  } catch {
    return;
  }
  try {
    spawnSync(script, ['--root', repoPath, '--apply'], { stdio: 'ignore' });
  } catch {
    // cleanup is opportunistic; a failed prune must not break the cycle
  }
}

export interface RunBoardRecoveryOptions extends RecoveryOptions {
  /** Injectable clock for the archive stamp (tests). */
  now?: Date;
}

/**
 * One recovery cycle: read the board, decide, perform the action, return the
 * verdict. Mutates the board file (requeue) and moves it (archive) exactly
 * where the shell did. Throws only when the board vanishes between the
 * decision and the action — the CLI maps that to exit 2 (unresolved → the
 * shell falls back to wait).
 */
export function runBoardRecovery(repoPath: string, opts: RunBoardRecoveryOptions): BoardRecoveryVerdict {
  const boardPath = join(repoPath, BOARD_FILE);
  const board = readBoard(boardPath);
  if (!board) return { action: 'wait', reason: 'board unreadable' };
  const stamp = formatTimestamp(opts.now ?? new Date());
  const sleep = `; sleeping ${opts.pollSecs}s`;

  const counts = countBoard(board.tasks);
  const intent = decideBoardRecovery(counts, opts);
  if (intent.kind === 'wait') return { action: 'wait', reason: intent.reason };
  if (intent.kind === 'archive-complete') {
    const rel = archiveBoard(repoPath, boardPath, 'board', stamp);
    pruneMergedWorktrees(repoPath);
    return { action: 'wait', reason: `board complete (${counts.done} done); archived to ${rel}` };
  }

  const reset = requeueParked(boardPath, board);
  const post = decidePostRequeue(countBoard(board.tasks));
  if (post.kind === 'requeue') {
    return { action: 'requeue', reason: `reset ${reset} parked task(s) to pending${sleep}` };
  }
  const rel = archiveBoard(repoPath, boardPath, 'board-stuck', stamp);
  return {
    action: 'archive',
    reason: `reset ${reset} parked task(s) to pending; ${post.detail}; archived to ${rel}`,
  };
}
