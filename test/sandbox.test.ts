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

import { readFileSync } from 'node:fs';
import { ClaudeCodeAdapter } from '../src/workers/claude-code.js';
import { OpenCodeAdapter } from '../src/workers/opencode.js';
import {
  buildSeatbeltProfile,
  prepareWorkerSpawn,
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
    expect(stripped.sort()).toEqual([
      'AWS_SECRET_ACCESS_KEY',
      'GITHUB_TOKEN',
      'MY_SERVICE_PASSWORD',
      'NPM_TOKEN',
      'SLACK_CLIENT_SECRET',
      'STRIPE_API_KEY',
    ]);
    expect(env.GITHUB_TOKEN).toBeUndefined();
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
      // other secrets still stripped
      expect(env.GITHUB_TOKEN).toBeUndefined();
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
});

describe('prepareWorkerSpawn', () => {
  beforeEach(() => {
    captured.length = 0;
    vi.unstubAllEnvs();
  });
  afterEach(() => {
    vi.unstubAllEnvs();
    platformOverride = null;
  });

  it('scrubs the worker env by default and flags replaceEnv', async () => {
    vi.stubEnv('GITHUB_TOKEN', 'x');
    const prepared = await prepareWorkerSpawn('claude', ['-p', 'hi'], SPAWN_OPTS);
    expect(prepared.cmd).toBe('claude');
    expect(prepared.opts.replaceEnv).toBe(true);
    expect(prepared.opts.env!.GITHUB_TOKEN).toBeUndefined();
    expect(prepared.strippedEnv).toContain('GITHUB_TOKEN');
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
});

describe('worker adapters route through the sandbox', () => {
  beforeEach(() => {
    captured.length = 0;
    vi.unstubAllEnvs();
  });
  afterEach(() => {
    vi.unstubAllEnvs();
  });

  it('claude-code spawn reaches execFile scrubbed', async () => {
    vi.stubEnv('GITHUB_TOKEN', 'x');
    const adapter = new ClaudeCodeAdapter();
    const result = await adapter.spawn({
      prompt: 'do thing',
      cwd: '/repo/wt',
      timeoutMs: 5_000,
    });
    expect(result.exitCode).toBe(0);
    expect(captured).toHaveLength(1);
    expect(captured[0].cmd).toBe('claude');
    expect(captured[0].env!.GITHUB_TOKEN).toBeUndefined();
    expect(captured[0].env!.PATH).toBeDefined();
  });

  it('opencode spawn reaches execFile scrubbed', async () => {
    vi.stubEnv('GITHUB_TOKEN', 'x');
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
    expect(captured[0].env!.GITHUB_TOKEN).toBeUndefined();
  });
});
