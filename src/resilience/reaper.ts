import { execSync } from 'node:child_process';

/**
 * Best-effort stale process reaper.
 * - killStaleProcessTree(pid): SIGTERM then SIGKILL the process group.
 * - findStaleWorkerPids: scan `ps` for opencode/claude workers older than threshold.
 * Guarded: only kills PIDs whose cmdline matches known worker patterns.
 */

const WORKER_PATTERN = /(opencode|claude)(\s|$|--)/i;

function cmdlineFor(pid: number): string {
  try {
    const out = execSync(`ps -o command= -p ${pid}`, { encoding: 'utf8', timeout: 2000 });
    return out.trim();
  } catch {
    return '';
  }
}

/**
 * PIDs of this process plus all its ancestors. Reaping must never kill
 * itself, its parent shell, or the devagent/LaunchAgent chain that invoked
 * it — otherwise a mid-run reap terminates the very pipeline it protects.
 */
export function ownAncestryPids(): Set<number> {
  const pids = new Set<number>();
  let pid = process.pid;
  for (let i = 0; i < 64 && pid > 1 && !pids.has(pid); i++) {
    pids.add(pid);
    try {
      const out = execSync(`ps -o ppid= -p ${pid}`, { encoding: 'utf8', timeout: 2000 });
      const next = Number(out.trim());
      if (!Number.isFinite(next) || next <= 1) break;
      pid = next;
    } catch {
      break;
    }
  }
  return pids;
}

function isWorkerPid(pid: number): boolean {
  const cmd = cmdlineFor(pid);
  return WORKER_PATTERN.test(cmd);
}

export function killStaleProcessTree(pid: number, reason = 'stale'): boolean {
  if (!Number.isFinite(pid) || pid <= 1) return false;
  if (!isWorkerPid(pid)) return false;
  try {
    // Try process group first (negative pid), then direct.
    try {
      process.kill(-pid, 'SIGTERM');
    } catch {
      process.kill(pid, 'SIGTERM');
    }
    // Brief grace then SIGKILL
    const deadline = Date.now() + 800;
    while (Date.now() < deadline) {
      try {
        process.kill(pid, 0);
      } catch {
        return true; // gone
      }
      Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 50);
    }
    try {
      process.kill(-pid, 'SIGKILL');
    } catch {
      try {
        process.kill(pid, 'SIGKILL');
      } catch {
        /* gone */
      }
    }
    return true;
  } catch {
    return false;
  }
}

export interface StaleWorker {
  pid: number;
  elapsedMs: number;
  command: string;
}

/**
 * Parse ps `etime` output ([[dd-]hh:]mm:ss or seconds) to milliseconds.
 * Portable across Linux procps and macOS BSD ps; `etimes` (raw seconds
 * column) is Linux-only and silently breaks the scan on macOS.
 */
export function parseEtimeToMs(etime: string): number {
  const s = etime.trim();
  if (/^\d+$/.test(s)) return Number(s) * 1000;
  const m = s.match(/^(?:(\d+)-)?(?:(\d+):)?(\d+):(\d+)$/);
  if (!m) return 0;
  const days = Number(m[1] ?? 0);
  const hours = Number(m[2] ?? 0);
  const minutes = Number(m[3]!);
  const seconds = Number(m[4]!);
  return (((days * 24 + hours) * 60 + minutes) * 60 + seconds) * 1000;
}

export function findStaleWorkerPids(olderThanMs = 10 * 60_000): StaleWorker[] {
  try {
    // etime (not etimes): BSD ps on macOS rejects etimes, which made the
    // whole scan throw and silently report "no stale workers" forever.
    const out = execSync('ps -eo pid,etime,command', { encoding: 'utf8', timeout: 3000 });
    const lines = out.split('\n').slice(1);
    const own = ownAncestryPids();
    const stale: StaleWorker[] = [];
    for (const line of lines) {
      const m = line.trim().match(/^(\d+)\s+(\S+)\s+(.*)$/);
      if (!m) continue;
      const pid = Number(m[1]);
      if (own.has(pid)) continue;
      const cmd = m[3] ?? '';
      if (!WORKER_PATTERN.test(cmd)) continue;
      const elapsedMs = parseEtimeToMs(m[2] ?? '');
      if (elapsedMs >= olderThanMs) stale.push({ pid, elapsedMs, command: cmd.slice(0, 300) });
    }
    return stale;
  } catch {
    return [];
  }
}

/** Kill all stale workers older than threshold. Returns pids killed. */
export function reapStaleWorkers(olderThanMs = 10 * 60_000, dryRun = false): StaleWorker[] {
  const stale = findStaleWorkerPids(olderThanMs);
  if (dryRun) return stale;
  for (const s of stale) {
    killStaleProcessTree(s.pid, 'reap-stale');
  }
  return stale;
}
