import { loadConfig, type DevAgentConfig } from '../config.js';
import {
  evaluateChecks,
  listOpenPrs,
  postPrComment,
  defaultRunGh,
  baseBranchGone,
  ageHours,
  type RunGh,
} from '../integrations/autopr.js';
import { appendPrHygieneRecord } from './ledger.js';

/**
 * Zombie-PR hygiene (PRD §17, surviving half of post-PR lifecycle automation):
 * per-task PRs ship via publishTaskPr, but nothing reaped them when their
 * context died. This sweep closes the loop for `devagent/TASK-*` PRs:
 * - base-superseded: the PR's head base branch was merged or deleted, so the
 *   branch can never integrate — auto-close it (dry-run: flag only).
 * - red-across-grace: CI stays red (completed failures, nothing pending) for
 *   the full grace window — flag the PR and skip autoMerge until a green
 *   check arrives.
 * Every action writes one ledger row (pr, reason, grace-window age) so the
 * run ledger stays the replayable record of what automation did to PRs.
 */

const TASK_PR_RE = /^devagent\/TASK-/;

export type PrHygieneAction = 'closed' | 'flagged' | 'skipped' | 'untouched';

export interface PrHygieneOutcome {
  pr: number;
  title: string;
  action: PrHygieneAction;
  /** Why the action fired: base-superseded | red-across-grace | not-a-task-pr | pending | green | red-within-grace. */
  reason: string;
  /** Hours since the PR's last update (grace-window age); null when unknown or not applicable. */
  graceAgeHours: number | null;
  detail: string;
}

export interface PrHygieneOptions {
  /** Hours a PR may stay red before it is flagged for skip-autoMerge (config `prHygiene.graceHours`, default 24). */
  graceHours?: number;
  /** Report without commenting or closing (config `prHygiene.dryRun`, default true). */
  dryRun?: boolean;
  /** Auto-merge flag as seen by the caller; the sweep skips the merge while CI is red across grace. */
  autoMerge?: boolean;
  log?: (msg: string) => void;
}

export interface PrHygieneResult {
  outcomes: PrHygieneOutcome[];
  /** True when at least one red-across-grace PR forces autoMerge off for this cycle. */
  skipAutoMerge: boolean;
}

/** Pure grace-window age in hours; null when the timestamp is missing or unparseable. */
export const graceAgeHours = ageHours;

/**
 * Zombie-PR hygiene sweep over open `devagent/TASK-*` PRs. Non-task PRs are
 * reported as 'untouched' and never acted on — the factory only reaps its
 * own output. With dryRun (the default) nothing is commented or closed; the
 * ledger still records 'flagged' rows so the dry-run leaves the same
 * evidence trail as a live sweep minus the GitHub mutations.
 */
