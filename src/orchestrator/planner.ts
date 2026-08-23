import type { OrchestratorTask } from './types.js';
import type { WorkerName } from '../types.js';
import { spawnCli } from '../workers/spawn-utils.js';

/**
 * Planner role: a headless planning agent (default claude-code) decomposes
 * the goal into small precise tasks as JSON. The plan is untrusted data:
 * validated field-by-field, ids normalized, dependency cycles rejected,
 * and any parse failure falls back to a single-task plan so orchestration
 * always makes progress.
 */

const PLANNER_SYSTEM_PROMPT = `You are a software planner. Decompose the given goal into 2-6 small, precise, independently testable implementation tasks for a coding agent.
Rules:
- Each task must be implementable in one focused session in an isolated worktree.
- "expected" must state how to verify completion (files that exist, tests that pass) — make it machine-checkable.
- Order tasks so dependencies come first; use dependsOn with earlier task ids.
- Respond with ONLY a JSON array (no prose, no markdown fences):
[{"id":"T1","title":"...","prompt":"precise implementation instructions including which files/functions to touch","expected":"e.g. 'src/x.ts exports y and npm test passes'","dependsOn":[]}]`;

interface RawTask {
  id?: unknown;
  title?: unknown;
  prompt?: unknown;
  expected?: unknown;
  dependsOn?: unknown;
}

export function parsePlan(output: string): OrchestratorTask[] | null {
  // Tolerate markdown fences around the JSON array
  const match = output.match(/\[[\s\S]*\]/);
  if (!match) return null;
  let raw: RawTask[];
  try {
    raw = JSON.parse(match[0]) as RawTask[];
  } catch {
    return null;
  }
  if (!Array.isArray(raw) || raw.length === 0 || raw.length > 12) return null;

  const tasks: OrchestratorTask[] = [];
  const seen = new Set<string>();
  for (let i = 0; i < raw.length; i++) {
    const r = raw[i]!;
    if (typeof r.title !== 'string' || !r.title.trim()) return null;
    if (typeof r.prompt !== 'string' || !r.prompt.trim()) return null;
    const id = typeof r.id === 'string' && r.id.trim() ? r.id.trim() : `T${i + 1}`;
    if (seen.has(id)) return null; // duplicate ids: reject whole plan
    seen.add(id);
    tasks.push({
      id,
      title: r.title.slice(0, 120),
      prompt: r.prompt,
      expectedOutput: typeof r.expected === 'string' ? r.expected : undefined,
      dependsOn: Array.isArray(r.dependsOn)
        ? r.dependsOn.filter((d): d is string => typeof d === 'string')
        : [],
      status: 'pending',
      attempts: 0,
    });
  }
  // Normalize dependsOn to known, non-self ids only
  const ids = new Set(tasks.map((t) => t.id));
  for (const t of tasks) {
    t.dependsOn = [...new Set(t.dependsOn)].filter((d) => ids.has(d) && d !== t.id);
  }
  if (hasCycle(tasks)) return null;
  return tasks;
}

export function hasCycle(tasks: OrchestratorTask[]): boolean {
  const byId = new Map(tasks.map((t) => [t.id, t]));
  const state = new Map<string, 'visiting' | 'done'>();
  const visit = (id: string): boolean => {
    const s = state.get(id);
    if (s === 'visiting') return true;
    if (s === 'done') return false;
    state.set(id, 'visiting');
    for (const d of byId.get(id)?.dependsOn ?? []) {
      if (byId.has(d) && visit(d)) return true;
    }
    state.set(id, 'done');
    return false;
  };
  return tasks.some((t) => visit(t.id));
}

export function fallbackPlan(goal: string): OrchestratorTask[] {
  return [
    {
      id: 'T1',
      title: `Implement goal: ${goal.slice(0, 80)}`,
      prompt: goal,
      dependsOn: [],
      status: 'pending',
      attempts: 0,
    },
  ];
}

export async function runPlanner(
  goal: string,
  repoPath: string,
  plannerWorker: WorkerName,
  timeoutMs: number,
): Promise<OrchestratorTask[]> {
  const { getWorker } = await import('../workers/index.js');
  const worker = getWorker(plannerWorker);
  const result = await worker.spawn({
    prompt: `${PLANNER_SYSTEM_PROMPT}\n\n## Goal\n${goal}`,
    cwd: repoPath,
    timeoutMs,
  });
  const plan = result.timedOut ? null : parsePlan(result.resultText ?? '');
  return plan ?? fallbackPlan(goal);
}
