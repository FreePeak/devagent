import { describe, expect, it } from 'vitest';
import { buildAuditPrompt, parseAuditReport } from '../src/orchestrator/auditor.js';
import { parseRecoveryContract } from '../src/orchestrator/planner.js';
import { runScheduler } from '../src/orchestrator/scheduler.js';
import { attemptSuffix, recomputeReadiness } from '../src/orchestrator/types.js';
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

  it('accepts ask verdicts with empty criteria but requires the question', () => {
    const r = parseAuditReport(
      '{"verdict":"ask","integrity":"clean","criteriaResults":[],"summary":"which DB credentials should the migration use?"}',
    )!;
    expect(r.verdict).toBe('ask');
    expect(parseAuditReport('{"verdict":"ask","integrity":"clean","criteriaResults":[],"summary":"  "}')).toBeNull();
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

  it('pauses a task as ask (no retry burn) when the auditor needs human input', async () => {
    const b = board([
      task({ id: 'T1' }),
      task({ id: 'T2', dependsOn: ['T1'] }),
    ]);
    let audits = 0;
    await runScheduler(
      b,
      { repoPath: '.', executor: 'claude-code', concurrency: 1, maxTaskRetries: 3, timeoutMs: 1000 },
      {
        executeTask: async () => ({ ok: true }),
        auditTask: async () => {
          audits += 1;
          return {
            verdict: 'ask',
            integrity: 'clean',
            criteriaResults: [],
            summary: 'need to know which env var holds the API key',
          };
        },
      },
      log,
    );
    expect(audits).toBe(1); // ask is not retried
    const t1 = b.tasks[0]!;
    expect(t1.status).toBe('ask');
    expect(t1.failureDetail).toContain('needs human input');
    expect(t1.attempts).toBeLessThanOrEqual(1);
    // dependent is blocked while upstream waits for the human
    expect(recomputeReadiness(b.tasks).find((t) => t.id === 'T2')!.status).toBe('blocked');
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

describe('recovery contracts (LH manager re-contracting)', () => {
  it('grants one planner re-contract on terminal failure, then succeeds', async () => {
    const b = board([task({ id: 'T1' })]);
    let execs = 0;
    let recoveries = 0;
    await runScheduler(
      b,
      { repoPath: '.', executor: 'claude-code', concurrency: 1, maxTaskRetries: 1, maxRecoveries: 1, timeoutMs: 1000 },
      {
        executeTask: async ({ task: t }) => {
          execs += 1;
          // first contract is doomed; the recovery contract passes
          return { ok: (t.recoveries ?? 0) > 0 };
        },
        planRecovery: async () => {
          recoveries += 1;
          return { prompt: 'targeted fix: export y from src/y.ts', acceptanceCriteria: ['src/y.ts exports y'] };
        },
      },
      log,
    );
    expect(recoveries).toBe(1);
    expect(execs).toBe(2); // original attempt + recovery attempt
    const t1 = b.tasks[0]!;
    expect(t1.status).toBe('done');
    expect(t1.prompt).toContain('targeted fix');
    expect(t1.acceptanceCriteria).toEqual(['src/y.ts exports y']);
  });

  it('stops granting after maxRecoveries and goes terminal failed', async () => {
    const b = board([task({ id: 'T1' })]);
    let recoveries = 0;
    await runScheduler(
      b,
      { repoPath: '.', executor: 'claude-code', concurrency: 1, maxTaskRetries: 1, maxRecoveries: 1, timeoutMs: 1000 },
      {
        executeTask: async () => ({ ok: false, detail: 'tests failed' }),
        planRecovery: async () => {
          recoveries += 1;
          return { prompt: `try harder (${recoveries})` };
        },
      },
      log,
    );
    expect(recoveries).toBe(1); // cap respected
    expect(b.tasks[0]!.status).toBe('failed');
  });

  it('goes straight to failed when no recovery planner is wired', async () => {
    const b = board([task({ id: 'T1' })]);
    await runScheduler(
      b,
      { repoPath: '.', executor: 'claude-code', concurrency: 1, maxTaskRetries: 1, timeoutMs: 1000 },
      { executeTask: async () => ({ ok: false, detail: 'boom' }) },
      log,
    );
    expect(b.tasks[0]!.status).toBe('failed');
  });
});

describe('repeat-gap escalation (SWE-agent L2)', () => {
  const failVerdict = (criterion: string): AuditVerdict => ({
    verdict: 'fail',
    integrity: 'clean',
    criteriaResults: [{ criterion, met: false, evidence: 'grep found nothing' }],
    summary: 'not done',
  });

  it('escalates to recovery after 2 identical primary gaps, before retries exhaust', async () => {
    const b = board([task({ id: 'T1' })]);
    let execs = 0;
    let recoveries = 0;
    await runScheduler(
      b,
      { repoPath: '.', executor: 'claude-code', concurrency: 1, maxTaskRetries: 3, maxRecoveries: 1, timeoutMs: 1000 },
      {
        executeTask: async ({ task: t }) => {
          execs += 1;
          return { ok: true };
        },
        auditTask: async ({ task: t }) =>
          (t.recoveries ?? 0) > 0
            ? { verdict: 'pass', integrity: 'clean', criteriaResults: [{ criterion: 'b holds', met: true, evidence: 'fixed under new contract' }], summary: 'verified' }
            : failVerdict('b holds'), // same primary gap every pre-recovery attempt
        planRecovery: async () => {
          recoveries += 1;
          return { prompt: `rewritten contract ${recoveries}`, acceptanceCriteria: ['b holds'] };
        },
      },
      log,
    );
    // attempt 1 -> gap streak 1 (retry); attempt 2 -> streak 2 triggers early
    // escalation instead of burning retries 3+ against the same wall
    expect(execs).toBe(3); // two doomed attempts + one under the new contract
    expect(recoveries).toBe(1);
    expect(b.tasks[0]!.status).toBe('done');
    expect(b.tasks[0]!.prompt).toContain('rewritten contract');
  });

  it('does not escalate when gaps differ between attempts', async () => {
    const b = board([task({ id: 'T1' })]);
    let recoveries = 0;
    let call = 0;
    await runScheduler(
      b,
      { repoPath: '.', executor: 'claude-code', concurrency: 1, maxTaskRetries: 2, maxRecoveries: 1, timeoutMs: 1000 },
      {
        executeTask: async () => ({ ok: true }),
        auditTask: async ({ task: t }) =>
          (t.recoveries ?? 0) > 0
            ? { verdict: 'pass', integrity: 'clean', criteriaResults: [{ criterion: 'ok', met: true, evidence: 'done' }], summary: 'verified' }
            : failVerdict(`criterion v${++call}`),
        planRecovery: async () => {
          recoveries += 1;
          return { prompt: 'rewrite' };
        },
      },
      log,
    );
    // distinct primary gaps each time: no early escalation, budget runs out
    // first, recovery granted exactly at exhaustion
    expect(recoveries).toBe(1);
    expect(b.tasks[0]!.status).toBe('done');
    expect(call).toBe(2);
  });
});

describe('parseRecoveryContract', () => {
  it('parses valid contracts and rejects malformed ones', () => {
    expect(parseRecoveryContract('```json\n{"prompt":"redo via migration","acceptanceCriteria":["down.sql exists"]}\n```')).toEqual({
      prompt: 'redo via migration',
      acceptanceCriteria: ['down.sql exists'],
    });
    expect(parseRecoveryContract('{"prompt":"   "}')).toBeNull();
    expect(parseRecoveryContract('{"acceptanceCriteria":["x"]}')).toBeNull();
    expect(parseRecoveryContract('{"prompt":"p","acceptanceCriteria":["ok",42]}')).toBeNull();
  });
});

describe('attemptSuffix', () => {
  it('stays legacy-compatible and extends for recoveries', () => {
    expect(attemptSuffix(2)).toBe('a2');
    expect(attemptSuffix(2, 0)).toBe('a2');
    expect(attemptSuffix(0, 1)).toBe('a0r1');
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
