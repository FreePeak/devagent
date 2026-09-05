import { Command } from 'commander';
import { dispatchRun, resolveCleanup, printOutcomes, parseConcurrency } from '../cli-helpers.js';

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


export function registerCliPart1(program: Command): void {
program
  .command('validate')
  .description('Run all applicable gates against a repository or worktree')
  .option('--worktree <path>', 'path to repository/worktree', process.cwd())
  .action(async (opts) => {
    const { runMigrationStaticGate } = await import('../validation/runner.js');
    const { runTestGate } = await import('../validation/test-gate.js');

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
      const { readProxyState } = await import('../resilience/proxy-state.js');
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
      const { validateModelId } = await import('../workers/model-id.js');
      const { loadConfig: loadModelCfg } = await import('../config.js');
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
      const { ResourceGovernor } = await import('../orchestrator/governor.js');
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
    const { writeDashboard } = await import('../observe.js');
    const home = process.env.DEVAGENT_HOME || join(process.env.HOME || '.', '.devagent');
    const { path, runs } = writeDashboard(home);
    console.log(`${runs} run(s) -> ${path}`);
  });

}
