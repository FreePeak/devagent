import { runCommandInHerdrPane } from '../integrations/herdr.js';
import { spawnCli, type SpawnCliOptions, type SpawnCliResult } from './spawn-utils.js';
import { spawnVisibility } from '../config.js';

export interface RunWorkerCliOptions extends SpawnCliOptions {
  /** Route this launch through the herdr pane runtime (see integrations/herdr.ts). */
  herdr?: boolean;
}

/**
 * Fallback warning state, keyed PER SPAWN SITE (the worker CLI name) so an
 * operator sees one loud line per CLI (omp, claude-code, ...) per process —
 * not one per launch and not one per process (FR-VIS-01 "no silent
 * fallbacks": a silently headless run is indistinguishable from a healthy
 * one in analytics, so the fallback must be observable).
 */
const fallbackWarnedSites = new Set<string>();

/** Test seam: capture fallback warnings instead of stderr; clears dedupe state. */
export function setFallbackSink(fn: ((site: string, message: string) => void) | null): void {
  fallbackSink = fn;
  fallbackWarnedSites.clear();
}

let fallbackSink: ((site: string, message: string) => void) | null = null;

/**
 * Emit the one-time-per-site fallback warning: stderr by default, the
 * injected sink in tests.
 */
export function warnFallbackOnce(site: string, message: string): void {
  if (fallbackWarnedSites.has(site)) return;
  fallbackWarnedSites.add(site);
  if (fallbackSink) fallbackSink(site, message);
  else console.warn(`[herdr:${site}] ${message}`);
}

/**
 * Execute a worker CLI launch either inside a herdr pane (opts.herdr, or the
 * visibility-derived default from shouldUseHerdr) or as a direct child
 * process. When herdr is unavailable or misbehaves, falls back to direct
 * execution — workers must keep running; the runtime is a visibility
 * enhancement, never a hard dependency. Every fallback is loud, once per
 * spawn site (warnFallbackOnce).
 */
export async function runWorkerCli(cmd: string, args: string[], opts: RunWorkerCliOptions): Promise<SpawnCliResult> {
  if (shouldUseHerdr(opts.herdr)) {
    try {
      const viaHerdr = await runCommandInHerdrPane(cmd, args, opts);
      if (viaHerdr) return viaHerdr;
      warnFallbackOnce(cmd, `herdr session not reachable; running ${cmd} directly`);
    } catch (err) {
      warnFallbackOnce(cmd, `herdr pane run failed (${(err as Error).message}); running ${cmd} directly`);
    }
  }
  return spawnCli(cmd, args, opts);
}

/**
 * Herdr routing decision. Precedence: explicit per-spawn flag wins; then
 * DEVAGENT_HERDR=1|0 (legacy env-wide override); then DEVAGENT_VISIBILITY
 * ("headless" forces direct spawn, "visible" routes to panes); then the
 * configured spawn.visibility, which defaults to visible — FR-VIS-01 flips
 * the historical default so worker launches are observable unless the
 * operator opts out. DEVAGENT_HERDR stays ahead of visibility so an
 * explicit runtime toggle keeps working during the rollout.
 */
export function shouldUseHerdr(explicit?: boolean): boolean {
  if (explicit !== undefined) return explicit;
  const env = process.env.DEVAGENT_HERDR;
  if (env !== undefined && env !== '') return env !== '0' && !/^false$/i.test(env);
  const visibility = process.env.DEVAGENT_VISIBILITY === 'visible' || process.env.DEVAGENT_VISIBILITY === 'headless'
    ? process.env.DEVAGENT_VISIBILITY
    : spawnVisibility();
  return visibility !== 'headless';
}
