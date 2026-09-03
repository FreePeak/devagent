import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { appendFileSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';

/**
 * Lessons eval guard (PRD Phase 4 backlog, docs/PRD.md §17): a content-level
 * dedupe gate that runs BEFORE any machine append to the lessons file. The
 * write-only lessons ratchet had no guard — `.selfbuild/lessons.md` repeated
 * the lessons-eval-guard recommendation 7x and re-injected near-identical
 * sweep-38/39 bullets, burning the 4000-char `lessonsMaxChars` digest budget
 * on repeats. The v1 deterministic slice of the propose→evaluate→accept
 * precedent (AHE/Meta-Harness) is: normalize, shingle, and reject a candidate
 * whose trigram-Jaccard similarity to an existing entry meets the threshold.
 *
 * This module owns the eval guard, shared with `src/prompt.ts`:
 *  - the dedupe gate (pure `checkLessonsDedupe`) — PR #116;
 *  - the evaluate→accept step (`runLessonsSuite` + the gated append wrapper
 *    `appendLessonGuarded`): the repo regression suite (`npm test`, vitest)
 *    runs against the PROPOSED lessons-file state before the append is
 *    accepted — green keeps the entry, red reverts the file and rejects;
 *  - one accept/reject `lessons-eval` ledger row per gated append (lesson
 *    excerpt hash, similarity score, predictedImpact, suite result);
 *  - the lessons-file cursor (`LESSONS_PATH`, `LESSONS_MAX_LINES`,
 *    `LESSONS_MAX_CHARS`) that both the guard and the digest
 *    (`loadLessons` / `loadLessonsDigest`) read from, so dedupe-before-append
 *    and cap-after-load never drift apart.
 *
 * Append granularity is one lesson per line, matching how `loadLessons`
 * slices the file and how machine appends arrive (dated `## <date>` headers
 * and `---` front-matter act as entry barriers, never comparison targets).
 */

/** Repo-relative default lessons file (config `lessonsFile` overrides). */
export const LESSONS_PATH = '.devagent/lessons.md';
/** Repo-relative lessons file the self-build loop's machine appends land in. */
export const SELFBUILD_LESSONS_PATH = '.selfbuild/lessons.md';
/** Lessons are context, not the task: cap the digest to the newest 40 lines. */
export const LESSONS_MAX_LINES = 40;
/** Hard character budget for injected lessons (PRD Q9: distilled, not verbatim). */
export const LESSONS_MAX_CHARS = 4000;

/**
 * Default reject threshold for the lessons dedupe guard: a candidate lesson
 * whose normalized trigram-Jaccard similarity to any existing entry is at or
 * above this value is treated as a near-duplicate and rejected before append.
 * Calibrated on the real `.selfbuild/lessons.md` regression case: duplicated
 * synthesis bullets score 0.85–1.00 while genuinely distinct lessons score
 * ~0.00–0.10. Configurable via `lessonsDedupeSimilarity`.
 */
export const DEFAULT_LESSONS_DEDUPE_SIMILARITY = 0.8;

/** Wall-clock budget for one evaluate-step suite run (default 10 minutes). */
export const DEFAULT_LESSONS_SUITE_TIMEOUT_MS = 600_000;

/** Why a gated append landed the way it did. */
export type LessonsEvalReason = 'missing-predictedImpact' | 'duplicate' | 'suite-red' | 'accepted';

/** Outcome of the evaluate step for one gated append. */
export type LessonsSuiteOutcome = 'green' | 'red' | 'skipped';

/** Why a loop terminated (self-build loop outcomes; matches ledger statuses). */
export type LoopResultStatus = 'ok' | 'failed' | 'failed-tests' | 'invalid' | 'skipped' | 'provider-degraded' | 'push-failed';

/**
 * One `loop-result` ledger row per loop iteration: the deterministic outcome
 * (status) the lessons-eval accept/reject rows from the same loop join against
 * (Q39 impact telemetry). The self-build driver writes one row per iteration
 * via `recordLoopResult`; the join key is the numeric `loop`.
 */
export interface LoopResultLedgerRecord {
  ts: string;
  kind: 'event';
  event: 'loop-result';
  loop: number;
  status: LoopResultStatus;
  goal: string;
}

/** Repo-relative ledger of orchestration events (lessons-eval, loop-result). */
export const EVENTS_FILE = '.devagent/runs/orchestration/events.jsonl';

/**
 * Best-effort, never-throws write of one loop-result ledger row (the
 * deterministic loop outcome for impact scoring). `status` must be a known
 * self-build loop status (ok | failed | failed-tests | invalid | skipped |
 * provider-degraded | push-failed); anything else is normalized to `failed`.
 */
export function recordLoopResult(repoPath: string, loop: number, status: string, goal: string): void {
  try {
    const file = join(repoPath, EVENTS_FILE);
    mkdirSync(dirname(file), { recursive: true });
    const record: LoopResultLedgerRecord = {
      ts: new Date().toISOString(),
      kind: 'event',
      event: 'loop-result',
      loop,
      status: (
        ['ok', 'failed', 'failed-tests', 'invalid', 'skipped', 'provider-degraded', 'push-failed'].includes(status)
          ? status
          : 'failed'
      ) as LoopResultStatus,
      goal: goal.trim().replace(/\s+/g, ' ').slice(0, 160),
    };
    appendFileSync(file, `${JSON.stringify(record)}\n`);
  } catch {
    // best-effort observability only
  }
}

/**
 * Read all structured events from the orchestration events.jsonl ledger.
 * Returns every parseable row; corrupt lines are silently skipped (best-effort).
 * Cost: O(N) in rows, linear in the file size. Call once per scoring pass.
 */
export function readEvents(repoPath: string): Record<string, unknown>[] {
  try {
    const file = join(repoPath, EVENTS_FILE);
    if (!existsSync(file)) return [];
    const raw = readFileSync(file, 'utf8');
    const out: Record<string, unknown>[] = [];
    for (const line of raw.split('\n').filter(Boolean)) {
      try {
        const parsed = JSON.parse(line) as Record<string, unknown>;
        if (parsed && typeof parsed === 'object') out.push(parsed);
      } catch {
        // skip corrupt line
      }
    }
    return out;
  } catch {
    return [];
  }
}

/**
 * Per-excerptHash impact score. Higher = more effective lesson.
 * `score` = acceptRate - repeatFailureDelta.
 * `acceptRate` = acceptedCount / evalCount (0 when no evals).
 * `repeatFailureDelta` = lessonLoopFailureRate - overallLoopFailureRate.
 *   Negative delta = lesson correlates with fewer failures (good).
 */
export interface LessonScore {
  score: number;
  acceptRate: number;
  delta: number;
  evalCount: number;
  lessonLoopFailureRate: number;
  overallLoopFailureRate: number;
}

/**
 * Compute lesson impact scores from the orchestration events.jsonl ledger.
 * Joins `lessons-eval` rows with `loop-result` rows on the numeric `loop` field.
 * A lessons-eval row without a `loop` field is matched to the nearest subsequent
 * loop-result row by timestamp fallback (existing rows from before the `--loop` flag).
 * Pure function: no I/O, operates on the parsed events array.
 *
 * Impact formula:
 *   score = acceptRate - (lessonLoopFailureRate - overallLoopFailureRate)
 *
 * Where:
 *   acceptRate = accepted / evalCount for the lesson
 *   lessonLoopFailureRate = failed loops with this lesson / total loops with this lesson
 *   overallLoopFailureRate = all failed loops / all loops (with lessons-eval rows)
 *
 * When no loop-result data exists for a lesson, delta = 0 and score = acceptRate.
 */
export function computeLessonScores(events: Record<string, unknown>[]): Map<string, LessonScore> {
  const lessonsEvalRows = events.filter((r) => r.event === 'lessons-eval');
  const loopResultRows = events.filter((r) => r.event === 'loop-result');

  if (lessonsEvalRows.length === 0) return new Map();

  // Build loop-result lookup: numeric loop → status
  const loopResultMap = new Map<number, string>();
  for (const row of loopResultRows) {
    const loop = Number(row.loop);
    if (!Number.isFinite(loop)) continue;
    loopResultMap.set(loop, String(row.status ?? 'failed'));
  }

  // For rows without a loop field, try timestamp-based matching:
  // find the nearest loop-result with ts >= this lessons-eval ts.
  const loopResultByTs = loopResultRows
    .filter((r) => r.ts && typeof r.ts === 'string')
    .sort((a, b) => String(a.ts).localeCompare(String(b.ts)));

  function findLoopResultByTs(ts: string): number | undefined {
    for (const row of loopResultByTs) {
      if (String(row.ts) >= ts) return Number(row.loop);
    }
    return undefined;
  }

  // Per-excerptHash aggregation
  const lessonEvals = new Map<string, { evalCount: number; acceptedCount: number; loopIds: Set<number> }>();
  const allLoopsWithEval = new Set<number>();

  for (const row of lessonsEvalRows) {
    const hash = String(row.excerptHash ?? '');
    if (!hash) continue;
    let loop = row.loop !== undefined ? Number(row.loop) : NaN;
    if (!Number.isFinite(loop)) {
      loop = row.ts && typeof row.ts === 'string' ? (findLoopResultByTs(row.ts) ?? NaN) : NaN;
    }

    let entry = lessonEvals.get(hash);
    if (!entry) {
      entry = { evalCount: 0, acceptedCount: 0, loopIds: new Set() };
      lessonEvals.set(hash, entry);
    }
    entry.evalCount++;
    if (row.accepted) entry.acceptedCount++;
    if (Number.isFinite(loop)) {
      entry.loopIds.add(loop);
      allLoopsWithEval.add(loop);
    }
  }

  // Compute overall failure rate
  let overallFailedCount = 0;
  let overallLoopCount = 0;
  for (const loop of allLoopsWithEval) {
    const status = loopResultMap.get(loop);
    if (status) {
      overallLoopCount++;
      if (status !== 'ok') overallFailedCount++;
    }
  }
  const overallLoopFailureRate = overallLoopCount > 0 ? overallFailedCount / overallLoopCount : 0;

  // Per-lesson scoring
  const scores = new Map<string, LessonScore>();
  for (const [hash, entry] of lessonEvals) {
    const acceptRate = entry.evalCount > 0 ? entry.acceptedCount / entry.evalCount : 0;

    // Lesson's loop failure rate
    let lessonFailedCount = 0;
    let lessonLoopCount = 0;
    for (const loop of entry.loopIds) {
      const status = loopResultMap.get(loop);
      if (status) {
        lessonLoopCount++;
        if (status !== 'ok') lessonFailedCount++;
      }
    }
    const lessonLoopFailureRate = lessonLoopCount > 0 ? lessonFailedCount / lessonLoopCount : 0;
    const delta = lessonLoopFailureRate - overallLoopFailureRate;
    const score = acceptRate - delta;

    scores.set(hash, {
      score,
      acceptRate,
      delta,
      evalCount: entry.evalCount,
      lessonLoopFailureRate,
      overallLoopFailureRate,
    });
  }

  return scores;
}

/**
 * Load lesson scores from the repo's events.jsonl ledger for digest ranking.
 * Returns a Map<excerptHash, score> for use by `loadLessonsDigest`.
 * Reads the events file and computes scores via `computeLessonScores`.
 * When no events exist, returns an empty map (digest falls back to file-order).
 */
export function loadLessonScores(repoPath: string): Map<string, number> {
  const events = readEvents(repoPath);
  const scores = computeLessonScores(events);
  const out = new Map<string, number>();
  for (const [hash, s] of scores) {
    out.set(hash, s.score);
  }
  return out;
}
/**
 * Normalize lesson text for comparison: strip the optional `[predictedImpact: ...]`
 * metadata suffix (it is captured separately in the lessons-eval ledger row and
 * must neither dilute nor bypass dedupe), lowercase, replace every run of
 * non-alphanumeric characters with a single space, then collapse whitespace.
 * Punctuation-only rewordings ("Lessons eval guard" vs "Lessons-eval-guard")
 * normalize to the same token stream; word changes still register.
 */
export function normalizeLessonText(text: string): string {
  return text
    .replace(/\s*\[predictedImpact:[^\]]*\]/gi, ' ')
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, ' ')
    .replace(/\s+/g, ' ')
    .trim();
}

