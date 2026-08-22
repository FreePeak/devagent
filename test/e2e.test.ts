import { describe, expect, it, vi, beforeEach, afterAll } from 'vitest';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

// Only the coding-agent CLI layer is mocked; git, worktrees, gates are real.
vi.mock('../src/workers/index.js', () => ({ getWorker: vi.fn() }));
vi.mock('../src/integrations/linear.js', () => ({
  fetchTicket: vi.fn(),
  postTicketComment: vi.fn(),
}));
// Docker-dependent gates skip honestly when docker is absent; force the skip.
vi.mock('../src/validation/migration-apply-gate.js', () => ({
  runMigrationApplyGate: vi.fn().mockResolvedValue({ passed: true, findings: [], detail: 'skipped: e2e' }),
}));

import { getWorker } from '../src/workers/index.js';
import { fetchTicket } from '../src/integrations/linear.js';
import { runPipeline } from '../src/pipeline.js';
import { RunLogger } from '../src/logger.js';
import type { WorkerResult } from '../src/types.js';

const mockGetWorker = vi.mocked(getWorker);
const mockFetch = vi.mocked(fetchTicket);

let dirs: string[] = [];
function tempRepo(): string {
  const dir = mkdtempSync(join(tmpdir(), 'da-e2e-'));
  dirs.push(dir);
  const run = (cmd: string, args: string[], cwd = dir) =>
    execFileSync(cmd, args, { cwd, stdio: 'pipe' });
  run('git', ['init', '-q']);
  run('git', ['config', 'user.email', 'e2e@test']);
  run('git', ['config', 'user.name', 'e2e']);
  writeFileSync(join(dir, 'package.json'), JSON.stringify({ name: 'fixture', scripts: { test: 'node -e ""' } }));
  mkdirSync(join(dir, 'migrations'));
  writeFileSync(
    join(dir, 'migrations', '001_init.up.sql'),
    'CREATE TABLE users (id int primary key);\nCREATE INDEX idx_users_id ON users(id);\n',
  );
  writeFileSync(
    join(dir, 'migrations', '001_init.down.sql'),
    'DROP TABLE users;\n',
  );
  run('git', ['add', '-A']);
  run('git', ['commit', '-qm', 'init']);
  return dir;
}

afterAll(() => {
  for (const d of dirs) rmSync(d, { recursive: true, force: true });
});

const ticket = {
  id: 'ENG-42',
  title: 'Add GET /health endpoint returning status JSON',
  description: 'Endpoint returns service status as JSON including uptime.',
  labels: [],
  acceptanceCriteria: ['returns 200'],
};

const migrationTicket = {
  id: 'ENG-43',
  title: 'Schema change: alter table users for new profile column',
  description: 'Migration required to extend the users schema. Alter table users accordingly.',
  labels: ['schema'],
  acceptanceCriteria: ['migration applies cleanly'],
};

