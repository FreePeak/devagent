import { existsSync, readFileSync, renameSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import type { OrchestratorTask, ProjectBoard } from './types.js';
import { recomputeReadiness } from './types.js';

/**
 * Durable project board: JSON at <repoPath>/.devagent-project.json, written
 * atomically (tmp+rename) so a crash mid-write never corrupts state. The
 * board is the orchestrator's source of truth — resume reads it back.
 */

export function loadBoard(repoPath: string): ProjectBoard | null {
  const file = join(repoPath, '.devagent-project.json');
  if (!existsSync(file)) return null;
  try {
    const raw = JSON.parse(readFileSync(file, 'utf8')) as ProjectBoard;
    if (!Array.isArray(raw.tasks) || typeof raw.goal !== 'string') return null;
    return raw;
  } catch {
    return null; // corrupted board: caller decides to re-plan
  }
}

export function saveBoard(repoPath: string, board: ProjectBoard): void {
  const file = join(repoPath, '.devagent-project.json');
  const next = { ...board, updatedAt: new Date().toISOString(), tasks: recomputeReadiness(board.tasks) };
  const tmp = `${file}.tmp`;
  writeFileSync(tmp, `${JSON.stringify(next, null, 2)}\n`);
  renameSync(tmp, file);
}

export function createBoard(
  goal: string,
  tasks: OrchestratorTask[],
  roles: ProjectBoard['roles'],
): ProjectBoard {
  return {
    goal,
    createdAt: new Date().toISOString(),
    updatedAt: new Date().toISOString(),
    roles,
    tasks,
  };
}

/**
 * Validate-before-spend rendering for `orchestrate --plan-only`: the full
 * contracts (prompt, acceptance criteria, boundary constraints) so an
 * operator can review the plan before any executor spend.
 */
export function formatPlanOnly(board: ProjectBoard): string {
  const lines: string[] = ['Contracts:'];
  for (const t of board.tasks) {
    lines.push('', `[${t.id}] ${t.title} (${t.status})`, t.prompt);
    if (t.acceptanceCriteria?.length) lines.push(`  criteria:\n    ${t.acceptanceCriteria.join('\n    ')}`);
    if (t.boundaryConstraints?.length) lines.push(`  constraints:\n    ${t.boundaryConstraints.join('\n    ')}`);
  }
  return lines.join('\n');
}
