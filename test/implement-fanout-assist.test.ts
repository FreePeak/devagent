import { describe, expect, it, vi, beforeEach, afterAll } from 'vitest';
import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

vi.mock('../src/workers/index.js', () => ({ getWorker: vi.fn() }));

import { getWorker } from '../src/workers/index.js';
import { buildDeps } from '../src/deps.js';
import { RunLogger } from '../src/logger.js';
import { planFromTicket } from '../src/planner.js';

const mockGetWorker = vi.mocked(getWorker);

const plan = planFromTicket({
  id: 'ENG-77',
  title: 'Add GET /health endpoint returning status JSON',
  description: 'Endpoint returns service status as JSON including uptime.',
  labels: [],
  acceptanceCriteria: ['returns 200'],
});

const dirs: string[] = [];
afterAll(() => {
  for (const d of dirs) rmSync(d, { recursive: true, force: true });
});

function initRepo(): string {
  const dir = mkdtempSync(join(tmpdir(), 'da-assist-'));
  dirs.push(dir);
  const repo = join(dir, 'repo');
  mkdirSync(repo);
  execFileSync('git', ['init'], { cwd: repo });
  execFileSync('git', ['config', 'user.email', 'test@example.com'], { cwd: repo });
  execFileSync('git', ['config', 'user.name', 'test'], { cwd: repo });
  writeFileSync(join(repo, 'f.txt'), 'x\n');
  execFileSync('git', ['add', '.'], { cwd: repo });
  execFileSync('git', ['commit', '-m', 'init'], { cwd: repo });
  return repo;
}

function workerMock(name: string, writeFile?: string) {
  return {
    name,
    spawn: vi.fn().mockImplementation(async ({ cwd }: { cwd: string }) => {
      if (writeFile) writeFileSync(join(cwd, writeFile), 'change\n');
      return { exitCode: 0, events: [], resultText: null, sessionId: null, durationMs: 10, timedOut: false };
    }),
  };
}

function fanoutCfg(repoPath: string) {
  return {
    repoPath,
    maxLoops: 1,
    timeoutMs: 30_000,
    worker: 'both' as const,
    autoPr: false,
  };
}

