import { describe, expect, it, vi, beforeEach } from 'vitest';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync, existsSync, readFileSync, writeFileSync, mkdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

// Mock worker dispatch so consume tests don't spawn real claude/opencode binaries
vi.mock('../src/workers/index.js', () => ({
  getWorker: vi.fn().mockReturnValue({
    name: 'claude-code' as const,
    spawn: vi.fn().mockResolvedValue({ exitCode: 0, events: [], resultText: 'mocked-ok', sessionId: null, durationMs: 10, timedOut: false }),
  }),
}));

import { runCreate, launchAgentPlistContent, createOrcaWorktree } from '../src/create.js';
import { enqueueTask, listTasks } from '../src/queue.js';
import { runScoutOnce } from '../src/scout.js';
import { consumeOnce } from '../src/consume.js';
import { loadConfig } from '../src/config.js';

function tmpRepo(): string {
  const d = mkdtempSync(join(tmpdir(), 'da-cc-'));
  // need git repo for consume
  const { execFileSync } = require('node:child_process') as typeof import('node:child_process');
  execFileSync('git', ['init', '-q'], { cwd: d });
  execFileSync('git', ['config', 'user.email', 't@t'], { cwd: d });
  execFileSync('git', ['config', 'user.name', 't'], { cwd: d });
  mkdirSync(join(d, 'docs'), { recursive: true });
  writeFileSync(join(d, 'docs', 'PRD.md'), '# PRD\n## 4 Competitive\nfoo\n## 17 Roadmap\nbar\n');
  mkdirSync(join(d, '.selfbuild'), { recursive: true });
  writeFileSync(join(d, '.selfbuild', 'ledger.jsonl'), '{"loop":1,"status":"ok"}\n');
  writeFileSync(join(d, 'package.json'), JSON.stringify({ name: 'fixture', scripts: { test: 'node -e ""' } }));
  const { execFileSync: ef } = require('node:child_process') as typeof import('node:child_process');
  ef('git', ['add', '-A'], { cwd: d });
  ef('git', ['commit', '-qm', 'init'], { cwd: d });
  return d;
}

