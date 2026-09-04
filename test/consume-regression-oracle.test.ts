import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync, mkdirSync, existsSync } from 'node:fs';
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
import { runRegressionOracle, consumeOnce } from '../src/consume.js';
import { enqueueTask } from '../src/queue.js';

const mockRun = vi.mocked(runCli);
const mockAutoMerge = vi.mocked(autoMergePr);
const mockRunPipeline = vi.mocked(runPipeline);

/** Real git repo with one commit so `git worktree add` can materialize branches. */
function gitRepo(files: Record<string, string> = {}): string {
  const dir = mkdtempSync(join(tmpdir(), 'da-reg-'));
  const git = (args: string[]) => execFileSync('git', args, { cwd: dir, stdio: 'pipe' });
  git(['init', '-q']);
  git(['config', 'user.email', 't@t']);
  git(['config', 'user.name', 't']);
  writeFileSync(join(dir, 'package.json'), JSON.stringify({ name: 'fixture', scripts: { test: 'node -e ""' } }));
  for (const [name, content] of Object.entries(files)) {
    const p = join(dir, name);
    mkdirSync(join(p, '..'), { recursive: true });
    writeFileSync(p, content);
  }
  git(['add', '-A']);
  git(['commit', '-qm', 'init']);
  git(['branch', 'devagent/pull-1']);
  return dir;
}

/** Drop package.json and re-point the PR branch at the non-JS tree. */
function dropPackageJson(repo: string): void {
  rmSync(join(repo, 'package.json'));
  execFileSync('git', ['add', '-A'], { cwd: repo });
  execFileSync('git', ['commit', '-qm', 'non-js'], { cwd: repo });
  execFileSync('git', ['branch', '-f', 'devagent/pull-1'], { cwd: repo });
}

/** Mocked runCli: real git worktree plumbing, scripted suite run. */
function mockRunCli(suite: { exitCode: number; stdout?: string; stderr?: string }): void {
  mockRun.mockImplementation(async (cmd, args, opts) => {
    if (cmd === 'git' && args[0] === 'worktree') {
      try {
        execFileSync('git', args, { cwd: opts.cwd, stdio: 'pipe' });
        return { exitCode: 0, stdout: '', stderr: '', timedOut: false };
      } catch (err) {
        return { exitCode: 1, stdout: '', stderr: String((err as Error).message), timedOut: false };
      }
    }
    return { exitCode: suite.exitCode, stdout: suite.stdout ?? '', stderr: suite.stderr ?? '', timedOut: false };
  });
}