// Temp repos have no package.json/go.mod, so runTestGate skips -> both legs
// pass scoring; claude-code wins the tie-break.
describe('implementStage fan-out merge-assist (PRD section 17 Phase 4)', () => {
  beforeEach(() => {
    mockGetWorker.mockReset();
  });

  const logDir = () => mkdtempSync(join(tmpdir(), 'da-assist-log-'));
  const logger = (dir: string) => new RunLogger(dir);

  it('commits winner changes, renames branch to canonical, cleans up losers', async () => {
    const repo = initRepo();
    const claude = workerMock('claude-code', 'winner.txt');
    const open = workerMock('opencode', 'loser.txt');
    mockGetWorker.mockImplementation((n) => (n === 'claude-code' ? claude : open) as never);
    const cfg = fanoutCfg(repo);
    const log = logger(logDir());

    const d = buildDeps({ linearApiKey: 'x' }, cfg, log);
    const result = await d.implementStage!(cfg, plan, log);

    expect(result.ok).toBe(true);
    expect(result.worker).toBe('claude-code');

    const winnerWt = join(repo, '.devagent-worktrees', 'ENG-77-claude-code');
    // Branch renamed to the canonical devagent/<ticket.id> ref
    const currentBranch = execFileSync('git', ['rev-parse', '--abbrev-ref', 'HEAD'], {
      cwd: winnerWt,
    }).toString().trim();
    expect(currentBranch).toBe('devagent/ENG-77');
    // Uncommitted winner changes were committed (clean tree, message references ticket and worker)
    const status = execFileSync('git', ['status', '--porcelain'], { cwd: winnerWt }).toString();
    expect(status).toBe('');
    const subject = execFileSync('git', ['log', '-1', '--format=%s'], { cwd: winnerWt }).toString().trim();
    expect(subject).toContain('ENG-77');
    expect(subject).toContain('claude-code');
    expect(existsSync(join(winnerWt, 'winner.txt'))).toBe(true);

    // Losing leg worktree and branch are gone
    const loserWt = join(repo, '.devagent-worktrees', 'ENG-77-opencode');
    expect(existsSync(loserWt)).toBe(false);
    const branches = execFileSync('git', ['branch', '--list'], { cwd: repo }).toString();
    expect(branches).not.toContain('devagent/ENG-77-opencode');
    expect(branches).toContain('devagent/ENG-77');
  });

  it('tolerates a winner with nothing to commit', async () => {
    const repo = initRepo();
    const claude = workerMock('claude-code'); // writes no files
    const open = workerMock('opencode');
    mockGetWorker.mockImplementation((n) => (n === 'claude-code' ? claude : open) as never);
    const cfg = fanoutCfg(repo);
    const log = logger(logDir());

    const d = buildDeps({ linearApiKey: 'x' }, cfg, log);
    const result = await d.implementStage!(cfg, plan, log);

    expect(result.ok).toBe(true);
    const winnerWt = join(repo, '.devagent-worktrees', 'ENG-77-claude-code');
    const currentBranch = execFileSync('git', ['rev-parse', '--abbrev-ref', 'HEAD'], {
      cwd: winnerWt,
    }).toString().trim();
    expect(currentBranch).toBe('devagent/ENG-77');
    // No extra commit was created beyond the initial one
    const subject = execFileSync('git', ['log', '-1', '--format=%s'], { cwd: winnerWt }).toString().trim();
    expect(subject).toBe('init');
    // Loser cleanup still ran
    expect(existsSync(join(repo, '.devagent-worktrees', 'ENG-77-opencode'))).toBe(false);
  });

  it('single worker: default auto-cleanup removes the tree on success, snapshotting to the branch', async () => {
    const repo = initRepo();
    const claude = workerMock('claude-code', 'single.txt');
    mockGetWorker.mockReturnValue(claude as never);
    const cfg = { ...fanoutCfg(repo), worker: 'claude-code' as const };
    const log = logger(logDir());

    const d = buildDeps({ linearApiKey: 'x' }, cfg, log);
    const result = await d.implementStage!(cfg, plan, log);

    expect(result.ok).toBe(true);
    const wt = join(repo, '.devagent-worktrees', 'ENG-77');
    // Cleanup 'auto' (default): success removes the run worktree...
    expect(existsSync(wt)).toBe(false);
    // ...but nothing was lost: uncommitted output was snapshotted onto the
    // canonical branch before removal.
    expect(
      execFileSync('git', ['branch', '--list', 'devagent/ENG-77'], { cwd: repo }).toString(),
    ).toContain('devagent/ENG-77');
    const subject = execFileSync('git', ['log', '-1', '--format=%s', 'devagent/ENG-77'], {
      cwd: repo,
    }).toString().trim();
    expect(subject).toContain('auto-cleanup snapshot');
    const files = execFileSync('git', ['show', '--name-only', '--format=', 'devagent/ENG-77'], {
      cwd: repo,
    }).toString().trim();
    expect(files).toContain('single.txt');
  });

  it("single worker: cleanup 'keep' preserves the worktree for inspection", async () => {
    const repo = initRepo();
    const claude = workerMock('claude-code', 'single.txt');
    mockGetWorker.mockReturnValue(claude as never);
    const cfg = { ...fanoutCfg(repo), worker: 'claude-code' as const, cleanup: 'keep' as const };
    const log = logger(logDir());

    const d = buildDeps({ linearApiKey: 'x' }, cfg, log);
    const result = await d.implementStage!(cfg, plan, log);

    expect(result.ok).toBe(true);
    const wt = join(repo, '.devagent-worktrees', 'ENG-77');
    expect(existsSync(wt)).toBe(true);
    const currentBranch = execFileSync('git', ['rev-parse', '--abbrev-ref', 'HEAD'], {
      cwd: wt,
    }).toString().trim();
    expect(currentBranch).toBe('devagent/ENG-77');
  });
});
