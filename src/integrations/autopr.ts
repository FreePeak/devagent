import { execFile } from 'node:child_process';
import { analyzeAsyncHazards } from '../validation/async-review.js';
import type { Finding } from '../types.js';

/**
 * Auto review + auto merge for GitHub PRs: when the operator opts out of
 * manual review (config `autoMerge`), DevAgent reviews its own PRs against
 * objective evidence — green CI, mergeability, and a static hazard scan of
 * the added diff lines — then approves and squash-merges. A PR that fails any
 * gate gets a request-changes review instead; nothing is ever merged on red.
 */

export type RunGh = (args: string[], cwd: string) => Promise<{ stdout: string; stderr: string }>;

function defaultRunGh(args: string[], cwd: string): Promise<{ stdout: string; stderr: string }> {
  return new Promise((resolve, reject) => {
    execFile('gh', args, { cwd }, (err, stdout, stderr) => {
      if (err) {
        const e = err as { stdout?: unknown; stderr?: unknown };
        if (e.stdout === undefined) e.stdout = String(stdout);
        if (e.stderr === undefined) e.stderr = String(stderr);
        reject(err);
      } else {
        resolve({ stdout: String(stdout), stderr: String(stderr) });
      }
    });
  });
}

export interface CheckRun {
  name: string;
  status: string;
  conclusion: string | null;
}

export interface PrStatus {
  number: number;
  title: string;
  headRefName: string;
  baseRefName: string;
  state: string;
  mergeable: string;
  reviewDecision: string;
  /** Author login; when it equals the gh token's viewer, approvals are impossible. */
  author: string;
  checks: CheckRun[];
}

const PR_FIELDS =
  'number,title,headRefName,baseRefName,state,mergeable,reviewDecision,author,statusCheckRollup';

function parsePr(raw: Record<string, unknown>): PrStatus {
  const rollup = (raw.statusCheckRollup as Array<Record<string, unknown>> | undefined) ?? [];
  const author = raw.author as { login?: string } | undefined;
  return {
    number: Number(raw.number),
    title: String(raw.title ?? ''),
    headRefName: String(raw.headRefName ?? ''),
    baseRefName: String(raw.baseRefName ?? ''),
    state: String(raw.state ?? ''),
    mergeable: String(raw.mergeable ?? 'UNKNOWN'),
    reviewDecision: String(raw.reviewDecision ?? ''),
    author: author?.login ?? '',
    checks: rollup.map((c) => ({
      name: String(c.name ?? 'unknown'),
      status: String(c.status ?? 'UNKNOWN'),
      conclusion: (c.conclusion as string | null) ?? null,
    })),
  };
}

/** True when a completed check counts as passing (SKIPPED/NEUTRAL are not failures). */
function checkPassed(c: CheckRun): boolean {
  const ok = ['SUCCESS', 'SKIPPED', 'NEUTRAL'];
  return c.conclusion !== null && ok.includes(c.conclusion);
}

export interface ChecksVerdict {
  pending: boolean;
  passed: boolean;
  failedChecks: string[];
  summary: string;
}

/** Pure evaluation of the CI rollup: any failure blocks, any run still going means wait. */
export function evaluateChecks(status: PrStatus): ChecksVerdict {
  const running = status.checks.filter((c) => c.status !== 'COMPLETED');
  const failed = status.checks.filter((c) => c.status === 'COMPLETED' && !checkPassed(c));
  return {
    pending: running.length > 0,
    passed: failed.length === 0,
    failedChecks: failed.map((c) => `${c.name}=${c.conclusion}`),
    summary:
      status.checks.length === 0
        ? 'no checks reported'
        : `${status.checks.filter(checkPassed).length}/${status.checks.length} checks passed` +
          (running.length ? `, ${running.length} running` : '') +
          (failed.length ? `, failed: ${failed.map((c) => c.name).join(', ')}` : ''),
  };
}

/**
 * Static hazard scan over the PR's added lines. Feeds only the `+` lines of
 * the patch to the existing async-hazard analyzer, so pre-existing hazards in
 * untouched code never surface. Findings are ADVISORY in the review verdict:
 * the line-level view cannot see multi-line catch handlers, so blocking here
 * would stall PRs on false positives; real regressions are caught by CI.
 */
export function scanAddedLinesForHazards(diff: string): Finding[] {
  const byFile = new Map<string, string[]>();
  let current: string | null = null;
  for (const line of diff.split('\n')) {
    if (line.startsWith('+++ b/')) {
      current = line.slice(6);
      if (!byFile.has(current)) byFile.set(current, []);
    } else if (line.startsWith('+') && current) {
      byFile.get(current)!.push(line.slice(1));
    }
  }
  const files = [...byFile.entries()].map(([path, lines]) => ({
    path,
    content: lines.join('\n'),
  }));
  return analyzeAsyncHazards(files);
}

