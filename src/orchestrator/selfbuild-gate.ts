import { readFileSync } from 'node:fs';

/**
 * Selfbuild loop gates (PRD:888) — the loop's highest-churn decisions folded
 * out of scripts/selfbuild-loop.sh into typed, tested code. The shell is a
 * thin caller via `devagent selfbuild-gate --starved / --already-shipped <goal>`
 * (src/cli.ts), using the same exit-code contract as `backlog-check` (PRD:889):
 * 0 = continue, 1 = gate verdict (halt/skip), 2 = unresolved (caller misuse).
 *
 * Starvation gate: consecutive non-productive iterations across ALL runs.
 * Unlike the circuit breaker (in-process failures), this catches a loop that
 * has been thrashing for days without shipping anything (Kitchen Loop 7.2).
 * Productive = shipped or handed to review; the ledger's richer statuses
 * ("pr-open", "merged", "pushed" — written by the Orca-driven runs) all count,
 * otherwise a healthy streak reads as starvation and halts the loop.
 * Degraded rows are expected pauses, not evidence of a thrashing loop — never
 * count them toward starvation (they neither break nor increment the streak):
 *   operator-degraded  operator absence / dirty PRD / omp wedge skip
 *   operator-diverged  doc-sync hit a diverged history the operator
 *                      must reconcile (conflict or diverged+dirty PRD)
 *   provider-degraded  preflight found the provider down (no spend);
 *                      2026-09-05: three circuit-outage rows tripped
 *                      the 5-strike gate and halted the factory after
 *                      the provider had already recovered.
 *
 * Q27 re-burn guard: a goal is "already handled" when any ledger entry with a
 * productive status carries the same goal text (normalized: quotes stripped,
 * whitespace collapsed). Phase 2-3 selection can otherwise re-pick a goal
 * whose PR already merged — loops 53-55/57/58 and the 2026-09-01 Q35 re-burn
 * (shipped as #100, then re-selected because the driver restart lost the
 * record) each burned attempts on an already-planned goal. Since PRD:889 this
 * is the fallback guard: goals naming a Phase 4 backlog id are first resolved
 * by the pick-time `devagent backlog-check`, which supersedes this heuristic
 * when it resolves.
 *
 * Id matching (loop-100 "+ Q41" false-positive guard): a goal is matched by
 * backlog id only when the id sits in the SUBJECT of BOTH the candidate and
 * the productive row. The id is stable but goal text is rewritten between
 * selection and record, and an ok row may only MENTION an id in passing:
 * loop-100's doc-sync goal reads "(PRD §17 doc-sync defect + Q41 degradation
 * surface)", yet Q41 — the open no-notification-surface gap (PRD :937) — was
 * still unshipped. A whole-row id match made loop-109's "Goal: Q41 ..." skip
 * as already-shipped (false positive that contributed to the 2026-09-06
 * starvation halt). Subject = goal text up to the first "(" (candidate: also
 * capped at 80 chars; row: capped at 90); the first-60-char goal-prefix
 * fallback is kept too.
 */

/** Ledger statuses that count as productive (break a starvation streak / mark a goal shipped). */
export const PRODUCTIVE_STATUSES = ['ok', 'pr-open', 'merged', 'pushed'] as const;

/** Ledger statuses exempt from starvation counting (expected operator/provider pauses). */
export const DEGRADED_STATUSES = ['operator-degraded', 'operator-diverged', 'provider-degraded'] as const;

const PRODUCTIVE_RE = new RegExp(`"status":"(?:${PRODUCTIVE_STATUSES.join('|')})"`);
const DEGRADED_RE = new RegExp(`"status":"(?:${DEGRADED_STATUSES.join('|')})"`);

export interface StarvationVerdict {
  /** True when `count` non-productive rows since the last productive row reach `limit`. */
  starved: boolean;
  /** Consecutive non-productive, non-degraded rows counted from the ledger tail. */
  count: number;
  limit: number;
}

export interface AlreadyShippedVerdict {
  shipped: boolean;
  /** Which rule matched: normalized goal-prefix, or backlog id in both subjects. */
  reason: 'goal-prefix' | 'subject-id' | null;
}

