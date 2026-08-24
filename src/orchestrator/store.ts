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
 * Human-in-the-loop: resolve a task the auditor paused with verdict 'ask'.
 * The answer folds into the contract and audit state resets so the next
 * attempt is re-verified against it. Shared by the CLI (--answer) and the
 * devagent_answer MCP tool.
 */
export function applyHumanAnswer(
  board: ProjectBoard,
  taskId: string,
  answer: string,
): { ok: boolean; note: string } {
  const id = taskId.trim();
  const text = answer.trim();
  const t = board.tasks.find((x) => x.id === id);
  if (!t || t.status !== 'ask' || !text) {
    return { ok: false, note: `no task '${id}' paused for input (or empty answer)` };
  }
  t.prompt += `\n\nHuman answer to "${t.failureDetail ?? 'prior question'}": ${text}`;
  t.evidenceGaps = undefined;
  t.status = 'pending';
  return { ok: true, note: `Answered ${id}; task back in queue.` };
}
