import { describe, expect, it } from 'vitest';
import { parsePlan, fallbackPlan, hasCycle } from '../src/orchestrator/planner.js';
import { recomputeReadiness } from '../src/orchestrator/types.js';
import { runScheduler } from '../src/orchestrator/scheduler.js';
import type { OrchestratorTask, ProjectBoard } from '../src/orchestrator/types.js';
import { RunLogger } from '../src/logger.js';

const log = new RunLogger();

function task(partial: Partial<OrchestratorTask> & { id: string }): OrchestratorTask {
  return {
    title: partial.id,
    prompt: 'do it',
    dependsOn: [],
    status: 'pending',
    attempts: 0,
    ...partial,
  };
}

describe('parsePlan', () => {
  it('parses a valid JSON array plan and normalizes deps', () => {
    const out = '```json\n[{"id":"T1","title":"a","prompt":"pa","dependsOn":[]},{"id":"T2","title":"b","prompt":"pb","dependsOn":["T1","T9","T2"]}]\n```';
    const tasks = parsePlan(out)!;
    expect(tasks).toHaveLength(2);
    expect(tasks[1]!.dependsOn).toEqual(['T1']); // unknown + self deps dropped
  });

  it('rejects duplicate ids, cycles, empty arrays, and non-JSON', () => {
    expect(parsePlan('[{"id":"T1","title":"a","prompt":"p","dependsOn":[]},{"id":"T1","title":"b","prompt":"q"}]')).toBeNull();
    expect(
      parsePlan('[{"id":"T1","title":"a","prompt":"p","dependsOn":["T2"]},{"id":"T2","title":"b","prompt":"q","dependsOn":["T1"]}]'),
    ).toBeNull();
    expect(parsePlan('[]')).toBeNull();
    expect(parsePlan('I will implement this in three steps')).toBeNull();
  });

  it('assigns sequential ids when missing', () => {
    expect(parsePlan('[{"title":"a","prompt":"p"},{"title":"b","prompt":"q"}]')!.map((t) => t.id)).toEqual(['T1', 'T2']);
  });
});

describe('hasCycle + fallbackPlan', () => {
  it('detects self-cycles', () => {
    expect(hasCycle([task({ id: 'T1', dependsOn: ['T1'] })])).toBe(true);
    expect(hasCycle([task({ id: 'T1' }), task({ id: 'T2', dependsOn: ['T1'] })])).toBe(false);
  });

  it('fallback is always a single runnable task', () => {
    const fb = fallbackPlan('build the thing');
    expect(fb).toHaveLength(1);
    expect(recomputeReadiness(fb)[0]!.status).toBe('ready');
  });
});

describe('recomputeReadiness', () => {
  it('promotes pending tasks when all deps done; blocks on failed/deps missing', () => {
    const tasks = [
      task({ id: 'T1', status: 'done' }),
      task({ id: 'T2', dependsOn: ['T1'] }), // -> ready
      task({ id: 'T3', dependsOn: ['T9'] }), // -> blocked (dangling)
      task({ id: 'T4', status: 'failed' }),
      task({ id: 'T5', dependsOn: ['T4'] }), // -> blocked (upstream failed)
    ];
    const byId = new Map(recomputeReadiness(tasks).map((t) => [t.id, t.status]));
    expect(byId.get('T2')).toBe('ready');
    expect(byId.get('T3')).toBe('blocked');
    expect(byId.get('T5')).toBe('blocked');
  });

  it('unblocks dependency-blocked tasks when upstream deps become done (loop-55 T3 stall)', () => {
    // Regression: T2 paused on `ask`, which cascaded T3 to `blocked`. After the
    // human answered and T2 reached `done`, a bare --resume must promote T3
    // back to `ready` - previously recomputeReadiness only moved `pending`
    // tasks, so `blocked` was terminal and the board livelocked at 2/5 done.
    const tasks = [
      task({ id: 'T1', status: 'done' }),
      task({ id: 'T2', status: 'done', dependsOn: ['T1'] }),
      task({ id: 'T3', status: 'blocked', dependsOn: ['T2'] }), // -> ready again
      task({ id: 'T4', status: 'blocked', dependsOn: ['T3'] }), // stays blocked (T3 not done)
    ];
    const byId = new Map(recomputeReadiness(tasks).map((t) => [t.id, t.status]));
    expect(byId.get('T3')).toBe('ready');
    expect(byId.get('T4')).toBe('blocked');
  });

  it('never resurrects tasks blocked for non-dependency reasons', () => {
    // A task the planner explicitly blocked (dangling dep) or that failed
    // terminally must not be silently promoted when deps later look done.
    const tasks = [
      task({ id: 'T1', status: 'done' }),
      task({ id: 'T2', status: 'failed' }),
      task({ id: 'T3', status: 'blocked', dependsOn: ['T2'] }), // upstream failed -> stays blocked
      task({ id: 'T4', status: 'blocked', dependsOn: ['T1', 'T99'] }), // dangling dep -> stays blocked
    ];
    const byId = new Map(recomputeReadiness(tasks).map((t) => [t.id, t.status]));
    expect(byId.get('T3')).toBe('blocked');
    expect(byId.get('T4')).toBe('blocked');
  });
});

