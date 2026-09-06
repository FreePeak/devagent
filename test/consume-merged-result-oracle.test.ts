import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

// The consume wiring under test ends in `gh pr merge` and real worker CLIs;
// both are replaced so tests exercise only the gate logic around them.
vi.mock('../src/integrations/github.js', () => ({
  autoMergePr: vi.fn().mockResolvedValue(undefined),
}));
vi.mock('../src/workers/spawn-utils.js', () => ({
  spawnCli: vi.fn(),
  runCli: vi.fn(),
}));
vi.mock('../src/pipeline.js', () => ({
  runPipeline: vi.fn(),
}));

import { autoMergePr } from '../src/integrations/github.js';
import { runCli } from '../src/workers/spawn-utils.js';
import { runPipeline } from '../src/pipeline.js';
import { runMergedResultOracle, consumeOnce } from '../src/consume.js';
import { enqueueTask } from '../src/queue.js';
import type { PrStatus } from '../src/integrations/autopr.js';

const mockRun = vi.mocked(runCli);
const mockAutoMerge = vi.mocked(autoMergePr);
const mockRunPipeline = vi.mocked(runPipeline);

/**
 * Real git repo with a `main` base and two devagent PR branches. Non-conflicting
 * by default (each adds a distinct file); `conflict: true` makes both add the
 * same path with different content so merging the board yields an add/add
 * conflict. `packageJson: false` drops the JS project so no suite is detectable.
 */
function boardRepo(opts: { conflict?: boolean; packageJson?: boolean } = {}): string {
  const dir = mkdtempSync(join(tmpdir(), 'da-mrg-'));
  const git = (args: string[]) => execFileSync('git', args, { cwd: dir, stdio: 'pipe' });
  git(['init', '-q']);
  git(['config', 'user.email', 't@t']);
  git(['config', 'user.name', 't']);
  if (opts.packageJson !== false) {
    writeFileSync(join(dir, 'package.json'), JSON.stringify({ name: 'fixture', scripts: { test: 'node -e ""' } }));
  }
  writeFileSync(join(dir, 'base.txt'), 'base\n');
  git(['add', '-A']);
  git(['commit', '-qm', 'init']);
  git(['branch', '-M', 'main']);
  // PR #1 branches off main.
  git(['checkout', '-q', '-b', 'devagent/pull-1']);
  writeFileSync(join(dir, opts.conflict ? 'shared.txt' : 'a.txt'), opts.conflict ? 'from-pull-1\n' : 'a\n');
  git(['add', '-A']);
  git(['commit', '-qm', 'p1']);
  // PR #2 branches off main too, so the two diverge.
  git(['checkout', '-q', 'main']);
  git(['checkout', '-q', '-b', 'devagent/pull-2']);
  writeFileSync(join(dir, opts.conflict ? 'shared.txt' : 'b.txt'), opts.conflict ? 'from-pull-2\n' : 'b\n');
  git(['add', '-A']);
  git(['commit', '-qm', 'p2']);
  git(['checkout', '-q', 'main']);
  return dir;
}

/** Mocked runCli: real git for ALL git commands (worktree + merge plumbing), scripted suite run. */
function mockGitCli(suite: { exitCode: number; stdout?: string; stderr?: string }): void {
  mockRun.mockImplementation(async (cmd, args, opts) => {
    if (cmd === 'git') {
      try {
        execFileSync('git', args, { cwd: opts.cwd, stdio: 'pipe' });
        return { exitCode: 0, stdout: '', stderr: '', timedOut: false };
      } catch (err) {
        const e = err as { status?: number; stdout?: Buffer; stderr?: Buffer; message: string };
        return {
          exitCode: typeof e.status === 'number' ? e.status : 1,
          stdout: e.stdout ? e.stdout.toString() : '',
          stderr: e.stderr ? e.stderr.toString() : e.message,
          timedOut: false,
        };
      }
    }
    return { exitCode: suite.exitCode, stdout: suite.stdout ?? '', stderr: suite.stderr ?? '', timedOut: false };
  });
}

/** Board enumeration stub for the oracle's injectable listPrs. */
function board(base = 'main'): () => Promise<PrStatus[]> {
  const head = (number: number, headRefName: string): PrStatus => ({
    number,
    title: '',
    headRefName,
    baseRefName: base,
    state: 'OPEN',
    mergeable: 'MERGEABLE',
    reviewDecision: '',
    headRefOid: '',
    updatedAt: '',
    author: '',
    checks: [],
  });
  return async () => [head(1, 'devagent/pull-1'), head(2, 'devagent/pull-2')];
}

