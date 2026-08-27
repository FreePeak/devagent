import { dirname, join } from 'node:path';
import type { AuditVerdict, OrchestratorTask, ProjectBoard } from './types.js';
import type { WorkerName } from '../types.js';
import { spawnCli } from '../workers/spawn-utils.js';
import { appendAuditRecord, auditLedgerRecord } from './ledger.js';

/**
 * Auditor role (LongHorizon-Harness lesson): executor self-reports never
 * become trusted state. After an executor finishes, an independent read-only
 * auditor checks each acceptance criterion against environmental evidence
 * (files, tests, command output) and returns a structured verdict. A task
 * flips to 'done' only on verdict=pass AND integrity=clean; any workspace
 * mutation observed during the audit voids the report.
 *
 * The auditor sees the task contract and the executor's summary — never its
 * transcript or reasoning — so it must re-derive completion from the
 * environment itself.
 */

const AUDITOR_SYSTEM_PROMPT = `You are an independent software auditor. You verify whether a coding task's acceptance criteria are actually met by inspecting the repository yourself. You are read-only: run only non-mutating commands (cat, ls, grep, find, git log/diff/status/show, test runners). Do NOT create, edit, delete, move, or format any file. Do NOT run install, build-with-side-effects, git add/commit/push/checkout, or anything that changes state.

For each acceptance criterion, collect concrete evidence from the environment (command output snippets, file excerpts, test results) and judge ONLY that evidence — do not trust the executor's claims.

Respond with ONLY a JSON object (no prose, no markdown fences):
{"verdict":"pass|fail|ask","integrity":"clean|suspect|violation","criteriaResults":[{"criterion":"...","met":true,"evidence":"command + output excerpt proving it"}],"summary":"one paragraph on what you inspected and how"}
Rules:
- verdict is "pass" only if EVERY criterion is met with real evidence.
- verdict is "ask" when completion cannot be judged without a human decision (missing credentials, ambiguous requirement needing the task owner, action you are not authorized to take). Put the precise question in "summary".
- integrity is "clean" unless you observed signs the workspace was mutated improperly during your inspection (set "violation" if you did, and explain in summary).
- criteriaResults must contain one entry per acceptance criterion (may be empty for "ask").`;

export interface AuditorInput {
  goal: string;
  task: OrchestratorTask;
  /** Executor's final report text (its claim — data to check, not evidence) */
  executorDetail?: string;
}

export function buildAuditPrompt(input: AuditorInput): string {
  const { goal, task, executorDetail } = input;
  const criteria =
    task.acceptanceCriteria?.length ? task.acceptanceCriteria : task.expectedOutput ? [task.expectedOutput] : [];
  return [
    AUDITOR_SYSTEM_PROMPT,
    '',
    '## Project goal',
    goal,
    '',
    `## Task ${task.id}: ${task.title}`,
    task.prompt,
    '',
    '## Acceptance criteria to verify',
    criteria.length ? criteria.map((c, i) => `${i + 1}. ${c}`).join('\n') : '(none listed — derive them from the task description and list what you checked)',
    task.boundaryConstraints?.length ? `\n## Boundary constraints the executor had to respect\n${task.boundaryConstraints.map((c) => `- ${c}`).join('\n')}` : '',
    executorDetail ? `\n## Executor claim (untrusted — verify, do not assume)\n${executorDetail.slice(0, 2000)}` : '',
  ]
    .filter(Boolean)
    .join('\n');
}

/**
 * Parse and validate an auditor's JSON report field-by-field. The report is
 * untrusted data: malformed shapes yield null so the caller can treat the
 * audit as inconclusive rather than trusting a partial verdict.
 */
