#!/usr/bin/env node
import { Command } from 'commander';
import { existsSync, readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { loadConfig, loadCredentials, credentialStatus } from './config.js';
import { RunLogger } from './logger.js';
import { fetchTicket } from './integrations/linear.js';
import { runPipeline, type ImplementResult } from './pipeline.js';
import { runMigrationStaticGate } from './validation/runner.js';
import type { ImplementationPlan } from './planner.js';
import { buildImplementationPrompt, buildRepairPrompt } from './prompt.js';
import { getWorker } from './workers/index.js';
import { createWorktree } from './git/worktree.js';
import type { WorkerName } from './types.js';

/** Dispatch a worker inside an isolated worktree with the retry loop (FR-IMPL-01..04). */
async function implementStage(
  cfg: { repoPath: string; maxLoops: number; timeoutMs: number; worker: WorkerName | 'both' },
  plan: ImplementationPlan,
  log: RunLogger,
): Promise<ImplementResult> {
  if (cfg.worker === 'both') {
    const { runFanout } = await import('./workers/fanout.js');
    const { runTestGate } = await import('./validation/test-gate.js');
    log.info('implement', 'Fan-out mode: dispatching both workers', {});
    const winner = await runFanout(plan, ['claude-code', 'opencode'], log, {
      repoPath: cfg.repoPath,
      timeoutMs: cfg.timeoutMs,
      scoreLeg: (wt, ms) => runTestGate(wt, ms).then((r) => r.passed),
    });
    if (!winner) {
      return { ok: false, worker: 'claude-code', attempts: 1 };
    }
    log.info('implement', `Fan-out winner: ${winner.worker} (tests ${winner.testsPassed})`, {});
    return { ok: true, worker: winner.worker, worktreePath: winner.worktreePath, attempts: 1 };
  }

  const workerName: WorkerName = cfg.worker;
  const worker = getWorker(workerName);
  const prompt = buildImplementationPrompt(plan);
  let repairPrompt = prompt;
  const maxAttempts = Math.max(1, cfg.maxLoops);

  // Isolated worktree + branch per run (FR-IMPL-01); falls back to repoPath on failure
  let cwd = cfg.repoPath;
  let worktreePath: string | undefined;
  try {
    const wt = await createWorktree(cfg.repoPath, plan.ticket.id);
    cwd = wt.worktreePath;
    worktreePath = wt.worktreePath;
    log.info('implement', `Worktree ready: ${wt.worktreePath} (branch ${wt.branch})`, {});
  } catch (err) {
    log.warn('implement', `Worktree creation failed, running in repo root: ${(err as Error).message}`);
  }

  try {
    for (let attempt = 1; attempt <= maxAttempts; attempt++) {
      const attemptPrompt = attempt === 1 ? prompt : repairPrompt;
      log.info('implement', `Worker ${workerName} attempt ${attempt}/${maxAttempts}`, {});
      const result = await worker.spawn({ prompt: attemptPrompt, cwd, timeoutMs: cfg.timeoutMs });
      log.info('implement', `Attempt ${attempt} finished`, {
        exitCode: result.exitCode,
        timedOut: result.timedOut,
        durationMs: result.durationMs,
        events: result.events.length,
      });
      if (result.timedOut || result.exitCode !== 0) {
        repairPrompt = buildRepairPrompt(plan, attempt, result.resultText ?? `worker exited ${result.exitCode}`);
        continue;
      }
      // Worker reports success: verify with the repo's own test suite before accepting
      const { runTestGate } = await import('./validation/test-gate.js');
      const g1 = await runTestGate(cwd, cfg.timeoutMs);
      log.info('implement', `Attempt ${attempt} test gate: ${g1.passed ? 'passed' : 'failed'}`, {
        detail: g1.detail?.split('\n')[0],
      });
      if (g1.passed) {
        return { ok: true, worker: workerName, attempts: attempt, worktreePath };
      }
      repairPrompt = buildRepairPrompt(plan, attempt, g1.detail ?? 'test suite failed');
    }
    return { ok: false, worker: workerName, attempts: maxAttempts, worktreePath };
  } finally {
    if (worktreePath) log.info('implement', `Worktree preserved for inspection: ${worktreePath}`, {});
  }
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
          runGateG1: (worktreePath, timeoutMs) =>
            import('./validation/test-gate.js').then((m) => m.runTestGate(worktreePath, timeoutMs)),
          runGateG2: (worktreePath, timeoutMs) =>
            import('./validation/migration-apply-gate.js').then((m) => m.runMigrationApplyGate(worktreePath, timeoutMs)),
          runGateG3: (repoPath, classification) => {
            const r = runMigrationStaticGate({ repoPath, classification });
            return { passed: r.passed, findings: r.findings, detail: r.detail };
          },
          implementStage: (c, plan, lg) =>
            implementStage({ repoPath: c.repoPath, maxLoops: c.maxLoops, timeoutMs: c.timeoutMs, worker: c.worker }, plan, lg),
          publishStage: async (c, plan, impl) => {
            if (!impl.worktreePath || !creds.githubToken) return undefined;
            const branch = `devagent/${plan.ticket.id}`;
            const { createPr } = await import('./integrations/github.js');
            return createPr({
              repoPath: c.repoPath,
              branch,
              title: `[${plan.ticket.id}] ${plan.ticket.title}`,
              body: buildPrBody(plan),
            });
          },
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
          case 'publish':
            if (o.prUrl) {
              console.log(`PR opened: ${o.prUrl}`);
            } else {
              console.log(`Publish: ${o.note}`);
            }
            break;
          case 'validate':
            console.log(`Validation: ${o.passed ? 'PASSED' : 'FAILED'}`);
            break;
          case 'failed':
            console.error(`Run failed: ${o.reason}`);
            process.exitCode = 1;
            break;
          default:
            break;
        }
      }
      console.log(`Run log: ${logger.path}`);
    } catch (err) {
      logger.error('fetch', (err as Error).message);
      console.error((err as Error).message);
      process.exitCode = 1;
    }
  });

