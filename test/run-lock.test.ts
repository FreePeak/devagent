import { describe, expect, it } from 'vitest';
import { spawnSync } from 'node:child_process';
import { existsSync, mkdtempSync, rmSync, writeFileSync, mkdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { sanitizeKey, tryAcquireRun } from '../src/runregistry.js';

const repoRoot = join(import.meta.dirname, '..');

interface RunResult {
  status: number;
  stdout: string;
  stderr: string;
}

function runCli(ticketId: string, devagentHome: string): RunResult {
  const r = spawnSync(
    'npx',
    ['tsx', 'src/cli.ts', 'run', '--ticket', ticketId, '--dry-run', '--repo', repoRoot],
    {
      cwd: repoRoot,
      stdio: 'pipe',
      encoding: 'utf8',
      env: {
        PATH: process.env.PATH,
        HOME: process.env.HOME,
        // Isolate the lock registry from the developer's real ~/.devagent
        DEVAGENT_HOME: devagentHome,
      },
    },
  );
  return { status: r.status ?? -1, stdout: r.stdout ?? '', stderr: r.stderr ?? '' };
}

const tmpHome = () => mkdtempSync(join(tmpdir(), 'da-runlock-'));

describe('run action per-ticket lock', () => {
  it('rejects a second concurrent run for the same ticket with non-zero exit', () => {
    const home = tmpHome();
    try {
      const holder = tryAcquireRun(home, 'LOCK-1');
      expect(holder).not.toBeNull();
      const r = runCli('LOCK-1', home);
      expect(r.status).not.toBe(0);
      expect(r.stderr).toContain('Run for LOCK-1 already active');
    } finally {
      rmSync(home, { recursive: true, force: true });
    }
  });

  it('allows different tickets concurrently (lock is per-ticket)', () => {
    const home = tmpHome();
    try {
      const r = runCli('LOCK-OTHER-1', home);
      expect(r.status).toBe(0);
      expect(r.stdout).toContain('Plan (endpoint-only):');
    } finally {
      rmSync(home, { recursive: true, force: true });
    }
  });

  it('releases the lock in finally after the run completes', () => {
    const home = tmpHome();
    try {
      const r = runCli('LOCK-2', home);
      expect(r.status).toBe(0);
      const locksDir = join(home, 'locks');
      expect(existsSync(join(locksDir, `${sanitizeKey('LOCK-2')}.lock`))).toBe(false);
      // Lock is gone: an immediate re-run must succeed
      const again = runCli('LOCK-2', home);
      expect(again.status).toBe(0);
    } finally {
      rmSync(home, { recursive: true, force: true });
    }
  });

  it('breaks stale locks older than the 1h TTL instead of rejecting', () => {
    const home = tmpHome();
    try {
      const locksDir = join(home, 'locks');
      mkdirSync(locksDir, { recursive: true });
      const lockPath = join(locksDir, `${sanitizeKey('LOCK-3')}.lock`);
      writeFileSync(lockPath, JSON.stringify({ pid: process.pid, startedAt: Date.now() - 2 * 60 * 60_000 }));
      const r = runCli('LOCK-3', home);
      // A fresh held lock would reject with non-zero; success proves the
      // stale lock was broken and reacquired (then released in finally)
      expect(r.status).toBe(0);
      expect(r.stdout).toContain('Plan (endpoint-only):');
      expect(existsSync(lockPath)).toBe(false);
    } finally {
      rmSync(home, { recursive: true, force: true });
    }
  });
});
