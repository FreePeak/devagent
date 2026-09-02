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

/** CI-Fixer lifecycle event (Q35, Phase 4). */
export interface FixerLedgerRecord extends LedgerRecordBase {
  kind: 'event';
  event: 'ci-fix-dispatched' | 'ci-fix-outcome';
  /** PR number the fixer was dispatched against. */
  pr: number;
  /** Failed check names at dispatch (identify the round-trip with taskId). */
  failedChecks: string[];
  /** Terminal outcome; set only on 'ci-fix-outcome' rows. */
  outcome?: 'failed-then-green' | 'still-red' | 'ci-fix-failed';
  /** Human detail (dispatch note / check summary); best-effort. */
  detail?: string;
}

/**
 * Append a fixer lifecycle record. Never throws into the caller's path —
 * best-effort observability by design.
 */
export function appendFixerRecord(repoPath: string, record: FixerLedgerRecord): void {
  try {
    const file = ledgerPath(repoPath);
    mkdirSync(join(repoPath, LEDGER_DIR), { recursive: true });
    appendFileSync(file, `${JSON.stringify(record)}\n`);
  } catch {
    // best-effort observability only
  }
}

/** Zombie-PR hygiene event (PRD §17). */
export interface PrHygieneLedgerRecord extends LedgerRecordBase {
  kind: 'event';
  event: 'pr-hygiene';
  /** PR number the hygiene action targeted. */
  pr: number;
  /** Action taken; skips are not recorded (no action happened). */
  action: 'closed' | 'flagged';
  /** Why the action fired: base-superseded | red-across-grace. */
  reason: string;
  /** Hours since the PR's last update (grace-window age); null when unknown. */
  graceAgeHours: number | null;
  /** Human detail; best-effort. */
  detail?: string;
}
/**
 * Append a PR-hygiene record. Never throws into the caller's path —
 * best-effort observability by design.
 */
export function appendPrHygieneRecord(repoPath: string, record: PrHygieneLedgerRecord): void {
  try {
    const file = ledgerPath(repoPath);
    mkdirSync(join(repoPath, LEDGER_DIR), { recursive: true });
    appendFileSync(file, `${JSON.stringify(record)}\n`);
  } catch {
    // best-effort observability only
  }
}
/**
 * Executor interrupt post-mortem (PRD:775 / Q24 taxonomy mirror, PR #100): a
 * structured failure record written when a task is aborted via `taskInterrupt`
 * — N+ identical trailing trail.jsonl failure signatures. Carries the compact
 * post-mortem (goal, failure class, last gate excerpt, attempts, trail hash)
 * so the next bridge can plan around the same failure mode instead of mining
 * raw trails by hand (loop-57/58 diagnostic gap).
 */
export interface TaskInterruptLedgerRecord extends LedgerRecordBase {
  kind: 'event';
  event: 'taskInterrupt';
  /** Board goal the interrupted task belonged to (Q22 archive context). */
  goal: string;
  /** Executor failure class (Q24 taxonomy mirror). */
  failureClass: string;
  /** Last gate excerpt that repeated across attempts (bounded). */
  lastGateExcerpt: string;
  /** Attempts spent before the interrupt fired. */
  attempts: number;
  /** Hash of the identical trailing trail.jsonl failure signatures. */
  trailHash: string;
  /** Human detail; best-effort. */
  detail?: string;
}

/**
 * Append an executor-interrupt post-mortem. Never throws into the caller's
 * path — best-effort observability by design.
 */
export function appendTaskInterruptRecord(repoPath: string, record: TaskInterruptLedgerRecord): void {
  try {
    const file = ledgerPath(repoPath);
    mkdirSync(join(repoPath, LEDGER_DIR), { recursive: true });
    appendFileSync(file, `${JSON.stringify(record)}\n`);
  } catch {
    // best-effort observability only
  }
}

/** Operator-role provider preflight (PRD §17 Phase 4, Q40). */
export interface OperatorDegradedLedgerRecord extends LedgerRecordBase {
  kind: 'event';
  event: 'operator-degraded';
  /** Operator loop role the preflight gated: prd-curator | po | selfbuild | warroom | reviewer. */
  role: string;
  /** Worker CLI the probe exercised (from repo config; empty = unknown). */
  worker: string;
  /** Model id the probe passed (empty = CLI default). */
  model: string;
  /** Probe outcome. Rows are written on probe failure; ok rows only when explicitly recorded. */
  ok: boolean;
  /** Probe attempts made (1..3). */
  attempts: number;
  /** Bounded last-failure excerpt from the probe CLI. */
  detail?: string;
}

/**
 * Append an operator-degraded record. Never throws into the caller's path —
 * best-effort observability by design.
 */
export function appendOperatorDegradedRecord(repoPath: string, record: OperatorDegradedLedgerRecord): void {
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
