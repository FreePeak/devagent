import { describe, expect, it } from 'vitest';
import { buildAuditPrompt, parseAuditReport } from '../src/orchestrator/auditor.js';
import { runScheduler } from '../src/orchestrator/scheduler.js';
import { recomputeReadiness } from '../src/orchestrator/types.js';
import type { AuditVerdict, OrchestratorTask, ProjectBoard } from '../src/orchestrator/types.js';
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

function board(tasks: OrchestratorTask[]): ProjectBoard {
  return {
    goal: 'test goal',
    createdAt: new Date().toISOString(),
    updatedAt: new Date().toISOString(),
    roles: { planner: 'claude-code', executor: 'claude-code', auditor: 'claude-code' },
    tasks,
  };
}

const cleanPass = (criterion = 'tests pass'): AuditVerdict => ({
  verdict: 'pass',
  integrity: 'clean',
  criteriaResults: [{ criterion, met: true, evidence: 'npm test: 12 passed' }],
  summary: 'ran the suite; all green',
});

describe('parseAuditReport', () => {
  it('parses a valid report with surrounding prose/fences', () => {
    const text = 'Here you go:\n```json\n{"verdict":"pass","integrity":"clean","criteriaResults":[{"criterion":"x exists","met":true,"evidence":"ls shows src/x.ts"}],"summary":"checked"}\n```';
    const r = parseAuditReport(text)!;
    expect(r.verdict).toBe('pass');
    expect(r.criteriaResults).toHaveLength(1);
    expect(r.criteriaResults[0]!.evidence).toContain('ls');
  });

  it('rejects malformed shapes field-by-field', () => {
    expect(parseAuditReport('no json here')).toBeNull();
    expect(parseAuditReport('{"verdict":"maybe","integrity":"clean","criteriaResults":[{"criterion":"a","met":true,"evidence":"e"}],"summary":"s"}')).toBeNull();
    expect(parseAuditReport('{"verdict":"pass","integrity":"clean","criteriaResults":[],"summary":"s"}')).toBeNull();
    expect(parseAuditReport('{"verdict":"pass","integrity":"clean","criteriaResults":[{"criterion":"a","met":"yes","evidence":"e"}],"summary":"s"}')).toBeNull();
  });

  it('coerces self-contradictory pass-with-unmet-criteria to fail', () => {
    const r = parseAuditReport(
      '{"verdict":"pass","integrity":"clean","criteriaResults":[{"criterion":"a","met":true,"evidence":"e1"},{"criterion":"b","met":false,"evidence":"missing"}],"summary":"s"}',
    )!;
    expect(r.verdict).toBe('fail');
  });
});

describe('buildAuditPrompt', () => {
  it('includes criteria, constraints, and the untrusted executor claim', () => {
    const t = task({
      id: 'T1',
      acceptanceCriteria: ['src/x.ts exists'],
      boundaryConstraints: ['do not touch src/y.ts'],
      prompt: 'implement x',
    });
    const p = buildAuditPrompt({ goal: 'ship it', task: t, executorDetail: 'I am done' });
    expect(p).toContain('read-only');
    expect(p).toContain('1. src/x.ts exists');
    expect(p).toContain('- do not touch src/y.ts');
    expect(p).toContain('untrusted');
    expect(p).toContain('I am done');
  });

  it('falls back to expectedOutput when no criteria list', () => {
    const t = task({ id: 'T1', expectedOutput: 'npm test passes' });
    expect(buildAuditPrompt({ goal: 'g', task: t })).toContain('npm test passes');
  });
});