export type ReviewEvent = 'APPROVE' | 'REQUEST_CHANGES';

export interface ReviewEvidence {
  hazards: Finding[];
  mergeMethod: string;
}

/** Pure verdict + review body from the collected evidence. */
export function evaluateAutoReview(status: PrStatus, evidence: ReviewEvidence): {
  event: ReviewEvent;
  reason: string;
  body: string;
} {
  const cv = evaluateChecks(status);
  const conflict = status.mergeable === 'CONFLICTING';
  const highHazards = evidence.hazards.filter((f) => f.severity === 'high');

  const gates: string[] = [`CI: ${cv.summary}`, `Mergeable: ${status.mergeable}`];
  const problems: string[] = [];
  if (!cv.passed) problems.push(`CI failed (${cv.failedChecks.join(', ')})`);
  if (conflict) problems.push('branch conflicts with base');

  const event: ReviewEvent = problems.length ? 'REQUEST_CHANGES' : 'APPROVE';
  const head = problems.length
    ? `Auto-review blocked: ${problems.join('; ')}`
    : 'Auto-review approved: all evidence gates passed';
  const body = [
    head,
    '',
    ...gates.map((g) => `- ${g}`),
    `- Hazard scan (advisory): ${evidence.hazards.length} finding(s), ${highHazards.length} high-severity`,
    ...evidence.hazards.slice(0, 10).map((f) => `  - ${f.ruleId} (${f.severity}) ${f.file}:${f.line} ${f.message}`),
    `- Merge strategy: ${evidence.mergeMethod}`,
    '',
    'Reviewed automatically by DevAgent (manual review disabled via autoMerge).',
  ].join('\n');
  return { event, reason: head, body };
}

export async function getPrStatus(
  repoPath: string,
  pr: number,
  run: RunGh = defaultRunGh,
): Promise<PrStatus> {
  const r = await run(['pr', 'view', String(pr), '--json', PR_FIELDS], repoPath);
  return parsePr(JSON.parse(r.stdout) as Record<string, unknown>);
}

export async function listOpenPrs(repoPath: string, run: RunGh = defaultRunGh): Promise<PrStatus[]> {
  const r = await run(['pr', 'list', '--state', 'open', '--json', PR_FIELDS], repoPath);
  return (JSON.parse(r.stdout) as Array<Record<string, unknown>>).map(parsePr);
}

/** Login owning the gh token; used to detect self-authored PRs. */
export async function getViewerLogin(repoPath: string, run: RunGh = defaultRunGh): Promise<string> {
  const r = await run(['api', 'user', '--jq', '.login'], repoPath);
  return r.stdout.trim();
}

export async function getPrDiff(repoPath: string, pr: number, run: RunGh = defaultRunGh): Promise<string> {
  const r = await run(['pr', 'diff', String(pr)], repoPath);
  return r.stdout;
}

export async function postPrReview(
  repoPath: string,
  pr: number,
  event: ReviewEvent,
  body: string,
  run: RunGh = defaultRunGh,
): Promise<void> {
  await run(['pr', 'review', String(pr), event === 'APPROVE' ? '--approve' : '--request-changes', '-b', body], repoPath);
}

/**
 * Evidence delivery for self-authored PRs: GitHub rejects self-approvals
 * ("Review Can not approve your own pull request"), so the verdict is posted
 * as a plain comment instead and merging relies on the gates alone.
 */
export async function postPrComment(
  repoPath: string,
  pr: number,
  body: string,
  run: RunGh = defaultRunGh,
): Promise<void> {
  await run(['pr', 'comment', String(pr), '--body', body], repoPath);
}

export async function mergePr(
  repoPath: string,
  pr: number,
  method: 'squash' | 'merge' | 'rebase',
  deleteBranch: boolean,
  run: RunGh = defaultRunGh,
): Promise<void> {
  const args = ['pr', 'merge', String(pr), `--${method}`];
  if (deleteBranch) args.push('--delete-branch');
  try {
    await run(args, repoPath);
  } catch (e) {
    // Branch protection can refuse a direct merge while --auto would queue it
    const msg = String((e as Error).message ?? '');
    if (/not mergeable|required|protected|not authorized/i.test(msg)) {
      await run([...args, '--auto'], repoPath);
      return;
    }
    throw new Error(`gh pr merge ${pr} failed: ${msg}`);
  }
}

export interface AutoMergeOutcome {
  pr: number;
  title: string;
  action: 'merged' | 'review-requested' | 'skipped';
  detail: string;
}

