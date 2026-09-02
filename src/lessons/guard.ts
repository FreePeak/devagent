import { appendFileSync, existsSync, mkdirSync, readFileSync } from 'node:fs';
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
 * This module owns two responsibilities, both shared with `src/prompt.ts`:
 *  - the guard (pure `checkLessonsDedupe` + append wrapper
 *    `appendLessonGuarded`);
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

/**
 * Normalize lesson text for comparison: lowercase, replace every run of
 * non-alphanumeric characters with a single space, then collapse whitespace.
 * Punctuation-only rewordings ("Lessons eval guard" vs "Lessons-eval-guard")
 * normalize to the same token stream; word changes still register.
 */
export function normalizeLessonText(text: string): string {
  return text
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
 * Guarded append: run the dedupe gate, and only when the candidate is not a
 * near-duplicate write it to the lessons file (creating the parent directory
 * on the first append). On rejection nothing is written and a
 * `lessons-dedupe-rejected` event row is recorded in the orchestration
 * events ledger (best-effort, mirroring every other append* helper), so the
 * loop's replayable evidence shows why a recommendation never landed.
 *
 * Returns the guard result so callers can branch on `ok` and report the
 * similarity + matched entry.
 */
export function appendLessonGuarded(
  repoPath: string,
  entry: string,
  opts: { lessonsFile?: string; threshold?: number; predictedImpact?: string } = {},
): LessonsDedupeResult {
  const lessonsFile = opts.lessonsFile || SELFBUILD_LESSONS_PATH;
  const check = checkLessonsDedupe(repoPath, entry, { lessonsFile, threshold: opts.threshold });
  if (!check.ok) {
    recordLessonsDedupeRejection(repoPath, {
      similarity: check.similarity,
      threshold: check.threshold,
      matchedEntry: check.matchedEntry,
      entry,
    });
    return check;
  }
  const p = join(repoPath, lessonsFile);
  mkdirSync(dirname(p), { recursive: true });
  const entryText = appendPredictedImpact(entry, opts.predictedImpact);
  const existing = existsSync(p) ? readFileSync(p, 'utf8') : '';
  appendFileSync(p, existing.length > 0 && !existing.endsWith('\n') ? `\n${entryText}\n` : `${entryText}\n`);
  return check;
}

/** Best-effort `lessons-dedupe-rejected` event row (never throws). */
function recordLessonsDedupeRejection(
  repoPath: string,
  args: { similarity: number; threshold: number; matchedEntry: string; entry: string },
): void {
  try {
    const { LEDGER_DIR } = { LEDGER_DIR: '.devagent/runs/orchestration' };
    const file = join(repoPath, LEDGER_DIR, 'events.jsonl');
    mkdirSync(join(repoPath, LEDGER_DIR), { recursive: true });
    const record = {
      ts: new Date().toISOString(),
      kind: 'event',
      event: 'lessons-dedupe-rejected',
      similarity: Math.round(args.similarity * 1000) / 1000,
      threshold: args.threshold,
      matchedEntry: args.matchedEntry.slice(0, 300),
      entry: args.entry.slice(0, 300),
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