export async function sweepTaskPrHygiene(
  repoPath: string,
  opts: PrHygieneOptions = {},
  run: RunGh = defaultRunGh,
): Promise<PrHygieneResult> {
  let cfg: NonNullable<DevAgentConfig['prHygiene']> = {};
  try {
    cfg = loadConfig(repoPath).prHygiene ?? {};
  } catch {
    // unreadable config: fall through to built-in defaults
  }
  const graceHours = opts.graceHours ?? cfg.graceHours ?? 24;
  const dryRun = opts.dryRun ?? cfg.dryRun ?? true;
  const autoMerge = opts.autoMerge ?? false;
  const log = opts.log ?? (() => {});
  const now = Date.now();

  const prs = (await listOpenPrs(repoPath, run)).slice().sort((a, b) => a.number - b.number);
  const outcomes: PrHygieneOutcome[] = [];
  let skipAutoMerge = false;

  for (const status of prs) {
    if (status.state !== 'OPEN') {
      outcomes.push({ pr: status.number, title: status.title, action: 'skipped', reason: `state-${status.state.toLowerCase()}`, graceAgeHours: null, detail: `state is ${status.state}` });
      continue;
    }
    if (!TASK_PR_RE.test(status.headRefName)) {
      outcomes.push({ pr: status.number, title: status.title, action: 'untouched', reason: 'not-a-task-pr', graceAgeHours: null, detail: `head ${status.headRefName} is not a devagent/TASK-* branch` });
      continue;
    }
    const age = graceAgeHours(status.updatedAt, now);
    const cv = evaluateChecks(status);

    // Base-superseded first (condition 1): a PR whose head base branch was
    // merged or deleted can never integrate, whatever its CI says — waiting
    // cannot fix a dead base.
    if (await baseBranchGone(repoPath, status.baseRefName, run)) {
      const detail = `base ${status.baseRefName} was merged or deleted; branch can never integrate`;
      if (dryRun) {
        appendPrHygieneRecord(repoPath, {
          ts: new Date().toISOString(),
          kind: 'event',
          event: 'pr-hygiene',
          taskId: `TASK-${status.number}`,
          attempt: 1,
          pr: status.number,
          action: 'flagged',
          reason: 'base-superseded',
          graceAgeHours: age,
          detail: `[dry-run] ${detail}`,
        });
        outcomes.push({ pr: status.number, title: status.title, action: 'flagged', reason: 'base-superseded', graceAgeHours: age, detail: `[dry-run] ${detail}` });
      } else {
        await postPrComment(
          repoPath,
          status.number,
          [
            'DevAgent zombie-PR sweep: auto-closing this PR.',
            `Base branch ${status.baseRefName} was merged or deleted, so this branch can no longer integrate.`,
            'Reopen with a re-targeted base if the work is still needed.',
          ].join('\n'),
          run,
        );
        await run(['pr', 'close', String(status.number)], repoPath);
        appendPrHygieneRecord(repoPath, {
          ts: new Date().toISOString(),
          kind: 'event',
          event: 'pr-hygiene',
          taskId: `TASK-${status.number}`,
          attempt: 1,
          pr: status.number,
          action: 'closed',
          reason: 'base-superseded',
          graceAgeHours: age,
          detail,
        });
        log(`#${status.number} closed: ${detail}`);
        outcomes.push({ pr: status.number, title: status.title, action: 'closed', reason: 'base-superseded', graceAgeHours: age, detail });
      }
      continue;
    }

    if (!cv.pending && !cv.passed && status.checks.length > 0) {
      // Red across the grace window: flag it and hold autoMerge until a
      // green check arrives. Never closed — a fix may still land.
      const overdue = age === null || age >= graceHours;
      if (overdue) {
        skipAutoMerge = true;
        const detail = `red across ${graceHours}h grace window (${age === null ? 'unknown age' : `${Math.floor(age)}h since last update`}); autoMerge skipped until green: ${cv.summary}`;
        appendPrHygieneRecord(repoPath, {
          ts: new Date().toISOString(),
          kind: 'event',
          event: 'pr-hygiene',
          taskId: `TASK-${status.number}`,
          attempt: 1,
          pr: status.number,
          action: 'flagged',
          reason: 'red-across-grace',
          graceAgeHours: age,
          detail: detail.slice(0, 300),
        });
        log(`#${status.number} flagged: ${detail}`);
        outcomes.push({ pr: status.number, title: status.title, action: 'flagged', reason: 'red-across-grace', graceAgeHours: age, detail });
        continue;
      }
      outcomes.push({ pr: status.number, title: status.title, action: 'untouched', reason: 'red-within-grace', graceAgeHours: age, detail: `red within grace: ${cv.summary}` });
      continue;
    }

    // Base-superseded was handled above; reaching here means the base is
    // intact and the PR is green, pending, or red-within-grace.
    outcomes.push({
      pr: status.number,
      title: status.title,
      action: 'untouched',
      reason: cv.pending ? 'pending' : 'green',
      graceAgeHours: age,
      detail: cv.pending ? `checks pending: ${cv.summary}` : `green and base ${status.baseRefName} intact: ${cv.summary}`,
    });
  }

  return { outcomes, skipAutoMerge: skipAutoMerge && autoMerge };
}
