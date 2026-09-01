import { describe, expect, it, beforeEach, afterEach } from 'vitest';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync, readFileSync, writeFileSync, existsSync, mkdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { transientErrorClass, isTransientProviderError } from '../src/resilience/classify.js';
import {
  proxyStatePath,
  readProxyState,
  recordProxyProbe,
  recordTransientClass,
} from '../src/resilience/proxy-state.js';

describe('transientErrorClass (coarse class, exhaustive over isTransientProviderError)', () => {
  it('labels unrecognized-model as unrecognized-model', () => {
    expect(transientErrorClass('[claude-code:unrecognized_model] {"model":"cmd/minimax/minimax-m3-free"}')).toBe('unrecognized-model');
    expect(transientErrorClass('unrecognized_model error from omniroute proxy')).toBe('unrecognized-model');
  });

  it('labels empty stream/response as empty-stream', () => {
    expect(transientErrorClass('Empty stream at flush')).toBe('empty-stream');
    expect(transientErrorClass('Claude returned an empty response (no content block)')).toBe('empty-stream');
  });

  it('labels rate-limit variants as rate-limit', () => {
    expect(transientErrorClass('429 too many requests')).toBe('rate-limit');
    expect(transientErrorClass('rate limit exceeded, retry after 60s')).toBe('rate-limit');
    expect(transientErrorClass('too many requests on upstream')).toBe('rate-limit');
  });

  it('labels overloaded as overloaded', () => {
    expect(transientErrorClass('overloaded')).toBe('overloaded');
    expect(transientErrorClass('Service overloaded, try again')).toBe('overloaded');
  });

  it('labels ETIMEDOUT as timeout and upstream/gateway/network/unavailable appropriately', () => {
    expect(transientErrorClass('ETIMEDOUT')).toBe('timeout');
    expect(transientErrorClass('gateway timeout on proxy')).toBe('bad-gateway');
    expect(transientErrorClass('bad gateway 502')).toBe('bad-gateway');
    expect(transientErrorClass('connection lost before init')).toBe('network');
    expect(transientErrorClass('ECONNREFUSED 127.0.0.1:9000')).toBe('network');
    expect(transientErrorClass('fetch failed')).toBe('network');
    expect(transientErrorClass('socket hang up')).toBe('network');
    expect(transientErrorClass('endpoint is unavailable')).toBe('unavailable');
    expect(transientErrorClass('upstream request failed')).toBe('upstream');
    expect(transientErrorClass('error from provider (code 503)')).toBe('upstream');
    expect(transientErrorClass('service unavailable')).toBe('unavailable');
  });

  it('returns null when not transient', () => {
    expect(transientErrorClass('Result: I implemented the feature as requested')).toBeNull();
    expect(transientErrorClass('test passed: 1/1')).toBeNull();
    expect(transientErrorClass(null)).toBeNull();
    expect(transientErrorClass(undefined)).toBeNull();
    expect(transientErrorClass('')).toBeNull();
  });

  it('returns null when the text matches non-retryable (auth/billing) even if it contains a transient word', () => {
    // 'invalid api key' patterns in NON_RETRYABLE_PATTERNS gate the transient check
    expect(transientErrorClass('invalid api key — fetch failed')).toBeNull();
    expect(transientErrorClass('billing: credit balance is low — error from provider')).toBeNull();
  });

  it('is consistent with isTransientProviderError for the core probe vocabulary', () => {
    const probeSamples = [
      '429 too many requests',
      'overloaded',
      'Empty stream at flush',
      '[claude-code:unrecognized_model] {"model":"cmd/minimax/minimax-m3-free"}',
      'connect ECONNREFUSED 127.0.0.1:20128',
      'ETIMEDOUT',
      'fetch failed',
      'gateway timeout on proxy',
      'Result: I implemented the feature',
      null,
      undefined,
    ];
    for (const raw of probeSamples) {
      const t = raw as string | null | undefined;
      const expectedTransient = isTransientProviderError(t as string);
      const label = transientErrorClass(t);
      expect(label !== null).toBe(expectedTransient);
    }
  });
});

