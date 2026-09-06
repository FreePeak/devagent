import { readEvents } from '../lessons/guard.js';

/**
 * Consecutive cross-role provider degradation (Q41).
 *
 * Why: the self-build starvation gate deliberately exempts degraded rows —
 * one pause is expected, not a thrashing loop (scripts/selfbuild-loop.sh:190
 * skips operator-degraded | operator-diverged | provider-degraded). The
 * exemption makes a sustained outage invisible on every human surface:
 * loops 106-108 logged three provider-degraded rows in 75s and nothing
 * surfaced it. This module is the read-side aggregation that closes that
 * gap for `devagent status --providers`.
 *
 * Contract: pure, read-only. Walk ledger rows newest-first, counting the
 * trailing run of degradation rows and stopping at the first productive
 * row. No new ledger status, no writes, no typed-union/TUI plumbing.
 */

/** Trailing degraded rows that surface as a provider-outage breach line. */
export const DEGRADE_STREAK_THRESHOLD = 3;

/**
 * loop-result statuses meaning "the loop paused without provider progress":
 * provider-degraded (preflight found the provider down; no spend) and
 * operator-diverged (doc-sync hit a history the operator must reconcile).
 * Every other loop-result status (ok, failed, failed-tests, invalid,
 * skipped, push-failed) means the provider answered — productive.
 */
const DEGRADED_LOOP_RESULT_STATUSES: readonly string[] = ['provider-degraded', 'operator-diverged'];

/** Aggregated trailing-degradation view over the orchestration ledger. */
export interface DegradationStreak {
  /** Length of the trailing run of degradation rows (0 when the newest row is productive). */
  count: number;
  /** Threshold the breach check used. */
  threshold: number;
  /** count >= threshold (never true at count 0). */
  breach: boolean;
  /** ts of the newest degradation row (null when count is 0). */
  latestTs: string | null;
  /** ts of the oldest degradation row in the run — the outage window start. */
  oldestTs: string | null;
  /** latestTs - oldestTs in ms; null unless both streak edges have parseable ts. */
  windowMs: number | null;
  /** Distinct roles seen on operator-degraded rows in the run, newest first. */
  roles: string[];
}

/**
 * Classify one ledger row as degradation evidence:
 * - `operator-degraded` events (the Q40 preflight rows) with ok !== true.
 *   Deliberately not filtered by role: the gate only ever writes the five
 *   PREFLIGHT_ROLES, and the streak is meant to be cross-role — an
 *   ok:true row is a passed probe (recovery), so it is productive.
 * - `loop-result` rows with status provider-degraded | operator-diverged.
 * Everything else (audit rows, lessons-eval, loop-phase, healthy
 * loop-results, ...) is productive: the first such row stops the walk.
 */
export function isDegradationRow(row: Record<string, unknown>): boolean {
  if (row.event === 'operator-degraded') return row.ok !== true;
  if (row.event === 'loop-result') {
    return typeof row.status === 'string' && DEGRADED_LOOP_RESULT_STATUSES.includes(row.status);
  }
  return false;
}

/**
 * Pure aggregation over ledger rows in file order (oldest first, as
 * appended). Walks newest-first counting the trailing degradation run;
 * stops at the first productive row. O(rows) worst case, O(streak) typical.
 */
export function degradationStreak(
  rows: Record<string, unknown>[],
  threshold: number = DEGRADE_STREAK_THRESHOLD,
): DegradationStreak {
  let latestTs: string | null = null;
  let oldestTs: string | null = null;
  let newestMs: number | null = null; // parsed ts of the newest degraded row (null = unparseable)
  let oldestMs: number | null = null; // parsed ts of the oldest degraded row so far
  const roles: string[] = [];
  let count = 0;

  for (let i = rows.length - 1; i >= 0; i--) {
    const row = rows[i];
    if (!row || !isDegradationRow(row)) break;
    count += 1;
    if (typeof row.ts === 'string') {
      if (latestTs === null) latestTs = row.ts;
      oldestTs = row.ts;
    }
    const tsMs = typeof row.ts === 'string' ? Date.parse(row.ts) : NaN;
    const parsed = Number.isFinite(tsMs) ? tsMs : null;
    // The window is only meaningful when both streak edges parse: the first
    // degraded row fixes newestMs; every further row overwrites oldestMs.
    if (count === 1) newestMs = parsed;
    oldestMs = parsed;
    if (row.event === 'operator-degraded' && typeof row.role === 'string' && row.role && !roles.includes(row.role)) {
      roles.push(row.role);
    }
  }

  return {
    count,
    threshold,
    breach: count > 0 && count >= threshold,
    latestTs,
    oldestTs,
    windowMs: newestMs !== null && oldestMs !== null ? newestMs - oldestMs : null,
    roles,
  };
}

/**
 * Read-side helper: parse the repo's orchestration events.jsonl
 * (best-effort — corrupt lines skipped, absent file = no rows) and compute
 * the trailing degradation streak for `devagent status --providers`.
 */
export function readDegradationStreak(
  repoPath: string,
  threshold: number = DEGRADE_STREAK_THRESHOLD,
): DegradationStreak {
  return degradationStreak(readEvents(repoPath), threshold);
}
