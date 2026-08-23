import { describe, expect, it } from 'vitest';
import { topoOrder } from '../src/orchestrator/merge.js';
import type { ProjectBoard } from '../src/orchestrator/types.js';

function board(tasks: ProjectBoard['tasks']): ProjectBoard {
  return { goal: 'g', createdAt: '', updatedAt: '', roles: { planner: 'claude-code', executor: 'opencode' }, tasks };
}

describe('topoOrder', () => {
  it('emits dependencies before dependents', () => {
    const b = board([
      { id: 'T3', title: '', prompt: '', dependsOn: ['T1', 'T2'], status: 'done', attempts: 1 },
      { id: 'T1', title: '', prompt: '', dependsOn: [], status: 'done', attempts: 1 },
      { id: 'T2', title: '', prompt: '', dependsOn: ['T1'], status: 'done', attempts: 2 },
    ]);
    const order = topoOrder(b);
    expect(order.indexOf('T1')).toBeLessThan(order.indexOf('T2'));
    expect(order.indexOf('T2')).toBeLessThan(order.indexOf('T3'));
  });

  it('includes only done tasks in merge candidates when filtered by caller', () => {
    // mergeProjectBranches filters to done; topoOrder covers all — contract check
    const b = board([
      { id: 'A', title: '', prompt: '', dependsOn: [], status: 'done', attempts: 1 },
      { id: 'B', title: '', prompt: '', dependsOn: ['A'], status: 'failed', attempts: 1 },
    ]);
    const done = new Set(b.tasks.filter((t) => t.status === 'done').map((t) => t.id));
    expect(topoOrder(b).filter((id) => done.has(id))).toEqual(['A']);
  });
});
