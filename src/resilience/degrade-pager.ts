import { loadConfig } from '../config.js';
import { DEGRADE_STREAK_THRESHOLD, readDegradationStreak } from './degradation.js';
import { postOperatorAlert } from './operator-alert.js';

/**
 * Shared one-per-episode degradation pager (PRD §18 Q41 write side).
 *
 * Q41 shipped the paging POST inside `runPreflightGate`, so only preflight
 * failures could page. The degradation streak `readDegradationStreak` reads is
 * wider than that: the selfbuild loop's doc-sync branch mirrors `loop-result`
 * rows into the same ledger for `operator-diverged` (rc=3) and
 * `provider-degraded` (rc=1) refusals, and those rows count toward the streak
 * but never reached a pager — loops 145–154 logged ten consecutive
 * operator-diverged rows with the operator blind. This module is that missing
 * caller-neutral write side: preflight and `devagent page-degrade-breach`
 * (the doc-sync surface) both page through it, so the threshold, the
 * once-per-episode rule, and the best-effort contract live in one place.
 *
 * Contract, unchanged from the preflight original: fires only on the cycle
 * where the trailing streak equals the threshold (mid-streak cycles stay
 * silent, the next episode breaches again from zero), and never throws — a
 * missing webhook, a broken `devagent.json`, a transport throw or a non-2xx
 * response must not change the caller's outcome. Paging is observability,
 * never a second failure surface on top of the condition it reports.
 */

/** Surfaces that can complete a degradation streak and claim the page. */
export const DEGRADE_BREACH_SOURCES = ['preflight', 'doc-sync'] as const;

export type DegradeBreachSource = (typeof DEGRADE_BREACH_SOURCES)[number];

export function isDegradeBreachSource(value: string): value is DegradeBreachSource {
  return (DEGRADE_BREACH_SOURCES as readonly string[]).includes(value);
}

/** Body of the one-per-episode operator paging POST (Q41 write side). */
export interface DegradeBreachAlert {
  /** Discriminator so a receiver routes the payload without parsing prose. */
  event: 'provider-degraded-breach';
  /** Which surface recorded the row that completed the streak. */
  source: DegradeBreachSource;
  /** ISO ts of the paging POST. */
  ts: string;
  /** Repo whose ledger tripped the streak. */
  repo: string;
  /** Role recorded on the breach (loop role; empty when the caller has none). */
  role: string;
  worker: string;
  model: string;
  /** Trailing degraded rows; equals `threshold` on the breach POST. */
  count: number;
  threshold: number;
  /** Outage window edges (null when the streak rows carry no parseable ts). */
  latestTs: string | null;
  oldestTs: string | null;
  /** Outage window width in ms (null when an edge has no parseable ts). */
  windowMs: number | null;
  /** Distinct roles degraded in the window, newest first. */
  roles: string[];
  /** Bounded human-readable why, carried off the failing surface. */
  detail?: string;
}

/** Injection seam for the breach transport (tests swap it out). */
export type DegradeBreachNotifier = (url: string, alert: DegradeBreachAlert) => Promise<void>;

/** Caller-supplied context for one paging attempt (tests inject `notify`). */
export interface PageDegradeBreachArgs {
  /** Repo owning `devagent.json` and the orchestration ledger. */
  repoPath: string;
  /** Surface that recorded the streak-completing row. */
  source: DegradeBreachSource;
  role?: string;
  worker?: string;
  model?: string;
  /** Bounded failure excerpt carried onto the alert. */
  detail?: string;
  /** Injection seam for tests: outbound paging transport. Defaults to postOperatorAlert. */
  notify?: DegradeBreachNotifier;
  /** Injectable clock for the alert ts (tests). */
  now?: Date;
}

/**
 * Page a human once per outage episode. Call it AFTER the degradation row for
 * this cycle lands, so the streak it reads includes this cycle. Returns true
 * only when a POST actually went out; every failure mode returns false.
 */
export async function pageDegradeBreach(args: PageDegradeBreachArgs): Promise<boolean> {
  let url: string | undefined;
  try {
    url = loadConfig(args.repoPath).resilience?.degradeWebhookUrl;
  } catch {
    return false; // a broken config file must not turn paging into a caller failure
  }
  if (!url) return false;
  const streak = readDegradationStreak(args.repoPath, DEGRADE_STREAK_THRESHOLD);
  if (streak.count !== streak.threshold) return false;
  const notify = args.notify ?? postOperatorAlert;
  try {
    await notify(url, {
      event: 'provider-degraded-breach',
      source: args.source,
      ts: (args.now ?? new Date()).toISOString(),
      repo: args.repoPath,
      role: args.role ?? '',
      worker: args.worker ?? '',
      model: args.model ?? '',
      count: streak.count,
      threshold: streak.threshold,
      latestTs: streak.latestTs,
      oldestTs: streak.oldestTs,
      windowMs: streak.windowMs,
      roles: streak.roles,
      ...(args.detail ? { detail: args.detail } : {}),
    });
  } catch {
    return false; // paging is observability, never a failure signal for the loop
  }
  return true;
}
