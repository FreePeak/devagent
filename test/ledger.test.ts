import { afterEach, describe, expect, it } from 'vitest';
import { appendFileSync, existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { LEDGER_DIR, appendAuditRecord, appendReleaseRecord, appendTaskInterruptRecord, auditLedgerRecord, clusterFailures, ledgerTailFor, readLedger, summarizeLedger } from '../src/orchestrator/ledger.js';
import type { AuditVerdict } from '../src/orchestrator/types.js';

const pass: AuditVerdict = {
  verdict: 'pass',
  integrity: 'clean',
  criteriaResults: [{ criterion: 'tests green', met: true, evidence: 'npm test: 3 passed' }],
  summary: 'verified',
};
const failWithUnmet = (criterion: string): AuditVerdict => ({
  verdict: 'fail',
  integrity: 'clean',
  criteriaResults: [{ criterion, met: false, evidence: 'grep found nothing' }],
  summary: 'not done',
});

describe('run ledger', () => {
  const dirs: string[] = [];
  const tempRepo = () => {
    const d = mkdtempSync(join(tmpdir(), 'da-ledger-'));
    dirs.push(d);
    return d;
  };
  afterEach(() => {
    for (const d of dirs) rmSync(d, { recursive: true, force: true });
    dirs.length = 0;
  });

  it('appends JSONL records with identity blocks and reads them back', () => {
    const repo = tempRepo();
    appendAuditRecord(repo, auditLedgerRecord({ taskId: 'T1', attempt: 1, verdict: failWithUnmet('b holds'), ts: '2026-08-24T02:00:00Z' }));
    appendAuditRecord(repo, auditLedgerRecord({ taskId: 'T1', attempt: 2, verdict: pass, ts: '2026-08-24T02:05:00Z' }));
    const all = readLedger(repo);
    expect(all).toHaveLength(2);
    expect(all[1]).toMatchObject({ kind: 'audit', taskId: 'T1', attempt: 2, verdict: 'pass' });

    const raw = readFileSync(join(repo, LEDGER_DIR, 'events.jsonl'), 'utf8').trim().split('\n');
    expect(raw).toHaveLength(2);
    expect(() => JSON.parse(raw[0]!)).not.toThrow();
  });

  it('records unmet criteria for failed audits and filters by task', () => {
    const repo = tempRepo();
    appendAuditRecord(repo, auditLedgerRecord({ taskId: 'T1', attempt: 1, verdict: failWithUnmet('schema exists') }));
    appendAuditRecord(repo, auditLedgerRecord({ taskId: 'T2', attempt: 1, verdict: pass }));
    const t1 = readLedger(repo, { taskId: 'T1' }) as ReturnType<typeof auditLedgerRecord>[];
    expect(t1).toHaveLength(1);
    expect(t1[0]!.unmetCriteria).toEqual(['schema exists']);
    expect(t1[0]!.summary.length).toBeLessThanOrEqual(500);
  });

  it('tolerates corrupt lines and missing ledger files', () => {
    const repo = tempRepo();
    expect(readLedger(repo)).toEqual([]);
    appendAuditRecord(repo, auditLedgerRecord({ taskId: 'T1', attempt: 1, verdict: pass }));
    // hand-corrupt the file
    const file = join(repo, LEDGER_DIR, 'events.jsonl');
    appendFileSync(file, '{broken json\n');
    const all = readLedger(repo);
    expect(all).toHaveLength(1); // corrupt line skipped, good record kept
    expect(existsSync(file)).toBe(true);
  });
});

describe('ledgerTailFor (project evidence history)', () => {
  const dirs: string[] = [];
  afterEach(() => {
    for (const d of dirs) rmSync(d, { recursive: true, force: true });
    dirs.length = 0;
  });

  it('returns newest-first compact history capped at n', () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-tail-'));
    dirs.push(repo);
    appendAuditRecord(repo, auditLedgerRecord({ taskId: 'T1', attempt: 1, verdict: failWithUnmet('x'), ts: '2026-08-24T01:00:00Z' }));
    appendAuditRecord(repo, auditLedgerRecord({ taskId: 'T1', attempt: 2, verdict: failWithUnmet('y'), ts: '2026-08-24T02:00:00Z' }));
    appendAuditRecord(repo, auditLedgerRecord({ taskId: 'T1', attempt: 3, verdict: pass, ts: '2026-08-24T03:00:00Z' }));
    const tail = ledgerTailFor(repo, 'T1');
    expect(tail.map((r) => r.attempt)).toEqual([3, 2, 1]);
    expect(tail[0]).toMatchObject({ verdict: 'pass', integrity: 'clean' });
    expect(ledgerTailFor(repo, 'T1', 2)).toHaveLength(2);
    expect(ledgerTailFor(repo, 'TX')).toEqual([]);
  });

  it('single passing audit yields no history line material', () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-tail-pass-'));
    dirs.push(repo);
    appendAuditRecord(repo, auditLedgerRecord({ taskId: 'T1', attempt: 1, verdict: pass }));
    // caller suppresses display for a lone clean pass; helper still returns it
    expect(ledgerTailFor(repo, 'T1')).toHaveLength(1);
  });
});