describe('runRegressionOracle', () => {
  let repo: string;
  beforeEach(() => {
    mockRun.mockReset();
    mockAutoMerge.mockClear();
  });
  afterEach(() => {
    if (repo) rmSync(repo, { recursive: true, force: true });
  });

  it('passes and removes the worktree when the suite is green', async () => {
    repo = gitRepo();
    mockRunCli({ exitCode: 0 });
    const r = await runRegressionOracle(repo, 'devagent/pull-1', { timeoutMs: 10_000, enabled: true });
    expect(r.passed).toBe(true);
    expect(r.skipped).toBe(false);
    expect(r.excerpt).toBeUndefined();
    // worktree removed in the finally path
    const last = mockRun.mock.calls.at(-1)!;
    expect(last[0]).toBe('git');
    expect(last[1]).toEqual(['worktree', 'remove', '--force', expect.any(String)]);
  });

  it('fails with a 15-line output excerpt on a red suite', async () => {
    repo = gitRepo();
    const lines = Array.from({ length: 40 }, (_, i) => `line-${i}`);
    mockRunCli({ exitCode: 1, stdout: lines.join('\n') });
    const r = await runRegressionOracle(repo, 'devagent/pull-1', { timeoutMs: 10_000, enabled: true });
    expect(r.passed).toBe(false);
    expect(r.skipped).toBe(false);
    expect(r.excerpt).toBeDefined();
    const excerptLines = r.excerpt!.split('\n');
    expect(excerptLines.length).toBe(15);
    expect(excerptLines.at(-1)).toBe('line-39');
    expect(r.excerpt).not.toContain('line-0');
  });

  it('skips when no test command is detectable', async () => {
    repo = gitRepo();
    dropPackageJson(repo);
    mockRunCli({ exitCode: 0 });
    const r = await runRegressionOracle(repo, 'devagent/pull-1', { timeoutMs: 10_000, enabled: true });
    expect(r.passed).toBe(true);
    expect(r.skipped).toBe(true);
    expect(r.reason).toBe('no-test-command');
  });

  it('skips when the orchestrate.regressionOracle knob is false', async () => {
    repo = gitRepo({ 'devagent.json': JSON.stringify({ orchestrate: { regressionOracle: false } }) });
    mockRunCli({ exitCode: 0 });
    const r = await runRegressionOracle(repo, 'devagent/pull-1', { timeoutMs: 10_000 });
    expect(r.passed).toBe(true);
    expect(r.skipped).toBe(true);
    expect(r.reason).toBe('disabled');
    expect(mockRun).not.toHaveBeenCalled();
  });

  it('resolves the knob from the repo config when enabled is omitted', async () => {
    repo = gitRepo({
      'devagent.json': JSON.stringify({ orchestrate: { regressionOracle: true } }),
      'pyproject.toml': '[tool.pytest]\n',
    });
    dropPackageJson(repo);
    mockRunCli({ exitCode: 0 });
    const r = await runRegressionOracle(repo, 'devagent/pull-1', { timeoutMs: 10_000 });
    expect(r.passed).toBe(true);
    expect(r.skipped).toBe(false);
    // python convention resolved from the worktree
    const suiteCall = mockRun.mock.calls.find((c) => c[0] === 'python3');
    expect(suiteCall?.[1]).toEqual(['-m', 'pytest']);
  });

  it('runs the suite in a non-JS repo when a testCommand is declared and the knob opts in', async () => {
    repo = gitRepo({
      'devagent.json': JSON.stringify({ testCommand: 'make test' }),
      'pyproject.toml': '[tool.pytest]\n',
    });
    dropPackageJson(repo);
    mockRunCli({ exitCode: 1, stderr: 'make: *** [test] Error 1' });
    const r = await runRegressionOracle(repo, 'devagent/pull-1', { timeoutMs: 10_000 });
    expect(r.passed).toBe(false);
    expect(r.skipped).toBe(false);
    const suiteCall = mockRun.mock.calls.find((c) => c[0] === 'make');
    expect(suiteCall?.[1]).toEqual(['test']);
    expect(r.excerpt).toContain('make: *** [test] Error 1');
  });

  it('installs npm deps with ci --ignore-scripts when the worktree has a lockfile', async () => {
    repo = gitRepo({
      'package-lock.json': JSON.stringify({ name: 'fixture', lockfileVersion: 3, packages: { '': {} } }),
    });
    mockRunCli({ exitCode: 0 });
    const r = await runRegressionOracle(repo, 'devagent/pull-1', { timeoutMs: 10_000, enabled: true });
    expect(r.passed).toBe(true);
    const install = mockRun.mock.calls.find((c) => c[0] === 'npm' && c[1][0] === 'ci');
    expect(install?.[1]).toEqual(['ci', '--ignore-scripts']);
    expect(install?.[2]?.cwd).toContain('.devagent-worktrees');
  });

  it('skips fail-open with install-failed when the dependency install fails', async () => {
    repo = gitRepo({
      'package-lock.json': JSON.stringify({ name: 'fixture', lockfileVersion: 3, packages: { '': {} } }),
    });
    mockRun.mockImplementation(async (cmd, args, opts) => {
      if (cmd === 'git' && args[0] === 'worktree') {
        try {
          execFileSync('git', args, { cwd: opts.cwd, stdio: 'pipe' });
          return { exitCode: 0, stdout: '', stderr: '', timedOut: false };
        } catch (err) {
          return { exitCode: 1, stdout: '', stderr: String((err as Error).message), timedOut: false };
        }
      }
      if (cmd === 'npm' && args[0] === 'ci') return { exitCode: 1, stdout: '', stderr: 'npm ERR! network', timedOut: false };
      return { exitCode: 0, stdout: '', stderr: '', timedOut: false };
    });
    const r = await runRegressionOracle(repo, 'devagent/pull-1', { timeoutMs: 10_000, enabled: true });
    expect(r.passed).toBe(true);
    expect(r.skipped).toBe(true);
    expect(r.reason).toBe('install-failed');
    // the red-on-missing-modules suite never ran
    const suiteCalls = mockRun.mock.calls.filter((c) => c[0] !== 'git' && c[0] !== 'npm');
    expect(suiteCalls).toEqual([]);
  });

  it('runs the suite without an install when the worktree has no lockfile', async () => {
    repo = gitRepo();
    mockRunCli({ exitCode: 0 });
    await runRegressionOracle(repo, 'devagent/pull-1', { timeoutMs: 10_000, enabled: true });
    const install = mockRun.mock.calls.find((c) => c[0] === 'npm' && c[1][0] === 'ci');
    expect(install).toBeUndefined();
  });

  it('skips with worktree-failed when git worktree add fails', async () => {
    repo = gitRepo();
    mockRun.mockImplementation(async (cmd, args) => {
      if (cmd === 'git' && args[0] === 'worktree' && args[1] === 'add') {
        return { exitCode: 128, stdout: '', stderr: 'fatal: invalid reference', timedOut: false };
      }
      return { exitCode: 0, stdout: '', stderr: '', timedOut: false };
    });
    const r = await runRegressionOracle(repo, 'devagent/pull-1', { timeoutMs: 10_000, enabled: true });
    expect(r.passed).toBe(true);
    expect(r.skipped).toBe(true);
    expect(r.reason).toBe('worktree-failed');
  });
});

