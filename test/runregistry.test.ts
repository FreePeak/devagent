import { describe, expect, it } from 'vitest';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { sanitizeKey, tryAcquireRun } from '../src/runregistry.js';

const home = () => mkdtempSync(join(tmpdir(), 'da-rr-'));

describe('tryAcquireRun', () => {
  it('acquires and releases cleanly', () => {
    const dir = home();
    try {
      const lock = tryAcquireRun(dir, 'ENG-1');
      expect(lock).not.toBeNull();
      expect(existsSync(lock!.path)).toBe(true);
      lock!.release();
      expect(existsSync(lock!.path)).toBe(false);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('rejects a second holder while the first is fresh', () => {
    const dir = home();
    try {
      const first = tryAcquireRun(dir, 'ENG-2')!;
      expect(tryAcquireRun(dir, 'ENG-2')).toBeNull();
      // different ticket is unaffected
      expect(tryAcquireRun(dir, 'ENG-3')).not.toBeNull();
      first.release();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('breaks stale locks (latest-wins)', () => {
    const dir = home();
    try {
      const locks = join(dir, 'locks');
      mkdirSync(locks, { recursive: true });
      const path = join(locks, 'ENG-4.lock');
      writeFileSync(path, JSON.stringify({ pid: 1, startedAt: 0 })); // ancient
      const acquired = tryAcquireRun(dir, 'ENG-4', { now: () => 10_000_000 });
      expect(acquired).not.toBeNull();
      expect(JSON.parse(readFileSync(acquired!.path, 'utf8')).pid).toBe(process.pid);
      acquired!.release();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('breaks corrupt locks', () => {
    const dir = home();
    try {
      const locks = join(dir, 'locks');
      mkdirSync(locks, { recursive: true });
      writeFileSync(join(locks, 'ENG-5.lock'), 'garbage{{{');
      expect(tryAcquireRun(dir, 'ENG-5')).not.toBeNull();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('sanitizes hostile ticket ids into safe filenames', () => {
    expect(sanitizeKey('../../etc/passwd')).toBe('.._.._etc_passwd');
    expect(sanitizeKey('a/b c')).toBe('a_b_c');
  });

  it('release is idempotent', () => {
    const dir = home();
    try {
      const lock = tryAcquireRun(dir, 'ENG-6')!;
      lock.release();
      expect(() => lock.release()).not.toThrow();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
