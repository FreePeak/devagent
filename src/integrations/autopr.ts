import { analyzeAsyncHazards } from '../validation/async-review.js';
import { runCli } from '../workers/spawn-utils.js';
import { loadConfig, type DevAgentConfig } from '../config.js';
import type { Finding } from '../types.js';

/**
 * Auto review + auto merge for GitHub PRs: when the operator opts out of
 * manual review (config `autoMerge`), DevAgent reviews its own PRs against
 * objective evidence — green CI, mergeability, and a static hazard scan of
 * the added diff lines — then approves and squash-merges. A PR that fails any
 * gate gets a request-changes review instead; nothing is ever merged on red.
 */

export type RunGh = (args: string[], cwd: string) => Promise<{ stdout: string; stderr: string }>;

async function defaultRunGh(args: string[], cwd: string): Promise<{ stdout: string; stderr: string }> {
  // runCli routes through buildEnv so `gh` resolves from the fallback PATH
  // when the parent has a minimal env (live-smoke lesson). Translate
  // non-zero exit into a thrown Error so existing `try/catch` call sites
  // keep working without churn.
  const r = await runCli('gh', args, { cwd, timeoutMs: 30_000 });
  if (r.exitCode !== 0) {
    const err = new Error(`gh ${args.join(' ')} exited ${r.exitCode}: ${r.stderr.slice(0, 200)}`) as Error & {
      stdout?: string;
      stderr?: string;
      code?: number | string;
    };
    err.stdout = r.stdout;
    err.stderr = r.stderr;
    err.code = r.exitCode;
    throw err;
  }
  return { stdout: r.stdout, stderr: r.stderr };
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
  /** Head commit SHA (when reported); identifies the superseded merge candidate in sweep comments. */
  headRefOid: string;
  /** ISO last-update timestamp (when reported); grace-window input for the zombie-PR sweep. */
  updatedAt: string;
  /** Author login; when it equals the gh token's viewer, approvals are impossible. */
  author: string;
  checks: CheckRun[];
}

const PR_FIELDS =
  'number,title,headRefName,baseRefName,state,mergeable,reviewDecision,author,statusCheckRollup,headRefOid,updatedAt';

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
    headRefOid: String(raw.headRefOid ?? ''),
    updatedAt: String(raw.updatedAt ?? ''),
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
  action: 'merged' | 'review-requested' | 'skipped' | 'ci-fix-failed';
  detail: string;
  /** CI-Fixer failure evidence (Q24 error taxonomy); set on 'ci-fix-failed' outcomes. */
  failedChecks?: string[];
  attempts?: number;
  summary?: string;
}

export interface CiFixRequest {
  repoPath: string;
  pr: number;
  /** Task identity for the re-dispatched run (TASK-fix-<pr>). */
  taskId: string;
  failedChecks: string[];
  prompt: string;
}

export type CiFixer = (req: CiFixRequest) => Promise<{ ok: boolean; note: string }>;

/**
 * Built-in CI-Fixer dispatch: delegate a `devagent task` repair run to the
 * shared host over SSH (same transport as `task --remote`), keyed by a
 * TASK-fix-<pr> task id. Deployment opt-in: without DEVAGENT_REMOTE_TARGET the
 * dispatcher reports not-ok so the PR keeps a structured failure outcome
 * instead of pretending a fix ran.
 */
