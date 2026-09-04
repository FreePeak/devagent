import type { ExecutorFailureClass, RunConfig, TicketClass, TicketSpec, WorkerName } from './types.js';
import type { ReadinessGateResult } from './validation/readiness-gate.js';
import type { RunLogger } from './logger.js';
import type { ImplementationPlan } from './planner.js';
import { checkSpec, planFromTicket } from './planner.js';

/**
 * Pipeline state machine (PRD section 10).
 * fetch -> plan -> spec-check -> implement -> validate(G3) -> publish.
 * Sandbox-dependent gates (G1/G2) are injected by the caller when available.
 */

export type StageOutcome =
  | { stage: 'clarify'; question: string }
  | { stage: 'plan'; summary: string; tasks: string[] }
  | {
      stage: 'implement';
      worker: WorkerName;
      worktreePath?: string;
      branch?: string;
      attempts: number;
      ok: boolean;
    }
  | { stage: 'validate'; passed: boolean }
  | { stage: 'publish'; prUrl?: string; note: string }
  | { stage: 'failed'; reason: string };

export interface ImplementResult {
  ok: boolean;
  worker: WorkerName;
  worktreePath?: string;
  branch?: string;
  attempts: number;
  /** Executor failure class on failure (PRD:775 / Q24 taxonomy mirror). */
  failureClass?: ExecutorFailureClass;
}

export interface PipelineDeps {
  fetchTicket(ticketId: string): Promise<TicketSpec>;
  postTicketComment?(trackerInternalId: string, comment: string): Promise<void>;
  /** G0 issue-readiness gate: type-specific ready-for-dev scoring before worker dispatch. Optional so hand-built test deps stay valid; buildDeps wires the deterministic default. */
  runGateG0?(ticket: TicketSpec, classification: TicketClass): ReadinessGateResult;
  runGateG3(repoPath: string, classification: import('./types.js').TicketClass): {
    passed: boolean;
    findings: unknown[];
    detail?: string;
  };
  /** Real worker dispatch; injected so the pipeline stays unit-testable. */
  implementStage?(cfg: RunConfig, plan: ImplementationPlan, log: RunLogger): Promise<ImplementResult>;
  /** G2 migration-apply gate; injected by the caller (Docker-based). */
  runGateG2?(worktreePath: string, timeoutMs: number): Promise<{ passed: boolean; skipped?: boolean; detail?: string }>;
  /** G4 async-review gate; injected by the caller (heuristic scan in cli/deps). */
  runGateG4?(worktreePath: string): Promise<{ passed: boolean; detail?: string }>;
  /** G1 test gate; injected by the caller (repo-native test run in cli.ts). */
  runGateG1?(worktreePath: string, timeoutMs: number): Promise<{ passed: boolean; skipped?: boolean; detail?: string }>;
  /** PR publishing; injected by the caller (gh-based in cli.ts). */
  publishStage?(cfg: RunConfig, plan: ImplementationPlan, impl: ImplementResult): Promise<string | undefined>;
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

  // Gate G0 (issue readiness): type-specific ready-for-dev scoring BEFORE
  // worker dispatch. Rejected tickets get actionable findings (posted to the
  // tracker when possible) so credits are not burned on under-specified work.
  if (deps.runGateG0) {
    const g0 = deps.runGateG0(ticket, plan.classification);
    log.info('validate', `G0 ${g0.skipped ? 'skipped' : g0.passed ? 'passed' : 'rejected'}: ${(g0.detail ?? '').split('\n')[0]}`, {
      score: g0.score,
      threshold: g0.threshold,
    });
    outcomes.push({ stage: 'validate', passed: g0.passed });
    if (!g0.passed) {
      log.warn('clarify', 'G0 readiness gate rejected ticket', { findings: g0.findings.length });
      if (ticket.trackerInternalId && deps.postTicketComment) {
        try {
          await deps.postTicketComment(ticket.trackerInternalId, g0.detail ?? 'G0 readiness gate rejected this ticket');
        } catch (err) {
          log.warn('clarify', `Failed to post G0 rejection comment: ${(err as Error).message}`);
        }
      }
      outcomes.push({ stage: 'failed', reason: `G0 readiness gate rejected: ${g0.detail ?? 'score below threshold'}` });
      return outcomes;
    }
  }

