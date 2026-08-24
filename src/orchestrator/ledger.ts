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

/**
 * Outcome aggregation (LangSmith lesson: raw traces earn their keep only
 * through pass/fail comparison). Attempts-to-pass measures how often the
 * evidence gate rejects first work — the auditor's real catch rate.
 */
export interface LedgerSummary {
  tasks: number;
  audits: number;
  /** Tasks with at least one clean-pass audit */
  resolved: number;
  /** Mean attempt index of the first passing audit, across resolved tasks */
  meanAttemptsToPass: number | null;
  /** Tasks whose latest audit is fail/ask — still open work */
  unresolved: number;
}

export function summarizeLedger(repoPath: string): LedgerSummary {
  const byTask = new Map<string, AuditLedgerRecord[]>();
  for (const r of readLedger(repoPath)) {
    const list = byTask.get(r.taskId) ?? [];
    list.push(r);
    byTask.set(r.taskId, list);
  }
  let audits = 0;
  let resolved = 0;
  let attemptsSum = 0;
  for (const records of byTask.values()) {
    audits += records.length;
    const firstPass = records.find((r) => r.verdict === 'pass' && r.integrity === 'clean');
    if (firstPass) {
      resolved += 1;
      attemptsSum += firstPass.attempt;
    }
  }
  return {
    tasks: byTask.size,
    audits,
    resolved,
    meanAttemptsToPass: resolved > 0 ? Math.round((attemptsSum / resolved) * 100) / 100 : null,
    unresolved: byTask.size - resolved,
  };
}

/** One recurring gap category across failed audits. */
export interface FailureCluster {
  /** First-seen original wording; grouping itself is normalized. */
  criterion: string;
  /** Failed audits citing this criterion. */
  occurrences: number;
  /** Distinct tasks whose failed audits cite it, in first-seen order. */
  tasks: string[];
  /** Tasks in `tasks` with no passing audit anywhere in the ledger. */
  openTasks: number;
}

/**
 * Failure-cluster reporting (Phase 4): recurring unmet acceptance criteria
 * should surface as an actionable ranked view, not just queryable rows.
 * Grouping normalizes case and whitespace so trivial rewording does not
 * fragment a cluster; semantic variants stay separate by design.
 */
export function clusterFailures(repoPath: string): FailureCluster[] {
  const records = readLedger(repoPath);
  const passedTasks = new Set(
    records.filter((r) => r.verdict === 'pass' && r.integrity === 'clean').map((r) => r.taskId),
  );
  const clusters = new Map<string, FailureCluster & { _key: string }>();
  for (const r of records) {
    if (r.verdict === 'pass') continue;
    for (const criterion of r.unmetCriteria) {
      const key = criterion.toLowerCase().replace(/\s+/g, ' ').trim();
      let c = clusters.get(key);
      if (!c) {
        c = { _key: key, criterion, occurrences: 0, tasks: [], openTasks: 0 };
        clusters.set(key, c);
      }
      c.occurrences += 1;
      if (!c.tasks.includes(r.taskId)) c.tasks.push(r.taskId);
    }
  }
  return [...clusters.values()]
    .map(({ _key, ...c }) => ({ ...c, openTasks: c.tasks.filter((t) => !passedTasks.has(t)).length }))
    .sort((a, b) => b.occurrences - a.occurrences || b.tasks.length - a.tasks.length);
}