/**
 * Word-level trigram (3-shingle) set of normalized lesson text. Word
 * shingles — not character n-grams — stay robust to markdown emphasis,
 * URL churn, and line-wrap differences while remaining sensitive to real
 * content changes.
 */
export function lessonShingles(text: string): Set<string> {
  const words = normalizeLessonText(text).split(' ').filter((w) => w.length > 0);
  const out = new Set<string>();
  for (let i = 0; i + 3 <= words.length; i++) {
    out.add(`${words[i]!} ${words[i + 1]!} ${words[i + 2]!}`);
  }
  return out;
}

/**
 * Trigram-Jaccard similarity between two lesson texts in [0, 1]: intersection
 * of the normalized word-trigram sets over their union. Two texts sharing no
 * trigram score 0; identical texts score 1; two texts that are both empty
 * (or too short to carry a trigram) score 1 so an empty candidate can never
 * bypass the guard by containing nothing to compare.
 */
export function lessonSimilarity(a: string, b: string): number {
  const sa = lessonShingles(a);
  const sb = lessonShingles(b);
  if (sa.size === 0 && sb.size === 0) return 1;
  let inter = 0;
  for (const s of sa) {
    if (sb.has(s)) inter += 1;
  }
  const union = sa.size + sb.size - inter;
  return union === 0 ? 1 : inter / union;
}

