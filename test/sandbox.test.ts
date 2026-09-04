import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// Mutable platform override so the seatbelt guard-rails are testable on any OS.
// Fallback is process.platform, NOT a node:os import (that would resolve to this
// very mock and recurse).
let platformOverride: string | null = null;
vi.mock('node:os', async (importOriginal) => {
  const actual = await importOriginal<typeof import('node:os')>();
  return {
    ...actual,
    platform: () => platformOverride ?? actual.platform(),
  };
});

// Capture what spawnCli ultimately hands to execFile.
type CapturedCall = { cmd: string; args: string[]; env?: Record<string, string> };
const captured: CapturedCall[] = [];
const execFileMock = vi.fn(
  (
    cmd: string,
    args: string[],
    opts: { env?: Record<string, string> },
    cb: (error: null, stdout: string, stderr: string) => void,
  ) => {
    captured.push({ cmd, args, env: opts?.env });
    setImmediate(() => cb(null, JSON.stringify({ result: 'ok', session_id: 's1' }), ''));
    return undefined as never;
  },
);
vi.mock('node:child_process', () => ({
  execFile: (...args: unknown[]) => (execFileMock as unknown)(...(args as [])),
}));

// See test/workers.test.ts: ambient DEVAGENT_NO_PROGRESS_TIMEOUT_MS from the
// operator's shell routes spawns through spawnCliStreaming -> spawn, which
// this execFile-only mock does not define. Pin the env for determinism.
beforeEach(() => {
  delete process.env.DEVAGENT_NO_PROGRESS_TIMEOUT_MS;
});

// Controllable DNS for allowlist-resolution cases.
const dnsLookupMock = vi.fn();
vi.mock('node:dns', async (importOriginal) => {
  const actual = await importOriginal<typeof import('node:dns')>();
  return {
    ...actual,
    promises: {
      ...actual.promises,
      lookup: (...args: unknown[]) =>
        (dnsLookupMock as unknown)(...(args as [])),
    },
  };
});

import { readFileSync } from 'node:fs';
import { ClaudeCodeAdapter } from '../src/workers/claude-code.js';
import { OpenCodeAdapter } from '../src/workers/opencode.js';
import {
  buildSeatbeltProfile,
  prepareWorkerSpawn,
  resolveNetworkAllowlist,
  sanitizeWorkerEnv,
} from '../src/workers/sandbox.js';
import type { SpawnCliOptions } from '../src/workers/spawn-utils.js';

const SPAWN_OPTS: SpawnCliOptions = { cwd: '/repo/worktree', timeoutMs: 5_000 };

describe('sanitizeWorkerEnv', () => {
  const baseEnv = {
    PATH: '/usr/bin',
    HOME: '/Users/t',
    ANTHROPIC_API_KEY: 'sk-ant-x',
    GITHUB_TOKEN: 'ghp-secret',
    NPM_TOKEN: 'npm-secret',
    AWS_SECRET_ACCESS_KEY: 'aws-secret',
    MY_SERVICE_PASSWORD: 'pw',
    SLACK_CLIENT_SECRET: 'slack',
    STRIPE_API_KEY: 'sk-live',
  };

  it('strips secret-shaped vars regardless of provider prefix', () => {
    const { env, stripped } = sanitizeWorkerEnv(baseEnv);
    // GITHUB_TOKEN is allowlisted (worker-side gh/curl api.github.com calls —
    // research crawls exhausted the anonymous 60 req/h IP budget, 2026-09-02).
    expect(stripped.sort()).toEqual([
      'AWS_SECRET_ACCESS_KEY',
      'MY_SERVICE_PASSWORD',
      'NPM_TOKEN',
      'SLACK_CLIENT_SECRET',
      'STRIPE_API_KEY',
    ]);
    expect(env.GITHUB_TOKEN).toBe('ghp-secret');
    expect(env.NPM_TOKEN).toBeUndefined();
    expect(env.AWS_SECRET_ACCESS_KEY).toBeUndefined();
  });

  it('keeps process basics and LLM auth vars the worker CLIs need', () => {
    const { env, stripped } = sanitizeWorkerEnv(baseEnv);
    expect(env.PATH).toBe('/usr/bin');
    expect(env.HOME).toBe('/Users/t');
    expect(env.ANTHROPIC_API_KEY).toBe('sk-ant-x');
    expect(stripped).not.toContain('ANTHROPIC_API_KEY');
  });

  it('does not mutate the caller environment', () => {
    sanitizeWorkerEnv(baseEnv);
    expect(baseEnv.GITHUB_TOKEN).toBe('ghp-secret');
  });

  describe('DEVAGENT_WORKER_ENV_ALLOWLIST override', () => {
    beforeEach(() => {
      vi.stubEnv('DEVAGENT_WORKER_ENV_ALLOWLIST', 'STRIPE_API_KEY,CUSTOM_TOKEN');
    });
    afterEach(() => {
      vi.unstubAllEnvs();
    });

    it('keeps explicitly allowlisted names even when secret-shaped', () => {
      const { env, stripped } = sanitizeWorkerEnv({
        ...baseEnv,
        CUSTOM_TOKEN: 'needed-by-worker',
      });
      expect(env.STRIPE_API_KEY).toBe('sk-live');
      expect(env.CUSTOM_TOKEN).toBe('needed-by-worker');
      expect(stripped).not.toContain('STRIPE_API_KEY');
      expect(stripped).not.toContain('CUSTOM_TOKEN');
      // other secrets still stripped (GITHUB_TOKEN is baseline-allowlisted)
      expect(env.NPM_TOKEN).toBeUndefined();
    });
  });
});