describe('proxy-state file (observable proxy-gate decision surface)', () => {
  let repoPath: string;

  beforeEach(() => {
    repoPath = mkdtempSync(join(tmpdir(), 'da-proxy-'));
  });

  afterEach(() => {
    rmSync(repoPath, { recursive: true, force: true });
  });

  it('readProxyState returns null for an absent file', () => {
    expect(readProxyState(repoPath)).toBeNull();
  });

  it('readProxyState returns null for a corrupt file', () => {
    mkdirSync(join(repoPath, '.devagent'), { recursive: true });
    writeFileSync(proxyStatePath(repoPath), 'not-json {', 'utf8');
    expect(readProxyState(repoPath)).toBeNull();
  });

  it('recordProxyProbe ok → circuit closed; fail → open; ok after open → half-open; ok after half-open → closed', () => {
    let state = recordProxyProbe(repoPath, { ok: true });
    expect(state.circuit).toBe('closed');
    expect(state.lastProbe?.ok).toBe(true);

    state = recordProxyProbe(repoPath, { ok: false });
    expect(state.circuit).toBe('open');
    expect(state.lastProbe?.ok).toBe(false);

    state = recordProxyProbe(repoPath, { ok: true });
    expect(state.circuit).toBe('half-open');

    state = recordProxyProbe(repoPath, { ok: true });
    expect(state.circuit).toBe('closed');

    const payload = JSON.parse(readFileSync(proxyStatePath(repoPath), 'utf8')) as Record<string, unknown>;
    expect(typeof payload.circuit).toBe('string');
    expect(typeof payload.updatedAt).toBe('string');
  });

  it('recordProxyProbe stores detail and circuitChangedAt', () => {
    recordProxyProbe(repoPath, { ok: true, detail: 'attempt 1/3' });
    const state = readProxyState(repoPath);
    expect(state?.lastProbe?.detail).toBe('attempt 1/3');
    expect(state?.circuitChangedAt).toBeDefined();
  });

  it('recordTransientClass writes class + bounded excerpt for transient text', () => {
    const r = recordTransientClass(repoPath, '429 too many requests because the proxy is rate limiting upstream');
    expect(r).not.toBeNull();
    expect(r!.class).toBe('rate-limit');
    const state = readProxyState(repoPath);
    expect(state?.lastTransient?.class).toBe('rate-limit');
    expect(state?.lastTransient?.excerpt).toContain('429');
    expect((state?.lastTransient?.excerpt ?? '').length).toBeLessThanOrEqual(200);
  });

  it('recordTransientClass returns null and leaves state unchanged for non-transient text', () => {
    mkdirSync(join(repoPath, '.devagent'), { recursive: true });
    writeFileSync(
      proxyStatePath(repoPath),
      JSON.stringify({ circuit: 'closed', circuitChangedAt: new Date().toISOString(), updatedAt: new Date().toISOString() }, null, 2),
      'utf8',
    );
    const before = readFileSync(proxyStatePath(repoPath), 'utf8');
    const r = recordTransientClass(repoPath, 'Result: I implemented the feature as requested');
    expect(r).toBeNull();
    expect(readFileSync(proxyStatePath(repoPath), 'utf8')).toBe(before);
  });

  it('recordTransientClass preserves an open circuit when recording', () => {
    recordProxyProbe(repoPath, { ok: false }); // open
    expect(readProxyState(repoPath)?.circuit).toBe('open');
    recordTransientClass(repoPath, '500 provider.*unavailable aut insurance rerov!');
    // Use an unavailable text that actually transients
    recordTransientClass(repoPath, 'service unavailable — upstream stalled');
    expect(readProxyState(repoPath)?.circuit).toBe('open');
    expect(readProxyState(repoPath)?.lastTransient?.class).toBe('unavailable');
  });

  it('decouples probe and transient attributes (each writer preserves the other)', () => {
    recordTransientClass(repoPath, 'Empty stream at flush');
    const afterTransient = readProxyState(repoPath);
    recordProxyProbe(repoPath, { ok: true, detail: 'attempt 1/3' });
    const afterProbe = readProxyState(repoPath);
    expect(afterProbe?.lastTransient?.class).toBe(afterTransient?.lastTransient?.class);
  });

  it('circuit: closed → open → half-open → closed round-trip with ok/failed sequence', () => {
    recordProxyProbe(repoPath, { ok: true }); // closed
    expect(readProxyState(repoPath)?.circuit).toBe('closed');
    recordProxyProbe(repoPath, { ok: false }); // open
    expect(readProxyState(repoPath)?.circuit).toBe('open');
    recordProxyProbe(repoPath, { ok: true }); // half-open
    expect(readProxyState(repoPath)?.circuit).toBe('half-open');
    recordProxyProbe(repoPath, { ok: true }); // closed
    expect(readProxyState(repoPath)?.circuit).toBe('closed');
    recordProxyProbe(repoPath, { ok: false }); // open again
    expect(readProxyState(repoPath)?.circuit).toBe('open');
  });
});

