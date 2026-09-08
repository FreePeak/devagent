import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { chmodSync, existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { runInit, renderInitReport } from '../src/commands/init.js';

/**
 * FR-SIMPLE-01: `devagent init` guided setup. Hermetic: stub `git` and `omp`
 * CLIs on a stub PATH dir (same trick as sessions-cli.test.ts's stub herdr);
 * the omp stub answers the real preflight probe's success shape so no
 * provider/network is touched.
 */

// omp stub: answers the preflight probe's success shape; STUB_OMP_FAIL=1
// simulates a broken provider (exit 1, no output).
const OMP_STUB = `#!/usr/bin/env node
if (process.env.STUB_OMP_FAIL === '1') process.exit(1);
process.stdout.write(JSON.stringify({ type: 'result', text: 'OK' }));
`;
const GIT_STUB = `#!/usr/bin/env node
if (process.argv[2] === '--version') { process.stdout.write('git version 2.50.0'); process.exit(0); }
process.exit(1);
`;

/** Strip ANSI escapes so chip/box contiguity can be asserted on visible text. */
function plain(s: string): string {
  return s.replace(/\x1b\[[0-9;]*m/g, '');
}

let stubDir: string;
let repoPath: string;
let priorPath: string | undefined;
let priorLinear: string | undefined;
let priorGithub: string | undefined;
let logs: string[];

beforeAll(() => {
  stubDir = mkdtempSync(join(tmpdir(), 'devagent-init-stubs-'));
  for (const [name, body] of [
    ['omp', OMP_STUB],
    ['git', GIT_STUB],
  ] as const) {
    const p = join(stubDir, name);
    writeFileSync(p, body);
    chmodSync(p, 0o755);
  }
});

afterAll(() => {
  rmSync(stubDir, { recursive: true, force: true });
});

beforeEach(() => {
  repoPath = mkdtempSync(join(tmpdir(), 'devagent-init-repo-'));
  priorPath = process.env.PATH;
  priorLinear = process.env.LINEAR_API_KEY;
  priorGithub = process.env.GITHUB_TOKEN;
  // Stub dir first: `which omp`/`which git` resolve to the stubs deterministically.
  process.env.PATH = `${stubDir}:${priorPath ?? ''}`;
  delete process.env.LINEAR_API_KEY;
  delete process.env.GITHUB_TOKEN;
  logs = [];
  vi.spyOn(console, 'log').mockImplementation((...a: unknown[]) => {
    logs.push(a.map(String).join(' '));
  });
});

afterEach(() => {
  vi.restoreAllMocks();
  if (priorPath === undefined) delete process.env.PATH;
  else process.env.PATH = priorPath;
  if (priorLinear === undefined) delete process.env.LINEAR_API_KEY;
  else process.env.LINEAR_API_KEY = priorLinear;
  if (priorGithub === undefined) delete process.env.GITHUB_TOKEN;
  else process.env.GITHUB_TOKEN = priorGithub;
  rmSync(repoPath, { recursive: true, force: true });
});

describe('runInit (FR-SIMPLE-01 guided setup)', () => {
  it('writes devagent.json with sane defaults on a clean repo and passes required checks', async () => {
    const r = await runInit({ repoPath });
    expect(r.ok).toBe(true);
    expect(r.created).toBe(true);
    const cfg = JSON.parse(readFileSync(r.configPath, 'utf8')) as Record<string, unknown>;
    expect(cfg).toMatchObject({ worker: 'omp', maxLoops: 3, timeoutMinutes: 30, githubBaseBranch: 'main' });
    // The written config must load (valid shape). Advisory rows: 'workers'
    // chips (FR-HAND-04), 'docker' (#144 R2), 'herdr' (FR-HAND-05) — present
    // on every run, never gating ok. Orca appears only when the binary does.
    const names = r.checks.map((c) => c.name);
    expect(names.slice(0, 3)).toEqual(['git', 'worker', 'workers']);
    expect(names).toContain('provider');
    expect(names).toContain('herdr');
    expect(names).toContain('docker');
    expect(r.checks.find((c) => c.name === 'provider')?.ok).toBe(true);
  });

  it('treats a missing worker CLI as a required failure with advice, still writing config', async () => {
    const emptyDir = mkdtempSync(join(tmpdir(), 'devagent-init-nopath-'));
    process.env.PATH = emptyDir; // no git, no omp
    try {
      const r = await runInit({ repoPath });
      expect(r.ok).toBe(false);
      expect(r.checks.find((c) => c.name === 'git')?.ok).toBe(false);
      expect(r.checks.find((c) => c.name === 'worker')?.ok).toBe(false);
      // Config is still written: setup advises, it does not gate.
      expect(existsSync(r.configPath)).toBe(true);
      logs = [];
      renderInitReport(r);
      const out = logs.join('\n');
      expect(out).toContain('required check');
      expect(out).toContain('install the worker CLI');
    } finally {
      rmSync(emptyDir, { recursive: true, force: true });
    }
  });

  it('is idempotent: an existing devagent.json is merged, choices win, never clobbered', async () => {
    writeFileSync(
      join(repoPath, 'devagent.json'),
      JSON.stringify({ worker: 'omp', model: 'omniroute/dev', autoMerge: true }, null, 2),
    );
    const r = await runInit({ repoPath });
    expect(r.created).toBe(false);
    expect(r.ok).toBe(true);
    const cfg = JSON.parse(readFileSync(r.configPath, 'utf8')) as Record<string, unknown>;
    expect(cfg).toMatchObject({ worker: 'omp', model: 'omniroute/dev', autoMerge: true, maxLoops: 3, timeoutMinutes: 30 });
  });

  it('reports a failed provider probe as advisory: ok stays true, plain-language detail', async () => {
    process.env.STUB_OMP_FAIL = '1';
    try {
      const r = await runInit({ repoPath });
      expect(r.ok).toBe(true); // required checks (git, worker) still pass
      const provider = r.checks.find((c) => c.name === 'provider');
      expect(provider?.ok).toBe(false);
      logs = [];
      renderInitReport(r);
      const out = logs.join('\n');
      expect(out).toContain('provider did not answer');
      expect(out).toContain('Next: state your goal in one sentence');
    } finally {
      delete process.env.STUB_OMP_FAIL;
    }
  });

  it('scopes the provider probe to omp: no provider check for claude-code', async () => {
    const r = await runInit({ repoPath, worker: 'claude-code' });
    expect(r.checks.find((c) => c.name === 'provider')).toBeUndefined();
    expect(r.checks.find((c) => c.name === 'worker')?.detail).toContain('claude');
    // Config records the requested worker.
    const cfg = JSON.parse(readFileSync(r.configPath, 'utf8')) as Record<string, unknown>;
    expect(cfg.worker).toBe('claude-code');
  });

  it('reports credential presence without ever printing values', async () => {
    process.env.LINEAR_API_KEY = 'lin_api_super_secret_value';
    const r = await runInit({ repoPath });
    const linear = r.checks.find((c) => c.name === 'LINEAR_API_KEY');
    expect(linear?.ok).toBe(true);
    expect(linear?.detail).toBe('LINEAR_API_KEY set');
    expect(linear?.unlocks).toContain('tracker tickets');
    logs = [];
    renderInitReport(r);
    expect(logs.join('\n')).not.toContain('lin_api_super_secret_value');
  });

  it('renders the plain-language checklist with §20.8 chips and a goal next action', async () => {
    const r = await runInit({ repoPath });
    renderInitReport(r);
    const out = plain(logs.join('\n'));
    expect(out).toContain('devagent tui');
    expect(out).toContain('press n, type the goal, Enter');
    // 1+1 bar: no third mandatory command is named as required.
    expect(out).not.toContain('orchestrate --goal');
    // Success never dumps raw logs: no probe stdout/stderr echo.
    expect(out).not.toContain('result');
  });

  it('FR-HAND-04: lists every worker with found/missing chips and install hints', async () => {
    // Restrict PATH to the stub dir: the machine may have real claude/
    // opencode/pi on PATH — the test must stay deterministic.
    process.env.PATH = stubDir;
    const r = await runInit({ repoPath });
    const chips = r.checks.find((c) => c.name === 'workers');
    expect(chips?.ok).toBe(true);
    expect(chips?.detail).toContain('omp: selected');
    expect(chips?.detail).toContain('claude-code: missing — install with npm i -g @anthropic-ai/claude-code');
    expect(chips?.detail).toContain('opencode: missing');
    expect(chips?.detail).toContain('pi: missing');
  });

  it('FR-HAND-04: defaults to the first detected worker when omp is absent and config is clean', async () => {
    // Only the pi stub is on the worker path: worker must resolve to pi.
    const piDir = mkdtempSync(join(tmpdir(), 'devagent-init-pi-'));
    writeFileSync(join(piDir, 'git'), GIT_STUB);
    chmodSync(join(piDir, 'git'), 0o755);
    writeFileSync(join(piDir, 'pi'), '#!/usr/bin/env node\nprocess.exit(0);\n');
    chmodSync(join(piDir, 'pi'), 0o755);
    process.env.PATH = piDir;
    try {
      const r = await runInit({ repoPath });
      const cfg = JSON.parse(readFileSync(r.configPath, 'utf8')) as Record<string, unknown>;
      expect(cfg.worker).toBe('pi');
      expect(r.checks.find((c) => c.name === 'worker')?.ok).toBe(true);
    } finally {
      rmSync(piDir, { recursive: true, force: true });
    }
  });

  it('FR-HAND-04: --worker wins over detection', async () => {
    // Deterministic: the machine may not have a real claude binary — a stub
    // on the front of PATH makes the "selected" chip reachable everywhere
    // (CI runners ship no claude-code).
    const claudeDir = mkdtempSync(join(tmpdir(), 'devagent-init-claude-'));
    writeFileSync(join(claudeDir, 'claude'), '#!/usr/bin/env node\nprocess.exit(0);\n');
    chmodSync(join(claudeDir, 'claude'), 0o755);
    process.env.PATH = `${claudeDir}:${process.env.PATH ?? ''}`;
    try {
      const r = await runInit({ repoPath, worker: 'claude-code' });
      expect(r.checks.find((c) => c.name === 'workers')?.detail).toContain('claude-code: selected');
      // The flag wins over detection/config for the resolved worker.
      const cfg = JSON.parse(readFileSync(r.configPath, 'utf8')) as Record<string, unknown>;
      expect(cfg.worker).toBe('claude-code');
    } finally {
      rmSync(claudeDir, { recursive: true, force: true });
    }
  });

  it('FR-HAND-05: herdr stub present flips herdr.enabled on when unset; absent stays unset', async () => {
    const stub = mkdtempSync(join(tmpdir(), 'devagent-init-herdr-'));
    writeFileSync(join(stub, 'herdr'), '#!/bin/sh\nexit 0;\n');
    chmodSync(join(stub, 'herdr'), 0o755);
    // stubDir first (git/omp stubs), the herdr stub appended; node itself
    // resolves from the real PATH.
    process.env.PATH = `${stubDir}:${stub}:${priorPath ?? ''}`;
    try {
      const r = await runInit({ repoPath });
      const cfg = JSON.parse(readFileSync(r.configPath, 'utf8')) as Record<string, unknown>;
      expect((cfg.herdr as Record<string, unknown>).enabled).toBe(true);
      expect(r.checks.find((c) => c.name === 'herdr')?.ok).toBe(true);
    } finally {
      rmSync(stub, { recursive: true, force: true });
    }
    // Absent binary: no flip, loud advisory row, ok untouched (fresh repo:
    // the previous run's config already recorded the flip). PATH restricted
    // to the stub dir + the running node's dir so the machine's real herdr
    // (this repo's own tooling) can never leak into the scan.
    const nodeDir = dirname(process.execPath);
    process.env.PATH = `${stubDir}:${nodeDir}`;
    const repo2 = mkdtempSync(join(tmpdir(), 'devagent-init-repo2-'));
    try {
      const r2 = await runInit({ repoPath: repo2 });
      const cfg2 = JSON.parse(readFileSync(join(repo2, 'devagent.json'), 'utf8')) as Record<string, unknown>;
      expect(cfg2.herdr).toBeUndefined();
      expect(r2.checks.find((c) => c.name === 'herdr')?.ok).toBe(false);
      expect(r2.ok).toBe(true);
    } finally {
      rmSync(repo2, { recursive: true, force: true });
    }
  });

  it('FR-HAND-06: orca on PATH registers the repo (no worktrees) and missing orca is skipped', async () => {
    const ensured: string[] = [];
    const stub = mkdtempSync(join(tmpdir(), 'devagent-init-orca-'));
    writeFileSync(join(stub, 'orca'), '#!/bin/sh\nexit 0;\n');
    chmodSync(join(stub, 'orca'), 0o755);
    process.env.PATH = `${stubDir}:${stub}`;
    try {
      const r = await runInit({ repoPath, ensureOrca: async (p) => { ensured.push(p); return true; } });
      expect(ensured).toEqual([repoPath]);
      expect(r.checks.find((c) => c.name === 'orca')?.ok).toBe(true);
    } finally {
      rmSync(stub, { recursive: true, force: true });
    }
    // No orca binary: no row at all.
    const r2 = await runInit({ repoPath, ensureOrca: async () => true });
    expect(r2.checks.find((c) => c.name === 'orca')).toBeUndefined();
  });
});
