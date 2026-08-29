import * as os from 'node:os';
import { execFile } from 'node:child_process';
import { readFile } from 'node:fs/promises';
import { promisify } from 'node:util';

const execFileAsync = promisify(execFile);

/**
 * Resource governor — sizes the worker pool from live OS signals.
 * PRD: Resource-Aware Concurrency (docs/prds/PRD-resource-aware-concurrency.md)
 *
 * Formula:
 * effective = clamp(1, configured, min(floor(avail * safetyRatio / estPerWorker), ceil(cpus * cpuFactor)))
 * When concurrency === 'auto': configured ceiling is not applied; effective = clamp(1, Infinity, min(...))
 * but still at least 1. The `safetyRatio` (default 0.7) keeps headroom.
 * `estMemPerWorker` self-calibrates via observed RSS p75 (never below minEstFloorBytes).
 */

export interface OsSnapshot {
  totalMem: number;
  freeMem: number;
  cpus: number;
  loadAvg?: number[];
}

export interface GovernorOptions {
  /** Safety headroom ratio (0..1), default 0.7 */
  safetyRatio?: number;
  /** Estimated mem per worker in bytes, default 1 GiB */
  estMemPerWorkerBytes?: number;
  /** CPU factor for cpu-bound cap, default 1.0 */
  cpuFactor?: number;
  /** Floor for estimated mem (never calibrate below), default 256 MiB */
  minEstFloorBytes?: number;
  /** Timeout waiting for pressure to lift, default 60s */
  pressureWaitTimeoutMs?: number;
  /** Cache TTL for OS reads in ms, default 1000 */
  cacheTtlMs?: number;
}

const DEFAULT_EST = 1 * 1024 * 1024 * 1024; // 1 GiB
const DEFAULT_MIN_FLOOR = 256 * 1024 * 1024;
const DEFAULT_SAFETY = 0.7;
const DEFAULT_CPU_FACTOR = 1.0;
const DEFAULT_CACHE_TTL = 1000;

function percentile(sorted: number[], p: number): number {
  if (sorted.length === 0) return 0;
  const idx = Math.ceil((p / 100) * sorted.length) - 1;
  return sorted[Math.max(0, Math.min(idx, sorted.length - 1))]!;
}

export class ResourceGovernor {
  private safetyRatio: number;
  private estMemPerWorkerBytes: number;
  private cpuFactor: number;
  private minEstFloorBytes: number;
  pressureWaitTimeoutMs: number;
  private cacheTtlMs: number;

  // RSS calibration
  private observedRss: number[] = [];
  // OS cache
  private cachedSnapshot: OsSnapshot | null = null;
  private cachedAt = 0;

  constructor(opts: GovernorOptions = {}) {
    this.safetyRatio = opts.safetyRatio ?? DEFAULT_SAFETY;
    this.estMemPerWorkerBytes = opts.estMemPerWorkerBytes ?? DEFAULT_EST;
    this.cpuFactor = opts.cpuFactor ?? DEFAULT_CPU_FACTOR;
    this.minEstFloorBytes = opts.minEstFloorBytes ?? DEFAULT_MIN_FLOOR;
    this.pressureWaitTimeoutMs = opts.pressureWaitTimeoutMs ?? 60_000;
    this.cacheTtlMs = opts.cacheTtlMs ?? DEFAULT_CACHE_TTL;
  }

  /** Record an observed worker RSS (bytes) and recalibrate p75 */
  recordRss(rssBytes: number): void {
    if (!Number.isFinite(rssBytes) || rssBytes <= 0) return;
    this.observedRss.push(rssBytes);
    // keep bounded history (last 100)
    if (this.observedRss.length > 100) this.observedRss.shift();
    const sorted = [...this.observedRss].sort((a, b) => a - b);
    const p75 = percentile(sorted, 75);
    // calibration can only raise the estimate, never lower below configured floor
    if (p75 > this.estMemPerWorkerBytes) {
      this.estMemPerWorkerBytes = p75;
    }
    if (this.estMemPerWorkerBytes < this.minEstFloorBytes) {
      this.estMemPerWorkerBytes = this.minEstFloorBytes;
    }
  }

  getEstMemPerWorker(): number {
    return this.estMemPerWorkerBytes;
  }

  getObservedCount(): number {
    return this.observedRss.length;
  }

  /** Clear calibration history (for tests) */
  resetCalibration(): void {
    this.observedRss = [];
  }

  /** Compute effective concurrency from explicit OS snapshot */
  effectiveConcurrency(snapshot: OsSnapshot, configuredConcurrency: number): number {
    if (!Number.isFinite(configuredConcurrency) || configuredConcurrency < 1) configuredConcurrency = 1;
    const avail = snapshot.freeMem;
    const cpus = snapshot.cpus || 1;
    const memCap = Math.floor((avail * this.safetyRatio) / this.estMemPerWorkerBytes);
    const cpuCap = Math.ceil(cpus * this.cpuFactor);
    const raw = Math.min(memCap, cpuCap);
    // clamp to [1, configured]
    const clamped = Math.max(1, Math.min(configuredConcurrency, raw));
    return clamped;
  }

  /** For 'auto' mode: no explicit ceiling, just mem/cpu bound, at least 1 */
  effectiveAuto(snapshot: OsSnapshot): number {
    const avail = snapshot.freeMem;
    const cpus = snapshot.cpus || 1;
    const memCap = Math.floor((avail * this.safetyRatio) / this.estMemPerWorkerBytes);
    const cpuCap = Math.ceil(cpus * this.cpuFactor);
    const raw = Math.min(memCap, cpuCap);
    return Math.max(1, raw);
  }

