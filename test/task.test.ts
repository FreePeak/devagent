import { describe, expect, it } from 'vitest';
import { defaultTaskId, runTask, syntheticTicketFromPrompt } from '../src/task.js';
import type { TaskDeps } from '../src/task.js';
import { RunLogger } from '../src/logger.js';

describe('defaultTaskId', () => {
  it('produces sanitize-safe TASK-prefixed ids unique per invocation', () => {
    const a = defaultTaskId();
    const b = defaultTaskId();
    expect(a).toMatch(/^TASK-[a-z0-9]+-[a-z0-9]{2,6}$/);
    expect(b).toMatch(/^TASK-[a-z0-9]+-[a-z0-9]{2,6}$/);
    expect(a).not.toBe(b);
  });

  it('uses the injected clock for the epoch segment', () => {
    expect(defaultTaskId(() => 0)).toMatch(/^TASK-0-[a-z0-9]{2,6}$/);
  });
});

describe('syntheticTicketFromPrompt', () => {
  it('splits first line as title, rest as description', () => {
    const t = syntheticTicketFromPrompt('Add rate limiting\n\nUse a token bucket per client IP.');
    expect(t.title).toBe('Add rate limiting');
    expect(t.description).toBe('Use a token bucket per client IP.');
    expect(t.labels).toContain('orchestrated');
  });

  it('uses the whole prompt as description when single-line', () => {
    const t = syntheticTicketFromPrompt('Only one line');
    expect(t.title).toBe('Only one line');
    expect(t.description).toBe('Only one line');
  });

  it('caps title at 80 chars', () => {
    expect(syntheticTicketFromPrompt('x'.repeat(200)).title).toHaveLength(80);
  });

  it('honors an explicit taskId for id and trackerInternalId', () => {
    const t = syntheticTicketFromPrompt('do thing', 'loop-66');
    expect(t.id).toBe('loop-66');
    expect(t.trackerInternalId).toBe('loop-66');
  });

  it('defaults to a unique collision-free id per call', () => {
    const a = syntheticTicketFromPrompt('one');
    const b = syntheticTicketFromPrompt('two');
    expect(a.id).not.toBe('TASK');
    expect(b.id).not.toBe(a.id);
  });
});

function fakeDeps(ok: boolean, prUrl?: string): TaskDeps {
  return {
    runPipelineDeps: {
      fetchTicket: async () => ({ id: 'TASK', title: '', description: '', labels: [], acceptanceCriteria: [] }),
      runGateG3: () => ({ passed: true, findings: [], detail: '' }),
    },
    implementStage: async (_cfg, ticket) => {
      seenTicket = ticket;
      return { ok, worker: 'claude-code', attempts: 1, worktreePath: ok ? '/wt' : undefined };
    },
    publishStage: async () => prUrl,
  };
}

let seenTicket: { id: string } | undefined;

describe('runTask', () => {
  const log = new RunLogger();

  it('threads opts.taskId into the dispatched ticket', async () => {
    seenTicket = undefined;
    await runTask(
      { prompt: 'do thing', repoPath: '.', autoPr: false, maxLoops: 1, timeoutMs: 1000, log, taskId: 'loop-66-x' },
      fakeDeps(true),
    );
    expect(seenTicket?.id).toBe('loop-66-x');
  });

  it('reports failure without publishing when implementation fails', async () => {
    const r = await runTask(
      { prompt: 'do thing', repoPath: '.', autoPr: true, maxLoops: 1, timeoutMs: 1000, log },
      fakeDeps(false),
    );
    expect(r.ok).toBe(false);
    expect(r.prUrl).toBeUndefined();
  });

  it('skips publish without --auto-pr and reports worktree', async () => {
    const r = await runTask(
      { prompt: 'do thing', repoPath: '.', autoPr: false, maxLoops: 1, timeoutMs: 1000, log },
      fakeDeps(true, 'https://example/pr/1'),
    );
    expect(r.ok).toBe(true);
    expect(r.prUrl).toBeUndefined(); // never called publisher
    expect(r.note).toContain('/wt');
  });

  it('publishes with --auto-pr and returns PR url', async () => {
    const r = await runTask(
      { prompt: 'do thing', repoPath: '.', autoPr: true, maxLoops: 1, timeoutMs: 1000, log },
      fakeDeps(true, 'https://example/pr/2'),
    );
    expect(r.ok).toBe(true);
    expect(r.prUrl).toBe('https://example/pr/2');
  });

  it('handles missing credentials gracefully in auto-pr mode', async () => {
    const deps = fakeDeps(true);
    deps.publishStage = async () => undefined;
    const r = await runTask(
      { prompt: 'do thing', repoPath: '.', autoPr: true, maxLoops: 1, timeoutMs: 1000, log },
      deps,
    );
    expect(r.ok).toBe(true);
    expect(r.note).toContain('no remote credentials');
  });
});