export async function defaultCiFixer(req: CiFixRequest): Promise<{ ok: boolean; note: string }> {
  const target = process.env.DEVAGENT_REMOTE_TARGET;
  if (!target) {
    return { ok: false, note: 'ci-fix not configured: set DEVAGENT_REMOTE_TARGET to enable the default fixer dispatch' };
  }
  const { runRemoteTask } = await import('../remote.js');
  const { spawnCli } = await import('../workers/spawn-utils.js');
  const res = await runRemoteTask(
    {
      target,
      prompt: req.prompt,
      taskId: req.taskId,
      timeoutMs: loadConfig(req.repoPath).timeoutMinutes * 60_000,
      log: { warn: () => {} },
    },
    { run: (argv, timeoutMs) => spawnCli(argv[0]!, argv.slice(1), { cwd: req.repoPath, timeoutMs }) },
  );
  return { ok: res.ok, note: res.note };
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
  /**
   * CI-Fixer: when failed checks block the merge, one bounded re-dispatch
   * (default: `defaultCiFixer`) runs before the verdict falls back to
   * request-changes; the merge only proceeds if checks are green afterward.
   */
  fixer?: CiFixer;
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
  let cv: ChecksVerdict;
  for (;;) {
    cv = evaluateChecks(status);
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

  // CI-Fixer (PRD.md:737): when failed checks are the blocker, give the PR one
  // bounded repair re-dispatch before falling back to request-changes. The
  // verdict is re-derived from a fresh status poll so only genuinely green
  // checks can reach the merge below (never worker-graded results).
  if (review.event === 'REQUEST_CHANGES' && cv.failedChecks.length > 0) {
    const fixer = opts.fixer ?? defaultCiFixer;
    const failedChecks = cv.failedChecks;
    const prompt = [
      `Fix the failing CI checks on PR #${pr} (${repoPath}).`,
      `Failed checks: ${failedChecks.join(', ')}.`,
      'Reproduce locally, apply the minimal fix, push to the PR branch, and let CI re-run.',
    ].join(' ');
    log(`ci-fix: re-dispatching fixer for PR #${pr} (failed: ${failedChecks.join(', ')})`);
    try {
      const fixRes = await fixer({ repoPath, pr, taskId: `TASK-fix-${pr}`, failedChecks, prompt });
      if (!fixRes.ok) {
        // Nothing was dispatched: record the structured failure and skip the
        // re-poll entirely (no pointless CI wait on an unchanged head SHA).
        return {
          pr,
          title: status.title,
          action: 'ci-fix-failed',
          detail: `ci-fix dispatch failed: ${fixRes.note.slice(0, 200)}`,
          failedChecks,
          attempts: 1,
          summary: cv.summary,
        };
      }
    } catch (e) {
      return {
        pr,
        title: status.title,
        action: 'ci-fix-failed',
        detail: `ci-fix dispatch threw: ${(e as Error).message.slice(0, 200)}`,
        failedChecks,
        attempts: 1,
        summary: cv.summary,
      };
    }
    // Re-poll to a completed rollup on the new head SHA, then re-evaluate.
    const fixDeadline = Date.now() + (opts.waitForChecksSec ?? 300) * 1000;
    for (;;) {
      status = await getPrStatus(repoPath, pr, run);
      const fixCv = evaluateChecks(status);
      if (!fixCv.pending) {
        if (fixCv.passed) {
          log(`ci-fix: checks green after fix dispatch; merging PR #${pr}`);
          break;
        }
        return {
          pr,
          title: status.title,
          action: 'ci-fix-failed',
          detail: `checks still failing after 1 fix attempt: ${fixCv.summary}`,
          failedChecks: fixCv.failedChecks.length ? fixCv.failedChecks : failedChecks,
          attempts: 1,
          summary: fixCv.summary,
        };
      }
      if (Date.now() >= fixDeadline) {
        return {
          pr,
          title: status.title,
          action: 'ci-fix-failed',
          detail: `checks still pending after fix attempt: ${fixCv.summary}`,
          failedChecks,
          attempts: 1,
          summary: fixCv.summary,
        };
      }
      log(`ci-fix: checks pending (${fixCv.summary}); retrying in 15s`);
      await new Promise((res) => setTimeout(res, opts.pollIntervalMs ?? 15_000));
    }
    const fixedReview = evaluateAutoReview(status, { hazards, mergeMethod: method });
    return finishAutoMerge(repoPath, pr, status, fixedReview, method, deleteBranch, run, log);
  }

  return finishAutoMerge(repoPath, pr, status, review, method, deleteBranch, run, log);
}

/** Post the verdict, then attempt the merge; shared by the direct and CI-fixed paths. */
async function finishAutoMerge(
  repoPath: string,
  pr: number,
  status: PrStatus,
  review: { event: ReviewEvent; reason: string; body: string },
  method: 'squash' | 'merge' | 'rebase',
  deleteBranch: boolean,
  run: RunGh,
  log: (msg: string) => void,
): Promise<AutoMergeOutcome> {
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

export type ZombiePrAction = 'superseded' | 'closed' | 'skipped' | 'untouched';

export interface ZombiePrOutcome {
  pr: number;
  title: string;
  action: ZombiePrAction;
  detail: string;
}

export interface ZombiePrOptions {
  /** Days a PR may stay red before auto-close (config `zombiePrs.graceDays`, default 7). */
  graceDays?: number;
  /** Report without commenting or closing (config `zombiePrs.dryRun`, default true). */
  dryRun?: boolean;
  log?: (msg: string) => void;
}

/**
 * A mergeable candidate: open, mergeable, and provably green (checks
 * completed and passed). PRs with pending or missing checks carry no evidence
 * and never count — neither as candidates nor as zombies.
 */
function isMergeCandidate(status: PrStatus): boolean {
  if (status.state !== 'OPEN' || status.mergeable !== 'MERGEABLE') return false;
  const cv = evaluateChecks(status);
  return !cv.pending && cv.passed && status.checks.length > 0;
}

/**
 * Zombie-PR hygiene sweep (PRD.md:737, second clause): every open PR is a
 * mergeable candidate, left untouched, commented as superseded, or closed —
 * per the config `zombiePrs` block (graceDays, dryRun; dry-run default):
 * - superseded: the head SHA is not a mergeable candidate while another open
 *   PR on the same base is; the PR gets a skip comment naming the candidate.
 * - closed: CI red (completed failures, nothing pending) across the grace
 *   window measured from `updatedAt`; closed with an explanatory comment.
 * - untouched: everything else (pending or missing checks, green, or red
 *   within grace) carries no supersession evidence and stays as-is.
 */
export async function sweepStalePrs(
  repoPath: string,
  opts: ZombiePrOptions = {},
  run: RunGh = defaultRunGh,
): Promise<ZombiePrOutcome[]> {
  let cfgZombie: NonNullable<DevAgentConfig['zombiePrs']> = {};
  try {
    cfgZombie = loadConfig(repoPath).zombiePrs ?? {};
  } catch {
    // unreadable config: fall through to built-in defaults
  }
  const graceDays = opts.graceDays ?? cfgZombie.graceDays ?? 7;
  const dryRun = opts.dryRun ?? cfgZombie.dryRun ?? true;
  const log = opts.log ?? (() => {});
  const prs = (await listOpenPrs(repoPath, run)).slice().sort((a, b) => a.number - b.number);
  const candidateByBase = new Map<string, number>();
  for (const p of prs) {
    if (isMergeCandidate(p) && !candidateByBase.has(p.baseRefName)) candidateByBase.set(p.baseRefName, p.number);
  }
  const outcomes: ZombiePrOutcome[] = [];
  for (const status of prs) {
    if (status.state !== 'OPEN') {
      outcomes.push({ pr: status.number, title: status.title, action: 'skipped', detail: `state is ${status.state}` });
      continue;
    }
    const candidate = candidateByBase.get(status.baseRefName);
    const cv = evaluateChecks(status);
    if (candidate !== undefined && candidate !== status.number && cv.pending === false && cv.passed) {
      // Not red, not a candidate, and another open PR on the same base is:
      // this head is superseded (conflicting, blocked, or unmergeable).
      const head = status.headRefOid || 'unknown-sha';
      const detail = `head ${head} is not a mergeable candidate; base ${status.baseRefName} superseded by #${candidate}`;
      const body = [
        `DevAgent zombie-PR sweep: skipping this PR — head ${head} is not a mergeable candidate.`,
        `Base ${status.baseRefName} is superseded by open PR #${candidate}; rebase or close this branch.`,
      ].join('\n');
      if (dryRun) {
        outcomes.push({ pr: status.number, title: status.title, action: 'superseded', detail: `[dry-run] ${detail}` });
      } else {
        await postPrComment(repoPath, status.number, body, run);
        log(`#${status.number} superseded: ${detail}`);
        outcomes.push({ pr: status.number, title: status.title, action: 'superseded', detail });
      }
      continue;
    }
    if (!cv.pending && !cv.passed) {
      const updatedMs = Date.parse(status.updatedAt);
      const redDays = Number.isFinite(updatedMs) ? (Date.now() - updatedMs) / 86_400_000 : Infinity;
      if (redDays >= graceDays) {
        const detail = `red across ${graceDays}d grace window (${Math.floor(redDays)}d since last update)`;
        if (dryRun) {
          outcomes.push({ pr: status.number, title: status.title, action: 'closed', detail: `[dry-run] ${detail}` });
        } else {
          await postPrComment(
            repoPath,
            status.number,
            [
              'DevAgent zombie-PR sweep: auto-closing this PR.',
              `CI has been red for the full ${graceDays}-day grace window (${cv.summary});`,
              'reopen with a fix or abandon the branch.',
            ].join('\n'),
            run,
          );
          await run(['pr', 'close', String(status.number)], repoPath);
          log(`#${status.number} closed: ${detail}`);
          outcomes.push({ pr: status.number, title: status.title, action: 'closed', detail });
        }
        continue;
      }
      outcomes.push({ pr: status.number, title: status.title, action: 'untouched', detail: `red within grace: ${cv.summary}` });
      continue;
    }
    outcomes.push({ pr: status.number, title: status.title, action: 'untouched', detail: cv.pending ? `checks pending: ${cv.summary}` : `no superseding candidate: ${cv.summary}` });
  }
  return outcomes;
}