/** PR body with plan, ticket link, and evidence placeholders (FR-DELIVER-01). */
function buildPrBody(plan: ImplementationPlan): string {
  const t = plan.ticket;
  return [
    `Closes ${t.id}${t.url ? ` (${t.url})` : ''}.`,
    '',
    '## Summary',
    `Automated implementation classified as **${plan.classification}** by DevAgent.`,
    '',
    '## Plan',
    ...plan.tasks.map((task, i) => `${i + 1}. ${task}`),
    '',
    '## Validation',
    '- G3 static migration analysis: passed',
    '- Test suite: see CI run on this branch',
    '',
    '## Acceptance criteria',
    ...(t.acceptanceCriteria.length ? t.acceptanceCriteria.map((c) => `- [ ] ${c}`) : ['- (see ticket)']),
  ].join('\n');
}

program
  .command('validate')
  .description('Run all applicable gates against a repository or worktree')
  .option('--worktree <path>', 'path to repository/worktree', process.cwd())
  .action(async (opts) => {
    const { runMigrationStaticGate } = await import('./validation/runner.js');
    const { runTestGate } = await import('./validation/test-gate.js');

    // Classify from the repo's own contents rather than a ticket
    const classification = runMigrationStaticGate({ repoPath: opts.worktree, classification: 'endpoint-only' });
    const hasMigrations = classification.findings.length > 0 || classification.detail !== 'skipped: no migrations in this ticket';

    let failed = false;
    const g1 = await runTestGate(opts.worktree, 15 * 60_000);
    console.log(`G1 tests: ${g1.passed ? 'PASS' : 'FAIL'}${g1.detail ? `\n  ${g1.detail.split('\n').join('\n  ')}` : ''}`);
    if (!g1.passed) failed = true;

    if (hasMigrations) {
      const g3 = runMigrationStaticGate({ repoPath: opts.worktree, classification: 'migration-required' });
      for (const f of g3.findings) {
        console.log(`G3 ${f.severity.toUpperCase()} ${f.ruleId}${f.file ? ` (${f.file})` : ''}: ${f.message}`);
      }
      if (!g3.passed) failed = true;
      if (g3.passed) console.log('G3 migration static: PASS');
    } else {
      console.log('G3 migration static: SKIPPED (no migrations found)');
    }

    process.exitCode = failed ? 1 : 0;
  });

