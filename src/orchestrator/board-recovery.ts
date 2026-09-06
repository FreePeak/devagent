import { accessSync, constants, existsSync, mkdirSync, readdirSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { join } from 'node:path';
import { BOARD_FILE } from './types.js';
import { loadConfig } from '../config.js';
import { postOperatorAlert } from '../resilience/operator-alert.js';
import type { OperatorNotifier } from '../resilience/operator-alert.js';

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
 *                                 attempts reset to 0; a task whose
 *                                 lifetime `totalAttempts` reaches the
 *                                 cumulative cap Q17/Q36 is refused the
 *                                 reset and stays dispatch-dead), then
 *                                 re-inspect:
 *     still failed/blocked      → archive board-stuck (fires in practice
 *                                 when the cap refused every dead task;
 *                                 defensively also if the reset write
 *                                 failed)
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
 *
 * Q16 (PRD:913) — archive is now a signalled, bounded event. Every board the
 * gate moves into .devagent/archive/ (both the completed-board and the
 * stuck-board verdicts) fires a best-effort `board-archived` operator alert
 * through the shared paging transport (src/resilience/operator-alert.ts, the
 * same injectable `notify` seam Q41 uses) and prunes the archive to the
 * newest `resilience.archiveKeep` boards. Paging/retention are observability:
 * neither may turn a successful archive into a cycle failure. The shell's old
 * inline `mv` (which archived with zero signal) is gone — archiving flows only
 * through this gate.
 */

/** Statuses the scheduler can never dispatch again (the dispatch-dead pair). */
const DEAD_STATUSES: readonly string[] = ['failed', 'blocked'];

