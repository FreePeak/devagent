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
  it('refuses LaunchAgent install for ephemeral (tmp) repos, allows real paths', async () => {
    const { shouldInstallLaunchAgent } = await import('../src/create.js');
    const tmpRepoDir = mkdtempSync(join(tmpdir(), 'da-ephemeral-'));
    try {
      expect(shouldInstallLaunchAgent(tmpRepoDir)).toBe(false);
      expect(shouldInstallLaunchAgent('/tmp/da-cc-nonexistent')).toBe(false);
      expect(shouldInstallLaunchAgent(process.cwd())).toBe(true);
    } finally {
      rmSync(tmpRepoDir, { recursive: true, force: true });
    }
  });

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

describe('create: tracker + builder + orchestrator LaunchAgent plists', () => {
  it('rolePlistSpecs returns one spec per requested role with correct labels and intervals', async () => {
    const { rolePlistSpecs } = await import('../src/create.js');
    const repo = '/tmp/selfbuild-test';
    const specs = rolePlistSpecs({ repoPath: repo, scout: true, tracker: true, builder: true, intervalMinutes: 42, scoutWorker: 'claude-code', trackIntervalMinutes: 17 });
    expect(specs.map((s) => s.label)).toEqual(['com.devagent.scout', 'com.devagent.tracker', 'com.devagent.builder']);
    expect(specs[0]!.programArgs.join(' ')).toContain('--interval 42');
    expect(specs[0]!.programArgs.join(' ')).toContain('claude-code');
    expect(specs[1]!.programArgs.join(' ')).toContain('--interval 17');
    expect(specs[1]!.logName).toBe('devagent-tracker.log');
    expect(specs[2]!.programArgs).toContain('/bin/bash');
    expect(specs[2]!.logName).toBe('devagent-builder.log');
  });

  it('orchestrator spec points at orchestrate-loop.sh and embeds goal env', async () => {
    const { rolePlistSpecs } = await import('../src/create.js');
    const specs = rolePlistSpecs({ repoPath: '/tmp/r', orchestrator: true, orchestratorGoal: 'Build the widget DAG', intervalMinutes: 30, scoutWorker: 'opencode', trackIntervalMinutes: 15 });
    const o = specs.find((s) => s.label === 'com.devagent.orchestrator')!;
    expect(o).toBeDefined();
    expect(o.programArgs.join(' ')).toContain('orchestrate-loop.sh');
    expect(o.logName).toBe('devagent-orchestrator.log');
    expect(o.env?.ORCHESTRATOR_GOAL).toBe('Build the widget DAG');
    expect(o.env?.ORCHESTRATOR_REPO).toBe('/tmp/r');
  });

  it('omits ORCHESTRATOR_GOAL env when no goal given', async () => {
    const { rolePlistSpecs } = await import('../src/create.js');
    const specs = rolePlistSpecs({ repoPath: '/tmp/r', orchestrator: true, intervalMinutes: 30, scoutWorker: 'opencode', trackIntervalMinutes: 15 });
    const o = specs.find((s) => s.label === 'com.devagent.orchestrator')!;
    expect(o.env?.ORCHESTRATOR_GOAL).toBeUndefined();
  });

  it('orchestrator plist passes macOS plutil -lint with goal env', { skip: process.platform !== 'darwin' }, async () => {
    const { rolePlistSpecs, buildLaunchAgentPlist } = await import('../src/create.js');
    const specs = rolePlistSpecs({ repoPath: '/tmp/r', scout: true, tracker: true, builder: true, orchestrator: true, orchestratorGoal: 'self-build devagent <now> & always', intervalMinutes: 30, scoutWorker: 'opencode', trackIntervalMinutes: 15 });
    expect(specs.map((s) => s.label)).toEqual(['com.devagent.scout', 'com.devagent.tracker', 'com.devagent.builder', 'com.devagent.orchestrator']);
    const xml = buildLaunchAgentPlist(specs[3]!);
    const tmp = join(tmpdir(), `da-orch-plist-${Date.now()}.plist`);
    writeFileSync(tmp, xml);
    try {
      const out = execFileSync('plutil', ['-lint', tmp]).toString().trim();
      expect(out).toBe(`${tmp}: OK`);
      expect(xml).toContain('ORCHESTRATOR_GOAL');
      expect(xml).toContain('&lt;now&gt;'); // xml-escaped
    } finally {
      rmSync(tmp, { force: true });
    }
  });

  it('tracker plist args contain devagent track and interval', async () => {
    const { rolePlistSpecs } = await import('../src/create.js');
    const specs = rolePlistSpecs({ repoPath: '/tmp/r', tracker: true, intervalMinutes: 30, scoutWorker: 'opencode', trackIntervalMinutes: 9 });
    const t = specs.find((s) => s.label === 'com.devagent.tracker')!;
    expect(t.programArgs).toContain('track');
    expect(t.programArgs).toContain('9');
  });

  it('builder plist points at scripts/build-loop.sh', async () => {
    const { rolePlistSpecs } = await import('../src/create.js');
    const specs = rolePlistSpecs({ repoPath: '/tmp/r', builder: true, intervalMinutes: 30, scoutWorker: 'opencode', trackIntervalMinutes: 15 });
    const b = specs.find((s) => s.label === 'com.devagent.builder')!;
    expect(b.programArgs.join(' ')).toContain('build-loop.sh');
  });
});
