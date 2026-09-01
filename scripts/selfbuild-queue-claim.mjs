#!/usr/bin/env node
// Queue-first goal selection for the selfbuild loop (scripts/selfbuild-loop.sh).
//
// Claims the oldest pending task from .devagent/queue and prints JSON:
//   {"id":"...","goal":"..."}   — a task was claimed (loop implements its goal)
//   {}                          — queue empty (loop falls back to LLM selection)
//
// Rationale: pending queue tasks (scout PRDs, backlog items) are concrete,
// already-validated work and outrank fresh LLM goal invention. Without this
// hook a full queue blocks the scout at maxQueued while the loop keeps
// inventing new goals — the 2026-09-01 deadlock (8 pending, 0 consumed).
// On task completion the loop marks the entry done (selfbuild-queue-done.js).
import { claimNextPending } from '../src/queue.ts';
import { resolve } from 'node:path';

const repo = process.argv[2] ?? process.cwd();
const repoPath = resolve(repo);
const task = claimNextPending(repoPath, `selfbuild-loop-${process.pid}`);
if (!task) {
  console.log('{}');
  process.exit(0);
}
const goal = task.goal?.trim() || task.title?.trim() || '';
if (!goal) {
  // Unusable entry: fail it so it never blocks the queue head again.
  const { setTaskStatus } = await import('../src/queue.js');
  setTaskStatus(repoPath, task.id, 'failed', 'empty goal and title');
  console.log('{}');
  process.exit(0);
}
console.log(JSON.stringify({ id: task.id, goal: goal.startsWith('Goal:') ? goal : `Goal: ${goal}` }));