describe('runMergedResultOracle', () => {
  let repo: string;
  beforeEach(() => {
    mockRun.mockReset();
    mockAutoMerge.mockClear();
  });
  afterEach(() => {
    if (repo) rmSync(repo, { recursive: true, force: true });
  });

  it('passes and removes the worktree when the merged board suite is green', async () => {
    repo = boardRepo();
    mockGitCli({ exitCode: 0 });
    const r = await runMergedResultOracle(repo, 'devagent/pull-1', { timeoutMs: 10_000, enabled: true, listPrs: board() });
    expect(r.passed).toBe(true);
    expect(r.skipped).toBe(false);
    expect(r.excerpt).toBeUndefined();
    // Both board heads merged onto the base before the suite ran.
    const mergedHeads = mockRun.mock.calls
      .filter((c) => c[0] === 'git' && c[1][0] === 'merge')
      .map((c) => c[1][2]);
    expect(mergedHeads).toEqual(['devagent/pull-1', 'devagent/pull-2']);
    // worktree removed in the finally path
    const last = mockRun.mock.calls.at(-1)!;
    expect(last[0]).toBe('git');
    expect(last[1]).toEqual(['worktree', 'remove', '--force', expect.any(String)]);
  });

  it('blocks with merge-conflict when two board heads collide', async () => {
    repo = boardRepo({ conflict: true });
    mockGitCli({ exitCode: 0 });
    const r = await runMergedResultOracle(repo, 'devagent/pull-1', { timeoutMs: 10_000, enabled: true, listPrs: board() });
    expect(r.passed).toBe(false);
    expect(r.skipped).toBe(false);
    expect(r.reason).toBe('merge-conflict');
    // the conflicted merge was aborted, leaving the tree clean
    const aborted = mockRun.mock.calls.some((c) => c[0] === 'git' && c[1][0] === 'merge' && c[1][1] === '--abort');
    expect(aborted).toBe(true);
    // the suite never ran: no non-git invocation
    const suiteCalls = mockRun.mock.calls.filter((c) => c[0] !== 'git');
    expect(suiteCalls).toEqual([]);
  });

  it('fails with a 15-line excerpt when the merged board suite is red', async () => {
    repo = boardRepo();
    const lines = Array.from({ length: 40 }, (_, i) => `line-${i}`);
    mockGitCli({ exitCode: 1, stdout: lines.join('\n') });
    const r = await runMergedResultOracle(repo, 'devagent/pull-1', { timeoutMs: 10_000, enabled: true, listPrs: board() });
    expect(r.passed).toBe(false);
    expect(r.skipped).toBe(false);
    expect(r.reason).toBeUndefined();
    const excerptLines = r.excerpt!.split('\n');
    expect(excerptLines.length).toBe(15);
    expect(excerptLines.at(-1)).toBe('line-39');
    expect(r.excerpt).not.toContain('line-0');
  });

  it('skips when no test command is detectable on the merged tree', async () => {
    repo = boardRepo({ packageJson: false });
    mockGitCli({ exitCode: 0 });
    const r = await runMergedResultOracle(repo, 'devagent/pull-1', { timeoutMs: 10_000, enabled: true, listPrs: board() });
    expect(r.passed).toBe(true);
    expect(r.skipped).toBe(true);
    expect(r.reason).toBe('no-test-command');
  });

  it('skips without touching git when the knob is disabled', async () => {
    repo = boardRepo();
    mockRun.mockReset();
    const r = await runMergedResultOracle(repo, 'devagent/pull-1', { timeoutMs: 10_000, enabled: false });
    expect(r.passed).toBe(true);
    expect(r.skipped).toBe(true);
    expect(r.reason).toBe('disabled');
    expect(mockRun).not.toHaveBeenCalled();
  });

  it('degrades to a candidate-only board when the candidate PR is not listed', async () => {
    repo = boardRepo();
    mockGitCli({ exitCode: 0 });
    const r = await runMergedResultOracle(repo, 'devagent/pull-1', {
      timeoutMs: 10_000,
      enabled: true,
      listPrs: async () => [],
    });
    expect(r.passed).toBe(true);
    expect(r.skipped).toBe(false);
    // only the candidate was merged onto the default base
    const mergedHeads = mockRun.mock.calls
      .filter((c) => c[0] === 'git' && c[1][0] === 'merge')
      .map((c) => c[1][2]);
    expect(mergedHeads).toEqual(['devagent/pull-1']);
  });

  it('fail-opens to a candidate-only board when enumeration throws', async () => {
    repo = boardRepo();
    mockGitCli({ exitCode: 0 });
    const r = await runMergedResultOracle(repo, 'devagent/pull-1', {
      timeoutMs: 10_000,
      enabled: true,
      listPrs: async () => {
        throw new Error('gh unavailable');
      },
    });
    expect(r.passed).toBe(true);
    expect(r.skipped).toBe(false);
  });
});

