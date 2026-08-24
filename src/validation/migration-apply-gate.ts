import { existsSync } from 'node:fs';
import { join } from 'node:path';
import type { GateResult } from '../types.js';
import { loadConfig, type DevAgentConfig } from '../config.js';
import { spawnCli } from '../workers/spawn-utils.js';

/**
 * Gate G2 (FR-VALID-02): apply up-migration against a sandboxed database,
 * then down-migration, verifying both succeed. Requires a compose file and
 * configured migration commands; otherwise skips (never silently passes a
 * migration-classified change without evidence).
 */

const COMPOSE_CANDIDATES = ['docker-compose.devagent.yml', 'docker-compose.yml', 'compose.yml'];

export function detectComposeFile(repoPath: string): string | null {
  for (const name of COMPOSE_CANDIDATES) {
    if (existsSync(join(repoPath, name))) return join(repoPath, name);
  }
  return null;
}

export interface G2Config {
  /** Service in the compose file that runs the database */
  dbService: string;
  /** Shell commands to apply up/down migrations inside the repo */
  migrationUp?: string;
  migrationDown?: string;
}

export function extractG2Config(config: DevAgentConfig): G2Config | null {
  const g2 = (config as DevAgentConfig & { g2?: Partial<G2Config> }).g2;
  if (!g2?.dbService || !g2.migrationUp) return null;
  return { dbService: g2.dbService, migrationUp: g2.migrationUp, migrationDown: g2.migrationDown };
}

async function dockerAvailable(cwd: string): Promise<boolean> {
  const r = await spawnCli('docker', ['info', '--format', '{{.ServerVersion}}'], { cwd, timeoutMs: 10_000 });
  return !r.timedOut && r.exitCode === 0;
}

export async function runMigrationApplyGate(
  worktreePath: string,
  timeoutMs: number,
): Promise<GateResult> {
  const composeFile = detectComposeFile(worktreePath);
  if (!composeFile) {
    return { gate: 'G2-migration-apply', passed: true, skipped: true, findings: [], detail: 'skipped: no compose file' };
  }
  if (!(await dockerAvailable(worktreePath))) {
    return { gate: 'G2-migration-apply', passed: true, skipped: true, findings: [], detail: 'skipped: docker not available' };
  }

  let g2: G2Config | null = null;
  try {
    g2 = extractG2Config(loadConfig(worktreePath));
  } catch {
    g2 = null;
  }
  if (!g2) {
    return {
      gate: 'G2-migration-apply',
      passed: true,
      skipped: true,
      findings: [],
      detail: 'skipped: configure g2.dbService + g2.migrationUp in devagent.json',
    };
  }

  const steps: string[] = [];
  const fail = (detail: string): GateResult => ({
    gate: 'G2-migration-apply',
    passed: false,
    findings: [],
    detail,
  });

  // Bring up the database service only
  const up = await spawnCli('docker', ['compose', '-f', composeFile, 'up', '-d', g2.dbService], {
    cwd: worktreePath,
    timeoutMs,
  });
  steps.push(`compose up ${g2.dbService}: exit ${up.exitCode}`);
  if (up.timedOut || up.exitCode !== 0) return fail(steps.join('; ') + `; stderr: ${up.stderr.slice(0, 400)}`);

  // Apply up-migration
  const migrate = await spawnCli('sh', ['-c', g2.migrationUp as string], { cwd: worktreePath, timeoutMs });
  steps.push(`up-migration: exit ${migrate.exitCode}`);
  if (migrate.timedOut || migrate.exitCode !== 0) {
    await teardown(composeFile, worktreePath, timeoutMs);
    return fail(steps.join('; ') + `; stderr: ${migrate.stderr.slice(0, 400)}`);
  }

  // Apply down-migration when configured
  if (g2.migrationDown) {
    const down = await spawnCli('sh', ['-c', g2.migrationDown], { cwd: worktreePath, timeoutMs });
    steps.push(`down-migration: exit ${down.exitCode}`);
    if (down.timedOut || down.exitCode !== 0) {
      await teardown(composeFile, worktreePath, timeoutMs);
      return fail(steps.join('; ') + `; stderr: ${down.stderr.slice(0, 400)}`);
    }
    // Rollback round-trip proof (FR-VALID-02): the up-migration must still
    // apply cleanly on a database the down-migration just rewound. Catches
    // non-idempotent or destructive migrations that pass a single forward run.
    const reUp = await spawnCli('sh', ['-c', g2.migrationUp as string], { cwd: worktreePath, timeoutMs });
    steps.push(`rollback round-trip re-up: exit ${reUp.exitCode}`);
    if (reUp.timedOut || reUp.exitCode !== 0) {
      await teardown(composeFile, worktreePath, timeoutMs);
      return fail(steps.join('; ') + `; stderr: ${reUp.stderr.slice(0, 400)}`);
    }
  }

  await teardown(composeFile, worktreePath, timeoutMs);
  return { gate: 'G2-migration-apply', passed: true, findings: [], detail: steps.join('; ') };
}

async function teardown(composeFile: string, cwd: string, timeoutMs: number): Promise<void> {
  await spawnCli('docker', ['compose', '-f', composeFile, 'down', '-v'], { cwd, timeoutMs });
}