  // Spec check (FR-TICKET-05)
  const spec = checkSpec(ticket);
  if (!spec.sufficient && spec.question) {
    log.warn('clarify', 'Insufficient specification', {});
    outcomes.push({ stage: 'clarify', question: spec.question });
    if (ticket.trackerInternalId && deps.postTicketComment) {
      try {
        await deps.postTicketComment(ticket.trackerInternalId, spec.question);
      } catch (err) {
        log.warn('clarify', `Failed to post clarification comment: ${(err as Error).message}`);
      }
    }
    return outcomes;
  }
  outcomes.push({ stage: 'plan', summary: plan.classification, tasks: plan.tasks });

  if (cfg.dryRun) {
    log.info('plan', 'Dry-run: stopping before implement', {});
    return outcomes;
  }

  // Stage: implement
  if (!deps.implementStage) {
    outcomes.push({ stage: 'failed', reason: 'no worker dispatch configured' });
    return outcomes;
  }
  const impl = await deps.implementStage(cfg, plan, log);
  outcomes.push({
    stage: 'implement',
    worker: impl.worker,
    worktreePath: impl.worktreePath,
    branch: impl.branch,
    attempts: impl.attempts,
    ok: impl.ok,
  });
  if (!impl.ok) {
    outcomes.push({ stage: 'failed', reason: 'worker failed to produce a diff within retry budget' });
    return outcomes;
  }

  // Stage: validate — G1 (tests) then G3 (migration static analysis)
  const gatePath = impl.worktreePath ?? cfg.repoPath;

  if (deps.runGateG1) {
    const g1 = await deps.runGateG1(gatePath, cfg.timeoutMs);
    log.info('validate', `G1 ${g1.skipped ? 'skipped' : g1.passed ? 'passed' : 'failed'}${g1.detail ? `: ${g1.detail.split('\n')[0]}` : ''}`, {});
    outcomes.push({ stage: 'validate', passed: g1.passed });
    if (!g1.passed) {
      outcomes.push({ stage: 'failed', reason: `test gate failed: ${g1.detail ?? 'no detail'}` });
      return outcomes;
    }
  }

  if (deps.runGateG2 && plan.classification === 'migration-required') {
    const g2 = await deps.runGateG2(gatePath, cfg.timeoutMs);
    log.info('validate', `G2 ${g2.skipped ? 'skipped' : g2.passed ? 'passed' : 'failed'}${g2.detail ? `: ${g2.detail.split(';')[0]}` : ''}`, {});
    outcomes.push({ stage: 'validate', passed: g2.passed });
    if (!g2.passed) {
      outcomes.push({ stage: 'failed', reason: `migration apply gate failed: ${g2.detail ?? 'no detail'}` });
      return outcomes;
    }
  }

  const g3 = deps.runGateG3(gatePath, plan.classification);
  log.info('validate', `G3 ${g3.passed ? 'passed' : 'failed'}: ${g3.detail ?? ''}`, {});
  outcomes.push({ stage: 'validate', passed: g3.passed });
  if (!g3.passed) {
    outcomes.push({ stage: 'failed', reason: 'migration static gate failed' });
    return outcomes;
  }

  // Stage: validate — G4 async review for consumer-classified tickets
  if (deps.runGateG4 && plan.classification === 'consumer-only') {
    const g4 = await deps.runGateG4(gatePath);
    log.info('validate', `G4 ${g4.passed ? 'passed' : 'failed'}: ${g4.detail ?? ''}`, {});
    outcomes.push({ stage: 'validate', passed: g4.passed });
    if (!g4.passed) {
      outcomes.push({ stage: 'failed', reason: `async review gate failed: ${g4.detail ?? 'no detail'}` });
      return outcomes;
    }
  }

  // Stage: publish — headless mode publishes; interactive mode defers to the caller
  if (cfg.autoPr && deps.publishStage) {
    try {
      const prUrl = await deps.publishStage(cfg, plan, impl);
      log.info('publish', `PR opened: ${prUrl ?? 'unknown'}`, {});
      outcomes.push({ stage: 'publish', prUrl, note: 'auto-pr enabled' });
    } catch (err) {
      log.error('publish', `PR creation failed: ${(err as Error).message}`);
      outcomes.push({ stage: 'failed', reason: `PR creation failed: ${(err as Error).message}` });
      return outcomes;
    }
  } else {
    outcomes.push({ stage: 'publish', note: cfg.autoPr ? 'no publisher configured' : 'awaiting approval' });
  }
  return outcomes;
}