describe('buildSeatbeltProfile', () => {
  it('denies writes by default and allowlists cwd, temp dirs, agent config home', () => {
    const profile = buildSeatbeltProfile({ writablePaths: ['/repo/wt'], network: 'allow' });
    expect(profile).toContain('(deny file-write*)');
    expect(profile).toContain('(subpath "/repo/wt")');
    expect(profile).toContain('(subpath "/private/tmp")');
    expect(profile).toContain('(subpath "/private/var/folders")');
    expect(profile).toMatch(/\(subpath "[^"]*\/\.claude"\)/);
  });

  it('expresses network policy as an explicit clause', () => {
    const profile = buildSeatbeltProfile({ writablePaths: [], network: 'allow' });
    expect(profile).toContain('(allow network)');
  });

  it('deny policy emits (deny network*) after the default allow, last-match-wins', () => {
    const profile = buildSeatbeltProfile({ writablePaths: [], network: 'deny' });
    expect(profile).toContain('(deny network*)');
    expect(profile.indexOf('(allow default)')).toBeLessThan(profile.indexOf('(deny network*)'));
    expect(profile).not.toContain('(allow network)');
  });

  it('network deny composes with the write allowlist instead of replacing it', () => {
    const profile = buildSeatbeltProfile({ writablePaths: ['/repo/wt'], network: 'deny' });
    expect(profile).toContain('(deny file-write*)');
    expect(profile).toContain('(subpath "/repo/wt")');
    expect(profile).toContain('(deny network*)');
  });

  it('allowlist mode denies all sockets, then re-allows each endpoint after it', () => {
    const endpoints = ['140.82.121.3:443', '104.16.26.34:443'];
    const profile = buildSeatbeltProfile({
      writablePaths: [],
      network: 'allowlist',
      networkAllowlist: endpoints,
    });
    expect(profile).toContain('(deny network*)');
    // SBPL is last-match-wins: re-allow clauses must come after the blanket deny.
    const denyIdx = profile.indexOf('(deny network*)');
    const allowIdxs = endpoints.map((e) =>
      profile.indexOf(`(allow network-outbound (remote ip "${e}"))`),
    );
    expect(allowIdxs[0]).toBeGreaterThan(-1);
    expect(allowIdxs[1]).toBeGreaterThan(-1);
    expect(denyIdx).toBeLessThan(allowIdxs[0]!);
    expect(allowIdxs[0]!).toBeLessThan(allowIdxs[1]!);
    // one clause per endpoint, no bare network allow
    expect(profile.match(/allow network-outbound/g)).toHaveLength(endpoints.length);
    expect(profile).not.toContain('(allow network)');
  });
});

describe('resolveNetworkAllowlist', () => {
  afterEach(() => {
    dnsLookupMock.mockReset();
  });

  it('resolves hostnames via dns lookup(all) with default port 443', async () => {
    dnsLookupMock.mockResolvedValue([
      { address: '140.82.121.3', family: 4 },
      { address: '2606:50c0:8000::153', family: 6 },
    ]);
    await expect(resolveNetworkAllowlist('api.github.com')).resolves.toEqual([
      '140.82.121.3:443',
      '2606:50c0:8000::153:443',
    ]);
    expect(dnsLookupMock).toHaveBeenCalledWith('api.github.com', { all: true });
  });

  it('applies default port 443 when an entry omits the port', async () => {
    dnsLookupMock.mockResolvedValue([{ address: '108.128.1.5', family: 4 }]);
    await expect(resolveNetworkAllowlist('api.anthropic.com')).resolves.toEqual([
      '108.128.1.5:443',
    ]);
  });

  it('honors explicit ports on hostname entries', async () => {
    dnsLookupMock.mockResolvedValue([{ address: '192.168.1.10', family: 4 }]);
    await expect(resolveNetworkAllowlist('registry.npmjs.org:8443')).resolves.toEqual([
      '192.168.1.10:8443',
    ]);
  });

  it('passes literal IPv4 and IPv6 entries through without touching dns', async () => {
    await expect(resolveNetworkAllowlist('10.0.0.5:8443, fd00::7')).resolves.toEqual([
      '10.0.0.5:8443',
      'fd00::7:443',
    ]);
    expect(dnsLookupMock).not.toHaveBeenCalled();
  });

  it('throws loudly naming an unresolvable host', async () => {
    dnsLookupMock.mockRejectedValue(new Error('lookup failed: ENOTFOUND'));
    await expect(resolveNetworkAllowlist('nope.invalid')).rejects.toThrow(/nope\.invalid/);
    expect(dnsLookupMock).toHaveBeenCalledTimes(1);
  });

  it('throws loudly when every entry is blank', async () => {
    await expect(resolveNetworkAllowlist(' , ,')).rejects.toThrow(
      /DEVAGENT_SANDBOX_NETWORK_ALLOWLIST/,
    );
  });
});

describe('prepareWorkerSpawn', () => {
  beforeEach(() => {
    captured.length = 0;
    vi.unstubAllEnvs();
  });
  afterEach(() => {
    vi.unstubAllEnvs();
    platformOverride = null;
    dnsLookupMock.mockReset();
  });

  it('scrubs the worker env by default and flags replaceEnv', async () => {
    vi.stubEnv('NPM_TOKEN', 'x');
    const prepared = await prepareWorkerSpawn('claude', ['-p', 'hi'], SPAWN_OPTS);
    expect(prepared.cmd).toBe('claude');
    expect(prepared.opts.replaceEnv).toBe(true);
    expect(prepared.opts.env!.NPM_TOKEN).toBeUndefined();
    expect(prepared.strippedEnv).toContain('NPM_TOKEN');
  });

  it('lets caller-provided opts.env win over the scrubbed base', async () => {
    vi.stubEnv('OPENAI_API_KEY', 'parent-key');
    const prepared = await prepareWorkerSpawn('claude', ['-p'], {
      ...SPAWN_OPTS,
      env: { OPENAI_API_KEY: 'override-key' },
    });
    expect(prepared.opts.env!.OPENAI_API_KEY).toBe('override-key');
  });

  it('fails loudly when seatbelt is requested on non-darwin', async () => {
    platformOverride = 'linux';
    vi.stubEnv('DEVAGENT_SANDBOX', 'seatbelt');
    await expect(prepareWorkerSpawn('claude', ['-p'], SPAWN_OPTS)).rejects.toThrow(/darwin/);
  });

  it.runIf(process.platform === 'darwin')(
    'wraps the command with sandbox-exec and writes the profile when enabled',
    async () => {
      vi.stubEnv('DEVAGENT_SANDBOX', 'seatbelt');
      const prepared = await prepareWorkerSpawn('claude', ['-p', 'hi'], SPAWN_OPTS);
      expect(prepared.cmd).toBe('/usr/bin/sandbox-exec');
      expect(prepared.args[0]).toBe('-f');
      const profileText = readFileSync(prepared.args[1], 'utf8');
      expect(profileText).toContain('(deny file-write*)');
      expect(profileText).toContain('(subpath "/repo/worktree")');
      expect(prepared.args.slice(2)).toEqual(['claude', '-p', 'hi']);
      expect(prepared.opts.replaceEnv).toBe(true);
    },
  );

  it.runIf(process.platform === 'darwin')(
    'DEVAGENT_SANDBOX_NETWORK=deny lands in the generated profile; unset keeps allow',
    async () => {
      vi.stubEnv('DEVAGENT_SANDBOX', 'seatbelt');
      const allowPrepared = await prepareWorkerSpawn('claude', ['-p'], SPAWN_OPTS);
      expect(readFileSync(allowPrepared.args[1], 'utf8')).toContain('(allow network)');

      vi.stubEnv('DEVAGENT_SANDBOX_NETWORK', 'deny');
      const denyPrepared = await prepareWorkerSpawn('claude', ['-p'], SPAWN_OPTS);
      const denyProfile = readFileSync(denyPrepared.args[1], 'utf8');
      expect(denyProfile).toContain('(deny network*)');
      expect(denyProfile).not.toContain('(allow network)');
    },
  );

  it.runIf(process.platform === 'darwin')(
    'throws loudly when allowlist mode has an empty or missing allowlist var',
    async () => {
    platformOverride = 'darwin';
    vi.stubEnv('DEVAGENT_SANDBOX', 'seatbelt');
    vi.stubEnv('DEVAGENT_SANDBOX_NETWORK', 'allowlist');
    vi.stubEnv('DEVAGENT_SANDBOX_NETWORK_ALLOWLIST', '');
    await expect(prepareWorkerSpawn('claude', ['-p'], SPAWN_OPTS)).rejects.toThrow(
      /DEVAGENT_SANDBOX_NETWORK_ALLOWLIST/,
    );
    vi.stubEnv('DEVAGENT_SANDBOX_NETWORK_ALLOWLIST', '   ');
    await expect(prepareWorkerSpawn('claude', ['-p'], SPAWN_OPTS)).rejects.toThrow(
      /DEVAGENT_SANDBOX_NETWORK_ALLOWLIST/,
    );
    },
  );

  it.runIf(process.platform === 'darwin')(
    'allowlist mode writes a profile with deny-then-resolved-endpoint clauses',
    async () => {
      dnsLookupMock.mockImplementation(async (host: string) => {
        if (host === 'api.anthropic.com') {
          return [{ address: '108.128.1.5', family: 4 }];
        }
        return [{ address: '151.101.0.1', family: 4 }];
      });
      platformOverride = 'darwin';
      vi.stubEnv('DEVAGENT_SANDBOX', 'seatbelt');
      vi.stubEnv('DEVAGENT_SANDBOX_NETWORK', 'allowlist');
      vi.stubEnv(
        'DEVAGENT_SANDBOX_NETWORK_ALLOWLIST',
        'api.anthropic.com, registry.npmjs.org:443',
      );
      const prepared = await prepareWorkerSpawn('claude', ['-p'], SPAWN_OPTS);
      expect(prepared.cmd).toBe('/usr/bin/sandbox-exec');
      const profile = readFileSync(prepared.args[1], 'utf8');
      const denyIdx = profile.indexOf('(deny network*)');
      const anthropicIdx = profile.indexOf('(allow network-outbound (remote ip "108.128.1.5:443"))');
      const npmIdx = profile.indexOf('(allow network-outbound (remote ip "151.101.0.1:443"))');
      expect(denyIdx).toBeGreaterThan(-1);
      expect(anthropicIdx).toBeGreaterThan(denyIdx);
      expect(npmIdx).toBeGreaterThan(anthropicIdx);
    },
  );

  it.runIf(process.platform === 'darwin')(
    'allowlist mode surfaces unresolvable hosts with the host named',
    async () => {
      dnsLookupMock.mockRejectedValue(new Error('lookup failed: ENOTFOUND'));
      platformOverride = 'darwin';
      vi.stubEnv('DEVAGENT_SANDBOX', 'seatbelt');
      vi.stubEnv('DEVAGENT_SANDBOX_NETWORK', 'allowlist');
      vi.stubEnv('DEVAGENT_SANDBOX_NETWORK_ALLOWLIST', 'dead.internal:443');
      await expect(prepareWorkerSpawn('claude', ['-p'], SPAWN_OPTS)).rejects.toThrow(
        /dead\.internal/,
      );
    },
  );
});

describe('worker adapters route through the sandbox', () => {
  beforeEach(() => {
    captured.length = 0;
    vi.unstubAllEnvs();
    // Hermetic vs selfbuild-loop.sh's exported DEVAGENT_NO_PROGRESS_TIMEOUT_MS:
    // an armed default routes spawnCli through spawnCliStreaming (unmocked
    // spawn here). Watchdog arming is covered in watchdog-health.test.ts.
    delete process.env.DEVAGENT_NO_PROGRESS_TIMEOUT_MS;
  });
  afterEach(() => {
    vi.unstubAllEnvs();
  });

  it('claude-code spawn reaches execFile scrubbed', async () => {
    vi.stubEnv('NPM_TOKEN', 'x');
    const adapter = new ClaudeCodeAdapter();
    const result = await adapter.spawn({
      prompt: 'do thing',
      cwd: '/repo/wt',
      timeoutMs: 5_000,
    });
    expect(result.exitCode).toBe(0);
    expect(captured).toHaveLength(1);
    expect(captured[0].cmd).toBe('claude');
    expect(captured[0].env!.NPM_TOKEN).toBeUndefined();
    expect(captured[0].env!.PATH).toBeDefined();
  });

  it('opencode spawn reaches execFile scrubbed', async () => {
    vi.stubEnv('NPM_TOKEN', 'x');
    const adapter = new OpenCodeAdapter();
    const result = await adapter.spawn({
      prompt: 'do thing',
      cwd: '/repo/wt',
      timeoutMs: 5_000,
    });
    expect(result.exitCode).toBe(0);
    expect(captured).toHaveLength(1);
    expect(captured[0].cmd).toBe('opencode');
    expect(captured[0].args.slice(0, 3)).toEqual(['run', '--format', 'json']);
    expect(captured[0].env!.NPM_TOKEN).toBeUndefined();
  });
});
