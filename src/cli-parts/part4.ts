import type { Command } from 'commander';
import { existsSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { loadConfig, loadCredentials, credentialStatus, type CleanupMode } from '../config.js';
import { RunLogger } from '../logger.js';
import { ensureStateBranch } from '../git/state-branch.js';
import { runInit, renderInitReport } from '../commands/init.js';
import { buildStatusView, renderStatusCard, statusJson } from '../commands/status.js';
import { buildProbeArgvFor } from '../commands/probe-argv.js';
import { runPreflightGate, PREFLIGHT_ROLES, isPreflightRole } from '../resilience/preflight.js';
import { runPipeline } from '../pipeline.js';
import { buildDeps, buildDryRunDeps } from '../deps.js';
import type { WorkerName } from '../types.js';
import { DEVAGENT_VERSION } from '../version.js';
import { appendReleaseRecord } from '../orchestrator/ledger.js';
import { buildAdjacentCategoryScanText } from '../research/scan-text.js';
import { dispatchRun, resolveCleanup, printOutcomes } from '../cli-helpers.js';

export function registerCliPart4(program: Command): void {
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
    const { listTasks } = await import('../queue.js');
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
    const { readTask, readPrd } = await import('../queue.js');
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
    const { bridgeIfQueued } = await import('../orchestrator/queue-bridge.js');
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
    const { consumeOnce } = await import('../consume.js');
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
    const { findStaleWorkerPids, reapStaleWorkers } = await import('../resilience/reaper.js');
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

}