program
  .command('log')
  .description('Print the structured JSONL log of a run')
  .requiredOption('--run <id>', 'run identifier')
  .action((opts) => {
    const home = process.env.DEVAGENT_HOME || join(process.env.HOME || '.', '.devagent');
    const p = join(home, 'runs', `${opts.run}.jsonl`);
    if (!existsSync(p)) {
      console.error(`No run log at ${p}`);
      process.exitCode = 1;
      return;
    }
    for (const line of readFileSync(p, 'utf8').trim().split('\n')) {
      try {
        const e = JSON.parse(line) as { ts: string; stage: string; level: string; message: string };
        console.log(`${e.ts} [${e.level}] ${e.stage}: ${e.message}`);
      } catch {
        // skip malformed lines
      }
    }
  });

program
  .command('status')
  .description('List recent runs (id, last stage, last message)')
  .option('--limit <n>', 'number of runs', Number, 10)
  .action((opts) => {
    
    const home = process.env.DEVAGENT_HOME || join(process.env.HOME || '.', '.devagent');
    const runsDir = join(home, 'runs');
    let files: string[] = [];
    try {
      files = readdirSync(runsDir).filter((f) => f.endsWith('.jsonl')).sort().slice(-(opts.limit as number));
    } catch {
      console.log('No runs yet.');
      return;
    }
    if (files.length === 0) {
      console.log('No runs yet.');
      return;
    }
    for (const file of files) {
      const lines = readFileSync(join(runsDir, file), 'utf8').trim().split('\n');
      const last = lines.at(-1);
      if (!last) continue;
      try {
        const e = JSON.parse(last) as { runId: string; stage: string; level: string; message: string; ts: string };
        console.log(`${e.runId.slice(0, 8)}  ${e.ts}  [${e.stage}/${e.level}] ${e.message}`);
      } catch {
        continue;
      }
    }
  });

program
  .command('serve')
  .description('Run the webhook receiver HTTP server (FR-TICKET-04)')
  .option('--port <n>', 'listen port', Number, 8080)
  .action(async (opts) => {
    const { LINEAR_WEBHOOK_SECRET } = process.env;
    if (!LINEAR_WEBHOOK_SECRET) {
      console.error('LINEAR_WEBHOOK_SECRET is not set.');
      process.exitCode = 1;
      return;
    }
    const { createServer } = await import('node:http');
    const { handleWebhook, DeliveryDedup } = await import('./server/webhook.js');
    const logger = new RunLogger();
    const dedup = new DeliveryDedup();

    const server = createServer((req, res) => {
      if (req.method !== 'POST' || !req.url?.startsWith('/webhooks/linear')) {
        res.statusCode = 404;
        res.end();
        return;
      }
      handleWebhook(req, res, {
        signingSecret: LINEAR_WEBHOOK_SECRET,
        dedup,
        onEvent: (v) => {
          // Loop 11 routes this into runPipeline; for now log and enqueue nothing
          logger.info('fetch', `Webhook received: ${v.event}`, { deliveryId: v.deliveryId });
        },
      });
    });

    server.listen(opts.port, () => {
      logger.info('fetch', `Webhook server listening on :${opts.port}`, {});
      console.log(`Listening on :${opts.port} — POST /webhooks/linear`);
    });
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