describe('consumeOnce merged-result oracle wiring', () => {
  let repo: string;
  beforeEach(() => {
    mockRun.mockReset();
    mockAutoMerge.mockReset();
    mockAutoMerge.mockResolvedValue(undefined);
    mockRunPipeline.mockReset();
  });
  afterEach(() => {
    if (repo) rmSync(repo, { recursive: true, force: true });
  });

  function stubPipeline(): void {
    mockRunPipeline.mockResolvedValue([
      { stage: 'implement', worker: 'omp', attempts: 1, ok: true, branch: 'devagent/pull-1' },
      { stage: 'validate', passed: true },
      { stage: 'publish', prUrl: 'https://github.com/acme/repo/pull/1', note: 'auto-pr enabled' },
    ]);
  }

  /** runCli mock: real git, `gh pr list` -> board JSON, everything else -> suite. */
  function mockBoardCli(suite: { exitCode: number; stdout?: string; stderr?: string }): void {
    mockRun.mockImplementation(async (cmd, args, opts) => {
      if (cmd === 'git') {
        try {
          execFileSync('git', args, { cwd: opts.cwd, stdio: 'pipe' });
          return { exitCode: 0, stdout: '', stderr: '', timedOut: false };
        } catch (err) {
          const e = err as { status?: number; stdout?: Buffer; stderr?: Buffer; message: string };
          return {
            exitCode: typeof e.status === 'number' ? e.status : 1,
            stdout: e.stdout ? e.stdout.toString() : '',
            stderr: e.stderr ? e.stderr.toString() : e.message,
            timedOut: false,
          };
        }
      }
      if (cmd === 'gh') {
        return {
          exitCode: 0,
          stdout: JSON.stringify([
            { number: 1, headRefName: 'devagent/pull-1', baseRefName: 'main' },
            { number: 2, headRefName: 'devagent/pull-2', baseRefName: 'main' },
          ]),
          stderr: '',
          timedOut: false,
        };
      }
      return { exitCode: suite.exitCode, stdout: suite.stdout ?? '', stderr: suite.stderr ?? '', timedOut: false };
    });
  }

  it('blocks auto-merge with a merge-conflict detail when the board collides', async () => {
    repo = boardRepo({ conflict: true });
    stubPipeline();
    mockBoardCli({ exitCode: 0 });
    enqueueTask(repo, { id: 'TASK-m1', title: 't', goal: 'g' });
    const r = await consumeOnce({ repoPath: repo, autoPr: false, autoMerge: true, maxLoops: 1, timeoutMs: 10_000 });
    expect(r.ok).toBe(true);
    expect(r.detail).toContain('merge-conflict');
    expect(r.merged).toBe(false);
    expect(mockAutoMerge).not.toHaveBeenCalled();
  });

  it('blocks auto-merge when the merged board is red even though the PR branch alone is green', async () => {
    repo = boardRepo();
    stubPipeline();
    // The per-branch oracle runs the suite in a regression-* worktree (green);
    // the merged oracle runs it in a merged-* worktree (red). Distinguish by cwd
    // so this proves the board-level gate catches what the isolated gate misses.
    mockRun.mockImplementation(async (cmd, args, opts) => {
      if (cmd === 'git') {
        try {
          execFileSync('git', args, { cwd: opts.cwd, stdio: 'pipe' });
          return { exitCode: 0, stdout: '', stderr: '', timedOut: false };
        } catch (err) {
          const e = err as { status?: number; stdout?: Buffer; stderr?: Buffer; message: string };
          return {
            exitCode: typeof e.status === 'number' ? e.status : 1,
            stdout: e.stdout ? e.stdout.toString() : '',
            stderr: e.stderr ? e.stderr.toString() : e.message,
            timedOut: false,
          };
        }
      }
      if (cmd === 'gh') {
        return {
          exitCode: 0,
          stdout: JSON.stringify([
            { number: 1, headRefName: 'devagent/pull-1', baseRefName: 'main' },
            { number: 2, headRefName: 'devagent/pull-2', baseRefName: 'main' },
          ]),
          stderr: '',
          timedOut: false,
        };
      }
      const inMerged = String(opts.cwd).includes('merged-');
      return { exitCode: inMerged ? 1 : 0, stdout: inMerged ? 'FAIL on merged board' : '', stderr: '', timedOut: false };
    });
    enqueueTask(repo, { id: 'TASK-m2', title: 't', goal: 'g' });
    const r = await consumeOnce({ repoPath: repo, autoPr: false, autoMerge: true, maxLoops: 1, timeoutMs: 10_000 });
    expect(r.ok).toBe(true);
    expect(r.detail).toContain('merged-regression-failed');
    expect(r.detail).toContain('FAIL on merged board');
    expect(r.merged).toBe(false);
    expect(mockAutoMerge).not.toHaveBeenCalled();
  });

  it('auto-merges when the merged board is green', async () => {
    repo = boardRepo();
    stubPipeline();
    mockBoardCli({ exitCode: 0 });
    enqueueTask(repo, { id: 'TASK-m3', title: 't', goal: 'g' });
    const r = await consumeOnce({ repoPath: repo, autoPr: false, autoMerge: true, maxLoops: 1, timeoutMs: 10_000 });
    expect(r.ok).toBe(true);
    expect(r.merged).toBe(true);
    expect(r.detail).toContain('auto-merged');
    expect(mockAutoMerge).toHaveBeenCalledWith(repo, 'https://github.com/acme/repo/pull/1');
  });
});
