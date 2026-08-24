import { afterEach, describe, expect, it } from 'vitest';
import { appendFileSync, existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { LEDGER_DIR, appendAuditRecord, auditLedgerRecord, ledgerTailFor, readLedger } from '../src/orchestrator/ledger.js';
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
