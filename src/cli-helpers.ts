import { join } from 'node:path';
import { loadConfig, loadCredentials, type CleanupMode } from './config.js';
import { RunLogger } from './logger.js';
import { runPipeline } from './pipeline.js';
import { buildDeps } from './deps.js';

export async function dispatchRun(
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

export function resolveCleanup(flag: string | undefined, fileConfig: CleanupMode | undefined): CleanupMode {
  const value = (flag ?? fileConfig ?? 'auto') as CleanupMode;
  if (!['auto', 'keep', 'always'].includes(value)) {
    throw new Error(`Invalid --cleanup "${value}"; expected auto, keep, or always`);
  }
  return value;
}

export function printOutcomes(outcomes: Array<{ stage: string } & Record<string, unknown>>): void {
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

export function parseConcurrency(v: string): number | 'auto' {
  if (v === 'auto' || v.toLowerCase() === 'auto') return 'auto';
  const n = Number(v);
  if (!Number.isFinite(n) || n < 1) throw new Error(`Invalid --concurrency "${v}"; expected positive integer or "auto"`);
  return Math.floor(n);
}
