/**
 * Shared §20.8 card/chip human defaults for queue / validate / ledger
 * (FR-SIMPLE-03 remainder, issue #144). Reuses chipFor / boxLines / dim /
 * cyan from src/tui/tui.ts — same language as renderStatusCard.
 */
import type { AuditLedgerRecord, LedgerSummary } from '../orchestrator/ledger.js';
import type { QueuedTask, QueuedTaskStatus } from '../queue.js';
import type { GateResult } from '../types.js';
import { boxLines, chipFor, cyan, dim, truncate } from '../tui/tui.js';

export function cardWidth(): number {
  return Math.max(46, Math.min(process.stdout?.columns ?? 100, 100));
}

function chipStateForQueue(status: QueuedTaskStatus): string {
  switch (status) {
    case 'done':
      return 'ok';
    case 'failed':
      return 'failed';
    case 'claimed':
      return 'running';
    default:
      return 'idle';
  }
}

function queueNextAction(counts: Record<QueuedTaskStatus | 'total', number>): string {
  if ((counts.pending ?? 0) > 0) return 'devagent consume --auto-pr';
  if ((counts.claimed ?? 0) > 0) return 'devagent status';
  if ((counts.failed ?? 0) > 0) return 'inspect failed tasks: devagent queue list --status failed';
  if ((counts.total ?? 0) === 0) return 'devagent status';
  return 'devagent status';
}

const QUEUE_ROW_CAP = 8;

/** Human card for `devagent queue list` (default, non-`--json`). */
export function renderQueueCard(
  tasks: QueuedTask[],
  counts: Record<QueuedTaskStatus | 'total', number>,
): string[] {
  const width = cardWidth();
  const countLine = ` ${chipFor('idle', 'queue')}  ${dim(
    `pending ${counts.pending ?? 0} · claimed ${counts.claimed ?? 0} · done ${counts.done ?? 0} · failed ${counts.failed ?? 0}`,
  )}`;
  const rows = tasks.slice(0, QUEUE_ROW_CAP).map((t) => {
    const err = t.lastError ? ` — ${truncate(t.lastError, 40)}` : '';
    return ` ${chipFor(chipStateForQueue(t.status), t.status)}  ${t.id}  ${truncate(t.title, 40)}${err}`;
  });
  if (tasks.length === 0) {
    rows.push(` ${dim('No tasks in the queue.')}`);
  } else if (tasks.length > QUEUE_ROW_CAP) {
    rows.push(` ${dim(`… and ${tasks.length - QUEUE_ROW_CAP} more`)}`);
  }
  const next = ` next: ${cyan(queueNextAction(counts))}`;
  return boxLines('Queue', [countLine, ...rows, next], width);
}

export interface ValidateGateRow {
  /** Short label shown on the chip (G1 / G3). */
  label: string;
  result: GateResult;
}

function gateChipState(r: GateResult): string {
  if (r.skipped) return 'idle';
  return r.passed ? 'ok' : 'failed';
}

function gateChipLabel(row: ValidateGateRow): string {
  if (row.result.skipped) return `${row.label} SKIP`;
  return row.result.passed ? `${row.label} PASS` : `${row.label} FAIL`;
}

function oneLineDetail(detail: string | undefined): string {
  if (!detail) return '';
  const first = detail.split('\n').find((l) => l.trim()) ?? '';
  return truncate(first.trim(), 70);
}

function validateNextAction(rows: ValidateGateRow[]): string {
  const failed = rows.find((r) => !r.result.passed && !r.result.skipped);
  if (failed) return 'fix tests / inspect gate detail above';
  return 'devagent status';
}

/** Human cards for `devagent validate` (default, non-`--json`). */
export function renderValidateCards(rows: ValidateGateRow[]): string[] {
  const width = cardWidth();
  const out: string[] = [];
  for (const row of rows) {
    const detail =
      oneLineDetail(row.result.detail) ||
      (row.result.skipped ? 'skipped' : row.result.passed ? 'passed' : 'failed');
    const body = [` ${chipFor(gateChipState(row.result), gateChipLabel(row))}  ${dim(detail)}`];
    out.push(...boxLines(`Gate ${row.label}`, body, width));
  }
  out.push(` next: ${cyan(validateNextAction(rows))}`);
  return out;
}

/** Machine payload for `devagent validate --json`. */
export function validateJson(rows: ValidateGateRow[]): string {
  const gates = rows.map((r) => ({
    gate: r.result.gate,
    label: r.label,
    passed: r.result.passed,
    skipped: r.result.skipped ?? false,
    detail: r.result.detail ?? null,
    findings: r.result.findings,
  }));
  const ok = rows.every((r) => r.result.passed || r.result.skipped);
  return JSON.stringify({ gates, ok }, null, 2);
}

function ledgerNextAction(sum: LedgerSummary): string {
  if (sum.unresolved > 0) return 'inspect open work: devagent ledger';
  if (sum.audits === 0) return 'devagent status';
  return 'devagent status';
}

/** Summary card for `devagent ledger --summary`. */
export function renderLedgerSummaryCard(sum: LedgerSummary): string[] {
  const width = cardWidth();
  const mean =
    sum.meanAttemptsToPass !== null ? ` · mean attempts-to-pass ${sum.meanAttemptsToPass}` : '';
  const body = [
    ` ${chipFor('idle', 'audits')}  ${dim(`${sum.audits}`)}  ${chipFor('ok', 'resolved')}  ${dim(`${sum.resolved}`)}  ${chipFor(sum.unresolved > 0 ? 'failed' : 'ok', 'unresolved')}  ${dim(`${sum.unresolved}`)}`,
    ` ${dim(`tasks ${sum.tasks}${mean}`)}`,
    ` next: ${cyan(ledgerNextAction(sum))}`,
  ];
  return boxLines('Ledger summary', body, width);
}

/** Chip lines for default `devagent ledger` list (replaces +/x/? ascii). */
export function renderLedgerListLines(records: AuditLedgerRecord[]): string[] {
  if (records.length === 0) {
    return ['No ledger records. Audits append to .devagent/runs/orchestration/events.jsonl.'];
  }
  const lines: string[] = [];
  for (const r of records) {
    if (r.kind !== 'audit') continue;
    const state = r.verdict === 'pass' ? 'ok' : r.verdict === 'ask' ? 'idle' : 'failed';
    const label = r.verdict === 'pass' ? 'pass' : r.verdict === 'ask' ? 'ask' : 'fail';
    const unmet = r.unmetCriteria.length ? ` unmet:${r.unmetCriteria.length}` : '';
    const detail = `${r.verdict}/${r.integrity}${unmet} — ${truncate(r.summary, 90)}`;
    lines.push(`${chipFor(state, label)}  ${r.ts} [${r.kind}] ${r.taskId} (attempt ${r.attempt}) ${detail}`);
  }
  if (lines.length === 0) {
    return ['No audit records in the ledger.'];
  }
  return lines;
}

/** `--json` for ledger summary or list. */
export function ledgerJson(payload: LedgerSummary | AuditLedgerRecord[]): string {
  return JSON.stringify(payload, null, 2);
}