describe('runScheduler', () => {
  function board(tasks: OrchestratorTask[]): ProjectBoard {
    return { goal: 'g', createdAt: '', updatedAt: '', roles: { planner: 'claude-code', executor: 'opencode' }, tasks };
  }

  it('executes in dependency waves: T2 runs only after T1 done', async () => {
    const order: string[] = [];
    const b = board([task({ id: 'T1' }), task({ id: 'T2', dependsOn: ['T1'] })]);
    await runScheduler(
      b,
      { repoPath: '.', executor: 'opencode', concurrency: 4, maxTaskRetries: 1, timeoutMs: 1000 },
      {
        executeTask: async ({ task: t }) => {
          order.push(t.id);
          return { ok: true };
        },
      },
      log,
    );
    expect(order).toEqual(['T1', 'T2']);
    expect(b.tasks.every((t) => t.status === 'done')).toBe(true);
  });

  it('retries a failed task within budget then succeeds', async () => {
    let calls = 0;
    const b = board([task({ id: 'T1' })]);
    await runScheduler(
      b,
      { repoPath: '.', executor: 'opencode', concurrency: 2, maxTaskRetries: 2, timeoutMs: 1000 },
      {
        executeTask: async () => {
          calls += 1;
          return { ok: calls >= 2, detail: calls < 2 ? 'tests failed' : undefined };
        },
      },
      log,
    );
    expect(calls).toBe(2);
    expect(b.tasks[0]!.status).toBe('done');
  });

  it('marks failed permanently when retries exhausted and blocks dependents', async () => {
    const b = board([task({ id: 'T1' }), task({ id: 'T2', dependsOn: ['T1'] })]);
    await runScheduler(
      b,
      { repoPath: '.', executor: 'opencode', concurrency: 2, maxTaskRetries: 1, timeoutMs: 1000 },
      { executeTask: async () => ({ ok: false, detail: 'boom' }) },
      log,
    );
    expect(b.tasks[0]!.status).toBe('failed');
    expect(b.tasks[1]!.status).toBe('blocked');
  });

  it('runs independent tasks concurrently up to the cap', async () => {
    let active = 0;
    let peak = 0;
    const b = board([task({ id: 'A' }), task({ id: 'B' }), task({ id: 'C' })]);
    await runScheduler(
      b,
      { repoPath: '.', executor: 'opencode', concurrency: 2, maxTaskRetries: 1, timeoutMs: 1000 },
      {
        executeTask: async () => {
          active += 1;
          peak = Math.max(peak, active);
          await new Promise((r) => setTimeout(r, 10));
          active -= 1;
          return { ok: true };
        },
      },
      log,
    );
    expect(peak).toBeLessThanOrEqual(2);
    expect(b.tasks.every((t) => t.status === 'done')).toBe(true);
  });
});

describe('runScheduler wave persistence', () => {
  it('persists the board after each wave', async () => {
    const { runScheduler: rs } = await import('../src/orchestrator/scheduler.js');
    const b = {
      goal: 'g', createdAt: '', updatedAt: '',
      roles: { planner: 'claude-code' as const, executor: 'opencode' as const },
      tasks: [task({ id: 'T1' }), task({ id: 'T2', dependsOn: ['T1'] })],
    };
    let persists = 0;
    await rs(
      b,
      { repoPath: '.', executor: 'opencode', concurrency: 2, maxTaskRetries: 1, timeoutMs: 1000, onWavePersisted: () => (persists += 1) },
      { executeTask: async () => ({ ok: true }) },
      log,
    );
    expect(persists).toBe(2); // one per wave
  });
});

describe('parsePlan robustness (live-smoke findings)', () => {
  it('parses JSON wrapped in prose and fences', () => {
    const out = 'Here is my plan:\n```json\n[{"id":"T1","title":"a","prompt":"p","dependsOn":[]}]\n```\nLet me know if you want changes.';
    expect(parsePlan(out)!.map((t) => t.id)).toEqual(['T1']);
  });

  it('parses trailing-comma-free arrays embedded in chatter', () => {
    const out = 'Sure! [ {"title":"a","prompt":"p"} ] hope that helps';
    expect(parsePlan(out)).toHaveLength(1);
  });
});