export function parseAuditReport(text: string): AuditVerdict | null {
  const start = text.indexOf('{');
  const end = text.lastIndexOf('}');
  if (start === -1 || end <= start) return null;
  let raw: unknown;
  try {
    raw = JSON.parse(text.slice(start, end + 1));
  } catch {
    return null;
  }
  const o = raw as Record<string, unknown>;
  if (o.verdict !== 'pass' && o.verdict !== 'fail' && o.verdict !== 'ask') return null;
  if (o.integrity !== 'clean' && o.integrity !== 'suspect' && o.integrity !== 'violation') return null;
  // ask verdicts may skip criteria; pass/fail must carry at least one
  if (!Array.isArray(o.criteriaResults)) return null;
  if (o.verdict !== 'ask' && o.criteriaResults.length === 0) return null;
  const criteriaResults = [];
  for (const r of o.criteriaResults) {
    const c = r as Record<string, unknown>;
    if (typeof c.criterion !== 'string' || typeof c.met !== 'boolean' || typeof c.evidence !== 'string') return null;
    criteriaResults.push({ criterion: c.criterion, met: c.met, evidence: c.evidence });
  }
  if (typeof o.summary !== 'string' || !o.summary.trim()) return null;
  // LH-Harness rule: pass requires every criterion met AND clean integrity.
  // A self-contradictory "pass" with unmet criteria coerces to fail.
  const allMet = criteriaResults.every((c) => c.met);
  return {
    verdict: o.verdict === 'pass' ? (allMet ? 'pass' : 'fail') : o.verdict,
    integrity: o.integrity,
    criteriaResults,
    summary: o.summary,
  };
}

/** Workspace mutation snapshot used to enforce auditor read-only discipline. */
async function dirtyFiles(cwd: string): Promise<Set<string>> {
  const r = await spawnCli('git', ['status', '--porcelain'], { cwd, timeoutMs: 30_000 });
  if (r.exitCode !== 0) return new Set(['<status-failed>']);
  return new Set(r.stdout.split('\n').map((s) => s.trim()).filter(Boolean));
}

export async function runAudit(args: {
  board: ProjectBoard;
  task: OrchestratorTask;
  worktreePath: string;
  timeoutMs: number;
  auditor: WorkerName;
}): Promise<AuditVerdict | null> {
  const { getWorker } = await import('../workers/index.js');
  const worker = getWorker(args.auditor);
  const before = await dirtyFiles(args.worktreePath);
  const cfg = (await import('../config.js')).loadConfig(args.worktreePath);
  const result = await worker.spawn({
    prompt: buildAuditPrompt({ goal: args.board.goal, task: args.task }),
    cwd: args.worktreePath,
    timeoutMs: args.timeoutMs,
    ...(cfg.model ? { model: cfg.model } : {}),
    ...(cfg.variant ? { variant: cfg.variant } : {}),
  });
  const after = await dirtyFiles(args.worktreePath);
  let report = result.timedOut || result.exitCode !== 0 ? null : parseAuditReport(result.resultText ?? '');
  // Harness-enforced integrity (LH lesson): any workspace change while the
  // auditor ran voids the report regardless of what it claims.
  const mutated = [...after].filter((f) => !before.has(f));
  if (mutated.length > 0) {
    report = report
      ? { ...report, integrity: 'violation', summary: `${report.summary}\n[harness] workspace mutated during audit: ${mutated.slice(0, 5).join(', ')}` }
      : { verdict: 'fail', integrity: 'violation', criteriaResults: [{ criterion: 'audit completed', met: false, evidence: `workspace mutated during audit: ${mutated.slice(0, 5).join(', ')}` }], summary: '[harness] workspace mutated during audit' };
  }
  // Persist to the run ledger (best-effort): history survives worktree
  // cleanup and board resets. Inconclusive runs record a fail/unknown entry
  // so gaps in the ledger are meaningful, not silent.
  const { appendAuditRecord } = await import('./ledger.js');
  appendAuditRecord(
    await repoRootFrom(args.worktreePath),
    auditLedgerRecord({
      taskId: args.task.id,
      attempt: args.task.attempts,
      verdict: report ?? { verdict: 'fail', integrity: 'suspect', criteriaResults: [], summary: 'audit inconclusive: worker crash or unparsable report' },
    }),
  );
  return report;
}

/**
 * Resolve the main repository root from a linked worktree so ledger writes
 * land in one durable place regardless of which worktree ran the audit.
 * Falls back to the worktree itself outside a git context.
 */
export async function repoRootFrom(worktreePath: string): Promise<string> {
  const r = await spawnCli('git', ['rev-parse', '--git-common-dir'], { cwd: worktreePath, timeoutMs: 15_000 });
  if (r.exitCode !== 0 || !r.stdout.trim()) return worktreePath;
  const dir = r.stdout.trim();
  return dir.endsWith('/.git') || dir === '.git' ? join(worktreePath, dir, '..') : dirname(dir);
}
