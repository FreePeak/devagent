import { describe, expect, it, vi } from 'vitest';
import {
  buildSshArgs,
  parseRemoteTarget,
  runRemoteTask,
  shellQuote,
} from '../src/remote.js';
import { RunLogger } from '../src/logger.js';

describe('parseRemoteTarget', () => {
  it('parses user@host:path', () => {
    expect(parseRemoteTarget('deploy@build01:/srv/repos/app')).toEqual({
      user: 'deploy',
      host: 'build01',
      path: '/srv/repos/app',
    });
  });

  it('parses bare host:path', () => {
    expect(parseRemoteTarget('build01:/srv/repos/app')).toEqual({
      host: 'build01',
      path: '/srv/repos/app',
    });
  });

  it('parses ssh:// URLs with port', () => {
    expect(parseRemoteTarget('ssh://deploy@build01:2222/srv/repos/app')).toEqual({
      user: 'deploy',
      host: 'build01',
      path: '/srv/repos/app',
      port: 2222,
    });
  });

  it('rejects targets without a path', () => {
    expect(() => parseRemoteTarget('build01')).toThrow(/expected/);
    expect(() => parseRemoteTarget('ssh://build01')).toThrow(/path/);
  });

  it('rejects relative remote paths and empty input', () => {
    expect(() => parseRemoteTarget('build01:relative/path')).toThrow(/absolute/);
    expect(() => parseRemoteTarget('   ')).toThrow(/empty/);
  });
});

describe('shellQuote / buildSshArgs', () => {
  it('single-quote escapes embedded quotes', () => {
    expect(shellQuote("it's")).toBe(`'it'\\''s'`);
  });

  it('builds ssh argv with optional port and user', () => {
    const t = { user: 'u', host: 'h', path: '/p', port: 2200 };
    expect(buildSshArgs(t, 'echo hi')).toEqual(['ssh', '-o', 'BatchMode=yes', '-p', '2200', 'u@h', 'echo hi']);
    const bare = { host: 'h', path: '/p' };
    expect(buildSshArgs(bare, 'echo hi')).toEqual(['ssh', '-o', 'BatchMode=yes', 'h', 'echo hi']);
  });
});

describe('runRemoteTask', () => {
  const makeDeps = (exitCode: number, stdout = '') => ({
    run: vi.fn().mockResolvedValue({ exitCode, stdout }),
  });

  it('fails fast on bad target without touching ssh', async () => {
    const deps = makeDeps(0);
    const res = await runRemoteTask(
      { target: 'nope', prompt: 'x', timeoutMs: 1000, log: new RunLogger() },
      deps,
    );
    expect(res.ok).toBe(false);
    expect(deps.run).not.toHaveBeenCalled();
  });

  it('preflight failure short-circuits before dispatch', async () => {
    const deps = makeDeps(127);
    const res = await runRemoteTask(
      { target: 'host:/srv/app', prompt: 'add endpoint', timeoutMs: 60_000, log: new RunLogger() },
      deps,
    );
    expect(res.ok).toBe(false);
    expect(res.note).toMatch(/preflight failed on host/);
    expect(deps.run).toHaveBeenCalledTimes(1);
    // Preflight runs with a bounded timeout, never the full budget
    expect(deps.run.mock.calls[0]![1]).toBeLessThanOrEqual(15_000);
  });

  it('dispatches devagent task --auto-pr remotely and extracts the PR URL', async () => {
    const prUrl = 'https://github.com/org/repo/pull/99';
    const deps = {
      run: vi.fn().mockResolvedValue({ exitCode: 0, stdout: `PR opened: ${prUrl}\n` }),
    };
    const res = await runRemoteTask(
      { target: 'deploy@host:/srv/app', prompt: "fix it's bug", worker: 'claude-code', timeoutMs: 60_000, log: new RunLogger() },
      deps,
    );
    expect(res.ok).toBe(true);
    expect(res.prUrl).toBe(prUrl);
    const [argv, timeoutMs] = deps.run.mock.calls[1]!;
    expect(argv[0]).toBe('ssh');
    expect(argv.at(-2)).toBe('deploy@host');
    const cmd = argv.at(-1) as string;
    expect(cmd).toContain(`cd '/srv/app'`);
    expect(cmd).toContain(`devagent task 'fix it'\\''s bug' --auto-pr`);
    expect(cmd).toContain(`--worker 'claude-code'`);
    expect(timeoutMs).toBe(60_000);
  });

  it('forwards taskId as a quoted --id flag in the dispatch command', async () => {
    const deps = {
      run: vi.fn().mockResolvedValue({ exitCode: 0, stdout: 'done\n' }),
    };
    await runRemoteTask(
      { target: 'host:/srv/app', prompt: 'x', taskId: "loop-66-a'b", timeoutMs: 60_000, log: new RunLogger() },
      deps,
    );
    const cmd = deps.run.mock.calls[1]![0].at(-1) as string;
    expect(cmd).toContain(`--id 'loop-66-a'\\''b'`);
  });

  it('reports non-zero dispatch exits and keeps a PR URL if one appeared', async () => {
    const prUrl = 'https://github.com/org/repo/pull/7';
    const res = await runRemoteTask(
      { target: 'host:/srv/app', prompt: 'x', timeoutMs: 1000, log: new RunLogger() },
      { run: vi.fn().mockResolvedValueOnce({ exitCode: 0, stdout: '' }).mockResolvedValueOnce({ exitCode: 1, stdout: `opened ${prUrl} then failed` }) },
    );
    expect(res.ok).toBe(false);
    expect(res.prUrl).toBe(prUrl);
    expect(res.note).toMatch(/remote task failed on host \(exit 1\)/);
  });
});
