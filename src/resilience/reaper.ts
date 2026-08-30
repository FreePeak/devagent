import { syncCli } from '../workers/spawn-utils.js';

/**
 * Best-effort stale process reaper.
 * - killStaleProcessTree(pid): SIGTERM then SIGKILL the process group.
 * - findStaleWorkerPids: scan `ps` for opencode/claude/omp workers older than the threshold.
 * Guarded: only kills PIDs whose cmdline matches known worker patterns.
 */

const WORKER_PATTERN = /\b(opencode|claude|omp)(\s|$|--)/i;

function cmdlineFor(pid: number): string {
  try {
    const out = syncCli('ps', ['-o', `command=`, '-p', String(pid)], {
      cwd: '/', timeoutMs: 2_000,
    });
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
      const out = syncCli('ps', ['-o', `ppid=`, '-p', String(pid)], {
        cwd: '/', timeoutMs: 2_000,
      });
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

/**
 * A process is only reap-eligible when it is provably a devagent-spawned
 * worker: headless print-mode with JSON output. Interactive sessions
 * (`claude` TUI in any project, or the user's bare `omp` TUIs) never match,
 * so the reaper can never kill the user's live work (2026-08-26 incident:
 * pattern-only matching reaped unrelated interactive claude processes
 * machine-wide).
 */
const DEVAGENT_WORKER_CMD = /(^|\s)(--print|-p)(\s|$)/;
const DEVAGENT_RESUME_CMD = /(^|\s)(--continue|-c)(\s|$)/;
const OMP_MODE_JSON_CMD = /(^|\s)--mode(\s|=)json(\s|$)/;

export function isDevagentWorkerCmd(cmd: string): boolean {
  if (!WORKER_PATTERN.test(cmd)) return false;
  const headless = DEVAGENT_WORKER_CMD.test(cmd) || DEVAGENT_RESUME_CMD.test(cmd);
  if (!headless) return false;
  // claude-code / opencode: --output-format json (unchanged)
  if (/\b(opencode|claude)\b/i.test(cmd)) return /--output-format\b/.test(cmd);
  // omp: --mode json (adapter always emits `--mode json`; interactive omp has
  // no flag and never matches)
  if (/\bomp\b/.test(cmd)) return OMP_MODE_JSON_CMD.test(cmd);
  return false;
}

function cwdFor(pid: number): string {
  try {
    const out = syncCli('lsof', ['-a', '-p', String(pid), '-d', 'cwd', '-Fn'], {
      cwd: '/', timeoutMs: 2_000,
    });
    const line = out.split('\n').find((l) => l.startsWith('n'));
    return line ? line.slice(1) : '';
  } catch {
    return '';
  }
}

export function findStaleWorkerPids(
  olderThanMs = 10 * 60_000,
  opts: { cwdPrefix?: string } = {},
): StaleWorker[] {
  try {
    // etime (not etimes): BSD ps on macOS rejects etimes, which made the
    // whole scan throw and silently report "no stale workers" forever.
    const out = syncCli('ps', ['-eo', 'pid,etime,command'], {
      cwd: '/', timeoutMs: 3_000,
    });
    const lines = out.split('\n').slice(1);
    const own = ownAncestryPids();
    const stale: StaleWorker[] = [];
    for (const line of lines) {
      const m = line.trim().match(/^(\d+)\s+(\S+)\s+(.*)$/);
      if (!m) continue;
      const pid = Number(m[1]);
      if (own.has(pid)) continue;
      const cmd = m[3] ?? '';
      if (!isDevagentWorkerCmd(cmd)) continue;
      if (opts.cwdPrefix && !cwdFor(pid).startsWith(opts.cwdPrefix)) continue;
      const elapsedMs = parseEtimeToMs(m[2] ?? '');
      if (elapsedMs >= olderThanMs) stale.push({ pid, elapsedMs, command: cmd.slice(0, 300) });
    }
    return stale;
  } catch {
    return [];
  }
}

/** Kill all stale workers older than threshold. Returns pids killed. */
export function reapStaleWorkers(
  olderThanMs = 10 * 60_000,
  dryRun = false,
  opts: { cwdPrefix?: string } = {},
): StaleWorker[] {
  const stale = findStaleWorkerPids(olderThanMs, opts);
  if (dryRun) return stale;
  for (const s of stale) {
    killStaleProcessTree(s.pid, 'reap-stale');
  }
  return stale;
}
