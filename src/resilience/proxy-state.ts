import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { transientErrorClass } from './classify.js';

/**
 * Durable provider/proxy health state (operator observability for the
 * orchestrate-loop proxy gate). Written by the proxy gate in
 * scripts/orchestrate-loop.sh and the transient-classification retry paths
 * (src/deps.ts, src/orchestrator/executor.ts); read by `devagent status
 * --providers`. Repo-scoped under `<repo>/.devagent/proxy-state.json` (like
 * the scout heartbeat) so parallel loops against different repos never
 * collide.
 *
 * Circuit model (classic breaker, probe = trial):
 *   closed    — healthy; probes pass
 *   open      — proxy gate failed (all 3 probes failed); work is skipped
 *   half-open — first probe passed after an outage; recovery trial in flight
 */

export type CircuitState = 'closed' | 'half-open' | 'open';

export interface ProxyProbeRecord {
  /** Result of the proxy-probe gate (true = response with "result" field). */
  ok: boolean;
  /** When the probe ran. */
  at: string;
  /** Extra detail (attempt count, fail summary). */
  detail?: string;
}

export interface TransientRecord {
  /** Coarse class label from src/resilience/classify.ts (transientErrorClass). */
  class: string;
  /** When the transient was classified. */
  at: string;
  /** Bounded excerpt of the classified error text. */
  excerpt: string;
}

export interface ProxyState {
  /** Current proxy circuit state. */
  circuit: CircuitState;
  /** When the circuit last transitioned. */
  circuitChangedAt: string;
  lastProbe?: ProxyProbeRecord;
  lastTransient?: TransientRecord;
  updatedAt: string;
}

export function proxyStatePath(repoPath: string): string {
  return join(repoPath, '.devagent', 'proxy-state.json');
}

export function readProxyState(repoPath: string): ProxyState | null {
  const p = proxyStatePath(repoPath);
  if (!existsSync(p)) return null;
  try {
    const raw = JSON.parse(readFileSync(p, 'utf8')) as ProxyState;
    if (!raw || typeof raw !== 'object' || !raw.circuit) return null;
    return raw;
  } catch {
    return null;
  }
}

function writeState(repoPath: string, state: ProxyState): void {
  mkdirSync(join(repoPath, '.devagent'), { recursive: true });
  writeFileSync(proxyStatePath(repoPath), JSON.stringify(state, null, 2) + '\n');
}

function nowIso(): string {
  return new Date().toISOString();
}

/**
 * Record one proxy-probe outcome and advance the circuit:
 *   fail       → open
 *   ok + open  → half-open (first recovery probe)
 *   ok         → closed (otherwise)
 * When state is absent it is created as closed. Best-effort: never throws
 * (observability must not break the gate).
 */
export function recordProxyProbe(repoPath: string, probe: { ok: boolean; detail?: string }): ProxyState {
  const prev = readProxyState(repoPath);
  const at = nowIso();
  let circuit: CircuitState;
  if (probe.ok) {
    circuit = prev?.circuit === 'open' ? 'half-open' : 'closed';
  } else {
    circuit = 'open';
  }
  const next: ProxyState = {
    circuit,
    circuitChangedAt: prev?.circuit !== circuit ? at : (prev?.circuitChangedAt ?? at),
    updatedAt: at,
  };
  if (prev?.lastTransient) next.lastTransient = prev.lastTransient;
  else if (prev?.lastTransient === undefined) {
    // explicit optional
  }
  next.lastProbe = { ok: probe.ok, at, ...(probe.detail ? { detail: probe.detail } : {}) };
  try {
    writeState(repoPath, next);
  } catch {
    // observability must never break the caller
  }
  return next;
}

/**
 * Classify an error text and, when it is a transient provider error, record
 * the coarse class + bounded excerpt as lastTransient. Returns the record
 * or null when the text is not transient (nothing is written). Best-effort.
 */
export function recordTransientClass(repoPath: string, text: string): TransientRecord | null {
  const cls = transientErrorClass(text);
  if (!cls) return null;
  const prev = readProxyState(repoPath);
  const at = nowIso();
  const record: TransientRecord = { class: cls, at, excerpt: text.replace(/\s+/g, ' ').trim().slice(0, 200) };
  const next: ProxyState = {
    circuit: prev?.circuit ?? 'closed',
    circuitChangedAt: prev?.circuitChangedAt ?? at,
    ...(prev?.lastProbe !== undefined ? { lastProbe: prev.lastProbe } : {}),
    lastTransient: record,
    updatedAt: at,
  };
  try {
    writeState(repoPath, next);
  } catch {
    // best-effort
  }
  return record;
}