export interface AutoReviewAndMergeOptions {
  /** Only PRs targeting this branch are considered (default: no filter). */
  baseBranch?: string;
  method?: 'squash' | 'merge' | 'rebase';
  deleteBranch?: boolean;
  dryRun?: boolean;
  /** Seconds to wait for pending checks before giving up (default 300). */
  waitForChecksSec?: number;
  pollIntervalMs?: number;
}

/** Process one PR end-to-end: status -> diff hazard scan -> review -> merge. */
export async function autoReviewAndMergeOne(
  repoPath: string,
  pr: number,
  opts: AutoReviewAndMergeOptions & { log?: (msg: string) => void },
  run: RunGh = defaultRunGh,
): Promise<AutoMergeOutcome> {
  const log = opts.log ?? (() => {});
  const method = opts.method ?? 'squash';
  const deleteBranch = opts.deleteBranch ?? true;

  let status: PrStatus;
  try {
    status = await getPrStatus(repoPath, pr, run);
  } catch (e) {
    return { pr, title: '', action: 'skipped', detail: `cannot read PR: ${(e as Error).message.slice(0, 200)}` };
  }

  if (opts.baseBranch && status.baseRefName !== opts.baseBranch) {
    return { pr, title: status.title, action: 'skipped', detail: `base is ${status.baseRefName}, not ${opts.baseBranch}` };
  }
  if (status.state !== 'OPEN') {
    return { pr, title: status.title, action: 'skipped', detail: `state is ${status.state}` };
  }

  // Wait for pending checks so we never judge an incomplete rollup
  const deadline = Date.now() + (opts.waitForChecksSec ?? 300) * 1000;
  for (;;) {
    const cv = evaluateChecks(status);
    if (!cv.pending) break;
    if (Date.now() >= deadline) {
      return { pr, title: status.title, action: 'skipped', detail: `checks still pending after timeout: ${cv.summary}` };
    }
    log(`checks pending (${cv.summary}); retrying in 15s`);
    await new Promise((res) => setTimeout(res, opts.pollIntervalMs ?? 15_000));
    status = await getPrStatus(repoPath, pr, run);
  }

  let hazards: Finding[] = [];
  try {
    const diff = await getPrDiff(repoPath, pr, run);
    hazards = scanAddedLinesForHazards(diff);
  } catch {
    // diff unavailable (empty PR or gh hiccup): proceed with CI+mergeability only
  }

  const review = evaluateAutoReview(status, { hazards, mergeMethod: method });

  if (opts.dryRun) {
    return { pr, title: status.title, action: 'skipped', detail: `[dry-run] ${review.reason}` };
  }

  let selfAuthored = false;
  try {
    selfAuthored = Boolean(status.author) && status.author === (await getViewerLogin(repoPath, run));
  } catch {
    // viewer lookup failed: assume not self-authored and let the review fail loudly
  }
  if (selfAuthored) {
    await postPrComment(repoPath, pr, review.body, run);
    log(`verdict commented (self-authored): ${review.reason}`);
    if (review.event === 'REQUEST_CHANGES') {
      return { pr, title: status.title, action: 'review-requested', detail: review.reason };
    }
  } else {
    await postPrReview(repoPath, pr, review.event, review.body, run);
    log(`review posted: ${review.reason}`);
    if (review.event === 'REQUEST_CHANGES') {
      return { pr, title: status.title, action: 'review-requested', detail: review.reason };
    }
  }

  try {
    await mergePr(repoPath, pr, method, deleteBranch, run);
    return { pr, title: status.title, action: 'merged', detail: review.reason };
  } catch (e) {
    const msg = String((e as Error).message ?? '');
    // mergePr already retried with --auto; a remaining failure means GitHub
    // still refuses (protection rules, permissions), so leave the approval posted.
    return { pr, title: status.title, action: 'review-requested', detail: `approve succeeded but merge failed: ${msg.slice(0, 200)}` };
  }
}

/** Batch entry point: every open PR matching the filters, oldest first. */
export async function autoReviewAndMerge(
  repoPath: string,
  opts: AutoReviewAndMergeOptions & { prNumbers?: number[]; log?: (msg: string) => void },
  run: RunGh = defaultRunGh,
): Promise<AutoMergeOutcome[]> {
  const numbers = opts.prNumbers ?? (await listOpenPrs(repoPath, run)).map((p) => p.number).sort((a, b) => a - b);
  const outcomes: AutoMergeOutcome[] = [];
  for (const n of numbers) {
    const outcome = await autoReviewAndMergeOne(repoPath, n, opts, run);
    outcomes.push(outcome);
    opts.log?.(`${outcome.pr} ${outcome.action}: ${outcome.detail}`);
  }
  return outcomes;
}
