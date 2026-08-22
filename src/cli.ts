#!/usr/bin/env node
import { Command } from 'commander';
import { loadConfig, loadCredentials, credentialStatus } from './config.js';
import { RunLogger } from './logger.js';
import { fetchTicket } from './integrations/linear.js';
import { runPipeline, type ImplementResult } from './pipeline.js';
import { runMigrationStaticGate } from './validation/runner.js';
import type { ImplementationPlan } from './planner.js';
import { buildImplementationPrompt } from './prompt.js';
import { getWorker } from './workers/index.js';
import type { WorkerName } from './types.js';

/** Dispatch a worker inside an isolated worktree with the retry loop (FR-IMPL-01..04). */
async function implementStage(
  cfg: { repoPath: string; maxLoops: number; timeoutMs: number; worker: WorkerName | 'both' },
  plan: ImplementationPlan,
  log: RunLogger,
): Promise<ImplementResult> {
  const workerName: WorkerName = cfg.worker === 'both' ? 'claude-code' : cfg.worker;
  const worker = getWorker(workerName);
  const prompt = buildImplementationPrompt(plan);
  const maxAttempts = Math.max(1, cfg.maxLoops);

  for (let attempt = 1; attempt <= maxAttempts; attempt++) {
    log.info('implement', `Worker ${workerName} attempt ${attempt}/${maxAttempts}`, {});
    const result = await worker.spawn({ prompt, cwd: cfg.repoPath, timeoutMs: cfg.timeoutMs });
    log.info('implement', `Attempt ${attempt} finished`, {
      exitCode: result.exitCode,
      timedOut: result.timedOut,
      durationMs: result.durationMs,
      events: result.events.length,
    });
    if (!result.timedOut && result.exitCode === 0) {
      return { ok: true, worker: workerName, attempts: attempt };
    }
  }
  return { ok: false, worker: workerName, attempts: maxAttempts };
}

const program = new Command();

program
  .name('devagent')
  .description('Autonomous backend delivery agent: ticket to tested PR')
  .version('0.1.0');

program
  .command('run')
  .description('Execute the full pipeline for one ticket')
  .requiredOption('--ticket <id>', 'tracker ticket identifier')
  .option('--repo <path>', 'target repository', process.cwd())
  .option('--worker <name>', 'claude-code | opencode | both')
  .option('--auto-pr', 'skip approval gates', false)
  .option('--interactive', 'pause at human gates')
  .option('--max-loops <n>', 'test-failure retry budget', Number)
  .option('--timeout <minutes>', 'wall-clock cap per run', Number)
  .option('--dry-run', 'plan only; no workers, no remotes', false)
  .action(async (opts) => {
    const config = loadConfig(opts.repo);
    const creds = loadCredentials();
    const logger = new RunLogger();

    const cfg = {
      ticketId: opts.ticket,
      repoPath: opts.repo,
      worker: opts.worker ?? config.worker,
      autoPr: opts.autoPr ?? false,
      interactive: opts.interactive ?? !opts.autoPr,
      maxLoops: opts.maxLoops ?? config.maxLoops,
      timeoutMs: (opts.timeout ?? config.timeoutMinutes) * 60_000,
      dryRun: opts.dryRun ?? false,
    };

    if (!creds.linearApiKey) {
      console.error('LINEAR_API_KEY is not set. Ticket fetching is unavailable.');
      process.exitCode = 1;
      return;
    }

    logger.info('fetch', `Run ${logger.runId} starting`, { ticket: cfg.ticketId, worker: cfg.worker, dryRun: cfg.dryRun });

    try {
      const outcomes = await runPipeline(
        cfg,
        {
          fetchTicket: (id) => fetchTicket(id, creds.linearApiKey!),
          postTicketComment: (internalId, comment) =>
            import('./integrations/linear.js').then((m) => m.postTicketComment(internalId, comment, creds.linearApiKey!)),
          runGateG3: (repoPath, classification) => {
            const r = runMigrationStaticGate({ repoPath, classification });
            return { passed: r.passed, findings: r.findings, detail: r.detail };
          },
          implementStage: (c, plan, lg) =>
            implementStage({ repoPath: c.repoPath, maxLoops: c.maxLoops, timeoutMs: c.timeoutMs, worker: c.worker }, plan, lg),
        },
        logger,
      );

      for (const o of outcomes) {
        switch (o.stage) {
          case 'plan':
            console.log(`Plan (${o.summary}):`);
            for (const t of o.tasks) console.log(`  - ${t}`);
            break;
          case 'clarify':
            console.log(`Needs clarification: ${o.question}`);
            break;
          case 'implement':
            console.log(`Implement (${o.worker}): ${o.ok ? 'ok' : 'failed'} after ${o.attempts} attempt(s)`);
            break;
          case 'validate':
            console.log(`Validation: ${o.passed ? 'PASSED' : 'FAILED'}`);
            break;
          case 'failed':
            console.error(`Run failed: ${o.reason}`);
            process.exitCode = 1;
            break;
          default:
            console.log(`${o.stage}: ${'note' in o ? o.note : ''}`);
        }
      }
      console.log(`Run log: ${logger.path}`);
    } catch (err) {
      logger.error('fetch', (err as Error).message);
      console.error((err as Error).message);
      process.exitCode = 1;
    }
  });

program
  .command('config')
  .description('Show effective configuration and credential presence (never values)')
  .action(() => {
    const config = loadConfig(process.cwd());
    const status = credentialStatus(loadCredentials());
    console.log(JSON.stringify({ config, credentials: status }, null, 2));
  });

program.parseAsync();
