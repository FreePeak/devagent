import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { execFileSync } from 'node:child_process';

import {
  PREFLIGHT_LEDGER_TASK_ID,
  PREFLIGHT_PROBE_ATTEMPTS,
  PREFLIGHT_ROLES,
  isPreflightRole,
  runPreflightGate,
} from '../src/resilience/preflight.js';
import { readProxyState } from '../src/resilience/proxy-state.js';
import { LEDGER_DIR } from '../src/orchestrator/ledger.js';

const noDelay = () => Promise.resolve();

describe('preflight roles (operator preflight, Q40)', () => {
  it('declares exactly the five operator roles', () => {
    expect([...PREFLIGHT_ROLES]).toEqual(['prd-curator', 'po', 'selfbuild', 'warroom', 'reviewer']);
  });

  it('accepts declared roles and rejects others', () => {
    expect(isPreflightRole('prd-curator')).toBe(true);
    expect(isPreflightRole('reviewer')).toBe(true);
    expect(isPreflightRole('orchestrator')).toBe(false);
    expect(isPreflightRole('')).toBe(false);
  });
});

describe('runPreflightGate (decision function)', () => {
  const dirs: string[] = [];
  const tempRepo = () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-preflight-'));
    dirs.push(dir);
    return dir;
  };
  afterEach(() => {
    while (dirs.length) rmSync(dirs.pop()!, { recursive: true, force: true });
  });

  it('proceeds on a first-attempt pass without writing ledger or circuit state', async () => {
    const repo = tempRepo();
    const probes: string[][] = [];
    const decision = await runPreflightGate({
      repoPath: repo,
      role: 'selfbuild',
      worker: 'omp',
      model: 'omniroute/dev',
      argv: ['omp', '-p', '--mode', 'json'],
      probe: async (_cmd, a) => {
        probes.push(a);
        return { ok: true };
      },
      delayMs: noDelay,
    });
    expect(decision).toEqual({ ok: true, role: 'selfbuild', worker: 'omp', model: 'omniroute/dev', attempts: 1 });
    // Prompt sits immediately after -p, before caller flags (buildOmpArgs shape).
    expect(probes[0]).toEqual(['-p', 'OK', '--mode', 'json']);
    expect(existsSync(join(repo, '.devagent', 'proxy-state.json'))).toBe(false);
    expect(existsSync(join(repo, LEDGER_DIR, 'events.jsonl'))).toBe(false);
  });

  it('retries to PREFLIGHT_PROBE_ATTEMPTS, then degrades: circuit opens and one operator-degraded row lands', async () => {
    const repo = tempRepo();
    let calls = 0;
    const decision = await runPreflightGate({
      repoPath: repo,
      role: 'warroom',
      worker: 'omp',
      model: 'omniroute/dev',
      argv: ['omp', '-p'],
      probe: async () => {
        calls += 1;
        return { ok: false, detail: 'unrecognized_model: probe 403' };
      },
      delayMs: noDelay,
    });
    expect(calls).toBe(PREFLIGHT_PROBE_ATTEMPTS);
    expect(decision.ok).toBe(false);
    expect(decision.role).toBe('warroom');
    expect(decision.attempts).toBe(PREFLIGHT_PROBE_ATTEMPTS);
    expect(decision.detail).toBe('unrecognized_model: probe 403');

    // Circuit state: probe failure opens the breaker for status --providers.
    const state = readProxyState(repo);
    expect(state?.circuit).toBe('open');
    expect(state?.lastProbe?.ok).toBe(false);
    expect(state?.lastProbe?.detail).toContain('preflight[warroom]');

    // Exactly one structured ledger row (skip evidence, not per-attempt noise).
    const rows = readFileSync(join(repo, LEDGER_DIR, 'events.jsonl'), 'utf8')
      .split('\n')
      .filter((l) => l.trim())
      .map((l) => JSON.parse(l) as Record<string, unknown>);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({
      kind: 'event',
      event: 'operator-degraded',
      taskId: PREFLIGHT_LEDGER_TASK_ID,
      role: 'warroom',
      worker: 'omp',
      model: 'omniroute/dev',
      ok: false,
      attempts: PREFLIGHT_PROBE_ATTEMPTS,
      detail: 'unrecognized_model: probe 403',
    });
    expect(typeof rows[0].ts).toBe('string');
  });

  it('recovers on a later attempt and reports ok without degrading anything', async () => {
    const repo = tempRepo();
    let calls = 0;
    const decision = await runPreflightGate({
      repoPath: repo,
      role: 'po',
      argv: ['omp', '-p'],
      probe: async () => {
        calls += 1;
        return calls >= 2 ? { ok: true } : { ok: false, detail: 'stream empty' };
      },
      delayMs: noDelay,
    });
    expect(decision.ok).toBe(true);
    expect(decision.attempts).toBe(2);
    expect(existsSync(join(repo, LEDGER_DIR, 'events.jsonl'))).toBe(false);
    expect(readProxyState(repo)).toBeNull();
  });

  it('treats a thrown probe as a failure and still writes the degraded row', async () => {
    const repo = tempRepo();
    const decision = await runPreflightGate({
      repoPath: repo,
      role: 'reviewer',
      argv: ['omp', '-p'],
      probe: async () => {
        throw new Error('spawn omp ENOENT');
      },
      delayMs: noDelay,
    });
    expect(decision.ok).toBe(false);
    expect(decision.detail).toBe('spawn omp ENOENT');
    const rows = readFileSync(join(repo, LEDGER_DIR, 'events.jsonl'), 'utf8')
      .split('\n')
      .filter((l) => l.trim());
    expect(rows).toHaveLength(1);
    expect(JSON.parse(rows[0]).event).toBe('operator-degraded');
  });

  it('rejects argv that does not start with the prompt flag', async () => {
    const repo = tempRepo();
    await expect(
      runPreflightGate({ repoPath: repo, role: 'po', argv: ['omp', '--mode', 'json'], probe: async () => ({ ok: true }), delayMs: noDelay }),
    ).rejects.toThrow(/prompt flag/);
  });
});

