import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

/**
 * Run dedup / latest-wins (Sweep lesson): one active pipeline per ticket key
 * across processes. Lock files under <home>/locks/<sanitized-ticket>.lock;
 * a stale lock (older than ttlMs) is broken rather than blocking forever.
 */

export interface RunLock {
  ticketId: string;
  path: string;
  release(): void;
}

export function sanitizeKey(ticketId: string): string {
  return ticketId.replace(/[^A-Za-z0-9._-]/g, '_');
}

export function tryAcquireRun(
  homeDir: string,
  ticketId: string,
  opts: { ttlMs?: number; now?: () => number } = {},
): RunLock | null {
  const locksDir = join(homeDir, 'locks');
  mkdirSync(locksDir, { recursive: true });
  const path = join(locksDir, `${sanitizeKey(ticketId)}.lock`);
  const now = (opts.now ?? Date.now)();
  const ttl = opts.ttlMs ?? 60 * 60_000;

  if (existsSync(path)) {
    let holder: { startedAt?: number } | null = null;
    try {
      holder = JSON.parse(readFileSync(path, 'utf8')) as { startedAt?: number };
    } catch {
      holder = null;
    }
    const age = now - (holder?.startedAt ?? 0);
    if (holder && age <= ttl) return null; // someone else holds it fresh
    // stale or corrupt: break the lock (latest-wins)
    rmSync(path, { force: true });
  }

  writeFileSync(path, JSON.stringify({ pid: process.pid, startedAt: now }));
  let released = false;
  return {
    ticketId,
    path,
    release: () => {
      if (released) return;
      released = true;
      rmSync(path, { force: true });
    },
  };
}
