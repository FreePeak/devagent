import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { lessonExcerptHash } from './guard.js';

/**
 * Lessons impact telemetry (PRD Phase 4 backlog, docs/PRD.md §17, Q39):
 * aggregate the eval guard's accept/reject outcomes against subsequent loop
 * results into a deterministic measured-effect score per lesson entry, then
 * rank the 4000-char lessonsMaxChars digest by that score (recency as
 * tiebreak) instead of recency alone. This is the prerequisite for the
 * AHE-style held-out evaluation tier: a lesson that predicts an impact must
 * be falsifiable against the loop ledger.
 *
 * Deterministic oracles only (tathadn rule): suite green (encoded in the
 * lessons-eval verdict), gate results, and ledger merge outcomes. No LLM
 * judgement is involved — the score is a pure function of the two ledger
 * files (lessons-eval rows + loop outcomes).
 *
 * Sources:
 *  - lessons-eval rows: one accept/reject row per gated append, written by
 *    `appendLessonGuarded` (src/lessons/guard.ts) to
 *    .devagent/runs/orchestration/events.jsonl. Carries excerptHash,
 *    accepted, predictedImpact, suite, ts.
 *  - loop ledger: .selfbuild/ledger.jsonl rows {loop, ts, status, goal}
 *    written by the self-build loop drivers.
 *
 * Score per lesson id (excerptHash), in [0, 1]:
 *  - never accepted -> 0 (never entered the digest, no effect to measure);
 *  - accepted at ts0 -> successes / (successes + failures) over loop rows
 *    with ts > ts0, where success = ok|pr-open|merged|pushed and failure =
 *    failed|failed-tests|invalid|push-failed. `skipped` loops and unknown
 *    statuses are infrastructure gaps, not lesson-related outcomes, so they
 *    are excluded from both numerator and denominator. No counted loops -> 0
 *    (no evidence yet: zero-effect, sinks to the bottom of the ranked digest).
 *
 * The ledger rows are the persistence (one accept/reject verdict per entry);
 * the score is recomputed from them on every digest build, so it can never go
 * stale. The digest builder degrades to the pre-Q39 recency cursor when no
 * lessons-eval rows exist at all.
 */

/** Ledger dir + file the eval guard writes lessons-eval rows into. */
export const EVENTS_LEDGER_FILE = '.devagent/runs/orchestration/events.jsonl';

/** Loop ledger the self-build loop drivers append one row per iteration to. */
export const LOOP_LEDGER_FILE = '.selfbuild/ledger.jsonl';

/** Loop statuses counted as productive outcomes (matches selfbuild drivers). */
const PRODUCTIVE_STATUSES: Record<string, true> = { ok: true, 'pr-open': true, merged: true, pushed: true };

/** Loop statuses counted as failures. `skipped`/unknown stay out of both. */
const FAILURE_STATUSES: Record<string, true> = { failed: true, 'failed-tests': true, invalid: true, 'push-failed': true };

/** One measured-effect score per lesson entry (key = excerptHash). */
export interface LessonImpact {
  /** Stable content hash of the lesson entry (the per-entry key). */
  excerptHash: string;
  /** Whether the eval guard accepted the lesson (ever). */
  accepted: boolean;
  /** The predictedImpact captured on the first accepted lessons-eval row. */
  predictedImpact: string;
  /** ISO timestamp of the first accepted row (the lesson's digest entry point). */
  ts: string;
  /** Measured effect in [0, 1]; 0 = never accepted or no counted loop evidence. */
  measuredEffect: number;
  /** Number of subsequent loop outcomes counted (successes + failures). */
  loops: number;
  /** Number of subsequent loop outcomes that were productive. */
  successes: number;
}

/** Parse a lessons-eval row from the events ledger; null when not one. */
export function parseLessonsEvalRow(
  raw: string,
): { ts: string; excerptHash: string; accepted: boolean; predictedImpact: string } | null {
  try {
    const parsed = JSON.parse(raw) as Record<string, unknown>;
    if (parsed.event !== 'lessons-eval' || typeof parsed.excerptHash !== 'string') return null;
    return {
      ts: typeof parsed.ts === 'string' ? parsed.ts : '',
      excerptHash: parsed.excerptHash,
      accepted: parsed.accepted === true,
      predictedImpact: typeof parsed.predictedImpact === 'string' ? parsed.predictedImpact : '',
    };
  } catch {
    return null;
  }
}

/** Parse a loop ledger row; null when malformed. */
export function parseLoopLedgerRow(raw: string): { ts: string; status: string } | null {
  try {
    const parsed = JSON.parse(raw) as Record<string, unknown>;
    if (typeof parsed.ts !== 'string' || typeof parsed.status !== 'string') return null;
    return { ts: parsed.ts, status: parsed.status };
  } catch {
    return null;
  }
}

/**
 * Aggregate lessons-eval rows into per-excerptHash verdicts. The lesson's
 * digest entry point is the EARLIEST accepted row's ts: a rejected attempt
 * that is later accepted enters the digest at acceptance, and a later
 * rejected duplicate does not move the entry point of an accepted lesson.
 * Rejected-only lessons keep the earliest row ts (unused for scoring).
 */