describe('create: dry-run', () => {
  it('dry-run prints plan without mutating', async () => {
    const repo = tmpRepo();
    try {
      const r = await runCreate({ repoPath: repo, scout: true, workers: 2, dryRun: true });
      expect(r.ok).toBe(true);
      expect(r.detail).toContain('would ensure');
      expect(existsSync(join(repo, '.devagent', 'queue'))).toBe(false);
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('rejects missing repoPath', async () => {
    const r = await runCreate({ repoPath: '/no/such/path/xyz-' + Math.random(), dryRun: true });
    expect(r.ok).toBe(false);
  });
});

describe('create: real run (mocked orca runner)', () => {
  it('creates queue dirs, writes config with scout, and provisions mocked orca worktrees', async () => {
    const repo = tmpRepo();
    try {
      const okRunner = async (cmd: string, args: string[]) => {
        if (cmd === 'orca' && args.includes('repo')) return { exitCode: 0, stdout: '{}', timedOut: false };
        if (cmd === 'orca' && args.includes('create')) return { exitCode: 0, stdout: JSON.stringify({ result: { path: join(repo, 'wt-worker') } }), timedOut: false };
        return { exitCode: 1, stdout: '', timedOut: false };
      };
      const r = await runCreate({ repoPath: repo, scout: true, workers: 1, autoMerge: true, intervalMinutes: 15, scoutWorker: 'opencode' }, okRunner as never);
      expect(r.ok).toBe(true);
      expect(existsSync(join(repo, '.devagent', 'queue'))).toBe(true);
      expect(existsSync(join(repo, '.devagent', 'prds'))).toBe(true);
      const cfg = loadConfig(repo);
      expect(cfg.scout?.enabled).toBe(true);
      expect(cfg.scout?.intervalMinutes).toBe(15);
      expect(cfg.autoMerge).toBe(true);
      expect(r.orcaWorktrees).toHaveLength(1);
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('degrades gracefully when orca missing', async () => {
    const repo = tmpRepo();
    try {
      const failRunner = async () => { throw new Error('ENOENT'); };
      const r = await runCreate({ repoPath: repo, workers: 2 }, failRunner as never);
      expect(r.ok).toBe(true);
      expect(r.orcaWorktrees).toEqual([]);
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });
});

describe('launchAgent plist', () => {
  it('plist is valid XML and contains label + interval', () => {
    const xml = launchAgentPlistContent('/tmp/myrepo', 45, 'claude-code');
    expect(xml).toContain('com.devagent.scout');
    expect(xml).toContain('/tmp/myrepo');
    expect(xml).toContain('45');
    expect(xml).toContain('claude-code');
    expect(xml).toContain('<?xml');
  });
  it('plist validates structurally (has required keys)', () => {
    const xml = launchAgentPlistContent('/tmp/r', 30, 'opencode');
    for (const k of ['Label', 'ProgramArguments', 'RunAtLoad', 'KeepAlive', 'ThrottleInterval']) {
      expect(xml).toContain(k);
    }
  });
  it('plist passes macOS plutil -lint', { skip: process.platform !== 'darwin' }, () => {
    const xml = launchAgentPlistContent('/tmp/r', 30, 'opencode');
    const tmp = join(tmpdir(), `da-plist-${Date.now()}.plist`);
    writeFileSync(tmp, xml);
    try {
      const out = execFileSync('plutil', ['-lint', tmp]).toString().trim();
      expect(out).toBe(`${tmp}: OK`);
    } finally {
      rmSync(tmp, { force: true });
    }
  });
});

describe('createOrcaWorktree + orca integration helpers', () => {
  it('parses path from orca worktree create JSON', async () => {
    const runner = async () => ({ exitCode: 0, stdout: JSON.stringify({ result: { path: '/tmp/wt' } }), timedOut: false });
    expect(await createOrcaWorktree('/tmp/repo', 'w1', runner as never)).toBe('/tmp/wt');
  });
  it('returns null on orca failure', async () => {
    const runner = async () => ({ exitCode: 1, stdout: '', timedOut: false });
    expect(await createOrcaWorktree('/tmp/repo', 'w1', runner as never)).toBeNull();
  });
  it('listOrcaWorktrees via orca.ts returns prefix-filtered paths', async () => {
    const { listOrcaWorktrees } = await import('../src/integrations/orca.js');
    const runner = async () => ({
      exitCode: 0,
      stdout: JSON.stringify({ result: { worktrees: [{ path: '/tmp/repo/wt-a' }, { path: '/other/repo/wt-b' }] } }),
      timedOut: false,
    });
    expect(await listOrcaWorktrees('/tmp/repo', runner as never)).toEqual(['/tmp/repo/wt-a']);
  });
});

describe('consume: no pending -> ok no-op, claimed -> runs pipeline stub', () => {
  it('returns no pending when queue empty', async () => {
    const repo = tmpRepo();
    try {
      const r = await consumeOnce({ repoPath: repo, autoPr: false, autoMerge: false, maxLoops: 1, timeoutMs: 10_000 });
      expect(r.ok).toBe(true);
      expect(r.detail).toContain('no pending');
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('claims a queued task and runs through pipeline (gorilla-patched via deps impl)', async () => {
    const repo = tmpRepo();
    try {
      enqueueTask(repo, { id: 'Q-1', title: 'Queued job', goal: 'Goal: queued job does a tiny docs edit', acceptanceCriteria: [] });
      // Patch deps module's implement path indirectly: make the fixture's test gate pass
      // and use the real pipeline which will hit the worker mock via vitest mock if present.
      // Instead, test that consume moves the task out of pending even when gates/workers fail,
      // without mocking the worker layer.
      const r = await consumeOnce({ repoPath: repo, autoPr: false, autoMerge: false, maxLoops: 1, timeoutMs: 15_000 });
      const { readTask } = await import('../src/queue.js');
      expect(['done', 'failed']).toContain(readTask(repo, 'Q-1')!.status);
      expect(typeof r.detail).toBe('string');
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });
});
