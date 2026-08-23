#!/usr/bin/env node
import { Command } from 'commander';
import { existsSync, readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { loadConfig, loadCredentials, credentialStatus } from './config.js';
import { RunLogger } from './logger.js';
import { runPipeline } from './pipeline.js';
import { buildDeps, buildDryRunDeps } from './deps.js';

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

    // Dry-run plans offline against a synthetic ticket; no credentials needed
    if (!cfg.dryRun && !creds.linearApiKey) {
      console.error('LINEAR_API_KEY is not set. Ticket fetching is unavailable.');
      process.exitCode = 1;
      return;
    }

    // Tracker selection: Jira when JIRA_* env present, else Linear
    const useJira = Boolean(process.env.JIRA_DOMAIN && process.env.JIRA_EMAIL && process.env.JIRA_API_TOKEN);
    const fetchForTracker = async (id: string) => {
      if (useJira) {
        const { fetchJiraTicket } = await import('./integrations/jira.js');
        return fetchJiraTicket(id, {
          domain: process.env.JIRA_DOMAIN!,
          email: process.env.JIRA_EMAIL!,
          apiToken: process.env.JIRA_API_TOKEN!,
        });
      }
      const { fetchTicket } = await import('./integrations/linear.js');
      return fetchTicket(id, creds.linearApiKey!);
    };

    logger.info('fetch', `Run ${logger.runId} starting`, { ticket: cfg.ticketId, worker: cfg.worker, dryRun: cfg.dryRun, tracker: useJira ? 'jira' : 'linear' });

    try {
      const deps = cfg.dryRun ? buildDryRunDeps(cfg.ticketId) : buildDeps(creds, cfg, logger);
      const outcomes = await runPipeline(cfg, deps, logger);
      printOutcomes(outcomes);
      console.log(`Run log: ${logger.path}`);
    } catch (err) {
      logger.error('fetch', (err as Error).message);
      console.error((err as Error).message);
      process.exitCode = 1;
    }
  });

program
  .command('fleet')
  .description('Run tickets across multiple repositories with bounded concurrency')
  .requiredOption('--ticket <ids...>', 'ticket identifiers (one or more)')
  .requiredOption('--repo <entries...>', 'repo entries as name=path (one or more)')
  .option('--concurrency <n>', 'max parallel runs', Number, 2)
  .option('--worker <name>', 'claude-code | opencode | both')
  .option('--auto-pr', 'publish PRs without approval gates', false)
  .option('--max-loops <n>', 'test-failure retry budget', Number)
  .action(async (opts) => {
    const config = loadConfig(process.cwd());

    const entries = [];
    for (const raw of opts.repo as string[]) {
      const eq = raw.indexOf('=');
      if (eq <= 0) {
        console.error(`Invalid --repo entry "${raw}" (expected name=path)`);
        process.exitCode = 1;
        return;
      }
      entries.push({ name: raw.slice(0, eq), path: raw.slice(eq + 1) });
    }

    const creds = loadCredentials();
    if (!creds.linearApiKey) {
      console.error('LINEAR_API_KEY is not set.');
      process.exitCode = 1;
      return;
    }

    const { runFleet } = await import('./fleet.js');
    const result = await runFleet({
      ticketIds: opts.ticket as string[],
      entries,
      concurrency: opts.concurrency,
      timeoutMs: config.timeoutMinutes * 60_000,
      worker: (opts.worker ?? config.worker) as 'claude-code' | 'opencode' | 'both',
      autoPr: opts.autoPr ?? false,
      maxLoops: opts.maxLoops ?? config.maxLoops,
      runOne: async ({ repoPath, ticketId, worker, autoPr, maxLoops, timeoutMs, log }) => {
        const cfg = {
          ticketId,
          repoPath,
          worker,
          autoPr,
          interactive: !autoPr,
          maxLoops,
          timeoutMs,
          dryRun: false,
        };
        const outcomes = await runPipeline(cfg, buildDeps(creds, cfg, log), log);
        const failed = outcomes.find((o) => o.stage === 'failed') as { reason?: string } | undefined;
        return { ok: !failed, summary: failed?.reason ?? 'completed' };
      },
    });

    console.log('\nFleet results:');
    for (const item of result.items) {
      console.log(`  ${item.ok ? '✓' : '✗'} ${item.entry}/${item.ticketId}: ${item.summary}${item.logPath ? ` (log ${item.logPath})` : ''}`);
    }
    console.log(`${result.succeeded} succeeded, ${result.failed} failed`);
    if (result.failed > 0) process.exitCode = 1;
  });