describe('devagent status --providers (report surface)', () => {
  function statusProviders(repoPath: string): string {
    return execFileSync(
      'npx',
      ['tsx', 'src/cli.ts', 'status', '--providers', '--repo', repoPath],
      {
        cwd: join(import.meta.dirname, '..'),
        stdio: 'pipe',
        env: {
          PATH: process.env.PATH,
          HOME: process.env.HOME,
          DEVAGENT_HOME: process.env.DEVAGENT_HOME ?? process.env.HOME ?? '.',
        },
        timeout: 30_000,
      },
    ).toString();
  }

  it('prints a no-state message when no proxy state has been recorded', () => {
    const empty = mkdtempSync(join(tmpdir(), 'da-status-empty-'));
    try {
      const out = statusProviders(empty);
      expect(out.toLowerCase()).toContain('no provider state');
    } finally {
      rmSync(empty, { recursive: true, force: true });
    }
  });

  it('reports probe, transient class, and circuit lanes when state is present', () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-status-filled-'));
    try {
      const now = new Date().toISOString();
      mkdirSync(join(repo, '.devagent'), { recursive: true });
      writeFileSync(
        proxyStatePath(repo),
        JSON.stringify(
          {
            circuit: 'open',
            circuitChangedAt: now,
            lastProbe: { ok: false, at: now, detail: 'all 3 probes failed' },
            lastTransient: { class: 'rate-limit', at: now, excerpt: '429 too many requests' },
            updatedAt: now,
          },
          null,
          2,
        ),
        'utf8',
      );
      const out = statusProviders(repo);
      expect(out).toContain('probe: fail');
      expect(out).toContain('transient: rate-limit');
      expect(out).toContain('circuit: open');
      expect(out).toContain('probe');
      // liveness check: each required lane must appear
      const labels = ['probe', 'transient', 'circuit'] as const;
      for (const lab of labels) expect(out.toLowerCase()).toContain(lab);
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });

  it('defaults --repo to process cwd (and is operator-friendly for transitive scripts)', () => {
    // Smoke test: when invoked from a temp dir as cwd, default reads that repo's state.
    // Use an absolute CLI path so cwd override doesn't break the relative src/cli.ts resolution
    // (mirrors the pattern: dry-run.test.ts runs npx tsx with cwd set to the repo root).
    const repo = mkdtempSync(join(tmpdir(), 'da-status-cwd-'));
    const cliPath = join(import.meta.dirname, '../src/cli.ts');
    try {
      const now = new Date().toISOString();
      mkdirSync(join(repo, '.devagent'), { recursive: true });
      writeFileSync(
        proxyStatePath(repo),
        JSON.stringify({ circuit: 'half-open', circuitChangedAt: now, updatedAt: now }, null, 2),
        'utf8',
      );
      const out = execFileSync('npx', ['tsx', cliPath, 'status', '--providers'], {
        cwd: repo,
        stdio: 'pipe',
        env: { PATH: process.env.PATH, HOME: process.env.HOME },
        timeout: 30_000,
      }).toString();
      expect(out).toContain('circuit: half-open');
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });
});
