#!/usr/bin/env node
/**
 * Dogfood dispatcher: run DevAgent tickets from docs/prd-dogfood-r1.md
 * through DevAgent's own pipeline (runPipeline + worktree isolation + G1),
 * with synthetic ticket specs standing in for Linear.
 * Usage: npx tsx scripts/dogfood.ts DA-DOG-01 [DA-DOG-02 ...]
 */
import { RunLogger } from '../src/logger.js';
import { runPipeline } from '../src/pipeline.js';
import type { PipelineDeps } from '../src/pipeline.js';
import type { TicketSpec } from '../src/types.js';

const TICKETS: Record<string, TicketSpec> = {
  'DA-DOG-01': {
    id: 'DA-DOG-01',
    title: '--dry-run must work without network credentials',
    description: [
      'devagent run --ticket X --dry-run exits 1 with "LINEAR_API_KEY is not set" despite advertising plan-only offline behavior.',
      '',
      'Fix: in src/cli.ts, skip the credential requirement when opts.dryRun is set; inject a synthetic fetchTicket returning a stub TicketSpec so the pipeline plans offline.',
      '',
      'Acceptance criteria:',
      '- devagent run --ticket ANY --dry-run succeeds with no environment credentials',
      '- Dry-run output prints the plan summary and classification',
      '- Dry-run never calls fetchTicket/postTicketComment/workers/remotes',
    ].join('\n'),
    labels: ['dogfood', 'bug'],
    acceptanceCriteria: [
      'dry-run succeeds without credentials',
      'plan summary printed',
      'no network calls in dry-run',
    ],
    url: 'docs/prd-dogfood-r1.md',
    trackerInternalId: 'DA-DOG-01',
  },
  'DA-DOG-02': {
    id: 'DA-DOG-02',
    title: 'fleet validates arguments in the wrong order',
    description: [
      'devagent fleet --ticket E-1 --repo badformat reports the credential error instead of Invalid --repo entry "badformat" (expected name=path).',
      '',
      'Fix: move --repo format validation before the LINEAR_API_KEY check in the fleet action of src/cli.ts.',
      '',
      'Acceptance criteria:',
      '- Malformed --repo entries produce the invalid-entry message regardless of credentials',
      '- Argument validation precedes credential validation',
      '- Exit code remains 1 on either failure',
    ].join('\n'),
    labels: ['dogfood', 'ux'],
    acceptanceCriteria: ['repo arg validated first', 'exit code 1 on failure'],
    url: 'docs/prd-dogfood-r1.md',
    trackerInternalId: 'DA-DOG-02',
  },
  'DA-DOG-03': {
    id: 'DA-DOG-03',
    title: 'Re-running a ticket loses worktree isolation',
    description: [
      'Second run for the same ticket throws on branch creation (devagent/<ticket> already exists) and silently falls back to executing the worker in the repo root, violating FR-IMPL-01.',
      '',
      'Fix in src/git/worktree.ts createWorktree:',
      '1. If branch devagent/<ticket> exists, reuse it: if a worktree dir already exists for it use that, else create a new worktree attached to the existing branch (git worktree add <path> <branch>).',
      '2. In src/deps.ts implementStage: when repoPath is a git repository and worktree creation fails, abort the run with a clear error instead of falling back to repo root.',
      '',
      'Acceptance criteria:',
      '- Existing branch is reused on re-run',
      '- No repo-root fallback for git repos; clear abort error instead',
      '- Unit tests cover both reuse paths',
    ].join('\n'),
    labels: ['dogfood', 'defect'],
    acceptanceCriteria: ['branch reused on re-run', 'no repo-root fallback', 'tests cover reuse'],
    url: 'docs/prd-dogfood-r1.md',
    trackerInternalId: 'DA-DOG-03',
  },
  'DA-DOG-04': {
    id: 'DA-DOG-04',
    title: 'Manual run bypasses the latest-wins lock registry',
    description: [
      'Only webhook dispatchRun acquires the per-ticket lock; two concurrent devagent run --ticket X invocations race on the same worktree/branch.',
      '',
      'Fix: in the run action of src/cli.ts, acquire tryAcquireRun before fetching and release in finally; exit non-zero with "Run for <ticket> already active" when the lock is held.',
      '',
      'Acceptance criteria:',
      '- run acquires tryAcquireRun before fetch and releases in finally',
      '- Second concurrent run exits non-zero with the already-active message',
      '- Stale locks (>1h) are broken per registry semantics',
    ].join('\n'),
    labels: ['dogfood', 'gap'],
    acceptanceCriteria: ['lock acquired in run', 'concurrent run rejected', 'stale locks broken'],
    url: 'docs/prd-dogfood-r1.md',
    trackerInternalId: 'DA-DOG-04',
  },
};

async function main(): Promise<void> {
  const ids = process.argv.slice(2);
  const unknown = ids.filter((id) => !TICKETS[id]);
  if (ids.length === 0 || unknown.length > 0) {
    console.error(`Usage: dogfood.ts <${Object.keys(TICKETS).join('|')}> [...]${unknown.length ? ` (unknown: ${unknown.join(',')})` : ''}`);
    process.exit(1);
  }

  const repoPath = new URL('..', import.meta.url).pathname;

  for (const id of ids) {
    const log = new RunLogger();
    log.info('fetch', `Dogfood run ${log.runId} starting`, { ticket: id });

    // Same deps shape the webhook server builds; fetchTicket serves the
    // PRD-derived spec locally (offline), publish stays local (no remote).
    const deps: PipelineDeps = {
      fetchTicket: async () => TICKETS[id]!,
      runGateG3: (rp, classification) => {
        // synchronous static gate via dynamic import shim
        throw new Error('replaced below');
      },
    };
    const { runMigrationStaticGate } = await import('../src/validation/runner.js');
    deps.runGateG3 = (rp, classification) => {
      const r = runMigrationStaticGate({ repoPath: rp, classification });
      return { passed: r.passed, findings: r.findings as never[], detail: r.detail };
    };

    const cfg = {
      ticketId: id,
      repoPath,
      worker: 'claude-code' as const,
      autoPr: false,
      interactive: true,
      maxLoops: 2,
      timeoutMs: 25 * 60_000,
      dryRun: false,
    };

    // implementStage/publishStage come from the shared factory like cli.ts does
    const { buildDeps } = await import('../src/deps.js');
    const creds = { linearApiKey: 'unused-offline' };
    const fullDeps = { ...buildDeps(creds, cfg, log), ...deps, fetchTicket: deps.fetchTicket };

    const outcomes = await runPipeline(cfg, fullDeps, log);
    for (const o of outcomes) {
      if (o.stage === 'failed') console.error(`[${id}] FAILED: ${o.reason}`);
      else if (o.stage === 'publish') console.log(`[${id}] publish: ${o.note}`);
      else console.log(`[${id}] ${o.stage}: ok`);
    }
    console.log(`[${id}] log: ${log.path}`);
  }
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
