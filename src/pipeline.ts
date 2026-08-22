import type { RunConfig, TicketSpec, WorkerName } from './types.js';
import type { RunLogger } from './logger.js';
import { checkSpec, planFromTicket } from './planner.js';

/**
 * Pipeline state machine skeleton (PRD section 10).
 * Loop 1 wires: fetch -> plan -> spec-check. Implement/validate/publish stages
 * call into worker adapters and gates; sandbox-dependent stages (G1/G2) are stubbed.
 */

export type StageOutcome =
  | { stage: 'clarify'; question: string }
  | { stage: 'plan'; summary: string; tasks: string[] }
  | { stage: 'implement'; worker: WorkerName; note: string }
  | { stage: 'validate'; passed: boolean }
  | { stage: 'publish'; prUrl?: string; note: string }
  | { stage: 'failed'; reason: string };

export interface PipelineDeps {
  fetchTicket(ticketId: string): Promise<TicketSpec>;
  postTicketComment?(ticketId: string, comment: string): Promise<void>;
  runGateG3(repoPath: string, classification: import('./types.js').TicketClass): {
    passed: boolean;
    findings: unknown[];
    detail?: string;
  };
}

export async function runPipeline(cfg: RunConfig, deps: PipelineDeps, log: RunLogger): Promise<StageOutcome[]> {
  const outcomes: StageOutcome[] = [];

  // Stage: fetch
  log.info('fetch', `Fetching ticket ${cfg.ticketId}`);
  const ticket = await deps.fetchTicket(cfg.ticketId);
  log.info('fetch', `Fetched "${ticket.title}"`, { labels: ticket.labels });

  // Stage: plan
  const plan = planFromTicket(ticket);
  log.info('plan', `Classified as ${plan.classification}`, { tasks: plan.tasks });

  // Spec check (FR-TICKET-05)
  const spec = checkSpec(ticket);
  if (!spec.sufficient && spec.question) {
    log.warn('clarify', 'Insufficient specification', {});
    outcomes.push({ stage: 'clarify', question: spec.question });
    await deps.postTicketComment?.(cfg.ticketId, spec.question);
    return outcomes;
  }
  outcomes.push({ stage: 'plan', summary: plan.classification, tasks: plan.tasks });

  if (cfg.dryRun) {
    log.info('plan', 'Dry-run: stopping before implement', {});
    return outcomes;
  }

  // Stage: implement — delegated to worker adapters (wired in cli.ts)
  outcomes.push({ stage: 'implement', worker: cfg.worker === 'both' ? 'claude-code' : cfg.worker, note: 'worker dispatch' });

  // Stage: validate — G3 runs without a sandbox
  const g3 = deps.runGateG3(cfg.repoPath, plan.classification);
  log.info('validate', `G3 ${g3.passed ? 'passed' : 'failed'}: ${g3.detail ?? ''}`, {});
  outcomes.push({ stage: 'validate', passed: g3.passed });
  if (!g3.passed) {
    outcomes.push({ stage: 'failed', reason: 'migration static gate failed' });
    return outcomes;
  }

  // Stage: publish — gated on autoPr/interactive (handled by caller)
  outcomes.push({ stage: 'publish', note: cfg.autoPr ? 'auto-pr enabled' : 'awaiting approval' });
  return outcomes;
}
