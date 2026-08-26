import { describe, expect, it } from 'vitest';
import { spawnCli } from '../src/workers/spawn-utils.js';

describe('spawnCli exit-code normalization', () => {
  it('maps a missing binary (ENOENT) to exitCode -1, never success', async () => {
    const r = await spawnCli('da-no-such-binary-xyz', ['--version'], { cwd: '.', timeoutMs: 10_000 });
    // Regression (dogfood loop 9): ENOENT used to normalize to 0 and read as
    // success, letting gates false-green on a missing tool.
    expect(r.exitCode).toBe(-1);
    expect(r.timedOut).toBe(false);
  });

  it('passes through a zero exit', async () => {
    const r = await spawnCli('node', ['-e', 'process.exit(0)'], { cwd: '.', timeoutMs: 10_000 });
    expect(r.exitCode).toBe(0);
  });

  it('passes through a nonzero exit code', async () => {
    const r = await spawnCli('node', ['-e', 'process.exit(3)'], { cwd: '.', timeoutMs: 10_000 });
    expect(r.exitCode).toBe(3);
  });

  it('marks timeouts with exitCode -1', async () => {
    const r = await spawnCli('node', ['-e', 'setTimeout(()=>{}, 60000)'], { cwd: '.', timeoutMs: 300 });
    expect(r.timedOut).toBe(true);
    expect(r.exitCode).toBe(-1);
  });
});

describe('spawnCli PATH fallback (live-smoke regression 2026-08-25)', () => {
  it('appends fallback segments when PATH is missing entirely (launchd context)', async () => {
    const r = await spawnCli(
      process.execPath,
      ['-e', 'console.log(process.env.PATH ?? "")'],
      { cwd: '.', timeoutMs: 10_000, replaceEnv: true, env: {} },
    );
    expect(r.exitCode).toBe(0);
    for (const seg of ['/opt/homebrew/bin', '/usr/bin', '/bin']) {
      expect(r.stdout.trim()).toContain(seg);
    }
  });

  it('keeps an already-usable PATH intact', async () => {
    const r = await spawnCli(
      process.execPath,
      ['-e', 'console.log(process.env.PATH ?? "")'],
      { cwd: '.', timeoutMs: 10_000, replaceEnv: true, env: { PATH: '/usr/bin:/bin' } },
    );
    expect(r.exitCode).toBe(0);
    expect(r.stdout.trim()).toBe('/usr/bin:/bin');
  });

  it('extends a partial PATH with only the missing segments', async () => {
    const r = await spawnCli(
      process.execPath,
      ['-e', 'console.log(process.env.PATH ?? "")'],
      { cwd: '.', timeoutMs: 10_000, replaceEnv: true, env: { PATH: '/custom/tools:/bin' } },
    );
    expect(r.exitCode).toBe(0);
    const out = r.stdout.trim();
    expect(out.startsWith('/custom/tools:/bin:')).toBe(true);
    expect(out).toContain('/usr/bin');
    expect(out).toContain('/opt/homebrew/bin');
  });
});

describe('spawnCli PWD/cwd consistency (live-smoke regression 2026-08-26)', () => {
  it('sets PWD to opts.cwd so CLIs trusting $PWD resolve the intended directory', async () => {
    const r = await spawnCli(
      process.execPath,
      ['-e', 'console.log(process.env.PWD ?? "")'],
      { cwd: '/tmp', timeoutMs: 10_000 },
    );
    expect(r.exitCode).toBe(0);
    expect(r.stdout.trim()).toBe('/tmp');
  });

  it('drops OLDPWD when overriding PWD', async () => {
    const r = await spawnCli(
      process.execPath,
      ['-e', 'console.log(process.env.OLDPWD === undefined ? "unset" : process.env.OLDPWD)'],
      { cwd: '/tmp', timeoutMs: 10_000 },
    );
    expect(r.exitCode).toBe(0);
    expect(r.stdout.trim()).toBe('unset');
  });

  it('leaves env untouched when no cwd is provided', async () => {
    const r = await spawnCli(
      process.execPath,
      ['-e', 'console.log(process.env.PWD === undefined ? "unset" : "kept")'],
      { cwd: '', timeoutMs: 10_000, replaceEnv: true, env: {} } as never,
    );
    expect(r.exitCode).toBe(0);
    expect(r.stdout.trim()).toBe('unset');
  });
});
