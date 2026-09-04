import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { chmodSync, existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
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
    // The written config must load (valid shape).
    const names = r.checks.map((c) => c.name);
    expect(names).toEqual(['git', 'worker', 'provider', 'LINEAR_API_KEY', 'GITHUB_TOKEN']);
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
    expect(out).toContain('DevAgent setup —');
    expect(out).toContain('● git'); // chip: required check
    expect(out).toContain('● LINEAR_API_KEY'); // optional credential chip
    expect(out).toContain('unlocks: pushing branches and opening PRs');
    expect(out).toContain('devagent orchestrate --goal');
    // Success never dumps raw logs: no probe stdout/stderr echo.
    expect(out).not.toContain('result');
  });
});
