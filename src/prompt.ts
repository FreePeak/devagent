import type { TicketSpec } from './types.js';
import type { ImplementationPlan } from './planner.js';

/**
 * Build the implementation prompt handed to a headless worker.
 * Ticket content is treated as untrusted data (PRD risk R5): it is quoted as
 * source material inside an explicit structure, never as free-form instructions.
 */
export function buildImplementationPrompt(plan: ImplementationPlan): string {
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
- Follow existing repo conventions for structure, naming, and tests.
- If the plan includes database changes, write both up- and down-migrations.
  Prefer additive (expand-first) changes; never drop or narrow existing columns.
- Do not touch unrelated files, lockfiles, or CI configuration.
- When finished, ensure the test suite passes as well as you can without a live environment.`;
}
