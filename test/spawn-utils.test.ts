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
