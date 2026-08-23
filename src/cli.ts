#!/usr/bin/env node
import { Command } from 'commander';
import { existsSync, readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { loadConfig, loadCredentials, credentialStatus } from './config.js';
import { RunLogger } from './logger.js';
import { runPipeline } from './pipeline.js';
import { buildDeps, buildDryRunDeps } from './deps.js';
import type { WorkerName } from './types.js';

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

    // Latest-wins dedup: one active run per ticket across processes
    const { tryAcquireRun } = await import('./runregistry.js');
    const home = process.env.DEVAGENT_HOME || join(process.env.HOME || '.', '.devagent');
    const lock = tryAcquireRun(home, cfg.ticketId);
    if (!lock) {
      console.error(`Run for ${cfg.ticketId} already active`);
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
    } finally {
      lock.release();
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
  .command('task')
  .description('Run one prompt-driven task headlessly (orchestrator integration mode)')
  .requiredOption('--prompt <text>', 'task description (first line becomes the title)')
  .option('--repo <path>', 'target repository', process.cwd())
  .option('--worker <name>', 'claude-code | opencode | both')
  .option('--auto-pr', 'push branch and open PR when green', false)
  .option('--max-loops <n>', 'test-failure retry budget', Number)
  .action(async (opts) => {
    const config = loadConfig(opts.repo);
    const creds = loadCredentials();
    const logger = new RunLogger();

    const cfg = {
      prompt: opts.prompt as string,
      repoPath: opts.repo,
      autoPr: opts.autoPr ?? false,
      maxLoops: opts.maxLoops ?? config.maxLoops,
      timeoutMs: config.timeoutMinutes * 60_000,
      log: logger,
    };
    logger.info('task', `Task run ${logger.runId} starting`, { repo: cfg.repoPath, autoPr: cfg.autoPr });

    try {
      const taskMod = await import('./task.js');
      const { runTask } = taskMod;
      type TaskDeps = import('./task.js').TaskDeps;
      const { implementStage } = await import('./deps.js');
      const { runMigrationStaticGate } = await import('./validation/runner.js');

      const workerName = ((opts.worker ?? config.worker) as 'claude-code' | 'opencode' | 'both');
      const deps: TaskDeps = {
        runPipelineDeps: {
          fetchTicket: async () => ({ id: 'TASK', title: '', description: '', labels: [], acceptanceCriteria: [] }),
          runGateG3: (rp, classification) => {
            const r = runMigrationStaticGate({ repoPath: rp, classification });
            return { passed: r.passed, findings: r.findings, detail: r.detail };
          },
        },
        implementStage: async (c, ticket, lg) => {
          const plan = { ticket, classification: 'endpoint-only' as const, tasks: [], summary: ticket.title };
          return implementStage(
            { repoPath: c.repoPath, maxLoops: c.maxLoops, timeoutMs: c.timeoutMs, worker: workerName, autoPr: c.autoPr },
            plan,
            lg,
          );
        },
        publishStage: async (c, _ticket, impl) => {
          if (!impl.worktreePath || !creds.githubToken) return undefined;
          const branch = `devagent/task-${logger.runId.slice(0, 8)}`;
          const { pushBranch, createPr } = await import('./integrations/github.js');
          await pushBranch(impl.worktreePath, branch);
          void loadConfig(c.repoPath).githubBaseBranch; // base defaults via gh when omitted
          return createPr({
            repoPath: c.repoPath,
            branch,
            title: cfg.prompt.split('\n')[0]!.slice(0, 80),
            body: `Automated task via \`devagent task\`.\n\n## Prompt\n${cfg.prompt}`,
          });
        },
      };

      const result = await runTask(cfg, deps);
      console.log(result.note);
      if (!result.ok) process.exitCode = 1;
      console.log(`Run log: ${logger.path}`);
    } catch (err) {
      console.error((err as Error).message);
      process.exitCode = 1;
    }
  });

program
  .command('orchestrate')
  .description('Planner decomposes a goal into tasks; executors implement them in dependency waves')
  .requiredOption('--goal <text>', 'product goal to decompose and implement')
  .option('--repo <path>', 'target repository', process.cwd())
  .option('--planner <name>', 'planner worker (default from config)')
  .option('--executor <name>', 'executor worker (default from config)')
  .option('--concurrency <n>', 'parallel executor slots', Number, 2)
  .option('--max-task-retries <n>', 'scheduler retry budget per task', Number, 1)
  .option('--resume', 'continue an existing board instead of re-planning', false)
  // NOTE: no explicit default — commander negates --no-merge to opts.merge=true
  .option('--no-merge', 'skip merge-back even when all tasks are done')
  .action(async (opts) => {
    const cfg = { dryRunMerge: opts.merge === false };
    const config = loadConfig(opts.repo);
    const logger = new RunLogger();
    logger.info('task', `Orchestration run ${logger.runId} starting`, { repo: opts.repo });

    try {
      const { loadBoard, saveBoard, createBoard } = await import('./orchestrator/store.js');
      const { runScheduler } = await import('./orchestrator/scheduler.js');
      const { executeTask } = await import('./orchestrator/executor.js');

      const plannerName = (opts.planner ?? config.worker) as WorkerName;
      const executorName = (opts.executor ?? config.worker) as WorkerName;
      const timeoutMs = config.timeoutMinutes * 60_000;

      let board = opts.resume ? loadBoard(opts.repo) : null;
      if (!board) {
        if (opts.resume) console.error('No existing board found; planning fresh.');
        const { runPlanner } = await import('./orchestrator/planner.js');
        const tasks = await runPlanner(opts.goal, opts.repo, plannerName, timeoutMs);
        board = createBoard(opts.goal, tasks, { planner: plannerName, executor: executorName });
        saveBoard(opts.repo, board);
        console.log(`Plan (${tasks.length} task(s)):`);
        for (const t of tasks) {
          console.log(`  ${t.id}: ${t.title}${t.dependsOn.length ? ` (after ${t.dependsOn.join(',')})` : ''}`);
        }
      }

      const result = await runScheduler(
        board,
        {
          repoPath: opts.repo,
          executor: executorName,
          concurrency: opts.concurrency,
          maxTaskRetries: opts.maxTaskRetries,
          timeoutMs,
          // persist after every wave so resume never re-runs done work
          onWavePersisted: (b) => saveBoard(opts.repo, b),
        },
        { executeTask: (a) => executeTask({ ...a, executor: executorName }) },
        logger,
      );
      saveBoard(opts.repo, result);

      const done = result.tasks.filter((t) => t.status === 'done').length;
      const failed = result.tasks.filter((t) => t.status === 'failed').length;
      const blocked = result.tasks.filter((t) => t.status === 'blocked').length;
      console.log(`\nProject: ${done}/${result.tasks.length} done, ${failed} failed, ${blocked} blocked`);
      for (const t of result.tasks) {
        console.log(`  [${t.status}] ${t.id}: ${t.title}${t.failureDetail ? ` — ${t.failureDetail.slice(0, 100)}` : ''}`);
      }
      if (failed > 0 || blocked > 0) process.exitCode = 1;
      // resume hint for the next session
      console.log(`Board: ${opts.repo}/.devagent-project.json (resume with --resume)`);
      console.log(`Run log: ${logger.path}`);

      // Merge-back when every task is done and not a dry inspection
      const allDone = result.tasks.length > 0 && result.tasks.every((t) => t.status === 'done');
      if (!allDone || cfg.dryRunMerge) return;

      const baseBranch = loadConfig(opts.repo).githubBaseBranch ?? 'main';
      console.log(`\nAll tasks done — merging into ${baseBranch}...`);
      const { mergeProjectBranches } = await import('./orchestrator/merge.js');
      const mr = await mergeProjectBranches(opts.repo, result, baseBranch, logger);
      if (mr.ok) {
        console.log(`Integrated: ${mr.merged.join(', ')}`);
      } else {
        console.error(`Integration failed at ${mr.failure!.taskId} (${mr.failure!.stage}): ${mr.failure!.detail}`);
        console.error('Board preserved; fix and re-run with --resume.');
        process.exitCode = 1;
      }
    } catch (err) {
      console.error((err as Error).message);
      process.exitCode = 1;
    }
  });
program
  .command('project')
  .description('Show orchestrator project board status for a repository')
  .option('--repo <path>', 'target repository', process.cwd())
  .action(async (opts) => {
    const { loadBoard } = await import('./orchestrator/store.js');
    const board = loadBoard(opts.repo);
    if (!board) {
      console.log('No project board. Start one: devagent orchestrate --goal "..."');
      return;
    }
    const counts = new Map<string, number>();
    for (const t of board.tasks) counts.set(t.status, (counts.get(t.status) ?? 0) + 1);
    const bar = Object.fromEntries([...counts].map(([k, v]) => [k, v]));
    console.log(`Goal: ${board.goal}`);
    console.log(`Roles: planner=${board.roles.planner} executor=${board.roles.executor}`);
    console.log(`Tasks: ${board.tasks.length} (${Object.entries(bar).map(([k, v]) => `${k}:${v}`).join(' ')})`);
    for (const t of board.tasks) {
      const mark = t.status === 'done' ? '✓' : t.status === 'failed' ? '✗' : t.status === 'dispatched' ? '▶' : t.status === 'blocked' ? '⛔' : '·';
      console.log(` ${mark} [${t.status}] ${t.id}: ${t.title}${t.failureDetail ? ` — ${t.failureDetail.slice(0, 80)}` : ''}`);
    }
    console.log(`Updated: ${board.updatedAt}`);
    const allDone = board.tasks.length > 0 && board.tasks.every((t) => t.status === 'done');
    if (allDone) console.log('Ready to integrate: devagent orchestrate --goal "" --resume');
    else console.log('Resume: devagent orchestrate --goal "" --resume');
  });

program
  .command('mcp')
  .description('Expose DevAgent as MCP tools over stdio (devagent_dispatch/status/log)')
  .action(async () => {
    const { startMcpServer } = await import('./server/mcp.js');
    startMcpServer();
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
  .command('guard')
  .description(
    'Run a headless Claude Code session with auto-resume on API failure (args after --)',
  )
  .option('--resume-prompt <text>', 'prompt sent when resuming', 'Continue')
  .option('--max-attempts <n>', 'total launches including the first', Number, 5)
  .option('--base-delay-ms <n>', 'first resume backoff', Number, 2_000)
  .option('--max-delay-ms <n>', 'backoff ceiling', Number, 60_000)
  .option(
    '--no-progress-timeout-ms <n>',
    'kill + resume when the child streams nothing for this long (0 disables)',
    Number,
    0,
  )
  .argument('<claudeArgs...>', 'claude invocation, e.g. -- claude -p "task"')
  .action(async (claudeArgs: string[], opts) => {
    const { runGuard } = await import('./sessionguard/guard.js');
    const { spawnClaude } = await import('./sessionguard/spawn.js');
    const result = await runGuard({
      argv: claudeArgs,
      resumePrompt: opts.resumePrompt as string,
      maxAttempts: opts.maxAttempts as number,
      backoff: {
        baseDelayMs: opts.baseDelayMs as number,
        maxDelayMs: opts.maxDelayMs as number,
      },
      noProgressTimeoutMs: opts.noProgressTimeoutMs as number,
      log: (message) => console.error(message),
      onLine: (line, stream) => (stream === 'stdout' ? process.stdout.write(line + '\n') : console.error(line)),
      runner: spawnClaude,
    });
    if (!result.ok) {
      console.error(`[cc-guard] gave up after ${result.attempts} attempt(s): ${result.reason}${result.lastError ? ` — ${result.lastError}` : ''}`);
      process.exitCode = 1;
    } else {
      console.error(`[cc-guard] completed after ${result.attempts} attempt(s), resumed ${result.resumed} time(s)`);
    }
  });

program
  .command('guard-status')
  .description(
    'Check whether the latest Claude Code session for a project ended on an API error',
  )
  .option('--project-dir <path>', 'working directory to derive the project slug from', process.cwd())
  .option('--resume-prompt <text>', 'prompt used when resuming an interrupted session', 'Continue')
  .option(
    '--resume',
    'if the session is interrupted, resume it headlessly via devagent guard',
    false,
  )
  .action(async (opts) => {
    const { claudeProjectsDir, latestTranscript, inspectTranscript, projectSlug } =
      await import('./sessionguard/transcript.js');
    const projects = claudeProjectsDir(process.env.HOME || '.');
    const dir = join(projects, projectSlug(opts.projectDir as string));
    let file;
    try {
      file = latestTranscript(dir);
    } catch {
      console.error(`No transcript directory at ${dir}`);
      process.exitCode = 1;
      return;
    }
    if (!file) {
      console.error(`No transcripts found in ${dir}`);
      process.exitCode = 1;
      return;
    }
    const status = inspectTranscript(file);
    if (status.interrupted) {
      console.log(
        `INTERRUPTED session ${status.sessionId} (${status.file})\nlast error: ${status.lastErrorText ?? 'unknown'}\nresume with: claude --resume ${status.sessionId}`,
      );
      if (opts.resume && status.sessionId) {
        const { runGuard } = await import('./sessionguard/guard.js');
        const { spawnClaude } = await import('./sessionguard/spawn.js');
        const result = await runGuard({
          argv: ['claude', '--resume', status.sessionId, '-p', opts.resumePrompt as string],
          resumePrompt: opts.resumePrompt as string,
          runner: spawnClaude,
          log: (message) => console.error(message),
          onLine: (line, stream) =>
            stream === 'stdout' ? process.stdout.write(line + '\n') : console.error(line),
        });
        if (!result.ok) {
          console.error(`[cc-guard] resume failed after ${result.attempts} attempt(s): ${result.reason}`);
          process.exitCode = 1;
        } else {
          console.error(`[cc-guard] session ${status.sessionId} resumed and completed`);
        }
      } else {
        process.exitCode = 1;
      }
    } else {
      console.log(
        `OK session ${status.sessionId} (${status.file}) last activity ${status.lastTimestamp ?? 'unknown'}`,
      );
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
