/**
 * FR-SIMPLE completion (#144): re-wire init/validate/ledger/queue-list after
 * the base CLI registers them, so we avoid a 90KB full-file rewrite over MCP.
 * Commander .action() replaces the prior handler; .option() is additive.
 */
import type { Command } from 'commander';
import {
  runInit,
  renderInitReport,
  renderOrientation,
  renderSmokeReport,
} from './init.js';
import {
  renderQueueCard,
  renderValidateCards,
  validateJson,
  renderLedgerSummaryCard,
  renderLedgerListLines,
  ledgerJson,
} from './human-card.js';

export function wireFrSimple(program: Command): void {
  const init = program.commands.find((c) => c.name() === 'init');
  if (init) {
    init.option('--smoke', 'after checklist write, run a hermetic fixture smoke (stub → done); default off', false);
    init.description(
      'Guided setup (§21 FR-SIMPLE-01): check prerequisites, write devagent.json with sane defaults, print a plain-language checklist; optional --smoke hermetic fixture',
    );
    init.action(async (opts: { repo: string; worker?: string; model?: string; smoke?: boolean }) => {
      const repoPath = opts.repo;
      const result = await runInit({
        repoPath,
        worker: opts.worker,
        model: opts.model,
        smoke: Boolean(opts.smoke),
      });
      renderInitReport(result);
      if (result.smoke) renderSmokeReport(result.smoke);
      await renderOrientation(repoPath);
      if (!result.ok || (result.smoke && !result.smoke.ok)) process.exitCode = 1;
    });
  }

  const validate = program.commands.find((c) => c.name() === 'validate');
  if (validate) {
    validate.option('--json', 'emit structured gate results as JSON', false);
    validate.description(
      'Run all applicable gates against a repository or worktree (§20.8 card/chip default; --json for scripts)',
    );
    validate.action(async (opts: { worktree: string; json?: boolean }) => {
      const { runMigrationStaticGate } = await import('../validation/runner.js');
      const { runTestGate } = await import('../validation/test-gate.js');

      const probe = runMigrationStaticGate({ repoPath: opts.worktree, classification: 'endpoint-only' });
      const hasMigrations =
        probe.findings.length > 0 || probe.detail !== 'skipped: no migrations in this ticket';

      const g1 = await runTestGate(opts.worktree, 15 * 60_000);
      const rows: import('./human-card.js').ValidateGateRow[] = [{ label: 'G1', result: g1 }];

      if (hasMigrations) {
        const g3 = runMigrationStaticGate({ repoPath: opts.worktree, classification: 'migration-required' });
        rows.push({ label: 'G3', result: g3 });
      } else {
        rows.push({
          label: 'G3',
          result: {
            gate: 'G3-migration-static',
            passed: true,
            skipped: true,
            findings: [],
            detail: 'skipped: no migrations found',
          },
        });
      }

      if (opts.json) {
        console.log(validateJson(rows));
      } else {
        for (const line of renderValidateCards(rows)) console.log(line);
      }

      const failed = rows.some((r) => !r.result.passed && !r.result.skipped);
      process.exitCode = failed ? 1 : 0;
    });
  }

  const ledger = program.commands.find((c) => c.name() === 'ledger');
  if (ledger) {
    ledger.option('--json', 'emit summary or list payload as JSON', false);
    ledger.description(
      'Show the orchestration run ledger (persisted audit verdicts); §20.8 chips by default, --json for scripts',
    );
    ledger.action(async (opts: {
      repo: string;
      task?: string;
      summary?: boolean;
      clusters?: string | boolean;
      json?: boolean;
    }) => {
      if (opts.clusters !== undefined) {
        const { clusterFailures } = await import('../orchestrator/ledger.js');
        const top = typeof opts.clusters === 'string' ? parseInt(opts.clusters, 10) : 5;
        const clusters = clusterFailures(opts.repo);
        const limit = Number.isFinite(top) && top > 0 ? top : 5;
        if (opts.json) {
          // FailureCluster[] is not LedgerSummary | AuditLedgerRecord[] — serialize directly
          console.log(JSON.stringify(clusters.slice(0, limit), null, 2));
          return;
        }
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
        const { summarizeLedger } = await import('../orchestrator/ledger.js');
        const sum = summarizeLedger(opts.repo);
        if (opts.json) {
          console.log(ledgerJson(sum));
          return;
        }
        for (const line of renderLedgerSummaryCard(sum)) console.log(line);
        return;
      }
      const { readLedger } = await import('../orchestrator/ledger.js');
      const records = readLedger(opts.repo, { taskId: opts.task });
      if (opts.json) {
        console.log(ledgerJson(records));
        return;
      }
      for (const line of renderLedgerListLines(records)) console.log(line);
    });
  }

  const queue = program.commands.find((c) => c.name() === 'queue');
  const list = queue?.commands.find((c) => c.name() === 'list');
  if (list) {
    list.description('List queued tasks (§20.8 card/chip default; --json emits the task array)');
    list.action(async (opts: { repo: string; status?: string; json?: boolean }) => {
      const { listTasks, taskCount } = await import('../queue.js');
      const tasks = listTasks(opts.repo, opts.status ? { status: opts.status as 'pending' | 'claimed' | 'done' | 'failed' } : undefined);
      if (opts.json) console.log(JSON.stringify(tasks, null, 2));
      else {
        const counts = taskCount(opts.repo);
        for (const line of renderQueueCard(tasks, counts)) console.log(line);
      }
    });
  }
}