/**
 * Result of a lessons dedupe check.
 */
export interface LessonsDedupeResult {
  /** True when no existing entry meets the similarity threshold. */
  ok: boolean;
  /** Trigram-Jaccard similarity of the nearest existing entry (0 when none). */
  similarity: number;
  /** The nearest existing entry, '' when the file has no entries at all. */
  matchedEntry: string;
  /** The threshold that was applied. */
  threshold: number;
  /** Why the gated append landed this way (set by `appendLessonGuarded`). */
  reason?: LessonsEvalReason;
  /** Evaluate-step outcome for the gated append (set by `appendLessonGuarded`). */
  suite?: LessonsSuiteOutcome;
  /** Bounded tail of the failing suite output when the suite ran and failed. */
  suiteDetail?: string;
}

/**
 * Read the candidate-comparison surface of a lessons file: its non-blank
 * content lines. Blank lines and structural lines are not lesson content, so
 * the guard would waste the digest budget comparing a candidate against a
 * dated `## <date>` header or a `---` front-matter fence; only actual content
 * lines (the granularity the ratchet appends and the digest slices) count.
 */
export function readLessonEntries(lessonsPath: string): string[] {
  if (!existsSync(lessonsPath)) return [];
  let raw: string;
  try {
    raw = readFileSync(lessonsPath, 'utf8');
  } catch {
    return [];
  }
  const out: string[] = [];
  for (const line of raw.split('\n')) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    if (trimmed === '---') continue;
    if (/^#{1,6}\s/.test(trimmed)) continue;
    out.push(line.trimEnd());
  }
  return out;
}