describe('summarizeLedger (outcome analytics)', () => {
  const dirs: string[] = [];
  afterEach(() => {
    for (const d of dirs) rmSync(d, { recursive: true, force: true });
    dirs.length = 0;
  });

  it('computes resolved rate and mean attempts-to-pass across tasks', () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-sum-'));
    dirs.push(repo);
    // T1: fail then pass on attempt 2; T2: pass first try; T3: never passes
    appendAuditRecord(repo, auditLedgerRecord({ taskId: 'T1', attempt: 1, verdict: failWithUnmet('x') }));
    appendAuditRecord(repo, auditLedgerRecord({ taskId: 'T1', attempt: 2, verdict: pass }));
    appendAuditRecord(repo, auditLedgerRecord({ taskId: 'T2', attempt: 1, verdict: pass }));
    appendAuditRecord(repo, auditLedgerRecord({ taskId: 'T3', attempt: 1, verdict: failWithUnmet('y') }));
    const sum = summarizeLedger(repo);
    expect(sum).toEqual({ tasks: 3, audits: 4, resolved: 2, meanAttemptsToPass: 1.5, unresolved: 1 });
  });

  it('reports zero state for an empty ledger without dividing by zero', () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-sum-empty-'));
    dirs.push(repo);
    expect(summarizeLedger(repo)).toEqual({
      tasks: 0,
      audits: 0,
      resolved: 0,
      meanAttemptsToPass: null,
      unresolved: 0,
    });
  });
});

describe('failure clusters', () => {
  const dirs: string[] = [];
  const tempRepo = () => {
    const d = mkdtempSync(join(tmpdir(), 'da-cluster-'));
    dirs.push(d);
    return d;
  };
  afterEach(() => {
    for (const d of dirs.splice(0)) rmSync(d, { recursive: true, force: true });
  });

  it('groups unmet criteria across tasks, counts open tasks, ranks by frequency', () => {
    const repo = tempRepo();
    // "tests green" hits T1 (later passes) and T2 (never passes), twice total
    appendAuditRecord(repo, auditLedgerRecord({ taskId: 'T1', attempt: 1, verdict: failWithUnmet('Tests Green') }));
    appendAuditRecord(repo, auditLedgerRecord({ taskId: 'T2', attempt: 1, verdict: failWithUnmet('tests   green') }));
    appendAuditRecord(repo, auditLedgerRecord({ taskId: 'T1', attempt: 2, verdict: pass }));
    appendAuditRecord(repo, auditLedgerRecord({ taskId: 'T3', attempt: 1, verdict: failWithUnmet('schema exists') }));
    const clusters = clusterFailures(repo);
    expect(clusters).toHaveLength(2);
    expect(clusters[0]).toEqual({
      criterion: 'Tests Green',
      occurrences: 2,
      tasks: ['T1', 'T2'],
      openTasks: 1,
    });
    expect(clusters[1]).toMatchObject({ criterion: 'schema exists', occurrences: 1, openTasks: 1 });
  });

  it('ignores passing audits without unmet criteria and returns [] for an empty ledger', () => {
    const repo = tempRepo();
    appendAuditRecord(repo, auditLedgerRecord({ taskId: 'T1', attempt: 1, verdict: pass }));
    expect(clusterFailures(repo)).toEqual([]);
    const empty = tempRepo();
    expect(clusterFailures(empty)).toEqual([]);
  });

  it('keeps distinct criteria with different wording in separate clusters', () => {
    const repo = tempRepo();
    appendAuditRecord(repo, auditLedgerRecord({ taskId: 'T1', attempt: 1, verdict: failWithUnmet('branch pushed') }));
    appendAuditRecord(repo, auditLedgerRecord({ taskId: 'T2', attempt: 1, verdict: failWithUnmet('pushed branch') }));
    const clusters = clusterFailures(repo);
    expect(clusters.map((c) => c.criterion).sort()).toEqual(['branch pushed', 'pushed branch']);
  });
});

