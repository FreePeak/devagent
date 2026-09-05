#!/usr/bin/env node
import { Command } from 'commander';
import { existsSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { loadConfig, loadCredentials, credentialStatus, type CleanupMode } from './config.js';
import { RunLogger } from './logger.js';
import { ensureStateBranch } from './git/state-branch.js';
import { runInit, renderInitReport } from './commands/init.js';
import { buildStatusView, renderStatusCard, statusJson } from './commands/status.js';
import { buildProbeArgvFor } from './commands/probe-argv.js';
import { runPreflightGate, PREFLIGHT_ROLES, isPreflightRole } from './resilience/preflight.js';
import { runPipeline } from './pipeline.js';
import { buildDeps, buildDryRunDeps } from './deps.js';
import type { WorkerName } from './types.js';
import { DEVAGENT_VERSION } from './version.js';
import { appendReleaseRecord } from './orchestrator/ledger.js';

function parseConcurrency(v: string): number | 'auto' {
  if (v === 'auto' || v.toLowerCase() === 'auto') return 'auto';
  const n = Number(v);
  if (!Number.isFinite(n) || n < 1) throw new Error(`Invalid --concurrency "${v}"; expected positive integer or "auto"`);
  return Math.floor(n);
}

const program = new Command();

program
  .name('devagent')
  .description('Autonomous backend delivery agent: ticket to tested PR')
  .version(DEVAGENT_VERSION);

program
  .command('init')
  .description('Guided setup (§21 FR-SIMPLE-01): check prerequisites, write devagent.json with sane defaults, print a plain-language checklist')
  .option('--repo <path>', 'repository to set up', process.cwd())
  .option('--worker <name>', 'worker CLI to check and record (default omp; claude-code | opencode | omp | pi)')
  .option('--model <id>', 'model id to record (provider/model)')
  .action(async (opts) => {
    const result = await runInit({ repoPath: opts.repo as string, worker: opts.worker as string | undefined, model: opts.model as string | undefined });
    renderInitReport(result);
    if (!result.ok) process.exitCode = 1;
  });

program
  .command('run')
  .description('Execute the full pipeline for one ticket')
  .requiredOption('--ticket <id>', 'tracker ticket identifier')
  .option('--repo <path>', 'target repository', process.cwd())
  .option('--worker <name>', 'claude-code | opencode | omp | pi | both')
  .option('--model <id>', 'model override passed to the worker CLI (provider/model)')
  .option('--variant <name>', 'variant for opencode model (maps to --variant or #variant)')
  .option('--cleanup <mode>', "post-run worktree disposal: auto | keep | always")
  .option('--drop-orca-workspace', 'drop the enclosing Orca workspace after done (when repoPath is Orca-managed)', false)
  .option('--auto-pr', 'skip approval gates', false)
  .option('--interactive', 'pause at human gates')
  .option('--max-loops <n>', 'test-failure retry budget', Number)
  .option('--timeout <minutes>', 'wall-clock cap per run', Number)
  .option('--dry-run', 'plan only; no workers, no remotes', false)
  .option('--auto-merge', 'auto review + merge the PR once CI is green (default from config autoMerge)', false)
  .action(async (opts) => {
    const config = loadConfig(opts.repo);
    const creds = loadCredentials();
    const logger = new RunLogger();

    const cfg = {
      ticketId: opts.ticket,
      autoMerge: opts.autoMerge || config.autoMerge || undefined,
      repoPath: opts.repo,
      worker: opts.worker ?? config.worker,
      model: opts.model ?? config.model,
      variant: opts.variant ?? config.variant,
      autoPr: opts.autoPr ?? false,
      interactive: opts.interactive ?? !opts.autoPr,
      maxLoops: opts.maxLoops ?? config.maxLoops,
      timeoutMs: (opts.timeout ?? config.timeoutMinutes) * 60_000,
      dryRun: opts.dryRun ?? false,
      cleanup: resolveCleanup(opts.cleanup, config.cleanup),
      dropOrcaWorkspace: opts.dropOrcaWorkspace ?? config.dropOrcaWorkspace ?? false,
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
  .option('--concurrency <n>', 'max parallel runs (number or "auto")', parseConcurrency, 2)
  .option('--worker <name>', 'claude-code | opencode | omp | pi | both')
  .option('--cleanup <mode>', "post-run worktree disposal: auto | keep | always")
  .option('--drop-orca-workspace', 'drop the enclosing Orca workspace after done (when repoPath is Orca-managed)', false)
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
    let governorForFleet: import('./orchestrator/governor.js').ResourceGovernor | undefined;
    let fleetConcurrency: number | 'auto' = opts.concurrency as number | 'auto';
    if (fleetConcurrency === 'auto') {
      const { ResourceGovernor } = await import('./orchestrator/governor.js');
      governorForFleet = new ResourceGovernor();
      const snap = governorForFleet.getSnapshotSync();
      const eff = governorForFleet.effectiveAuto(snap);
      console.log(`[governor] fleet auto -> ${eff} (${governorForFleet.formatStatus('auto', eff, snap)})`);
    }
    const result = await runFleet({
      ticketIds: opts.ticket as string[],
      entries,
      concurrency: fleetConcurrency,
      ...(governorForFleet ? { governor: governorForFleet } : {}),
      timeoutMs: config.timeoutMinutes * 60_000,
      worker: (opts.worker ?? config.worker) as 'claude-code' | 'opencode' | 'omp' | 'pi' | 'both',
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
          cleanup: resolveCleanup(opts.cleanup, config.cleanup),
          dropOrcaWorkspace: opts.dropOrcaWorkspace ?? config.dropOrcaWorkspace ?? false,
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
      // Human-in-the-loop resume endpoint (LangGraph interrupt/resume pattern:
      // observation via devagent_board, decisions POSTed here). Token-gated.
      if (req.method === 'POST' && req.url?.startsWith('/api/answer')) {
        const token = process.env.DEVAGENT_ANSWER_TOKEN;
        if (!token || req.headers.authorization !== `Bearer ${token}`) {
          // 503 when unconfigured: the service refuses to run approvals open
          res.statusCode = token ? 401 : 503;
          res.setHeader('content-type', 'application/json');
          res.end(JSON.stringify({ ok: false, note: token ? 'unauthorized' : 'DEVAGENT_ANSWER_TOKEN not configured' }));
          return;
        }
        let raw = '';
        req.on('data', (c: Buffer) => {
          raw += c;
          if (raw.length > 1_000_000) req.destroy(); // bound the body
        });
        req.on('end', () => {
          let parsed: { repoPath?: unknown; taskId?: unknown; answer?: unknown };
          try {
            parsed = JSON.parse(raw) as typeof parsed;
          } catch {
            res.statusCode = 400;
            res.end(JSON.stringify({ ok: false, note: 'invalid JSON body' }));
            return;
          }
          if (typeof parsed.repoPath !== 'string' || typeof parsed.taskId !== 'string' || typeof parsed.answer !== 'string') {
            res.statusCode = 400;
            res.end(JSON.stringify({ ok: false, note: 'expected {repoPath, taskId, answer} strings' }));
            return;
          }
          void import('./orchestrator/store.js').then(({ applyAnswerToRepo }) => {
            const r = applyAnswerToRepo(parsed.repoPath as string, parsed.taskId as string, parsed.answer as string);
            res.statusCode = r.status;
            res.setHeader('content-type', 'application/json');
            res.end(JSON.stringify(r.body));
          });
        });
        return;
      }
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

    server.listen(opts.port, () =>
      console.log(`Listening on :${opts.port} — POST /webhooks/linear, POST /api/answer (Bearer DEVAGENT_ANSWER_TOKEN)`),
    );
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
      cleanup: resolveCleanup(undefined, config.cleanup),
      dropOrcaWorkspace: config.dropOrcaWorkspace ?? false,
    };
    const outcomes = await runPipeline(cfg, buildDeps(creds, cfg, logger), logger);
    printOutcomes(outcomes);
  } catch (err) {
    logger.error('fetch', `Dispatch failed: ${(err as Error).message}`);
  } finally {
    lock.release();
  }
}

/** CLI --cleanup flag wins over devagent.json; final default is 'auto'. */
function resolveCleanup(flag: string | undefined, fileConfig: CleanupMode | undefined): CleanupMode {
  const value = (flag ?? fileConfig ?? 'auto') as CleanupMode;
  if (!['auto', 'keep', 'always'].includes(value)) {
    throw new Error(`Invalid --cleanup "${value}"; expected auto, keep, or always`);
  }
  return value;
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
  .description('Show status: plain-language phase card + recent runs (§20.8 FR-SIMPLE-03/04); --json emits the phase view as JSON')
  .option('--limit <n>', 'number of runs', Number, 10)
  .option('--repo <path>', 'target repository', process.cwd())
  .option('--providers', 'report proxy-probe result, last transient class, circuit state, and the dispatch model-id preflight verdict for the repo config (Q32)', false)
  .option('--json', 'emit the phase view as JSON (machine format)', false)
  .action(async (opts) => {
    if (opts.json) {
      console.log(statusJson(await buildStatusView((opts.repo as string) ?? process.cwd())));
      return;
    }
    if (opts.providers) {
      const { readProxyState } = await import('./resilience/proxy-state.js');
      const repoPath = (opts.repo as string) ?? process.cwd();
      const state = readProxyState(repoPath);
      if (!state) {
        console.log('No provider state recorded yet. The orchestrate-loop probe gate populates .devagent/proxy-state.json.');
      } else {
        console.log('Providers:');
        const probeLine = state.lastProbe
          ? `probe: ${state.lastProbe.ok ? 'ok' : 'fail'} @ ${state.lastProbe.at}${state.lastProbe.detail ? ` — ${state.lastProbe.detail}` : ''}`
          : 'probe: never recorded';
        console.log(`  ${probeLine}`);
        const transientLine = state.lastTransient
          ? `transient: ${state.lastTransient.class} @ ${state.lastTransient.at} — ${state.lastTransient.excerpt.slice(0, 120)}`
          : 'transient: none recorded';
        console.log(`  ${transientLine}`);
        console.log(`  circuit: ${state.circuit} (since ${state.circuitChangedAt ?? state.updatedAt})`);
      }
      // Dispatch model-id preflight verdict (PRD Phase 4 Q32): the same gate
      // executor.ts/deps.ts run before any worker spend, surfaced here so a
      // smoke run can show validation applied without needing a live dispatch.
      const { validateModelId } = await import('./workers/model-id.js');
      const { loadConfig: loadModelCfg } = await import('./config.js');
      const cfg = loadModelCfg(repoPath);
      const workerName = cfg.worker === 'both' ? 'claude-code' : cfg.worker;
      const problem = validateModelId(workerName, cfg.model);
      const wName = cfg.worker === 'both' ? 'claude-code | opencode' : cfg.worker;
      console.log(`Model validation (${wName}): ${problem ? 'REJECTED' : 'ok'}`);
      console.log(`  model: ${cfg.model ? JSON.stringify(cfg.model) : '(unset — adapter default)'}`);
      if (problem) console.log(`  reason: ${problem}`);
      if (problem) process.exitCode = 1;
      return;
    }
    // Human headline (FR-SIMPLE-03/04): current phase + one next action
    // above the machine-history run table.
    for (const line of renderStatusCard(await buildStatusView((opts.repo as string) ?? process.cwd()))) {
      console.log(line);
    }
    console.log('');
    const home = process.env.DEVAGENT_HOME || join(process.env.HOME || '.', '.devagent');
    const runsDir = join(home, 'runs');
    let files: string[] = [];
    try {
      files = readdirSync(runsDir).filter((f) => f.endsWith('.jsonl')).sort().slice(-(opts.limit as number));
    } catch {
      console.log('No runs yet.');
    }
    if (files.length === 0) {
      console.log('No runs yet.');
    } else {
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
    }
    try {
      const { ResourceGovernor } = await import('./orchestrator/governor.js');
      const gov = new ResourceGovernor();
      const snap = gov.getSnapshotSync();
      const eff = gov.effectiveAuto(snap);
      console.log(gov.formatStatus('auto', eff, snap));
    } catch {
      // governor best-effort
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
  .description('Run one prompt-driven task headlessly (orchestrator integration mode); --pick resolves a PRD Phase 4 backlog item and cross-checks it before dispatch')
  .option('--prompt <text>', 'task description (first line becomes the title); required unless --pick is used')
  .option('--pick <backlog-id>', 'PRD Phase 4 backlog item id (e.g. Q40): resolved from docs/PRD.md and cross-checked against merged PR titles + completion notes before dispatch; a shipped pick is rejected with "already shipped" and struck from the backlog')
  .option('--dry-run', 'validate the pick only; no workers, no remotes, no PRD write', false)
  .option(
    '--id <taskId>',
    'task identity: names the worktree (.devagent-worktrees/<id>) and branch (devagent/<id>); default $DEVAGENT_TASK_ID, else a collision-free TASK-<suffix>',
  )
  .option('--repo <path>', 'target repository', process.cwd())
  .option('--worker <name>', 'claude-code | opencode | omp | pi | both')
  .option('--model <id>', 'model override passed to the worker CLI (provider/model)')
  .option('--variant <name>', 'variant for opencode model (maps to --variant or #variant)')
  .option('--cleanup <mode>', "post-run worktree disposal: auto | keep | always")
  .option('--drop-orca-workspace', 'drop the enclosing Orca workspace after done (when repoPath is Orca-managed)', false)
  .option('--auto-pr', 'push branch and open PR when green', false)
  .option('--auto-merge', 'auto review + merge the PR once CI is green (default from config autoMerge)', false)
  .option('--max-loops <n>', 'test-failure retry budget', Number)
  .option(
    '--remote <target>',
    'delegate to a shared host instead of running locally: [user@]host:/abs/repo/path or ssh://user@host:port/path',
  )
  .action(async (opts) => {
    const config = loadConfig(opts.repo);
    const creds = loadCredentials();
    const logger = new RunLogger();

    // PRD backlog reconciliation at pick time (curation run 24 decision):
    // validate a --pick against merged PR titles + completion notes BEFORE
    // any dispatch. A shipped pick (the Q40 re-selected-3x class) is rejected
    // with a clear message; confirmed-shipped items are struck from the Phase
    // 4 backlog section in the same run (dry-run validates only, no writes).
    let prompt = opts.prompt as string | undefined;
    if (opts.pick) {
      const { checkBacklogPick, listMergedPrTitles, strikeBacklogItems } = await import('./task.js');
      const prdPath = join(opts.repo, 'docs', 'PRD.md');
      if (!existsSync(prdPath)) {
        console.error(`task: no docs/PRD.md in ${opts.repo}`);
        process.exitCode = 1;
        return;
      }
      const prd = readFileSync(prdPath, 'utf8');
      const mergedTitles = await listMergedPrTitles(opts.repo);
      const check = checkBacklogPick(opts.pick, prd, mergedTitles);
      if (!opts.dryRun && check.struckIds.length > 0) {
        writeFileSync(prdPath, strikeBacklogItems(prd, check.struckIds));
        if (check.shipped) console.log(`[task] struck confirmed-shipped backlog items: ${check.struckIds.join(', ')}`);
      }
      if (!check.ok) {
        console.error(check.message);
        process.exitCode = 1;
        return;
      }
      if (opts.dryRun) {
        console.log(`[dry-run] pick ${opts.pick}: ${check.message}`);
        return;
      }
      prompt = check.prompt;
    }
    if (!prompt) {
      console.error('task: --prompt or --pick is required');
      process.exitCode = 1;
      return;
    }

    // Remote execution: the shared host owns its checkout, workers and
    // credentials; this process is a thin client that ships the prompt over
    // SSH and reports the outcome.
    if (opts.remote) {
      const { runRemoteTask } = await import('./remote.js');
      const { spawnCli } = await import('./workers/index.js');
      logger.info('task', `Task run ${logger.runId} starting (remote: ${opts.remote})`, {});
      const result = await runRemoteTask(
        {
          target: opts.remote as string,
          prompt,
          taskId: ((opts.id as string | undefined) ?? process.env.DEVAGENT_TASK_ID) || undefined,
          worker: opts.worker as string | undefined,
          timeoutMs: config.timeoutMinutes * 60_000,
          log: logger,
        },
        { run: (argv, timeoutMs) => spawnCli(argv[0]!, argv.slice(1), { cwd: process.cwd(), timeoutMs }) },
      );
      console.log(result.ok ? result.note : `remote task failed: ${result.note}`);
      if (result.prUrl) console.log(`PR: ${result.prUrl}`);
      process.exitCode = result.ok ? 0 : 1;
      return;
    }

    const cfg = {
      prompt,
      autoMerge: opts.autoMerge || config.autoMerge || undefined,
      repoPath: opts.repo,
      autoPr: opts.autoPr ?? false,
      maxLoops: opts.maxLoops ?? config.maxLoops,
      timeoutMs: config.timeoutMinutes * 60_000,
      cleanup: resolveCleanup(opts.cleanup, config.cleanup),
      dropOrcaWorkspace: opts.dropOrcaWorkspace ?? config.dropOrcaWorkspace ?? false,
      log: logger,
      taskId: ((opts.id as string | undefined) ?? process.env.DEVAGENT_TASK_ID) || undefined,
    };
    logger.info('task', `Task run ${logger.runId} starting`, { repo: cfg.repoPath, autoPr: cfg.autoPr });

    try {
      // Durable-state bootstrap: make sure selfbuild/state exists upstream so
      // the lessons feedback loop has a real branch to read. Best-effort — a
      // missing state branch must not kill the run (the lessons read path
      // already tolerates absence).
      try {
        const state = await ensureStateBranch(opts.repo);
        if (state.action === 'created') logger.info('task', 'created selfbuild/state branch on origin');
      } catch (err) {
        logger.info('task', `ensureStateBranch failed (continuing): ${(err as Error).message}`);
      }

      const taskMod = await import('./task.js');
      const { runTask, publishTaskBranch } = taskMod;
      type TaskDeps = import('./task.js').TaskDeps;
      const { implementStage } = await import('./deps.js');
      const { runMigrationStaticGate } = await import('./validation/runner.js');

      const workerName = ((opts.worker ?? config.worker) as 'claude-code' | 'opencode' | 'omp' | 'pi' | 'both');
      const deps: TaskDeps = {
        runPipelineDeps: {
          fetchTicket: async () => ({ id: cfg.taskId ?? 'TASK', title: '', description: '', labels: [], acceptanceCriteria: [] }),
          runGateG3: (rp, classification) => {
            const r = runMigrationStaticGate({ repoPath: rp, classification });
            return { passed: r.passed, findings: r.findings, detail: r.detail };
          },
        },
        implementStage: async (c, ticket, lg) => {
          const plan = { ticket, classification: 'endpoint-only' as const, tasks: [], summary: ticket.title };
          const model = opts.model ?? config.model;
          const variant = opts.variant ?? config.variant;
          return implementStage(
            {
              repoPath: c.repoPath,
              maxLoops: c.maxLoops,
              timeoutMs: c.timeoutMs,
              worker: workerName,
              autoPr: c.autoPr,
              lessonsFile: config.lessonsFile,
              lessonsMaxChars: config.lessonsMaxChars,
              ...(model ? { model } : {}),
              ...(variant ? { variant } : {}),
              cleanup: c.cleanup,
              dropOrcaWorkspace: c.dropOrcaWorkspace,
            },
            plan,
            lg,
          );
        },
        publishStage: async (c, _ticket, impl) => {
          if (!impl.worktreePath || !creds.githubToken) return undefined;
          const git = await import('./git/worktree.js');
          const { pushBranch, createPr } = await import('./integrations/github.js');
          const baseBranch = loadConfig(c.repoPath).githubBaseBranch ?? 'main';
          // Dogfood loops 7-9: commit worker output and push the worktree's
          // ACTUAL branch — the old code invented devagent/task-<runId>, a ref
          // nobody created ("src refspec does not match any" on every run).
          const prUrl = await publishTaskBranch(
            { repoPath: c.repoPath, prompt: cfg.prompt, baseBranch, log: logger },
            impl,
            {
              commitAllChanges: git.commitAllChanges,
              currentBranch: git.currentBranch,
              listChangedFiles: git.listChangedFiles,
              pushBranch,
              createPr,
            },
          );
          if (!prUrl) return undefined;
          if (c.autoMerge) {
            const n = /pull\/(\d+)/.exec(prUrl)?.[1];
            if (n) {
              void import('./integrations/autopr.js')
                .then((m) =>
                  m.autoReviewAndMergeOne(c.repoPath, Number(n), {
                    baseBranch: loadConfig(c.repoPath).githubBaseBranch ?? 'main',
                  }),
                )
                .then((o) => logger.info('task', `auto-merge PR #${n}: ${o.action} (${o.detail.slice(0, 120)})`, {}))
                .catch((err) => logger.warn('task', `auto-merge PR #${n} failed: ${(err as Error).message}`));
            }
          }
          return prUrl;
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
  .option('--auditor <name>', 'independent auditor worker; pass --no-audit to trust executor gates only')
  .option('--no-audit', 'disable the independent audit gate')
  .option(
    '--answer <id=text>',
    'resolve a task paused for human input (repeatable, use with --resume)',
    (v: string, acc: string[]) => {
      acc.push(v);
      return acc;
    },
    [] as string[],
  )
  .option('--concurrency <n>', 'parallel executor slots (number or "auto")', parseConcurrency, 2)
  .option('--max-task-retries <n>', 'scheduler retry budget per task', Number, 1)
  .option('--max-recoveries <n>', 'planner-written recovery re-contracts per task before terminal failure (0 disables)', Number, 1)
  .option('--plan-only', 'persist and print the plan (with contracts), then exit before any executor spend', false)
  .option('--max-waves <n>', 'hard cap on dispatch waves; unfinished tasks stay pending for --resume', Number)
  .option('--resume', 'continue an existing board instead of re-planning', false)
  // NOTE: no explicit default — commander negates --no-merge to opts.merge=true
  .option('--no-merge', 'skip merge-back even when all tasks are done')
  .action(async (opts) => {
    const cfg = { dryRunMerge: opts.merge === false };
    const config = loadConfig(opts.repo);
    const logger = new RunLogger();
    logger.info('task', `Orchestration run ${logger.runId} starting`, { repo: opts.repo });

    try {
      const { loadBoard, saveBoard, createBoard, formatPlanOnly, applyHumanAnswer } = await import('./orchestrator/store.js');
      const { runScheduler } = await import('./orchestrator/scheduler.js');
      const { executeTask } = await import('./orchestrator/executor.js');
      const { runAudit } = await import('./orchestrator/auditor.js');

      // Durable-state bootstrap: ensure selfbuild/state exists on origin
      // before the scheduler runs so the lessons feedback loop has a real
      // upstream branch. Best-effort: log-and-continue, never fatal.
      try {
        const state = await ensureStateBranch(opts.repo);
        if (state.action === 'created') logger.info('task', 'created selfbuild/state branch on origin');
      } catch (err) {
        logger.info('task', `ensureStateBranch failed (continuing): ${(err as Error).message}`);
      }

      const plannerName = (opts.planner ?? config.worker) as WorkerName;
      const executorName = (opts.executor ?? config.worker) as WorkerName;
      // Evidence gate on by default (LH-Harness lesson): executor success is a
      // claim; only an independent audit makes it trusted state.
      const auditorName: WorkerName | undefined = opts.audit === false ? undefined : ((opts.auditor ?? executorName) as WorkerName);
      const timeoutMs = config.timeoutMinutes * 60_000;

      let board = opts.resume ? loadBoard(opts.repo) : null;
      if (!board) {
        if (opts.resume) console.error('No existing board found; planning fresh.');
        const { runPlanner } = await import('./orchestrator/planner.js');
        const tasks = await runPlanner(opts.goal, opts.repo, plannerName, timeoutMs, {
          model: config.model,
          variant: config.variant,
        });
        board = createBoard(opts.goal, tasks, { planner: plannerName, executor: executorName, auditor: auditorName });
        saveBoard(opts.repo, board);
        console.log(`Plan (${tasks.length} task(s)):`);
        for (const t of tasks) {
          console.log(`  ${t.id}: ${t.title}${t.dependsOn.length ? ` (after ${t.dependsOn.join(',')})` : ''}`);
        }
      }

      // Validate-before-spend (CrewAI/LangGraph plan-preview lesson): show the
      // full contracts and stop before executors burn tokens.
      if (opts.planOnly) {
        console.log(formatPlanOnly(board));
        console.log(`\nPlan-only: board saved at ${join(opts.repo, '.devagent-project.json')}. Execute later with --resume.`);
        return;
      }

      // Human-in-the-loop: resolve tasks the auditor paused with verdict 'ask'
      for (const a of (opts.answer ?? []) as string[]) {
        const eq = a.indexOf('=');
        if (eq <= 0) {
          console.error(`--answer ${a}: expected <taskId>=<answer>; ignored`);
          continue;
        }
        const r = applyHumanAnswer(board!, a.slice(0, eq), a.slice(eq + 1));
        (r.ok ? console.log : console.error)(`${r.ok ? '' : `--answer ${a}: `}${r.note}`);
      }

      // Resource governor for auto concurrency (PRD-resource-aware-concurrency)
      let governor: import('./orchestrator/governor.js').ResourceGovernor | undefined;
      if (opts.concurrency === 'auto') {
        const { ResourceGovernor } = await import('./orchestrator/governor.js');
        governor = new ResourceGovernor();
        const snap = governor.getSnapshotSync();
        const eff = governor.effectiveAuto(snap);
        const line = governor.formatStatus('auto', eff, snap);
        console.log(`[governor] ${line}`);
        logger.info('governor', `auto -> ${eff}`, { freeMem: snap.freeMem, totalMem: snap.totalMem, cpus: snap.cpus, estPerWorker: governor.getEstMemPerWorker() });
      }

      const result = await runScheduler(
        board,
        {
          repoPath: opts.repo,
          lessonsFile: config.lessonsFile,
          lessonsMaxChars: config.lessonsMaxChars,
          executor: executorName,
          concurrency: opts.concurrency as number | 'auto',
          ...(governor ? { governor } : {}),
          maxTaskRetries: opts.maxTaskRetries,
          maxRecoveries: opts.maxRecoveries,
          maxWaves: opts.maxWaves,
          timeoutMs,
          // persist after every wave so resume never re-runs done work
          onWavePersisted: (b) => saveBoard(opts.repo, b),
        },
        {
          executeTask: (a) => executeTask({ ...a, executor: executorName }),
          auditTask: auditorName
            ? (a) => runAudit({ board: a.board, task: a.task, worktreePath: a.task.worktreePath ?? a.repoPath, timeoutMs: a.timeoutMs, auditor: auditorName })
            : undefined,
          planRecovery:
            opts.maxRecoveries > 0
              ? (a) =>
                  import('./orchestrator/planner.js').then(({ runRecoveryPlanner }) =>
                    runRecoveryPlanner({
                      goal: board!.goal,
                      task: a.task,
                      repoPath: opts.repo,
                      plannerWorker: plannerName,
                      timeoutMs,
                      model: config.model,
                      variant: config.variant,
                    }),
                  )
              : undefined,
          // Publish each verified task branch as a PR as soon as it's done,
          // so one failing task no longer gates every other task's PR behind
          // the all-done merge-back (loop-69: devagent/T1-a1r1 pushed, no PR).
          publishTaskPr: (a) =>
            import('./integrations/github.js').then(async ({ pushBranch, createPr }) => {
              const { attemptSuffix } = await import('./orchestrator/types.js');
              const task = a.task;
              const branch = `devagent/${task.id}-${attemptSuffix(task.attempts, task.recoveries)}`;
              const repoPath = a.repoPath;
              // The task worktree holds the committed work; push that branch.
              await pushBranch(repoPath, branch);
              const baseBranch = config.githubBaseBranch ?? 'main';
              const body = [
                `## Task ${task.id}`,
                ``,
                task.prompt ? task.prompt.split('\n').slice(0, 30).join('\n') : task.title,
              ].join('\n');
              const prUrl = await createPr({
                repoPath,
                branch,
                title: `[${task.id}] ${task.title}`,
                body,
                baseBranch,
              });
              a.log.info('publish', `opened PR ${prUrl} for ${task.id}`, {});
              return prUrl;
            }),
        },
        logger,
      );
      saveBoard(opts.repo, result);

      const done = result.tasks.filter((t) => t.status === 'done').length;
      const failed = result.tasks.filter((t) => t.status === 'failed').length;
      const blocked = result.tasks.filter((t) => t.status === 'blocked').length;
      const untrusted = result.tasks.filter((t) => t.status === 'untrusted').length;
      console.log(
        `\nProject: ${done}/${result.tasks.length} done, ${failed} failed, ${blocked} blocked${untrusted ? `, ${untrusted} awaiting audit` : ''}`,
      );
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

      // Zombie-PR hygiene (PRD §17) on the all-done path: reap PRs whose base
      // branch died and hold the merge-back while any TASK PR is still red
      // across the grace window (merge only provably green work).
      try {
        const { sweepTaskPrHygiene } = await import('./orchestrator/pr-hygiene.js');
        const hygiene = await sweepTaskPrHygiene(opts.repo, { autoMerge: true, log: (m) => logger.info('task', m, {}) });
        for (const o of hygiene.outcomes) {
          if (o.action !== 'untouched' && o.action !== 'skipped') logger.info('task', `pr-hygiene #${o.pr} ${o.action} (${o.reason}): ${o.detail.slice(0, 160)}`, {});
        }
        if (hygiene.skipAutoMerge) {
          logger.warn('task', 'merge-back skipped: red-across-grace TASK PR(s) present; re-run when CI is green', {});
          console.log('\nMerge-back skipped: red-across-grace TASK PR(s) present; re-run with --resume when CI is green.');
          return;
        }
      } catch (err) {
        // hygiene is best-effort; never block the merge-back on a sweep crash
        logger.warn('task', `pr-hygiene sweep failed (continuing): ${(err as Error).message}`, {});
      }

      // Gate the legacy merge-back (PRD Q20 / IMPROVE-retire-legacy-mergeback):
      // when the per-task PR flow (PR #71 publishTaskPr) already opened PRs
      // for done tasks, their integration is owned by those PRs — running
      // mergeProjectBranches here would double-merge the same branches
      // locally. Leave integration to the PRs and end the run.
      const { perTaskPrPublished } = await import('./orchestrator/merge.js');
      if (perTaskPrPublished(result)) {
        const prTasks = result.tasks.filter((t) => t.status === 'done' && t.prUrl).length;
        const note = `Merge-back skipped: ${prTasks} done task(s) already published per-task PR(s); integration is owned by those PRs.`;
        logger.info('task', note, {});
        console.log(`\n${note}`);
        return;
      }

      const baseBranch = loadConfig(opts.repo).githubBaseBranch ?? 'main';
      console.log(`\nAll tasks done — merging into ${baseBranch}...`);
      const git = await import('./git/worktree.js');
      const stashSha = await git.stashMainWorktree(opts.repo, 'devagent auto-stash before merge');
      if (stashSha) console.log(`Auto-stashed uncommitted changes as ${stashSha}`);
      try {
        await git.assertCleanMainWorktree(opts.repo, baseBranch);
        const { mergeProjectBranches } = await import('./orchestrator/merge.js');
        const mr = await mergeProjectBranches(opts.repo, result, baseBranch, logger);
        if (mr.ok) {
          console.log(`Integrated: ${mr.merged.join(', ')}`);
        } else {
          console.error(`Integration failed at ${mr.failure!.taskId} (${mr.failure!.stage}): ${mr.failure!.detail}`);
          console.error('Board preserved; fix and re-run with --resume.');
          process.exitCode = 1;
        }
      } finally {
        if (stashSha) {
          // Restore the loop's own auto-stash by concrete SHA (indices shift
          // under concurrent stashes). If the restore fails, leave the stash
          // intact rather than dropping user work.
          const popped = await git.popStashBySha(opts.repo, stashSha);
          if (!popped) {
            console.error(`Warning: could not restore stash ${stashSha}; stash kept for manual recovery.`);
          }
        }
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
    const { ledgerTailFor } = await import('./orchestrator/ledger.js');
    const board = loadBoard(opts.repo);
    if (!board) {
      console.log('No project board. Start one: devagent orchestrate --goal "..."');
      return;
    }
    const counts = new Map<string, number>();
    for (const t of board.tasks) counts.set(t.status, (counts.get(t.status) ?? 0) + 1);
    const bar = Object.fromEntries([...counts].map(([k, v]) => [k, v]));
    console.log(`Goal: ${board.goal}`);
    const roleLine = `Roles: planner=${board.roles.planner} executor=${board.roles.executor}`;
    console.log(board.roles.auditor ? `${roleLine} auditor=${board.roles.auditor}` : roleLine);
    console.log(`Tasks: ${board.tasks.length} (${Object.entries(bar).map(([k, v]) => `${k}:${v}`).join(' ')})`);
    for (const t of board.tasks) {
      const mark =
        t.status === 'done'
          ? '✓'
          : t.status === 'failed'
            ? '✗'
            : t.status === 'dispatched'
              ? '▶'
              : t.status === 'blocked'
                ? '⛔'
                : t.status === 'untrusted'
                  ? '~'
                  : t.status === 'ask'
                    ? '?'
                    : '·';
      const auditNote = t.audit
        ? ` [audit ${t.audit.verdict}/${t.audit.integrity}${t.audit.criteriaResults.some((c) => !c.met) ? `, unmet: ${t.audit.criteriaResults.filter((c) => !c.met).length}` : ''}]`
        : '';
      const gapNote = t.evidenceGaps?.length ? ` — gaps: ${t.evidenceGaps[0]!.slice(0, 80)}` : '';
      console.log(
        ` ${mark} [${t.status}] ${t.id}: ${t.title}${auditNote}${gapNote}${!gapNote && t.failureDetail ? ` — ${t.failureDetail.slice(0, 80)}` : ''}`,
      );
      // Ledger evidence history (loop 49 L4): verdict trends at a glance
      const tail = ledgerTailFor(opts.repo, t.id);
      if (tail.length > 1 || (tail.length === 1 && tail[0]!.verdict !== 'pass')) {
        console.log(`    history: ${tail.map((r) => `${r.verdict}/${r.integrity}@a${r.attempt}`).join(' -> ')}`);
      }
    }
    console.log(`Updated: ${board.updatedAt}`);
    const allDone = board.tasks.length > 0 && board.tasks.every((t) => t.status === 'done');
    if (allDone) console.log('Ready to integrate: devagent orchestrate --goal "" --resume');
    else console.log('Resume: devagent orchestrate --goal "" --resume');
  });

program
  .command('ledger')
  .description('Show the orchestration run ledger (persisted audit verdicts)')
  .option('--repo <path>', 'target repository', process.cwd())
  .option('--task <id>', 'filter to one task id')
  .option('--summary', 'print aggregate outcome stats instead of the record list')
  .option('--clusters [n]', 'print ranked failure clusters (recurring unmet criteria), top N (default 5)')
  .action(async (opts) => {
    if (opts.clusters !== undefined) {
      const { clusterFailures } = await import('./orchestrator/ledger.js');
      const top = typeof opts.clusters === 'string' ? parseInt(opts.clusters, 10) : 5;
      const clusters = clusterFailures(opts.repo);
      if (!Number.isFinite(top) || top <= 0 || clusters.length === 0) {
        console.log(
          clusters.length === 0
            ? 'No failure clusters. Failed audits with unmet criteria cluster here once the ledger has records.'
            : `Nothing to show for --clusters ${opts.clusters}.`,
        );
        return;
      }
      console.log(`failure clusters (top ${Math.min(top, clusters.length)} of ${clusters.length}):`);
      for (const c of clusters.slice(0, top)) {
        console.log(
          `- "${c.criterion}" — ${c.occurrences} occurrence(s) across ${c.tasks.length} task(s)` +
            ` (${c.openTasks} still open): ${c.tasks.join(', ')}`,
        );
      }
      return;
    }
    if (opts.summary) {
      const { summarizeLedger } = await import('./orchestrator/ledger.js');
      const sum = summarizeLedger(opts.repo);
      console.log(
        `tasks: ${sum.tasks} | audits: ${sum.audits} | resolved: ${sum.resolved} | unresolved: ${sum.unresolved}` +
          (sum.meanAttemptsToPass !== null ? ` | mean attempts-to-pass: ${sum.meanAttemptsToPass}` : ''),
      );
      return;
    }
    const { readLedger } = await import('./orchestrator/ledger.js');
    const records = readLedger(opts.repo, { taskId: opts.task });
    if (records.length === 0) {
      console.log('No ledger records. Audits append to .devagent/runs/orchestration/events.jsonl.');
      return;
    }
    for (const r of records) {
      if (r.kind !== 'audit') continue;
      const icon = r.verdict === 'pass' ? '+' : r.verdict === 'ask' ? '?' : 'x';
      const detail = `${r.verdict}/${r.integrity}${r.unmetCriteria.length ? ` unmet:${r.unmetCriteria.length}` : ''} — ${r.summary.slice(0, 90)}`;
      console.log(`${icon} ${r.ts} [${r.kind}] ${r.taskId} (attempt ${r.attempt}) ${detail}`);
    }
  });

program
  .command('mcp')
  .description('Expose DevAgent as MCP tools over stdio (devagent_dispatch/status/log)')
  .action(async () => {
    const { startMcpServer } = await import('./server/mcp.js');
    startMcpServer();
  });

program
  .command('preflight')
  .description(
    'Operator-role provider preflight (Q40): probe the configured worker CLI; on failure write an operator-degraded ledger row, open the circuit, and exit nonzero so the calling loop skips its agent dispatch this cycle (opt-outs ORCHESTRATOR_MODEL_PROBE=0 or OPERATOR_PROBE_DISABLED=1)',
  )
  .requiredOption('--role <name>', `operator role to gate (${PREFLIGHT_ROLES.join(' | ')})`)
  .option('--repo <path>', 'target repository owning the ledger/proxy state', process.cwd())
  .option('--worker <name>', 'worker CLI to probe (default from repo config)')
  .option('--model <id>', 'model id passed to the probe (default from repo config)')
  .action(async (opts) => {
    if (!isPreflightRole(opts.role)) {
      console.error(`[preflight] unknown role "${opts.role}"; expected one of: ${PREFLIGHT_ROLES.join(', ')}`);
      process.exitCode = 1;
      return;
    }
    // Default-on opt-out: env var disables the probe gate entirely so
    // operator loops proceed without gating (Q40).
    if (process.env.OPERATOR_PROBE_DISABLED === '1') {
      console.log(`[preflight] disabled by OPERATOR_PROBE_DISABLED=1 — skipping probe for role=${opts.role}`);
      return;
    }
    // Explicit opt-out (mirrors the orchestrate-loop probe's documented
    // ORCHESTRATOR_MODEL_PROBE=0, PRD §17): a disabled probe must not block
    // the cycle, write a ledger row, or touch circuit state.
    if (process.env.ORCHESTRATOR_MODEL_PROBE === '0') {
      console.log(`[preflight] probe disabled for role=${opts.role} (ORCHESTRATOR_MODEL_PROBE=0) — cycle proceeds unprobed`);
      return;
    }
    const config = loadConfig(opts.repo);
    const worker = (opts.worker as string | undefined) ?? config.worker;
    const rawModel = (opts.model as string | undefined) ?? config.model;
    const decision = await runPreflightGate({
      repoPath: opts.repo,
      role: opts.role,
      worker,
      model: rawModel ?? '',
      argv: buildProbeArgvFor(worker, rawModel),
      cwd: opts.repo,
    });
    if (decision.ok) {
      console.log(
        `[preflight] ok role=${decision.role} worker=${worker} model=${rawModel || '(default)'} attempts=${decision.attempts}`,
      );
    } else {
      console.error(
        `[preflight] DEGRADED role=${decision.role} worker=${worker} model=${rawModel || '(default)'} attempts=${decision.attempts} — skip this cycle's agent dispatch (ledger: .devagent/runs/orchestration/events.jsonl)`,
      );
      if (decision.detail) console.error(`[preflight] last probe: ${decision.detail}`);
      process.exitCode = 1;
    }
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
  .option('--max-attempts <n>', 'total launches including the first (default Infinity, set DEVAGENT_API_MAX_ATTEMPTS or pass N to cap)', Number)
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

program
  .command('automerge')
  .description('Auto review + merge open PRs against objective gates (CI green, mergeable, hazard scan)')
  .option('--repo <path>', 'target repository (used for gh context)', process.cwd())
  .option('--pr <n>', 'specific PR number (repeat to target a set)', (v: string, acc: number[]) => { acc.push(Number(v)); return acc; }, [] as number[])
  .option('--base <branch>', 'only PRs targeting this base branch')
  .option('--method <method>', 'squash | merge | rebase', 'squash')
  .option('--timeout <seconds>', 'max seconds to wait for pending checks', Number, 300)
  .option('--grace-hours <hours>', 'hours a PR may stay red before the queue skips it (default from config prHygiene.graceHours)', Number)
  .option('--dry-run', 'evaluate and print verdicts without reviewing or merging', false)
  .action(async (opts) => {
    const { autoReviewAndMerge } = await import('./integrations/autopr.js');
    try {
      const outcomes = await autoReviewAndMerge(opts.repo, {
        prNumbers: opts.pr.length ? (opts.pr as number[]) : undefined,
        baseBranch: opts.base,
        method: opts.method,
        dryRun: opts.dryRun,
        waitForChecksSec: opts.timeout,
        graceHours: opts.graceHours,
        log: (msg) => console.log(msg),
      });
      const merged = outcomes.filter((o) => o.action === 'merged').length;
      console.log(`
${merged}/${outcomes.length} merged`);
      if (merged < outcomes.length && !opts.dryRun) process.exitCode = 1;
    } catch (err) {
      console.error((err as Error).message);
      process.exitCode = 1;
    }
  });

program
  .command('autosweep')
  .description('Zombie-PR hygiene sweep: skip superseded PRs and auto-close PRs red across the grace window')
  .option('--stale-prs', 'sweep stale open PRs (superseded-base skip + grace-window close)', false)
  .option('--repo <path>', 'target repository (used for gh context)', process.cwd())
  .option('--grace-days <days>', 'days a PR may stay red before auto-close (default from config zombiePrs.graceDays)', Number)
  .option('--apply', 'comment and close for real (default: config zombiePrs.dryRun, itself defaulting to dry-run)', false)
  .action(async (opts) => {
    if (!opts.stalePrs) {
      console.error('nothing to sweep: pass --stale-prs');
      process.exitCode = 1;
      return;
    }
    const { sweepStalePrs } = await import('./integrations/autopr.js');
    try {
      const outcomes = await sweepStalePrs(opts.repo, {
        graceDays: opts.graceDays,
        dryRun: !opts.apply,
        log: (msg) => console.log(msg),
      });
      for (const o of outcomes) {
        console.log(`#${o.pr} ${o.action}: ${o.detail}`);
      }
      const acted = outcomes.filter((o) => o.action === 'superseded' || o.action === 'closed').length;
      console.log(`
${acted}/${outcomes.length} PR(s) ${opts.apply ? 'swept' : 'flagged (dry-run; pass --apply to act)'}`);
    } catch (err) {
      console.error((err as Error).message);
      process.exitCode = 1;
    }
  });

program
  .command('pr-hygiene')
  .description('Zombie-PR hygiene for devagent/TASK-* PRs: close base-superseded, flag red-across-grace (skips autoMerge until green)')
  .option('--repo <path>', 'target repository (used for gh context)', process.cwd())
  .option('--grace-hours <hours>', 'hours a PR may stay red before being flagged (default from config prHygiene.graceHours)', Number)
  .option('--auto-merge', 'report skipAutoMerge when a red-across-grace PR would have merged (default from config autoMerge)', false)
  .option('--apply', 'comment and close for real (default: config prHygiene.dryRun, itself defaulting to dry-run)', false)
  .action(async (opts) => {
    const { sweepTaskPrHygiene } = await import('./orchestrator/pr-hygiene.js');
    try {
      const { outcomes, skipAutoMerge } = await sweepTaskPrHygiene(opts.repo, {
        graceHours: opts.graceHours,
        dryRun: !opts.apply,
        autoMerge: opts.autoMerge || loadConfig(opts.repo).autoMerge || false,
        log: (msg) => console.log(msg),
      });
      for (const o of outcomes) {
        console.log(`#${o.pr} ${o.action} (${o.reason}): ${o.detail}`);
      }
      const acted = outcomes.filter((o) => o.action === 'closed' || o.action === 'flagged').length;
      console.log(
        `\n${acted}/${outcomes.length} PR(s) ${opts.apply ? 'swept' : 'flagged (dry-run; pass --apply to act)'}` +
          (skipAutoMerge ? '\nautoMerge skipped: red-across-grace PR(s) present' : ''),
      );
      if (skipAutoMerge) process.exitCode = 1;
    } catch (err) {
      console.error((err as Error).message);
      process.exitCode = 1;
    }
  });

program
  .command('rebase-stack')
  .description('Rebase stacked branches onto their updated parents (merge-queue refresh)')
  .option('--repo <path>', 'target repository', process.cwd())
  .option('--onto <branch>', 'base of the bottom-most stacked branch', 'main')
  .option('--push', 'force-push updated branches to origin with lease', false)
  .argument('<branches...>', 'stack bottom to top, e.g. devagent/loop60 devagent/loop61')
  .action(async (branches: string[], opts) => {
    const { rebaseStack } = await import('./git/rebase-stack.js');
    const r = await rebaseStack(opts.repo, branches, { onto: opts.onto, push: opts.push });
    for (const b of r.results) {
      console.log(`${b.outcome === 'up-to-date' ? '=' : b.outcome === 'rebased' || b.outcome === 'pushed' ? '+' : 'x'} ${b.branch}: ${b.outcome}${b.detail ? ` — ${b.detail}` : ''}`);
    }
    if (!r.ok) process.exitCode = 1;
  });

program
  .command('herdr-sweep')
  .description('Close idle/agentless stale panes in the devagent herdr session (session-scoped; never touches other sessions or non-herdr processes)')
  .option('--session <name>', 'herdr session to sweep (default DEVAGENT_HERDR_SESSION or "devagent")')
  .option('--dry-run', 'list stale panes without closing', false)
  .action(async (opts) => {
    const { resolveSession, sweepStalePanes } = await import('./integrations/herdr.js');
    const session = resolveSession(opts.session);
    const stale = await sweepStalePanes(session, { dryRun: opts.dryRun });
    if (stale.length === 0) {
      console.log(`[${session}] no stale panes`);
      return;
    }
    for (const s of stale) {
      console.log(`${opts.dryRun ? '[stale]' : '[closed]'} ${s.paneId} (${s.label}) status=${s.agentStatus} reason=${s.reason}`);
    }
    console.log(`${stale.length} pane(s) ${opts.dryRun ? 'found' : 'closed'} in session "${session}"`);
  });

program
  .command('pane-run')
  .description('Run one command inside a herdr pane of the devagent session (FR-VIS-04): research/PO/headless phases become operator-visible; falls back to a direct child when herdr is down')
  .requiredOption('--cwd <path>', 'working directory for the command')
  .requiredOption('--timeout <seconds>', 'wall-clock cap in seconds', Number)
  .requiredOption('--out <path>', 'stdout capture file')
  .requiredOption('--err <path>', 'stderr capture file')
  .requiredOption('--done <path>', 'exit-code marker file')
  .option('--session <name>', 'herdr session (default DEVAGENT_HERDR_SESSION or "devagent")')
  .argument('<cmd>', 'command binary to run')
  .argument('[args...]', 'command arguments')
  .action(async (cmd: string, args: string[], opts) => {
    const { runCommandInHerdrPane } = await import('./integrations/herdr.js');
    const result = await runCommandInHerdrPane(cmd, args, {
      cwd: opts.cwd,
      timeoutMs: opts.timeout * 1000,
      session: opts.session,
    });
    if (!result) {
      // Caller (loop driver) inspects the missing done-marker and falls back
      // to its own direct dispatch; keep the exit code distinct for triage.
      console.error('[pane-run] herdr pane unavailable');
      process.exitCode = 3;
      return;
    }
    // Capture contract matches the driver's own direct dispatch: stdout ->
    // out, stderr -> err, exit code -> done marker.
    const { writeFileSync } = await import('node:fs');
    if (result.stdout) writeFileSync(opts.out, result.stdout);
    if (result.stderr) writeFileSync(opts.err, result.stderr);
    writeFileSync(opts.done, String(result.exitCode));
    if (result.timedOut) process.exitCode = 124;
  });

program
  .command('sessions')
  .description('List live worker panes in the herdr session (FR-VIS-02)')
  .option('--json', 'raw JSON output', false)
  .option('--repo <path>', 'repo for ledger writes', process.cwd())
  .action(async (opts) => {
    const { runSessions } = await import('./commands/sessions.js');
    await runSessions({ json: opts.json, repoPath: opts.repo });
  });

program
  .command('attach <task>')
  .description('Print (or with --exec, run) the jump-in command for a worker pane (FR-VIS-02)')
  .option('--exec', 'attach immediately', false)
  .option('--repo <path>', 'repo for ledger writes', process.cwd())
  .action(async (task: string, opts) => {
    const { runAttach } = await import('./commands/sessions.js');
    await runAttach(task, { exec: opts.exec, repoPath: opts.repo });
  });

program
  .command('daemon')
  .description('Run the FR-CTRL control-plane daemon (HTTP+SSE on 127.0.0.1; UDS with --uds-path)')
  .option('--port <n>', 'TCP port (0 = ephemeral)', Number, 7788)
  .option('--repo <path>', 'repo the API reads from and dispatches into', process.cwd())
  .option('--uds-path <path>', 'listen on a Unix-domain socket instead of TCP')
  .option('--token <token>', 'bearer token (default DEVAGENT_DAEMON_TOKEN or a fresh one persisted to daemon-token)')
  .action(async (opts) => {
    const { startDaemon } = await import('./server/daemon.js');
    const handle = await startDaemon({
      port: opts.port,
      repoPath: opts.repo,
      udsPath: opts.udsPath,
      token: opts.token,
    });
    const where = handle.udsPath ?? `http://127.0.0.1:${handle.port}`;
    console.log(`devagent daemon listening on ${where} (token: ${handle.token})`);
    // Foreground service: resolve only when the process is signalled.
    await new Promise<void>(() => {});
  });

program
  .command('tui')
  .description(
    'Full-screen terminal dashboard (FR-TUI): workers/sessions/live-log views, selection + detail panels, kill, upgrade hint. One command — attaches to a running daemon, or embeds an ephemeral one for the session (--attach-only = never spawn; `devagent daemon` runs a long-lived shared one)',
  )
  .option('--url <url>', 'daemon base URL (explicit target = never embed; default probes 127.0.0.1:7788)')
  .option('--token <token>', 'bearer token (default DEVAGENT_DAEMON_TOKEN or the daemon-token file)')
  .option('--uds-path <path>', 'talk to the daemon over a Unix-domain socket')
  .option('--repo <path>', 'repo echoed into kill (approve) calls', process.cwd())
  .option('--attach-only', 'never embed a daemon; attach or show UNREACHABLE', false)
  .action(async (opts) => {
    const { runTui } = await import('./tui/tui.js');
    await runTui({
      url: opts.url,
      token: opts.token,
      udsPath: opts.udsPath,
      repoPath: opts.repo,
      attachOnly: opts.attachOnly,
    });
  });

program
  .command('scout')
  .description('24/7 opencode scout: research backlog -> PRD -> queue (FR-SCOUT-01)')
  .option('--repo <path>', 'target repository', process.cwd())
  .option('--worker <name>', 'opencode | claude-code | omp | pi')
  .option('--interval <minutes>', 'cycle interval minutes (loop mode only)', Number)
  .option('--timeout <minutes>', 'per-cycle wall-clock cap', Number)
  .option('--once', 'run exactly one cycle then exit (default in non-daemon mode)', false)
  .option('--dry-run', 'enqueue deterministic fallback task without calling LLM', false)
  .option('--replay', 'replay captured worker-output fixtures through extractScoutPayload and diff against golden.json', false)
  .action(async (opts) => {
    const { runScoutOnce, runScoutLoop, replayScoutFixtures } = await import('./scout.js');
    if (opts.replay) {
      const results = replayScoutFixtures();
      const failures = results.filter((r) => !r.pass);
      for (const r of results) {
        console.log(`${r.pass ? 'PASS' : 'FAIL'} ${r.name}`);
        if (!r.pass) {
          console.log(`  expected: ${JSON.stringify(r.expected)}`);
          console.log(`  actual:   ${JSON.stringify(r.actual)}`);
        }
      }
      console.log(`${results.length - failures.length}/${results.length} fixtures match golden`);
      if (failures.length > 0) process.exitCode = 1;
      return;
    }
    const config = loadConfig(opts.repo);
    const worker = (opts.worker ?? config.scout?.worker ?? 'omp') as 'opencode' | 'claude-code' | 'omp' | 'pi';
    const intervalMinutes = opts.interval ?? config.scout?.intervalMinutes ?? 30;
    const timeoutMs = (opts.timeout ?? 30) * 60_000;
    if (opts.once || opts.dryRun) {
      const r = await runScoutOnce({ repoPath: opts.repo, worker, intervalMinutes, dryRun: Boolean(opts.dryRun), timeoutMs }, config);
      console.log(r.detail);
      if (r.taskId) console.log(`task ${r.taskId} -> ${r.prdPath} + ${r.queuePath}`);
      console.log(`heartbeat: ${r.heartbeatPath}`);
      if (!r.ok) process.exitCode = 1;
      return;
    }
    console.log(`Scout loop started: ${worker} every ${intervalMinutes}m in ${opts.repo} (Ctrl+C to stop)`);
    const ac = new AbortController();
    process.on('SIGINT', () => ac.abort());
    process.on('SIGTERM', () => ac.abort());
    await runScoutLoop({ repoPath: opts.repo, worker, intervalMinutes, timeoutMs, signal: ac.signal }, config, (r) => {
      console.log(`[scout] ${r.detail}`);
    });
  });

program
  .command('scout-status')
  .description('Show scout heartbeat + queue depth')
  .option('--repo <path>', 'target repository', process.cwd())
  .option('--json', 'emit JSON', false)
  .action(async (opts) => {
    const { readHeartbeat } = await import('./scout.js');
    const { taskCount } = await import('./queue.js');
    const hb = readHeartbeat(opts.repo);
    const counts = taskCount(opts.repo);
    if (opts.json) {
      console.log(JSON.stringify({ heartbeat: hb, queue: counts }, null, 2));
      return;
    }
    if (!hb) console.log('No scout heartbeat yet. Run: devagent scout --once --dry-run');
    else {
      const ageMs = Date.now() - Date.parse(hb.lastRunAt);
      const age = ageMs < 60_000 ? `${Math.round(ageMs / 1000)}s ago` : `${Math.round(ageMs / 60_000)}m ago`;
      console.log(`Scout: ${hb.worker} interval ${hb.intervalMinutes}m last ${hb.lastStatus} ${age}`);
      console.log(`  lastTask: ${hb.lastTaskId ?? '(none)'}  detail: ${hb.lastDetail}`);
      console.log(`  at: ${hb.lastRunAt}`);
    }
    console.log(`Queue: total ${counts.total} (pending:${counts.pending} claimed:${counts.claimed} done:${counts.done} failed:${counts.failed})`);
  });

program
  .command('track')
  .description('Progress-tracker agent: snapshot queue+scout+ledger+git+PRs -> .selfbuild/progress.md')
  .option('--repo <path>', 'target repository', process.cwd())
  .option('--interval <minutes>', 'loop mode: run every N minutes (omit for one-shot)', Number)
  .option('--json', 'print the snapshot JSON instead of a summary line', false)
  .action(async (opts) => {
    const { trackOnce, trackLoop } = await import('./tracker.js');
    if (!opts.interval) {
      const r = await trackOnce({ repoPath: opts.repo });
      if (opts.json && r.snapshot) console.log(JSON.stringify(r.snapshot, null, 2));
      else console.log(`${r.ok ? 'ok' : 'FAILED'}: ${r.detail}\nprogress: ${r.progressMdPath ?? '(n/a)'}\nheartbeat: ${r.heartbeatPath}`);
      if (!r.ok) process.exitCode = 1;
      return;
    }
    console.log(`Tracker loop started: every ${opts.interval}m in ${opts.repo} (Ctrl+C to stop)`);
    const ac = new AbortController();
    process.on('SIGINT', () => ac.abort());
    process.on('SIGTERM', () => ac.abort());
    await trackLoop({ repoPath: opts.repo, intervalMinutes: opts.interval, signal: ac.signal }, (r) => {
      console.log(`[track] ${r.detail}`);
    });
  });

program
  .command('create')
  .description('Bootstrap the factory: queue dirs, config, optional scout LaunchAgent + Orca worktrees (FR-CREATE-01)')
  .requiredOption('--repo <path>', 'repository to bootstrap')
  .option('--scout', 'enable scout daemon (PRD writer, role 1)', false)
  .option('--tracker', 'enable progress-tracker agent (role 2)', false)
  .option('--builder', 'enable builder agent consuming the queue (role 3)', false)
  .option('--orchestrator', 'enable orchestrator agent driving the DAG board (role 4)', false)
  .option('--orchestrator-goal <text>', 'goal text for the orchestrator planner (falls back to .devagent/orchestrator-goal.txt)')
  .option('--workers <n>', 'number of Orca worker worktrees to provision', Number, 0)
  .option('--auto-merge', 'enable auto-merge of green PRs', false)
  .option('--self-update', 'enable self-update after merges', false)
  .option('--interval <minutes>', 'scout interval minutes', Number)
  .option('--track-interval <minutes>', 'tracker interval minutes (loop mode)', Number)
  .option('--scout-worker <name>', 'scout worker: opencode | claude-code | omp')
  .option('--dry-run', 'print plan without mutating', false)
  .action(async (opts) => {
    if (opts.orchestrator && !opts.orchestratorGoal && !existsSync(join(opts.repo, '.devagent', 'orchestrator-goal.txt'))) {
      console.error('--orchestrator requires --orchestrator-goal (or .devagent/orchestrator-goal.txt)');
      process.exitCode = 1;
      return;
    }
    const { runCreate } = await import('./create.js');
    const r = await runCreate({
      repoPath: opts.repo,
      scout: Boolean(opts.scout),
      tracker: Boolean(opts.tracker),
      builder: Boolean(opts.builder),
      orchestrator: Boolean(opts.orchestrator),
      orchestratorGoal: opts.orchestratorGoal as string | undefined,
      workers: Number(opts.workers) || 0,
      autoMerge: Boolean(opts.autoMerge),
      selfUpdate: Boolean(opts.selfUpdate),
      dryRun: Boolean(opts.dryRun),
      intervalMinutes: opts.interval ? Number(opts.interval) : undefined,
      scoutWorker: opts.scoutWorker as 'opencode' | 'claude-code' | 'omp' | 'pi' | undefined,
      trackIntervalMinutes: opts.trackInterval ? Number(opts.trackInterval) : undefined,
    });
    console.log(r.detail);
    if (r.configPath) console.log(`config: ${r.configPath}`);
    for (const p of r.launchAgentPlists ?? []) console.log(`LaunchAgent: ${p}`);
    if (!r.launchAgentPlists?.length && r.launchAgentPlist) console.log(`LaunchAgent: ${r.launchAgentPlist}`);
    if (r.orcaWorktrees?.length) for (const p of r.orcaWorktrees) console.log(`  worktree: ${p}`);
    if (!r.ok) process.exitCode = 1;
  });

program
  .command('lessons')
  .description('Append a lesson behind the eval guard (PRD Phase 4 "Lessons eval guard", evaluate→accept slice). Machine appends must go through this: a candidate needs a non-empty --predicted-impact, must clear the dedupe gate, and must leave the repo regression suite green against the proposed lessons-file state (red reverts the file). Exactly one lessons-eval ledger row is written per gated append.')
  .option('--repo <path>', 'repository containing the lessons file', process.cwd())
  .option('--entry <text>', 'lesson entry to append (one line)')
  .option('--predicted-impact <text>', 'predictedImpact field required by the eval guard (AHE/Meta-Harness propose→evaluate→accept precedent); empty values are rejected')
  .option('--lessons-file <path>', 'repo-relative lessons file path (default .selfbuild/lessons.md)')
  .option('--threshold <n>', 'similarity reject threshold in [0,1] (default 0.8)', Number)
  .option('--suite-timeout-ms <n>', 'evaluate-step wall-clock budget in ms (default 600000)', Number)
  .option('--loop <n>', 'self-build loop number; written to the lessons-eval ledger row so impact scoring can join it to the loop-result row (Q39)', Number)
  .option('--dry-run', 'validate the append (dedupe + held-out must-beat check) without staging the lessons file, running the suite, or writing ledger rows; prints what a real append would do', false)
  .action(async (opts) => {
    // Runtime required-option check: --entry and --predicted-impact are
    // .option() (not .requiredOption()) so the `scores` subcommand can
    // dispatch without Commander validating them at parse time.
    if (!opts.entry || !opts.predictedImpact) {
      console.error('--entry and --predicted-impact are required for the append action.');
      process.exitCode = 1;
      return;
    }
    const { appendLessonGuarded, DEFAULT_LESSONS_DEDUPE_SIMILARITY, DEFAULT_LESSONS_SUITE_TIMEOUT_MS } = await import('./lessons/guard.js');
    const config = loadConfig(opts.repo);
    const lessonsFile = (opts.lessonsFile as string | undefined) ?? config.lessonsFile ?? '.selfbuild/lessons.md';
    const threshold = opts.threshold === undefined ? (config.lessonsDedupeSimilarity ?? DEFAULT_LESSONS_DEDUPE_SIMILARITY) : Number(opts.threshold);
    const r = appendLessonGuarded(opts.repo as string, opts.entry as string, {
      lessonsFile,
      threshold,
      predictedImpact: opts.predictedImpact as string | undefined,
      suiteTimeoutMs: opts.suiteTimeoutMs === undefined ? DEFAULT_LESSONS_SUITE_TIMEOUT_MS : Number(opts.suiteTimeoutMs),
      loop: opts.loop === undefined ? undefined : Number(opts.loop),
      ...(opts.dryRun ? { dryRun: true } : {}),
    });
    const output: Record<string, unknown> = { appended: r.ok, reason: r.reason, suite: r.suite, similarity: r.similarity, threshold: r.threshold, matchedEntry: r.matchedEntry };
    if (opts.dryRun) {
      output.dryRun = true;
      output.heldOut = r.heldOut ?? 0;
      output.mustBeat = r.mustBeat ?? 'none';
      output.mustBeatScore = r.mustBeatScore ?? null;
      console.log(JSON.stringify(output));
      // Dry-run never writes, so there is nothing to accept/reject; exit 0
      // only when every checkable gate passed: dedupe cleared, predictedImpact
      // present, and the held-out must-beat check not already predicting a
      // rejection. The suite is not evaluated here (suite: 'skipped' means
      // not-run, not green) — a red suite can only surface on a real append.
      if (r.reason !== 'accepted') process.exitCode = 1;
      return;
    }
    console.log(JSON.stringify(output));
    // `ok` tracks the dedupe verdict; acceptance is the reason — a suite-red
    // revert or missing predictedImpact must also fail the invocation.
    if (r.reason !== 'accepted') process.exitCode = 1;
  });

const lessonsCmd = program.commands.find((c) => c.name() === 'lessons')!;
lessonsCmd
  .command('scores')
  .description('Show measured lesson impact scores (Q39): per-excerptHash accept rate, repeat-failure delta, and composite score aggregated from the orchestration ledger (lessons-eval rows joined with loop-result rows).')
  .option('--json', 'emit JSON', false)
  .action(async function (this: Command, opts) {
    // --repo is declared on the parent `lessons` command; Commander binds it
    // to the parent even when given after the subcommand name (v12 behavior).
    const repoPath = (this.parent?.opts()?.repo as string | undefined) ?? process.cwd();
    const { readEvents, computeLessonScores } = await import('./lessons/guard.js');
    const events = readEvents(repoPath);
    const scores = computeLessonScores(events);
    if (scores.size === 0) {
      console.log('No lesson impact scores: no lessons-eval rows in the orchestration ledger yet.');
      return;
    }
    const rows = [...scores.entries()].sort((a, b) => b[1].score - a[1].score);
    if (opts.json) {
      console.log(JSON.stringify(Object.fromEntries(rows), null, 2));
      return;
    }
    console.log('excerptHash      score   acceptRate  delta   evals  loopFailRate');
    for (const [hash, s] of rows) {
      console.log(
        `${hash.padEnd(16)} ${s.score.toFixed(3).padStart(6)}  ${s.acceptRate.toFixed(3).padStart(6)}    ${s.delta.toFixed(3).padStart(6)}  ${String(s.evalCount).padStart(5)}  ${s.lessonLoopFailureRate.toFixed(3)}`,
      );
    }
  });

program
  .command('queue')
  .description('Queue operations (FR-QUEUE-01)')
  .action(() => {
    // subcommands handle dispatch; bare `queue` prints help
    program.commands.find((c) => c.name() === 'queue')!.outputHelp();
  });

const queueCmd = program.commands.find((c) => c.name() === 'queue')!;
queueCmd
  .command('list')
  .description('List queued tasks')
  .option('--repo <path>', 'repository', process.cwd())
  .option('--status <s>', 'filter: pending | claimed | done | failed')
  .option('--json', 'emit JSON', false)
  .action(async (opts) => {
    const { listTasks } = await import('./queue.js');
    const tasks = listTasks(opts.repo, opts.status ? { status: opts.status } : undefined);
    if (opts.json) console.log(JSON.stringify(tasks, null, 2));
    else {
      if (tasks.length === 0) console.log('No tasks' + (opts.status ? ` with status ${opts.status}` : '') + '.');
      else for (const t of tasks) console.log(`${t.status.padEnd(8)} ${t.id}  ${t.title}${t.lastError ? ` — ${t.lastError.slice(0, 60)}` : ''}`);
    }
  });

queueCmd
  .command('show')
  .description('Show one queued task + PRD head')
  .argument('<id>', 'task id')
  .option('--repo <path>', 'repository', process.cwd())
  .option('--json', 'emit JSON', false)
  .action(async (id: string, opts) => {
    const { readTask, readPrd } = await import('./queue.js');
    const t = readTask(opts.repo, id);
    if (!t) { console.error(`Task ${id} not found`); process.exitCode = 1; return; }
    if (opts.json) console.log(JSON.stringify(t, null, 2));
    else {
      console.log(`${t.status} ${t.id}: ${t.title}`);
      console.log(`  goal: ${t.goal}`);
      if (t.acceptanceCriteria.length) console.log(`  criteria: ${t.acceptanceCriteria.join('; ')}`);
      if (t.prdPath) { const prd = readPrd(opts.repo, id); if (prd) console.log(`
--- PRD head ---
${prd.slice(0, 600)}`); }
    }
  });

queueCmd
  .command('bridge')
  .description('Bridge the oldest pending queued goal into an orchestrator board (idempotent; no-op when a board exists)')
  .option('--repo <path>', 'repository', process.cwd())
  .action(async (opts) => {
    const { bridgeIfQueued } = await import('./orchestrator/queue-bridge.js');
    const r = await bridgeIfQueued(opts.repo);
    if (!r) {
      console.log('nothing to bridge: no pending queued tasks');
      return;
    }
    if (r.idempotent || !r.created) console.log(`board already exists at ${r.boardPath} (${r.tasksWritten} task(s)) — no-op`);
    else console.log(`bridged ${r.tasksWritten} task(s) into ${r.boardPath}`);
  });

program
  .command('consume')
  .description('Claim one queued task and run it through devagent task + validation -> PR (FR-WORKER-02)')
  .option('--repo <path>', 'repository', process.cwd())
  .option('--once', 'consume exactly one task then exit', true)
  .option('--auto-pr', 'push branch and open PR when green', false)
  .option('--auto-merge', 'auto-merge the PR when checks pass (--auto-pr required)', false)
  .option('--max-loops <n>', 'retry budget per task', Number)
  .action(async (opts) => {
    const { consumeOnce } = await import('./consume.js');
    const config = loadConfig(opts.repo);
    const r = await consumeOnce({
      repoPath: opts.repo,
      autoPr: Boolean(opts.autoPr),
      autoMerge: Boolean(opts.autoMerge) || Boolean(config.autoMerge),
      maxLoops: opts.maxLoops ?? config.maxLoops,
      timeoutMs: config.timeoutMinutes * 60_000,
    });
    console.log(r.detail);
    if (r.prUrl) console.log(`PR: ${r.prUrl}`);
    if (!r.ok && r.taskId) process.exitCode = 1;
    if (!r.taskId) console.log('No pending tasks.');
  });

program
  .command('reap-stale')
  .description('Find and kill stale opencode/claude/omp worker processes (infinite-retry reaper)')
  .option('--older-than <ms>', 'consider workers stale after this many ms (default 600000 = 10m)', Number, 10 * 60_000)
  .option('--repo <path>', 'scope to workers whose cwd is inside <repo>/.devagent-worktrees (default: all devagent workers)')
  .option('--dry-run', 'list without killing', false)
  .action(async (opts) => {
    const { join } = await import('node:path');
    const { findStaleWorkerPids, reapStaleWorkers } = await import('./resilience/reaper.js');
    const olderThan = Number(opts.olderThan) || 10 * 60_000;
    const cwdPrefix = opts.repo ? join(opts.repo, '.devagent-worktrees') : undefined;
    const reapOpts = cwdPrefix ? { cwdPrefix } : {};
    if (opts.dryRun) {
      const stale = findStaleWorkerPids(olderThan, reapOpts);
      if (stale.length === 0) console.log('No stale workers.');
      else for (const s of stale) console.log(`${s.pid} ${Math.round(s.elapsedMs / 1000)}s ${s.command}`);
      return;
    }
    const killed = reapStaleWorkers(olderThan, false, reapOpts);
    if (killed.length === 0) console.log('No stale workers.');
    else for (const s of killed) console.log(`killed ${s.pid} ${Math.round(s.elapsedMs / 1000)}s ${s.command}`);
  });


program
  .command('record')
  .description('Append a structured event to the orchestration run ledger (Q24)')
  .action(() => {
    // subcommands handle dispatch; bare `record` prints help
    program.commands.find((c) => c.name() === 'record')!.outputHelp();
  });

const recordCmd = program.commands.find((c) => c.name() === 'record')!;
recordCmd
  .command('release')
  .description('Record a release/tag event as a first-class ledger outcome (Q24)')
  .requiredOption('--tag <tag>', 'git tag created, e.g. v0.1.0')
  .requiredOption('--sha <sha>', 'commit SHA the tag points to')
  .option('--repo <path>', 'target repository owning the ledger', process.cwd())
  .option('--source <source>', 'recording source (default cli)', 'cli')
  .action(async (opts) => {
    const tag = String(opts.tag);
    const sha = String(opts.sha);
    const version = tag.replace(/^v/, '');
    appendReleaseRecord(opts.repo, {
      ts: new Date().toISOString(),
      kind: 'event',
      event: 'release-created',
      taskId: `release/${version}`,
      attempt: 1,
      tag,
      sha,
      version,
      source: String(opts.source),
    });
    console.log(`recorded release-created ${version} (${tag} @ ${sha}) -> .devagent/runs/orchestration/events.jsonl`);
  });

program.parseAsync();