describe('audit-gated scheduler transitions', () => {
  it('promotes executor success to done only on clean passing audit', async () => {
    const b = board([task({ id: 'T1' })]);
    await runScheduler(
      b,
      { repoPath: '.', executor: 'claude-code', concurrency: 1, maxTaskRetries: 1, timeoutMs: 1000 },
      {
        executeTask: async () => ({ ok: true }),
        auditTask: async () => cleanPass(),
      },
      log,
    );
    expect(b.tasks[0]!.status).toBe('done');
    expect(b.tasks[0]!.audit?.verdict).toBe('pass');
    expect(b.tasks[0]!.evidenceGaps).toBeUndefined();
  });

  it('never trusts an executor claim alone when auditing is enabled', async () => {
    const b = board([task({ id: 'T1' })]);
    let audits = 0;
    await runScheduler(
      b,
      { repoPath: '.', executor: 'claude-code', concurrency: 1, maxTaskRetries: 2, timeoutMs: 1000 },
      {
        executeTask: async () => ({ ok: true }),
        auditTask: async () => {
          audits += 1;
          return null; // inconclusive
        },
      },
      log,
    );
    expect(audits).toBe(2); // initial + one retry within budget
    expect(b.tasks[0]!.status).toBe('failed');
    expect(b.tasks[0]!.evidenceGaps![0]).toContain('inconclusive');
  });

  it('externalizes failed-audit evidence gaps and retries against them', async () => {
    const b = board([
      task({
        id: 'T1',
        acceptanceCriteria: ['a holds', 'b holds'],
        evidenceGaps: undefined,
      }),
    ]);
    const promptsSeen: string[][] = [];
    const fail = (criterion: string): AuditVerdict => ({
      verdict: 'fail',
      integrity: 'clean',
      criteriaResults: [{ criterion, met: false, evidence: `grep found nothing for ${criterion}` }],
      summary: 'not done',
    });
    await runScheduler(
      b,
      { repoPath: '.', executor: 'claude-code', concurrency: 1, maxTaskRetries: 2, timeoutMs: 1000 },
      {
        executeTask: async ({ task: t }) => {
          promptsSeen.push(t.evidenceGaps ?? []);
          return { ok: true };
        },
        auditTask: async ({ task: t }) => (t.attempts >= 2 ? cleanPass('b holds') : fail('b holds')),
      },
      log,
    );
    expect(b.tasks[0]!.status).toBe('done');
    // first attempt saw no gaps, retry saw the targeted gap from the audit
    expect(promptsSeen[0]).toEqual([]);
    expect(promptsSeen[1]!.join(' ')).toContain('unmet: b holds');
  });

  it('rejects a passing verdict with integrity violation (mutation voids report)', async () => {
    const b = board([task({ id: 'T1' })]);
    const v = { ...cleanPass(), integrity: 'violation' as const };
    await runScheduler(
      b,
      { repoPath: '.', executor: 'claude-code', concurrency: 1, maxTaskRetries: 1, timeoutMs: 1000 },
      {
        executeTask: async () => ({ ok: true }),
        auditTask: async () => v,
      },
      log,
    );
    expect(b.tasks[0]!.status).toBe('failed');
    expect(b.tasks[0]!.failureDetail).toContain('integrity violation');
  });

  it('legacy mode without auditTask still completes directly', async () => {
    const b = board([task({ id: 'T1' })]);
    await runScheduler(
      b,
      { repoPath: '.', executor: 'opencode', concurrency: 1, maxTaskRetries: 1, timeoutMs: 1000 },
      { executeTask: async () => ({ ok: true }) },
      log,
    );
    expect(b.tasks[0]!.status).toBe('done');
  });
});

describe('readiness with audit-era statuses', () => {
  it('holds dependents while upstream is untrusted; blocks on ask', () => {
    const tasks = [
      task({ id: 'A', status: 'untrusted' }),
      task({ id: 'B', dependsOn: ['A'] }), // waits: A not audited yet
      task({ id: 'C', status: 'ask' }),
      task({ id: 'D', dependsOn: ['C'] }), // blocked: human input pending
    ];
    const out = recomputeReadiness(tasks);
    expect(out.find((t) => t.id === 'B')!.status).toBe('pending');
    expect(out.find((t) => t.id === 'D')!.status).toBe('blocked');
  });
});