/**
 * Stable content hash of a lesson excerpt: sha256 over the normalized text,
 * truncated to 16 hex chars (same shape as the executor trail signature). It
 * keys the `lessons-eval` ledger row so replay can match a row to its entry
 * without embedding the full line.
 */
export function lessonExcerptHash(entry: string): string {
  return createHash('sha256').update(normalizeLessonText(entry)).digest('hex').slice(0, 16);
}

/** Result of one evaluate-step suite run. */
export interface LessonsSuiteResult {
  /** True when the suite exited 0. */
  ok: boolean;
  /** Bounded output tail (or failure detail) for the ledger row. */
  detail: string;
}

/**
 * Evaluate step of the propose→evaluate→accept gate: run the repo regression
 * suite (`npm test` — vitest in this repo) against the proposed lessons-file
 * state. Never throws: a suite that cannot even start is a red result, not a
 * crash path — a bad lesson must never land because the runner broke.
 */
export function runLessonsSuite(repoPath: string, opts: { timeoutMs?: number } = {}): LessonsSuiteResult {
  try {
    const r = spawnSync('npm', ['test'], {
      cwd: repoPath,
      encoding: 'utf8',
      timeout: opts.timeoutMs ?? DEFAULT_LESSONS_SUITE_TIMEOUT_MS,
      shell: process.platform === 'win32',
    });
    const detail = `${r.stdout ?? ''}\n${r.stderr ?? ''}`.trim();
    if (r.status === 0) return { ok: true, detail: detail.slice(-300) };
    return { ok: false, detail: (detail || `npm test exited ${r.status} ${r.signal ?? ''}`).slice(-300) };
  } catch (e) {
    return { ok: false, detail: `npm test failed to run: ${(e as Error).message}`.slice(0, 300) };
  }
}

