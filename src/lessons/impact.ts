import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { lessonExcerptHash } from './guard.js';

// ─── Types ───────────────────────────────────────────────────────────────────

/** Parsed lessons-eval ledger row from events.jsonl. */
export interface LessonsEvalRow {
  ts: string;
  excerptHash: string;
  similarity: number;
  threshold: number;
  predictedImpact: string;
  suite: 'green' | 'red' | 'skipped';
  accepted: boolean;
  reason: 'missing-predictedImpact' | 'duplicate' | 'suite-red' | 'accepted';
  entry: string;
  suiteDetail?: string;
}

/** Parsed loop outcome from .selfbuild/ledger.jsonl. */
export interface LoopOutcome {
  loop: number;
  ts: string;
  status: string;
  goal: string;
}

/** Computed measured-effect score for one lesson excerpt. */
export interface LessonScore {
  excerptHash: string;
  /** Total gated-append attempts for this excerpt hash. */
  totalAppends: number;
  /** Accepted gated-append attempts for this excerpt hash. */
  acceptedAppends: number;
  /** Fraction of appends that were accepted [0, 1]. */
  acceptRate: number;
  /** ISO timestamp of the first accepted append, or undefined. */
  firstAcceptedTs?: string;
  /** Failure rate of loops before the lesson's first acceptance (0 when no loops before). */
  beforeFailRate: number;
  /** Failure rate of loops from the lesson's first acceptance onward (0 when no loops after). */
  afterFailRate: number;
  /** beforeFailRate − afterFailRate. Positive = lesson correlated with fewer loop failures. */
  repeatFailureDelta: number;
  /** Combined score: acceptRate + repeatFailureDelta (rounded to 4 decimal places). */
  score: number;
}


// ─── Helpers ─────────────────────────────────────────────────────────────────

const PRODUCTIVE_STATUSES: Record<string, true> = {
  ok: true,
  'pr-open': true,
  merged: true,
  pushed: true,
};

function isFailure(status: string): boolean {
  return !(status in PRODUCTIVE_STATUSES);
}

function round4(n: number): number {
  return Math.round(n * 10_000) / 10_000;
}

// ─── Scoring ─────────────────────────────────────────────────────────────────

/**
 * Score each unique excerptHash from the lessons-eval ledger, using loop
 * outcomes to compute accept rate and repeat-failure delta.
 *
 * Accept rate: fraction of gated appends for this hash that were accepted
 * (higher = lesson more likely to survive the guard).
 *
 * Repeat-failure delta: baseline failure rate (loops before first acceptance)
 * minus post-lesson failure rate (loops from first acceptance). Positive means
 * the lesson's availability correlates with fewer loop failures.
 *
 * Score = acceptRate + repeatFailureDelta. Both components are in [0, 1], so
 * the combined score is in [0, 2] (negative delta would clamp but is rare
 * with a healthy lesson).
 *
 * Deterministic over the input arrays: no external state, no randomness.
 */
export function scoreLessons(
  evalRows: LessonsEvalRow[],
  loopOutcomes: LoopOutcome[],
): Map<string, LessonScore> {
  // Group eval rows by excerptHash
  const byHash = new Map<string, LessonsEvalRow[]>();
  for (const row of evalRows) {
    const group = byHash.get(row.excerptHash);
    if (group) group.push(row);
    else byHash.set(row.excerptHash, [row]);
  }

  const scores = new Map<string, LessonScore>();

  for (const [hash, rows] of byHash) {
    const totalAppends = rows.length;
    const acceptedRows = rows.filter((r) => r.accepted);
    const acceptedAppends = acceptedRows.length;
    const acceptRate = totalAppends > 0 ? round4(acceptedAppends / totalAppends) : 0;

    // First acceptance timestamp (deterministic: earliest ISO string)
    const firstAcceptedTs = acceptedRows.length > 0
      ? acceptedRows.map((r) => r.ts).sort()[0]!
      : undefined;

    // Failure rates before and after first acceptance
    let beforeFailRate = 0;
    let afterFailRate = 0;
    let repeatFailureDelta = 0;

    if (firstAcceptedTs && loopOutcomes.length > 0) {
      const before = loopOutcomes.filter((o) => o.ts < firstAcceptedTs);
      const after = loopOutcomes.filter((o) => o.ts >= firstAcceptedTs);

      if (before.length > 0) {
        beforeFailRate = before.filter((o) => isFailure(o.status)).length / before.length;
      }
      if (after.length > 0) {
        afterFailRate = after.filter((o) => isFailure(o.status)).length / after.length;
      }
      repeatFailureDelta = round4(beforeFailRate - afterFailRate);
    }

    const score = round4(acceptRate + repeatFailureDelta);

    scores.set(hash, {
      excerptHash: hash,
      totalAppends,
      acceptedAppends,
      acceptRate,
      firstAcceptedTs,
      beforeFailRate,
      afterFailRate,
      repeatFailureDelta,
      score,
    });
  }

  return scores;
}