/**
 * Normalize a goal/row text the way the shell did: strip double quotes,
 * collapse whitespace runs to single spaces.
 */
export function normalizeGoalText(text: string): string {
  return text.replace(/"/g, '').replace(/\s+/g, ' ');
}

/**
 * First backlog item id (Q<number>) in the goal's SUBJECT — the text before
 * the first "(" capped at 80 chars. Empty string when the goal names none.
 */
export function goalSubjectItem(goal: string): string {
  const want = normalizeGoalText(goal);
  const paren = want.indexOf('(');
  const subject = (paren >= 0 ? want.slice(0, paren) : want).slice(0, 80);
  return subject.match(/Q[0-9]+/)?.[0] ?? '';
}

/**
 * Starvation gate: walk the ledger from the tail; a productive row breaks the
 * streak, degraded rows are skipped (neither break nor count), anything else
 * increments. Starved when the count reaches `limit`.
 */
export function evaluateStarvation(ledgerLines: string[], limit: number): StarvationVerdict {
  let count = 0;
  for (let i = ledgerLines.length - 1; i >= 0; i--) {
    const line = ledgerLines[i] ?? '';
    if (PRODUCTIVE_RE.test(line)) break;
    if (DEGRADED_RE.test(line)) continue;
    if (++count >= limit) break;
  }
  return { starved: count >= limit, count, limit };
}

/**
 * Q27 re-burn guard: does any productive ledger row already carry this goal?
 * Rule 1 (prefix): the first 60 normalized chars of the goal appear anywhere
 * in a normalized productive row. Rule 2 (subject-id): when the goal's subject
 * names a backlog id, a productive row whose own subject (goal field, before
 * the first "(") contains that id matches — the id must sit in the SUBJECT of
 * both sides, so an incidental "+ Q41" mention inside a parenthetical cannot
 * false-positive (loop-100 guard).
 *
 * One intentional divergence from the awk original (verified 81/82 parity on
 * the real .selfbuild/ledger.jsonl; the single delta is this case): the awk
 * `index($0, "")` returns 1, so an EMPTY goal matched every productive row
 * and read as already-shipped. Here an empty goal never matches — the
 * driver's `^Goal:` gate makes an empty candidate unreachable, and
 * "empty matches everything" is exactly the false-positive class the
 * loop-100 guard exists to eliminate.
 */
export function alreadyShipped(goal: string, ledgerLines: string[]): AlreadyShippedVerdict {
  const want = normalizeGoalText(goal);
  const item = goalSubjectItem(goal);
  const key = want.slice(0, 60);
  for (const raw of ledgerLines) {
    if (!PRODUCTIVE_RE.test(raw)) continue;
    let row = normalizeGoalText(raw);
    if (key !== '' && row.includes(key)) return { shipped: true, reason: 'goal-prefix' };
    // Isolate the goal field value: drop everything up to the LAST "goal:"
    // (greedy, mirroring the awk `gsub(/.*goal:/, "", $0)`); a row without
    // the marker is checked whole.
    const gi = row.lastIndexOf('goal:');
    if (gi >= 0) row = row.slice(gi + 'goal:'.length);
    let head = row.slice(0, 90);
    const pi = head.indexOf('(');
    if (pi >= 0) head = head.slice(0, pi);
    if (item !== '' && head.includes(item)) return { shipped: true, reason: 'subject-id' };
  }
  return { shipped: false, reason: null };
}

/**
 * Read ledger lines from a JSONL file. A missing/unreadable ledger reads as
 * empty — the same fallback the shell applied (`[ -f ... ] || return 1` →
 * not starved / not shipped → continue).
 */
export function readLedgerLines(ledgerPath: string): string[] {
  let content: string;
  try {
    content = readFileSync(ledgerPath, 'utf8');
  } catch {
    return [];
  }
  const lines = content.split('\n');
  // awk record semantics: a trailing newline terminates the last record and
  // does not produce a phantom empty line.
  if (lines.length > 0 && lines[lines.length - 1] === '') lines.pop();
  return lines;
}