  /** Resolve concurrency input ('auto' or number) against live OS */
  resolveConcurrency(input: number | string): number {
    if (typeof input === 'string' && input === 'auto') {
      const snap = this.getSnapshotSync();
      return this.effectiveAuto(snap);
    }
    if (typeof input === 'string') {
      const n = Number(input);
      if (Number.isFinite(n)) return Math.max(1, Math.floor(n));
      return 2;
    }
    return Math.max(1, Math.floor(input));
  }

  /** Synchronous snapshot from Node os (cached) */
  getSnapshotSync(): OsSnapshot {
    const now = Date.now();
    if (this.cachedSnapshot && now - this.cachedAt < this.cacheTtlMs) {
      return this.cachedSnapshot;
    }
    const snap: OsSnapshot = {
      totalMem: os.totalmem(),
      freeMem: os.freemem(),
      cpus: os.cpus().length,
      loadAvg: os.loadavg(),
    };
    this.cachedSnapshot = snap;
    this.cachedAt = now;
    return snap;
  }

  /** Force refresh (bypass cache) */
  refreshSnapshot(): OsSnapshot {
    const snap: OsSnapshot = {
      totalMem: os.totalmem(),
      freeMem: os.freemem(),
      cpus: os.cpus().length,
      loadAvg: os.loadavg(),
    };
    this.cachedSnapshot = snap;
    this.cachedAt = Date.now();
    return snap;
  }

  /** Human-readable governor line for `devagent status` */
  formatStatus(concurrencyInput: number | string, effective: number, snapshot?: OsSnapshot): string {
    const snap = snapshot ?? this.getSnapshotSync();
    const freeGb = (snap.freeMem / (1024 * 1024 * 1024)).toFixed(1);
    const totalGb = (snap.totalMem / (1024 * 1024 * 1024)).toFixed(1);
    const estGb = (this.estMemPerWorkerBytes / (1024 * 1024 * 1024)).toFixed(1);
    const inputStr = String(concurrencyInput);
    if (inputStr === 'auto') {
      return `workers auto->${effective} (mem ${freeGb} GB free of ${totalGb}, est ${estGb} GB/worker, cpus ${snap.cpus})`;
    }
    return `workers ${effective}/${inputStr} (mem ${freeGb} GB free of ${totalGb}, est ${estGb} GB/worker, cpus ${snap.cpus})`;
  }

  /** For tests: inject a fake snapshot and bypass cache */
  injectSnapshot(snap: OsSnapshot): void {
    this.cachedSnapshot = snap;
    this.cachedAt = Date.now();
  }

  /** Clear OS cache (for tests) */
  clearCache(): void {
    this.cachedSnapshot = null;
    this.cachedAt = 0;
  }
}

/**
 * Sample RSS of a pid in bytes.
 * - Linux: /proc/<pid>/status VmRSS
 * - macOS: ps -o rss= -p <pid> (KB -> bytes)
 * Returns null on unsupported platform or if process not found.
 * No network calls. <5ms when cached? Single ps is ~10-20ms but caller caches snapshot, not RSS polling per se.
 */
export async function sampleWorkerRss(pid: number): Promise<number | null> {
  if (!Number.isFinite(pid) || pid <= 0) return null;
  if (process.platform === 'linux') {
    try {
      const text = await readFile(`/proc/${pid}/status`, 'utf8');
      const m = /VmRSS:\s+(\d+)\s+kB/.exec(text);
      if (!m) return null;
      const kb = Number(m[1]);
      if (!Number.isFinite(kb)) return null;
      return kb * 1024;
    } catch {
      return null;
    }
  }
  if (process.platform === 'darwin') {
    try {
      const { stdout } = await execFileAsync('ps', ['-o', 'rss=', '-p', String(pid)]);
      const kb = Number(stdout.trim());
      if (!Number.isFinite(kb) || kb <= 0) return null;
      return kb * 1024;
    } catch {
      return null;
    }
  }
  // Unsupported platform
  return null;
}

/**
 * Parse a concurrency CLI value: number or 'auto'.
 * Returns number|string; numeric strings become numbers, 'auto' stays string.
 */
export function parseConcurrencyInput(raw: unknown): number | 'auto' {
  if (raw === 'auto' || raw === 'Auto') return 'auto';
  if (typeof raw === 'number' && Number.isFinite(raw)) return Math.max(1, Math.floor(raw));
  if (typeof raw === 'string') {
    if (raw.trim().toLowerCase() === 'auto') return 'auto';
    const n = Number(raw);
    if (Number.isFinite(n)) return Math.max(1, Math.floor(n));
  }
  return 2;
}

/**
 * Resolve effective concurrency for scheduler/fleet:
 * - if input is number: return it directly (governor not consulted) per AC-7 zero-regression guard
 * - if 'auto': delegate to governor.effectiveAuto()
 * Returns the effective number and the snapshot used (for logging).
 */
export function resolveEffectiveConcurrency(
  input: number | 'auto',
  governor: ResourceGovernor,
  injectedSnapshot?: OsSnapshot,
): { effective: number; snapshot: OsSnapshot; wasThrottled: boolean } {
  if (typeof input === 'number') {
    const snap = injectedSnapshot ?? governor.getSnapshotSync();
    return { effective: input, snapshot: snap, wasThrottled: false };
  }
  const snap = injectedSnapshot ?? governor.getSnapshotSync();
  const effective = governor.effectiveAuto(snap);
  const wasThrottled = effective < Math.ceil(snap.cpus * 1.0); // heuristic: throttled if below cpu cap
  return { effective, snapshot: snap, wasThrottled };
}