describe('taskInterrupt post-mortem (executor failure surface, PRD:775)', () => {
  const dirs: string[] = [];
  const tempRepo = () => {
    const d = mkdtempSync(join(tmpdir(), 'da-ledger-interrupt-'));
    dirs.push(d);
    return d;
  };
  afterEach(() => {
    for (const d of dirs.splice(0)) rmSync(d, { recursive: true, force: true });
  });

  it('appends a taskInterrupt row with post-mortem payload (goal, failure class, gate excerpt, attempts, trail hash)', () => {
    const repo = tempRepo();
    appendTaskInterruptRecord(repo, {
      ts: '2026-09-01T08:00:00Z',
      kind: 'event',
      event: 'taskInterrupt',
      taskId: 'T1',
      attempt: 3,
      goal: 'ship release gate',
      failureClass: 'test-gate',
      lastGateExcerpt: 'npm test: 3 failed (same every time)',
      attempts: 3,
      trailHash: 'abc123def456',
    });
    const raw = readFileSync(join(repo, LEDGER_DIR, 'events.jsonl'), 'utf8').trim().split('\n');
    expect(raw).toHaveLength(1);
    const row = JSON.parse(raw[0]!);
    expect(row).toMatchObject({
      kind: 'event',
      event: 'taskInterrupt',
      taskId: 'T1',
      goal: 'ship release gate',
      failureClass: 'test-gate',
      lastGateExcerpt: 'npm test: 3 failed (same every time)',
      attempts: 3,
      trailHash: 'abc123def456',
    });
    // Verify the full JSONL shape (all fields, including ts and attempt)
    expect(row.ts).toBe('2026-09-01T08:00:00Z');
    expect(row.attempt).toBe(3);
  });

  it('tolerates missing ledger directory (best-effort write)', () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-ledger-interrupt-noop-'));
    dirs.push(repo);
    // Should not throw even when the ledger dir does not exist — the
    // append creates it unconditionally.
    expect(() =>
      appendTaskInterruptRecord(repo, {
        ts: '2026-09-01T09:00:00Z',
        kind: 'event',
        event: 'taskInterrupt',
        taskId: 'T2',
        attempt: 2,
        goal: 'fix build',
        failureClass: 'worker-error',
        lastGateExcerpt: 'worker crashed',
        attempts: 2,
        trailHash: 'xyz',
      }),
    ).not.toThrow();
    const raw = readFileSync(join(repo, LEDGER_DIR, 'events.jsonl'), 'utf8').trim().split('\n');
    expect(raw).toHaveLength(1);
  });
});

describe('release-created ledger record (Q24 release/tag outcome)', () => {
  const dirs: string[] = [];
  const tempRepo = () => {
    const d = mkdtempSync(join(tmpdir(), 'da-ledger-release-'));
    dirs.push(d);
    return d;
  };
  afterEach(() => {
    for (const d of dirs.splice(0)) rmSync(d, { recursive: true, force: true });
  });

  it('appends a release-created row with the full shape (tag, sha, version, source) and reads it back', () => {
    const repo = tempRepo();
    appendReleaseRecord(repo, {
      ts: '2026-09-03T10:00:00Z',
      kind: 'event',
      event: 'release-created',
      taskId: 'release/0.1.0',
      attempt: 1,
      tag: 'v0.1.0',
      sha: 'abc123def456',
      version: '0.1.0',
      source: 'release.yml',
    });
    const raw = readFileSync(join(repo, LEDGER_DIR, 'events.jsonl'), 'utf8').trim().split('\n');
    expect(raw).toHaveLength(1);
    const row = JSON.parse(raw[0]!);
    expect(row).toMatchObject({
      kind: 'event',
      event: 'release-created',
      taskId: 'release/0.1.0',
      attempt: 1,
      tag: 'v0.1.0',
      sha: 'abc123def456',
      version: '0.1.0',
      source: 'release.yml',
    });
    // Verify the full JSONL shape (all fields, including ts)
    expect(row.ts).toBe('2026-09-03T10:00:00Z');
  });

  it('tolerates missing ledger directory (best-effort write)', () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-ledger-release-noop-'));
    dirs.push(repo);
    expect(() =>
      appendReleaseRecord(repo, {
        ts: '2026-09-03T11:00:00Z',
        kind: 'event',
        event: 'release-created',
        taskId: 'release/0.2.0',
        attempt: 1,
        tag: 'v0.2.0',
        sha: 'deadbeef',
        version: '0.2.0',
        source: 'cli',
      }),
    ).not.toThrow();
    const raw = readFileSync(join(repo, LEDGER_DIR, 'events.jsonl'), 'utf8').trim().split('\n');
    expect(raw).toHaveLength(1);
  });
});