describe('e2e: pipeline over a real git fixture', () => {
  beforeEach(() => {
    mockGetWorker.mockReset();
    mockFetch.mockReset();
    mockFetch.mockResolvedValue(ticket);
  });

  function okWorker(): void {
    mockGetWorker.mockReturnValue({
      name: 'claude-code' as const,
      spawn: vi.fn().mockResolvedValue({
        exitCode: 0,
        events: [],
        resultText: 'done',
        sessionId: null,
        durationMs: 10,
        timedOut: false,
      } satisfies WorkerResult),
    } as never);
  }

  it('runs implement -> G1 -> G3 -> publish over an isolated worktree', async () => {
    okWorker();
    const repo = tempRepo();
    const log = new RunLogger(mkTempHome());

    const outcomes = await runPipeline(
      {
        ticketId: 'ENG-42',
        repoPath: repo,
        worker: 'claude-code',
        autoPr: false,
        interactive: true,
        maxLoops: 2,
        timeoutMs: 30_000,
        dryRun: false,
      },
      deps(repo),
      log,
    );

    expect(outcomes.map((o) => o.stage)).toEqual(['plan', 'implement', 'validate', 'validate', 'publish']);
    // G3 runs even for endpoint-only tickets (skips honestly) — hence two validates: G1 + G3
    // worktree branch was created and contains the migration fixture
    const branches = execFileSync('git', ['branch', '--list', 'devagent/*'], { cwd: repo }).toString();
    expect(branches).toContain('devagent/ENG-42');
  });

  it('retries with repair prompt when tests fail, then succeeds', async () => {
    let calls = 0;
    const spawnMock = vi.fn().mockImplementation(async (): Promise<WorkerResult> => {
      calls++;
      return {
        exitCode: 0,
        events: [],
        resultText: `attempt ${calls}`,
        sessionId: null,
        durationMs: 10,
        timedOut: false,
      };
    });
    mockGetWorker.mockReturnValue({ name: 'claude-code' as const, spawn: spawnMock } as never);

    const repo = tempRepo();
    const log = new RunLogger(mkTempHome());
    const depsMod = await import('../src/deps.js');

    // First gate call fails, second passes -> exactly one retry
    const { runTestGate } = await import('../src/validation/test-gate.js');
    const gateSpy = vi.spyOn(await import('../src/validation/test-gate.js'), 'runTestGate')
      .mockResolvedValueOnce({ gate: 'G1-tests', passed: false, findings: [], detail: 'FAIL first attempt' })
      .mockResolvedValueOnce({ gate: 'G1-tests', passed: true, findings: [], detail: 'ok' })
      .mockResolvedValue({ gate: 'G1-tests', passed: true, findings: [], detail: 'ok' });

    const outcomes = await runPipeline(
      {
        ticketId: 'ENG-42',
        repoPath: repo,
        worker: 'claude-code',
        autoPr: false,
        interactive: true,
        maxLoops: 3,
        timeoutMs: 30_000,
        dryRun: false,
      },
      deps(repo),
      log,
    );

    expect(spawnMock).toHaveBeenCalledTimes(2);
    // gates fire per attempt inside implementStage plus once at pipeline level
    expect(gateSpy).toHaveBeenCalledTimes(3);
    void runTestGate;
    void depsMod;
    expect(outcomes.at(-1)).toMatchObject({ stage: 'publish' });
  });

  it('blocks a destructive migration via G3', async () => {
    okWorker();
    mockFetch.mockResolvedValue(migrationTicket);
    const repo = tempRepo();
    writeFileSync(
      join(repo, 'migrations', '002_drop.up.sql'),
      'DROP TABLE users;\n',
    );
    writeFileSync(join(repo, 'migrations', '002_drop.down.sql'), 'CREATE TABLE users (id int primary key);\n');
    execFileSync('git', ['add', '-A'], { cwd: repo });
    execFileSync('git', ['commit', '-qm', 'destructive change'], { cwd: repo });

    const log = new RunLogger(mkTempHome());
    const outcomes = await runPipeline(
      {
        ticketId: 'ENG-43',
        repoPath: repo,
        worker: 'claude-code',
        autoPr: false,
        interactive: true,
        maxLoops: 1,
        timeoutMs: 30_000,
        dryRun: false,
      },
      deps(repo),
      log,
    );

    expect(outcomes.at(-1)).toMatchObject({ stage: 'failed', reason: /migration static gate/ });
  });
});

import type { PipelineDeps } from '../src/pipeline.js';
import type { RunConfig } from '../src/types.js';
import { runMigrationStaticGate } from '../src/validation/runner.js';

function deps(repo: string): PipelineDeps {
  const cfg: Omit<RunConfig, 'ticketId'> = {
    repoPath: repo,
    worker: 'claude-code',
    autoPr: false,
    interactive: true,
    maxLoops: 3,
    timeoutMs: 30_000,
    dryRun: false,
  };
  // Real deps except Linear (mocked module) and publish (no remote).
  return {
    fetchTicket: mockFetch as unknown as PipelineDeps['fetchTicket'],
    runGateG1: (worktreePath, timeoutMs) =>
      import('../src/validation/test-gate.js').then((m) => m.runTestGate(worktreePath, timeoutMs)),
    runGateG2: () =>
      Promise.resolve({ passed: true, detail: 'skipped: e2e fixture has no docker' }),
    runGateG3: (repoPath, classification) => {
      const r = runMigrationStaticGate({ repoPath, classification });
      return { passed: r.passed, findings: r.findings, detail: r.detail };
    },
    implementStage: async (_c, plan, lg) => {
      const mod = await import('../src/deps.js');
      const d = mod.buildDeps({ linearApiKey: 'x' }, { ...cfg, autoPr: false }, lg);
      return d.implementStage!(_c, plan, lg);
    },
  };
}

let homeCount = 0;
function mkTempHome(): string {
  const d = mkdtempSync(join(tmpdir(), 'da-home-'));
  dirs.push(d);
  return `${d}/home${homeCount++}`;
}
