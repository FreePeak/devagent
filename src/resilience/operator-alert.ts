import type { DegradeBreachAlert } from './preflight.js';

/**
 * Operator paging transport (PRD:913 Q16).
 *
 * Q41 shipped the only outbound signal the factory had: a single JSON POST
 * to `resilience.degradeWebhookUrl` when the provider-degraded streak first
 * breached (see `DegradeBreachAlert` in preflight.ts). Q16 generalizes that
 * one-off transport into a typed operator-alert seam so any subsystem that
 * needs to page a human posts through the same best-effort path — today the
 * board-recovery gate, which archives a stalled board with zero outbound
 * signal (the 2026-08-29 factory sat archived for 6h unnoticed).
 *
 * An `OperatorAlert` is a discriminated union keyed on `event`, so a receiver
 * routes the payload without parsing prose. `postOperatorAlert` is the default
 * transport; every caller injects its own `notify` in tests and swallows the
 * result — paging is observability, never a second failure surface on top of
 * the condition it reports.
 */

/** Wall-clock cap for the paging POST: paging must never stall a loop cycle. */
export const OPERATOR_ALERT_TIMEOUT_MS = 5_000;

/**
 * A board moved into `.devagent/archive/` (Q16). Fired once per archive, for
 * both gate verdicts: `prefix: 'board'` is the completed-board infinity-cycle
 * archive, `prefix: 'board-stuck'` is the undispatchable archive.
 */
export interface BoardArchivedAlert {
  /** Discriminator so a receiver routes the payload without parsing prose. */
  event: 'board-archived';
  /** ISO ts of the paging POST. */
  ts: string;
  /** Repo whose board was archived. */
  repo: string;
  /** Archive filename prefix — the gate verdict that caused the move. */
  prefix: 'board' | 'board-stuck';
  /** Repo-relative path the board was moved to. */
  path: string;
  /** Human-readable why, carried off the recovery verdict. */
  reason: string;
}

/** Every payload the operator webhook accepts, keyed on `event`. */
export type OperatorAlert = DegradeBreachAlert | BoardArchivedAlert;

/** Injection seam for the outbound paging transport (tests swap it out). */
export type OperatorNotifier = (url: string, alert: OperatorAlert) => Promise<void>;

/**
 * Default paging transport: JSON POST to the operator webhook. A non-2xx
 * response rejects; every caller swallows it so paging can never become a
 * second failure surface on top of the condition it reports.
 */
export async function postOperatorAlert(url: string, alert: OperatorAlert): Promise<void> {
  const res = await fetch(url, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(alert),
    signal: AbortSignal.timeout(OPERATOR_ALERT_TIMEOUT_MS),
  });
  if (!res.ok) throw new Error(`operator alert webhook responded ${res.status}`);
}
