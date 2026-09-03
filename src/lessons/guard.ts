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
/** One `lessons-eval` ledger row, as written by `recordLessonsEval`. */
export interface LessonsEvalLedgerRow {
  ts?: string;
  excerptHash?: string;
  similarity?: number;
  threshold?: number;
  predictedImpact?: string;
  suite?: LessonsSuiteOutcome;
  accepted?: boolean;
  reason?: LessonsEvalReason;
  entry?: string;
  suiteDetail?: string;
}

/**
 * Per-lesson aggregation of its `lessons-eval` ledger history. `acceptRate`
 * is the share of gated appends that landed; `repeatFailures` counts how
 * often the loop re-proposed the same excerpt after it was already accepted
 * (the dedupe gate's duplicate rejections) — the deterministic signal that
 * the lesson's predicted effect did not stick. `effectScore` folds both into
 * one ranking scalar for the digest (see `scoreLessonEffect`).
 */
export interface LessonsLessonScore {
  /** Excerpt hash the `lessons-eval` rows key on. */
  excerptHash: string;
  /** Accepted gated appends over total proposals; 0 when untested. */
  acceptRate: number;
  /** Duplicate rejections after the first acceptance (repeat-failure delta). */
  repeatFailures: number;
  /** Ranking scalar in [0, 1]; see `scoreLessonEffect`. */
  effectScore: number;
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
  opts: { lessonsFile?: string; threshold?: number; predictedImpact?: string; suiteTimeoutMs?: number } = {},
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
  },
): void {
  try {
    const { LEDGER_DIR } = { LEDGER_DIR: '.devagent/runs/orchestration' };
    const file = join(repoPath, LEDGER_DIR, 'events.jsonl');
    mkdirSync(join(repoPath, LEDGER_DIR), { recursive: true });
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

/**
 * Read all `lessons-eval` rows from the shared events.jsonl ledger. Returns
 * an empty array when the ledger is absent or unreadable (best-effort).
 */
export function readLessonsEvalLedger(repoPath: string): LessonsEvalLedgerRow[] {
  try {
    const { LEDGER_DIR } = { LEDGER_DIR: '.devagent/runs/orchestration' };
    const file = join(repoPath, LEDGER_DIR, 'events.jsonl');
    if (!existsSync(file)) return [];
    const out: LessonsEvalLedgerRow[] = [];
    for (const line of readFileSync(file, 'utf8').split('\n')) {
      const trimmed = line.trim();
      if (!trimmed) continue;
      try {
        const r = JSON.parse(trimmed) as Record<string, unknown>;
        if (r.event === 'lessons-eval') {
          out.push(r as unknown as LessonsEvalLedgerRow);
        }
      } catch {
        // skip corrupt lines; best-effort
      }
    }
    return out;
  } catch {
    return [];
  }
}

/**
 * Compute a single lesson's effect score from its `lessons-eval` ledger rows.
 * Deterministic — uses only gate-results signals (accept/reject reasons and
 * suite outcomes). The score is a ranking scalar in [0, 1]:
 *
 *   - 1.0  accepted at least once, zero repeat failures (never re-proposed
 *          after acceptance — the lesson's effect held)
 *   - 0.5  accepted at least once but re-proposed as duplicate afterwards
 *          (the lesson's effect needed reinforcement)
 *   - 0.0  never accepted (all proposals rejected)
 *   - 0.5  default for untested lessons (no ledger rows yet)
 */
export function scoreLessonEffect(rows: LessonsEvalLedgerRow[]): LessonsLessonScore {
  const excerptHash = rows.length > 0 ? (rows[0]!.excerptHash ?? '') : '';
  if (rows.length === 0) {
    return { excerptHash, acceptRate: 0, repeatFailures: 0, effectScore: 0.5 };
  }
  const total = rows.length;
  const accepted = rows.filter((r) => r.accepted === true).length;
  const acceptRate = total > 0 ? accepted / total : 0;

  // Find the index of the first accepted row.
  const firstAcceptIdx = rows.findIndex((r) => r.accepted === true);
  // Repeat failures: duplicate rejections that occur after the first acceptance.
  const repeatFailures = firstAcceptIdx >= 0
    ? rows.slice(firstAcceptIdx + 1).filter((r) => r.reason === 'duplicate').length
    : 0;

  let effectScore: number;
  if (accepted === 0) {
    effectScore = 0;
  } else if (repeatFailures === 0) {
    effectScore = 1;
  } else {
    effectScore = 0.5;
  }

  return { excerptHash, acceptRate, repeatFailures, effectScore };
}

/**
 * Read the lessons-eval ledger, group by excerpt hash, score each lesson,
 * write one aggregated `lessons-impact` ledger row, and return the per-lesson
 * scores keyed by excerpt hash. Best-effort: never throws.
 */
export function scoreLessonsImpact(repoPath: string): Record<string, LessonsLessonScore> {
  try {
    const rows = readLessonsEvalLedger(repoPath);
    const byHash = new Map<string, LessonsEvalLedgerRow[]>();
    for (const r of rows) {
      const hash = r.excerptHash;
      if (!hash) continue;
      const list = byHash.get(hash) ?? [];
      list.push(r);
      byHash.set(hash, list);
    }
    const scores: Record<string, LessonsLessonScore> = {};
    let acceptedLessonCount = 0;
    let totalRepeatFailures = 0;
    let acceptRateSum = 0;
    const lessonCount = byHash.size;
    // No eval rows → nothing to score, skip the lessons-impact row entirely.
    if (lessonCount === 0) return scores;
    for (const [hash, hashRows] of byHash) {
      const s = scoreLessonEffect(hashRows);
      scores[hash] = s;
      if (s.acceptRate > 0) acceptedLessonCount++;
      totalRepeatFailures += s.repeatFailures;
      acceptRateSum += s.acceptRate;
    }
    const meanAcceptRate = lessonCount > 0 ? acceptRateSum / lessonCount : 0;

    // Write the aggregated lessons-impact ledger row.
    const { LEDGER_DIR } = { LEDGER_DIR: '.devagent/runs/orchestration' };
    const file = join(repoPath, LEDGER_DIR, 'events.jsonl');
    mkdirSync(join(repoPath, LEDGER_DIR), { recursive: true });
    const record = {
      ts: new Date().toISOString(),
      kind: 'event',
      event: 'lessons-impact',
      lessonCount,
      acceptedLessonCount,
      meanAcceptRate: Math.round(meanAcceptRate * 1000) / 1000,
      totalRepeatFailures,
      scores: Object.values(scores).map((s) => ({
        excerptHash: s.excerptHash,
        acceptRate: Math.round(s.acceptRate * 1000) / 1000,
        repeatFailures: s.repeatFailures,
        effectScore: s.effectScore,
      })),
    };
    appendFileSync(file, `${JSON.stringify(record)}\n`);
    return scores;
  } catch {
    return {};
  }
}