/**
 * Pure content-similarity gate run before a candidate lesson is appended:
 * compare the candidate (normalized word trigrams) against every existing
 * content line of the lessons file and reject when the nearest match is at
 * or above `threshold`. Returns the decision plus what it was based on so
 * callers can surface / record the rejection.
 */
export function checkLessonsDedupe(
  repoPath: string,
  entry: string,
  opts: { lessonsFile?: string; threshold?: number } = {},
): LessonsDedupeResult {
  const threshold = opts.threshold ?? DEFAULT_LESSONS_DEDUPE_SIMILARITY;
  const lessonsFile = opts.lessonsFile || SELFBUILD_LESSONS_PATH;
  const entries = readLessonEntries(join(repoPath, lessonsFile));
  let best = 0;
  let bestEntry = '';
  for (const line of entries) {
    const s = lessonSimilarity(entry, line);
    if (bestEntry === '' || s > best) {
      best = s;
      bestEntry = line;
    }
  }
  return { ok: best < threshold, similarity: best, matchedEntry: bestEntry, threshold };
}

/**
 * Eval-gated append (PRD §17 "Lessons eval guard", evaluate→accept slice).
 *
 * A candidate lesson is accepted only when ALL of the following hold:
 *  1. it carries a non-empty `predictedImpact` (PR #116 captured the field but
 *     nothing required it — a lesson that predicts nothing cannot be verified);
 *  2. the dedupe gate passes (trigram-Jaccard below the threshold);
 *  3. the evaluate step is green: the repo regression suite (`npm test`)
 *     passes against the PROPOSED lessons-file state. The entry is staged by
 *     writing it to the file first, the suite runs, and on failure the file is
 *     restored byte-for-byte to its pre-append state — a lesson that regresses
 *     anything (including the suite itself) never lands.
 *
 * Exactly one `lessons-eval` ledger row is written per gated append, accept or
 * reject, carrying the lesson excerpt hash, similarity score, predictedImpact,
 * and suite result so the loop's replayable evidence shows why content landed.
 */