export function aggregateEvalRows(
  evalRows: Array<{ ts: string; excerptHash: string; accepted: boolean; predictedImpact: string }>,
): Map<string, { accepted: boolean; ts: string; predictedImpact: string }> {
  const byHash = new Map<string, { accepted: boolean; ts: string; predictedImpact: string }>();
  for (const row of evalRows) {
    const prev = byHash.get(row.excerptHash);
    if (!prev) {
      byHash.set(row.excerptHash, {
        accepted: row.accepted,
        ts: row.ts,
        predictedImpact: row.predictedImpact,
      });
      continue;
    }
    if (row.accepted) {
      if (!prev.accepted || row.ts < prev.ts) {
        prev.accepted = true;
        prev.ts = row.ts;
        prev.predictedImpact = row.predictedImpact;
      }
    } else if (!prev.accepted && row.ts < prev.ts) {
      prev.ts = row.ts;
    }
  }
  return byHash;
}

/**
 * Compute measured-effect scores for every lesson that has a lessons-eval
 * ledger row, correlating each accepted lesson's digest entry point against
 * subsequent loop outcomes. Pure function over parsed rows so tests can feed
 * synthetic fixtures without touching the filesystem.
 */
export function computeLessonImpact(
  evalRows: Array<{ ts: string; excerptHash: string; accepted: boolean; predictedImpact: string }>,
  loopRows: Array<{ ts: string; status: string }>,
): Map<string, LessonImpact> {
  const verdicts = aggregateEvalRows(evalRows);
  const loops = loopRows
    .filter((r) => r.ts && r.status)
    .sort((a, b) => a.ts.localeCompare(b.ts));

  const out = new Map<string, LessonImpact>();
  for (const [excerptHash, v] of verdicts) {
    if (!v.accepted) {
      out.set(excerptHash, {
        excerptHash,
        accepted: false,
        predictedImpact: v.predictedImpact,
        ts: v.ts,
        measuredEffect: 0,
        loops: 0,
        successes: 0,
      });
      continue;
    }
    let successes = 0;
    let counted = 0;
    for (const loop of loops) {
      if (loop.ts <= v.ts) continue;
      if (PRODUCTIVE_STATUSES[loop.status]) {
        successes++;
        counted++;
      } else if (FAILURE_STATUSES[loop.status]) {
        counted++;
      }
      // skipped + unknown statuses: excluded from both numerator and denominator
    }
    out.set(excerptHash, {
      excerptHash,
      accepted: true,
      predictedImpact: v.predictedImpact,
      ts: v.ts,
      measuredEffect: counted > 0 ? successes / counted : 0,
      loops: counted,
      successes,
    });
  }
  return out;
}

/**
 * Read the two ledger files for a repo and return their parsed rows. Absent
 * or unreadable files contribute nothing (empty inputs), never throw.
 */
export function readLedgers(repoPath: string): {
  evalRows: Array<{ ts: string; excerptHash: string; accepted: boolean; predictedImpact: string }>;
  loopRows: Array<{ ts: string; status: string }>;
} {
  const evalRows: Array<{ ts: string; excerptHash: string; accepted: boolean; predictedImpact: string }> = [];
  const loopRows: Array<{ ts: string; status: string }> = [];
  try {
    const eventsPath = join(repoPath, EVENTS_LEDGER_FILE);
    if (existsSync(eventsPath)) {
      for (const line of readFileSync(eventsPath, 'utf8').split('\n')) {
        const row = parseLessonsEvalRow(line);
        if (row) evalRows.push(row);
      }
    }
  } catch {
    // best-effort: a broken ledger means "no evidence", not a crash
  }
  try {
    const loopPath = join(repoPath, LOOP_LEDGER_FILE);
    if (existsSync(loopPath)) {
      for (const line of readFileSync(loopPath, 'utf8').split('\n')) {
        const row = parseLoopLedgerRow(line);
        if (row) loopRows.push(row);
      }
    }
  } catch {
    // best-effort
  }
  return { evalRows, loopRows };
}

/** One-line entry point for the digest builder: ledgers on disk -> impact map. */
export function computeRepoLessonImpact(repoPath: string): Map<string, LessonImpact> {
  const { evalRows, loopRows } = readLedgers(repoPath);
  return computeLessonImpact(evalRows, loopRows);
}

/**
 * Rank lesson lines by measured effect (desc), recency as tiebreak:
 *  - both lines carry a ledger ts -> newer ts first;
 *  - otherwise -> later file line first (append-only ratchet order).
 *
 * Lines with no ledger row (legacy lessons, `## date` headers, fences) have no
 * measured effect, so they sink below every scored lesson and order by
 * recency among themselves. Structural lines are never hashed (a header must
 * not collide with a lesson's digest key).
 *
 * Returns null when NO line carries an accepted (scored) impact verdict, so
 * callers can keep the pre-Q39 recency cursor byte-for-byte in repos without
 * lessons-eval evidence instead of reordering on zero data.
 */
export function rankLinesByImpact(lines: string[], impact: Map<string, LessonImpact>): string[] | null {
  const scored = lines.map((line, idx) => {
    const trimmed = line.trim();
    const isStructural = !trimmed || trimmed === '---' || /^#{1,6}\s/.test(trimmed);
    const hash = isStructural ? '' : lessonExcerptHash(line);
    const score = hash ? impact.get(hash) : undefined;
    return {
      line,
      idx,
      effect: score?.accepted ? score.measuredEffect : 0,
      ts: score?.accepted ? score.ts : '',
      active: score?.accepted === true,
    };
  });
  if (!scored.some((s) => s.active)) return null;
  const sorted = [...scored].sort((a, b) => {
    if (a.effect !== b.effect) return b.effect - a.effect;
    if (a.ts && b.ts) return b.ts.localeCompare(a.ts);
    return b.idx - a.idx;
  });
  return sorted.map((s) => s.line);
}
