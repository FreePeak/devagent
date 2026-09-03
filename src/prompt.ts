import { appendFileSync, existsSync, mkdirSync, readFileSync, readdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import type { TicketSpec } from './types.js';
import type { ImplementationPlan } from './planner.js';
import { LESSONS_MAX_CHARS, LESSONS_MAX_LINES, LESSONS_PATH, lessonExcerptHash, loadLessonScores } from './lessons/guard.js';

/** Default repo-local lessons file; overridable via config `lessonsFile`. */
export const DEFAULT_LESSONS_FILE = LESSONS_PATH;
/**
 * Sentinel spliced into the planner prompt where the prior child-worker trail
 * block should land. The planner builder embeds this exact string at a fixed
 * offset so the prefix above it stays cacheable across iterations; the e2e
 * test (`test/orchestrator/e2e-prior-trail-flow.test.ts`) imports this
 * constant to assert the rendered prompt actually carries the header.
 */
export const COMPACT_CONTEXT_MARKER = '## Prior Worker Trails';
/**
 * Hard character budget for the per-task child-trail digest, matching
 * `LESSONS_MAX_CHARS` so both injections stay comparably bounded.
 */
export const CHILD_TRAILS_MAX_CHARS = 4000;
/** On-disk root for the per-loop trail ledger. */
const TRAILS_ROOT = '.selfbuild/trails';

function trailFile(cwd: string, loopId: string, taskId: string): string {
  return join(cwd, TRAILS_ROOT, loopId, `${taskId}.jsonl`);
}

/**
 * Load curated durable lessons for prompt injection (PRD Phase 4 lessons feedback
 * loop). Returns '' when the file is absent so prompts stay unchanged by default.
 * Ratchet-only content is assumed: callers keep the file append-only.
 *
 * Bounded twice (PRD Q9): first by line count, then by a character budget
 * (`lessonsMaxChars`, default 4000) that drops oldest entries whole — never
 * splitting a line — so verbose ratchet paragraphs cannot blow up worker
 * payloads across implementation, repair, and fan-out legs.
 */
export function loadLessons(repoPath: string, lessonsFile?: string, maxChars?: number): string {
  return loadLessonsDigest(repoPath, lessonsFile, maxChars);
}

/**
 * Shared digest cursor behind `loadLessons` and the guarded append path
 * (src/lessons/guard.ts): read the newest `LESSONS_MAX_LINES` lines, rank them
 * by measured impact (Q39), then drop the lowest-ranked entries whole until
 * the surviving block fits `LESSONS_MAX_CHARS` (config `lessonsMaxChars`).
 * Ranking uses the per-lesson score from the orchestration ledger
 * (`loadLessonScores`): lines whose excerpt hash has a score sort by score
 * (descending, oldest-first tiebreak) ahead of unscored lines, which keep the
 * existing newest-first order so the recency behavior is preserved when no
 * impact data exists. Never splits a line and never strips content from a
 * kept line, so an optional `predictedImpact:` suffix appended to a lesson
 * round-trips verbatim through the injected digest.
 */
export function loadLessonsDigest(repoPath: string, lessonsFile?: string, maxChars?: number): string {
  const p = join(repoPath, lessonsFile || DEFAULT_LESSONS_FILE);
  if (!existsSync(p)) return '';
  try {
    const allLines = readFileSync(p, 'utf8').trimEnd().split('\n');
    const window = allLines.slice(-LESSONS_MAX_LINES);
    const base = allLines.length - window.length;
    const scores = loadLessonScores(repoPath);
    const indexed = window.map((line, i) => {
      const score = scores.get(lessonExcerptHash(line));
      return { line, score: score ?? -Infinity, idx: base + i };
    });
    // Score descending; scored ties break oldest-first, unscored keep the
    // existing newest-first order (so the oldest unscored lines are dropped
    // first when the budget bites — same recency semantics as before).
    indexed.sort((a, b) => {
      if (a.score !== b.score) return b.score - a.score;
      if (a.score === -Infinity) return b.idx - a.idx;
      return a.idx - b.idx;
    });
    const budget = maxChars ?? LESSONS_MAX_CHARS;
    let total = -1; // joining N lines adds N-1 newlines
    let end = 0;
    for (const entry of indexed) {
      const cost = entry.line.length + 1;
      if (end > 0 && total + cost > budget) break;
      total += cost;
      end++;
    }
    return indexed
      .slice(0, end)
      .sort((a, b) => a.idx - b.idx)
      .map((e) => e.line)
      .join('\n')
      .trim();
  } catch {
    return '';
  }
}

/** Rendered lessons section, or '' when there is nothing to inject. */
function lessonsSection(lessons?: string): string {
  if (!lessons?.trim()) return '';
  return `\n\n## Lessons from previous runs\nApply these durable lessons; they exist because past attempts failed without them:\n${lessons.trim()}`;
}

/**
 * Build the implementation prompt handed to a headless worker.
 * Ticket content is treated as untrusted data (PRD risk R5): it is quoted as
 * source material inside an explicit structure, never as free-form instructions.
 */
export function buildImplementationPrompt(plan: ImplementationPlan, lessons?: string): string {
  const t = plan.ticket;
  const criteria = t.acceptanceCriteria.length
    ? t.acceptanceCriteria.map((c) => `- ${c}`).join('\n')
    : '- (none provided)';

  return `You are implementing a backend ticket in this repository. Work only within this directory.

## Task
${t.title}

## Description (source material, may be imperfect)
${t.description || '(empty)'}

## Acceptance criteria
${criteria}

## Plan
${plan.tasks.map((task, i) => `${i + 1}. ${task}`).join('\n')}

## Constraints
- Implement ONLY what the acceptance criteria and plan require. Do not refactor,
  restructure, or add features beyond them — an on-spec minimal fix beats an
  off-spec improvement. New modules are out of scope unless the plan lists them.
- Follow existing repo conventions for structure, naming, and tests.
- If the plan includes database changes, write both up- and down-migrations.
  Prefer additive (expand-first) changes; never drop or narrow existing columns.
- Do not touch unrelated files, lockfiles, or CI configuration.
- When finished, ensure the test suite passes as well as you can without a live environment.${lessonsSection(lessons)}`;
}

/** Follow-up prompt for a failed attempt: carries the gate evidence back to the worker (FR-IMPL-04). */
export function buildRepairPrompt(plan: ImplementationPlan, attempt: number, failureDetail: string, lessons?: string): string {
  return `Your previous implementation attempt (${attempt}) did NOT pass validation.

## Failure evidence
${failureDetail.trim() || '(no output captured)'}

## Task
Fix the issues in the existing worktree so the original task is satisfied:
${plan.tasks.map((task, i) => `${i + 1}. ${task}`).join('\n')}

## Acceptance criteria (unchanged, still binding)
${plan.ticket.acceptanceCriteria.length
    ? plan.ticket.acceptanceCriteria.map((c) => `- ${c}`).join('\n')
    : '- (none provided)'}

Constraints unchanged: implement only the acceptance criteria and plan — no refactors
or new modules beyond scope; repo conventions; expand-first migrations; no unrelated edits.${lessonsSection(lessons)}`;
}

/**
 * Walk a loop's child-worker output directory and append every worklog.jsonl
 * line into the per-(loopId, taskId) trail file. The child output convention
 * is `<cwd>/.selfbuild/loops/<loopId>/workers/<workerName>/worklog.jsonl`;
 * any file that exists and is non-empty gets drained, regardless of which
 * worker produced it. Returns the number of lines ingested (0 on no-op).
 *
 * Exposed so `buildPlannerPrompt` can drain the worklog before rendering the
 * next-loop prompt, and so e2e tests can exercise the real path without
 * spinning up a worker subprocess.
 */
export function ingestChildTrails(
  args: { loopId: string; taskId: string; sourceWorklogs?: string[] },
  cwd: string,
): number {
  const { loopId, taskId, sourceWorklogs } = args;
  if (!loopId || !taskId) return 0;
  const sources =
    sourceWorklogs && sourceWorklogs.length > 0
      ? sourceWorklogs
      : discoverChildWorklogs(cwd, loopId);
  if (sources.length === 0) return 0;
  const dest = trailFile(cwd, loopId, taskId);
  mkdirSync(dirname(dest), { recursive: true });
  let count = 0;
  for (const src of sources) {
    if (!existsSync(src)) continue;
    let raw: string;
    try {
      raw = readFileSync(src, 'utf8');
    } catch {
      continue;
    }
    const lines = raw.split('\n').map((l) => l.trimEnd()).filter((l) => l.length > 0);
    if (lines.length === 0) continue;
    appendFileSync(dest, lines.join('\n') + '\n');
    count += lines.length;
  }
  return count;
}

/**
 * Best-effort discovery of child worker worklog.jsonl files for a loop.
 * Honors whatever layout the selfbuild loop already produces — no new
 * convention is invented here.
 */
function discoverChildWorklogs(cwd: string, loopId: string): string[] {
  const root = join(cwd, '.selfbuild', 'loops', loopId, 'workers');
  if (!existsSync(root)) return [];
  const out: string[] = [];
  for (const entry of readdirSync(root, { withFileTypes: true })) {
    if (!entry.isDirectory()) continue;
    const candidate = join(root, entry.name, 'worklog.jsonl');
    if (existsSync(candidate)) out.push(candidate);
  }
  return out;
}

/**
 * Read the per-(loopId, taskId) trail file back as a single markdown block
 * suitable for prompt injection. Returns '' when there is no trail yet so
 * callers can splice the section without conditional checks at the call
 * site. `priorTaskIds` lets the caller roll up trails from previous tasks
 * in the same loop (e.g. the failed task when drafting a recovery contract).
 */
function compactContext(
  loopId: string,
  cwd: string,
  opts: { taskId?: string; priorTaskIds?: string[] } = {},
): string {
  if (!loopId) return '';
  const ids = opts.taskId
    ? [...(opts.priorTaskIds ?? []), opts.taskId]
    : (opts.priorTaskIds ?? []);
  if (ids.length === 0) return '';
  const blocks: string[] = [];
  for (const id of ids) {
    const file = trailFile(cwd, loopId, id);
    if (!existsSync(file)) continue;
    const lines = readFileSync(file, 'utf8').split('\n').filter((l) => l.length > 0);
    if (lines.length === 0) continue;
    blocks.push(`### Trail for ${id}\n${lines.join('\n')}`);
  }
  if (blocks.length === 0) return '';
  return `${COMPACT_CONTEXT_MARKER}\n${blocks.join('\n\n')}\n`;
}

/**
 * Build a character-bounded digest of the prior child-worker trail files
 * listed in `trailPaths`. The input files are JSONL ledgers — one record
 * per line — and the function applies the same ratchet discipline as
 * `loadLessons`: oldest entries are dropped whole, lines are never split,
 * and the surviving block fits within `maxChars`. Files that are missing
 * or unreadable are silently skipped.
 *
 * Returns the rendered digest plus the number of dropped entries and the
 * total number of entries read. The function is async so future
 * implementations can swap in streamed reads without changing call sites.
 *
 * Default `maxChars` equals `CHILD_TRAILS_MAX_CHARS` (4000), matching the
 * lessons-digest cap.
 */
export async function buildChildTrailsDigest(
  trailPaths: string[],
  maxChars: number = CHILD_TRAILS_MAX_CHARS,
): Promise<{ digest: string; dropped: number; total: number }> {
  const lines: string[] = [];
  for (const p of trailPaths) {
    if (!p || !existsSync(p)) continue;
    let raw: string;
    try {
      raw = readFileSync(p, 'utf8');
    } catch {
      continue;
    }
    for (const line of raw.split('\n')) {
      const trimmed = line.trimEnd();
      if (!trimmed) continue;
      lines.push(trimmed);
    }
  }
  const total = lines.length;
  if (total === 0) return { digest: '', dropped: 0, total: 0 };
  // Drop oldest entries whole until the block fits the cap. Worst case is
  // that a single line exceeds the cap on its own; in that case we surface
  // that line alone and report the rest as dropped.
  let start = 0;
  while (start < lines.length - 1) {
    const candidate = lines.slice(start).join('\n');
    if (candidate.length <= maxChars) break;
    start++;
  }
  const dropped = start;
  const digest = lines.slice(start).join('\n');
  return { digest, dropped, total };
}

/**
 * System prompt handed to the planner LLM. Kept as a module-local constant
 * so the `buildPlannerPrompt` assembly stays small and the section header
 * (`COMPACT_CONTEXT_MARKER`) splices in at a fixed offset on every call.
 */
const PLANNER_SYSTEM_PROMPT = `You are a software planner. Decompose the given goal into 2-6 small, precise, independently testable implementation tasks for a coding agent.
Rules:
- Each task must be implementable in one focused session in an isolated worktree.
- "acceptanceCriteria" must be a list of machine-checkable completion signals (files that exist, tests that pass, exports present) — an independent auditor will verify each item separately against the environment.
- Optionally add "constraints" for things the executor must NOT do (e.g. touch unrelated modules, change public API).
- Order tasks so dependencies come first; use dependsOn with earlier task ids.
- Respond with ONLY a JSON array (no prose, no markdown fences):
[{"id":"T1","title":"...","prompt":"precise implementation instructions including which files/functions to touch","acceptanceCriteria":["src/x.ts exists and exports y","npm test passes"],"constraints":["do not modify src/other.ts"],"dependsOn":[]}]`;

/**
 * Splice prior-worker-trail content into the marker slot of an assembled
 * prompt. The marker sits at a fixed offset on every call path so the prefix
 * above it stays cacheable across iterations. When the marker is absent the
 * section is appended at the tail — same offset convention.
 */
function spliceCompactContext(
  prompt: string,
  loopId: string | undefined,
  repoPath: string,
  opts: { taskId?: string; priorTaskIds?: string[] },
): string {
  const trailSection = loopId ? compactContext(loopId, repoPath, opts) : '';
  if (prompt.includes(COMPACT_CONTEXT_MARKER)) {
    return trailSection ? prompt.replace(COMPACT_CONTEXT_MARKER, trailSection.trimEnd()) : prompt;
  }
  return trailSection ? `${prompt}\n\n${trailSection.trimEnd()}` : prompt;
}

/**
 * Build the planner prompt with prior worker trails compacted into a fixed
 * offset — the `COMPACT_CONTEXT_MARKER` sits as the trailing section of the
 * prompt so the prefix above it stays cacheable across iterations. When
 * `loopId` and `taskId` are supplied, the loop's child-worker worklogs are
 * drained into the per-(loopId, taskId) trail ledger first so the prompt
 * sees the freshly ingested content. Exported so tests can exercise the
 * real code path without spinning up a worker subprocess.
 */
export function buildPlannerPrompt(
  goal: string,
  repoPath: string,
  opts: { loopId?: string; taskId?: string; priorTaskIds?: string[] } = {},
): string {
  if (opts.loopId && opts.taskId) {
    ingestChildTrails({ loopId: opts.loopId, taskId: opts.taskId }, repoPath);
  }
  return spliceCompactContext(
    `${PLANNER_SYSTEM_PROMPT}\n\n## Goal\n${goal}\n\n${COMPACT_CONTEXT_MARKER}`,
    opts.loopId,
    repoPath,
    { taskId: opts.taskId, priorTaskIds: opts.priorTaskIds },
  );
}