describe('consumeOnce regression gate wiring', () => {
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

  it('blocks merge with a regression-failed detail on a red suite', async () => {
    repo = gitRepo();
    stubPipeline();
    mockRunCli({ exitCode: 1, stderr: 'FAIL src/x.test.ts' });
    enqueueTask(repo, { id: 'TASK-reg1', title: 't', goal: 'g' });
    const r = await consumeOnce({ repoPath: repo, autoPr: false, autoMerge: true, maxLoops: 1, timeoutMs: 10_000 });
    expect(r.ok).toBe(true);
    expect(r.detail).toContain('regression-failed');
    expect(r.detail).toContain('FAIL src/x.test.ts');
    expect(r.merged).toBe(false);
    expect(mockAutoMerge).not.toHaveBeenCalled();
  });

  it('merges when the suite is green', async () => {
    repo = gitRepo();
    stubPipeline();
    mockRunCli({ exitCode: 0 });
    enqueueTask(repo, { id: 'TASK-reg2', title: 't', goal: 'g' });
    const r = await consumeOnce({ repoPath: repo, autoPr: false, autoMerge: true, maxLoops: 1, timeoutMs: 10_000 });
    expect(r.ok).toBe(true);
    expect(r.merged).toBe(true);
    expect(r.detail).toContain('auto-merged');
    expect(mockAutoMerge).toHaveBeenCalledWith(repo, 'https://github.com/acme/repo/pull/1');
  });

  it('skips the gate for non-JS repos and merges', async () => {
    repo = gitRepo();
    stubPipeline();
    dropPackageJson(repo);
    mockRunCli({ exitCode: 0 });
    enqueueTask(repo, { id: 'TASK-reg3', title: 't', goal: 'g' });
    const r = await consumeOnce({ repoPath: repo, autoPr: false, autoMerge: true, maxLoops: 1, timeoutMs: 10_000 });
    expect(r.ok).toBe(true);
    expect(r.merged).toBe(true);
    // the suite never ran: no non-git runCli invocation
    const suiteCalls = mockRun.mock.calls.filter((c) => c[0] !== 'git');
    expect(suiteCalls).toEqual([]);
  });
});
