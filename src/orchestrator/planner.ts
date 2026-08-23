import type { OrchestratorTask } from './types.js';
import type { WorkerName } from '../types.js';
import { join } from 'node:path';
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
  // Tolerate markdown fences and surrounding prose around the JSON array
  const fenced = output.match(/```(?:json)?\s*([\s\S]*?)```/);
  const candidate = fenced ? fenced[1]! : output;
  const start = candidate.indexOf('[');
  const end = candidate.lastIndexOf(']');
  if (start === -1 || end <= start) return null;
  let raw: RawTask[];
  try {
    raw = JSON.parse(candidate.slice(start, end + 1)) as RawTask[];
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
  let plan = result.timedOut ? null : parsePlan(result.resultText ?? '');
  // Live-smoke lesson: claude occasionally returns exit 0 with empty stdout
  // (transient). One retry before falling back.
  if (!plan && !result.timedOut && !result.resultText) {
    const retry = await worker.spawn({
      prompt: `${PLANNER_SYSTEM_PROMPT}\n\n## Goal\n${goal}`,
      cwd: repoPath,
      timeoutMs,
    });
    plan = retry.timedOut ? null : parsePlan(retry.resultText ?? '');
    if (plan) return plan;
  }
  if (plan) return plan;
  // Observability: persist raw planner output so parse failures are debuggable
  const { appendFileSync, mkdirSync } = await import('node:fs');
  try {
    mkdirSync(join(repoPath, '.devagent-planner'), { recursive: true });
    appendFileSync(
      join(repoPath, '.devagent-planner', `plan-${Date.now()}.txt`),
      `--- timedOut=${result.timedOut} exitCode=${result.exitCode} resultBytes=${(result.resultText ?? '').length} stderr=${(result as unknown as { stderr?: string }).stderr?.slice(0, 200) ?? 'n/a'} ---\n${result.resultText ?? '(empty)'}\n`,
    );
  } catch {
    // best-effort diagnostics only
  }
  return fallbackPlan(goal);
}