export function appendLessonGuarded(
  repoPath: string,
  entry: string,
  opts: { lessonsFile?: string; threshold?: number; predictedImpact?: string; suiteTimeoutMs?: number; loop?: number } = {},
): LessonsDedupeResult {
  const lessonsFile = opts.lessonsFile || SELFBUILD_LESSONS_PATH;
  const excerptHash = lessonExcerptHash(entry);
  const suiteLedger = (suite: LessonsSuiteOutcome, reason: LessonsEvalReason, base: LessonsDedupeResult, suiteDetail?: string): LessonsDedupeResult => {
    recordLessonsEval(repoPath, {
      excerptHash,
      similarity: base.similarity,
      threshold: base.threshold,
      predictedImpact: opts.predictedImpact,
      suite,
      accepted: reason === 'accepted',
      reason,
      entry,
      suiteDetail,
      loop: opts.loop,
    });
    return { ...base, reason, suite, ...(suiteDetail !== undefined ? { suiteDetail } : {}) };
  };

  // Gate 1: predictedImpact is required for machine appends.
  if (!opts.predictedImpact || !opts.predictedImpact.trim()) {
    const check = checkLessonsDedupe(repoPath, entry, { lessonsFile, threshold: opts.threshold });
    return suiteLedger('skipped', 'missing-predictedImpact', check);
  }

  const check = checkLessonsDedupe(repoPath, entry, { lessonsFile, threshold: opts.threshold });

  // Gate 2: dedupe (similarity below threshold).
  if (!check.ok) {
    return suiteLedger('skipped', 'duplicate', check);
  }

  // Gate 3: evaluate step — stage the proposed state, run the suite, revert on red.
  const p = join(repoPath, lessonsFile);
  const entryText = appendPredictedImpact(entry, opts.predictedImpact);
  const existing = existsSync(p) ? readFileSync(p, 'utf8') : null;
  mkdirSync(dirname(p), { recursive: true });
  appendFileSync(p, existing !== null && existing.length > 0 && !existing.endsWith('\n') ? `\n${entryText}\n` : `${entryText}\n`);

  const suite = runLessonsSuite(repoPath, { timeoutMs: opts.suiteTimeoutMs });
  if (!suite.ok) {
    // Revert: restore the pre-append bytes exactly (including file absence).
    try {
      if (existing === null) rmSync(p);
      else writeFileSync(p, existing);
    } catch {
      // best-effort: a failed revert still records as rejected below
    }
    return suiteLedger('red', 'suite-red', check, suite.detail);
  }

  return suiteLedger('green', 'accepted', check);
}

/**
 * One accept/reject ledger row per gated append (best-effort, never throws):
 * carries the lesson excerpt hash, similarity score, predictedImpact, and
 * suite result so replay can answer "why did this lesson land or not".
 */
function recordLessonsEval(
  repoPath: string,
  args: {
    excerptHash: string;
    similarity: number;
    threshold: number;
    predictedImpact?: string;
    suite: LessonsSuiteOutcome;
    accepted: boolean;
    reason: LessonsEvalReason;
    entry: string;
    suiteDetail?: string;
    loop?: number;
  },
): void {
  try {
    const file = join(repoPath, EVENTS_FILE);
    mkdirSync(dirname(file), { recursive: true });
    const record = {
      ts: new Date().toISOString(),
      kind: 'event',
      event: 'lessons-eval',
      excerptHash: args.excerptHash,
      similarity: Math.round(args.similarity * 1000) / 1000,
      threshold: args.threshold,
      predictedImpact: args.predictedImpact ? args.predictedImpact.trim().replace(/\s+/g, ' ').slice(0, 300) : '',
      suite: args.suite,
      accepted: args.accepted,
      reason: args.reason,
      entry: args.entry.slice(0, 300),
      ...(args.loop !== undefined ? { loop: args.loop } : {}),
      ...(args.suiteDetail ? { suiteDetail: args.suiteDetail } : {}),
    };
    appendFileSync(file, `${JSON.stringify(record)}\n`);
  } catch {
    // best-effort observability only
  }
}

/**
 * Optional AHE/Meta-Harness `predictedImpact` field: a short, free-form
 * prediction of which future outcome the lesson should flip (e.g. "avoids
 * re-picking already-shipped backlog items"). When present it is appended to
 * the lesson line on a distinct `predictedImpact:` suffix so it round-trips
 * through the digest verbatim (the digest is a text cursor, not a parser —
 * it slices lines whole). Absent, the entry is written exactly as given.
 */
export function appendPredictedImpact(entry: string, predictedImpact?: string): string {
  if (!predictedImpact || !predictedImpact.trim()) return entry.trimEnd();
  const text = entry.trimEnd();
  const impact = predictedImpact.trim().replace(/\s+/g, ' ').slice(0, 300);
  return text.endsWith('predictedImpact:') || /\bpredictedImpact:\s*\S/.test(text)
    ? text
    : `${text} [predictedImpact: ${impact}]`;
}
