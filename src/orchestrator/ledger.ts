import { appendFileSync, existsSync, mkdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { AuditVerdict } from './types.js';

/**
 * Run ledger (ATSC/OTel-GenAI lesson: sparse-but-accurate records with an
 * identity block beat complete-but-dummy schemas). Audit verdicts are
 * appended as JSONL under <repo>/.devagent/runs/orchestration/events.jsonl so
 * history survives worktree cleanup and board resets — the board is the live
 * view; the ledger is replayable evidence.
 */

export const LEDGER_DIR = '.devagent/runs/orchestration';

/** Identity block shared by every record (LangSmith coding-agent contract). */
export interface LedgerRecordBase {
  /** ISO timestamp of the event */
  ts: string;
  kind: 'audit' | 'event';
  taskId: string;
  attempt: number;
}

export interface AuditLedgerRecord extends LedgerRecordBase {
  kind: 'audit';
  verdict: AuditVerdict['verdict'];
  integrity: AuditVerdict['integrity'];
  unmetCriteria: string[];
  summary: string;
}

export function auditLedgerRecord(args: {
  taskId: string;
  attempt: number;
  verdict: AuditVerdict;
  ts?: string;
}): AuditLedgerRecord {
  return {
    ts: args.ts ?? new Date().toISOString(),
    kind: 'audit',
    taskId: args.taskId,
    attempt: args.attempt,
    verdict: args.verdict.verdict,
    integrity: args.verdict.integrity,
    unmetCriteria: args.verdict.criteriaResults.filter((c) => !c.met).map((c) => c.criterion),
    summary: args.verdict.summary.slice(0, 500),
  };
}

function ledgerPath(repoPath: string): string {
  return join(repoPath, LEDGER_DIR, 'events.jsonl');
}

/**
 * Append one record. Never throws into the caller's path — a ledger write
 * failure must not fail an otherwise valid audit (best-effort by design).
 */
export function appendAuditRecord(repoPath: string, record: AuditLedgerRecord): void {
  try {
    const file = ledgerPath(repoPath);
    mkdirSync(join(repoPath, LEDGER_DIR), { recursive: true });
    appendFileSync(file, `${JSON.stringify(record)}\n`);
  } catch {
    // best-effort observability only
  }
}

/** Read audit records, oldest first; optional task filter. Returns [] when absent. */
export function readLedger(repoPath: string, opts: { taskId?: string } = {}): AuditLedgerRecord[] {
  const file = ledgerPath(repoPath);
  if (!existsSync(file)) return [];
  const out: AuditLedgerRecord[] = [];
  for (const line of readFileSync(file, 'utf8').split('\n')) {
    if (!line.trim()) continue;
    try {
      const r = JSON.parse(line) as AuditLedgerRecord;
      if (r.kind !== 'audit') continue;
      if (!opts.taskId || r.taskId === opts.taskId) out.push(r);
    } catch {
      // skip corrupt lines; a ledger is data, not truth
    }
  }
  return out;
}

/**
 * Compact evidence history for one task, newest first (SWE-agent lesson:
 * informative-but-concise feedback — operators see verdict trends without
 * reading raw JSONL).
 */
export function ledgerTailFor(
  repoPath: string,
  taskId: string,
  n = 3,
): Array<{ ts: string; attempt: number; verdict: string; integrity: string }> {
  return readLedger(repoPath, { taskId })
    .slice(-n)
    .reverse()
    .map((r) => ({ ts: r.ts, attempt: r.attempt, verdict: r.verdict, integrity: r.integrity }));
}
