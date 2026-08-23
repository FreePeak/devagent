import { describe, expect, it, vi } from 'vitest';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { runPipeline, type PipelineDeps } from '../src/pipeline.js';
import { buildDryRunDeps } from '../src/deps.js';
import { RunLogger } from '../src/logger.js';
import type { RunConfig } from '../src/types.js';

const baseCfg = (over: Partial<RunConfig> = {}): RunConfig => ({
  ticketId: 'ANY-42',
  repoPath: '.',
  worker: 'claude-code',
  autoPr: false,
  interactive: true,
  maxLoops: 3,
  timeoutMs: 60_000,
  dryRun: true,
  ...over,
});

const tmpLog = () => {
  const dir = mkdtempSync(join(tmpdir(), 'da-dryrun-'));
  return { log: new RunLogger(dir), dir };
};

describe('buildDryRunDeps', () => {
  it('returns a sufficient synthetic ticket without credentials or network', async () => {
    const deps = buildDryRunDeps('ANY-42');
    const ticket = await deps.fetchTicket('ANY-42');
    expect(ticket.id).toBe('ANY-42');
    expect(ticket.title).toContain('ANY-42');
    // Spec check must pass so the clarify path (and postTicketComment) is unreachable
    expect(ticket.description.length).toBeGreaterThanOrEqual(40);
  });

  it('exposes no network-touching stages', () => {
    const deps = buildDryRunDeps('ANY-42') as Record<string, unknown>;
    for (const key of ['postTicketComment', 'implementStage', 'runGateG1', 'runGateG2', 'runGateG4', 'publishStage']) {
      expect(deps[key]).toBeUndefined();
    }
  });
});

describe('dry-run pipeline offline behavior', () => {
  it('plans and classifies without any network calls', async () => {
    const { log, dir } = tmpLog();
    try {
      const deps = buildDryRunDeps('ANY-42') as PipelineDeps & Record<string, unknown>;
      const outcomes = await runPipeline(baseCfg(), deps, log);
      expect(outcomes).toHaveLength(1);
      // Plan summary + classification are printed by printOutcomes from this outcome
      expect(outcomes[0]).toMatchObject({ stage: 'plan', summary: 'endpoint-only' });
      expect(outcomes[0].tasks).toEqual([
        'Define route/handler for the new endpoint',
        'Add request/response types per repo conventions',
        'Write integration tests hitting the endpoint',
      ]);
      // No remote-mutating dependency exists to be called
      expect(deps.postTicketComment).toBeUndefined();
      expect(deps.publishStage).toBeUndefined();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('runs the real CLI with no credentials and prints plan summary + classification', () => {
    const repoRoot = join(tmpdir(), 'da-dryrun-cli-repo');
    mkdtempSync(repoRoot);
    try {
      const out = execFileSync(
        'npx',
        ['tsx', 'src/cli.ts', 'run', '--ticket', 'ANY-42', '--dry-run', '--repo', repoRoot],
        {
          cwd: join(import.meta.dirname, '..'),
          stdio: 'pipe',
          env: {
            PATH: process.env.PATH,
            HOME: process.env.HOME,
            // Explicitly absent: LINEAR_API_KEY, JIRA_DOMAIN/JIRA_EMAIL/JIRA_API_TOKEN
          },
        },
      ).toString();
      expect(out).toContain('Plan (endpoint-only):');
      expect(out).toContain('Define route/handler for the new endpoint');
    } finally {
      rmSync(repoRoot, { recursive: true, force: true });
    }
  });
});