describe('devagent preflight CLI (skip semantics end to end)', () => {
  const dirs: string[] = [];
  let binDir: string;
  let repo: string;
  const cli = join(process.cwd(), 'src', 'cli.ts');

  beforeEach(() => {
    binDir = mkdtempSync(join(tmpdir(), 'da-pf-bin-'));
    repo = mkdtempSync(join(tmpdir(), 'da-pf-repo-'));
    dirs.push(binDir, repo);
    mkdirSync(join(repo, '.devagent'), { recursive: true });
    writeFileSync(join(repo, 'devagent.json'), JSON.stringify({ worker: 'omp', model: 'omniroute/dev' }));
  });
  afterEach(() => {
    while (dirs.length) rmSync(dirs.pop()!, { recursive: true, force: true });
  });

  const runCli = () => {
    try {
      const out = execFileSync(
        'npx',
        ['tsx', cli, 'preflight', '--role', 'selfbuild', '--repo', repo],
        { env: { ...process.env, PATH: `${binDir}:${process.env.PATH}` }, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] },
      );
      return { code: 0, out };
    } catch (err) {
      const e = err as { status?: number; stdout?: string; stderr?: string };
      return { code: e.status ?? 1, out: `${e.stdout ?? ''}${e.stderr ?? ''}` };
    }
  };

  it('healthy provider: exit 0, no ledger row', () => {
    writeFileSync(
      join(binDir, 'omp'),
      '#!/bin/sh\necho \'{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"OK"}]}}\'\n',
    );
    chmodSync(join(binDir, 'omp'), 0o755);
    const r = runCli();
    expect(r.code).toBe(0);
    expect(r.out).toContain('[preflight] ok role=selfbuild');
    expect(existsSync(join(repo, LEDGER_DIR, 'events.jsonl'))).toBe(false);
  });

  it('dead provider: exit 1 (skip signal) + operator-degraded row + open circuit', () => {
    writeFileSync(join(binDir, 'omp'), '#!/bin/sh\necho "unrecognized_model: no key" >&2\nexit 1\n');
    chmodSync(join(binDir, 'omp'), 0o755);
    const r = runCli();
    expect(r.code).toBe(1);
    expect(r.out).toContain('DEGRADED');
    expect(r.out).toContain('skip this cycle');
    expect(readProxyState(repo)?.circuit).toBe('open');
    const rows = readFileSync(join(repo, LEDGER_DIR, 'events.jsonl'), 'utf8')
      .split('\n')
      .filter((l) => l.trim())
      .map((l) => JSON.parse(l));
    expect(rows).toHaveLength(1);
    expect(rows[0].event).toBe('operator-degraded');
    expect(rows[0].role).toBe('selfbuild');
    expect(rows[0].ok).toBe(false);
  }, 240_000);
});
