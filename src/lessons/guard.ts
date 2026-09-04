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

export type LessonsEvalReason = 'missing-predictedImpact' | 'duplicate' | 'suite-red' | 'held-out' | 'accepted';

/** Outcome of the evaluate step for one gated append. */
export type LessonsSuiteOutcome = 'green' | 'red' | 'skipped';

/**
 * Result of the must-beat-best-so-far check at append time.
 * `'none'` = no applicable baseline: no measured scores among non-held-out
 * lessons (cold start — the candidate's own impact is the only score), or the
 * best measured score is saturated (>= 1) and can no longer discriminate
 * (predictedImpactGrade is capped at 1 while measured scores reach 1 on any
 * all-green history). Both accept: a bar that cannot be beaten is not a bar.
 * `'beat'` = candidate predictedImpact strictly beats the digest best score
 * (with the held-out slice excluded from that best).
 * `'below'` = candidate predictedImpact does not strictly beat the digest
 * best score — the append is rejected.
 */
export type LessonsMustBeatOutcome = 'none' | 'beat' | 'below';

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

/** Fraction of the newest machine-appended lessons held out of digest scoring. */
export const HELD_OUT_FRACTION = 0.2;
/** Minimum number of held-out lessons (always holds out at least one). */
export const HELD_OUT_MIN = 1;
/** Maximum number of held-out lessons (the digest window is 40 lines). */
export const HELD_OUT_MAX = 3;
/** Suffix that marks a machine-appended lesson line (see `appendPredictedImpact`). */
export const PREDICTED_IMPACT_SUFFIX = 'predictedImpact:';

/**
 * Held-out slice of a lessons file: the newest 20% (min 1, max 3) of
 * machine-appended lesson lines, by append order. Append order is file order
 * (the ratchet is append-only — `selfbuild-state.sh merge_lessons` dedupes
 * on exact content and appends new lines at the end, so the tail of the file
 * is the newest content). Only machine-appended lines (those carrying the
 * `predictedImpact:` suffix) are eligible: lines that never went through the
 * propose→evaluate→accept gate have no measured effect to hold out, and the
 * leading front-matter / date headings / prose are not lessons. Returns the
 * slice's content lines plus the excerpt hashes that digest scoring must
 * exclude.
 */