// ─── Ledger readers ──────────────────────────────────────────────────────────

/** Parse a lessons-eval row from a raw JSON object. Returns null on bad shape. */
export function parseLessonsEvalRow(raw: Record<string, unknown>): LessonsEvalRow | null {
  if (raw.event !== 'lessons-eval') return null;
  if (typeof raw.excerptHash !== 'string') return null;
  return {
    ts: typeof raw.ts === 'string' ? raw.ts : '',
    excerptHash: raw.excerptHash as string,
    similarity: typeof raw.similarity === 'number' ? raw.similarity : 0,
    threshold: typeof raw.threshold === 'number' ? raw.threshold : 0,
    predictedImpact: typeof raw.predictedImpact === 'string' ? raw.predictedImpact : '',
    suite: (raw.suite as LessonsEvalRow['suite']) ?? 'skipped',
    accepted: raw.accepted === true,
    reason: (raw.reason as LessonsEvalRow['reason']) ?? 'missing-predictedImpact',
    entry: typeof raw.entry === 'string' ? raw.entry : '',
    suiteDetail: typeof raw.suiteDetail === 'string' ? raw.suiteDetail : undefined,
  };
}

/** Read lessons-eval rows from the repo's events.jsonl ledger. */
export function readLessonsEvalRows(repoPath: string): LessonsEvalRow[] {
  const p = join(repoPath, '.devagent', 'runs', 'orchestration', 'events.jsonl');
  if (!existsSync(p)) return [];
  try {
    const raw = readFileSync(p, 'utf8');
    return raw
      .split('\n')
      .filter(Boolean)
      .map((line) => {
        try {
          return JSON.parse(line) as Record<string, unknown>;
        } catch {
          return null;
        }
      })
      .filter((r): r is Record<string, unknown> => r !== null)
      .map((r) => parseLessonsEvalRow(r))
      .filter((r): r is LessonsEvalRow => r !== null);
  } catch {
    return [];
  }
}

/** Read loop outcomes from the repo's .selfbuild/ledger.jsonl. */
export function readLoopOutcomes(repoPath: string): LoopOutcome[] {
  const p = join(repoPath, '.selfbuild', 'ledger.jsonl');
  if (!existsSync(p)) return [];
  try {
    const raw = readFileSync(p, 'utf8');
    const out: LoopOutcome[] = [];
    for (const line of raw.split('\n').filter(Boolean)) {
      try {
        const parsed = JSON.parse(line) as Record<string, unknown>;
        const loop = Number(parsed.loop);
        const ts = typeof parsed.ts === 'string' ? parsed.ts : '';
        const status = typeof parsed.status === 'string' ? parsed.status : '';
        const goal = typeof parsed.goal === 'string' ? parsed.goal : '';
        if (ts && status) {
          out.push({ loop: isNaN(loop) ? 0 : loop, ts, status, goal });
        }
      } catch {
        // skip malformed lines
      }
    }
    return out;
  } catch {
    return [];
  }
}

// ─── Digest ordering ─────────────────────────────────────────────────────────

/**
 * Rank lesson lines by their excerptHash score (descending), newest-first
 * tiebreak (higher line index = later addition). Lines without a score
 * (no matching excerptHash, or no ledger data) get score 0 and sink.
 */
export function rankDigestLines(
  lines: string[],
  scores: Map<string, LessonScore>,
): string[] {
  return lines
    .map((line, i) => ({
      line,
      i,
      hash: lessonExcerptHash(line),
    }))
    .sort((a, b) => {
      const sa = scores.get(a.hash)?.score ?? 0;
      const sb = scores.get(b.hash)?.score ?? 0;
      // descending score, then descending index (newest-first)
      return sb - sa || b.i - a.i;
    })
    .map((x) => x.line);
}

/**
 * Fit ranked lines into the character budget, never splitting a line.
 * Keeps highest-scoring (first in array) lines while they fit; drops
 * the rest whole. A single oversized line is kept whole rather than
 * returning nothing (same policy as the original digest).
 */
export function fitDigestBudget(lines: string[], maxChars: number): string {
  const budget = maxChars;
  let total = -1; // joining N lines adds N-1 newlines
  const kept: string[] = [];
  for (const line of lines) {
    const cost = line.length + 1;
    if (kept.length > 0 && total + cost > budget) break;
    total += cost;
    kept.push(line);
  }
  return kept.join('\n').trim();
}