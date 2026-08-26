import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { TicketSpec } from './types.js';
import type { ImplementationPlan } from './planner.js';

/** Default repo-local lessons file; overridable via config `lessonsFile`. */
export const DEFAULT_LESSONS_FILE = '.devagent/lessons.md';
/** Lessons are context, not the task: cap to the tail so prompts stay bounded. */
const LESSONS_MAX_LINES = 40;
/** Hard character budget for injected lessons (PRD Q9: distilled, not verbatim). */
export const LESSONS_MAX_CHARS = 4000;

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
  const p = join(repoPath, lessonsFile || DEFAULT_LESSONS_FILE);
  if (!existsSync(p)) return '';
  try {
    const lines = readFileSync(p, 'utf8').trimEnd().split('\n').slice(-LESSONS_MAX_LINES);
    const budget = maxChars ?? LESSONS_MAX_CHARS;
    let total = -1; // joining N lines adds N-1 newlines
    let start = lines.length;
    while (start > 0) {
      const cost = lines[start - 1]!.length + 1;
      if (start < lines.length && total + cost > budget) break;
      total += cost;
      start--;
    }
    return lines.slice(start).join('\n').trim();
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