export function heldOutLessonHashes(repoPath: string, lessonsFile?: string): { lines: string[]; hashes: Set<string> } {
  const file = join(repoPath, lessonsFile || SELFBUILD_LESSONS_PATH);
  if (!existsSync(file)) return { lines: [], hashes: new Set() };
  try {
    const machine: string[] = [];
    for (const line of readFileSync(file, 'utf8').split('\n').filter(Boolean)) {
      if (!line.trim() || /^[-*]\s*$/.test(line.trim()) || /^#{1,6}\s/.test(line.trim()) || /^---/.test(line.trim())) continue;
      if (line.includes(PREDICTED_IMPACT_SUFFIX)) machine.push(line);
    }
    const keep = Math.max(HELD_OUT_MIN, Math.min(HELD_OUT_MAX, Math.floor(machine.length * HELD_OUT_FRACTION)));
    const slice = machine.slice(-keep);
    return { lines: slice, hashes: new Set(slice.map((l) => lessonExcerptHash(l))) };
  } catch {
    return { lines: [], hashes: new Set() };
  }
}

/**
 * Numeric grade for a `predictedImpact` text, used by the must-beat gate.
 * A higher grade means the lesson predicts a bigger measured improvement
 * (fewer future re-picks / less repeat failure). Mirrors the Q39 digest
 * formula's acceptRate − repeatFailureDelta shape. When the text names no
 * number, the grade is 0 so unquantified predictions lose to quantified ones
 * that name real reductions.
 */
export function predictedImpactGrade(impact: string): number {
  const text = impact.toLowerCase();
  let reductions = 0;
  // Direction 1 (word before the number, ≤ 20 words between): "cuts failures
  // by 25%", "reduces re-picks of shipped items by 50%", "drops 30 percent".
  const percent = '((?:\\d+(?:\\.\\d+)?%|\\d+(?:\\.\\d+)?\\s*(?:percent|percentage|per\\s+cent)))';
  const cut = '(?:fewer|less|lower|reduc\\w*|reduction|drop|down|cut\\w*)';
  const wordFirst = new RegExp(`${cut}\\D{0,80}?${percent}`, 'g');
  for (const m of text.matchAll(wordFirst)) {
    const n = parseFloat(m[1]!);
    if (Number.isFinite(n)) reductions += n;
  }
  if (reductions > 0) return Math.min(reductions / 100, 1);
  return 0;
}

/**
 * Load the current digest best measured score, excluding the held-out slice:
 * the best score among non-held-out lessons that have ledger evidence. Used
 * by the append-time must-beat gate — a candidate must beat the best score on
 * loops the held-out slice did not inform, so the newest 20% cannot be used
 * to chase a self-set bar. Returns `null` when no eligible lesson has a
 * measured score (nothing to beat yet).
 */
export function loadBestMeasuredScore(repoPath: string, lessonsFile?: string): number | null {
  const heldOut = heldOutLessonHashes(repoPath, lessonsFile);
  let best: number | null = null;
  for (const [hash, score] of loadLessonScores(repoPath)) {
    if (heldOut.hashes.has(hash)) continue; // held-out lessons never set the bar
    if (best === null || score > best) best = score;
  }
  return best;
}

/**
 * Must-beat-best-so-far check at append time (held-out tier): the candidate's
 * `predictedImpact` grade must strictly beat the digest's current best
 * measured score on loops the held-out slice did not inform. `'none'` when no
 * applicable baseline exists — no measured scores among non-held-out lessons
 * (cold start: the candidate's own impact is the only score), or the best is
 * saturated (>= 1, the unavoidable score of any accepted lesson on an ok
 * loop; predictedImpactGrade caps at 1, so a saturated bar would reject every
 * future append and lock the ratchet). `'beat'` when the grade strictly
 * exceeds the best; `'below'` otherwise (rejected). Callers run this only
 * after the evaluate step is green so a proposal that regresses the suite
 * never reaches the gate.
 */
export function checkMustBeat(repoPath: string, predictedImpact: string, lessonsFile?: string): LessonsMustBeatOutcome {
  const grade = predictedImpactGrade(predictedImpact);
  const best = loadBestMeasuredScore(repoPath, lessonsFile);
  // No baseline, or a saturated one the grade scale cannot beat: accept.
  if (best === null || best >= 1) return 'none';
  if (grade > best) return 'beat';
  return 'below';
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
  /** Held-out slice size the must-beat check judged against (when it ran). */
  heldOut?: number;
  /** Must-beat-best-so-far outcome (when the check ran). */
  mustBeat?: LessonsMustBeatOutcome;
  /** Best measured non-held-out score the must-beat check compared against. */
  mustBeatScore?: number;
  /** True when the append ran in dry-run mode (validated, nothing written). */
  dryRun?: boolean;
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
 *     anything (including the suite itself) never lands;
 *  4. when `opts.mustBeat` is set (default), the must-beat-best-so-far check
 *     passes (held-out tier): the candidate's predictedImpact must beat the
 *     digest's current best measured score on loops the held-out slice did
 *     not inform. The check runs strictly AFTER a green suite so a proposal
 *     that regresses the repo is rejected before the comparison matters, and
 *     a failed comparison reverts the staged file exactly like a red suite.
 *
 * Exactly one `lessons-eval` ledger row is written per gated append, accept or
 * reject, carrying the lesson excerpt hash, similarity score, predictedImpact,
 * suite result, and (when the must-beat check ran) the held-out slice size and
 * the must-beat outcome + best score it was judged against.
 */
export function appendLessonGuarded(
  repoPath: string,
  entry: string,
  opts: {
    lessonsFile?: string;
    threshold?: number;
    predictedImpact?: string;
    suiteTimeoutMs?: number;
    loop?: number;
    /** Run the held-out must-beat-best-so-far check after a green suite (default true). */
    mustBeat?: boolean;
    /** Validate without writing: dedupe + held-out checks run, no file/ledger write, no suite spawn. */
    dryRun?: boolean;
  } = {},
): LessonsDedupeResult {
  const lessonsFile = opts.lessonsFile || SELFBUILD_LESSONS_PATH;
  const excerptHash = lessonExcerptHash(entry);
  const runMustBeat = opts.mustBeat !== false;
  const dryRun = opts.dryRun === true;
  // Captured before the entry is staged so the ledger records the slice the
  // check judged against (the candidate is not part of it yet).
  const heldOutLines = runMustBeat ? heldOutLessonHashes(repoPath, lessonsFile).lines.length : 0;
  const suiteLedger = (
    suite: LessonsSuiteOutcome,
    reason: LessonsEvalReason,
    base: LessonsDedupeResult,
    suiteDetail?: string,
    extra: { heldOut?: number; mustBeat?: LessonsMustBeatOutcome; mustBeatScore?: number } = {},
  ): LessonsDedupeResult => {
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
      heldOut: extra.heldOut,
      mustBeat: extra.mustBeat,
      mustBeatScore: extra.mustBeatScore,
    });
    return {
      ...base,
      reason,
      suite,
      ...(suiteDetail !== undefined ? { suiteDetail } : {}),
      ...(extra.heldOut !== undefined ? { heldOut: extra.heldOut } : {}),
      ...(extra.mustBeat !== undefined ? { mustBeat: extra.mustBeat } : {}),
      ...(extra.mustBeatScore !== undefined ? { mustBeatScore: extra.mustBeatScore } : {}),
    };
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

  // Gate 2b (dry-run): dedupe is a pure read and always runs; everything
  // from the evaluate step onward is skipped so the validation never stages
  // the lessons file, spawns the suite, or appends a ledger row.
  if (dryRun) {
    const heldOut = heldOutLessonHashes(repoPath, lessonsFile).lines.length;
    const mustBeat = runMustBeat ? checkMustBeat(repoPath, opts.predictedImpact!, lessonsFile) : undefined;
    const best = runMustBeat ? loadBestMeasuredScore(repoPath, lessonsFile) : null;
    return {
      ok: check.ok,
      similarity: check.similarity,
      matchedEntry: check.matchedEntry,
      threshold: check.threshold,
      reason: mustBeat === 'below' ? 'held-out' : 'accepted',
      suite: 'skipped',
      heldOut,
      mustBeat,
      mustBeatScore: best ?? undefined,
      dryRun: true,
    };
  }

  // Gate 3: evaluate step — stage the proposed state, run the suite, revert on red.
  const p = join(repoPath, lessonsFile);
  const entryText = appendPredictedImpact(entry, opts.predictedImpact);
  const existing = existsSync(p) ? readFileSync(p, 'utf8') : null;
  mkdirSync(dirname(p), { recursive: true });
  appendFileSync(p, existing !== null && existing.length > 0 && !existing.endsWith('\n') ? `\n${entryText}\n` : `${entryText}\n`);

  const revert = (): void => {
    // Restore the pre-append bytes exactly (including file absence).
    try {
      if (existing === null) rmSync(p);
      else writeFileSync(p, existing);
    } catch {
      // best-effort: a failed revert still records as rejected below
    }
  };

  const suite = runLessonsSuite(repoPath, { timeoutMs: opts.suiteTimeoutMs });
  if (!suite.ok) {
    revert();
    return suiteLedger('red', 'suite-red', check, suite.detail);
  }

  // Gate 4 (held-out tier): must-beat-best-so-far — only after the suite is
  // green, so the staged state is the proposal being compared. A failed
  // comparison rejects and reverts exactly like a red suite.
  if (runMustBeat) {
    const mustBeat = checkMustBeat(repoPath, opts.predictedImpact, lessonsFile);
    if (mustBeat !== 'none' && mustBeat !== 'beat') {
      revert();
      const best = loadBestMeasuredScore(repoPath, lessonsFile);
      return suiteLedger('green', 'held-out', check, undefined, {
        heldOut: heldOutLines,
        mustBeat,
        mustBeatScore: best ?? undefined,
      });
    }
    // Accepted (or no baseline to beat): record what the check saw.
    const best = loadBestMeasuredScore(repoPath, lessonsFile);
    return suiteLedger('green', 'accepted', check, undefined, {
      heldOut: heldOutLines,
      mustBeat,
      mustBeatScore: best ?? undefined,
    });
  }

  return suiteLedger('green', 'accepted', check);
}

/**
 * One accept/reject ledger row per gated append (best-effort, never throws):
 * carries the lesson excerpt hash, similarity score, predictedImpact, suite
 * result, and the held-out-tier fields (`heldOut` slice size + `mustBeat`
 * outcome / best score) so replay can answer "why did this lesson land or
 * not".
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
    heldOut?: number;
    mustBeat?: LessonsMustBeatOutcome;
    mustBeatScore?: number;
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
      ...(args.heldOut !== undefined ? { heldOut: args.heldOut } : {}),
      ...(args.mustBeat !== undefined ? { mustBeat: args.mustBeat } : {}),
      ...(args.mustBeatScore !== undefined ? { mustBeatScore: Math.round(args.mustBeatScore * 1000) / 1000 } : {}),
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