/** Board task shape as read back from JSON (statuses are validated loosely, mirroring the shell's `node -e` leniency). */
interface BoardTaskLike {
  status?: unknown;
  attempts?: unknown;
  totalAttempts?: unknown;
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
  /**
   * Cumulative lifetime dispatch cap (Q17/Q36): a failed/blocked task whose
   * `totalAttempts` reaches it is refused the requeue reset and stays
   * terminal. 0/undefined = unbounded (legacy: every requeue round handed
   * out a fresh `maxTaskRetries` budget — the loops 53-55 re-burn class).
   */
  maxTotalAttempts?: number;
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
 * The stuck branch fires when the cumulative attempt cap (Q17/Q36) refused
 * the reset for at least one dead task — those stay failed/blocked and the
 * board is archived; defensively it also covers a failed reset write (which
 * throws, exiting the gate unresolved). The all-pending branch is the
 * original real one: a board with no done task never recomputes readiness, so
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

/** Outcome of a requeue pass: resets performed vs refused by the cumulative cap. */
export interface RequeueResult {
  /** Dead tasks reset to pending with a fresh per-round attempts budget. */
  reset: number;
  /** Dead tasks at/above the cumulative cap — refused, left terminal (Q17/Q36). */
  capped: number;
}

/**
 * Port of requeue_parked(): failed/blocked → pending with the attempts
 * budget reset; writes only when something changed, preserving the shell's
 * byte-for-byte JSON shape (2-space indent, trailing newline). tmp+rename
 * so a crash mid-write cannot corrupt the board. Above the cumulative cap
 * (Q17/Q36, 0 = unbounded) a dead task is refused the fresh budget and
 * stays failed/blocked; `totalAttempts` itself is never reset. Missing or
 * non-numeric lifetime history counts as zero, mirroring the shell's
 * leniency on hand-edited boards.
 */
export function requeueParked(boardPath: string, board: BoardFile, maxTotalAttempts = 0): RequeueResult {
  let reset = 0;
  let capped = 0;
  for (const t of board.tasks) {
    if (!t || !DEAD_STATUSES.includes(String(t.status))) continue;
    const total = Number(t.totalAttempts);
    if (maxTotalAttempts > 0 && Number.isFinite(total) && total >= maxTotalAttempts) {
      capped++;
      continue;
    }
    t.status = 'pending';
    t.attempts = 0;
    reset++;
  }
  if (reset > 0) {
    const tmp = `${boardPath}.tmp`;
    writeFileSync(tmp, JSON.stringify(board.raw, null, 2) + '\n');
    renameSync(tmp, boardPath);
  }
  return { reset, capped };
}

/** Default bound for .devagent/archive/ retention (Q16): the newest N stamped archives are kept. */
export const ARCHIVE_RETENTION_KEEP = 20;

/** Archive filenames carry a `YYYYMMDD-HHMMSS` stamp; only these are pruned. */
const ARCHIVE_NAME = /^(?:board|board-stuck)-(\d{8}-\d{6})\.json$/;

/**
 * Bounded retention prune of .devagent/archive/ (Q16): keep the newest `keep`
 * stamped archives, delete the rest. Ordering is by the filename stamp (not
 * mtime) so it is deterministic under an injected clock; files that do not
 * match the archive pattern are never touched. `keep` <= 0 / non-finite is
 * unbounded (never prune). Best-effort: a failed unlink is swallowed — a
 * retention miss must not break the recovery cycle. Returns the removed names.
 */
export function pruneArchive(repoPath: string, opts: { keep?: number } = {}): string[] {
  const keep = opts.keep ?? ARCHIVE_RETENTION_KEEP;
  if (!Number.isFinite(keep) || keep <= 0) return [];
  const dir = join(repoPath, '.devagent', 'archive');
  let names: string[];
  try {
    names = readdirSync(dir);
  } catch {
    return []; // no archive dir yet — nothing to prune
  }
  const stamped = names
    .map((name) => ({ name, stamp: ARCHIVE_NAME.exec(name)?.[1] }))
    .filter((x): x is { name: string; stamp: string } => x.stamp !== undefined)
    .sort((a, b) => (a.stamp < b.stamp ? 1 : a.stamp > b.stamp ? -1 : 0)); // newest first
  const removed: string[] = [];
  for (const { name } of stamped.slice(keep)) {
    try {
      rmSync(join(dir, name));
      removed.push(name);
    } catch {
      // a single stubborn file must not abort the prune or the cycle
    }
  }
  return removed;
}

/** Options for archiveBoard's best-effort Q16 side effects (tests inject them). */
export interface ArchiveBoardOptions {
  /** Operator webhook URL; absent/empty = no paging (opt-in, like Q41). */
  url?: string;
  /** Injection seam for tests: outbound paging transport. Defaults to postOperatorAlert. */
  notify?: OperatorNotifier;
  /** Injectable clock for the alert ts (tests). */
  now?: Date;
  /** Human-readable why, carried onto the board-archived alert. */
  reason?: string;
  /** Retention bound; unset = ARCHIVE_RETENTION_KEEP. */
  keep?: number;
}

/**
 * Move the board into .devagent/archive/ under `prefix-<stamp>.json`, then
 * fire the best-effort `board-archived` operator alert and prune the archive
 * to the retention bound (Q16). Returns the repo-relative path. Paging and
 * retention never fail the archive: a missing webhook, a broken config, a
 * transport throw or a stubborn unlink are all swallowed.
 */
export async function archiveBoard(
  repoPath: string,
  boardPath: string,
  prefix: 'board' | 'board-stuck',
  stamp: string,
  opts: ArchiveBoardOptions = {},
): Promise<string> {
  const dir = join(repoPath, '.devagent', 'archive');
  mkdirSync(dir, { recursive: true });
  const rel = join('.devagent', 'archive', `${prefix}-${stamp}.json`);
  renameSync(boardPath, join(repoPath, rel));
  if (opts.url) {
    const notify = opts.notify ?? postOperatorAlert;
    try {
      await notify(opts.url, {
        event: 'board-archived',
        ts: (opts.now ?? new Date()).toISOString(),
        repo: repoPath,
        prefix,
        path: rel,
        reason: opts.reason ?? `${prefix} board archived`,
      });
    } catch {
      // paging is observability, never a failure signal for the loop (Q16)
    }
  }
  pruneArchive(repoPath, { keep: opts.keep });
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
  /** Injection seam for tests: outbound operator paging transport (Q16). */
  notify?: OperatorNotifier;
}

/**
 * One recovery cycle: read the board, decide, perform the action, return the
 * verdict. Mutates the board file (requeue) and moves it (archive) exactly
 * where the shell did. Throws only when the board vanishes between the
 * decision and the action — the CLI maps that to exit 2 (unresolved → the
 * shell falls back to wait).
 */
export async function runBoardRecovery(
  repoPath: string,
  opts: RunBoardRecoveryOptions,
): Promise<BoardRecoveryVerdict> {
  const boardPath = join(repoPath, BOARD_FILE);
  const board = readBoard(boardPath);
  if (!board) return { action: 'wait', reason: 'board unreadable' };
  const stamp = formatTimestamp(opts.now ?? new Date());
  const sleep = `; sleeping ${opts.pollSecs}s`;
  // Q16: resolve the paging webhook + retention bound once. A broken config
  // must not turn an archive into a cycle failure, so paging stays opt-in and
  // the retention bound falls back to ARCHIVE_RETENTION_KEEP.
  let url: string | undefined;
  let keep: number | undefined;
  try {
    const r = loadConfig(repoPath).resilience;
    url = r?.degradeWebhookUrl;
    keep = r?.archiveKeep;
  } catch {
    // unreadable devagent.json: archive proceeds without paging/retention override
  }

  const counts = countBoard(board.tasks);
  const intent = decideBoardRecovery(counts, opts);
  if (intent.kind === 'wait') return { action: 'wait', reason: intent.reason };
  if (intent.kind === 'archive-complete') {
    const rel = await archiveBoard(repoPath, boardPath, 'board', stamp, {
      url,
      keep,
      notify: opts.notify,
      now: opts.now,
      reason: `board complete (${counts.done} done)`,
    });
    pruneMergedWorktrees(repoPath);
    return { action: 'wait', reason: `board complete (${counts.done} done); archived to ${rel}` };
  }

  const { reset, capped } = requeueParked(boardPath, board, opts.maxTotalAttempts ?? 0);
  const post = decidePostRequeue(countBoard(board.tasks));
  if (post.kind === 'requeue') {
    return { action: 'requeue', reason: `reset ${reset} parked task(s) to pending${sleep}` };
  }
  // capped > 0 guarantees stuck > 0 here, so refusals always land on this
  // archive verdict: the tasks stay terminal and the loop re-bridges.
  const capNote =
    capped > 0 ? `${capped} task(s) over cumulative attempt cap ${opts.maxTotalAttempts ?? 0}; ` : '';
  const rel = await archiveBoard(repoPath, boardPath, 'board-stuck', stamp, {
    url,
    keep,
    notify: opts.notify,
    now: opts.now,
    reason: `${capNote}${post.detail}`,
  });
  return {
    action: 'archive',
    reason: `reset ${reset} parked task(s) to pending; ${capNote}${post.detail}; archived to ${rel}`,
  };
}