program
  .command('serve')
  .description('Run the webhook receiver HTTP server (FR-TICKET-04)')
  .option('--port <n>', 'listen port', Number, 8080)
  .option('--repo <path>', 'target repository for dispatched runs', process.cwd())
  .action(async (opts) => {
    const creds = loadCredentials();
    if (!process.env.LINEAR_WEBHOOK_SECRET) {
      console.error('LINEAR_WEBHOOK_SECRET is not set.');
      process.exitCode = 1;
      return;
    }
    if (!creds.linearApiKey) {
      console.error('LINEAR_API_KEY is not set.');
      process.exitCode = 1;
      return;
    }
    const config = loadConfig(opts.repo);

    const { createServer } = await import('node:http');
    const { handleWebhook, DeliveryDedup, parseAgentSessionEvent, parseGithubIssueEvent } = await import('./server/webhook.js');
    const dedup = new DeliveryDedup();

    const server = createServer((req, res) => {
      if (req.method !== 'POST' || !req.url?.startsWith('/webhooks/linear')) {
        res.statusCode = 404;
        res.end();
        return;
      }
      handleWebhook(req, res, {
        signingSecret: process.env.LINEAR_WEBHOOK_SECRET!,
        dedup,
        onEvent: (v) => {
          const linearDispatch = parseAgentSessionEvent(v.payload);
          const githubDispatch = linearDispatch ? null : parseGithubIssueEvent(v.githubEvent, v.payload);
          const ticketId = linearDispatch?.issueIdentifier ?? githubDispatch?.issueIdentifier;
          if (!ticketId) return; // not an event we act on
          void dispatchRun(ticketId, creds, config).catch(() => {
            // logged inside dispatchRun
          });
        },
      });
    });

    server.listen(opts.port, () => console.log(`Listening on :${opts.port} — POST /webhooks/linear`));
  });

/** Fire-and-forget pipeline execution for webhook-dispatched tickets. */
async function dispatchRun(
  ticketId: string,
  creds: ReturnType<typeof loadCredentials>,
  config: ReturnType<typeof loadConfig>,
): Promise<void> {
  // Latest-wins dedup: skip if a run for this ticket is already active
  const { tryAcquireRun } = await import('./runregistry.js');
  const home = process.env.DEVAGENT_HOME || join(process.env.HOME || '.', '.devagent');
  const lock = tryAcquireRun(home, ticketId);
  if (!lock) {
    console.log(`Run for ${ticketId} already active; skipping duplicate trigger`);
    return;
  }

  const logger = new RunLogger();
  logger.info('fetch', `Webhook-dispatched run ${logger.runId} starting`, { ticket: ticketId });
  try {
    const cfg = {
      ticketId,
      repoPath: process.cwd(),
      worker: config.worker,
      autoPr: false,
      interactive: true,
      maxLoops: config.maxLoops,
      timeoutMs: config.timeoutMinutes * 60_000,
      dryRun: false,
    };
    const outcomes = await runPipeline(cfg, buildDeps(creds, cfg, logger), logger);
    printOutcomes(outcomes);
  } catch (err) {
    logger.error('fetch', `Dispatch failed: ${(err as Error).message}`);
  } finally {
    lock.release();
  }
}

function printOutcomes(outcomes: Array<{ stage: string } & Record<string, unknown>>): void {
  for (const o of outcomes) {
    switch (o.stage) {
      case 'plan':
        console.log(`Plan (${o.summary}):`);
        for (const t of o.tasks as string[]) console.log(`  - ${t}`);
        break;
      case 'clarify':
        console.log(`Needs clarification: ${o.question}`);
        break;
      case 'implement':
        console.log(`Implement (${o.worker}): ${o.ok ? 'ok' : 'failed'} after ${o.attempts} attempt(s)`);
        break;
      case 'validate':
        console.log(`Validation gate: ${o.passed ? 'PASSED' : 'FAILED'}`);
        break;
      case 'publish':
        if (o.prUrl) console.log(`PR opened: ${o.prUrl}`);
        else console.log(`Publish: ${o.note}`);
        break;
      case 'failed':
        console.error(`Run failed: ${o.reason}`);
        process.exitCode = 1;
        break;
      default:
        break;
    }
  }
}

program
  .command('validate')
  .description('Run all applicable gates against a repository or worktree')
  .option('--worktree <path>', 'path to repository/worktree', process.cwd())
  .action(async (opts) => {
    const { runMigrationStaticGate } = await import('./validation/runner.js');
    const { runTestGate } = await import('./validation/test-gate.js');

    const probe = runMigrationStaticGate({ repoPath: opts.worktree, classification: 'endpoint-only' });
    const hasMigrations =
      probe.findings.length > 0 || probe.detail !== 'skipped: no migrations in this ticket';

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
  .command('dashboard')
  .description('Generate a static HTML status board from run logs')
  .action(async () => {
    const { writeDashboard } = await import('./observe.js');
    const home = process.env.DEVAGENT_HOME || join(process.env.HOME || '.', '.devagent');
    const { path, runs } = writeDashboard(home);
    console.log(`${runs} run(s) -> ${path}`);
  });

program
  .command('clean')
  .description('Remove run worktrees older than the cutoff (default 7 days)')
  .option('--repo <path>', 'target repository', process.cwd())
  .option('--older-than <days>', 'age cutoff in days', Number, 7)
  .action(async (opts) => {
    const { findStaleWorktrees } = await import('./maintenance.js');
    const { removeWorktree } = await import('./git/worktree.js');
    const stale = findStaleWorktrees(opts.repo, (opts.olderThan as number) * 86_400_000);
    if (stale.length === 0) {
      console.log('No stale worktrees.');
      return;
    }
    for (const wt of stale) {
      const ticketKey = wt.path.split('/').pop()!;
      try {
        await removeWorktree(opts.repo, ticketKey);
        console.log(`removed ${wt.path} (${Math.round(wt.ageMs / 86_400_000)}d old)`);
      } catch (err) {
        console.error(`failed to remove ${wt.path}: ${(err as Error).message}`);
        process.exitCode = 1;
      }
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
