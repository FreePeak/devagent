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

export interface AnswerEndpointResult {
  /** HTTP status code for the answer route */
  status: number;
  body: Record<string, unknown>;
}

/**
 * Repo-level answer application with HTTP semantics, shared by the `serve`
 * command's POST /api/answer route (LangGraph interrupt/resume pattern:
 * observation and resumption are separate endpoints; decisions bind
 * explicitly to a task id and reject cleanly when state moved on).
 */
export function applyAnswerToRepo(repoPath: string, taskId: string, answer: string): AnswerEndpointResult {
  if (!answer.trim()) {
    return { status: 400, body: { ok: false, note: 'empty answer' } };
  }
  const board = loadBoard(repoPath);
  if (!board) {
    return { status: 404, body: { ok: false, note: 'no project board for this repo' } };
  }
  const r = applyHumanAnswer(board, taskId, answer);
  if (!r.ok) {
    // unknown id or task no longer in 'ask': a stale/duplicate decision is a conflict
    return { status: 409, body: { ok: false, note: r.note } };
  }
  saveBoard(repoPath, board);
  return { status: 200, body: { ok: true, note: r.note } };
}
>>>>>>> devagent/loop44-mcp-answer
